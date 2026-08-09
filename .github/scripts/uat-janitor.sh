#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# UAT orphan janitor — the backstop for the in-run teardown (uat-{gcp,aws,azure}
# .yaml "Destroy Cluster"). A UAT run can leave its cluster + full resource group
# (GPU node, VPC network group, router/NAT, resource group, TF state) behind when
# teardown never runs: a runner hard-killed mid-cancel, a daytime-up held cluster
# whose evening daytime-down never fired, a manual skip_delete run abandoned, or a
# Bringup "failure" that stranded a healthy cluster. Those orphans accumulate until
# they exhaust a cloud quota (in Aug 2026 the GCP NETWORKS quota, 50, hit 48/50).
#
# This reaps them. For every cloud resource whose name carries the UAT deployment
# id (aicr-uat-<run_id> nightly and aicr-uat-day-<reservation-or-slug>-<run_id>
# daytime — see the ALLOWLIST below for both forms), it
# asks GitHub whether the OWNING run <run_id> is still active — and only reaps once
# the run is finished and old enough. It reaps via the SAME actuator `destroy` the
# in-run teardown uses, so Terraform handles resource ordering (router/NAT before
# VPC, the GCP default-route quirk, the Azure resource group) correctly.
#
# Safety model (this deletes cloud infrastructure — read before editing):
#   * ALLOWLIST — a candidate must match one of the two deployment-id schemas,
#     anchored end to end:
#       ^aicr-uat-[0-9]+$                 nightly, all clouds
#       ^aicr-uat-day-[a-z0-9-]+-[0-9]+$  daytime, all clouds. The middle segment
#                                         is deliberately permissive so it matches
#                                         BOTH the ADR-017 name now used on main —
#                                         aicr-uat-day-<slug>-<slot>-<run_id>
#                                         (for example gh1-0) — AND the legacy
#                                         aicr-uat-day-<reservation>-<run_id>
#                                         form (for example gcp-h100) during the
#                                         transition. Tighten to the slug/slot
#                                         shape once no legacy-held clusters remain.
#                                         Still anchored: the aicr-uat-day- prefix
#                                         and a trailing numeric run_id are required.
#     (GCP nightly stays aicr-uat-<run_id> too: the GKE actuator now bounds the
#     derived node-SA account_id — mchmarny/cluster#36, image >= v0.5.17.)
#     A prefix test would admit aicr-uat-unrelated-7, which no workflow produces;
#     the anchors reject it. Persistent infra (aicr-testgrid-*, anything else)
#     matches neither schema and is never in scope. Per-cloud discovery is
#     deliberately coarser — it only nominates CANDIDATES; classify() is the
#     authoritative gate, and run_id_of/is_daytime remain defense in depth.
#   * RUN-ID REQUIRED — a name with no trailing numeric run_id is skipped. We
#     never guess.
#   * NOT-ACTIVE — the owning run must be `completed` (or purged/404). This is an
#     allowlist: queued / in_progress / waiting / pending / requested are skipped
#     because the cluster is live, and ANY other status — including one GitHub
#     adds after this was written — is skipped as indeterminate.
#   * FAIL CLOSED — any GitHub API error that is not a definitive run-level 404
#     skips the candidate this cycle. We never delete on ambiguous liveness.
#     "Definitive" is narrow on purpose: the runs endpoint must first be proven
#     reachable (else a repo-level 404 would mark every candidate an orphan), the
#     error must be an `HTTP 404` and not merely text containing "404", and a
#     200 whose body lacks `status`/`updated_at` is ambiguous, not a green light.
#     An unparsable `updated_at` skips rather than reading as infinitely old.
#   * AGE FLOOR — keyed off the RUN's updated_at (uniform across clouds), not the
#     cloud resource's create time. Nightly/ad-hoc: NIGHTLY_MIN_AGE_HOURS (default
#     24h). Well above the 280-min job cap, so a still-finishing run's just-created
#     cluster is never touched — and, deliberately, long enough that a nightly
#     `skip_delete` run (a supported debugging hold, uat-{aws,azure,gcp}.yaml) can
#     survive a full working day before the janitor reclaims it. There is NO API
#     signal that distinguishes a skip_delete hold from a genuine orphan, so the
#     floor is the only lever: a real mid-cancel orphan therefore also persists up
#     to ~a day, which is acceptable — quota pressure builds over days, and the
#     hourly janitor reaps within the hour once the floor clears. (Lower it only if
#     a cloud's quota headroom demands faster reclamation.) Daytime:
#     DAYTIME_MIN_AGE_HOURS (default 24h). daytime-up is on-demand (workflow_dispatch), so its run is
#     already `completed` while the cluster is legitimately HELD for the working
#     day; the sole daytime schedule is the evening daytime-down (uat-daytime.yaml,
#     cron '0 2 * * *' UTC), which reaps the held cluster each night. A legitimate
#     hold therefore always lasts < 24h (up just after one 02:00 down, torn down at
#     the next), so a daytime cluster older than 24h has provably survived an
#     evening daytime-down = an orphan the nightly teardown missed. (Do NOT lower
#     this to the nightly floor: a completed daytime-up run does not mean the
#     cluster should be gone.)
#   * DRY_RUN — defaults to true. It reports every decision and never deletes until
#     a caller sets DRY_RUN=false (the workflow's `enforce` dispatch input). Only
#     the exact strings `true`/`false` are accepted; anything else aborts before
#     discovery rather than being resolved toward enforce. In dry-run the summary
#     counts "would-reap", never "reaped" — a report must not claim a destroy
#     that did not happen.
#
# Usage:   DRY_RUN=true ./uat-janitor.sh <gcp|aws|azure>
# Requires: gh (GH_TOKEN in env, actions:read), jq, yq, docker, and the cloud CLI
#           + credentials for <cloud> already configured by the caller.

