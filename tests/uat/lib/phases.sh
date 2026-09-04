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
# shellcheck shell=bash
#
# Shared UAT phase library. Holds the cloud-agnostic phase implementations
# (prep, install, conformance, train, serve, verify, debug) and the phase
# dispatcher, sourced by the thin per-cloud runners tests/uat/{aws,gcp,azure}/run.
# The runners were ~identical (aws/gcp differed only in comment wording; azure
# added a federated-session refresh); this consolidates the shared body here and
# leaves each runner as a shim that sources this file and calls uat_main "$@".
#
# Cloud-specific behavior is injected through the cloud_refresh_credentials hook
# (default no-op; Azure overrides it to redeem a fresh federated OIDC assertion)
# and CLOUD_REFRESH_INTERVAL_SECONDS. Everything else is identical across clouds.
#
# Usage (from a per-cloud runner):
#   AICR_BIN=./aicr RUN_ID=12345 ./run <phase> <test-config.yaml>
#
# Phases:
#   prep         snapshot + recipe + dry-run validate + bundle
#   install      helmfile apply (deploys gpu-operator, kubeflow/dynamo, ...)
#   conformance  validate ALL phases (deployment + conformance + performance)
#                + emit signed evidence bundle
#   train        submit TrainJob, wait for completion, capture logs
#                (intent=training CUJ)
#   serve        deploy a DynamoGraphDeployment, wait for readiness, hit the
#                OpenAI-compatible endpoint, assert a completion, capture logs
#                (intent=inference CUJ — the DC3 counterpart of `train`)
#   verify       aicr evidence verify against the signed bundle
#   debug        snapshot live cluster state into cluster-debug/ (nodes, taints,
#                events, operator CRs incl. Skyhook status, operator/check-Job
#                logs) — best-effort, run on failure BEFORE teardown
#   all          run every phase in order (for local reproduction); the CUJ
#                phase is chosen by the config's recipe intent — `train` for
#                training, `serve` for inference
#
# Required env:
#   AICR_BIN    Path to the aicr binary
#   RUN_ID      Per-run correlation tag (`run-<id>`) applied to the evidence
#               push target (e.g. ${{ github.run_id }} in CI, or `local-$(date +%s)`)
#
# Working files are written to $PWD: snapshot.yaml, recipe.yaml, bundle/,
# dry-run.json, report.json, evidence/, evidence-result.json, train-logs/.

# Shared, cloud-agnostic cluster debug-bundle collector (collect_cluster_debug,
# capture_skyhook_snapshot), invoked by the `debug` phase on failure and inline
# from phase_conformance. Lives alongside this file in tests/uat/lib/.
# shellcheck source=./collect-debug.sh
source "$(dirname "${BASH_SOURCE[0]}")/collect-debug.sh"

# Train-job knobs (overridable for local reproduction or future inference variant).
TRAINJOB_NAMESPACE="${TRAINJOB_NAMESPACE:-kubeflow}"
TRAINJOB_NAME="${TRAINJOB_NAME:-pytorch-mnist}"
TRAINJOB_IMAGE="${TRAINJOB_IMAGE:-kubeflow/pytorch-dist-mnist:v1-9e12c68}"
TRAINJOB_TIMEOUT_SECONDS="${TRAINJOB_TIMEOUT_SECONDS:-1200}" # 20 min
# TrainJob node count. Defaults to 2 to span the cloud lanes' 2-GPU pools (and
# exercise multi-node distributed training); the single-GPU nvkind lane
# (tests/uat/kind/run) overrides to 1.
TRAINJOB_NUM_NODES="${TRAINJOB_NUM_NODES:-2}"
HELMFILE_TIMEOUT_SECONDS="${HELMFILE_TIMEOUT_SECONDS:-1200}" # 20 min
# ArgoCD deployer knobs (see install_argocd). ARGOCD_HELM_TIMEOUT_SECONDS
# bounds the `helm upgrade --install` of the argo-cd chart itself;
# ARGOCD_SYNC_TIMEOUT_SECONDS bounds the wait for every Application to
# reach a terminal-pass state. Both are shared wall-clock budgets, matching
# the discipline HELMFILE_TIMEOUT_SECONDS uses for the helmfile lane.
# ARGOCD_ROOT_APP_GRACE_SECONDS gives Argo CD a short window to reify the
# root `nvidia-stack` Application after `kubectl apply` -- a missing root
# after the grace means the apply silently produced no Application (RBAC
# collision, CRD not yet Established, etc.) and we should fail closed.
ARGOCD_HELM_TIMEOUT_SECONDS="${ARGOCD_HELM_TIMEOUT_SECONDS:-300}" # 5 min
ARGOCD_SYNC_TIMEOUT_SECONDS="${ARGOCD_SYNC_TIMEOUT_SECONDS:-1800}" # 30 min
ARGOCD_ROOT_APP_GRACE_SECONDS="${ARGOCD_ROOT_APP_GRACE_SECONDS:-120}"
# Prefix for the OCI target `aicr bundle --output` pushes to and the
# `--repo` baseURL baked into every Application. The path is under
# ghcr.io/nvidia (workflow already grants `packages: write` for evidence
# push) in a distinct namespace so bundle artifacts don't collide with
# signed evidence. See phase_prep's argocd branch.
ARGOCD_OCI_PREFIX="${ARGOCD_OCI_PREFIX:-oci://ghcr.io/nvidia/aicr-bundle-scratch}"
# Budget for the post-install readiness gate (see phase_install), which runs
# `aicr validate --phase deployment` until it passes READINESS_CONSECUTIVE_PASSES
# times in a row. This is the gate window ONLY -- it is entered AFTER helmfile
# apply returns, so the workflow install step must budget helmfile + this. It
# must span nodewright (skyhook) node tuning -- which REBOOTS the GPU node and
# re-inits the gpu-operator operands after the operator first reports ready (each
# validate run now polls internally, up to GPUReadinessTimeout, to ride through a
# reboot) -- plus cold-boot GPU-operator convergence
# (driver/toolkit/device-plugin/DCGM) and prometheus-adapter APIService
# aggregation, across the required consecutive passes. Sized at 60m because
# nodewright (skyhook) tuning plus its reboot cycles alone can run past 30m on a
# cold GPU node (the earlier 30m budget timed out purely waiting on tuning), and
# the gate must still have room to observe the required consecutive passes after
# tuning settles. Fails closed if exceeded.
READINESS_TIMEOUT_SECONDS="${READINESS_TIMEOUT_SECONDS:-3600}" # 60 min
# The gate requires the deployment phase to pass this many times CONSECUTIVELY
# before declaring readiness. A single pass proves "converged at instant T", not
# "will stay converged". TWO DIFFERENT threats motivate the streak:
#
# Reason 1 -- TEMPORAL FLAPPING of nodes that ALREADY EXIST: nodewright (skyhook)
# tuning reboots the GPU node more than once (the tuning packages carry
# interrupt: reboot) and re-opens status=in_progress after each reboot, so a pass
# can land in a lull between reboot cycles. Requiring N consecutive passes, spaced
# by the retry sleep, ensures the reboots have settled before the conformance
# phase (`--phase all`) runs -- any validate regression resets the counter.
#
# Reason 2 -- CENSUS GROWTH (issue #2096): the consecutive-pass streak defends
# ONLY against nodes that already exist. A not-yet-joined GPU node cannot fail a
# gate attempt, so the streak is structurally blind to it. When a late GPU node
# joins (observed 84s after a gate pass), Skyhook cordons+tunes it (taint
# skyhook.nvidia.com=runtime-required:NoSchedule + spec.unschedulable), re-opening
# convergence WHILE the later `--phase all` validate runs. So each streak-
# advancing attempt ALSO folds in a GPU-node census verdict (gpu_census_verdict):
# an attempt counts toward the streak only if validate passes AND the census is
# settled at that instant; a grown/cordoned census is treated as a regression and
# resets the streak, riding census growth out on the SAME READINESS_TIMEOUT_SECONDS
# budget as a reboot flap. (phase_conformance adds a second, adjacency-close census
# gate right before validate; see CENSUS_STABILITY_TIMEOUT_SECONDS below.)
READINESS_CONSECUTIVE_PASSES="${READINESS_CONSECUTIVE_PASSES:-2}"

# --- GPU-node census guard (issue #2096) --------------------------------------
# Defends the readiness gate (and the pre-conformance adjacency window) against
# GPU-node census GROWTH -- see the READINESS_CONSECUTIVE_PASSES note above,
# Reason 2. Env-var contract (the per-cloud runners wire the producer side):
#   EXPECTED_GPU_NODES   authoritative GPU-node count (integer). May be empty
#                        (degrade mode) or the literal `skip` (single-node nvkind
#                        lane opt-out). Computed from each cloud's cluster-config.
#   GPU_CENSUS_SELECTOR  kubectl label selector for the GPU pool; default
#                        `nodeGroup=gpu-worker` (common to all three clouds).
#
# Bounded budget for the pre-conformance census-stability gate (assert_gpu_census
# in phase_conformance). Closes the adjacency window between the readiness gate
# passing and `validate --phase all`: an authoritative GPU-node census can still
# GROW here (a late node joins ~84s after gate pass, Skyhook cordons+tunes it,
# re-opening convergence). This budget lets a just-joined node finish tuning and
# the census settle; fails closed if exceeded -- a growing/non-settling census
# fails the cell EARLY with a self-explanatory message instead of letting validate
# fail on a legitimately non-converged cluster. Separate from
# READINESS_TIMEOUT_SECONDS, which budgets the install-phase gate.
CENSUS_STABILITY_TIMEOUT_SECONDS="${CENSUS_STABILITY_TIMEOUT_SECONDS:-300}" # 5 min
# kubectl label selector for the authoritative GPU pool (all three clouds label
# their GPU pool the same way).
GPU_CENSUS_SELECTOR="${GPU_CENSUS_SELECTOR:-nodeGroup=gpu-worker}"
# Per-poll sleep and per-kubectl wall-clock bound for the live census wrappers.
GPU_CENSUS_SETTLE_SECONDS="${GPU_CENSUS_SETTLE_SECONDS:-10}"
GPU_CENSUS_KUBECTL_TIMEOUT="${GPU_CENSUS_KUBECTL_TIMEOUT:-30}"

# Inference-serve knobs (phase_serve; overridable for local reproduction). The
# defaults mirror the served DynamoGraphDeployment in demos/cuj2-inference.md
# (demos/workloads/inference/vllm-agg.yaml) so CI and the demo stay in lockstep.
SERVE_NAMESPACE="${SERVE_NAMESPACE:-dynamo-workload}"
SERVE_NAME="${SERVE_NAME:-vllm-agg}"
SERVE_QUEUE="${SERVE_QUEUE:-dynamo}"
SERVE_MODEL="${SERVE_MODEL:-Qwen/Qwen3-0.6B}"
SERVE_RUNTIME_IMAGE="${SERVE_RUNTIME_IMAGE:-nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.4.2}"
# GPU pool placement, applied to BOTH graph components (#1644 — it keeps the
# Frontend off the small CPU nodes, where its ~12GB image pull blew the
# readiness budget). The demo pins nodeGroup=gpu-worker; every UAT cluster
# labels its GPU pool the same way (tests/uat/*/cluster-config.yaml), so the
# default lands the graph on the GPU pool on any of the clouds.
SERVE_GPU_NODE_SELECTOR_KEY="${SERVE_GPU_NODE_SELECTOR_KEY:-nodeGroup}"
SERVE_GPU_NODE_SELECTOR_VALUE="${SERVE_GPU_NODE_SELECTOR_VALUE:-gpu-worker}"
SERVE_FRONTEND_PORT="${SERVE_FRONTEND_PORT:-8000}"
# Readiness budget: image pull of the multi-GB vllm-runtime + model download +
# engine warmup on a cold GPU node. Generous; fails closed if exceeded.
SERVE_READY_TIMEOUT_SECONDS="${SERVE_READY_TIMEOUT_SECONDS:-1800}" # 30 min
SERVE_REQUEST_TIMEOUT_SECONDS="${SERVE_REQUEST_TIMEOUT_SECONDS:-120}" # 2 min

