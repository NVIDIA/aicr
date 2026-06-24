#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Recipe-evidence gate: discover leaf overlays affected by a PR diff,
# then per affected recipe check for an evidence pointer, verify the
# bundle, and compare its signed digest against the current recipe's
# canonical digest. Writes a Markdown report; never blocks the PR.
#
# A recipe is "affected" when the PR touches its overlay, an ancestor
# overlay in its base chain, a referenced component values file, a
# *changed* registry component it transitively references, or its own
# evidence pointer (recipes/evidence/<slug>.yaml) — so a pointer-only
# "refresh evidence" PR is verified rather than silently skipped.
#
# The report separates "protected" recipes — those with a committed
# pointer in recipes/evidence/, the set the gate actively verifies — from
# "other affected" recipes that have no evidence yet (best-effort, not a
# warning). This keeps a broad-impact PR from rendering dozens of alarming
# "missing" rows for recipes nobody has produced evidence for.
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
#   MAX_ROWS    cap on protected-recipe rows (default 80) to keep the
#               report under GitHub's ~65KB comment-body cap
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
#   * `aicr evidence verify` fetches OCI artifacts from PR-controlled
#     URLs; an allow-list of trusted registries is a follow-up.

set -euo pipefail

: "${AICR:?AICR is required (path to aicr binary)}"
: "${BASE_SHA:?BASE_SHA is required}"
: "${HEAD_SHA:?HEAD_SHA is required}"
: "${REPORT_OUT:?REPORT_OUT is required}"

REPO_URL="${REPO_URL:-}"
MAX_ROWS="${MAX_ROWS:-80}"
# Cap on the collapsed "other affected" list so a broad PR can't blow past
# GitHub's ~65KB comment-body limit.
OTHER_MAX="${OTHER_MAX:-200}"

# md_escape sanitizes a value rendered into a Markdown table cell. Slugs and
# verifier-supplied hints can in principle carry characters that break the
# table or the surrounding backticks; collapse newlines, escape pipes, and
# drop backticks so a value can't break out of a code span.
md_escape() {
  printf '%s' "$1" | tr '\n' ' ' | sed 's/|/\\|/g; s/`//g'
}

# log_sanitize strips characters from PR-controlled text before it is echoed
# into a `::warning::`/`::error::` workflow command, so the text cannot inject
# its own workflow commands. Collapses newlines and carriage returns, and
# neutralizes `::`.
log_sanitize() {
  printf '%s' "$1" | tr '\n\r' '  ' | sed 's/::/:_:/g'
}

# Three-dot range so we only see what the PR actually changed (relative
# to its merge base) — not main commits that landed after PR open.
if ! changed_files=$(git diff --name-only "${BASE_SHA}...${HEAD_SHA}" 2>&1); then
  echo "::error::git diff failed — cannot compute affected overlays"
  echo "git diff output: ${changed_files}"
  exit 1
fi

# A change to recipes/overlays/base.yaml is genuinely broad — it sits at
# the root of (almost) every base chain — so it still promotes all leaf
# recipes. A registry.yaml change is NOT treated as broad: instead we
# compute which component *entries* changed and promote only the recipes
# that transitively reference one of them (see changed_components + Rule 5).
promote_all=false
if printf '%s\n' "$changed_files" | grep -qxF 'recipes/overlays/base.yaml'; then
  promote_all=true
fi

