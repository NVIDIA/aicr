// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const assert = require('node:assert/strict');
const test = require('node:test');
const runStaleIssues = require('./stale-issues.cjs');

const defaultDate = '2026-01-01T12:00:00Z';
const staleLabel = { name: 'lifecycle/stale' };
const fieldId = 363192;

function fixture({
  priority,
  issue = {},
  updates = {},
  events = [],
  comments = [],
  canSeeIssueFields = true,
  fieldError = null,
} = {}) {
  const originalIssue = {
    number: 1,
    state: 'open',
    locked: false,
    labels: [],
    created_at: defaultDate,
    updated_at: defaultDate,
    ...issue,
  };
  const calls = [];
  const errors = [];
  const info = [];
  const record = operation => async params => calls.push([operation, params]);
  const args = {
    context: { repo: { owner: 'NVIDIA', repo: 'aicr' } },
    core: { info: message => info.push(message), error: message => errors.push(message) },
    github: {
      graphql: async () => ({
        repository: {
          viewerCanSeeIssueFields: canSeeIssueFields,
          issueFields: {
            nodes: [{
              name: 'Priority',
              fullDatabaseId: String(fieldId),
              options: ['Urgent', 'High', 'Medium', 'Low'].map(name => ({ name })),
            }],
          },
        },
      }),
      request: async (route, args) => {
        assert.equal(route, 'GET /repos/{owner}/{repo}/issues/{issue_number}/issue-field-values');
        assert.equal(args.headers['X-GitHub-Api-Version'], '2026-03-10');
        if (fieldError) {
          throw fieldError;
        }
        return {
          data: priority === undefined ? [] : [{
            issue_field_id: fieldId,
            data_type: 'single_select',
            single_select_option: { name: priority },
          }],
        };
      },
      paginate: async method => method(),
      rest: {
        issues: {
          listForRepo: async () => [originalIssue],
          get: async () => ({ data: { ...originalIssue, ...updates } }),
          listEvents: async () => events,
          listComments: async () => comments,
          addLabels: record('mark'),
          removeLabel: record('clear'),
          update: record('close'),
          createComment: record('comment'),
        },
      },
    },
  };
  return { args, calls, errors, info };
}

const cases = [
  { name: 'unprioritized issue uses 90-day threshold', expected: ['comment', 'mark'] },
  { name: 'low issue uses 90-day threshold', priority: 'Low', expected: ['comment', 'mark'] },
  {
    name: 'recent inactivity does not become stale',
    issue: { updated_at: '2026-04-01T12:00:00Z' },
    expected: [],
  },
  { name: 'medium is protected through its six-month window', priority: 'Medium', expected: [] },
  {
    name: 'six-month window clamps to last day of target month',
    priority: 'Medium',
    issue: { created_at: '2026-08-31T12:00:00Z', updated_at: '2026-08-31T12:00:00Z' },
    now: '2027-02-28T11:59:59Z',
    expected: [],
  },
  {
    name: 'clamped deadline expires at the original time of day',
    priority: 'Medium',
    issue: { created_at: '2026-08-31T12:00:00Z', updated_at: '2026-08-31T12:00:00Z' },
    now: '2027-02-28T12:00:00Z',
    expected: ['comment', 'mark'],
  },
  { name: 'issue with new activity after planning is skipped', updates: { updated_at: '2026-04-30T12:00:00Z' }, expected: [] },
  { name: 'pull requests are skipped', issue: { pull_request: {} }, expected: [] },
  { name: 'locked issues are skipped', issue: { locked: true }, expected: [] },
  { name: 'frozen issues are skipped', issue: { labels: [{ name: 'lifecycle/frozen' }] }, expected: [] },
  { name: 'good first issues are skipped', issue: { labels: [{ name: 'good first issue' }] }, expected: [] },
];

for (const { name, now = '2026-04-30T15:00:00Z', expected, ...config } of cases) {
  test(name, async () => {
    const { args, calls } = fixture(config);
    await runStaleIssues({ ...args, now: Date.parse(now) });
    assert.deepEqual(calls.map(([operation]) => operation), expected);
  });
}

const oldStaleIssue = {
  issue: { labels: [staleLabel] },
  events: [{ event: 'labeled', label: staleLabel, created_at: defaultDate }],
};

test('close and comment after 30 days without new activity', async () => {
  const { args, calls } = fixture(oldStaleIssue);
  await runStaleIssues({ ...args, now: Date.parse('2026-01-31T12:00:01Z') });
  assert.deepEqual(calls.map(([operation]) => operation), ['close', 'comment']);
  assert.equal(calls[0][1].state_reason, 'not_planned');
});