# cloud_refresh_credentials: hook to refresh cloud credentials mid-run. Default
# no-op — AWS/GCP sessions either outlast the job or are refreshed by the
# workflow (configure-aws-credentials / google-github-actions/auth). Azure's
# federated CI session self-expires (~5-minute OIDC assertion), so its runner
# overrides this to redeem a fresh assertion. Called periodically inside the
# readiness gate (gated by CLOUD_REFRESH_INTERVAL_SECONDS) and once before debug
# collection. Must return non-zero on failure so the gate's timer only advances
# on a successful refresh.
cloud_refresh_credentials() { :; }

# How often the readiness gate calls cloud_refresh_credentials. Effectively-never
# by default (AWS/GCP need no mid-gate refresh); Azure's runner lowers it.
CLOUD_REFRESH_INTERVAL_SECONDS="${CLOUD_REFRESH_INTERVAL_SECONDS:-999999999}"

# gpu_census_verdict: PURE, unit-testable census decision (issue #2096). Reads the
# JSON of `kubectl get nodes -l <selector> -o json` on STDIN; $1 is the
# authoritative expected GPU-node count (may be empty or non-integer). Prints a
# one-line reason (or `ok (...)`) to STDOUT and returns 0 (settled) / 1 (not
# settled). It NEVER calls kubectl, so a Go test can exec it with JSON fixtures:
#   echo "$json" | gpu_census_verdict 2
#
# Logic (order matters):
#   - expected is a non-empty integer and present != expected -> return 1
#     (the census-GROWTH / census-shrink catch a not-yet-joined node evades)
#   - expected empty/non-integer -> require present >= 1 (degrade mode; stability
#     across polls is the caller's job in assert_gpu_census)
#   - every present node must be Ready, else `node <name> not Ready`, return 1
#   - no present node may be cordoned: spec.unschedulable==true OR a NoSchedule
#     taint whose key starts with `skyhook.nvidia.com` (the exact incident state:
#     present but cordoned+driverless) -> `node <name> cordoned ...`, return 1
#   - otherwise print `ok (<present> gpu-worker nodes ready)`, return 0
# Robust to missing fields (jq `// empty` / `// false`); unparseable JSON -> 1.
gpu_census_verdict() {
  local expected="${1:-}"
  local out ok reason
  if ! out="$(jq -r --arg expected "${expected}" '
      (.items // []) as $items
      | ($items | length) as $present
      | ( $expected
          | if . == "" then null
            elif test("^[0-9]+$") then tonumber
            else null end ) as $exp
      | ( if $exp != null and $present != $exp then
            { ok: false, reason: "census \($present)/\($exp) gpu-worker nodes present" }
          elif $exp == null and $present < 1 then
            { ok: false, reason: "census 0/>=1 gpu-worker nodes present" }
          else
            ( [ $items[]
                | select( ( [ .status.conditions[]? | select(.type == "Ready") | .status ]
                            | first // "Unknown" ) != "True" )
                | .metadata.name ] | first ) as $notready
            | if $notready != null then
                { ok: false, reason: "node \($notready) not Ready" }
              else
                ( [ $items[]
                    | select( (.spec.unschedulable // false) == true
                              or ( [ .spec.taints[]?
                                     | select(.effect == "NoSchedule"
                                              and ((.key // "") | startswith("skyhook.nvidia.com"))) ]
                                   | length > 0 ) )
                    | .metadata.name ] | first ) as $cordoned
                | if $cordoned != null then
                    { ok: false, reason: "node \($cordoned) cordoned by skyhook (tuning in progress)" }
                  else
                    { ok: true, reason: "ok (\($present) gpu-worker nodes ready)" }
                  end
              end
          end )
      | [ (.ok | tostring), .reason ] | @tsv
    ' 2>/dev/null)"; then
    echo "census check failed: node JSON was unparseable"
    return 1
  fi
  IFS=$'\t' read -r ok reason <<<"${out}"
  echo "${reason}"
  [[ "${ok}" == "true" ]]
}

# gpu_census_check_once: a SINGLE bounded census observation, printing the verdict
# reason and returning its status. Used by the readiness-gate streak, which
# already owns the retry loop and timeout budget -- so this must NOT poll. $1 is
# the expected count (passed through to gpu_census_verdict). The one kubectl call
# is wrapped in `timeout` so it can never hang the gate.
gpu_census_check_once() {
  local expected="${1:-}"
  local selector="${GPU_CENSUS_SELECTOR:-nodeGroup=gpu-worker}"
  local json
  json="$(timeout "${GPU_CENSUS_KUBECTL_TIMEOUT}" kubectl get nodes -l "${selector}" -o json 2>/dev/null || echo '{}')"
  printf '%s' "${json}" | gpu_census_verdict "${expected}"
}

# assert_gpu_census <timeout_seconds>: bounded LIVE wrapper around
# gpu_census_verdict (issue #2096). Polls `kubectl get nodes -l <selector>` (each
# call bounded by `timeout`) until the census settles or the budget elapses; fails
# closed on timeout -- it never hangs forever waiting for the Nth node.
#   - EXPECTED_GPU_NODES=skip     -> log + return 0 immediately (single-node lane).
#   - EXPECTED set (integer)      -> succeed as soon as the verdict is ok.
#   - EXPECTED empty/non-integer  -> DEGRADE mode: warn the exact count is unknown,
#                                    then require the verdict ok AND the present
#                                    count STABLE across two observations spaced by
#                                    the settle interval before returning 0.
assert_gpu_census() {
  local timeout_seconds="${1:?assert_gpu_census requires a timeout in seconds}"
  local selector="${GPU_CENSUS_SELECTOR:-nodeGroup=gpu-worker}"
  local expected="${EXPECTED_GPU_NODES:-}"

  if [[ "${expected}" == "skip" ]]; then
    echo "gpu census check skipped (EXPECTED_GPU_NODES=skip)"
    return 0
  fi

  # DEGRADE mode when the expected count is absent or not an integer: we cannot
  # assert an exact census, so fall back to "present and settled" and warn that
  # the strong (exact-count) census-growth guard is disabled.
  local degrade=false
  if [[ -z "${expected}" ]]; then
    degrade=true
  elif ! [[ "${expected}" =~ ^[0-9]+$ ]]; then
    echo "::warning::EXPECTED_GPU_NODES='${expected}' is not an integer; treating the expected count as unknown"
    expected=""
    degrade=true
  fi
  if [[ "${degrade}" == true ]]; then
    echo "::warning::EXPECTED_GPU_NODES is unknown; gpu census runs in degrade mode (require >=1 ready, uncordoned gpu-worker node, stable across two polls) -- exact census-growth detection is disabled"
  fi

  local deadline=$(( SECONDS + timeout_seconds ))
  local json cur_count rc
  local reason="(no census observation)"
  local prev_count=""
  while (( SECONDS < deadline )); do
    json="$(timeout "${GPU_CENSUS_KUBECTL_TIMEOUT}" kubectl get nodes -l "${selector}" -o json 2>/dev/null || echo '{}')"
    rc=0
    reason="$(printf '%s' "${json}" | gpu_census_verdict "${expected}")" || rc=$?
    if (( rc == 0 )); then
      if [[ "${degrade}" != true ]]; then
        echo "gpu census ${reason}"
        return 0
      fi
      # Degrade mode: require the present count to be STABLE across two ok
      # observations (a still-growing census would otherwise pass on one lucky
      # poll, right before Skyhook cordons the just-joined node).
      cur_count="$(printf '%s' "${json}" | jq '(.items // []) | length' 2>/dev/null || echo "")"
      if [[ -n "${prev_count}" && "${cur_count}" == "${prev_count}" ]]; then
        echo "gpu census ${reason} (stable at ${cur_count} across two polls)"
        return 0
      fi
      prev_count="${cur_count}"
      echo "gpu census ${reason}; confirming stability (count=${cur_count}), re-checking in ${GPU_CENSUS_SETTLE_SECONDS}s"
    else
      # Not settled -- reset the degrade-mode stability window too.
      prev_count=""
      echo "gpu census not settled: ${reason}; re-checking in ${GPU_CENSUS_SETTLE_SECONDS}s"
    fi
    sleep "${GPU_CENSUS_SETTLE_SECONDS}"
  done
  echo "::error::gpu census did not settle within ${timeout_seconds}s (last: ${reason})" >&2
  return 1
}

inject_push_target() {
  local current repo expected
  current="$(yq '.spec.validate.evidence.attestation.push' "${config}")"
  repo="${current%%:run-*}"
  expected="${repo}:run-${RUN_ID}"
  if [[ "${current}" != "${expected}" ]]; then
    yq -i ".spec.validate.evidence.attestation.push = \"${expected}\"" "${config}"
  fi
  echo "evidence push target: $(yq '.spec.validate.evidence.attestation.push' "${config}")"
}

phase_prep() {
  # Capture cluster state on snapshot failure so the post-mortem has
  # actual pod scheduling / image pull / event data instead of just the
  # generic "pod did not become ready" timeout the aicr CLI prints.
  local snapshot_ns
  snapshot_ns="$(yq '.spec.snapshot.agent.namespace // "aicr-validation"' "${config}")"
  echo "::group::Snapshot live cluster"
  if ! "${AICR_BIN}" snapshot --config "${config}"; then
    echo "::endgroup::"
    echo "::group::Snapshot failure debug"
    echo "--- nodes ---"
    kubectl get nodes -o wide --show-labels 2>&1 || true
    echo "--- node taints ---"
    kubectl get nodes -o json 2>/dev/null | \
      jq -r '.items[] | "\(.metadata.name)\n  taints: \(.spec.taints // [])\n  conditions: \([.status.conditions[] | select(.type == "Ready") | .status])"' || true
    echo "--- pods in ${snapshot_ns} ---"
    kubectl get pods -n "${snapshot_ns}" -o wide 2>&1 || true
    for p in $(kubectl get pods -n "${snapshot_ns}" -o name 2>/dev/null); do
      echo "--- describe ${p} ---"
      kubectl describe -n "${snapshot_ns}" "${p}" 2>&1 || true
      echo "--- logs ${p} (all containers, last 50 lines) ---"
      kubectl logs -n "${snapshot_ns}" "${p#pod/}" --all-containers --tail=50 2>&1 || true
    done
    echo "::endgroup::"
    exit 1
  fi
  test -f snapshot.yaml
  echo "::endgroup::"

  echo "::group::Generate recipe"
  "${AICR_BIN}" recipe --config "${config}"
  test -f recipe.yaml
  echo "::endgroup::"

  echo "::group::Validate (dry-run, --no-cluster)"
  # Validate against a copy of the config with spec.validate.evidence stripped so
  # the offline dry-run cannot emit/sign/push an evidence bundle. --no-cluster
  # reports every check as "skipped", so an attestation would attest to nothing;
  # worse, with evidence.attestation.{out,push} set (as UAT configs are) the
  # dry-run would sign and push a bundle to the same OCI repo the conformance
  # phase's authoritative validate later pushes to, leaving two independently-
  # signed bundles and breaking `evidence verify` (signed subject != pulled
  # run-tagged digest). Mirrors the readiness-gate strip in phase_conformance.
  # Belt-and-suspenders with the CLI's own --no-cluster guard: this keeps prep
  # safe even against an older released aicr that lacks that guard.
  # Run in a subshell with an EXIT trap so the temp config is removed on every
  # exit path (normal return, set -e abort on validate failure, interrupt),
  # mirroring the signing subshell below. A non-zero validate rc propagates out
  # of the subshell and aborts prep under set -e, as before.
  (
    prep_config="$(mktemp)"
    trap 'rm -f "${prep_config}"' EXIT
    yq 'del(.spec.validate.evidence)' "${config}" > "${prep_config}"
    "${AICR_BIN}" validate \
      --config "${prep_config}" \
      --phase deployment \
      --no-cluster \
      --output dry-run.json
  )
  test -f dry-run.json
  echo "::endgroup::"

  echo "::group::Generate bundle"
  # The bundle shape (and how it is delivered to the cluster) is deployer-
  # specific: helmfile emits bundle/helmfile.yaml consumed off the local
  # filesystem; argocd emits bundle/app-of-apps.yaml AND pushes the same
  # content to an OCI registry so Argo CD's repo-server can pull it. Read
  # the deployer straight from the AICRConfig -- keeping selection in the
  # test-config (not a workflow env var) means the artifact under review
  # is self-describing.
  local deployer
  deployer="$(yq -r '.spec.bundle.deployment.deployer // "helmfile"' "${config}")"
  case "${deployer}" in
    helmfile)
      "${AICR_BIN}" bundle --config "${config}"
      test -f bundle/helmfile.yaml || {
        echo "expected bundle/helmfile.yaml (deployer: helmfile) — got:" >&2
        ls -la bundle >&2 || true
        exit 1
      }
      ;;
    argocd)
      # --output pushes bundle content to OCI; --repo sets the source.repoURL
      # baked into the rendered nvidia-stack Application. Both flags override
      # the config's spec.bundle.output.target (pkg/cli/bundle.go:583-604)
      # so the AICRConfig stays deployer-neutral.
      #
      # URL shape (matches KWOK argocd-oci precedent at
      # kwok/scripts/validate-scheduling.sh:951+):
      #   oci_repo   = <prefix>/<recipe-slug>               (per-recipe, no tag)
      #   oci_target = <prefix>/<recipe-slug>:run-<RUN_ID>  (same repo, with tag)
      # `aicr bundle` renders root Application spec.source.repoURL=<oci_repo>
      # and targetRevision=<tag>, so Argo CD pulls from oci_target — the same
      # URL we pushed to. Passing a bare-prefix --repo (without the slug)
      # would leave Argo CD chasing a non-existent artifact at
      # oci://<prefix>:main — verified locally with `aicr bundle --deployer
      # argocd` inspection during Tier 1 validation of issue #2194.
      #
      # RUN_ID isolates concurrent runs on the same recipe. Argo CD prefix-
      # matches repo-creds (util/db/repository_secrets.go:
      # getRepositoryCredentialIndex → HasPrefix), so a single Secret at
      # ARGOCD_OCI_PREFIX covers every <recipe-slug> pushed under that
      # prefix — provisioned once by install_argocd.
      #
      # Retention follow-up (#2194): this pushes a new run-tagged artifact
      # every dispatch, and there is no cleanup on the successful path. Left
      # deferred while this cell is manual-dispatch-only (accumulation rate
      # low, cost bounded); to be addressed at nightly enrollment by EITHER
      # a workflow teardown step that `gh api DELETE`s the tag it stashed as
      # a job output, OR an org-level retention policy on
      # ghcr.io/nvidia/aicr-bundle-scratch. The `-scratch` namespace name
      # already signals ephemeral intent to any operator inspecting GHCR.
      local bundle_slug oci_repo oci_target
      bundle_slug="$(yq -r '.metadata.name' "${config}")"
      oci_repo="${ARGOCD_OCI_PREFIX}/${bundle_slug}"
      oci_target="${oci_repo}:run-${RUN_ID}"
      echo "argocd bundle push target: ${oci_target}"
      echo "argocd Application source.repoURL: ${oci_repo}"
      "${AICR_BIN}" bundle --config "${config}" \
        --output "${oci_target}" \
        --repo "${oci_repo}"
      test -f bundle/app-of-apps.yaml || {
        echo "expected bundle/app-of-apps.yaml (deployer: argocd) — got:" >&2
        ls -la bundle >&2 || true
        exit 1
      }
      ;;
    *)
      echo "::error::unsupported deployer: ${deployer} (supported: helmfile, argocd)" >&2
      exit 1
      ;;
  esac
  echo "::endgroup::"
}

# Print the SHA256 of a file, using whichever of sha256sum (Linux) or shasum
# (macOS) is present. Fails closed when neither is: a runner that cannot verify
# a pinned checksum must not proceed to install the artifact.
uat_sha256() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${file}" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${file}" | awk '{print $1}'
  else
    echo "::error::neither sha256sum nor shasum is available; cannot verify checksums" >&2
    return 1
  fi
}

# Map the host to a helm-diff release-asset suffix and the matching
# .settings.yaml checksum key. Upstream labels macOS assets "macos" while the
# checksum keys use GOOS names, so the two differ on Darwin.
uat_helm_diff_platform() {
  local os arch
  case "$(uname -s)" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    *) echo "::error::unsupported OS $(uname -s) for helm-diff install" >&2; return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "::error::unsupported architecture $(uname -m) for helm-diff install" >&2; return 1 ;;
  esac
  # "<asset-suffix> <checksum-key>"
  echo "${os/darwin/macos}-${arch} ${os}_${arch}"
}

phase_install() {
  # Dispatch to the deployer-specific install body. The readiness gate below
  # is deployer-agnostic (it validates deployed cluster state, not the
  # deployment mechanism) so it stays in phase_install; only the "get the
  # stack onto the cluster" step differs.
  local deployer
  deployer="$(yq -r '.spec.bundle.deployment.deployer // "helmfile"' "${config}")"
  case "${deployer}" in
    helmfile) install_helmfile ;;
    argocd)   install_argocd ;;
    *)
      echo "::error::unsupported deployer: ${deployer} (supported: helmfile, argocd)" >&2
      exit 1
      ;;
  esac

  echo "::group::Cluster state post-install"
  kubectl get nodes -o wide
  kubectl get pods -A | grep -Ev '\s+Running\s+|\s+Completed\s+' || true
  echo "::endgroup::"

  install_readiness_gate
}

