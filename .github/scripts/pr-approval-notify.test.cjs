// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const assert = require('node:assert/strict');
const test = require('node:test');
const notifyPendingApprovals = require('./pr-approval-notify.cjs');

const repo = { owner: 'NVIDIA', repo: 'aicr' };
const context = { repo, serverUrl: 'https://github.com' };
const sha = n => n.toString(16).padStart(40, '0');
const pull = (number, { draft = false, head = sha(number) } = {}) => ({ number, draft, head: { sha: head } });
const link = number => `<https://github.com/NVIDIA/aicr/pull/${number}|#${number}>`;

function fixture({ pulls, waiting = {}, runsResponse }) {
  const lookups = [];
  const outputs = {};
  const logs = [];
  const github = {
    paginate: async (method, params) => {
      if (method === github.rest.pulls.list) {
        assert.deepEqual(params, { ...repo, state: 'open', per_page: 100 });
        return pulls;
      }
      assert.equal(method, github.rest.actions.listWorkflowRunsForRepo);
      lookups.push(params);
      if (runsResponse) {
        return runsResponse(params);
      }
      return (waiting[params.head_sha] ?? []).map(created_at => ({ created_at }));
    },
    rest: {
      pulls: { list: () => {} },
      actions: {
        listWorkflowRunsForRepo: () => {
          throw new Error('Run lookups must page through github.paginate');
        },
      },
    },
  };
  const core = {
    info: message => logs.push(message),
    setOutput: (name, value) => { outputs[name] = value; },
  };
  return { github, core, lookups, outputs, logs };
}

test('lists ready PRs whose head commit awaits approval, oldest wait first', async () => {
  const f = fixture({
    pulls: [pull(3713), pull(3906, { draft: true }), pull(3488), pull(3920), pull(3899), pull(3921)],
    // Newest first, as the API returns them.
    waiting: {
      [sha(3713)]: ['2026-11-19T09:00:00Z'],
      [sha(3906)]: ['2026-11-18T09:00:00Z'],
      [sha(3488)]: ['2026-10-29T09:00:00Z'],
      [sha(3920)]: ['2026-11-19T10:00:00Z', '2026-10-01T09:00:00Z'],
      [sha(3921)]: ['2026-11-19T09:00:00Z'],
    },
  });

  await notifyPendingApprovals({ github: f.github, context, core: f.core });

  assert.equal(
    f.outputs.message,
    `PRs waiting for CI approval: ${link(3920)} ${link(3488)} ${link(3713)} ${link(3921)}\n` +
      'Read the full diff before approving. Approved runs execute this code in AICR CI. ' +
      'Look hardest at .github/, Makefile, .goreleaser.yaml, tools/, scripts, go.mod/go.sum and tests.',
  );
  assert.deepEqual(f.logs, [f.outputs.message]);
  assert.deepEqual(
    f.lookups.map(params => params.head_sha),
    [sha(3713), sha(3488), sha(3920), sha(3899), sha(3921)],
  );
  for (const params of f.lookups) {
    assert.deepEqual(params, { ...repo, head_sha: params.head_sha, status: 'action_required', per_page: 100 });
  }
});

test('takes the earliest waiting run from every page', async () => {
  const day = n => `2026-10-${String(n).padStart(2, '0')}T09:00:00Z`;
  const manyRuns = Array.from({ length: 150 }, (_, i) => (i === 149 ? '2026-09-01T09:00:00Z' : day(1 + (i % 28))));
  const f = fixture({
    pulls: [pull(3713), pull(3920)],
    // #3713 waits longer than any of #3920's first 100 runs but not its 150th.
    waiting: { [sha(3713)]: ['2026-09-15T09:00:00Z'], [sha(3920)]: manyRuns },
  });

  await notifyPendingApprovals({ github: f.github, context, core: f.core });

  assert.ok(f.outputs.message.startsWith(`PRs waiting for CI approval: ${link(3920)} ${link(3713)}\n`));
});

test('outputs an empty message when nothing awaits approval', async () => {
  const f = fixture({
    pulls: [pull(3713), pull(3906, { draft: true })],
    waiting: { [sha(3906)]: ['2026-11-18T09:00:00Z'] },
  });

  await notifyPendingApprovals({ github: f.github, context, core: f.core });

  assert.equal(f.outputs.message, '');
  assert.deepEqual(f.logs, ['No pull request is waiting for workflow approval']);
});

test('fails closed on data it cannot trust', async t => {
  const cases = [
    ['an invalid PR number', { pulls: [{ number: '3713', draft: false, head: { sha: sha(1) } }] }, /invalid number/],
    ['a missing head commit', { pulls: [pull(3713, { head: 'main' })] }, /no valid head commit/],
    ['a run lookup that is not a list', { pulls: [pull(3713)], runsResponse: () => ({}) }, /no run list/],
    [
      'a waiting run without a creation time',
      { pulls: [pull(3713)], waiting: { [sha(3713)]: ['2026-11-19T09:00:00Z', undefined] } },
      /no creation time/,
    ],
    [
      'a failed run lookup',
      { pulls: [pull(3713)], runsResponse: () => { throw new Error('API rate limit exceeded'); } },
      /rate limit/,
    ],
  ];
  for (const [name, options, error] of cases) {
    await t.test(name, async () => {
      const f = fixture(options);
      await assert.rejects(notifyPendingApprovals({ github: f.github, context, core: f.core }), error);
      assert.deepEqual(f.outputs, {});
    });
  }
});
