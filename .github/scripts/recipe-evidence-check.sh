#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Recipe-evidence gate: discover leaf overlays affected by a PR diff,
# then per affected recipe check for an evidence pointer, verify the
# bundle, and compare its signed digest against the current recipe's
# canonical digest. Writes a Markdown report; never blocks the PR.
#
# Required env:
#   AICR        path to the aicr binary (built from a trusted source)
#   BASE_SHA    PR base SHA (target branch tip at PR creation)
#   HEAD_SHA    PR head SHA (PR branch tip; must be locally reachable)
#   REPORT_OUT  destination path for the Markdown report
#
# Optional env:
#   REPO_URL    base URL for absolute links in the report (e.g.
#               https://github.com/NVIDIA/aicr); when unset, the
#               trust-model link in the trailer is omitted
#   MAX_ROWS    cap on per-recipe rows (default 80) to keep the report
#               under GitHub's ~65KB comment-body cap on broad-impact PRs
#
# Local invocation:
#   make build  # or `go build -o ./bin/aicr ./cmd/aicr`
#   AICR=./bin/aicr \
#   BASE_SHA=$(git merge-base origin/main HEAD) \
#   HEAD_SHA=$(git rev-parse HEAD) \
#   REPORT_OUT=/tmp/report.md \
#   .github/scripts/recipe-evidence-check.sh
#
# Known limitations (tracked as follow-ups):
#   * Pointer file name is computed as `basename overlay .yaml`, while
#     `aicr validate --emit-attestation` writes pointers named after
#     the criteria-derived slug (RecipeNameFor). Overlays not following
#     the canonical naming will be reported as missing-pointer.
#   * Discovery walks `spec.base` ancestors but not `spec.mixins`, and
#     promote-all only matches recipes/registry.yaml or recipes/
#     overlays/base.yaml. Mixin/check/component edits outside literal
#     valuesFile refs are not promoted. Parity with kwok-recipes.
#   * `aicr evidence verify` fetches OCI artifacts from PR-controlled
#     URLs; an allow-list of trusted registries is a follow-up.

set -euo pipefail

: "${AICR:?AICR is required (path to aicr binary)}"
: "${BASE_SHA:?BASE_SHA is required}"
: "${HEAD_SHA:?HEAD_SHA is required}"
: "${REPORT_OUT:?REPORT_OUT is required}"

REPO_URL="${REPO_URL:-}"
MAX_ROWS="${MAX_ROWS:-80}"

# Three-dot range so we only see what the PR actually changed (relative
# to its merge base) — not main commits that landed after PR open.
if ! changed_files=$(git diff --name-only "${BASE_SHA}...${HEAD_SHA}" 2>&1); then
  echo "::error::git diff failed — cannot compute affected overlays"
  echo "git diff output: ${changed_files}"
  exit 1
fi

promote_all=false
if echo "$changed_files" | grep -qE '^recipes/(registry\.yaml|overlays/base\.yaml)$'; then
  promote_all=true
fi