install_helmfile() {
  command -v helmfile >/dev/null || { echo "helmfile not on PATH" >&2; exit 1; }
  command -v helm     >/dev/null || { echo "helm not on PATH"     >&2; exit 1; }
  command -v curl     >/dev/null || { echo "curl not on PATH"     >&2; exit 1; }
  command -v gpg      >/dev/null || { echo "gpg not on PATH"      >&2; exit 1; }

  # Read helm-diff version from the single source of truth (.settings.yaml).
  local SCRIPT_DIR
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  local REPO_ROOT="${SCRIPT_DIR}/../../.."
  local HELM_DIFF_VERSION
  HELM_DIFF_VERSION="$(yq -r '.testing_tools.helm_diff // ""' "${REPO_ROOT}/.settings.yaml")"
  if [[ -z "${HELM_DIFF_VERSION}" ]]; then
    echo "::error::testing_tools.helm_diff is not pinned in .settings.yaml" >&2
    exit 1
  fi

  # Declared separately from the assignment: `local x="$(...)"` would mask the
  # substitution's exit status behind local's own success.
  local HELM_DIFF_PLATFORM
  HELM_DIFF_PLATFORM="$(uat_helm_diff_platform)" || exit 1
  local HELM_DIFF_ASSET HELM_DIFF_SHA_KEY
  read -r HELM_DIFF_ASSET HELM_DIFF_SHA_KEY <<<"${HELM_DIFF_PLATFORM}"
  local HELM_DIFF_SHA256
  HELM_DIFF_SHA256="$(yq -r ".testing_tools.helm_diff_checksums.${HELM_DIFF_SHA_KEY} // \"\"" \
    "${REPO_ROOT}/.settings.yaml")"
  if [[ -z "${HELM_DIFF_SHA256}" ]]; then
    echo "::error::no helm-diff checksum pinned for ${HELM_DIFF_SHA_KEY} in .settings.yaml; refresh with tools/update-helm-diff-checksums ${HELM_DIFF_VERSION}" >&2
    exit 1
  fi

  echo "::group::Install helm-diff plugin (${HELM_DIFF_VERSION})"
  # Check installed version; reinstall if missing or at a different version so the
  # .settings.yaml pin is always effective (not just "any version is fine").
  local installed_ver
  installed_ver="$(helm plugin list 2>/dev/null | awk '$1=="diff" {print $2}')"
  if [[ "${installed_ver}" == "${HELM_DIFF_VERSION#v}" ]]; then
    echo "helm-diff ${HELM_DIFF_VERSION} already installed"
  else
    if [[ -n "${installed_ver}" ]]; then
      echo "helm-diff ${installed_ver} installed; removing to pin ${HELM_DIFF_VERSION}"
      helm plugin remove diff
    fi
    # Install the exact bytes we verified. Four independent gates, all on one
    # downloaded copy — helm never re-fetches, so there is no window in which a
    # hostile cache can substitute different bytes between check and install:
    #
    #   1. SHA256 matches the .settings.yaml pin  — fixes *which* bytes.
    #   2. The .prov clearsign carries VALIDSIG for the pinned maintainer
    #      fingerprint                            — proves upstream authorship.
    #   3. The SHA256 recorded inside that signed .prov equals the pin
    #                                             — binds (2) to (1), so the
    #      signature covers our bytes, not some other signed release.
    #   4. The version inside that signed .prov equals the requested version
    #                                             — the only gate that survives
    #      a poisoned pin; see its comment below.
    #
    # Gate 1 is the load-bearing control against a *replayed older release*: a
    # stale tarball carries a genuine upstream signature but the wrong hash, so
    # it is rejected before any signature is examined. That is what helm's own
    # --verify cannot do — it establishes only (2) and would accept any validly
    # signed release — and it is why passing --verify=false here is strictly
    # stronger rather than weaker. Gates 3 and 4 cover what Gate 1 cannot: a pin
    # that was poisoned at refresh time, when the hash alone proves nothing.
    # The cost is a cosmetic `PROVENANCE: unsigned` in `helm plugin list`.
    #
    # The tarball is staged as diff.tgz because helm's local-tarball installer
    # derives both the expected archive root and the install directory from the
    # file's basename, and helm-diff's tarball is rooted at diff/.
    #
    # Run in a subshell with an EXIT trap so the temp GNUPGHOME/staging dir are
    # removed on every path — including a set -e (L41) abort, where a RETURN
    # trap would not fire. The subshell keeps the trap and temp vars scoped to
    # this block without disturbing the parent shell's traps. Its `|| exit 1`
    # does not rely on the caller's set -e: a failed gate must abort even if
    # phase_install is ever invoked in a condition context, where errexit is
    # suppressed for the whole function body.
    (
      HELM_DIFF_KEY_FPR="C5645EF47482257A1F806D2BEA17A2A206AFF8CD"
      HELM_DIFF_FILE="helm-diff-${HELM_DIFF_ASSET}.tgz"
      HELM_DIFF_URL="https://github.com/databus23/helm-diff/releases/download/${HELM_DIFF_VERSION}/${HELM_DIFF_FILE}"
      GNUPGHOME="$(mktemp -d)"; export GNUPGHOME
      HELM_DIFF_STAGE="$(mktemp -d)"
      trap 'rm -rf "${GNUPGHOME}" "${HELM_DIFF_STAGE}"' EXIT
      tarball="${HELM_DIFF_STAGE}/diff.tgz"

      # No --max-time: the tarball is ~33 MB and a slow-but-progressing
      # transfer should not be killed. --retry-max-time bounds the retry window.
      curl -fsSL --connect-timeout 10 \
        --retry 3 --retry-delay 0 --retry-max-time 180 --retry-connrefused \
        -o "${tarball}" "${HELM_DIFF_URL}"
      curl -fsSL --connect-timeout 10 \
        --retry 3 --retry-delay 0 --retry-max-time 180 --retry-connrefused \
        -o "${tarball}.prov" "${HELM_DIFF_URL}.prov"

      # Gate 1: pinned bytes.
      actual_sha="$(uat_sha256 "${tarball}")"
      if [[ "${actual_sha}" != "${HELM_DIFF_SHA256}" ]]; then
        echo "::error::helm-diff tarball checksum mismatch for ${HELM_DIFF_URL}" >&2
        echo "::error::expected ${HELM_DIFF_SHA256} (.settings.yaml testing_tools.helm_diff_checksums.${HELM_DIFF_SHA_KEY}), got ${actual_sha}" >&2
        exit 1
      fi

      # Gate 2: the provenance document is signed by the pinned release key.
      # The fingerprint check on the imported key gives a clearer error when
      # the published key rotates; VALIDSIG is the one that actually binds the
      # signature to that fingerprint.
      curl -fsSL "https://github.com/databus23.gpg" | gpg --import
      if ! gpg --with-colons --fingerprint \
          | awk -F: '/^fpr:/{print $10}' | grep -qx "${HELM_DIFF_KEY_FPR}"; then
        echo "::error::helm-diff release key fingerprint mismatch (expected ${HELM_DIFF_KEY_FPR})" >&2
        exit 1
      fi
      # VALIDSIG's first field is the fingerprint of the key that actually made
      # the signature and its last field is the primary key's, which differ once
      # a maintainer signs with a dedicated subkey. Accept the pin in either
      # position so a legitimate future subkey does not false-reject.
      if ! gpg --verify --status-fd=1 "${tarball}.prov" 2>/dev/null \
          | awk -v fpr="${HELM_DIFF_KEY_FPR}" \
              '$1=="[GNUPG:]" && $2=="VALIDSIG" && ($3==fpr || $NF==fpr) {found=1} END{exit !found}'; then
        echo "::error::helm-diff provenance is not validly signed by ${HELM_DIFF_KEY_FPR}" >&2
        exit 1
      fi

      # Decode once: gates 3 and 4 both read the signed body.
      prov_body="$(gpg --decrypt "${tarball}.prov" 2>/dev/null)"

      # Gate 3: that signature covers the bytes we just pinned. The .prov keys
      # its files map by the upstream asset name, not our staged filename.
      prov_sha="$(awk -v a="${HELM_DIFF_FILE}:" \
        '$1==a {gsub(/"|sha256:/,"",$2); print $2}' <<<"${prov_body}")"
      if [[ "${prov_sha}" != "${HELM_DIFF_SHA256}" ]]; then
        echo "::error::helm-diff provenance records sha256 ${prov_sha:-<none>} for ${HELM_DIFF_FILE}, pinned ${HELM_DIFF_SHA256}" >&2
        exit 1
      fi

      # Gate 4: the signed body names the release we asked for. Gates 1-3 all
      # reduce to "these bytes match the pin", so they cannot detect a pin that
      # was itself poisoned: tools/update-helm-diff-checksums derives the pin
      # from an *unsigned* checksums.txt, and the asset filename carries no
      # version, so a compromised release/CDN can seed the pin with an older
      # release's hashes and then serve that older, genuinely signed tarball
      # here. Only the version inside the signature distinguishes them.
      prov_ver="$(awk '$1=="version:" {gsub(/"/,"",$2); print $2; exit}' <<<"${prov_body}")"
      if [[ "${prov_ver}" != "${HELM_DIFF_VERSION#v}" ]]; then
        echo "::error::helm-diff provenance is for version ${prov_ver:-<none>}, requested ${HELM_DIFF_VERSION#v} — possible downgrade" >&2
        exit 1
      fi
      echo "helm-diff verified (${HELM_DIFF_SHA_KEY}: ${HELM_DIFF_SHA256}, version ${prov_ver} signed by ${HELM_DIFF_KEY_FPR})"

      # --verify=false skips helm's provenance check, not helm-diff's install
      # hook, which still runs. The gates above constrain the .tgz bytes, not
      # what the hook does: today it finds the bundled diff/bin/diff and skips
      # downloading, so nothing unverified is fetched. A bump that stops
      # shipping that prebuilt binary would silently reintroduce a fetch and
      # must be caught in review.
      helm plugin install "${tarball}" --verify=false
    ) || exit 1
  fi
  echo "::endgroup::"

  # Retry helmfile apply on transient cluster-API errors (control-plane
  # warmup, throttling, etc.). helmfile is idempotent — re-running on a
  # partial-install state converges on the desired set. The 3 attempts SHARE a
  # single HELMFILE_TIMEOUT_SECONDS wall-clock budget (each attempt is capped at
  # the remaining budget, like the readiness gate below), not a full budget each:
  # transient errors fail fast so sharing leaves ample time for retries, while a
  # genuine hang cannot stretch install to 3x the budget. This bounds worst-case
  # install (helmfile + gate) within the workflow step's timeout-minutes.
  local helmfile_deadline=$(( SECONDS + HELMFILE_TIMEOUT_SECONDS ))
  local success=false helm_remaining
  for attempt in 1 2 3; do
    if (( SECONDS >= helmfile_deadline )); then
      # Distinguish a budget-starved run (a slow-but-progressing apply consumed
      # the shared window) from a genuine 3-strike failure: log why attempts
      # 2-3 never launched.
      echo "helmfile shared ${HELMFILE_TIMEOUT_SECONDS}s budget exhausted after attempt $(( attempt - 1 )); not starting attempt ${attempt}"
      break
    fi
    helm_remaining=$(( helmfile_deadline - SECONDS ))
    (( helm_remaining < 1 )) && helm_remaining=1
    echo "::group::helmfile apply attempt ${attempt}/3 (timeout ${helm_remaining}s of ${HELMFILE_TIMEOUT_SECONDS}s shared budget)"
    if ( cd bundle && timeout "${helm_remaining}" helmfile apply --skip-diff-on-install ); then
      success=true
      echo "::endgroup::"
      break
    fi
    echo "helmfile apply attempt ${attempt} failed"
    echo "::endgroup::"
    if (( attempt < 3 )); then
      echo "waiting 30s before retry"
      sleep 30
    fi
  done
  if [[ "${success}" != "true" ]]; then
    echo "::error::helmfile apply failed after 3 attempts" >&2
    exit 1
  fi
}

