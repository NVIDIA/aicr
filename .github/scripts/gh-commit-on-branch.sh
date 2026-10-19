#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Commit a set of file additions/deletions onto a branch through GitHub's
# GraphQL `createCommitOnBranch` mutation, so the commit carries GitHub's
# web-flow signature and shows the **Verified** badge (#1551).
#
# Why the API instead of `git push`: GitHub auto-signs only commits it creates
# server-side (REST contents API, GraphQL createCommitOnBranch, web editor,
# merge button). A commit that arrives via `git push` is never signed by
# GitHub, so a runner's client-side commit-back is always Unverified — the
# `github-actions[bot]` identity has no GPG/SSH key on the runner to `-S` with.
# createCommitOnBranch authors the commit as the GH_TOKEN identity and GitHub
# signs it.
#
# ## Why the request is assembled through files, never argv
#
# The mutation carries each file's FULL contents, base64-encoded. Linux caps a
# *single* argv string at MAX_ARG_STRLEN = 128 KiB (32 pages), independently of
# the much larger total ARG_MAX; macOS caps the total at 1 MiB. So every
# `jq --arg contents "$BASE64"` form dies with "Argument list too long" once the
# payload grows, and both previous copies of this logic did:
#
#   * render-golden-refresh.yaml passed pkg/bundler/testdata/stock_render_golden.yaml
#     (~436 KB raw, ~581 KB base64) as one `--arg`. It failed on every run where
#     the golden actually moved, which is the only case the workflow exists for.
#   * evidence-commit-signed.sh accumulated every file into one `--argjson
#     additions` value, so files individually under the cap still breached it
#     collectively.
#
# Every payload-sized value therefore reaches jq as `--rawfile` / `--slurpfile`
# (a path, not a value), and the finished request body is streamed to
# `gh api graphql --input <file>`. Only fixed-size scalars — repo, branch, oid,
# headline, body — travel through argv. Keep it that way: a single `--arg` on a
# file's contents silently reintroduces the bug, because it fails only once the
# payload is large enough, which is exactly when the automation matters.
#
# `expectedHeadOid` pins the mutation to the branch tip the caller resolved. A
# concurrent push anywhere on the branch during the generation window — a
# Renovate rebase, a maintainer push — fails the mutation loudly instead of
# silently committing content computed against a stale tree.
#
# Usage:
#   gh-commit-on-branch.sh --branch <name> --headline <text> \
#     [--repo <owner/repo>] [--body <text>] [--expected-head-oid <sha>] \
#     [--add <path>]... [--delete <path>]...
#
# `--add` / `--delete` paths are sent to GitHub verbatim and must therefore be
# repo-relative, exactly as they should appear in the tree — run from the
# repository root and pass the same paths `git diff --name-only` would print.
#
# Writes the new commit OID to stdout and nothing else, so callers can capture
# it. Progress and errors go to stderr.
#
# Required env:
#   GH_TOKEN   token authenticating `gh api`

set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: gh-commit-on-branch.sh --branch <name> --headline <text>
         [--repo <owner/repo>] [--body <text>] [--expected-head-oid <sha>]
         [--add <path>]... [--delete <path>]...

Writes the new commit OID to stdout. Requires GH_TOKEN.
USAGE
}

die() {
  echo "gh-commit-on-branch: $*" >&2
  exit 1
}

repo="${GITHUB_REPOSITORY:-}"
branch=""
headline=""
body=""
head_oid=""
add_paths=()
del_paths=()

while [ $# -gt 0 ]; do
  case "$1" in
    --repo)              repo="${2:?--repo needs a value}"; shift 2 ;;
    --branch)            branch="${2:?--branch needs a value}"; shift 2 ;;
    --headline)          headline="${2:?--headline needs a value}"; shift 2 ;;
    # `?` not `:?` — an empty body is legitimate, an absent one is not.
    --body)              body="${2?--body needs a value}"; shift 2 ;;
    --expected-head-oid) head_oid="${2:?--expected-head-oid needs a value}"; shift 2 ;;
    --add)               add_paths+=("${2:?--add needs a path}"); shift 2 ;;
    --delete)            del_paths+=("${2:?--delete needs a path}"); shift 2 ;;
    -h|--help)           usage; exit 0 ;;
    *)                   usage; die "unknown argument: $1" ;;
  esac
done

: "${GH_TOKEN:?GH_TOKEN is required (token authenticating gh api)}"
[ -n "$repo" ]     || die "--repo is required (or set GITHUB_REPOSITORY)"
[ -n "$branch" ]   || die "--branch is required"
[ -n "$headline" ] || die "--headline is required"

# An empty fileChanges set would create an empty commit rather than report that
# the caller computed nothing to commit. Callers that legitimately have nothing
# to do (evidence-commit-signed.sh's clean no-op) must not call this at all.
if [ "${#add_paths[@]}" -eq 0 ] && [ "${#del_paths[@]}" -eq 0 ]; then
  die "nothing to commit: pass at least one --add or --delete"
fi

# Default to the checked-out tip. Resolved here rather than defaulted to an
# empty string so an unset value can never reach the mutation as "no expected
# head", which would disable the concurrent-push guard.
if [ -z "$head_oid" ]; then
  head_oid="$(git rev-parse HEAD)" || die "could not resolve HEAD for --expected-head-oid"
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

additions="${work}/additions.ndjson"
deletions="${work}/deletions.ndjson"
: >"$additions"
: >"$deletions"

for path in ${add_paths[@]+"${add_paths[@]}"}; do
  [ -f "$path" ] || die "--add path is not a regular file: $path"
  # `base64 -w0` is GNU-only; piping through `tr` gets the same single line out
  # of BSD base64 too, so `make test-shell` exercises this on a macOS dev box
  # and not just on the ubuntu runner.
  base64 <"$path" | tr -d '\n' >"${work}/contents.b64"
  jq -nc --arg p "$path" --rawfile c "${work}/contents.b64" \
    '{path: $p, contents: $c}' >>"$additions"
done

for path in ${del_paths[@]+"${del_paths[@]}"}; do
  jq -nc --arg p "$path" '{path: $p}' >>"$deletions"
done

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

# `input` is an object variable, so it cannot be passed via
# `gh api graphql -f input=...` (that would send a string and fail
# type-checking) — build the full request body and stream it in.
jq -n \
  --arg query "$query" \
  --arg repo "$repo" \
  --arg branch "$branch" \
  --arg oid "$head_oid" \
  --arg headline "$headline" \
  --arg body "$body" \
  --slurpfile additions "$additions" \
  --slurpfile deletions "$deletions" \
  '{
    query: $query,
    variables: {
      input: {
        branch: {repositoryNameWithOwner: $repo, branchName: $branch},
        expectedHeadOid: $oid,
        message: {headline: $headline, body: $body},
        fileChanges: {additions: $additions, deletions: $deletions}
      }
    }
  }' >"${work}/request.json"

commit_oid="$(gh api graphql --input "${work}/request.json" \
  --jq '.data.createCommitOnBranch.commit.oid')"

# GraphQL reports a rejected mutation as a 200 with `data.createCommitOnBranch`
# null and an `errors` array, which `gh api` does not always surface as a
# non-zero exit. Without this check the caller would echo "Committed as null"
# and the job would go green having committed nothing.
if [ -z "$commit_oid" ] || [ "$commit_oid" = "null" ]; then
  die "createCommitOnBranch returned no commit oid (mutation rejected)"
fi

printf '%s\n' "$commit_oid"