set -euo pipefail

CLOUD="${1:?usage: uat-janitor.sh <gcp|aws|azure>}"
DRY_RUN="${DRY_RUN:-true}"
REPO="${GITHUB_REPOSITORY:-NVIDIA/aicr}"
# AWS/Azure discovery only. GCP matches a second shape too (see ALLOWLIST above),
# so it spells its patterns out inline rather than using this.
NAME_PREFIX="aicr-uat-"
NIGHTLY_MIN_AGE_HOURS="${NIGHTLY_MIN_AGE_HOURS:-24}"
DAYTIME_MIN_AGE_HOURS="${DAYTIME_MIN_AGE_HOURS:-24}"

# Directory of this script, so sibling helpers (uat-aws-cleanup-lb.sh) resolve
# regardless of the caller's CWD.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Probed once by main(). A path-level 404 (repo renamed, GITHUB_REPOSITORY wrong,
# token missing actions:read) is indistinguishable from a purged-run 404 when
# looking at a single candidate — and would classify the ENTIRE fleet as orphaned.
# So a 404 is only ever read as "run purged" once the runs endpoint itself is
# proven reachable. Default `no` keeps a sourced/never-probed caller fail-closed.
REPO_REACHABLE="${REPO_REACHABLE:-no}"

log() { echo "[janitor:${CLOUD}] $*"; }

# --- pure decision helpers -------------------------------------------------

# Trailing all-digit segment (the run id). Fails (non-zero) if the last
# hyphen-delimited segment is not purely numeric — we then skip the candidate.
run_id_of() {
  local last="${1##*-}"
  case "$last" in
    '' | *[!0-9]*) return 1 ;;
  esac
  printf '%s' "$last"
}

is_daytime() { case "$1" in aicr-uat-day-*) return 0 ;; *) return 1 ;; esac; }