install_argocd() {
  command -v helm    >/dev/null || { echo "helm not on PATH" >&2; exit 1; }
  command -v kubectl >/dev/null || { echo "kubectl not on PATH" >&2; exit 1; }

  local SCRIPT_DIR
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  local REPO_ROOT="${SCRIPT_DIR}/../../.."
  local ARGOCD_CHART_VERSION
  ARGOCD_CHART_VERSION="$(yq -r '.testing_tools.argocd_chart' "${REPO_ROOT}/.settings.yaml")"

  echo "::group::Install Argo CD (chart ${ARGOCD_CHART_VERSION})"
  # KWOK precedent (kwok/scripts/install-infra.sh:install_argocd) — same
  # chart, same namespace/release. `helm upgrade --install` is idempotent
  # so a re-run (or a cluster that already has Argo CD baked in) converges
  # cleanly. `--wait --timeout` blocks until argocd-server + application-
  # controller are Ready so downstream steps (repo-creds, apply) don't
  # race a partially-rolled-out install.
  helm repo add argo https://argoproj.github.io/argo-helm --force-update >/dev/null 2>&1 || true
  # Every fail-closed path in this function emits `::error::` and closes the
  # ::group:: before exiting so a triager gets a breadcrumb + no dangling
  # log group. `set -euo pipefail` would otherwise abort here silently.
  if ! helm repo update argo >/dev/null; then
    echo "::error::helm repo update argo failed" >&2
    echo "::endgroup::"
    exit 1
  fi
  if ! helm upgrade --install argocd argo/argo-cd \
      --namespace argocd --create-namespace \
      --version "${ARGOCD_CHART_VERSION}" \
      --wait --timeout "${ARGOCD_HELM_TIMEOUT_SECONDS}s"; then
    echo "::error::Argo CD helm install failed" >&2
    kubectl -n argocd get pods || true
    echo "::endgroup::"
    exit 1
  fi
  # CRD race guard — applications.argoproj.io must be Established before we
  # apply the app-of-apps, else kubectl apply races the CRD controller.
  if ! kubectl wait --for=condition=Established \
      crd/applications.argoproj.io --timeout=120s; then
    echo "::error::applications.argoproj.io CRD did not reach Established within 120s" >&2
    kubectl describe crd applications.argoproj.io 2>&1 | head -60 >&2 || true
    echo "::endgroup::"
    exit 1
  fi
  echo "::endgroup::"

  # Start the shared ARGOCD_SYNC_TIMEOUT_SECONDS wall clock HERE, before the
  # repo-creds Secret apply, so a hung apiserver on any single step (Secret
  # apply, app-of-apps apply, root-app grace, terminal-pass poll) draws from
  # the same 30m budget install_helmfile enforces with its own shared clock.
  # Without this, a stalled Secret apply would burn indefinite time before
  # the downstream retry loop even started measuring.
  local argocd_deadline=$(( SECONDS + ARGOCD_SYNC_TIMEOUT_SECONDS ))
  local secret_remaining

  echo "::group::Provision ghcr.io repo-creds Secret (prefix match)"
  # Argo CD prefix-matches repo-creds against Application source URLs
  # (util/db/repository_secrets.go::getRepositoryCredentialIndex → HasPrefix),
  # so ONE Secret annotated argocd.argoproj.io/secret-type=repo-creds at
  # url=ARGOCD_OCI_PREFIX covers every recipe pushed under that prefix.
  # GITHUB_TOKEN + GITHUB_ACTOR are provided by the UAT workflow env
  # (packages: write is already declared on every uat-*.yaml job).
  : "${GITHUB_TOKEN:?GITHUB_TOKEN required for ghcr.io pull creds}"
  : "${GITHUB_ACTOR:?GITHUB_ACTOR required for ghcr.io pull creds}"
  # Field shape mirrors kwok/scripts/install-infra.sh:apply_repo_secret
  # for chart 9.5.x / Argo CD v3.x — type: oci is the direct OCI credential
  # kind (chart 7.x needed type: helm + enableOCI=true; superfluous here
  # and dropped to avoid confusion). username/password are the only fields
  # KWOK omits, because its in-cluster registry is unauthenticated HTTP;
  # ghcr.io is HTTPS + auth, so we supply the GITHUB_ACTOR/GITHUB_TOKEN
  # already scoped to `packages: write` on every UAT job. The whole apply is
  # bounded by the remaining shared budget so a hung apiserver cannot outlast
  # the sync-wait window; clamp >= 1 for the `timeout 0 == no timeout`
  # boundary condition install_helmfile guards the same way.
  #
  # `kubectl create ... --from-literal | kubectl label --local | kubectl apply`
  # carries the token/actor as argv, never through a YAML parser -- defense-in-
  # depth against any future GH-token/actor value with YAML-breaking bytes
  # (safe by construction today, but the belt-and-suspenders is cheap).
  secret_remaining=$(( argocd_deadline - SECONDS ))
  (( secret_remaining < 1 )) && secret_remaining=1
  if ! kubectl create secret generic aicr-oci-repo-creds \
      --namespace argocd \
      --from-literal=type=oci \
      --from-literal="url=${ARGOCD_OCI_PREFIX}" \
      --from-literal="username=${GITHUB_ACTOR}" \
      --from-literal="password=${GITHUB_TOKEN}" \
      --dry-run=client -o yaml \
    | kubectl label --local -f - argocd.argoproj.io/secret-type=repo-creds -o yaml \
    | timeout "${secret_remaining}" kubectl apply -f -
  then
    echo "::error::repo-creds Secret apply failed (timeout ${secret_remaining}s of ${ARGOCD_SYNC_TIMEOUT_SECONDS}s shared budget)" >&2
    echo "::endgroup::"
    exit 1
  fi
  echo "::endgroup::"

  # Retry `kubectl apply -f bundle/app-of-apps.yaml` on transient apiserver
  # errors, mirroring install_helmfile's shared-budget discipline. In practice
  # `kubectl apply` on a single manifest almost never needs the retry, but the
  # shared-budget shape keeps the two branches behaviorally symmetric and
  # bounds the install step's worst-case wall clock the same way.
  #
  # Each attempt is capped at the remaining shared ARGOCD_SYNC_TIMEOUT_SECONDS
  # budget with `timeout`, and the deadline is re-checked BEFORE each attempt
  # so a stalled apiserver cannot spend the whole 30m budget on kubectl apply
  # and starve the downstream sync-wait. Clamp `remaining` to >= 1 for the
  # same reason install_helmfile does: `timeout 0` means "no timeout".
  local applied=false apply_remaining apply_nap
  for attempt in 1 2 3; do
    if (( SECONDS >= argocd_deadline )); then
      echo "argocd shared ${ARGOCD_SYNC_TIMEOUT_SECONDS}s budget exhausted after attempt $(( attempt - 1 )); not starting attempt ${attempt}"
      break
    fi
    apply_remaining=$(( argocd_deadline - SECONDS ))
    (( apply_remaining < 1 )) && apply_remaining=1
    echo "::group::kubectl apply app-of-apps (attempt ${attempt}/3, timeout ${apply_remaining}s of ${ARGOCD_SYNC_TIMEOUT_SECONDS}s shared budget)"
    if timeout "${apply_remaining}" kubectl apply -f bundle/app-of-apps.yaml; then
      applied=true
      echo "::endgroup::"
      break
    fi
    echo "::endgroup::"
    # Cap the retry delay to remaining shared budget so `sleep 15` near the
    # deadline can't overrun it — same discipline as the root-app and sync-
    # wait loops. Positive-only guard so the loop exits cleanly at budget=0.
    if (( attempt < 3 )); then
      apply_nap=15
      apply_remaining=$(( argocd_deadline - SECONDS ))
      (( apply_nap > apply_remaining )) && apply_nap=${apply_remaining}
      if (( apply_nap > 0 )); then
        echo "waiting ${apply_nap}s before retry"
        sleep "${apply_nap}"
      fi
    fi
  done
  if [[ "${applied}" != "true" ]]; then
    echo "::error::kubectl apply app-of-apps.yaml failed after 3 attempts (or budget exhausted)" >&2
    exit 1
  fi

  echo "::group::Wait for root Application '${ARGOCD_ROOT_APP:-nvidia-stack}' to be reified"
  # A missing root after the grace window means the apply silently produced
  # no Application (RBAC race, CRD version skew, malformed manifest). Fail
  # closed rather than time out downstream on an empty controller queue.
  # Cap the root-app grace at the shared argocd_deadline so this loop can
  # never run past the ARGOCD_SYNC_TIMEOUT_SECONDS budget that spans the
  # whole install path (Secret apply + apply retries + this grace + sync
  # poll). Without the cap, an upstream step that spent most of the shared
  # budget could still let the root-app grace add its full 2m on top.
  # `root_grace_effective` is the actual window this loop ran under (nominal
  # OR shorter if the shared cap fired) — used by the failure diagnostic
  # below so a budget-starved run doesn't look like a 120s hang.
  local root_deadline=$(( SECONDS + ARGOCD_ROOT_APP_GRACE_SECONDS ))
  (( root_deadline > argocd_deadline )) && root_deadline=${argocd_deadline}
  local root_grace_effective=$(( root_deadline - SECONDS ))
  local root_app="${ARGOCD_ROOT_APP:-nvidia-stack}"
  local root_ready=false root_remaining root_nap
  while (( SECONDS < root_deadline )); do
    # Bound each kubectl by remaining budget so a hung apiserver cannot burn
    # the whole grace window on one call. Clamp to >= 1 (same rationale as
    # install_helmfile: `timeout 0` means no timeout).
    root_remaining=$(( root_deadline - SECONDS ))
    (( root_remaining < 1 )) && root_remaining=1
    if timeout "${root_remaining}" kubectl -n argocd get application "${root_app}" >/dev/null 2>&1; then
      root_ready=true
      break
    fi
    # Cap the sleep to remaining budget so we do not overrun the deadline
    # waiting between polls.
    root_nap=5
    root_remaining=$(( root_deadline - SECONDS ))
    (( root_nap > root_remaining )) && root_nap=${root_remaining}
    (( root_nap > 0 )) && sleep "${root_nap}"
  done
  if [[ "${root_ready}" != "true" ]]; then
    if (( root_grace_effective < ARGOCD_ROOT_APP_GRACE_SECONDS )); then
      echo "::error::root Application '${root_app}' not reified within effective ${root_grace_effective}s (nominal ${ARGOCD_ROOT_APP_GRACE_SECONDS}s; shared sync budget capped this loop early)" >&2
    else
      echo "::error::root Application '${root_app}' not reified within ${ARGOCD_ROOT_APP_GRACE_SECONDS}s" >&2
    fi
    kubectl -n argocd get applications || true
    echo "::endgroup::"
    exit 1
  fi
  echo "::endgroup::"

  echo "::group::Wait for all Argo CD Applications to reach terminal-pass (budget $(( argocd_deadline - SECONDS ))s)"
  # Two premature-convergence guards run before the 4-arm terminal-pass
  # predicate (which mirrors tests/chainsaw/kwok/argocd-sync/chainsaw-test.yaml,
  # source-agnostic per that test's SYNC NOTE header):
  #   1. items == []               -> "no Applications yet" (CRD present but
  #      neither root nor children reified yet).
  #   2. items == [root] only, or root not Synced -> in the race window where
  #      the app-of-apps root is OutOfSync+Healthy while its child Applications
  #      have not yet been generated, arm 2 (OutOfSync+Healthy) would satisfy
  #      `bad=""` and return 0 before gpu-operator/DRA even exist. Require the
  #      root Application to be Synced (not merely present) as the crossover.
  # Otherwise apply the 4-arm predicate: Synced+Healthy (canonical); OutOfSync+
  # Healthy (operator mutation — gpu-operator ClusterPolicy, ResourceSlice
  # injection); Synced+Progressing / Synced+Degraded (Argo health-controller
  # divergence post-op — tolerated because the ultimate verdict lives in the
  # deployment readiness gate that follows).
  local root_app_name="${ARGOCD_ROOT_APP:-nvidia-stack}"
  local jq_bad
  jq_bad='
    if (.items | length) == 0 then
      "no Applications yet"
    elif ([.items[] | select(.metadata.name == "'"${root_app_name}"'" and .status.sync.status == "Synced")] | length) == 0 then
      "root '"${root_app_name}"' not Synced yet (children may not be reified)"
    else
      ([ .items[] |
        select(
          ((.status.sync.status == "Synced")    and (.status.health.status | IN("Healthy","Progressing","Degraded"))) or
          ((.status.sync.status == "OutOfSync") and (.status.health.status == "Healthy"))
        | not)
        | (.metadata.name + "[" + (.status.sync.status // "?") + "/" + (.status.health.status // "?") + "]")
      ] | join(", "))
    end'
  # Initialize `bad` with a sentinel so the failure diagnostic below reads
  # sensibly even in the pathological case where the while guard is false on
  # the first check (SECONDS already >= argocd_deadline because kubectl-apply
  # + root-grace consumed the whole shared budget). Loop iterations overwrite
  # this with real values.
  local bad="not sampled (sync-wait loop never ran — budget spent by upstream steps)"
  local sync_remaining sync_nap
  while (( SECONDS < argocd_deadline )); do
    # Bound each poll by remaining shared budget so a hung apiserver on `get`
    # cannot outlast the sync window (same discipline as the apply-retry loop
    # above). Clamp to >= 1 for the `timeout 0 == no timeout` boundary.
    sync_remaining=$(( argocd_deadline - SECONDS ))
    (( sync_remaining < 1 )) && sync_remaining=1
    bad="$(timeout "${sync_remaining}" kubectl -n argocd get applications -o json 2>/dev/null | jq -r "${jq_bad}" || echo "ERR")"
    if [[ -z "${bad}" ]]; then
      echo "all Applications in terminal-pass state"
      echo "::endgroup::"
      return 0
    fi
    echo "waiting for: ${bad}"
    # Cap the poll interval to remaining budget so `sleep 15` near the
    # deadline can't overrun it. Positive-only guard so we exit the loop
    # cleanly when budget hits 0.
    sync_nap=15
    sync_remaining=$(( argocd_deadline - SECONDS ))
    (( sync_nap > sync_remaining )) && sync_nap=${sync_remaining}
    (( sync_nap > 0 )) && sleep "${sync_nap}"
  done
  echo "::error::Argo CD sync did not converge within ${ARGOCD_SYNC_TIMEOUT_SECONDS}s; last bad: ${bad}" >&2
  # Best-effort failure diagnostic — every call `|| true` so a transient
  # apiserver error on `get` doesn't skip the `describe` and repo-server
  # logs that follow (which are what a reviewer actually needs to diagnose
  # an OCI-pull auth error or a sync-wave block). Matches the KWOK failure
  # dump semantics.
  kubectl -n argocd get applications || true
  kubectl -n argocd describe applications 2>&1 | head -200 >&2 || true
  kubectl -n argocd logs -l app.kubernetes.io/name=argocd-repo-server --tail=100 2>&1 >&2 || true
  echo "::endgroup::"
  exit 1
}

install_readiness_gate() {
  # Readiness gate: run the deployment validation phase -- the authoritative
  # expected-resources / ClusterPolicy / DRA / nodewright checks the later
  # `--phase all` run gates on -- in a retry loop until it passes
  # READINESS_CONSECUTIVE_PASSES times in a row. `helmfile apply` returns before
  # operator-driven convergence, and nodewright (skyhook) reboots the GPU node
  # AFTER gpu-operator first reports ready, re-initing the operands; proxy
  # signals (ClusterPolicy=ready, Skyhook status.status) read that transient
  # state as "done" prematurely, so we gate on the real check. A SINGLE pass only
  # proves "converged at instant T" -- skyhook tuning reboots the node more than
  # once and re-opens status=in_progress between cycles, so a lone pass can land
  # in a lull. Requiring consecutive passes (each spaced by the retry sleep, and
  # each riding through a transient in_progress via expected-resources' own poll)
  # ensures the reboots have settled before conformance runs. Any failure resets
  # the streak.
  #
  # TWO distinct threats reset the streak (see READINESS_CONSECUTIVE_PASSES):
  # (1) temporal flapping of an EXISTING node -- a validate failure below; and
  # (2) census GROWTH (#2096) -- a late GPU node joins and Skyhook cordons+tunes
  # it. The streak alone is blind to (2): a not-yet-joined node cannot fail a
  # validate attempt. So each passing attempt ALSO folds in gpu_census_verdict --
  # a validate pass whose census is not settled (grown/cordoned) is treated as a
  # regression and resets the streak, riding census growth out on this same
  # budget. Fail-closed: the install fails if the streak is never reached within
  # the budget.
  #
  # Run against a copy of the config with spec.validate.evidence stripped so the
  # gate never emits/pushes an evidence bundle -- that is the conformance phase's
  # job (phase_conformance, `--phase all`).
  local gate_config gate_log
  gate_config="$(mktemp)"
  gate_log="$(mktemp)"
  yq 'del(.spec.validate.evidence)' "${config}" > "${gate_config}"
  # Persist each attempt's validate output (timestamped) to the bundle so the
  # status.status progression across the tuning-settling window survives into the
  # failure artifact — capturing the complete→in_progress flips as they happen
  # (see CLUSTER_DEBUG_GATE_LOG in tests/uat/lib/collect-debug.sh). The mktemp
  # gate_log is still used for the immediate on-screen failure diagnostic.
  mkdir -p "${CLUSTER_DEBUG_DIR}"
  : > "${CLUSTER_DEBUG_GATE_LOG}" || true
  local ready_deadline=$(( SECONDS + READINESS_TIMEOUT_SECONDS ))
  local attempt=1 streak=0 ready=false remaining attempt_result
  # Baseline for the periodic cloud-credential refresh (see cloud_refresh_credentials).
  local last_cred_refresh=${SECONDS}
  # GPU-node census fold-in (#2096): each streak-advancing attempt must ALSO see a
  # settled census. Opt out on the single-node lane (EXPECTED_GPU_NODES=skip).
  local census_expected="${EXPECTED_GPU_NODES:-}"
  local census_enabled=true
  if [[ "${census_expected}" == "skip" ]]; then
    census_enabled=false
    echo "gpu census streak fold-in disabled (EXPECTED_GPU_NODES=skip)"
  fi
  echo "::group::Readiness gate: validate --phase deployment x${READINESS_CONSECUTIVE_PASSES} consecutive (timeout ${READINESS_TIMEOUT_SECONDS}s)"
  # Check the deadline BEFORE each attempt so no validation run is launched once
  # the budget is spent (a single run can itself take minutes). The last
  # attempt's output is captured in gate_log for the failure diagnostic rather
  # than re-running validate past the deadline.
  while (( SECONDS < ready_deadline )); do
    # Keep cloud credentials fresh across the gate window. Default no-op; Azure's
    # federated CI session self-expires (~5m assertion) and overrides
    # cloud_refresh_credentials + lowers CLOUD_REFRESH_INTERVAL_SECONDS. The timer
    # only advances on success, so a transient refresh failure retries on the next
    # attempt while the currently-cached tokens are still valid.
    if (( SECONDS - last_cred_refresh >= CLOUD_REFRESH_INTERVAL_SECONDS )); then
      if cloud_refresh_credentials; then
        last_cred_refresh=${SECONDS}
      else
        echo "::warning::cloud credential refresh failed; retrying on the next gate attempt"
      fi
    fi
    # Bound each attempt with `timeout` (like the helmfile retry above) so a hung
    # validate cannot block past the deadline -- the loop only re-checks
    # ready_deadline between attempts. Cap at the remaining budget so no single
    # attempt overruns the gate window. Clamp to >= 1: SECONDS is re-read here and
    # can tick to ready_deadline between the while guard and this line, and
    # `timeout 0` means "no timeout" (runs forever) -- the exact hang we guard.
    remaining=$(( ready_deadline - SECONDS ))
    (( remaining < 1 )) && remaining=1
    if timeout "${remaining}" "${AICR_BIN}" validate --config "${gate_config}" --phase deployment --output /dev/null > "${gate_log}" 2>&1; then
      # Validate passed -> the CURRENTLY PRESENT nodes converged. Fold in the
      # GPU-node census (#2096): a late-joining GPU node that Skyhook just
      # cordoned+tuned re-opens convergence WITHOUT failing this attempt (it was
      # not present when the check ran), so a bare validate pass is not enough.
      local census_reason="" census_rc=0
      if [[ "${census_enabled}" == true ]]; then
        census_reason="$(gpu_census_check_once "${census_expected}")" || census_rc=$?
      fi
      if (( census_rc != 0 )); then
        # Census not settled (grown/cordoned) -> treat like a reboot regression.
        attempt_result="census-regress"
        echo "gpu census not settled (${census_reason}); resetting streak"
        streak=0
      else
        attempt_result=pass
        streak=$(( streak + 1 ))
        echo "deployment phase passed (attempt ${attempt}, streak ${streak}/${READINESS_CONSECUTIVE_PASSES})"
        if (( streak >= READINESS_CONSECUTIVE_PASSES )); then
          ready=true
        fi
      fi
    else
      # A regression (e.g. nodewright flipped back to in_progress on a reboot)
      # invalidates the streak -- start over.
      attempt_result=fail
      if (( streak > 0 )); then
        echo "deployment phase regressed after ${streak} consecutive pass(es); resetting streak"
      fi
      streak=0
      echo "deployment phase not ready (attempt ${attempt})"
    fi
    # Append this attempt (timestamped) to the persistent gate series. This is the
    # status.status time-series that lets a reviewer see the tuning flips directly.
    {
      echo "===== attempt ${attempt} @ $(date -u +%Y-%m-%dT%H:%M:%SZ) result=${attempt_result} streak=${streak}/${READINESS_CONSECUTIVE_PASSES} ====="
      cat "${gate_log}"
      echo
    } >> "${CLUSTER_DEBUG_GATE_LOG}" 2>/dev/null || true
    [[ "${ready}" == true ]] && break
    attempt=$(( attempt + 1 ))
    sleep 15
  done
  if [[ "${ready}" != true ]]; then
    echo "::error::deployment readiness gate did not reach ${READINESS_CONSECUTIVE_PASSES} consecutive passes within ${READINESS_TIMEOUT_SECONDS}s (last streak ${streak}); last attempt output:" >&2
    cat "${gate_log}" >&2 || true
    rm -f "${gate_config}" "${gate_log}"
    echo "::endgroup::"
    exit 1
  fi
  rm -f "${gate_config}" "${gate_log}"
  echo "deployment readiness gate passed (${READINESS_CONSECUTIVE_PASSES} consecutive, ${attempt} attempt(s))"
  echo "::endgroup::"
}

phase_conformance() {
  inject_push_target
  # Run ALL validation phases, not just conformance. The deployment phase is
  # the readiness barrier this UAT was missing: its health-check asserts poll
  # (chainsaw assert, ~6m budget) until the GPU stack converges — gpu-operator
  # ClusterPolicy reaches state=ready, the DRA kubelet-plugin DaemonSet is
  # fully rolled out, and nodewright node tuning completes. The helmfile bundle
  # (unlike the helm deploy.sh, which `kubectl wait`s on these) returns from
  # `helmfile apply` as soon as each release's own resources are ready, leaving
  # operator-driven work in flight; running conformance alone then validated a
  # not-yet-converged cluster and failed. The deployment phase gates that
  # convergence deployer-agnostically. The performance phase's only check,
  # nccl-all-reduce-bw, needs >=2 GPU nodes for its East-West fabric test and
  # is now active: each cloud's cluster-config provisions 2 GPU nodes in the
  # gpu-worker pool. If the pool is ever scaled back to a single GPU node, the
  # check skips gracefully (skip != fail) rather than failing.
  # Evidence is rendered/attested from the merged multi-phase report, so the
  # signed bundle covers all phases.

  # First-party platform sanity check. The emitted bundle's TestGrid tab
  # coordinate is derived from the recipe's author-declared `platform`
  # (inference→dynamo, training→kubeflow) and is NOT captured by the
  # fingerprint — pkg/fingerprint/match.go treats the platform dimension as
  # uncaptured — so a mis-declared platform would silently route evidence to
  # the wrong tab. Cross-check that the platform operator's workload CRD is
  # actually installed on the cluster the bundle deployed, so the declared
  # coordinate matches the deployed component set. Unknown/other platforms are
  # skipped (no false failure), a known platform whose CRD is absent fails closed.
  # Runs BEFORE `validate --phase all` emits + pushes the signed bundle: a
  # mis-declared platform must fail the leg before any incorrectly-routed
  # evidence is published to the wrong TestGrid tab, not after.
  echo "::group::Platform coordinate sanity check"
  local platform crd
  platform="$(yq -r '.criteria.platform // ""' recipe.yaml)"
  case "${platform}" in
    dynamo)   crd="dynamographdeployments.nvidia.com" ;;
    kubeflow) crd="trainjobs.trainer.kubeflow.org" ;;
    *)        crd="" ;;  # unknown/other platform: no cross-check wired
  esac
  if [[ -z "${crd}" ]]; then
    echo "recipe declares platform '${platform}': no workload-CRD cross-check wired for it (skipping)"
  elif ! kubectl get crd "${crd}" >/dev/null 2>&1; then
    echo "::error::recipe declares platform=${platform} but its workload CRD ${crd} is not installed — the emitted evidence would map to the wrong TestGrid tab" >&2
    kubectl get crd 2>&1 | head -50 >&2 || true
    echo "::endgroup::"
    exit 1
  else
    echo "platform=${platform}: workload CRD ${crd} present"
  fi
  echo "::endgroup::"

  # #2096 adjacency close. The install-phase readiness gate can pass with N GPU
  # nodes present; in the ~84s window before `validate --phase all` launches, a
  # late-joining GPU node can join and Skyhook cordons+tunes it (taint
  # skyhook.nvidia.com=...:NoSchedule + spec.unschedulable), re-opening convergence
  # WHILE validate runs -- so validate correctly fails on a genuinely non-converged
  # cluster (a false-red cell, no product defect). Re-assert a settled GPU-node
  # census right here, immediately before validate, so a growing/cordoned census
  # fails the cell EARLY with a self-explanatory message rather than surfacing as
  # an opaque validate failure. Bounded by CENSUS_STABILITY_TIMEOUT_SECONDS and
  # fails closed.
  echo "::group::GPU-node census stability gate (#2096)"
  if ! assert_gpu_census "${CENSUS_STABILITY_TIMEOUT_SECONDS}"; then
    echo "::error::GPU-node census did not stabilize before conformance (#2096 census guard: a late-joining GPU node likely re-opened Skyhook convergence). Failing the cell early instead of letting 'validate --phase all' fail on a non-converged cluster." >&2
    echo "::endgroup::"
    exit 1
  fi
  echo "::endgroup::"

  echo "::group::Validate (all phases) + emit signed evidence"
  # Capture the exit code rather than letting `set -e` abort: on a validate
  # failure we snapshot the skyhook CR + node reboot fingerprint INLINE — seconds
  # after the failing check gave up, while status.status is most likely still
  # in_progress — before propagating the failure. The teardown-time collector runs
  # minutes later, by when the CR may have re-converged and hidden the flip.
  local vrc=0
  "${AICR_BIN}" validate \
    --config "${config}" \
    --phase all \
    --output report.json || vrc=$?
  echo "::endgroup::"
  if (( vrc != 0 )); then
    capture_skyhook_snapshot conformance-validate
    return "${vrc}"
  fi

  if [[ ! -f ./evidence/pointer.yaml ]]; then
    echo "evidence pointer not emitted at ./evidence/pointer.yaml" >&2
    ls -la ./evidence >&2 || true
    exit 1
  fi
  echo "evidence pointer:"
  cat ./evidence/pointer.yaml
}

