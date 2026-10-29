// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cncf

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const resultPropagationHarness = `#!/usr/bin/env bash
source "${SCRIPT_DIR}/collect-evidence.sh"

# A developer's own NKTP_NAMESPACE must not steer backend resolution.
unset NKTP_NAMESPACE

kubectl() {
    [ "${1:-}" = "cluster-info" ]
}

detect_cluster_info() {
    CLUSTER_DESC="test-cluster"
    CLUSTER_K8S_VERSION="v1.test"
    CLUSTER_PLATFORM="linux/amd64"
    CLUSTER_OS_IMAGE="test-os"
}

mode="fixture"
if [ -f "${SCRIPT_DIR}/mode" ]; then
    IFS= read -r mode < "${SCRIPT_DIR}/mode"
fi

case "${mode}" in
    fixture)
        collect_gateway() {
            if [ -f "${SCRIPT_DIR}/fixture.md" ]; then
                cp "${SCRIPT_DIR}/fixture.md" "${EVIDENCE_DIR}/inference-gateway.md"
            fi
            if [ -f "${SCRIPT_DIR}/collector-fail" ]; then
                return 7
            fi
            return 0
        }
        SECTION="gateway"
        ;;
    gateway-absent)
        SECTION="gateway"
        ;;
    operator-absent)
        SECTION="operator"
        ;;
    operator-dynamo-no-dgd)
        # Dynamo operator is installed, its webhook DEMONSTRABLY rejects the
        # probe (the stub returns the webhook-attributed denial — the SKIP
        # branch sits behind the required webhook gate), but no
        # DynamoGraphDeployment exists and the DGD query itself succeeds:
        # an absent inference workload is an absent prerequisite (SKIP),
        # not a failure.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"--dry-run=server"*)
                    echo 'Error from server (Forbidden): error when creating "STDIN": admission webhook "vdynamographdeployment.kb.io" denied the request: spec.services must have at least one service' >&2
                    return 1 ;;
                *"dynamo-platform-dynamo-operator-controller-manager"*) echo "dynamo-operator 1/1" ;;
                *" dynamographdeployments "*) return 0 ;;
            esac
            return 0
        }
        SECTION="operator"
        ;;
    operator-dynamo-no-dgd-no-webhook)
        # Operator present, DGD query succeeds with zero rows, and the
        # webhook probe is ADMITTED (server dry-run succeeds — no denial of
        # any kind): the webhook gate must FAIL the section before the
        # absent-DGD SKIP branch can swallow the observed webhook failure.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"--dry-run=server"*)
                    echo "dynamographdeployment.nvidia.com/webhook-test-invalid created (server dry run)"
                    return 0 ;;
                *"dynamo-platform-dynamo-operator-controller-manager"*) echo "dynamo-operator 1/1" ;;
                *" dynamographdeployments "*) return 0 ;;
            esac
            return 0
        }
        SECTION="operator"
        ;;
    gang-barrier-pass)
        # Full two-phase gang happy path through the REAL collect_gang:
        # idle-node pick, blocker staging, occupancy gates at both ends,
        # binding + two-part gang-evaluation reads, release-phase waits,
        # and the verdict ladder. The occupancy read is stateful: 0 GPUs
        # used at pick time, 3 (the blocker) at the post-blocker gate and
        # the post-window re-check.
        sleep() { return 0; }
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"get namespace gang-scheduling-test"*)
                    return 1 ;;
                *"--field-selector"*)
                    local n=0
                    [ -f "${SCRIPT_DIR}/occ-count" ] && IFS= read -r n < "${SCRIPT_DIR}/occ-count"
                    n=$((n + 1)); printf '%s' "${n}" > "${SCRIPT_DIR}/occ-count"
                    if [ "${n}" -ge 2 ]; then
                        echo "Running A 3 I "
                    fi
                    return 0 ;;
                *"PodScheduled"*)
                    echo "False/Unschedulable"
                    return 0 ;;
                *" podgroup gang-test-group "*)
                    echo "PodSchedulingErrors: Resources were found for 1 pods while 2 are required for gang scheduling. Additional pods cannot be scheduled."
                    return 0 ;;
                *"spec.nodeName"*)
                    return 0 ;;
                *"containerStatuses"*)
                    return 0 ;;
                *"status.phase"*)
                    if [[ "$*" == *"gang-capacity-blocker"* ]]; then
                        echo "Running"
                    else
                        echo "Succeeded"
                    fi
                    return 0 ;;
                *"nvidia.com/gpu.present"*)
                    echo "gang-test-node 4 cordoned= ready=True"
                    return 0 ;;
            esac
            return 0
        }
        SECTION="gang"
        ;;
    operator-kubeflow-webhook-pass)
        # Exercises the Kubeflow branch of the operator dispatch (every
        # other operator lane routes to Dynamo): the exact-name webhook
        # grep for validator.trainjob.trainer.kubeflow.org AND the
        # readyReplicas >= 1 controller readiness (an HA-scaled 2-replica
        # controller must not be misread as unready under the stricter
        # webhook-required verdict).
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"dynamo-platform-dynamo-operator-controller-manager"*)
                    return 0 ;;
                *"k8s-nim-operator"*)
                    return 0 ;;
                *"readyReplicas"*)
                    echo "2"
                    return 0 ;;
                *"kubeflow-trainer-controller-manager --no-headers"*)
                    echo "kubeflow-trainer-controller-manager 2/2 2 2 5d"
                    return 0 ;;
                *"--dry-run=server"*)
                    echo 'Error from server (Forbidden): error when creating "STDIN": admission webhook "validator.trainjob.trainer.kubeflow.org" denied the request: specified clusterTrainingRuntime must be created before the TrainJob is created' >&2
                    return 1 ;;
                *" get crds "*)
                    printf '%s\n' "trainjobs.trainer.kubeflow.org" "trainingruntimes.trainer.kubeflow.org" "clustertrainingruntimes.trainer.kubeflow.org"
                    return 0 ;;
            esac
            return 0
        }
        SECTION="operator"
        ;;
    operator-dynamo-schema-reject)
        # A schema-shaped rejection (Required value) must land in the
        # NOT-the-webhook branch and FAIL — the exact regression the
        # schema-valid probe payloads guard against.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"--dry-run=server"*)
                    echo 'The DynamoGraphDeployment "webhook-test-invalid" is invalid: spec.services: Required value' >&2
                    return 1 ;;
                *"dynamo-platform-dynamo-operator-controller-manager"*) echo "dynamo-operator 1/1" ;;
                *" dynamographdeployments "*) return 0 ;;
            esac
            return 0
        }
        SECTION="operator"
        ;;
    operator-dynamo-foreign-webhook)
        # The probe is denied by a DIFFERENT admission webhook (a cluster
        # policy webhook) — the exact-name gate must classify this as NOT
        # demonstrating the operator's webhook and FAIL, never PASS or
        # SKIP. This is the discriminating branch of the name-pinned grep.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"--dry-run=server"*)
                    # The foreign name deliberately EMBEDS the operator's
                    # webhook name as a prefix: a naive substring grep would
                    # match it, so this lane fails if the exact-name
                    # (quote-anchored) matching is ever loosened.
                    echo 'Error from server (Forbidden): error when creating "STDIN": admission webhook "vdynamographdeployment.kb.io.evil.example.com" denied the request: denied by cluster policy' >&2
                    return 1 ;;
                *"dynamo-platform-dynamo-operator-controller-manager"*) echo "dynamo-operator 1/1" ;;
                *" dynamographdeployments "*) return 0 ;;
            esac
            return 0
        }
        SECTION="operator"
        ;;
    operator-dynamo-query-failed)
        # Dynamo operator is installed but the DGD query fails: preserve the
        # fail-closed behavior instead of treating the failed query as zero rows.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"dynamo-platform-dynamo-operator-controller-manager"*) echo "dynamo-operator 1/1" ;;
                *" dynamographdeployments "*) return 1 ;;
            esac
            return 0
        }
        SECTION="operator"
        ;;
    autoscaler-absent)
        SECTION="cluster-autoscaling"
        ;;
    dynamo-dispatch-list-failed)
        # Drives the REAL collect_service_metrics dispatcher (no override): the
        # Dynamo pod list fails while the namespace exists. The dispatcher must
        # route to the Dynamo collector so the failure surfaces, rather than
        # falling through to NIM/trainer and emitting unrelated evidence.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"dynamo-component-type=worker"*) return 1 ;;
                *" get namespace dynamo-workload "*) return 0 ;;
            esac
            return 0
        }
        SECTION="service-metrics"
        ;;
    dynamo-dispatch-both-queries-failed)
        # Both Dynamo probes fail (cluster-wide read failure: RBAC revoked,
        # expired credentials, apiserver outage). The namespace state cannot be
        # classified, so the dispatcher must NOT read that as "no workload" and
        # fall through to NIM/trainer — it routes to Dynamo, which fails closed.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"dynamo-component-type=worker"*) return 1 ;;
                *" get namespace dynamo-workload "*)
                    echo "Error from server (Forbidden): namespaces \"dynamo-workload\" is forbidden" >&2
                    return 1 ;;
            esac
            return 0
        }
        SECTION="service-metrics"
        ;;
    dynamo-workload-absent)
        # No worker pods: the section MEASURES an existing workload and must
        # never deploy one, so an absent workload is an absent prerequisite
        # (SKIP), not a cue to apply the manifest.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            return 0
        }
        collect_service_metrics() {
            EVIDENCE_FILE="${EVIDENCE_DIR}/ai-service-metrics.md"
            collect_service_metrics_dynamo
        }
        SECTION="service-metrics"
        ;;
    dynamo-workload-list-failed)
        # The pod list fails: a read error must fail closed rather than be
        # flattened into "no workload present".
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"dynamo-component-type=worker"*) return 1 ;;
            esac
            return 0
        }
        collect_service_metrics() {
            EVIDENCE_FILE="${EVIDENCE_DIR}/ai-service-metrics.md"
            collect_service_metrics_dynamo
        }
        SECTION="service-metrics"
        ;;
    hpa-scaled-cleanup-ok|hpa-scaled-cleanup-failed)
        # Both lanes observe a real scale-up (replicas>1 with a current metric),
        # so hpa_scaled is true and the ONLY difference is whether the test
        # namespace could be deleted. The section must not report PASS while its
        # unbounded CUDA workload may still be running, so the cleanup lane has
        # to flip the verdict to FAIL — and the control lane proves the FAIL is
        # attributable to cleanup rather than to a stub that never passes.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"averageValue"*) echo "75" ;;
                *"currentReplicas"*) echo "2" ;;
            esac
            return 0
        }
        sleep() { return 0; }
        if [ "${mode}" = "hpa-scaled-cleanup-failed" ]; then
            cleanup_ns() { return 1; }
        else
            cleanup_ns() { return 0; }
        fi
        SECTION="hpa"
        ;;
    dynamo-unhealthy)
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *" --no-headers "*) echo "dynamo-worker Pending" ;;
            esac
            return 0
        }
        sleep() { return 0; }
        collect_service_metrics() {
            EVIDENCE_FILE="${EVIDENCE_DIR}/ai-service-metrics.md"
            collect_service_metrics_dynamo
        }
        SECTION="service-metrics"
        ;;
    nim-unhealthy)
        collect_service_metrics() {
            EVIDENCE_FILE="${EVIDENCE_DIR}/ai-service-metrics.md"
            collect_service_metrics_nim
        }
        SECTION="service-metrics"
        ;;
    dynamo-lgtm-unavailable)
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            if [[ "$*" == *"component-type=worker"* && "$*" == *"--field-selector=status.phase=Running"* ]]; then
                echo "worker"
            elif [[ "$*" == *"component-type=frontend"* && "$*" == *"--field-selector=status.phase=Running"* ]]; then
                echo "frontend"
            elif [[ " $* " == *" --no-headers "* ]]; then
                echo "dynamo-worker Running"
            elif [[ "$*" == *"metadata.name=lgtm"* ]]; then
                echo "nktp"
            elif [[ " $* " == *" port-forward "* ]]; then
                return 1
            fi
            return 0
        }
        sleep() { return 0; }
        wait_for_port() { return 1; }
        collect_service_metrics() {
            EVIDENCE_FILE="${EVIDENCE_DIR}/ai-service-metrics.md"
            collect_service_metrics_dynamo
        }
        SECTION="service-metrics"
        ;;
    trainer-lgtm-unavailable)
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            if [[ "$*" == *"jsonpath={.status.phase}"* ]]; then
                echo "Running"
            elif [[ "$*" == *"metadata.name=lgtm"* ]]; then
                echo "nktp"
            elif [[ " $* " == *" port-forward "* ]]; then
                return 1
            fi
            return 0
        }
        sleep() { return 0; }
        wait_for_port() { return 1; }
        collect_service_metrics() {
            EVIDENCE_FILE="${EVIDENCE_DIR}/ai-service-metrics.md"
            collect_service_metrics_trainer
        }
        SECTION="service-metrics"
        ;;
    app-*)
        # Drives the REAL lgtm resolution, discovery, and application-series
        # gate of one AI service metrics collector against a fake otel-lgtm
        # that evaluates label selectors and the sample-age filter.
        # Mode: app-<workload>-<target>[-<app>]. workload: trainer|nim|dynamo.
        # target: up (healthy), down (up is 0), stale (up is 1 but sampled
        # before the check started), nolgtm (no Service named lgtm).
        # app: absent (series under the discovered target), empty, failed, or
        # elsewhere (series only under an unrelated workload).
        IFS=- read -r _ workload target app_state _ <<< "${mode}"
        case "${workload}" in
            trainer)
                job="pytorch-training-metrics" ns="trainer-metrics-test" inst="10.0.0.7:8080"
                metric="training_step_total" other_job="${job}" other_ns="other-team" other_inst="10.9.9.9:8080" ;;
            nim)
                job="nim-llm" ns="nim-workload" inst="10.0.1.5:8000"
                metric="prompt_tokens_total" other_job="vllm-other" other_ns="other-team" other_inst="10.9.9.9:8000" ;;
            dynamo)
                job="dynamo-system/dynamo-worker" ns="dynamo-workload" inst="10.0.2.3:9090"
                metric="dynamo_component_requests_total" other_job="${job}" other_ns="other-dynamo" other_inst="10.9.9.9:9090" ;;
        esac
        up_value=1 up_ts=1700000030
        case "${target}" in
            down) up_value=0 ;;
            stale) up_ts=1699999000 ;;
        esac
        app_job="${job}" app_ns="${ns}" app_inst="${inst}"
        if [ "${app_state}" = "elsewhere" ]; then
            app_job="${other_job}" app_ns="${other_ns}" app_inst="${other_inst}"
        fi
        series='[{"labels":{"__name__":"up","job":"'"${job}"'","namespace":"'"${ns}"'","instance":"'"${inst}"'"},"value":"'"${up_value}"'","ts":'"${up_ts}"'}'
        if [ "${app_state}" != "empty" ]; then
            series="${series}"',{"labels":{"__name__":"'"${metric}"'","job":"'"${app_job}"'","namespace":"'"${app_ns}"'","instance":"'"${app_inst}"'","model_name":"m"},"value":"42","ts":1700000040}'
        fi
        series="${series}]"
        cat > "${SCRIPT_DIR}/fake_promql.py" <<'PYEOF'
import json, os, re, sys
query = sys.argv[1]
series = json.loads(os.environ["SERIES"])
m = re.match(r'\s*([A-Za-z_:][A-Za-z0-9_:]*)?\{([^}]*)\}', query)
if not m:
    print(json.dumps({"status": "error", "error": "unsupported query"}))
    sys.exit(0)
matchers = []
if m.group(1):
    matchers.append(("__name__", "=", m.group(1)))
for key, op, val in re.findall(r'(\w+)\s*(=~|=)\s*"((?:[^"\\]|\\.)*)"', m.group(2)):
    matchers.append((key, op, re.sub(r'\\(.)', r'\1', val)))
since = re.search(r'timestamp\(.*\)\s*>\s*([0-9.]+)\)\s*$', query)
def selected(s):
    if since and s["ts"] <= float(since.group(1)):
        return False
    for key, op, val in matchers:
        have = s["labels"].get(key, "")
        if op == "=" and have != val:
            return False
        if op == "=~" and not re.fullmatch(val, have):
            return False
    return True
result = [{"metric": s["labels"], "value": [s["ts"], s["value"]]} for s in series if selected(s)]
print(json.dumps({"status": "success", "data": {"resultType": "vector", "result": result}}))
PYEOF
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"metadata.name=lgtm"*)
                    [ "${target}" != "nolgtm" ] && echo "nktp"
                    return 0 ;;
                *" apply "*)
                    echo "workload applied" > "${SCRIPT_DIR}/applied"
                    return 0 ;;
                *"jsonpath={.status.phase}"*)
                    echo "Running"
                    return 0 ;;
                *"component-type=worker"*"status.phase=Running"*)
                    echo "worker"
                    return 0 ;;
                *"component-type=frontend"*"status.phase=Running"*)
                    echo "frontend"
                    return 0 ;;
                *" --no-headers "*)
                    echo "dynamo-worker Running"
                    return 0 ;;
                *"k8s-nim-operator"*"status.phase=Running"*)
                    echo "nim-pod"
                    return 0 ;;
            esac
            return 0
        }
        curl() {
            # An unbounded request to otel-lgtm fails, so every PASS lane also
            # proves the timeouts are applied.
            case " $* " in *" --connect-timeout "*" --max-time "*) ;; *) return 28 ;; esac
            local last="${!#}"
            if [ "${last}" = "query=time()" ]; then
                echo '{"status":"success","data":{"resultType":"scalar","result":[1700000000.5,"1700000000.5"]}}'
                return 0
            fi
            case "${last}" in
                query=*)
                    case "${last}" in
                        "query=up{"*) ;;
                        *) if [ "${app_state}" = "failed" ]; then return 22; fi ;;
                    esac
                    SERIES="${series}" python3 "${SCRIPT_DIR}/fake_promql.py" "${last#query=}"
                    return 0 ;;
            esac
            return 0
        }
        sleep() { return 0; }
        wait_for_port() { return 0; }
        collect_service_metrics() {
            EVIDENCE_FILE="${EVIDENCE_DIR}/ai-service-metrics.md"
            "collect_service_metrics_${workload}"
        }
        SECTION="service-metrics"
        ;;
    accelerator-pass|accelerator-dcgm-absent|accelerator-exporter-empty|accelerator-backend-empty|accelerator-backend-unreachable)
        # Drives the REAL accelerator collector. PASS needs the exporter
        # Service, DCGM_FI_* series from its /metrics, and a non-empty
        # otel-lgtm query; each other lane removes one.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"metadata.name=lgtm"*)
                    echo "nktp"
                    return 0 ;;
                *"app=nvidia-dcgm-exporter -o jsonpath"*)
                    [ "${mode}" != "accelerator-dcgm-absent" ] && printf '%s' "nvidia-dcgm-exporter"
                    return 0 ;;
            esac
            return 0
        }
        curl() {
            case " $* " in *" --connect-timeout "*" --max-time "*) ;; *) return 28 ;; esac
            case " $* " in
                *"localhost:9401/metrics"*)
                    echo "# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %)."
                    if [ "${mode}" != "accelerator-exporter-empty" ]; then
                        echo 'DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-0"} 37'
                    fi ;;
                *"query=DCGM_FI_DEV_"*)
                    if [ "${mode}" = "accelerator-backend-empty" ]; then
                        echo '{"status":"success","data":{"resultType":"vector","result":[]}}'
                    else
                        echo '{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"DCGM_FI_DEV_GPU_UTIL","gpu":"0"},"value":[1700000000,"37"]}]}}'
                    fi ;;
            esac
            return 0
        }
        sleep() { return 0; }
        if [ "${mode}" = "accelerator-backend-unreachable" ]; then
            wait_for_port() { return 1; }
        else
            wait_for_port() { return 0; }
        fi
        SECTION="accelerator-metrics"
        ;;
    accelerator-backend-read-error|accelerator-backend-ambiguous|accelerator-nktp-namespace-missing)
        # lgtm resolution fails closed: a failed lookup, two lgtm Services,
        # or NKTP_NAMESPACE naming a namespace without lgtm.
        if [ "${mode}" = "accelerator-nktp-namespace-missing" ]; then
            NKTP_NAMESPACE="nktp"
        fi
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
                *"metadata.name=lgtm"*)
                    if [ "${mode}" = "accelerator-backend-read-error" ]; then
                        echo 'Error from server (Forbidden): services is forbidden' >&2
                        return 1
                    fi
                    printf '%s\n' "nktp" "observability"
                    return 0 ;;
                *" svc lgtm -n nktp "*)
                    echo 'Error from server (NotFound): services "lgtm" not found' >&2
                    return 1 ;;
            esac
            return 0
        }
        SECTION="accelerator-metrics"
        ;;
    *)
        echo "unknown test mode: ${mode}" >&2
        exit 2
        ;;
esac

main_rc=0
main || main_rc=$?
{
    printf '%b' "${CHECK_RESULTS}"
    printf 'main_rc:%s\n' "${main_rc}"
} > "${SCRIPT_DIR}/result.txt"
exit "${main_rc}"
`

