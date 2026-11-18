// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const staleLabel = 'lifecycle/stale';
const exemptLabels = ['lifecycle/frozen', 'good first issue'];
const protectionMonths = new Map([
  ['Urgent', 1],
  ['High', 1],
  ['Medium', 6],
  ['Low', 0],
]);
const second = 1000;
const day = 24 * 60 * 60 * second;
const staleAfter = 90 * day;
const closeAfter = 30 * day;
const labelUpdateSkew = 15_000;
const apiVersion = { 'X-GitHub-Api-Version': '2026-03-10' };

const staleMessage = `This issue has been marked as stale due to 90 days of inactivity.
It will be closed in 30 days unless there is new activity.

To keep this issue open:
- Add a comment with an update
- Add the \`lifecycle/frozen\` label

If this issue is no longer relevant, it's okay to let it close.`;

const closeMessage = `This issue was automatically closed due to inactivity after being marked as stale.

If you believe this issue is still relevant:
- Reopen with a comment providing updated details or context
- Add the \`lifecycle/frozen\` label to prevent it from being closed again`;

function parseTime(value) {
  const time = Date.parse(value);
  if (!Number.isFinite(time)) {
    throw new Error(`Invalid issue timestamp: ${value}`);
  }
  return time;
}

function protectedByPriority(priority, createdAt, now) {
  const months = priority === null ? 0 : protectionMonths.get(priority);
  if (months === undefined) {
    throw new Error(`Unknown issue Priority: ${priority}`);
  }
  if (months === 0) {
    return false;
  }

  const deadline = new Date(parseTime(createdAt));
  const dayOfMonth = deadline.getUTCDate();
  deadline.setUTCMonth(deadline.getUTCMonth() + months + 1, 0);
  deadline.setUTCDate(Math.min(dayOfMonth, deadline.getUTCDate()));
  return now < deadline.getTime();
}

function hasLabel(issue, label) {
  return issue.labels.some(entry => entry.name.toLowerCase() === label);
}

function isEligible(issue) {
  return issue.state === 'open' && !issue.pull_request && !issue.locked &&
    !exemptLabels.some(label => hasLabel(issue, label));
}

function isCandidate(issue, now) {
  return isEligible(issue) &&
    (hasLabel(issue, staleLabel) || now - parseTime(issue.updated_at) > staleAfter);
}

async function readPriorityField(github, repo) {
  // A hidden field and an unset field can both yield an empty issue-field-values response.
  const { repository } = await github.graphql(
    `query($owner: String!, $repo: String!) {
      repository(owner: $owner, name: $repo) {
        viewerCanSeeIssueFields
        issueFields(first: 25) {
          nodes {
            ... on IssueFieldSingleSelect {
              name
              fullDatabaseId
              options { name }
            }
          }
        }
      }
    }`,
    repo,
  );
  const field = repository?.issueFields?.nodes.find(node => node.name === 'Priority');
  const fieldId = Number(field?.fullDatabaseId);
  if (!repository?.viewerCanSeeIssueFields || !Number.isSafeInteger(fieldId) || fieldId <= 0 ||
      field.options.length !== protectionMonths.size ||
      field.options.some(option => !protectionMonths.has(option.name))) {
    throw new Error('Cannot verify access to the native Priority field and its configured options');
  }
  return fieldId;
}

async function readIssuePriority(github, issueRef, fieldId) {
  const { data } = await github.request(
    'GET /repos/{owner}/{repo}/issues/{issue_number}/issue-field-values',
    { ...issueRef, per_page: 100, headers: apiVersion },
  );
  if (!Array.isArray(data) || data.some(value => !Number.isSafeInteger(value?.issue_field_id))) {
    throw new Error('Invalid issue field values response');
  }
  const values = data.filter(value => value.issue_field_id === fieldId);
  if (values.length === 0) {
    return null;
  }
  if (values.length !== 1 || values[0].data_type !== 'single_select' ||
      !protectionMonths.has(values[0].single_select_option?.name)) {
    throw new Error('Invalid native Priority value');
  }
  return values[0].single_select_option.name;
}