phase_train() {
  echo "::group::Submit TrainJob"
  kubectl apply -f - <<EOF
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainJob
metadata:
  name: ${TRAINJOB_NAME}
  namespace: ${TRAINJOB_NAMESPACE}
spec:
  trainer:
    numNodes: ${TRAINJOB_NUM_NODES}
    image: ${TRAINJOB_IMAGE}
    command:
      - python3
      - /opt/mnist/src/mnist.py
      - --epochs=1
    resourcesPerNode:
      requests:
        nvidia.com/gpu: 1
      limits:
        nvidia.com/gpu: 1
  runtimeRef:
    name: torch-distributed
    apiGroup: trainer.kubeflow.org
    kind: ClusterTrainingRuntime
EOF
  echo "::endgroup::"

  echo "::group::Wait for TrainJob completion (timeout ${TRAINJOB_TIMEOUT_SECONDS}s)"
  local deadline=$(( SECONDS + TRAINJOB_TIMEOUT_SECONDS ))
  local complete="" failed=""
  while (( SECONDS < deadline )); do
    complete="$(kubectl get trainjob "${TRAINJOB_NAME}" -n "${TRAINJOB_NAMESPACE}" \
      -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null || true)"
    failed="$(kubectl get trainjob "${TRAINJOB_NAME}" -n "${TRAINJOB_NAMESPACE}" \
      -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || true)"
    if [[ "${complete}" == "True" ]]; then
      echo "TrainJob Complete=True"
      break
    fi
    if [[ "${failed}" == "True" ]]; then
      echo "TrainJob Failed=True" >&2
      kubectl describe trainjob "${TRAINJOB_NAME}" -n "${TRAINJOB_NAMESPACE}" >&2 || true
      mkdir -p train-logs
      kubectl logs -n "${TRAINJOB_NAMESPACE}" -l "job-name=${TRAINJOB_NAME}-node-0" \
        --all-containers --tail=-1 > train-logs/"${TRAINJOB_NAME}".log 2>&1 || true
      exit 1
    fi
    sleep 15
  done
  if [[ "${complete}" != "True" ]]; then
    echo "TrainJob did not complete within ${TRAINJOB_TIMEOUT_SECONDS}s" >&2
    kubectl describe trainjob "${TRAINJOB_NAME}" -n "${TRAINJOB_NAMESPACE}" >&2 || true
    exit 1
  fi
  echo "::endgroup::"

  echo "::group::Capture TrainJob logs"
  mkdir -p train-logs
  kubectl logs -n "${TRAINJOB_NAMESPACE}" -l "job-name=${TRAINJOB_NAME}-node-0" \
    --all-containers --tail=-1 > train-logs/"${TRAINJOB_NAME}".log 2>&1 || true
  wc -l train-logs/"${TRAINJOB_NAME}".log || true
  echo "::endgroup::"
}