# DRY_RUN gates every destroy, and only the exact string `true` holds it back.
# Anything else would select the enforce path, so a typo (DRY_RUN=flase) or a
# shell-truthy value (1, yes, TRUE) must be rejected outright rather than read as
# permission to delete. Only `true`/`false` are accepted.
valid_dry_run() { case "$1" in true | false) return 0 ;; *) return 1 ;; esac; }

# The age floors gate every destroy on the run's age. A non-integer floor (e.g.
# "24h") makes `[ "$age" -lt "$min" ]` error under an `if` — read as false, i.e.
# REAP — and a negative floor clears for every age. The shipped workflow never
# sets these, but the script accepts them as configuration, so validate them up
# front like DRY_RUN. `*[!0-9]*` rejects empty, non-numeric, and negative alike;
# the length cap rejects a value big enough to overflow bash arithmetic (past
# ~2^63 the same `-lt` test errors with status 2 and fails open to REAP). Five
# digits (<=99999h ~= 11y) is far more than any real floor and well under overflow.
valid_age_floor() {
  case "$1" in '' | *[!0-9]*) return 1 ;; esac
  [ "${#1}" -le 5 ]
}

# Whole hours between an ISO-8601 timestamp and now (GNU date; runner is ubuntu).
# Returns non-zero on an unparsable timestamp rather than substituting an epoch:
# a 0 would read as an infinitely-old deployment and clear every age floor.
age_hours_since() {
  local t
  t="$(date -u -d "$1" +%s 2>/dev/null)" || return 1
  [ -n "$t" ] || return 1
  echo $(( ( $(date -u +%s) - t ) / 3600 ))
}

# Is a UAT lifecycle run currently in flight? A daytime cluster's NAME carries the
# daytime-UP run id, so classify()'s run-status gate cannot see an evening
# daytime-DOWN tearing down the SAME reservation (a different run). The teardown
# always executes as a uat-run.yaml run — whether the evening scheduler dispatched
# it (uat-daytime.yaml's drive job dispatches uat-run.yaml as its own top-level
# run) or an operator ran `gh workflow run uat-run.yaml -f lifecycle=daytime-down`
# directly — so THAT is the workflow we watch, not the scheduler wrapper (whose
# watcher can time out before the child). Coarse but safe: ANY in-flight uat-run
# run defers ALL daytime reaps this cycle (the janitor is hourly, so the cost is a
# one-cycle delay).
#
# This is a liveness PROBE, not a mutex: it collapses the common race (a
# scheduled/dispatched down already running) to near zero, but a residual TOCTOU
# window remains — a down that STARTS in the ~1h between this check and the reap.
# The 24h floor and DRY_RUN-default backstop that window; a shared cross-workflow
# lease (#2132) is the tracked follow-up for a hard "never races" guarantee.
#
# Fails CLOSED: any API error, or a 200 whose body is not a well-formed run list
# (`{"message":...}`, `{"workflow_runs":null}`), reads as "active" (defer) rather
# than idle. Uses `gh` raw + external `jq` (like classify()) so the harness drives it.
LIFECYCLE_WORKFLOW="uat-run.yaml"
uat_lifecycle_active() {
  local json count
  # Filter to in_progress SERVER-SIDE (?status=in_progress) rather than reading the
  # newest N mixed-status runs and filtering here — that removes any dependence on
  # dispatch volume fitting in one page. in_progress is the state of a run actually
  # executing a teardown; a run still queued / about to start is the documented
  # TOCTOU residual, backstopped by the 24h floor + DRY_RUN default. per_page=100
  # is far above the number of concurrently-executing uat-run runs (the lease
  # serializes per reservation), so the count is effectively unbounded for our need.
  json="$(gh api "repos/${REPO}/actions/workflows/${LIFECYCLE_WORKFLOW}/runs?status=in_progress&per_page=100" 2>/dev/null)" || return 0
  # Fail CLOSED on a malformed 200: require an actual array, else emit a non-numeric
  # marker the case below reads as "active" (defer). `.workflow_runs | length` on a
  # missing/null field would otherwise be 0 = idle.
  count="$(jq -r 'if (.workflow_runs | type) == "array" then (.workflow_runs | length) else "malformed" end' <<<"$json" 2>/dev/null)" || return 0
  case "$count" in '' | *[!0-9]*) return 0 ;; esac
  [ "$count" -gt 0 ]
}

