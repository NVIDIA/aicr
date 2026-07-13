#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Commit the signed/relocated evidence pointers back to the branch through
# GitHub's GraphQL `createCommitOnBranch` mutation so the commit carries
# GitHub's web-flow signature and shows the **Verified** badge (#1551).
#
# Why the API instead of `git push`: GitHub auto-signs only commits it creates
# server-side (REST contents API, GraphQL createCommitOnBranch, web editor,
# merge button). A commit that arrives via `git push` is never signed by
# GitHub, so the runner's client-side commit-back was always Unverified — the
# `github-actions[bot]` identity has no GPG/SSH key on the runner to `-S` with.
# createCommitOnBranch authors the commit as the GITHUB_TOKEN identity
# (github-actions[bot]) and GitHub signs it → Verified.
#
# The signing step's relocation is a delete (flat pointer) + add (nested
# pointer) plus an in-place signer patch, so the mutation sends the FULL
# fileChanges.additions / fileChanges.deletions set computed from the working
# tree against HEAD.
#
# Behavior preserved from the previous `git push` implementation:
#   * Clean no-op when nothing under recipes/evidence/ changed (nothing to
#     sign): exit 0 without creating a commit.
#   * DCO sign-off — a `Signed-off-by:` trailer matching the bot author is
#     added to the commit body so the DCO check passes on the commit-back.
#   * Loop guard — createCommitOnBranch runs with the default GITHUB_TOKEN, and
#     GitHub does not trigger workflow runs for token-authored commits, so the
#     commit-back does not re-trigger the sign workflow. The headline is
#     unchanged so the workflow's belt-and-suspenders `startsWith(...)` guard
#     still matches for any fork pushing via a PAT.
#
# Required env:
#   GH_TOKEN            token authenticating `gh api` (github.token)
#   GITHUB_REPOSITORY   owner/repo (provided by Actions)
#   GITHUB_REF_NAME     branch name to commit onto (provided by Actions)

set -euo pipefail

: "${GH_TOKEN:?GH_TOKEN is required (token authenticating gh api)}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required (owner/repo)}"
: "${GITHUB_REF_NAME:?GITHUB_REF_NAME is required (branch name)}"

# Match the bot author createCommitOnBranch stamps on the commit so the DCO
# sign-off trailer is consistent with the commit author.
readonly BOT_NAME="github-actions[bot]"
readonly BOT_EMAIL="41898282+github-actions[bot]@users.noreply.github.com"
readonly HEADLINE="chore(evidence): sign pending evidence pointers"

# Stage the relocation (delete flat + add nested + in-place signer patch) so an
# untracked relocated file is counted, then decide whether there is anything to
# commit. --no-renames splits every rename into a delete + add pair, which is
# exactly the shape createCommitOnBranch.fileChanges expects.
git add -A recipes/evidence/
if git diff --cached --quiet -- recipes/evidence/; then
  echo "No pointer changes to commit (nothing to sign)."
  exit 0
fi

additions='[]'
deletions='[]'
while IFS= read -r -d '' status && IFS= read -r -d '' path; do
  case "$status" in
    D)
      deletions=$(jq -c --arg p "$path" '. += [{path: $p}]' <<<"$deletions")
      ;;
    *)
      # A (add) or M (modify): send the full file contents, base64-encoded as
      # the GraphQL API requires. -w0 keeps it single-line (GNU coreutils on
      # the ubuntu runner).
      contents=$(base64 -w0 <"$path")
      additions=$(jq -c --arg p "$path" --arg c "$contents" \
        '. += [{path: $p, contents: $c}]' <<<"$additions")
      ;;
  esac
done < <(git diff --cached --name-status --no-renames -z -- recipes/evidence/)

# expectedHeadOid pins the mutation to the branch tip we checked out; a
# concurrent advance fails the mutation loudly (re-dispatch after pulling)
# rather than silently racing.
head_oid=$(git rev-parse HEAD)
body="Signed-off-by: ${BOT_NAME} <${BOT_EMAIL}>"

variables=$(jq -n \
  --arg repo "$GITHUB_REPOSITORY" \
  --arg branch "$GITHUB_REF_NAME" \
  --arg oid "$head_oid" \
  --arg headline "$HEADLINE" \
  --arg body "$body" \
  --argjson additions "$additions" \
  --argjson deletions "$deletions" \
  '{
    input: {
      branch: {repositoryNameWithOwner: $repo, branchName: $branch},
      expectedHeadOid: $oid,
      message: {headline: $headline, body: $body},
      fileChanges: {additions: $additions, deletions: $deletions}
    }
  }')

read -r -d '' query <<'GRAPHQL' || true
mutation ($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) {
    commit {
      oid
      url
    }
  }
}
GRAPHQL

# Post {query, variables} to the GraphQL endpoint. `input` is an object
# variable, so it cannot be passed via `gh api graphql -f input=...` (that
# would send a string and fail type-checking) — build the full request body
# and stream it in.
commit_oid=$(jq -n --arg q "$query" --argjson v "$variables" '{query: $q, variables: $v}' \
  | gh api graphql --input - --jq '.data.createCommitOnBranch.commit.oid')

echo "Committed signed pointers as ${commit_oid} (GitHub-signed, Verified)."