# serve_debug dumps the served-workload state (DGD, pods, describe, events,
# logs) into serve-logs/ and the step log so a failed or successful phase_serve
# is diagnosable from the uploaded artifacts. Best-effort; never fails the run.
serve_debug() {
  mkdir -p serve-logs
  {
    echo "--- dynamographdeployments ---"
    kubectl get dynamographdeployments -n "${SERVE_NAMESPACE}" -o yaml 2>&1 || true
    echo "--- pods ---"
    kubectl get pods -n "${SERVE_NAMESPACE}" -o wide 2>&1 || true
    echo "--- describe pods ---"
    kubectl describe pods -n "${SERVE_NAMESPACE}" 2>&1 || true
    echo "--- events ---"
    kubectl get events -n "${SERVE_NAMESPACE}" --sort-by=.lastTimestamp 2>&1 || true
    echo "--- logs (all pods, all containers, last 200 lines; --previous for crash loops) ---"
    for p in $(kubectl get pods -n "${SERVE_NAMESPACE}" -o name 2>/dev/null); do
      echo "=== ${p} ==="
      kubectl logs -n "${SERVE_NAMESPACE}" "${p#pod/}" --all-containers --tail=200 2>&1 || true
      # CrashLoopBackOff replaces the current container before this dump
      # runs, so the dying process's stdout is on --previous. Missing
      # previous (first start, or already GC'd) is a no-op.
      echo "=== ${p} (previous) ==="
      kubectl logs -n "${SERVE_NAMESPACE}" "${p#pod/}" --all-containers --previous --tail=200 2>&1 || true
    done
  } | tee serve-logs/"${SERVE_NAME}".log
}