# Echo "REAP" or "SKIP:<reason>" for a deployment id. Consults the GitHub run for
# both liveness and age so the decision is identical across clouds.
classify() {
  local id="$1" run_id json status upd age min
  # Exactly the two supported deployment-id schemas (see the ALLOWLIST bullet in
  # the header) — fully anchored, so merely starting with `aicr-` is not enough and
  # a shape we do not generate (aicr-uat-unrelated-7, aicr-testgrid-vpc) can never
  # reach the actuator. discover_* is deliberately coarser; THIS is the gate.
  if [[ ! "$id" =~ ^aicr-uat-[0-9]+$ ]] &&
     [[ ! "$id" =~ ^aicr-uat-day-[a-z0-9-]+-[0-9]+$ ]]; then
    echo "SKIP:not-allowlisted"; return
  fi
  run_id="$(run_id_of "$id")" || { echo "SKIP:no-run-id"; return; }

  local errf; errf="$(mktemp)"
  if json="$(gh api "repos/${REPO}/actions/runs/${run_id}" 2>"$errf")"; then
    rm -f "$errf"
    status="$(jq -r '.status // empty' <<<"$json")"
    upd="$(jq -r '.updated_at // empty' <<<"$json")"
    if [ -z "$status" ] || [ -z "$upd" ]; then
      echo "SKIP:incomplete-run-json(run ${run_id})"; return   # fail closed
    fi
  elif [ "$REPO_REACHABLE" = yes ] && grep -q 'HTTP 404' "$errf"; then
    rm -f "$errf"
    # Run purged from history (GitHub retains ~months) => long dead => orphan.
    status="completed"; upd=""
  else
    rm -f "$errf"
    echo "SKIP:gh-api-error(run ${run_id})"; return   # fail closed
  fi

  # Allowlist, not denylist: ONLY a confirmed `completed` run may be reaped. The
  # active statuses are named separately because they are a routine skip an
  # operator should not have to think about; anything else is a status GitHub did
  # not have when this was written, and fails closed rather than falling through.
  case "$status" in
    completed) ;;
    queued | in_progress | waiting | pending | requested)
      echo "SKIP:run-active(${status},run ${run_id})"; return ;;
    *)
      echo "SKIP:run-status-unsupported(${status},run ${run_id})"; return ;;
  esac

  if [ -n "$upd" ]; then
    age="$(age_hours_since "$upd")" || { echo "SKIP:bad-updated-at(${upd})"; return; }
  else
    age=999999   # confirmed run-level 404: purged from history, past every floor
  fi
  if is_daytime "$id"; then min="$DAYTIME_MIN_AGE_HOURS"; else min="$NIGHTLY_MIN_AGE_HOURS"; fi
  if [ "$age" -lt "$min" ]; then
    echo "SKIP:too-young(${age}h < ${min}h)"; return
  fi
  # Daytime last-mile race guard (see the concurrency note in uat-janitor.yaml):
  # only daytime needs it — a nightly cluster's owning run IS the one the gate
  # above already checked, but a daytime name points at the daytime-UP run, not
  # the daytime-DOWN run (a uat-run.yaml run) that tears it down. Defer if any
  # uat-run lifecycle run is in flight. Reduces, does not eliminate, the race
  # (residual TOCTOU per uat_lifecycle_active); fails closed.
  if is_daytime "$id" && uat_lifecycle_active; then
    echo "SKIP:lifecycle-active(run ${run_id})"; return
  fi
  echo "REAP"
}

# --- reaping (actuator destroy) --------------------------------------------

