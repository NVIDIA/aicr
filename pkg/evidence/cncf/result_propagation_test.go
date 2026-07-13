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
        # Dynamo operator is installed but no DynamoGraphDeployment exists and the
        # DGD query itself succeeds: an absent inference workload is an absent
        # prerequisite (SKIP), not a failure.
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            case " $* " in
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
    dynamo-prometheus-unavailable)
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
    trainer-prometheus-unavailable)
        kubectl() {
            if [ "${1:-}" = "cluster-info" ]; then
                return 0
            fi
            if [[ "$*" == *"jsonpath={.status.phase}"* ]]; then
                echo "Running"
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
			name:              "Dynamo Prometheus connection failure is explicit fail",
			mode:              "dynamo-prometheus-unavailable",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — Could not connect to Prometheus.",
			singleVerdictFile: "ai-service-metrics.md",
			wantErr:           true,
		},
		{
			name:              "trainer Prometheus connection failure is explicit fail",
			mode:              "trainer-prometheus-unavailable",
			displayName:       "AI Service Metrics",
			wantStatus:        "FAIL",
			wantEvidence:      "**Result: FAIL** — Could not connect to Prometheus.",
			singleVerdictFile: "ai-service-metrics.md",
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
			if tt.mode == "trainer-prometheus-unavailable" {
				manifestDir := filepath.Join(dir, "manifests")
				if err := os.MkdirAll(manifestDir, 0o755); err != nil {
					t.Fatalf("create manifest directory: %v", err)
				}
				manifestPath := filepath.Join(manifestDir, "trainer-pytorch-test.yaml")
				if err := os.WriteFile(manifestPath, []byte("---\n"), 0o600); err != nil {
					t.Fatalf("write trainer manifest fixture: %v", err)
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
				evidencePath := filepath.Join(outputDir, "ai-service-metrics.md")
				evidence, evidenceErr := os.ReadFile(evidencePath)
				if evidenceErr != nil {
					t.Fatalf("read evidence artifact: %v", evidenceErr)
				}
				if !strings.Contains(string(evidence), tt.wantEvidence) {
					t.Errorf("evidence does not contain %q", tt.wantEvidence)
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
	for _, line := range strings.Split(evidence, "\n") {
		if strings.HasPrefix(line, "**Result:") {
			count++
		}
	}
	return count
}