affected="[]"
for overlay in recipes/overlays/*.yaml; do
  name=$(basename "$overlay" .yaml)
  service=$(yq eval '.spec.criteria.service // ""' "$overlay" 2>/dev/null || true)
  accel=$(yq eval '.spec.criteria.accelerator // ""' "$overlay" 2>/dev/null || true)

  # Leaf filter: skip intermediates and wildcards. Evidence is only
  # meaningful for user-selectable, hardware-bound recipes.
  if [[ -z "$service" || "$service" == "null" || "$service" == "any" ]]; then
    continue
  fi
  if [[ -z "$accel" || "$accel" == "null" || "$accel" == "any" ]]; then
    continue
  fi

  if [[ "$promote_all" == "true" ]]; then
    affected=$(echo "$affected" | jq -c --arg r "$name" '. + [$r]')
    continue
  fi

  include=false

  # Rule 1: overlay file itself changed. `grep -qxF` (eXact, Fixed
  # string) matches whole lines, so `recipes/overlays/foo.yaml.bak`
  # in the diff doesn't trigger the rule for `foo.yaml`.
  if printf '%s\n' "$changed_files" | grep -qxF "recipes/overlays/${name}.yaml"; then
    include=true
  fi

  # Rule 2: an ancestor overlay in the base chain changed.
  # Cycle guard: track visited overlays so a malformed `A→B→A` chain
  # doesn't hang the step. The aicr recipe builder would also reject
  # such a graph, but discovery runs before any aicr invocation.
  if [[ "$include" == "false" ]]; then
    current="$overlay"
    declare -A visited_r2=()
    while true; do
      parent=$(yq eval '.spec.base // ""' "$current" 2>/dev/null || true)
      if [[ -z "$parent" || "$parent" == "null" ]]; then break; fi
      if [[ -n "${visited_r2[$parent]:-}" ]]; then
        echo "::warning::cyclic base chain detected at overlay '${name}' (re-visited '${parent}')"
        break
      fi
      visited_r2[$parent]=1
      if printf '%s\n' "$changed_files" | grep -qxF "recipes/overlays/${parent}.yaml"; then
        include=true
        break
      fi
      current="recipes/overlays/${parent}.yaml"
      if [[ ! -f "$current" ]]; then break; fi
    done
    unset visited_r2
  fi

  # Rule 3: a component values file referenced by this overlay or any
  # ancestor changed.
  if [[ "$include" == "false" ]]; then
    values_files=""
    current="$overlay"
    declare -A visited_r3=()
    while true; do
      vf=$(yq eval '.spec.componentRefs[].valuesFile // ""' "$current" 2>/dev/null | grep -v '^$' || true)
      if [[ -n "$vf" ]]; then
        values_files="${values_files}"$'\n'"${vf}"
      fi
      parent=$(yq eval '.spec.base // ""' "$current" 2>/dev/null || true)
      if [[ -z "$parent" || "$parent" == "null" ]]; then break; fi
      if [[ -n "${visited_r3[$parent]:-}" ]]; then
        echo "::warning::cyclic base chain detected at overlay '${name}' (re-visited '${parent}')"
        break
      fi
      visited_r3[$parent]=1
      current="recipes/overlays/${parent}.yaml"
      if [[ ! -f "$current" ]]; then break; fi
    done
    unset visited_r3
    while IFS= read -r vf; do
      if [[ -z "$vf" ]]; then continue; fi
      if printf '%s\n' "$changed_files" | grep -qxF "recipes/${vf}"; then
        include=true
        break
      fi
    done <<<"$values_files"
  fi

  if [[ "$include" == "true" ]]; then
    affected=$(echo "$affected" | jq -c --arg r "$name" '. + [$r]')
  fi
done

count=$(echo "$affected" | jq 'length')
echo "Affected leaf overlays: ${count}"

# --- Report build ------------------------------------------------------

mkdir -p "$(dirname "$REPORT_OUT")"
: > "$REPORT_OUT"

{
  echo "## Recipe evidence check"
  echo
  if [[ "$promote_all" == "true" ]]; then
    echo "> **Broad impact:** \`recipes/registry.yaml\` or \`recipes/overlays/base.yaml\` changed;"
    echo "> every leaf recipe is potentially affected. The list below covers all of them — each"
    echo "> one would ideally have refreshed evidence before merge."
    echo
  fi
} >> "$REPORT_OUT"

warnings=0

if [[ "$count" -eq 0 ]]; then
  {
    echo "No leaf overlays affected by this PR."
    echo
    echo "_This gate is warning-only and never blocks merge._"
  } >> "$REPORT_OUT"
  echo "warnings=0"
  exit 0
fi

{
  echo "Affected leaf overlays: **${count}**"
  echo
  echo "| Recipe | Pointer | Verify | Digest match |"
  echo "|---|---|---|---|"
} >> "$REPORT_OUT"

rows_written=0
rows_truncated=0

# `while IFS= read -r` (not `for x in $(...)`) so slugs with shell
# metachars don't word-split or glob-expand.
while IFS= read -r slug; do
  if [[ -z "$slug" ]]; then continue; fi
  if [[ "$rows_written" -ge "$MAX_ROWS" ]]; then
    rows_truncated=$((rows_truncated + 1))
    continue
  fi

  overlay="recipes/overlays/${slug}.yaml"
  pointer="recipes/evidence/${slug}.yaml"

  # Wall-clock cap on aicr invocations so a hung / tarpit OCI registry
  # behind a PR-controlled pointer URL can't burn the whole job budget.
  # `timeout` exits 124 on timeout; the existing verify-exit default
  # branch catches that without a code change.
  : "${AICR_TIMEOUT:=30s}"

  digest_err=$(mktemp)
  current_digest=""
  if ! current_digest=$(timeout "$AICR_TIMEOUT" "$AICR" evidence digest -r "$overlay" 2>"$digest_err"); then
    echo "::warning::digest failed for ${overlay}: $(head -c 500 "$digest_err")"
    echo "| \`${slug}\` | — | — | :warning: could not compute current digest |" >> "$REPORT_OUT"
    warnings=$((warnings + 1))
    rows_written=$((rows_written + 1))
    rm -f "$digest_err"
    continue
  fi
  rm -f "$digest_err"

  if [[ ! -f "$pointer" ]]; then
    echo "| \`${slug}\` | :warning: missing | — | — |" >> "$REPORT_OUT"
    warnings=$((warnings + 1))
    rows_written=$((rows_written + 1))
    continue
  fi

  verify_json=$(mktemp)
  verify_err=$(mktemp)
  set +e
  timeout "$AICR_TIMEOUT" "$AICR" evidence verify "$pointer" --format json >"$verify_json" 2>"$verify_err"
  verify_exit=$?
  set -e
  if [[ "$verify_exit" -ne 0 && -s "$verify_err" ]]; then
    echo "::warning::verify exit ${verify_exit} for ${pointer}: $(head -c 500 "$verify_err")"
  fi
  rm -f "$verify_err"

  # aicr maps both ErrCodeConflict (phase failures recorded; bundle
  # valid) and ErrCodeInvalidRequest (bundle invalid) to OS exit 2 —
  # see pkg/errors/exitcode.go. Disambiguate via whether the JSON
  # payload contains a parseable predicate.
  signed_digest=""
  if [[ -s "$verify_json" ]]; then
    signed_digest=$(jq -r '.predicate.recipe.digest // empty' "$verify_json" 2>/dev/null || true)
  fi

  case "$verify_exit" in
    0)
      verify_cell=":white_check_mark: passed"
      ;;
    2)
      if [[ -n "$signed_digest" ]]; then
        verify_cell=":warning: phase failures recorded (informational)"
      else
        verify_cell=":x: bundle invalid"
      fi
      ;;
    *)
      verify_cell=":x: verify error (exit ${verify_exit})"
      ;;
  esac

  if [[ -n "$signed_digest" ]]; then
    if [[ "$signed_digest" == "$current_digest" ]]; then
      digest_cell=":white_check_mark: matches"
    else
      digest_cell=":warning: stale (\`${signed_digest:0:12}…\` vs current \`${current_digest:0:12}…\`)"
      warnings=$((warnings + 1))
    fi
  else
    digest_cell=":warning: skipped (verify did not surface a signed digest)"
    warnings=$((warnings + 1))
  fi

  echo "| \`${slug}\` | :white_check_mark: present | ${verify_cell} | ${digest_cell} |" >> "$REPORT_OUT"
  rows_written=$((rows_written + 1))
  rm -f "$verify_json"
done < <(echo "$affected" | jq -r '.[]')

if [[ "$rows_truncated" -gt 0 ]]; then
  echo "| _… +${rows_truncated} more (truncated; raise MAX_ROWS or split the PR)_ | | | |" >> "$REPORT_OUT"
fi

{
  echo
  if [[ "$warnings" -gt 0 ]]; then
    echo "### How to refresh evidence"
    echo
    echo "Run on a cluster matching the recipe's \`criteria\`:"
    echo
    echo '```shell'
    echo "aicr snapshot -o snapshot.yaml"
    echo "aicr validate \\"
    echo "  -r recipes/overlays/<slug>.yaml \\"
    echo "  -s snapshot.yaml \\"
    echo "  --emit-attestation ./out \\"
    echo "  --push ghcr.io/<your-fork>/aicr-evidence"
    echo "cp ./out/pointer.yaml recipes/evidence/<slug>.yaml"
    echo '```'
    echo
  fi
  if [[ -n "$REPO_URL" ]]; then
    echo "_This gate is warning-only and never blocks merge. See [ADR-007](${REPO_URL}/blob/main/docs/design/007-recipe-evidence.md) for the trust model._"
  else
    echo "_This gate is warning-only and never blocks merge._"
  fi
} >> "$REPORT_OUT"

echo "warnings=${warnings}"
echo "rows_written=${rows_written}"
echo "rows_truncated=${rows_truncated}"
