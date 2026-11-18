// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const assert = require('node:assert/strict');
const test = require('node:test');
const syncPriorityLabel = require('./sync-priority-label.cjs');

const repo = { owner: 'NVIDIA', repo: 'aicr' };
const issueNumber = 7;
const priorityField = { name: 'Priority', fullDatabaseId: '101' };
const options = ['Urgent', 'High', 'Medium', 'Low'].map(name => ({ id: `option-${name}`, name }));
const issueLabel = name => ({ name });
const connection = nodes => ({ pageInfo: { hasNextPage: false }, nodes });
const failure = (status, message) => Object.assign(new Error(message), { status });

function fixture({
  priority = null,
  labels = [],
  available = [],
  payload = {},
  configureSnapshot = () => {},
} = {}) {
  const calls = [];
  const logs = [];
  const state = {
    priority,
    labels: new Set(labels),
    definitions: new Map(available.map(name => [name, { name, color: '123abc', description: 'Existing' }])),
  };
  const github = {
    graphql: async (query, vars) => {
      assert.match(query, /issueOrPullRequest\(number: \$issueNumber\)/);
      assert.deepEqual(vars, { ...repo, issueNumber });
      const current = {
        nameWithOwner: 'NVIDIA/aicr',
        viewerCanSeeIssueFields: true,
        issueFields: connection([{ ...priorityField, id: 'priority-node', options }]),
        issueOrPullRequest: {
          __typename: 'Issue',
          number: issueNumber,
          labels: connection([...state.labels].map(issueLabel)),
          issueFieldValues: connection(state.priority ? [{
            __typename: 'IssueFieldSingleSelectValue',
            field: priorityField,
            name: state.priority,
            optionId: `option-${state.priority}`,
          }] : []),
        },
      };
      configureSnapshot(current);
      return { repository: current };
    },
    rest: {
      issues: {
        getLabel: async ({ owner, repo: name, name: label }) => {
          assert.deepEqual({ owner, repo: name }, repo);
          calls.push(['get', label]);
          if (!state.definitions.has(label)) {
            throw failure(404, 'Label missing');
          }
          return { data: state.definitions.get(label) };
        },
        createLabel: async ({ owner, repo: name, ...definition }) => {
          assert.deepEqual({ owner, repo: name }, repo);
          calls.push(['create', definition.name]);
          state.definitions.set(definition.name, definition);
          return { data: definition };
        },
        removeLabel: async ({ owner, repo: name, issue_number: number, name: label }) => {
          assert.deepEqual({ owner, repo: name }, repo);
          assert.equal(number, issueNumber);
          calls.push(['remove', label]);
          assert.ok(state.labels.delete(label));
        },
        addLabels: async ({ owner, repo: name, issue_number: number, labels: toAdd }) => {
          assert.deepEqual({ owner, repo: name }, repo);
          assert.equal(number, issueNumber);
          assert.equal(toAdd.length, 1);
          const label = toAdd[0];
          assert.ok(state.definitions.has(label), 'label definition must exist before attaching');
          calls.push(['add', label]);
          state.labels.add(label);
        },
      },
    },
  };
  const sync = () => syncPriorityLabel({
    github, context: { repo, payload }, core: { info: message => logs.push(message) }, issueNumber,
  });
  return { sync, github, state, calls, logs };
}

for (const [priority, target] of [
  ['Urgent', 'priority/P0'],
  ['High', 'priority/P0'],
  ['Medium', 'priority/P1'],
  ['Low', 'priority/P2'],
]) {
  test(`mirrors ${priority} to ${target}`, async () => {
    const { sync, state, calls } = fixture({ priority });
    await sync();
    assert.deepEqual(calls, [['get', target], ['create', target], ['add', target]]);
    assert.deepEqual([...state.labels], [target]);
  });
}

test('ensures the desired definition before removing obsolete labels and preserves other labels', async () => {
  const unrelated = ['ready', 'lifecycle/frozen', 'priority/P10', 'P0'];
  const { sync, state, calls } = fixture({
    priority: 'Medium',
    labels: ['priority/P0', ...unrelated, 'priority/P2'],
  });
  await sync();
  assert.deepEqual(calls, [
    ['get', 'priority/P1'],
    ['create', 'priority/P1'],
    ['remove', 'priority/P0'],
    ['remove', 'priority/P2'],
    ['add', 'priority/P1'],
  ]);
  assert.deepEqual([...state.labels], [...unrelated, 'priority/P1']);
  await sync();
  assert.equal(calls.length, 5);
});

