// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const priorityLabels = new Map([
  ['Urgent', 'priority/P0'],
  ['High', 'priority/P0'],
  ['Medium', 'priority/P1'],
  ['Low', 'priority/P2'],
]);
const definitions = {
  'priority/P0': { color: 'd73a4a', description: 'Mirrors P0 issue priority' },
  'priority/P1': { color: 'e9962e', description: 'Mirrors P1 issue priority' },
  'priority/P2': { color: 'fbca04', description: 'Mirrors P2 issue priority' },
};
const managedNames = new Set(Object.keys(definitions).map(name => name.toLowerCase()));

function completeNodes(connection, description) {
  if (connection?.pageInfo?.hasNextPage !== false || !Array.isArray(connection.nodes) ||
      connection.nodes.some(node => node === null || typeof node !== 'object')) {
    throw new Error(`Incomplete ${description} data`);
  }
  return connection.nodes;
}

function databaseId(field) {
  const id = String(field?.fullDatabaseId);
  if (!/^[1-9]\d*$/.test(id)) {
    throw new Error('Missing native issue field ID');
  }
  return id;
}

async function readSnapshot(github, repo, issueNumber) {
  const { repository } = await github.graphql(
    `query($owner: String!, $repo: String!, $issueNumber: Int!) {
      repository(owner: $owner, name: $repo) {
        nameWithOwner
        viewerCanSeeIssueFields
        issueFields(first: 30) {
          pageInfo { hasNextPage }
          nodes {
            ... on IssueFieldCommon { name fullDatabaseId }
            ... on IssueFieldSingleSelect { id options { id name } }
          }
        }
        issueOrPullRequest(number: $issueNumber) {
          __typename
          ... on Issue {
            number
            labels(first: 100) { pageInfo { hasNextPage } nodes { name } }
            issueFieldValues(first: 30) {
              pageInfo { hasNextPage }
              nodes {
                __typename
                ... on IssueFieldValueCommon { field { ... on IssueFieldCommon { name fullDatabaseId } } }
                ... on IssueFieldSingleSelectValue { name optionId }
              }
            }
          }
        }
      }
    }`,
    { ...repo, issueNumber },
  );
  if (repository?.nameWithOwner?.toLowerCase() !== `${repo.owner}/${repo.repo}`.toLowerCase()) {
    throw new Error('Unable to verify the repository for the Priority label sync');
  }
  const issue = repository.issueOrPullRequest;
  if (issue?.__typename === 'PullRequest') {
    return null;
  }
  if (issue?.__typename !== 'Issue' || issue.number !== issueNumber) {
    throw new Error('Unable to verify the issue for the Priority label sync');
  }
  if (repository.viewerCanSeeIssueFields !== true) {
    throw new Error('Native issue fields are unreadable');
  }

  const priorityFields = completeNodes(repository.issueFields, 'native issue field')
    .filter(field => field.name === 'Priority');
  const field = priorityFields[0];
  if (priorityFields.length !== 1 || typeof field.id !== 'string' || !field.id || !Array.isArray(field.options) ||
      field.options.length !== priorityLabels.size ||
      field.options.some(option => !priorityLabels.has(option?.name) || typeof option.id !== 'string' || !option.id) ||
      new Set(field.options.map(option => option.name)).size !== priorityLabels.size ||
      new Set(field.options.map(option => option.id)).size !== priorityLabels.size) {
    throw new Error('Unable to verify the native Priority field and its options');
  }

  const fieldId = databaseId(field);
  const priorities = completeNodes(issue.issueFieldValues, 'issue field value')
    .filter(value => databaseId(value.field) === fieldId);
  const value = priorities[0];
  if (priorities.length > 1 || (value && (
    value.__typename !== 'IssueFieldSingleSelectValue' ||
    value.field.name !== 'Priority' ||
    !field.options.some(option => option.id === value.optionId && option.name === value.name)
  ))) {
    throw new Error('Unable to verify the native Priority value');
  }

  const labels = completeNodes(issue.labels, 'issue label').map(label => label.name);
  if (labels.some(label => typeof label !== 'string' || !label)) {
    throw new Error('Unable to verify the current issue labels');
  }
  return {
    fieldId,
    fieldNodeId: field.id,
    desired: value ? priorityLabels.get(value.name) : null,
    managed: labels.filter(label => managedNames.has(label.toLowerCase())),
  };
}

async function ensureLabel(github, repo, name) {
  const label = { ...repo, name };
  try {
    await github.rest.issues.getLabel(label);
    return;
  } catch (error) {
    if (error.status !== 404) {
      throw error;
    }
  }

  try {
    await github.rest.issues.createLabel({ ...label, ...definitions[name] });
  } catch (error) {
    if (error.status !== 422) {
      throw error;
    }
    try {
      await github.rest.issues.getLabel(label);
    } catch (readError) {
      throw readError.status === 404 ? error : readError;
    }
  }
}

function isInSync({ desired, managed }) {
  return desired === null
    ? managed.length === 0
    : managed.length === 1 && managed[0].toLowerCase() === desired.toLowerCase();
}

async function removeLabel(github, issue, name) {
  try {
    await github.rest.issues.removeLabel({ ...issue, name });
  } catch (error) {
    if (error.status !== 404) {
      throw error;
    }
    const { issue_number: issueNumber, ...repo } = issue;
    const snapshot = await readSnapshot(github, repo, issueNumber);
    if (!snapshot || snapshot.managed.some(label => label.toLowerCase() === name.toLowerCase())) {
      throw error;
    }
  }
}

module.exports = async function syncPriorityLabel({ github, context, core, issueNumber }) {
  if (!Number.isSafeInteger(issueNumber) || issueNumber <= 0) {
    throw new Error('Provide a positive issue number for the Priority label sync');
  }
  const eventField = context.payload?.issue_field;
  if (eventField?.name && eventField.name !== 'Priority') {
    return;
  }
  const repo = context.repo;
  const issue = { ...repo, issue_number: issueNumber };
  let current = await readSnapshot(github, repo, issueNumber);
  if (!current) {
    core.info(`Skipping Priority label sync for pull request #${issueNumber}`);
    return;
  }
  if (eventField?.id !== undefined && ![current.fieldId, current.fieldNodeId].includes(String(eventField.id))) {
    throw new Error('Priority event does not match the current native Priority field');
  }

  for (let attempt = 0; attempt < 2; attempt++) {
    if (isInSync(current)) {
      return;
    }
    const desired = current.desired;
    const desiredName = desired?.toLowerCase();
    const needsLabel = desired && !current.managed.some(label => label.toLowerCase() === desiredName);
    if (needsLabel) {
      await ensureLabel(github, repo, desired);
    }
    for (const label of current.managed) {
      if (label.toLowerCase() !== desiredName) {
        await removeLabel(github, issue, label);
      }
    }
    if (needsLabel) {
      await github.rest.issues.addLabels({ ...issue, labels: [desired] });
    }

    current = await readSnapshot(github, repo, issueNumber);
    if (current && isInSync(current)) {
      core.info(`Synced public Priority label for issue #${issueNumber}`);
      return;
    }
    if (!current || current.desired === desired) {
      break;
    }
  }
  throw new Error('Unable to verify the derived Priority label against the current issue field');
};