async function verifyLivePriorityRead(github, repo, fieldId) {
  for (const expectedPriority of ['Medium', 'High', 'Urgent', 'Low']) {
    const { repository } = await github.graphql(
      `query($owner: String!, $repo: String!, $priority: String!) {
        repository(owner: $owner, name: $repo) {
          issues(first: 1, states: [OPEN, CLOSED], orderBy: { field: CREATED_AT, direction: DESC },
            filterBy: { issueFieldValues: [{ fieldName: "Priority", singleSelectOptionValue: $priority }] }) {
            nodes { number }
          }
        }
      }`,
      { ...repo, priority: expectedPriority },
    );
    const probeNumber = repository.issues.nodes[0]?.number;
    if (probeNumber === undefined) {
      continue;
    }

    const actualPriority = await readIssuePriority(github, { ...repo, issue_number: probeNumber }, fieldId);
    if (actualPriority !== expectedPriority) {
      throw new Error('REST Priority read did not match the populated value from GraphQL');
    }
    return;
  }
  throw new Error('Cannot verify REST Priority access without a prioritized issue');
}

function isActionsBot(user) {
  return user?.type === 'Bot' && user.login === 'github-actions[bot]';
}

async function wasEditedSince(github, issueRef, since) {
  const { repository } = await github.graphql(
    `query($owner: String!, $repo: String!, $issue_number: Int!) {
      repository(owner: $owner, name: $repo) {
        issue(number: $issue_number) { lastEditedAt }
      }
    }`,
    issueRef,
  );
  const lastEditedAt = repository?.issue?.lastEditedAt;
  return lastEditedAt !== null && parseTime(lastEditedAt) >= since;
}

async function commentsAtOrAfter(github, issueRef, cutoff) {
  // Use the preceding second to keep comments at the documented exclusive boundary.
  const since = new Date(cutoff - second).toISOString();
  const comments = await github.paginate(github.rest.issues.listComments, {
    ...issueRef, since, per_page: 100,
  });
  return comments.filter(comment => parseTime(comment.updated_at ?? comment.created_at) >= cutoff);
}

async function staleAction(github, issueRef, issue, now) {
  if (!hasLabel(issue, staleLabel)) {
    return 'mark';
  }

  const events = await github.paginate(github.rest.issues.listEvents, { ...issueRef, per_page: 100 });
  const markIndex = events.findLastIndex(event =>
    (event.event === 'labeled' || event.event === 'unlabeled') &&
    event.label?.name?.toLowerCase() === staleLabel);
  const lastStaleEvent = events[markIndex];
  if (lastStaleEvent?.event !== 'labeled') {
    throw new Error(`Missing current ${staleLabel} label event`);
  }
  const markedAt = parseTime(lastStaleEvent.created_at);
  const updatedAt = parseTime(issue.updated_at);
  const comments = await commentsAtOrAfter(github, issueRef, markedAt);

  const hasComment = comments.some(comment =>
    comment.user?.type === 'User' && comment.body?.trim() !== staleMessage);
  const timeAfterMark = updatedAt - markedAt;
  if (hasComment || events.length > markIndex + 1 ||
      timeAfterMark > labelUpdateSkew ||
      (timeAfterMark > 0 && !isActionsBot(lastStaleEvent.actor))) {
    return 'clear';
  }

  const dueToClose = now - Math.max(markedAt, updatedAt) > closeAfter;
  // GitHub can record the bot's label update slightly after the event. Check for
  // issue edits separately because body edits are absent from the event list.
  if ((timeAfterMark > 0 || dueToClose) && await wasEditedSince(github, issueRef, markedAt)) {
    return 'clear';
  }
  return dueToClose ? 'close' : null;
}