# Component-level registry scoping (#1435). When recipes/registry.yaml
# changes, diff it at the component-entry level between BASE and HEAD and
# collect the names whose entry was added, removed, or modified. An
# `aws-efa`-only edit then flags only recipes that reference `aws-efa`,
# not every leaf. Empty when registry.yaml is unchanged or only changed
# cosmetically (no entry differs).
registry_changed=false
changed_components=""
if printf '%s\n' "$changed_files" | grep -qxF 'recipes/registry.yaml'; then
  registry_changed=true
  reg_base=$(mktemp)
  reg_head=$(mktemp)
  # A revision missing the file (e.g. registry.yaml newly added) yields an
  # empty doc, so every HEAD component reads as added — fail-open to broad
  # for that recipe set, which is the safe direction for a warning gate.
  git show "${BASE_SHA}:recipes/registry.yaml" >"$reg_base" 2>/dev/null || : >"$reg_base"
  git show "${HEAD_SHA}:recipes/registry.yaml" >"$reg_head" 2>/dev/null || : >"$reg_head"
  comp_names=$( { yq eval '.components[].name // ""' "$reg_base" 2>/dev/null || true; \
                  yq eval '.components[].name // ""' "$reg_head" 2>/dev/null || true; } \
                | grep -v '^$' | sort -u )
  while IFS= read -r cn; do
    [[ -z "$cn" ]] && continue
    # Pass the name via strenv() rather than string-interpolating it into the
    # yq expression: a PR-controlled component name must be data, not code.
    # strenv (not env) keeps it a string so a scalar-looking name (`true`,
    # `null`, `123`) isn't YAML-typed and made to miss its registry entry.
    b=$(CN="$cn" yq eval '.components[] | select(.name == strenv(CN))' "$reg_base" 2>/dev/null || true)
    h=$(CN="$cn" yq eval '.components[] | select(.name == strenv(CN))' "$reg_head" 2>/dev/null || true)
    if [[ "$b" != "$h" ]]; then
      changed_components="${changed_components}${cn}"$'\n'
    fi
  done <<<"$comp_names"
  changed_components=$(printf '%s' "$changed_components" | grep -v '^$' | sort -u || true)
  rm -f "$reg_base" "$reg_head"
  cc_count=$(printf '%s\n' "$changed_components" | grep -c . || true)
  echo "Changed registry components: ${cc_count}"
fi