# The Azure reap stages a copy of the runner's az credentials for the actuator
# container. This is module-level (not a `local`) so main() can arm an EXIT trap
# for it BEFORE any credential is written, which a `trap ... RETURN` inside reap
# cannot do safely: a RETURN trap set in a function is not cleared when that
# function returns, so it fires again on the caller's return with the variable
# out of scope — under `set -e` that aborts the whole janitor mid-sweep.
AZ_MOUNT=""
cleanup_az_mount() {
  [ -n "$AZ_MOUNT" ] || return 0
  rm -rf "$AZ_MOUNT"
  AZ_MOUNT=""
}

# Break a stale Terraform state lock before destroying. Orphans from a run
# cancelled mid-bringup carry an orphaned lock that would fail every destroy
# attempt (see uat-*.yaml teardown). Best-effort; a genuinely-absent lock is the
# common case. AWS has no state lock (S3 backend without dynamodb/use_lockfile).
break_state_lock() {
  local id="$1" loc
  case "$CLOUD" in
    gcp)
      loc="$(yq -r '.deployment.location' "$JANITOR_CONFIG")"
      gcloud storage rm \
        "gs://cluster-state-${GCP_PROJECT_ID}/deployments/${loc}/${id}/default.tflock" \
        2>/dev/null && log "cleared stale GCS state lock for ${id}" || true
      ;;
    azure)
      loc="$(yq -r '.deployment.location' "$JANITOR_CONFIG")"
      local rg="cluster-state-rg" sa key blob="deployments/${loc}/${id}/terraform.tfstate"
      sa="$(az storage account list -g "$rg" --subscription "$AZURE_SUBSCRIPTION_ID" \
        --query "[?starts_with(name, 'clst')].name | [0]" -o tsv 2>/dev/null || true)"
      if [ -n "${sa:-}" ]; then
        key="$(az storage account keys list -g "$rg" -n "$sa" \
          --subscription "$AZURE_SUBSCRIPTION_ID" --query '[0].value' -o tsv 2>/dev/null || true)"
        # Hand the account key to `az` through AZURE_STORAGE_KEY, not --account-key,
        # so the full-access tfstate key never appears in argv (world-readable via
        # /proc/<pid>/cmdline). Mirrors the proven pattern in uat-azure.yaml teardown.
        AZURE_STORAGE_ACCOUNT="$sa" AZURE_STORAGE_KEY="$key" \
          az storage blob lease break --account-name "$sa" \
          --container-name tfstate --blob-name "$blob" >/dev/null 2>&1 \
          && log "broke stale state-blob lease for ${id}" || true
      fi
      ;;
  esac
}

# One EKS actuator destroy pass for the staged config <cfg>. Factored out so the
# AWS reap can run it, sweep leaked LB resources, and run it again.
aws_actuator_destroy() {
  docker run --rm \
    -e CONFIG_CONTENT="$(base64 <"$1" | tr -d '\n')" \
    -e AUTO_APPROVE=true \
    -e AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
    -e AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
    -e AWS_SESSION_TOKEN="${AWS_SESSION_TOKEN:-}" \
    "$JANITOR_ACTUATOR_IMAGE" apply
}