# serve_render_manifest prints the serve manifests — KAI Queue, Namespace, and
# the DynamoGraphDeployment — to STDOUT and touches no cluster. It is split out
# from phase_serve so the scheduling contract documented there is assertable
# off-cluster: #1644 was a MISSING field (the Frontend had no nodeSelector), and
# only a test that reads the rendered manifest can catch an absent field. See
# TestServeGraphGPUPlacement in tests/uat/serve_manifest_test.go.
serve_render_manifest() {
  cat <<EOF
apiVersion: scheduling.run.ai/v2
kind: Queue
metadata:
  name: ${SERVE_QUEUE}
spec:
  parentQueue: default-parent-queue
  resources:
    gpu: {limit: -1, overQuotaWeight: 1, quota: 0}
    cpu: {limit: -1, overQuotaWeight: 1, quota: 0}
    memory: {limit: -1, overQuotaWeight: 1, quota: 0}
---
apiVersion: v1
kind: Namespace
metadata:
  name: ${SERVE_NAMESPACE}
---
apiVersion: nvidia.com/v1beta1
kind: DynamoGraphDeployment
metadata:
  name: ${SERVE_NAME}
  namespace: ${SERVE_NAMESPACE}
spec:
  backendFramework: vllm
  components:
    - name: Frontend
      type: frontend
      replicas: 1
      podTemplate:
        spec:
          nodeSelector:
            ${SERVE_GPU_NODE_SELECTOR_KEY}: ${SERVE_GPU_NODE_SELECTOR_VALUE}
          tolerations:
            - {key: dedicated, operator: Equal, value: worker-workload, effect: NoSchedule}
            - {key: dedicated, operator: Equal, value: worker-workload, effect: NoExecute}
            - {key: dedicated, operator: Equal, value: gpu-workload, effect: NoSchedule}
            - {key: dedicated, operator: Equal, value: system-workload, effect: NoSchedule}
            - {key: dedicated, operator: Equal, value: system-workload, effect: NoExecute}
            - {key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}
          containers:
            - name: main
              image: ${SERVE_RUNTIME_IMAGE}
              env:
                - {name: SERVED_MODEL_NAME, value: ${SERVE_MODEL}}
                - {name: DYN_ROUTER_MODE, value: kv}
                - {name: DYN_EVENT_PLANE, value: zmq}
    - name: VllmDecodeWorker
      type: worker
      replicas: 1
      sharedMemorySize: 2Gi
      podTemplate:
        spec:
          nodeSelector:
            ${SERVE_GPU_NODE_SELECTOR_KEY}: ${SERVE_GPU_NODE_SELECTOR_VALUE}
          tolerations:
            - {key: dedicated, operator: Equal, value: worker-workload, effect: NoSchedule}
            - {key: dedicated, operator: Equal, value: worker-workload, effect: NoExecute}
            - {key: dedicated, operator: Equal, value: gpu-workload, effect: NoSchedule}
            - {key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}
          containers:
            - name: main
              image: ${SERVE_RUNTIME_IMAGE}
              workingDir: /workspace/examples/backends/vllm
              # A bare `python3 -m dynamo.vllm` here crash-looped on GKE before
              # binding its health port (run 32732018329), while inference-perf
              # served the same runtime on the SAME cluster minutes earlier
              # using this wrapper. GKE mounts the node driver at
              # /usr/local/nvidia without putting it on LD_LIBRARY_PATH, so
              # vLLM cannot dlopen libcuda.so.1 ("Failed to infer device type",
              # observed live in gke-default qualification — see the same
              # wrapper in validators/performance/testdata/inference).
              # Shell APPEND with \${VAR:+} so we do not clobber
              # the image's nixl/ucx/cuda entries or create a leading empty
              # ld.so entry. Harmless no-op when the path is absent (EKS/AKS
              # GPU Operator toolkit). \$ so the unquoted heredoc does not
              # expand LD_LIBRARY_PATH at render time.
              command:
                - /bin/bash
                - -c
                - export LD_LIBRARY_PATH="\${LD_LIBRARY_PATH:+\${LD_LIBRARY_PATH}:}/usr/local/nvidia/lib64"; exec python3 -m dynamo.vllm "\$@"
                - dynamo.vllm
              env:
                - {name: DYN_EVENT_PLANE, value: zmq}
              args:
                - --model
                - ${SERVE_MODEL}
                - --kv-events-config
                - '{"enable_kv_cache_events":true,"publisher":"zmq","endpoint":"tcp://*:5557"}'
              resources:
                limits:
                  nvidia.com/gpu: 1
EOF
}