# recipe_components <overlay-path> — print the sorted, unique set of
# component names a recipe resolves to, using the *same* engine the digest
# uses: `aicr recipe` resolves by criteria across all overlays, with an
# unconditional base.yaml merge and criteria-matched wildcard overlays
# (e.g. monitoring-hpa injecting prometheus-adapter). Hand-walking
# spec.base/spec.mixins misses base.yaml and wildcard injections, which
# would make Rule 5 a false-negative for base/wildcard components — exactly
# the registry entries this scoping targets. Resolving via aicr keeps the
# affected-set consistent with the digest comparison downstream.
recipe_components() {
  local overlay="$1" service accel os intent platform
  service=$(yq eval '.spec.criteria.service // ""' "$overlay" 2>/dev/null || true)
  accel=$(yq eval '.spec.criteria.accelerator // ""' "$overlay" 2>/dev/null || true)
  os=$(yq eval '.spec.criteria.os // ""' "$overlay" 2>/dev/null || true)
  intent=$(yq eval '.spec.criteria.intent // ""' "$overlay" 2>/dev/null || true)
  platform=$(yq eval '.spec.criteria.platform // ""' "$overlay" 2>/dev/null || true)

  local args=()
  [[ -n "$service" && "$service" != "null" ]] && args+=(--service "$service")
  [[ -n "$accel" && "$accel" != "null" ]] && args+=(--accelerator "$accel")
  [[ -n "$os" && "$os" != "null" ]] && args+=(--os "$os")
  [[ -n "$intent" && "$intent" != "null" ]] && args+=(--intent "$intent")
  [[ -n "$platform" && "$platform" != "null" ]] && args+=(--platform "$platform")

  : "${AICR_TIMEOUT:=30s}"
  # Return non-zero when the resolver itself fails (timeout / error), so the
  # caller can fail OPEN (include the recipe) rather than mistake a resolver
  # hiccup for "references no changed component" — a false negative is the
  # dangerous direction for a protection gate.
  local out
  if ! out=$(timeout "$AICR_TIMEOUT" "$AICR" recipe "${args[@]}" --format json 2>/dev/null); then
    return 1
  fi
  printf '%s\n' "$out" | jq -r '.componentRefs[].name // empty' 2>/dev/null | sort -u
}

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
        echo "::warning::cyclic base chain detected at overlay '$(log_sanitize "$name")' (re-visited '$(log_sanitize "$parent")')"
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
        echo "::warning::cyclic base chain detected at overlay '$(log_sanitize "$name")' (re-visited '$(log_sanitize "$parent")')"
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

  # Rule 4: the recipe's own evidence pointer was added or modified.
  # Refreshing evidence (`cp ./out/pointer.yaml recipes/evidence/<slug>.yaml`)
  # touches no overlay or values file, so without this rule a pointer-only
  # PR — the canonical "refresh evidence" workflow — would slip through
  # unverified. `grep -qxF` matches the whole line, so a sibling like
  # `recipes/evidence/${name}.yaml.bak` doesn't trigger it.
  if [[ "$include" == "false" ]]; then
    if printf '%s\n' "$changed_files" | grep -qxF "recipes/evidence/${name}.yaml"; then
      include=true
    fi
  fi

  # Rule 5: a changed registry component entry that this recipe
  # transitively references (component-level cascade scoping, #1435). Only
  # the recipes that actually reference a changed component are promoted —
  # not every leaf — so an `aws-efa`-only registry edit no longer flags
  # non-AWS recipes.
  if [[ "$include" == "false" && "$registry_changed" == "true" && -n "$changed_components" ]]; then
    if rc=$(recipe_components "$overlay"); then
      # Resolved cleanly: include only on a real intersection.
      if [[ -n "$rc" ]] && comm -12 <(printf '%s\n' "$rc") <(printf '%s\n' "$changed_components") | grep -q .; then
        include=true
      fi
    else
      # Resolver failed (aicr recipe timeout/error). Fail open: include the
      # recipe so a transient failure can't hide real drift on a protected one.
      echo "::warning::component resolution failed for $(log_sanitize "$overlay"); including conservatively"
      include=true
    fi
  fi

  if [[ "$include" == "true" ]]; then
    affected=$(echo "$affected" | jq -c --arg r "$name" '. + [$r]')
  fi
done

count=$(echo "$affected" | jq 'length')
echo "Affected leaf overlays: ${count}"

# Partition affected recipes into "protected" (a committed pointer exists
# in recipes/evidence/) and "other" (affected but no evidence yet). The
# gate actively verifies the protected set; the others are surfaced as a
# best-effort summary, not per-recipe warnings (#1432).
#
# A pointer present in BASE or HEAD counts as protected: a PR that *deletes*
# a pointer leaves no file at HEAD, but removing evidence from a previously
# protected recipe is a de-protection we must surface — not silently demote
# it to "no evidence yet". The protected loop renders such a recipe as
# "evidence pointer removed" rather than verifying it.
protected="[]"
others="[]"
while IFS= read -r slug; do
  [[ -z "$slug" ]] && continue
  if [[ -f "recipes/evidence/${slug}.yaml" ]] \
     || git cat-file -e "${BASE_SHA}:recipes/evidence/${slug}.yaml" 2>/dev/null; then
    protected=$(echo "$protected" | jq -c --arg r "$slug" '. + [$r]')
  else
    others=$(echo "$others" | jq -c --arg r "$slug" '. + [$r]')
  fi
done < <(echo "$affected" | jq -r '.[]')
protected_count=$(echo "$protected" | jq 'length')
other_count=$(echo "$others" | jq 'length')
echo "Protected (have pointers): ${protected_count}; other affected (no evidence yet): ${other_count}"