// TestEvidenceResultPropagatesThroughCollector executes the shipped evidence
// script in a subprocess while replacing only the cluster-facing functions.
// This covers the complete result path without a Kubernetes cluster or GPU:
// markdown verdict parsing, main's summary/exit decision, and runSection's
// conversion of a nonzero script exit into a collector error.
func TestEvidenceResultPropagatesThroughCollector(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("bash not available; skipping evidence result subprocess test")
	}

	tests := []struct {
		name           string
		mode           string
		displayName    string
		fixture        string
		writeFixture   bool
		collectorFails bool
		staleOutput    bool
		wantStatus     string
		wantEvidence   string
		// wantEvidenceAlso lists further substrings the evidence must contain.
		wantEvidenceAlso []string
		// wantNotApplied asserts the section never ran kubectl apply.
		wantNotApplied bool
		// singleVerdictFile, when set, is an evidence artifact (relative to the
		// evidence dir) that must contain exactly one column-zero "**Result:"
		// verdict line — the invariant evidence_result relies on. Only set for
		// section collectors that emit their own verdict, not for injected
		// fixtures that deliberately carry zero or multiple verdicts.
		singleVerdictFile string
		wantErr           bool
	}{
		{
			name:         "pass",
			fixture:      "# Evidence\n\n**Result: PASS** — checks passed\n",
			writeFixture: true,
			wantStatus:   "PASS",
		},
		{
			name:         "partial pass preserves pass semantics",
			fixture:      "# Evidence\n\n**Result: PASS (partial)** — partial evidence accepted\n",
			writeFixture: true,
			wantStatus:   "PASS",
		},
		{
			name:         "explicit skip",
			fixture:      "# Evidence\n\n**Result: SKIP** — optional prerequisite absent\n",
			writeFixture: true,
			wantStatus:   "SKIP",
		},
		{
			name:              "absent gateway emits explicit skip",
			mode:              "gateway-absent",
			displayName:       "Inference Gateway",
			wantStatus:        "SKIP",
			singleVerdictFile: "inference-gateway.md",
		},
		{
			name:              "absent operator emits explicit skip",
			mode:              "operator-absent",
			displayName:       "Robust AI Operator",
			wantStatus:        "SKIP",
			singleVerdictFile: "robust-operator.md",
		},
		{
			name:              "present operator without DynamoGraphDeployment skips",
			mode:              "operator-dynamo-no-dgd",
			displayName:       "Robust AI Operator",
			wantStatus:        "SKIP",
			singleVerdictFile: "robust-operator.md",
		},
		{
			// The central regression case for the webhook-before-SKIP rule:
			// with no operator-attributed denial, an absent workload DGD
			// must NOT convert the webhook failure into a non-failing SKIP.
			name:              "present operator without DGD and without webhook denial fails",
			mode:              "operator-dynamo-no-dgd-no-webhook",
			displayName:       "Robust AI Operator",
			wantStatus:        "FAIL",
			singleVerdictFile: "robust-operator.md",
			wantErr:           true,
		},
		{
			// The PR's headline behavioral logic end to end: barrier
			// staging, both occupancy gates, the two-part gang-evaluation
			// gate, and joint completion — through the real collect_gang.
			name:              "gang two-phase barrier passes with full affirmative evidence",
			mode:              "gang-barrier-pass",
			displayName:       "Gang Scheduling",
			wantStatus:        "PASS",
			singleVerdictFile: "gang-scheduling.md",
		},
		{
			// The Kubeflow dispatch branch: exact-name webhook grep plus
			// readyReplicas>=1 (HA controller must not read as unready).
			name:              "kubeflow webhook rejection passes with HA controller",
			mode:              "operator-kubeflow-webhook-pass",
			displayName:       "Robust AI Operator",
			wantStatus:        "PASS",
			singleVerdictFile: "robust-operator.md",
		},
		{
			// A schema-shaped rejection must not read as a webhook denial.
			name:              "schema rejection does not demonstrate the operator webhook",
			mode:              "operator-dynamo-schema-reject",
			displayName:       "Robust AI Operator",
			wantStatus:        "FAIL",
			singleVerdictFile: "robust-operator.md",
			wantErr:           true,
		},
		{
			// The discriminating branch of the exact-name webhook gate: a
			// denial from a foreign (policy) webhook must FAIL, not count.
			name:              "foreign webhook denial does not demonstrate the operator webhook",
			mode:              "operator-dynamo-foreign-webhook",
			displayName:       "Robust AI Operator",
			wantStatus:        "FAIL",
			singleVerdictFile: "robust-operator.md",
			wantErr:           true,
		},
		{
			name:              "operator DGD query failure fails closed",
			mode:              "operator-dynamo-query-failed",
			displayName:       "Robust AI Operator",
			wantStatus:        "FAIL",
			singleVerdictFile: "robust-operator.md",
			wantErr:           true,
		},
		{
			name:              "absent autoscaler emits explicit skip",
			mode:              "autoscaler-absent",
			displayName:       "Cluster Autoscaling",
			wantStatus:        "SKIP",
			singleVerdictFile: "cluster-autoscaling.md",
		},
		{
			name: "fail is not masked by overview prose",
			fixture: "# Evidence\n\n## Summary\n\n" +
				"6. **Result: PASS**\n\n**Result: FAIL** — live check failed\n",
			writeFixture: true,
			wantStatus:   "FAIL",
			wantErr:      true,
		},
		{
			name:       "missing file fails closed",
			wantStatus: "FAIL",
			wantErr:    true,
		},
		{
			name:        "stale result is removed before missing-file failure",
			staleOutput: true,
			wantStatus:  "FAIL",
			wantErr:     true,
		},
		{
			name:         "missing verdict fails closed",
			fixture:      "# Evidence\n\nChecks ended without a verdict.\n",
			writeFixture: true,
			wantStatus:   "FAIL",
			wantErr:      true,
		},
		{
			name:         "unknown verdict fails closed",
			fixture:      "# Evidence\n\n**Result: UNKNOWN**\n",
			writeFixture: true,
			wantStatus:   "FAIL",
			wantErr:      true,
		},
		{
			name: "valid plus unknown verdict fails closed",
			fixture: "# Evidence\n\n**Result: PASS**\n" +
				"**Result: UNKNOWN**\n",
			writeFixture: true,
			wantStatus:   "FAIL",
			wantErr:      true,
		},
		{
			name:         "malformed verdict fails closed",
			fixture:      "# Evidence\n\n**Result: PASS** unexpected trailing text\n",
			writeFixture: true,
			wantStatus:   "FAIL",
			wantErr:      true,
		},
		{
			name: "multiple verdicts fail closed",
			fixture: "# Evidence\n\n**Result: PASS**\n" +
				"**Result: SKIP** — conflicting verdict\n",
			writeFixture: true,
			wantStatus:   "FAIL",
			wantErr:      true,
		},
		{
			name:           "collector subprocess failure overrides pass",
			fixture:        "# Evidence\n\n**Result: PASS**\n",
			writeFixture:   true,
			collectorFails: true,
			wantStatus:     "FAIL",
			wantErr:        true,
		},
		{
			name:              "dispatcher routes a failed Dynamo list to the Dynamo collector",
			mode:              "dynamo-dispatch-list-failed",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "the workload state is unknown",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "unclassifiable Dynamo namespace state fails closed",
			mode:              "dynamo-dispatch-both-queries-failed",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "the workload state is unknown",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "absent Dynamo workload skips instead of deploying",
			mode:              "dynamo-workload-absent",
			displayName:       "AI Service Metrics",
			wantStatus:        "SKIP",
			wantEvidence:      "no running Dynamo workload in dynamo-workload",
			singleVerdictFile: "ai-service-metrics.md",
		},
		{
			name:              "Dynamo worker pod list failure fails closed",
			mode:              "dynamo-workload-list-failed",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "the workload state is unknown",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "HPA scale-up with successful cleanup is pass",
			mode:              "hpa-scaled-cleanup-ok",
			displayName:       "Pod Autoscaling (HPA)",
			wantStatus:        "PASS",
			singleVerdictFile: "pod-autoscaling.md",
			wantErr:           false,
		},
		{
			name:              "HPA scale-up with failed cleanup is fail",
			mode:              "hpa-scaled-cleanup-failed",
			displayName:       "Pod Autoscaling (HPA)",
			wantStatus:        "FAIL",
			wantEvidence:      "the hpa-test namespace could not be deleted",
			singleVerdictFile: "pod-autoscaling.md",
			wantErr:           true,
		},
		{
			name:              "present but unhealthy Dynamo is fail",
			mode:              "dynamo-unhealthy",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "present but unhealthy NIM is fail",
			mode:              "nim-unhealthy",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "Dynamo otel-lgtm connection failure is explicit fail",
			mode:              "dynamo-lgtm-unavailable",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — Could not connect to otel-lgtm (svc/lgtm in nktp).",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "trainer otel-lgtm connection failure is explicit fail",
			mode:              "trainer-lgtm-unavailable",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — Could not connect to otel-lgtm (svc/lgtm in nktp).",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:        "healthy up series with scoped application series is pass",
			mode:        "app-trainer-up",
			displayName: "AI Service Metrics",
			wantStatus:  "PASS",
			wantEvidence: `$ promql 'up{job=~"pytorch-training-metrics",namespace=~"trainer-metrics-test"} and ` +
				`(timestamp(up{job=~"pytorch-training-metrics",namespace=~"trainer-metrics-test"}) > 1700000000.5)'` + "\n" +
				`{"job": "pytorch-training-metrics", "instance": "10.0.0.7:8080", "namespace": "trainer-metrics-test", "up": "1"}`,
			wantEvidenceAlso: []string{
				`# application queries are scoped to {job=~"pytorch-training-metrics",namespace=~"trainer-metrics-test",instance=~"10\\.0\\.0\\.7:8080"}`,
				"training_step_total = 42",
				"**Result: PASS** — The PyTorch training workload's ServiceMonitor target was discovered and scraped, " +
					"and its training metrics (step count, loss, throughput, GPU memory) are queryable in otel-lgtm.",
			},
			singleVerdictFile: "ai-service-metrics.md",
		},
		{
			name:              "up series of 0 is fail",
			mode:              "app-trainer-down",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      `{"job": "pytorch-training-metrics", "instance": "10.0.0.7:8080", "namespace": "trainer-metrics-test", "up": "0"}`,
			wantEvidenceAlso:  []string{"appeared in otel-lgtm within 3 minutes. NKTP needs no monitor label"},
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "up series sampled before the check started is fail",
			mode:              "app-trainer-stale",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "(no series)",
			wantEvidenceAlso:  []string{"appeared in otel-lgtm within 3 minutes."},
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "empty application queries are fail",
			mode:              "app-trainer-up-empty",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — discovery succeeded, but no application series came back from otel-lgtm; NKTP's OTLP export may have renamed them",
			wantEvidenceAlso:  []string{"(no training series returned by otel-lgtm)"},
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "failed application queries are fail",
			mode:              "app-trainer-up-failed",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — discovery succeeded, but no application series came back from otel-lgtm;",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "no lgtm Service fails before deploying the trainer",
			mode:              "app-trainer-nolgtm",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — NKTP's otel-lgtm was not found: no Service named lgtm in any namespace.",
			singleVerdictFile: "ai-service-metrics.md",
			wantNotApplied:    true,
			wantErr:           true,
		},
		{
			name:              "trainer series only under an unrelated namespace is fail",
			mode:              "app-trainer-up-elsewhere",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — discovery succeeded, but no application series came back from otel-lgtm;",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "NIM series scoped to the discovered target is pass",
			mode:              "app-nim-up",
			displayName:       "AI Service Metrics",
			wantStatus:        "PASS",
			wantEvidence:      `prompt_tokens_total{model_name="m"} = 42`,
			wantEvidenceAlso:  []string{"**Result: PASS** — The NIM inference workload's ServiceMonitor targets were discovered and scraped"},
			singleVerdictFile: "ai-service-metrics.md",
		},
		{
			name:              "NIM series only under an unrelated job and namespace is fail",
			mode:              "app-nim-up-elsewhere",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — discovery succeeded, but no application series came back from otel-lgtm;",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:        "Dynamo series scoped to the workload pods' namespace is pass",
			mode:        "app-dynamo-up",
			displayName: "AI Service Metrics",
			wantStatus:  "PASS",
			wantEvidence: `# application queries are scoped to ` +
				`{job=~"dynamo-system/dynamo-worker",namespace=~"dynamo-workload",instance=~"10\\.0\\.2\\.3:9090"}`,
			wantEvidenceAlso:  []string{"dynamo_component_requests_total = 42", "**Result: PASS** — The Dynamo inference workload's"},
			singleVerdictFile: "ai-service-metrics.md",
		},
		{
			name:              "Dynamo series only under another workload namespace is fail",
			mode:              "app-dynamo-up-elsewhere",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — discovery succeeded, but no application series came back from otel-lgtm;",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "accelerator evidence with exporter series and otel-lgtm data is pass",
			mode:              "accelerator-pass",
			displayName:       "Accelerator Metrics",
			wantStatus:        "PASS",
			wantEvidence:      `DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-0"} 37`,
			wantEvidenceAlso:  []string{"**Result: PASS** — DCGM exporter provides per-GPU metrics", `"__name__": "DCGM_FI_DEV_GPU_UTIL"`},
			singleVerdictFile: "accelerator-metrics.md",
		},
		{
			name:              "accelerator evidence without the exporter Service is fail",
			mode:              "accelerator-dcgm-absent",
			displayName:       "Accelerator Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — DCGM exporter Service not found in gpu-operator.",
			singleVerdictFile: "accelerator-metrics.md",
			wantErr:           true,
		},
		{
			name:              "accelerator evidence without exporter DCGM series is fail",
			mode:              "accelerator-exporter-empty",
			displayName:       "Accelerator Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — no DCGM_FI_* series read from the exporter's /metrics.",
			singleVerdictFile: "accelerator-metrics.md",
			wantErr:           true,
		},
		{
			name:              "accelerator evidence with empty otel-lgtm queries is fail",
			mode:              "accelerator-backend-empty",
			displayName:       "Accelerator Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — none of the four DCGM queries returned data from otel-lgtm.",
			wantEvidenceAlso:  []string{`"result": []`},
			singleVerdictFile: "accelerator-metrics.md",
			wantErr:           true,
		},
		{
			name:              "accelerator evidence without an otel-lgtm port-forward is fail",
			mode:              "accelerator-backend-unreachable",
			displayName:       "Accelerator Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — could not port-forward to otel-lgtm (svc/lgtm in nktp).",
			singleVerdictFile: "accelerator-metrics.md",
			wantErr:           true,
		},
		{
			name:              "failed lgtm lookup fails closed",
			mode:              "accelerator-backend-read-error",
			displayName:       "Accelerator Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — could not list Services named lgtm to find NKTP's otel-lgtm",
			singleVerdictFile: "accelerator-metrics.md",
			wantErr:           true,
		},
		{
			name:              "lgtm Services in two namespaces are ambiguous",
			mode:              "accelerator-backend-ambiguous",
			displayName:       "Accelerator Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "set NKTP_NAMESPACE to choose NKTP's",
			singleVerdictFile: "accelerator-metrics.md",
			wantErr:           true,
		},
		{
			name:              "explicit NKTP namespace without lgtm fails closed",
			mode:              "accelerator-nktp-namespace-missing",
			displayName:       "Accelerator Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — NKTP_NAMESPACE=nktp is set, but Service lgtm could not be read there",
			singleVerdictFile: "accelerator-metrics.md",
			wantErr:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			outputDir := filepath.Join(dir, "evidence")
			if err := os.MkdirAll(outputDir, 0o755); err != nil {
				t.Fatalf("create output directory: %v", err)
			}

			libraryPath := filepath.Join(dir, "collect-evidence.sh")
			if err := os.WriteFile(libraryPath, collectScript, 0o700); err != nil { //nolint:gosec // test script needs execute permission
				t.Fatalf("write embedded evidence script: %v", err)
			}
			harnessPath := filepath.Join(dir, "result-harness.sh")
			if err := os.WriteFile(harnessPath, []byte(resultPropagationHarness), 0o700); err != nil { //nolint:gosec // test harness needs execute permission
				t.Fatalf("write result harness: %v", err)
			}
			if tt.writeFixture {
				if err := os.WriteFile(filepath.Join(dir, "fixture.md"), []byte(tt.fixture), 0o600); err != nil {
					t.Fatalf("write evidence fixture: %v", err)
				}
			}
			if tt.mode != "" {
				if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(tt.mode+"\n"), 0o600); err != nil {
					t.Fatalf("write harness mode: %v", err)
				}
			}
			if strings.HasPrefix(tt.mode, "trainer-") || strings.HasPrefix(tt.mode, "app-trainer-") {
				manifestDir := filepath.Join(dir, "manifests")
				if err := os.MkdirAll(manifestDir, 0o755); err != nil {
					t.Fatalf("create manifest directory: %v", err)
				}
				manifestPath := filepath.Join(manifestDir, "trainer-pytorch-test.yaml")
				if err := os.WriteFile(manifestPath, []byte("---\n"), 0o600); err != nil {
					t.Fatalf("write trainer manifest fixture: %v", err)
				}
			}
			if strings.HasPrefix(tt.mode, "hpa-") {
				manifestDir := filepath.Join(dir, "manifests")
				if err := os.MkdirAll(manifestDir, 0o755); err != nil {
					t.Fatalf("create manifest directory: %v", err)
				}
				manifestPath := filepath.Join(manifestDir, "hpa-gpu-test.yaml")
				if err := os.WriteFile(manifestPath, []byte("---\n"), 0o600); err != nil {
					t.Fatalf("write HPA manifest fixture: %v", err)
				}
			}
			if strings.HasPrefix(tt.mode, "gang-") {
				manifestDir := filepath.Join(dir, "manifests")
				if err := os.MkdirAll(manifestDir, 0o755); err != nil {
					t.Fatalf("create manifest directory: %v", err)
				}
				manifestPath := filepath.Join(manifestDir, "gang-scheduling-test.yaml")
				if err := os.WriteFile(manifestPath, []byte("# GANG_TEST_NODE placeholder fixture\n---\n"), 0o600); err != nil {
					t.Fatalf("write gang manifest fixture: %v", err)
				}
			}
			if tt.collectorFails {
				if err := os.WriteFile(filepath.Join(dir, "collector-fail"), nil, 0o600); err != nil {
					t.Fatalf("write collector failure marker: %v", err)
				}
			}
			if tt.staleOutput {
				stale := []byte("# Stale evidence\n\n**Result: PASS**\n")
				if err := os.WriteFile(filepath.Join(outputDir, "inference-gateway.md"), stale, 0o600); err != nil {
					t.Fatalf("write stale evidence: %v", err)
				}
			}

			collector := NewCollector(outputDir)
			err := collector.runSection(context.Background(), harnessPath, dir, "result-fixture")
			if (err != nil) != tt.wantErr {
				t.Fatalf("runSection() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var structuredErr *errors.StructuredError
				if !stderrors.As(err, &structuredErr) {
					t.Fatalf("runSection() error is %T, want *errors.StructuredError", err)
				}
				if structuredErr.Code != errors.ErrCodeInternal {
					t.Errorf("runSection() error code = %s, want %s", structuredErr.Code, errors.ErrCodeInternal)
				}
			}

			result, readErr := os.ReadFile(filepath.Join(dir, "result.txt"))
			if readErr != nil {
				t.Fatalf("read harness result: %v", readErr)
			}
			wantRC := 0
			if tt.wantErr {
				wantRC = 1
			}
			displayName := tt.displayName
			if displayName == "" {
				displayName = "Inference Gateway"
			}
			want := displayName + ":" + tt.wantStatus + "\nmain_rc:" + strconv.Itoa(wantRC) + "\n"
			if got := string(result); got != want {
				t.Errorf("result = %q, want %q", got, want)
			}
			if tt.wantEvidence != "" {
				// Assert against the artifact the case names, falling back to
				// the service-metrics file the original lanes all used.
				evidenceName := tt.singleVerdictFile
				if evidenceName == "" {
					evidenceName = "ai-service-metrics.md"
				}
				evidencePath := filepath.Join(outputDir, evidenceName)
				evidence, evidenceErr := os.ReadFile(evidencePath)
				if evidenceErr != nil {
					t.Fatalf("read evidence artifact: %v", evidenceErr)
				}
				for _, want := range append([]string{tt.wantEvidence}, tt.wantEvidenceAlso...) {
					if !strings.Contains(string(evidence), want) {
						t.Errorf("evidence does not contain %q\n%s", want, evidence)
					}
				}
			}
			if tt.wantNotApplied {
				if _, statErr := os.Stat(filepath.Join(dir, "applied")); statErr == nil {
					t.Error("section applied a workload manifest, want none")
				} else if !stderrors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("stat applied marker: %v", statErr)
				}
			}
			if tt.singleVerdictFile != "" {
				evidencePath := filepath.Join(outputDir, tt.singleVerdictFile)
				evidence, evidenceErr := os.ReadFile(evidencePath)
				if evidenceErr != nil {
					t.Fatalf("read evidence artifact %s: %v", tt.singleVerdictFile, evidenceErr)
				}
				if got := countColumnZeroVerdicts(string(evidence)); got != 1 {
					t.Errorf("evidence %s has %d column-zero **Result: verdicts, want exactly 1", tt.singleVerdictFile, got)
				}
			}
		})
	}
}

// countColumnZeroVerdicts counts lines that begin at column zero with the
// "**Result:" verdict marker — the same anchoring evidence_result() uses to
// ignore numbered overview prose. Section collectors must emit exactly one so
// the parser never fails closed on a healthy check.
func countColumnZeroVerdicts(evidence string) int {
	count := 0
	for line := range strings.SplitSeq(evidence, "\n") {
		if strings.HasPrefix(line, "**Result:") {
			count++
		}
	}
	return count
}