# Destroy one orphaned deployment via the actuator (destroy == apply w/ destroy=true).
# Returns the actuator's exit status so the caller counts only destroys that
# actually happened — the rollout asks operators to trust these logs before
# enabling enforce, so a reported reap must be a real one.
reap() {
  local id="$1" cfg rc=0
  # `reap` is invoked as `if reap "$id"`, which disables errexit for this whole
  # body — a failure while staging the destroy config does NOT abort here. So
  # check each step explicitly and bail BEFORE docker: an unmodified or
  # half-written config still carries the committed placeholder deployment.id and
  # destroy=false, which in ENFORCE mode would make the actuator APPLY
  # (provision/update) a cluster while this logged a reap. Fail closed instead.
  if ! cfg="$(mktemp --suffix=.yaml)"; then
    echo "::warning::janitor could not create temp config for ${id}"
    return 1
  fi
  if ! yq '.' "$JANITOR_CONFIG" >"$cfg" ||
     ! DEPLOY_ID="$id" yq -i '.deployment.id = strenv(DEPLOY_ID) | .deployment.destroy = true' "$cfg"; then
    echo "::warning::janitor could not stage destroy config for ${id} (destroy NOT attempted)"
    rm -f "$cfg"
    return 1
  fi

  if [ "$DRY_RUN" = "true" ]; then
    log "DRY-RUN would reap: ${id}"
    rm -f "$cfg"; return 0
  fi

  log "reaping: ${id}"
  break_state_lock "$id"
  case "$CLOUD" in
    gcp)
      docker run --rm \
        -e CONFIG_CONTENT="$(base64 <"$cfg" | tr -d '\n')" \
        -e AUTO_APPROVE=true \
        -e KEY_CONTENT="$(base64 <"$GOOGLE_GHA_CREDS_PATH" | tr -d '\n')" \
        "$JANITOR_ACTUATOR_IMAGE" apply || rc=$?
      ;;
    aws)
      # A LoadBalancer Service (AWS inference UAT deploys agentgateway) leaves a
      # classic ELB + k8s-elb-* SG OUTSIDE Terraform state; `destroy` then fails
      # at DeleteVpc with DependencyViolation and the VPC leaks — the exact orphan
      # class this janitor exists to reap, failing identically every cycle without
      # this. Mirror uat-aws.yaml teardown: delete the LB Services gracefully while
      # the cluster still answers (best-effort — no-op once the cluster is gone),
      # destroy, and if that fails, sweep the leaked ELB/SG and retry once so a
      # VPC-only remnant still tears down. Both helper modes are scoped to this
      # deployment id's ownership tag and always exit 0.
      "$SCRIPT_DIR/uat-aws-cleanup-lb.sh" graceful "$id" || true
      aws_actuator_destroy "$cfg" || rc=$?
      if [ "$rc" -ne 0 ]; then
        log "aws destroy failed for ${id} (rc=${rc}); sweeping leaked LB resources and retrying once"
        "$SCRIPT_DIR/uat-aws-cleanup-lb.sh" sweep "$id" || true
        rc=0
        aws_actuator_destroy "$cfg" || rc=$?
      fi
      ;;
    azure)
      # The actuator container runs as uid `builder` (not the runner's uid) and
      # writes to its az config dir on startup, so it needs a writable copy —
      # `chmod -R a+rwX` on a throwaway mirrors the proven Bringup/Destroy steps
      # in uat-azure.yaml. Do not tighten the mode without confirming the image's
      # uid, or every Azure reap fails. mktemp -d is 0700, so the copy is private
      # until the chmod, and the runner is ephemeral and single-tenant.
      #
      # Cleanup is already armed: AZ_MOUNT is module-level and main() installs an
      # EXIT trap for it before any reap runs, so the credentials cannot survive
      # even a `set -e` abort between mktemp and the copy. The inline call below
      # removes them at the earliest possible moment rather than at exit.
      AZ_MOUNT="$(mktemp -d)"
      if cp -a "$HOME/.azure/." "$AZ_MOUNT/" && chmod -R a+rwX "$AZ_MOUNT"; then
        docker run --rm \
          -e CONFIG_CONTENT="$(base64 <"$cfg" | tr -d '\n')" \
          -e AUTO_APPROVE=true \
          -v "$AZ_MOUNT:/home/builder/.azure" \
          "$JANITOR_ACTUATOR_IMAGE" apply || rc=$?
      else
        rc=1
        echo "::warning::janitor could not stage az config for ${id}"
      fi
      cleanup_az_mount
      ;;
  esac
  rm -f "$cfg"

  if [ "$rc" -eq 0 ]; then
    log "reaped: ${id}"
  else
    echo "::warning::janitor reap failed for ${id} (rc=${rc}; will retry next run)"
  fi
  return "$rc"
}