# Orphan-pointer check. The loop above only iterates existing overlays,
# so a pointer added/modified for a slug with no matching leaf overlay
# (a typo'd slug, or evidence for a retired recipe) never gets checked —
# it would pass the gate unverified, defeating its purpose. Diff the
# changed evidence pointers against the overlay set to surface those.
# Only pointers present on disk at HEAD count: a *deleted* pointer is
# absent here and is not an orphan (if its recipe still exists it is
# already flagged via Rule 4 and reported as missing-pointer). This
# shares the gate's basename==slug assumption (see header note 1).
orphans="[]"
while IFS= read -r pf; do
  if [[ -z "$pf" ]]; then continue; fi
  case "$pf" in
    recipes/evidence/*.yaml) ;;
    *) continue ;;
  esac
  pslug=$(basename "$pf" .yaml)
  if [[ -f "$pf" && ! -f "recipes/overlays/${pslug}.yaml" ]]; then
    orphans=$(echo "$orphans" | jq -c --arg s "$pslug" '. + [$s]')
  fi
done < <(printf '%s\n' "$changed_files")
orphan_count=$(echo "$orphans" | jq 'length')
echo "Orphan evidence pointers: ${orphan_count}"

# --- Report build ------------------------------------------------------

mkdir -p "$(dirname "$REPORT_OUT")"
: > "$REPORT_OUT"

{
  echo "## Recipe evidence check"
  echo
  if [[ "$promote_all" == "true" ]]; then
    echo "> **Broad impact:** \`recipes/overlays/base.yaml\` changed; every leaf recipe is"
    echo "> potentially affected. Recipes that carry committed evidence are verified below;"
    echo "> the rest have no evidence yet (best-effort)."
    echo
  elif [[ "$registry_changed" == "true" && -n "$changed_components" ]]; then
    echo "> **Registry change:** scoped to recipes that reference a changed component"
    echo "> entry in \`recipes/registry.yaml\` (not every leaf)."
    echo
  fi
} >> "$REPORT_OUT"

warnings=0

if [[ "$count" -eq 0 && "$orphan_count" -eq 0 ]]; then
  {
    echo "No leaf overlays affected by this PR."
    echo
    echo "_This gate is warning-only and never blocks merge._"
  } >> "$REPORT_OUT"
  echo "warnings=0"
  exit 0
fi

rows_written=0
rows_truncated=0

if [[ "$protected_count" -gt 0 ]]; then
  {
    echo "### Protected recipes"
    echo
    echo "Recipes with committed evidence (\`recipes/evidence/<slug>.yaml\`) that this PR affects: **${protected_count}**"
    echo
    echo "| Recipe | Verify | Digest match |"
    echo "|---|---|---|"
  } >> "$REPORT_OUT"
fi

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

  # Protected via a BASE pointer that this PR deletes (absent at HEAD).
  # Removing evidence from a previously protected recipe is a de-protection
  # to surface, not a file to verify.
  if [[ ! -f "$pointer" ]]; then
    echo "| \`$(md_escape "$slug")\` | :warning: evidence pointer removed | — |" >> "$REPORT_OUT"
    warnings=$((warnings + 1))
    rows_written=$((rows_written + 1))
    continue
  fi

  # Wall-clock cap on aicr invocations so a hung / tarpit OCI registry
  # behind a PR-controlled pointer URL can't burn the whole job budget.
  # `timeout` exits 124 on timeout; the existing verify-exit default
  # branch catches that without a code change.
  : "${AICR_TIMEOUT:=30s}"

  digest_err=$(mktemp)
  current_digest=""
  if ! current_digest=$(timeout "$AICR_TIMEOUT" "$AICR" evidence digest -r "$overlay" 2>"$digest_err"); then
    echo "::warning::digest failed for $(log_sanitize "$overlay"): $(log_sanitize "$(head -c 500 "$digest_err")")"
    echo "| \`$(md_escape "$slug")\` | — | :warning: could not compute current digest |" >> "$REPORT_OUT"
    warnings=$((warnings + 1))
    rows_written=$((rows_written + 1))
    rm -f "$digest_err"
    continue
  fi
  rm -f "$digest_err"

  verify_json=$(mktemp)
  verify_err=$(mktemp)
  set +e
  timeout "$AICR_TIMEOUT" "$AICR" evidence verify "$pointer" --format json >"$verify_json" 2>"$verify_err"
  verify_exit=$?
  set -e
  if [[ "$verify_exit" -ne 0 && -s "$verify_err" ]]; then
    # Echo the full stderr into the job log (not just the table) so a
    # maintainer can see the underlying cause beyond the classified row.
    echo "::warning::verify exit ${verify_exit} for $(log_sanitize "$pointer"): $(log_sanitize "$(head -c 1000 "$verify_err")")"
  fi
  rm -f "$verify_err"

  # Pull structured fields surfaced by `aicr evidence verify --format json`
  # (see pkg/evidence/verifier): VerifyResult.Exit (0 valid, 1 valid with
  # recorded phase failures, 2 invalid), the predicate digest, the
  # pending-signature flag, and — on Exit 2 — the classified failureCause
  # {class, httpStatus, hint}. We branch on the JSON `.exit`, not the OS exit
  # code, because aicr collapses both Exit 1 (ErrCodeConflict) and Exit 2
  # (ErrCodeInvalidRequest) to OS exit 2 (see pkg/errors/exitcode.go).
  result_exit=""
  signed_digest=""
  pending="false"
  cause_class=""
  cause_status=""
  cause_hint=""
  if [[ -s "$verify_json" ]]; then
    result_exit=$(jq -r '.exit // empty' "$verify_json" 2>/dev/null || true)
    signed_digest=$(jq -r '.predicate.recipe.digest // empty' "$verify_json" 2>/dev/null || true)
    pending=$(jq -r '.pending // false' "$verify_json" 2>/dev/null || echo false)
    cause_class=$(jq -r '.failureCause.class // empty' "$verify_json" 2>/dev/null || true)
    cause_status=$(jq -r '.failureCause.httpStatus // empty' "$verify_json" 2>/dev/null || true)
    cause_hint=$(jq -r '.failureCause.hint // empty' "$verify_json" 2>/dev/null || true)
  fi
  rm -f "$verify_json"

  # verify_ok tracks whether the bundle itself verified (exit 0/1). When it
  # did not, the verify cell already owns the single warning for this row, so
  # the digest section must not add a second one (a broken bundle is one
  # problem, not two).
  verify_ok=true
  if [[ -z "$result_exit" ]]; then
    # No parseable VerifyResult — verify errored before producing output
    # (bad input / early failure) or `timeout` killed it (exit 124).
    verify_cell=":x: verify error (exit ${verify_exit})"
    verify_ok=false
    warnings=$((warnings + 1))
  else
    case "$result_exit" in
      0)
        if [[ "$pending" == "true" ]]; then
          verify_cell=":hourglass: pending signature"
        else
          verify_cell=":white_check_mark: passed"
        fi
        ;;
      1)
        # Valid bundle whose recorded validator results show phase failures
        # — informational, not a gate warning.
        verify_cell=":warning: phase failures recorded (informational)"
        ;;
      2)
        # Render the classified, actionable cause instead of a bare
        # "invalid" (#1437): e.g. "invalid — registry-forbidden (HTTP 403):
        # make the fork's aicr-evidence package public".
        verify_cell=":x: invalid"
        if [[ -n "$cause_class" ]]; then
          verify_cell="${verify_cell} — ${cause_class}"
          [[ -n "$cause_status" ]] && verify_cell="${verify_cell} (HTTP ${cause_status})"
          [[ -n "$cause_hint" ]] && verify_cell="${verify_cell}: $(md_escape "$cause_hint")"
        fi
        verify_ok=false
        warnings=$((warnings + 1))
        ;;
      *)
        verify_cell=":x: verify error (result exit ${result_exit})"
        verify_ok=false
        warnings=$((warnings + 1))
        ;;
    esac
  fi

  if [[ -n "$signed_digest" ]]; then
    if [[ "$signed_digest" == "$current_digest" ]]; then
      digest_cell=":white_check_mark: matches"
    else
      digest_cell=":warning: stale (\`${signed_digest:0:12}…\` vs current \`${current_digest:0:12}…\`)"
      warnings=$((warnings + 1))
    fi
  else
    digest_cell=":warning: skipped (no signed digest)"
    # Only a warning when the bundle otherwise verified — a failed verify
    # already counted its single warning above.
    if [[ "$verify_ok" == "true" ]]; then
      warnings=$((warnings + 1))
    fi
  fi

  echo "| \`$(md_escape "$slug")\` | ${verify_cell} | ${digest_cell} |" >> "$REPORT_OUT"
  rows_written=$((rows_written + 1))
done < <(echo "$protected" | jq -r '.[]')

if [[ "$rows_truncated" -gt 0 ]]; then
  echo "| _… +${rows_truncated} more (truncated; raise MAX_ROWS or split the PR)_ | | |" >> "$REPORT_OUT"
fi

# Other affected recipes (no committed evidence). Surfaced as a collapsed
# summary, not per-recipe warnings: evidence is hardware-gated and most
# recipes have none yet, so flagging each as "missing" on a broad PR is
# noise. The set grows naturally as evidence is added.
if [[ "$other_count" -gt 0 ]]; then
  {
    echo
    echo "<details><summary>Other affected recipes without evidence yet: <b>${other_count}</b></summary>"
    echo
    echo "These recipes are affected by this PR but carry no committed evidence pointer, so there is"
    echo "nothing to verify. This is expected — evidence is hardware-gated and added over time."
    echo
  } >> "$REPORT_OUT"
  # Cap the list (OTHER_MAX) so a broad PR's collapsed section can't push the
  # comment past GitHub's ~65KB body limit; the count in the summary is still
  # the full total.
  other_listed=0
  other_omitted=0
  while IFS= read -r oslug; do
    [[ -z "$oslug" ]] && continue
    if [[ "$other_listed" -ge "$OTHER_MAX" ]]; then
      other_omitted=$((other_omitted + 1))
      continue
    fi
    echo "- \`$(md_escape "$oslug")\`" >> "$REPORT_OUT"
    other_listed=$((other_listed + 1))
  done < <(echo "$others" | jq -r '.[]')
  if [[ "$other_omitted" -gt 0 ]]; then
    echo "- _… +${other_omitted} more (list capped at ${OTHER_MAX})_" >> "$REPORT_OUT"
  fi
  {
    echo
    echo "</details>"
  } >> "$REPORT_OUT"
fi

if [[ "$orphan_count" -gt 0 ]]; then
  {
    echo
    echo "### Orphan evidence pointers"
    echo
    echo "Added or modified, but with **no matching leaf overlay** (\`recipes/overlays/<slug>.yaml\`)."
    echo "This gate keys evidence to a recipe by filename, so an orphan pointer is never verified —"
    echo "usually a typo'd slug or evidence left behind for a retired recipe. Rename it to match the"
    echo "overlay, or remove it."
    echo
    echo "| Pointer | Issue |"
    echo "|---|---|"
  } >> "$REPORT_OUT"
  while IFS= read -r oslug; do
    if [[ -z "$oslug" ]]; then continue; fi
    oslug_md=$(md_escape "$oslug")
    echo "| \`recipes/evidence/${oslug_md}.yaml\` | :warning: no \`recipes/overlays/${oslug_md}.yaml\` |" >> "$REPORT_OUT"
    warnings=$((warnings + 1))
  done < <(echo "$orphans" | jq -r '.[]')
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
echo "protected=${protected_count}"
echo "other_affected=${other_count}"
echo "orphan_pointers=${orphan_count}"