for (const priority of ['Urgent', 'High']) {
  test(`${priority} does not close during its one-month window`, async () => {
    const { args, calls } = fixture({ ...oldStaleIssue, priority });
    await runStaleIssues({ ...args, now: Date.parse('2026-01-31T12:00:01Z') });
    assert.deepEqual(calls.map(([operation]) => operation), ['clear']);
  });
}

test('human comment clears the stale label', async () => {
  const { args, calls } = fixture({
    ...oldStaleIssue,
    comments: [{ user: { type: 'User' }, body: 'This still matters' }],
  });
  await runStaleIssues({ ...args, now: Date.parse('2026-02-01T12:00:00Z') });
  assert.deepEqual(calls.map(([operation]) => operation), ['clear']);
});

test('body edits clear stale status even when the REST timeline omits them', async () => {
  for (const updatedAt of ['2026-01-01T12:00:01Z', '2026-01-21T12:00:00Z']) {
    const { args, calls } = fixture({
      ...oldStaleIssue,
      issue: { ...oldStaleIssue.issue, updated_at: updatedAt },
    });
    await runStaleIssues({ ...args, now: Date.parse('2026-03-01T12:00:00Z') });
    assert.deepEqual(calls.map(([operation]) => operation), ['clear']);
  }
});

test('missing or superseded stale label event cannot close an issue', async () => {
  const removedStaleEvent = {
    event: 'unlabeled', label: staleLabel, created_at: '2026-01-02T12:00:00Z',
  };
  for (const events of [[], [...oldStaleIssue.events, removedStaleEvent]]) {
    const { args, calls, errors } = fixture({ ...oldStaleIssue, events });
    await assert.rejects(
      runStaleIssues({ ...args, now: Date.parse('2026-03-01T12:00:00Z') }),
      /Stale issue processing failed/,
    );
    assert.deepEqual(calls, []);
    assert.match(errors[0], /Missing current lifecycle\/stale label event/);
  }
});

test('legacy warning before the label does not restart the stale clock', async () => {
  const { args, calls } = fixture({
    ...oldStaleIssue,
    issue: {
      ...oldStaleIssue.issue,
      created_at: '2025-09-01T12:00:00Z',
      updated_at: '2026-01-01T11:59:59Z',
    },
  });
  await runStaleIssues({ ...args, now: Date.parse('2026-01-31T12:00:01Z') });
  assert.deepEqual(calls.map(([operation]) => operation), ['close', 'comment']);
});

test('hidden priority field stops the job before any writes', async () => {
  const { args, calls } = fixture({ canSeeIssueFields: false });
  await assert.rejects(
    runStaleIssues({ ...args, now: Date.parse('2026-05-01T12:00:00Z') }),
    /Cannot verify access/,
  );
  assert.deepEqual(calls, []);
});

test('REST read failure or unexpected priority does not close an issue', async () => {
  for (const config of [{ priority: 'Critical' }, { fieldError: new Error('REST Forbidden') }]) {
    const { args, calls, errors } = fixture({ ...oldStaleIssue, ...config });
    await assert.rejects(
      runStaleIssues({ ...args, now: Date.parse('2026-05-01T12:00:00Z') }),
      /Stale issue processing failed/,
    );
    assert.deepEqual(calls, []);
    assert.match(errors[0], /Invalid native Priority value|REST Forbidden/);
  }
});

test('dry run reads populated priority and previews changes without writing', async () => {
  const previews = [
    { config: {}, plan: 'mark 1, clear 0, close 0' },
    { config: oldStaleIssue, plan: 'mark 0, clear 0, close 1' },
    {
      config: {
        ...oldStaleIssue,
        comments: [{ user: { type: 'User' }, body: 'Still active' }],
      },
      plan: 'mark 0, clear 1, close 0',
    },
  ];
  for (const { config, plan } of previews) {
    const { args, calls, info } = fixture({ ...config, priority: 'Low' });
    await runStaleIssues({
      ...args, dryRun: true, priorityProbeIssue: 1, now: Date.parse('2026-05-01T12:00:00Z'),
    });
    assert.deepEqual(calls, []);
    assert.ok(info.includes('Dry run verified a populated native Priority value'));
    assert.ok(info.includes(`Dry run: ${plan}`));
  }
});

test('dry run fails when probe priority is blank or inaccessible', async () => {
  const { args, calls } = fixture();
  await assert.rejects(
    runStaleIssues({ ...args, dryRun: true, priorityProbeIssue: 1 }),
    /must have a readable native Priority value/,
  );
  assert.deepEqual(calls, []);
});