async function addStaleLabelAfterWarning(github, issueRef, warning, fieldId, now) {
  const warnedAt = parseTime(warning?.created_at);
  for (const mayRetry of [true, false]) {
    const comments = await commentsAtOrAfter(github, issueRef, warnedAt);
    if (comments.some(comment => comment.id !== warning.id) ||
        await wasEditedSince(github, issueRef, warnedAt)) {
      return false;
    }

    const priority = await readIssuePriority(github, issueRef, fieldId);
    const { data: current } = await github.rest.issues.get(issueRef);
    if (!isEligible(current) || hasLabel(current, staleLabel) ||
        parseTime(current.updated_at) > warnedAt ||
        protectedByPriority(priority, current.created_at, now)) {
      return false;
    }

    try {
      await github.rest.issues.addLabels({ ...issueRef, labels: [staleLabel] });
      return true;
    } catch (error) {
      if (!mayRetry || ![500, 502, 503, 504].includes(error.status)) {
        throw error;
      }
    }
  }
}

module.exports = async function runStaleIssues({
  github, context, core, now = Date.now(), dryRun = false, priorityProbeIssue,
}) {
  const repo = context.repo;
  const fieldId = await readPriorityField(github, repo);
  core.info('Verified access to the native Priority field');
  if (dryRun) {
    if (!Number.isSafeInteger(priorityProbeIssue) || priorityProbeIssue <= 0) {
      throw new Error('Provide a positive issue number with native Priority set for the dry run');
    }
    const probe = await readIssuePriority(github, { ...repo, issue_number: priorityProbeIssue }, fieldId);
    if (probe === null) {
      throw new Error('The probe issue must have a readable native Priority value');
    }
    core.info('Dry run verified a populated native Priority value');
  } else {
    await verifyLivePriorityRead(github, repo, fieldId);
    core.info('Verified a populated native Priority value for live processing');
  }
  const issues = await github.paginate(github.rest.issues.listForRepo, {
    ...repo, state: 'open', per_page: 100, sort: 'created', direction: 'desc',
  });

  const planned = { clear: 0, mark: 0, close: 0 };
  let failures = 0;
  for (const issue of issues) {
    try {
      if (!isCandidate(issue, now)) {
        continue;
      }
      const issueRef = { ...repo, issue_number: issue.number };
      let action = await staleAction(github, issueRef, issue, now);
      if (action === null) {
        continue;
      }

      const priority = await readIssuePriority(github, issueRef, fieldId);
      const { data: current } = await github.rest.issues.get(issueRef);
      if (!isCandidate(current, now) || issue.updated_at !== current.updated_at ||
          hasLabel(issue, staleLabel) !== hasLabel(current, staleLabel)) {
        continue;
      }

      if (protectedByPriority(priority, current.created_at, now)) {
        if (!hasLabel(current, staleLabel)) {
          continue;
        }
        action = 'clear';
      }
      if (dryRun) {
        planned[action]++;
        continue;
      }

      if (action === 'clear') {
        await github.rest.issues.removeLabel({ ...issueRef, name: staleLabel });
        core.info(`Cleared stale label on #${issue.number}`);
      } else if (action === 'mark') {
        const { data: warning } = await github.rest.issues.createComment({ ...issueRef, body: staleMessage });
        if (await addStaleLabelAfterWarning(github, issueRef, warning, fieldId, now)) {
          core.info(`Marked #${issue.number} stale`);
        } else {
          core.info(`Issue #${issue.number} changed after the stale warning`);
        }
      } else if (action === 'close') {
        await github.rest.issues.update({ ...issueRef, state: 'closed', state_reason: 'not_planned' });
        await github.rest.issues.createComment({ ...issueRef, body: closeMessage });
        core.info(`Closed #${issue.number} after inactivity`);
      }
    } catch (error) {
      const message = `Stale issue check for #${issue.number} failed: ${error.message}`;
      if ([401, 403, 429].includes(error.status)) {
        throw new Error(message, { cause: error });
      }
      failures++;
      core.error(message);
    }
  }
  if (dryRun) {
    core.info(`Dry run: mark ${planned.mark}, clear ${planned.clear}, close ${planned.close}`);
  }
  if (failures > 0) {
    throw new Error(`Stale issue processing failed for ${failures} issue(s)`);
  }
};
