#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Work out which evidence pointers the signing step changed and hand them to
# gh-commit-on-branch.sh, which commits them through GitHub's GraphQL
# `createCommitOnBranch` mutation so the commit shows the **Verified** badge
# (#1551). That script owns the mutation, the base64 encoding, and the
# expectedHeadOid concurrency guard; this one owns only what is specific to
# evidence — the staging rules, the headline, and the no-op condition.
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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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

args=()
while IFS= read -r -d '' status && IFS= read -r -d '' path; do
  case "$status" in
    D) args+=(--delete "$path") ;;
    *) args+=(--add "$path") ;;   # A (add) or M (modify)
  esac
done < <(git diff --cached --name-status --no-renames -z -- recipes/evidence/)

body="Signed-off-by: ${BOT_NAME} <${BOT_EMAIL}>"

# Base64 encoding, request assembly, and the expectedHeadOid concurrency guard
# all live in gh-commit-on-branch.sh, which hands file contents to jq as a path
# (--rawfile) instead of a value (--arg). What used to live here accumulated
# every file into one `--argjson additions` value, which breaches the 128 KiB
# Linux cap on a single argv string once the staged pointers total more than
# ~96 KB (#2893). It had never fired only because they are smaller than that.
commit_oid=$("${SCRIPT_DIR}/gh-commit-on-branch.sh" \
  --repo "$GITHUB_REPOSITORY" \
  --branch "$GITHUB_REF_NAME" \
  --headline "$HEADLINE" \
  --body "$body" \
  "${args[@]}")

echo "Committed signed pointers as ${commit_oid} (GitHub-signed, Verified)."