phase_serve() {
  # Deploy the served inference graph (Dynamo) — the intent=inference CUJ, the
  # DC3 counterpart of phase_train. Graph topology mirrors the
  # DynamoGraphDeployment in demos/cuj2-inference.md
  # (demos/workloads/inference/vllm-agg.yaml): the KAI queue and a
  # two-component (Frontend + decode Worker) graph serving an OpenAI-compatible
  # endpoint. Two intentional divergences from the demo: Frontend placement
  # (the demo pins nodeGroup=cpu-worker; this graph selects the GPU pool for
  # both components — pool-selection note below, #1644), and the worker
  # command (the demo uses python3 -m dynamo.vllm; this graph wraps it with
  # the GKE driver-lib append used by inference-perf, or vLLM cannot see
  # libcuda.so.1 on gke-default). The worker requests its GPU as a scalar
  # nvidia.com/gpu limit — the device-plugin production default (#1327).
  #
  # Tolerations are a portable SUPERSET of the taints across all UAT clusters
  # (AWS and GKE GPU pools use different `dedicated` values, the AKS pool carries
  # only nvidia.com/gpu; the DRA/gpu-operator adds nvidia.com/gpu). Tolerating a
  # taint a node does not carry is a no-op, so one list schedules correctly on
  # any of the clouds.
  #
  # Both components select the GPU pool (#1644). Left unselected, the Frontend
  # landed on a CPU-pool node (e2-standard-4 / n2-standard-8) and wedged in
  # ContainerCreating on the ~12GB vllm-runtime image, exceeding
  # SERVE_READY_TIMEOUT_SECONDS: on 4-8 vCPUs with modest egress, both the
  # download and the CPU-bound layer decompression are slow. The GPU pool's
  # a3-megagpu-8g class pulls the same image in a fraction of the budget, which
  # is what this selector buys. It therefore also needs the nvidia.com/gpu
  # toleration (the AKS pool carries only that taint). Sharing the pool consumes
  # no device: the Frontend declares no nvidia.com/gpu limit, so the device
  # plugin never allocates one to it. This matches how the inference-perf
  # validator places every component on the GPU cohort
  # (validators/performance/inference_perf_constraint.go). The worker command
  # is the same GKE driver-lib append as that validator's Dynamo templates;
  # without it, vLLM crash-loops on gke-default (run 32732018329).
  #
  # This selects the POOL, not a node: the pool holds two GPU nodes and nothing
  # constrains the two components to the same one, so they may be split. That is
  # deliberate rather than an oversight — a split yields two pulls running in
  # PARALLEL on two high-bandwidth nodes, so readiness waits out roughly one
  # pull either way. Do not read the co-location as load-bearing; it is not, and
  # there is no shared cache to inherit (the operator creates both components at
  # once, so kubelet merely dedupes co-located pulls into one).
  echo "::group::Deploy DynamoGraphDeployment (${SERVE_NAME} in ${SERVE_NAMESPACE})"
  serve_render_manifest | kubectl apply -f -
  echo "::endgroup::"

  echo "::group::Wait for DynamoGraphDeployment readiness (timeout ${SERVE_READY_TIMEOUT_SECONDS}s)"
  # Expected pod count = sum of replicas across the graph's components (Frontend +
  # decode Worker = 2 here). Gating on "all pods Ready" alone races: the Frontend
  # pod is created and goes Ready seconds before the Dynamo operator materializes
  # the Worker, so `total == ready_count` is briefly true with only the Frontend
  # present — the readiness gate would pass, then serve traffic with no worker.
  # Require at least the declared component count of pods before certifying ready.
  local expected_pods
  # The `|| expected_pods=""` is load-bearing: this runs under `set -euo
  # pipefail`, so without it a transient kubectl/jq failure (API hiccup, CRD
  # registration lag) makes the pipeline non-zero, the assignment propagates it,
  # and errexit would kill the whole leg — with both stderr streams sent to
  # /dev/null — before the `expected_pods=2` fallback below could run. Tolerate
  # the read failure here so the fallback stays reachable.
  expected_pods="$(kubectl get dynamographdeployments.nvidia.com "${SERVE_NAME}" -n "${SERVE_NAMESPACE}" \
    -o json 2>/dev/null | jq '[.spec.components[]?.replicas // 1] | add // 0' 2>/dev/null)" || expected_pods=""
  # Fall back to the known-minimum (Frontend + Worker) on any read failure or an
  # under-count, so a transient hiccup can never relax the gate below two
  # components (fail-safe: the gate only ever tightens, never loosens).
  if [[ -z "${expected_pods}" || "${expected_pods}" -lt 2 ]]; then
    expected_pods=2
  fi
  echo "expecting ${expected_pods} workload pod(s) (sum of component replicas)"
  local deadline=$(( SECONDS + SERVE_READY_TIMEOUT_SECONDS ))
  local ready=false pods_json total ready_count
  while (( SECONDS < deadline )); do
    pods_json="$(kubectl get pods -n "${SERVE_NAMESPACE}" -o json 2>/dev/null || echo '{}')"
    # Fail closed as soon as a workload pod is unrecoverable so a bad image or
    # crash loop surfaces immediately instead of burning the whole readiness
    # budget. Two cases: a terminal phase=Failed, OR a container wedged in an
    # image-pull / crash-loop / config-error waiting state — the latter keep the
    # pod in phase=Running/Pending, so they must be checked explicitly (a
    # multi-GB vllm-runtime pull that 404s would otherwise wait out the full
    # SERVE_READY_TIMEOUT_SECONDS).
    bad="$(echo "${pods_json}" | jq -r '
      .items[]? as $p
      | ( [ ($p.status.containerStatuses // [])[]?, ($p.status.initContainerStatuses // [])[]?
            | .state.waiting.reason // empty ]
          + (if $p.status.phase == "Failed" then ["phase=Failed"] else [] end) )[]
      | select(. == "phase=Failed" or . == "ImagePullBackOff" or . == "ErrImagePull"
               or . == "CrashLoopBackOff" or . == "InvalidImageName"
               or . == "CreateContainerConfigError")' 2>/dev/null | head -n1)"
    if [[ -n "${bad}" ]]; then
      echo "a workload pod is unrecoverable (${bad}); not waiting out the readiness budget" >&2
      break
    fi
    # Ready when all expected component pods exist and every pod reports
    # Ready=True. Gating on expected_pods (not just total>0) closes the race
    # where the Frontend is Ready before the Worker pod is even created.
    total="$(echo "${pods_json}" | jq '[.items[]?] | length')"
    ready_count="$(echo "${pods_json}" | jq '[.items[]? | select(.status.conditions[]? | select(.type=="Ready" and .status=="True"))] | length')"
    if [[ "${total}" -ge "${expected_pods}" && "${total}" == "${ready_count}" ]]; then
      ready=true
      break
    fi
    echo "workload not ready (${ready_count}/${total} pods Ready, need ${expected_pods}); retrying in 15s..."
    sleep 15
  done
  echo "::endgroup::"

  if [[ "${ready}" != true ]]; then
    echo "::error::DynamoGraphDeployment ${SERVE_NAME} did not become ready within ${SERVE_READY_TIMEOUT_SECONDS}s" >&2
    echo "::group::Serve failure debug"
    serve_debug
    echo "::endgroup::"
    exit 1
  fi

  echo "::group::Query the OpenAI-compatible endpoint"
  local svc="${SERVE_NAME}-frontend"
  if ! kubectl get svc -n "${SERVE_NAMESPACE}" "${svc}" >/dev/null 2>&1; then
    echo "::error::frontend service ${svc} not found in ${SERVE_NAMESPACE}" >&2
    kubectl get svc -n "${SERVE_NAMESPACE}" >&2 || true
    echo "::endgroup::"
    exit 1
  fi
  # Port-forward the frontend and issue a sample chat completion. The forwarder
  # is a background child; kill it on every exit path so a failing assertion
  # does not leak the port-forward (belt-and-braces to the script exit, which
  # would also reap it).
  kubectl port-forward -n "${SERVE_NAMESPACE}" "svc/${svc}" \
    "${SERVE_FRONTEND_PORT}:${SERVE_FRONTEND_PORT}" >/dev/null 2>&1 &
  local pf_pid=$!
  local up=false resp="" rc=0
  for _ in $(seq 1 30); do
    if curl -fsS "http://localhost:${SERVE_FRONTEND_PORT}/v1/models" >/dev/null 2>&1; then
      up=true
      break
    fi
    sleep 2
  done
  if [[ "${up}" == true ]]; then
    resp="$(curl -sS --max-time "${SERVE_REQUEST_TIMEOUT_SECONDS}" \
      -X POST "http://localhost:${SERVE_FRONTEND_PORT}/v1/chat/completions" \
      -H 'Content-Type: application/json' \
      -d "{\"model\":\"${SERVE_MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"What is Kubernetes?\"}],\"max_tokens\":64}")" || rc=$?
  fi
  kill "${pf_pid}" 2>/dev/null || true
  wait "${pf_pid}" 2>/dev/null || true
  echo "::endgroup::"

  if [[ "${up}" != true ]]; then
    echo "::error::frontend endpoint ${svc} did not accept connections within the port-forward window" >&2
    echo "::group::Serve failure debug"
    serve_debug
    echo "::endgroup::"
    exit 1
  fi
  echo "completion response:"
  echo "${resp}"
  # Assert a valid, non-empty OpenAI-compatible chat completion.
  if [[ "${rc}" -ne 0 ]] || \
     ! echo "${resp}" | jq -e '.choices[0].message.content | type == "string" and (length > 0)' >/dev/null 2>&1; then
    echo "::error::inference request did not return a valid chat completion (curl rc=${rc})" >&2
    echo "::group::Serve failure debug"
    serve_debug
    echo "::endgroup::"
    exit 1
  fi
  echo "served inference OK: /v1/chat/completions returned a non-empty completion"

  echo "::group::Capture serve logs"
  serve_debug >/dev/null
  wc -l serve-logs/"${SERVE_NAME}".log || true
  echo "::endgroup::"
}

phase_verify() {
  echo "::group::Bootstrap Sigstore TUF root"
  "${AICR_BIN}" trust update
  echo "::endgroup::"

  # Pin the expected signer when EXPECTED_ISSUER / EXPECTED_IDENTITY_REGEXP
  # are set (CI sets them to the workflow's OIDC identity). Without pinning,
  # any Fulcio-signed payload would pass verification — useful for local
  # dev where the signing identity is the user, mandatory in CI.
  local args=(evidence verify ./evidence/pointer.yaml)
  if [[ -n "${EXPECTED_ISSUER:-}" ]]; then
    args+=(--expected-issuer "${EXPECTED_ISSUER}")
  fi
  if [[ -n "${EXPECTED_IDENTITY_REGEXP:-}" ]]; then
    args+=(--expected-identity-regexp "${EXPECTED_IDENTITY_REGEXP}")
  fi
  args+=(-o evidence-result.json -t json)

  echo "::group::Verify signed evidence bundle"
  # Capture the exit code instead of letting `set -e` abort here: on a
  # verification failure the binary exits non-zero, which would kill the script
  # before the diagnostic `cat evidence-result.json` below ever ran and would
  # leave the ::group:: unclosed. Print the result, then propagate the status.
  local vrc=0
  "${AICR_BIN}" "${args[@]}" || vrc=$?
  echo "::endgroup::"

  if (( vrc != 0 )); then
    echo "::error::evidence verify exited ${vrc}" >&2
    cat evidence-result.json 2>/dev/null || true
    exit "${vrc}"
  fi

  local exit_code
  exit_code="$(jq -r '.exit' evidence-result.json)"
  echo "evidence verify exit code: ${exit_code}"
  if [[ "${exit_code}" != "0" ]]; then
    cat evidence-result.json
    exit 1
  fi
}

# uat_main parses args, validates required env, and dispatches to the requested
# phase. Called by each per-cloud runner as `uat_main "$@"`. phase/config are
# declared local here but remain visible to the phase functions via bash dynamic
# scoping (the functions read ${config}).
uat_main() {
  local phase="${1:-}" config="${2:-}"

  if [[ -z "${phase}" || -z "${config}" ]]; then
    echo "Usage: $0 <phase> <test-config.yaml>" >&2
    echo "Phases: prep | install | conformance | train | serve | verify | debug | all" >&2
    exit 2
  fi

  if [[ ! -f "${config}" ]]; then
    echo "test-config not found: ${config}" >&2
    exit 2
  fi

  : "${AICR_BIN:?Set AICR_BIN to the path of the aicr binary}"
  : "${RUN_ID:?Set RUN_ID (workflow passes \${{ github.run_id }}; locally use local-\$(date +%s))}"

  case "${phase}" in
    prep)        phase_prep ;;
    install)     phase_install ;;
    conformance) phase_conformance ;;
    train)       phase_train ;;
    serve)       phase_serve ;;
    verify)      phase_verify ;;
    debug)
      # Refresh cloud credentials first (no-op on AWS/GCP; Azure redeems a fresh
      # federated session). A failure that surfaces after a long phase can leave a
      # dead credential, which would make every kubectl in the collector no-op and
      # the bundle come out silently empty — so warn (don't abort) on failure
      # rather than swallowing it, so a truncated bundle is explained.
      cloud_refresh_credentials || echo "::warning::cloud credential refresh failed before debug collection; the bundle may come out empty" >&2
      collect_cluster_debug
      ;;
    all)
      # The CUJ phase is chosen by the config's recipe intent so `run all`
      # reproduces the right end-to-end flow: serve for inference, train
      # otherwise. Defaults to training if the intent is unset.
      phase_prep; phase_install; phase_conformance
      intent="$(yq -r '.spec.recipe.criteria.intent // "training"' "${config}")"
      case "${intent}" in
        inference) phase_serve ;;
        *)         phase_train ;;
      esac
      phase_verify
      ;;
    *)
      echo "unknown phase: ${phase}" >&2
      echo "Phases: prep | install | conformance | train | serve | verify | debug | all" >&2
      exit 2
      ;;
  esac
}
