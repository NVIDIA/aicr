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
  repliesAfterWarning = [],
  canSeeIssueFields = true,
  fieldError = null,
  transientLabelError = false,
  liveProbePriority = 'Medium',
  liveProbeState = 'OPEN',
  lastEditedAt = null,
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
  let warning;
  const args = {
    context: { repo: { owner: 'NVIDIA', repo: 'aicr' } },
    core: { info: message => info.push(message), error: message => errors.push(message) },
    github: {
      graphql: async (query, args) => {
        if (query.includes('lastEditedAt')) {
          return { repository: { issue: { lastEditedAt } } };
        }
        if (query.includes('filterBy')) {
          const hasProbe = liveProbeState !== null && query.includes(liveProbeState) && args.priority === 'Medium';
          return { repository: { issues: { nodes: hasProbe ? [{ number: 2 }] : [] } } };
        }
        return {
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
        };
      },
      request: async (route, args) => {
        assert.equal(route, 'GET /repos/{owner}/{repo}/issues/{issue_number}/issue-field-values');
        assert.equal(args.headers['X-GitHub-Api-Version'], '2026-03-10');
        if (args.issue_number !== 2 && fieldError) {
          throw fieldError;
        }
        const selectedPriority = args.issue_number === 2 ? liveProbePriority : priority;
        return {
          data: selectedPriority === null || selectedPriority === undefined ? [] : [{
            issue_field_id: fieldId,
            data_type: 'single_select',
            single_select_option: { name: selectedPriority },
          }],
        };
      },
      paginate: async (method, options) => method(options),
      rest: {
        issues: {
          listForRepo: async () => [originalIssue],
          get: async () => ({ data: { ...originalIssue, ...updates } }),
          listEvents: async () => events,
          listComments: async ({ since } = {}) => {
            const available = warning ? [...comments, warning, ...repliesAfterWarning] : comments;
            return available.filter(comment =>
              since === undefined || Date.parse(comment.updated_at ?? comment.created_at) > Date.parse(since));
          },
          addLabels: async params => {
            await record('mark')(params);
            if (transientLabelError) {
              transientLabelError = false;
              throw Object.assign(new Error('Service unavailable'), { status: 503 });
            }
          },
          removeLabel: record('clear'),
          update: record('close'),
          createComment: async params => {
            await record('comment')(params);
            warning = { id: 1, created_at: originalIssue.updated_at };
            return { data: warning };
          },
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

const actionsBot = { type: 'Bot', login: 'github-actions[bot]' };
const oldStaleIssue = {
  issue: { labels: [staleLabel] },
  events: [{ event: 'labeled', label: staleLabel, created_at: defaultDate, actor: actionsBot }],
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
    comments: [{ user: { type: 'User' }, body: 'This still matters', created_at: '2026-01-03T12:00:00Z' }],
  });
  await runStaleIssues({ ...args, now: Date.parse('2026-02-01T12:00:00Z') });
  assert.deepEqual(calls.map(([operation]) => operation), ['clear']);
});

test('body edits clear stale status even when the REST timeline omits them', async () => {
  for (const [updatedAt, lastEditedAt] of [
    ['2026-01-01T12:00:01Z', '2026-01-01T12:00:01Z'],
    ['2026-01-01T12:01:00Z', null],
    ['2026-01-21T12:00:00Z', null],
  ]) {
    const { args, calls } = fixture({
      ...oldStaleIssue,
      issue: { ...oldStaleIssue.issue, updated_at: updatedAt },
      lastEditedAt,
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
  const initial = {
    ...oldStaleIssue,
    issue: {
      ...oldStaleIssue.issue,
      created_at: '2025-09-01T12:00:00Z',
      updated_at: '2026-01-01T11:59:59Z',
    },
    comments: [
      {
        user: { type: 'User' },
        body: 'Historical report',
        created_at: '2025-09-01T12:00:00Z',
      },
      {
        user: { type: 'Bot', login: 'github-actions[bot]' },
        body: 'This issue has been marked as stale due to 90 days of inactivity.',
        created_at: '2026-01-01T11:59:59Z',
      },
    ],
  };
  const pastMark = '2026-01-02T12:00:00Z';
  const afterReply = {
    ...initial,
    issue: { ...initial.issue, updated_at: defaultDate },
    comments: [...initial.comments, { user: { type: 'User' }, body: 'Still relevant', created_at: pastMark }],
  };
  for (const [config, expected] of [[initial, ['close', 'comment']], [afterReply, ['clear']]]) {
    const { args, calls } = fixture(config);
    await runStaleIssues({ ...args, now: Date.parse('2026-01-31T12:00:01Z') });
    assert.deepEqual(calls.map(([operation]) => operation), expected);
  }
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
        comments: [{ user: { type: 'User' }, body: 'Still active', created_at: '2026-01-03T12:00:00Z' }],
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

test('live processing fails before writes when the probe REST value is hidden or mismatched', async () => {
  for (const liveProbePriority of [null, 'Low']) {
    const { args, calls } = fixture({ ...oldStaleIssue, liveProbePriority });
    await assert.rejects(
      runStaleIssues({ ...args, now: Date.parse('2026-05-01T12:00:00Z') }),
      /REST Priority read did not match/,
    );
    assert.deepEqual(calls, []);
  }
});

test('a lagging bot label timestamp alone leaves the stale label in place', async () => {
  const { args, calls } = fixture({
    ...oldStaleIssue,
    issue: { ...oldStaleIssue.issue, updated_at: '2026-01-01T12:00:01Z' },
  });
  await runStaleIssues({ ...args, now: Date.parse('2026-01-02T12:00:00Z') });
  assert.deepEqual(calls, []);
  await runStaleIssues({ ...args, now: Date.parse('2026-02-01T12:00:00Z') });
  assert.deepEqual(calls.map(([operation]) => operation), ['close', 'comment']);
});

test('reply in the same second as the stale warning keeps the issue active', async () => {
  const { args, calls } = fixture({
    repliesAfterWarning: [{ id: 2, user: { type: 'User' }, body: 'I replied', created_at: defaultDate }],
  });
  await runStaleIssues({ ...args, now: Date.parse('2026-05-01T12:00:00Z') });
  assert.deepEqual(calls.map(([operation]) => operation), ['comment']);
});

test('transient label error retries with the same warning', async () => {
  const { args, calls } = fixture({ transientLabelError: true });
  await runStaleIssues({ ...args, now: Date.parse('2026-05-01T12:00:00Z') });
  assert.deepEqual(calls.map(([operation]) => operation), ['comment', 'mark', 'mark']);
});

test('live processing can verify Priority through a closed issue', async () => {
  const { args, calls } = fixture({ liveProbeState: 'CLOSED' });
  await runStaleIssues({ ...args, now: Date.parse('2026-05-01T12:00:00Z') });
  assert.deepEqual(calls.map(([operation]) => operation), ['comment', 'mark']);
});

test('live processing fails without a prioritized issue in either state', async () => {
  const { args, calls } = fixture({ liveProbeState: null });
  await assert.rejects(
    runStaleIssues({ ...args, now: Date.parse('2026-05-01T12:00:00Z') }),
    /Cannot verify REST Priority access/,
  );
  assert.deepEqual(calls, []);
});

test('reply in the same second as the stale label prevents closure', async () => {
  const { args, calls } = fixture({
    ...oldStaleIssue,
    comments: [{ id: 2, user: { type: 'User' }, body: 'Still active', created_at: defaultDate }],
  });
  await runStaleIssues({ ...args, now: Date.parse('2026-02-01T12:00:00Z') });
  assert.deepEqual(calls.map(([operation]) => operation), ['clear']);
});