# --- per-cloud discovery ---------------------------------------------------
# Each emits candidate deployment ids (one per line, matching the header's
# ALLOWLIST), deduped by the caller. Discovery is intentionally broad (clusters
# AND their networks /
# resource groups) so a cluster that was already deleted but left its network
# group / resource group behind is still caught.

# All GCP deployment ids (nightly aicr-uat-<run_id> and daytime aicr-uat-day-*)
# share the aicr-uat- prefix, same as AWS/Azure.
# This is a coarse CANDIDATE filter; classify() is the authoritative, schema-anchored
# gate.
# Run one cloud list command and echo its stdout. A NON-ZERO exit (missing CLI,
# expired creds, a per-API authorization gap the auth step did not catch, a
# throttle) is surfaced as a ::warning:: rather than swallowed — otherwise a
# failed listing looks identical to an empty fleet and the run reports "nothing
# to reap" while orphans persist. Discovery stays best-effort (the run continues
# and reaps whatever DID list), but the failure is now visible in the log.
run_discovery() {
  local label="$1"; shift
  local out errf rc
  errf="$(mktemp)"
  out="$("$@" 2>"$errf")" && rc=0 || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "::warning::${CLOUD} discovery '${label}' failed (rc=${rc}); its orphans are NOT reconciled this run: $(tr '\n' ' ' <"$errf")" >&2
  fi
  rm -f "$errf"
  printf '%s' "$out"
}

discover_gcp() {
  run_discovery "GKE clusters" gcloud container clusters list \
    --project "$GCP_PROJECT_ID" --format="value(name)" | grep -E "^${NAME_PREFIX}" || true
  # A deployment's VPC is named <id>-vpc; recover <id> for clusters already gone.
  # The ^ anchor means a foreign name merely ending in -vpc cannot be captured.
  run_discovery "GCP networks" gcloud compute networks list \
    --project "$GCP_PROJECT_ID" --format="value(name)" | sed -nE "s/^(${NAME_PREFIX}.*)-vpc$/\1/p" || true
}

discover_aws() {
  run_discovery "EKS clusters" aws eks list-clusters \
    --region "$AWS_REGION" --query 'clusters[]' --output text | tr '\t' '\n' | grep -E "^${NAME_PREFIX}" || true
  # The EKS actuator tags the VPC Name=<id>-vpc (network.tf); recover <id> for a
  # cluster already deleted but whose VPC group still leaks (mirrors the GCP -vpc
  # and Azure -rg recovery so AWS is not the one path that misses network-only
  # orphans). --output text tab-joins Name-tag values within a VPC.
  run_discovery "EC2 VPCs" aws ec2 describe-vpcs --region "$AWS_REGION" \
    --query "Vpcs[].Tags[?Key=='Name'].Value | [] | [?starts_with(@, '${NAME_PREFIX}')]" --output text \
    | tr '\t' '\n' | sed -nE "s/^(${NAME_PREFIX}.*)-vpc$/\1/p" || true
}

discover_azure() {
  run_discovery "AKS clusters" az aks list \
    --subscription "$AZURE_SUBSCRIPTION_ID" --query '[].name' -o tsv | grep -E "^${NAME_PREFIX}" || true
  # The actuator's resource group is <id>-rg; recover <id> for clusters already gone.
  run_discovery "Azure resource groups" az group list \
    --subscription "$AZURE_SUBSCRIPTION_ID" --query '[].name' -o tsv | sed -nE "s/^(${NAME_PREFIX}.*)-rg$/\1/p" || true
}

# --- main ------------------------------------------------------------------

