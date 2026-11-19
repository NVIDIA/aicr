// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const reviewReminder = 'Read the full diff before approving. Approved runs execute this code ' +
  'in AICR CI. Look hardest at .github/, Makefile, .goreleaser.yaml, tools/, scripts, ' +
  'go.mod/go.sum and tests.';

function pullNumber(pull) {
  if (!Number.isSafeInteger(pull?.number) || pull.number < 1) {
    throw new Error('Pull request list returned an invalid number');
  }
  return pull.number;
}

function headSha(pull) {
  const sha = pull?.head?.sha;
  if (typeof sha !== 'string' || !/^[0-9a-f]{40}$/.test(sha)) {
    throw new Error(`Pull request #${pull.number} has no valid head commit`);
  }
  return sha;
}

// Approval covers one commit, so only runs on the current head count: older
// commits keep their action_required runs forever. A run's pull_requests list
// is empty for fork PRs, which is why the lookup starts from the PR, not the run.
async function waitingSince(github, repo, pull) {
  const runs = await github.paginate(github.rest.actions.listWorkflowRunsForRepo, {
    ...repo,
    head_sha: headSha(pull),
    status: 'action_required',
    per_page: 100,
  });
  if (!Array.isArray(runs)) {
    throw new Error(`Workflow run lookup for #${pull.number} returned no run list`);
  }
  if (runs.length === 0) {
    return null;
  }
  const created = runs.map(run =>
    typeof run?.created_at === 'string' ? Date.parse(run.created_at) : Number.NaN);
  if (created.some(Number.isNaN)) {
    throw new Error(`Workflow run lookup for #${pull.number} returned no creation time`);
  }
  return Math.min(...created);
}

async function findWaitingPullRequests(github, repo) {
  const pulls = await github.paginate(github.rest.pulls.list, {
    ...repo,
    state: 'open',
    per_page: 100,
  });
  const waiting = [];
  for (const pull of pulls) {
    const number = pullNumber(pull);
    if (pull.draft) {
      continue;
    }
    const since = await waitingSince(github, repo, pull);
    if (since !== null) {
      waiting.push({ number, since });
    }
  }
  return waiting.sort((a, b) => a.since - b.since || a.number - b.number);
}

function formatMessage(waiting, repoUrl) {
  if (waiting.length === 0) {
    return '';
  }
  const links = waiting.map(({ number }) => `<${repoUrl}/pull/${number}|#${number}>`).join(' ');
  return `PRs waiting for CI approval: ${links}\n${reviewReminder}`;
}

module.exports = async function notifyPendingApprovals({ github, context, core }) {
  const repo = context.repo;
  const waiting = await findWaitingPullRequests(github, repo);
  const message = formatMessage(waiting, `${context.serverUrl}/${repo.owner}/${repo.repo}`);
  core.info(message || 'No pull request is waiting for workflow approval');
  core.setOutput('message', message);
};