test('unset Priority clears all managed labels even when another field is populated', async () => {
  const { sync, state, calls } = fixture({
    labels: ['priority/P0', 'blocked', 'priority/P1', 'priority/P2'],
    configureSnapshot(repository) {
      repository.issueOrPullRequest.issueFieldValues.nodes.push({
        __typename: 'IssueFieldSingleSelectValue',
        field: { name: 'Effort', fullDatabaseId: '202' },
        name: 'High',
        optionId: 'effort-high',
      });
    },
  });
  await sync();
  assert.deepEqual(calls, [['remove', 'priority/P0'], ['remove', 'priority/P1'], ['remove', 'priority/P2']]);
  assert.deepEqual([...state.labels], ['blocked']);
});

test('confirmation after a label creation race preserves the concurrent definition', async () => {
  const { sync, github, state, calls } = fixture({ priority: 'Low' });
  const { createLabel } = github.rest.issues;
  const concurrent = { name: 'priority/P2', color: '789abc', description: 'Created concurrently' };
  github.rest.issues.createLabel = async params => {
    await createLabel({ ...params, ...concurrent });
    throw failure(422, 'Label already exists');
  };
  await sync();
  assert.deepEqual(calls, [
    ['get', 'priority/P2'],
    ['create', 'priority/P2'],
    ['get', 'priority/P2'],
    ['add', 'priority/P2'],
  ]);
  assert.deepEqual(state.definitions.get('priority/P2'), concurrent);
});

test('failed definition creation leaves existing labels untouched', async () => {
  const { sync, github, state, calls } = fixture({ priority: 'Low', labels: ['priority/P0'] });
  github.rest.issues.createLabel = async () => { throw failure(422, 'Label cannot be created'); };
  await assert.rejects(sync(), /Label cannot be created/);
  assert.deepEqual(calls, [['get', 'priority/P2'], ['get', 'priority/P2']]);
  assert.deepEqual([...state.labels], ['priority/P0']);
});

test('an already-removed managed label is accepted after rereading the issue', async () => {
  const { sync, github, state } = fixture({ priority: 'Low', labels: ['priority/P0'] });
  github.rest.issues.removeLabel = async ({ name }) => {
    state.labels.delete(name);
    throw failure(404, 'Already removed');
  };
  await sync();
  assert.deepEqual([...state.labels], ['priority/P2']);
});

test('reconciles once more when Priority changes during label writes', async () => {
  const { sync, github, state, calls } = fixture({ priority: 'Medium' });
  const { addLabels } = github.rest.issues;
  github.rest.issues.addLabels = async params => {
    await addLabels(params);
    state.priority = 'High';
  };
  await sync();
  assert.deepEqual(calls, [
    ['get', 'priority/P1'], ['create', 'priority/P1'], ['add', 'priority/P1'],
    ['get', 'priority/P0'], ['create', 'priority/P0'], ['remove', 'priority/P1'], ['add', 'priority/P0'],
  ]);
  assert.deepEqual([...state.labels], ['priority/P0']);
});

const unreadableCases = [
  ['hidden issue fields', repository => { repository.viewerCanSeeIssueFields = false; }, /unreadable/],
  ['missing Priority', repository => { repository.issueFields.nodes = []; }, /Priority field/],
  ['truncated labels', repository => { repository.issueOrPullRequest.labels.pageInfo.hasNextPage = true; }, /Incomplete issue label/],
  ['truncated values', repository => { repository.issueOrPullRequest.issueFieldValues.pageInfo.hasNextPage = true; }, /Incomplete issue field value/],
  ['unknown option', repository => { repository.issueOrPullRequest.issueFieldValues.nodes[0].optionId = 'another-option'; }, /Priority value/],
];

for (const [name, configureSnapshot, expected] of unreadableCases) {
  test(`does not write on ${name}`, async () => {
    const { sync, calls, state } = fixture({ priority: 'Low', labels: ['priority/P0'], configureSnapshot });
    await assert.rejects(sync(), expected);
    assert.deepEqual(calls, []);
    assert.deepEqual([...state.labels], ['priority/P0']);
  });
}

test('ignores unrelated issue field events and rejects an incorrect Priority field ID', async () => {
  const unrelated = fixture({ payload: { issue_field: { name: 'Effort', id: 202 } }, priority: 'Low' });
  await unrelated.sync();
  assert.deepEqual(unrelated.calls, []);
  const incorrect = fixture({ payload: { issue_field: { name: 'Priority', id: 202 } }, priority: 'Low' });
  await assert.rejects(incorrect.sync(), /Priority event does not match/);
  assert.deepEqual(incorrect.calls, []);
});

test('does not write labels to a pull request number', async () => {
  const { sync, calls } = fixture({
    priority: 'Urgent',
    configureSnapshot(repository) { repository.issueOrPullRequest = { __typename: 'PullRequest' }; },
  });
  await sync();
  assert.deepEqual(calls, []);
});