main() {
  : "${JANITOR_CONFIG:?committed cluster-config path for this cloud}"
  : "${JANITOR_ACTUATOR_IMAGE:?actuator image for this cloud}"

  # Arm credential cleanup before anything can stage credentials. Installed here
  # rather than at module scope so that sourcing this file (the unit harness)
  # does not clobber the caller's own EXIT trap.
  trap cleanup_az_mount EXIT

  # Reject an unknown cloud up front with the usage message rather than letting a
  # typo reach `discover_<x>` and die with a cryptic `command not found`. Fails
  # closed — it can only narrow scope, never widen deletion.
  case "$CLOUD" in
    gcp | aws | azure) ;;
    *) log "FATAL: unknown cloud '${CLOUD}' (want gcp|aws|azure)"; return 2 ;;
  esac

  # Refuse to run at all on a malformed DRY_RUN, before any discovery: an
  # unrecognized value must never be resolved in the enforce direction.
  if ! valid_dry_run "$DRY_RUN"; then
    log "FATAL: DRY_RUN must be exactly 'true' or 'false', got '${DRY_RUN}'"
    return 2
  fi

  # Same fail-closed treatment for the age floors: a non-integer or negative
  # override would otherwise silently widen the reap window (see valid_age_floor).
  local floor
  for floor in NIGHTLY_MIN_AGE_HOURS DAYTIME_MIN_AGE_HOURS; do
    if ! valid_age_floor "${!floor}"; then
      log "FATAL: ${floor} must be a non-negative integer, got '${!floor}'"
      return 2
    fi
  done
  log "mode: $([ "$DRY_RUN" = true ] && echo DRY-RUN || echo ENFORCE) | nightly>=${NIGHTLY_MIN_AGE_HOURS}h daytime>=${DAYTIME_MIN_AGE_HOURS}h"

  # Prove the runs endpoint resolves before any 404 may be read as "run purged".
  # classify() consults this; without it a repo-level 404 orphans the whole fleet.
  if gh api "repos/${REPO}/actions/runs?per_page=1" >/dev/null 2>&1; then
    REPO_REACHABLE=yes
  else
    REPO_REACHABLE=no
    log "WARNING: repos/${REPO}/actions/runs unreachable — every 404 stays ambiguous"
  fi

  local candidates
  candidates="$(discover_"$CLOUD" | sort -u)"
  [ -n "$candidates" ] || log "no allowlisted deployments found"

  local reaped=0 failed=0 skipped=0 id verdict
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    verdict="$(classify "$id")"
    if [ "$verdict" = "REAP" ]; then
      log "REAP  ${id}"
      if reap "$id"; then reaped=$((reaped + 1)); else failed=$((failed + 1)); fi
    else
      log "skip  ${id} -> ${verdict#SKIP:}"
      skipped=$((skipped + 1))
    fi
  done <<<"$candidates"

  # In dry-run nothing was destroyed, so the count is of candidates that WOULD
  # be — never label it "reaped". Operators read these logs to decide whether to
  # trust enforce; a report claiming destroys that never happened poisons that.
  local mode verb
  if [ "$DRY_RUN" = true ]; then mode=dry-run; verb=would-reap; else mode=enforce; verb=reaped; fi
  log "summary: ${reaped} ${verb}, ${failed} failed, ${skipped} skipped (${mode})"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    {
      echo "### UAT janitor — ${CLOUD} (${mode})"
      echo
      echo "| ${verb} | failed | skipped |"
      echo "| ---: | ---: | ---: |"
      echo "| ${reaped} | ${failed} | ${skipped} |"
    } >>"$GITHUB_STEP_SUMMARY"
  fi

  # A destroy that keeps failing (stale lock, permissions gap) is exactly the
  # silent drift this workflow exists to catch, so fail the job rather than
  # leaving it buried in a ::warning:: that nobody reads until a quota blows.
  [ "$failed" -eq 0 ]
}

# Run main only when executed directly; sourcing exposes the pure helpers
# (run_id_of, is_daytime, age_hours_since, classify) without provisioning
# anything. tools/uat-janitor_test.sh drives them via `make test`.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main
fi
