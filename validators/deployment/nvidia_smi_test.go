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

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestGpuNodeCoverage_CoverageExtra(t *testing.T) {
	tests := []struct {
		name     string
		coverage gpuNodeCoverage
		want     map[string]string
	}{
		{
			name:     "full coverage",
			coverage: gpuNodeCoverage{total: 2, passed: 2},
			want:     map[string]string{"nodesValidated": "2", "nodesTotal": "2", "nodesFailed": "0", "nodesSkipped": "0"},
		},
		{
			name:     "one busy node skipped",
			coverage: gpuNodeCoverage{total: 2, passed: 1, busy: []string{"b"}},
			want:     map[string]string{"nodesValidated": "1", "nodesTotal": "2", "nodesFailed": "0", "nodesSkipped": "1"},
		},
		{
			name: "every skip cause counts toward nodesSkipped",
			coverage: gpuNodeCoverage{total: 5, passed: 1, failed: 1,
				busy: []string{"b"}, probeErrors: []string{"e"}, cordoned: []string{"c"}},
			want: map[string]string{"nodesValidated": "1", "nodesTotal": "5", "nodesFailed": "1", "nodesSkipped": "3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extra := tt.coverage.coverageExtra()
			if len(extra) != len(tt.want) {
				t.Fatalf("coverageExtra() = %v, want %v", extra, tt.want)
			}
			for k, v := range tt.want {
				if extra[k] != v {
					t.Errorf("coverageExtra()[%q] = %q, want %q", k, extra[k], v)
				}
			}
			// The emitted transport line must be valid low-cardinality JSON that
			// the orchestrator can parse back — mirror the EmitExtra contract.
			data, err := json.Marshal(extra)
			if err != nil {
				t.Fatalf("coverage extra must marshal to JSON: %v", err)
			}
			payload, ok := strings.CutPrefix(ctrf.ExtraLinePrefix+string(data), ctrf.ExtraLinePrefix)
			if !ok {
				t.Fatalf("emitted line missing sentinel prefix")
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(payload), &got); err != nil {
				t.Fatalf("emitted payload is not valid JSON: %v", err)
			}
		})
	}
}

func TestNvidiaSMISkipExtra(t *testing.T) {
	for _, reason := range []string{skipReasonNoGPUNodes, skipReasonNoSchedulableGPUNodes, skipReasonNodesBusy} {
		t.Run(reason, func(t *testing.T) {
			extra := nvidiaSMISkipExtra(reason)
			if extra["skipReason"] != reason {
				t.Errorf("skipReason = %q, want %q", extra["skipReason"], reason)
			}
			if len(extra) != 1 {
				t.Errorf("skip extra must carry exactly skipReason, got %v", extra)
			}
		})
	}
}

func TestVerifyNvidiaSMILogs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		logs    string
		wantErr string
	}{
		{
			name: "accepts legacy banner fields",
			logs: "NVIDIA-SMI\nDriver Version: 570.86.15\nCUDA Version: 12.8\n" + gpuCheckSuccessMsg,
		},
		{
			name: "accepts renamed banner fields",
			logs: "NVIDIA-SMI\nKMD Version: 580.65.06\nCUDA UMD Version: 13.0\n" + gpuCheckSuccessMsg,
		},
		{
			// Representative table-banner layout of a renamed-field driver
			// branch (see issue #1667): single header row, pipe-delimited,
			// fields separated by padding rather than newlines.
			name: "accepts renamed banner in table layout",
			logs: "| NVIDIA-SMI 610.43.02              KMD Version: 610.43.02     CUDA UMD Version: 13.3     |\n" +
				gpuCheckSuccessMsg,
		},
		{
			name: "accepts mixed legacy and renamed banner fields",
			logs: "NVIDIA-SMI\nDriver Version: 570.86.15\nCUDA UMD Version: 13.0\n" + gpuCheckSuccessMsg,
		},
		{
			// The renamed fields are documented only via `nvidia-smi
			// --version` deprecation text, which spells them lowercase
			// ("KMD version"); no fixture pins the table banner's casing
			// (issue #1667), so matching is case-insensitive.
			name: "accepts lowercase renamed banner fields",
			logs: "NVIDIA-SMI\nKMD version: 580.65.06\nCUDA UMD version: 13.0\n" + gpuCheckSuccessMsg,
		},
		{
			name: "accepts uppercase legacy banner fields",
			logs: "NVIDIA-SMI\nDRIVER VERSION: 570.86.15\nCUDA VERSION: 12.8\n" + gpuCheckSuccessMsg,
		},
		{
			name:    "rejects logs missing both driver banner alternatives",
			logs:    "NVIDIA-SMI\nCUDA UMD Version: 13.0\n" + gpuCheckSuccessMsg,
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [Driver Version: or KMD Version:]",
		},
		{
			name:    "rejects logs missing both CUDA banner alternatives",
			logs:    "NVIDIA-SMI\nKMD Version: 580.65.06\n" + gpuCheckSuccessMsg,
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [CUDA Version: or CUDA UMD Version:]",
		},
		{
			name:    "separates multiple missing marker groups",
			logs:    "NVIDIA-SMI\n" + gpuCheckSuccessMsg,
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [Driver Version: or KMD Version:; CUDA Version: or CUDA UMD Version:]",
		},
		{
			name:    "rejects logs missing NVIDIA-SMI marker",
			logs:    "Driver Version: 570.86.15\nCUDA Version: 12.8\n" + gpuCheckSuccessMsg,
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [NVIDIA-SMI]",
		},
		{
			name:    "rejects logs missing success marker",
			logs:    "NVIDIA-SMI\nDriver Version: 570.86.15\nCUDA Version: 12.8\n",
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [" + gpuCheckSuccessMsg + "]",
		},
	}

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "aicr-validation", Name: "nvidia-smi-verify-test"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := verifyNvidiaSMILogs(tt.logs, pod)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyNvidiaSMILogs() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyNvidiaSMILogs() error = nil, want %q", tt.wantErr)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("verifyNvidiaSMILogs() error = %q, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseNvidiaSMIDriverVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		logs    string
		want    string
		wantErr bool
	}{
		{
			name: "legacy Driver Version field",
			logs: "NVIDIA-SMI\nDriver Version: 570.86.15\nCUDA Version: 12.8\n",
			want: "570.86.15",
		},
		{
			name: "renamed KMD Version field",
			logs: "NVIDIA-SMI\nKMD Version: 580.65.06\nCUDA UMD Version: 13.0\n",
			want: "580.65.06",
		},
		{
			name: "table layout with KMD Version",
			logs: "| NVIDIA-SMI 610.43.02              KMD Version: 610.43.02     CUDA UMD Version: 13.3     |\n",
			want: "610.43.02",
		},
		{
			name: "case-insensitive legacy field",
			logs: "DRIVER VERSION: 570.86.15\n",
			want: "570.86.15",
		},
		{
			name: "GKE A4X Max floor example",
			logs: "Driver Version: 580.95.05\n",
			want: "580.95.05",
		},
		{
			name:    "banner present but no numeric version",
			logs:    "Driver Version:\nCUDA Version: 12.8\n",
			wantErr: true,
		},
		{
			name:    "no driver banner at all",
			logs:    "NVIDIA-SMI\nCUDA Version: 12.8\n",
			wantErr: true,
		},
		{
			// Four numeric components must not truncate to a three-component
			// prefix that could falsely satisfy a floor (#1995 CodeRabbit).
			name:    "rejects more than three version components",
			logs:    "Driver Version: 580.95.05.1\n",
			wantErr: true,
		},
		{
			name:    "rejects nonnumeric suffix after version",
			logs:    "Driver Version: 580.95.05-rc1\n",
			wantErr: true,
		},
		{
			name:    "rejects version on the next line after the field",
			logs:    "Driver Version:\n580.95.05\n",
			wantErr: true,
		},
		{
			// \s+ would let "Driver\nVersion:" match as the field label.
			name:    "rejects newline between field words",
			logs:    "Driver\nVersion: 580.95.05\n",
			wantErr: true,
		},
		{
			name: "accepts table pipe immediately after version",
			logs: "| Driver Version: 580.95.05| CUDA Version: 12.8 |\n",
			want: "580.95.05",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseNvidiaSMIDriverVersion(tt.logs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseNvidiaSMIDriverVersion() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNvidiaSMIDriverVersion() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("parseNvidiaSMIDriverVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEnforceGPUDriverVersionFloor covers the #1995 host-driver floor: no
// constraint is a no-op; a present constraint fails closed when the banner is
// unreadable; pass/fail follow the numeric comparison.
func TestEnforceGPUDriverVersionFloor(t *testing.T) {
	t.Parallel()

	const goodLogs = "NVIDIA-SMI\nDriver Version: 580.95.05\nCUDA Version: 12.8\n" + gpuCheckSuccessMsg
	const lowLogs = "NVIDIA-SMI\nDriver Version: 570.86.15\nCUDA Version: 12.8\n" + gpuCheckSuccessMsg
	const noVersionLogs = "NVIDIA-SMI\nDriver Version:\nCUDA Version: 12.8\n" + gpuCheckSuccessMsg

	tests := []struct {
		name       string
		constraint string // empty = no constraint
		logs       string
		wantErrSub string // empty = want nil
	}{
		{
			name: "no constraint is a no-op even with parseable version",
			logs: goodLogs,
		},
		{
			name:       "no constraint is a no-op even when version is unreadable",
			logs:       noVersionLogs,
			constraint: "",
		},
		{
			name:       "satisfies floor",
			constraint: ">= 580.95.05",
			logs:       goodLogs,
		},
		{
			name:       "satisfies floor from KMD Version banner",
			constraint: ">= 580.95.05",
			logs:       "NVIDIA-SMI\nKMD Version: 580.95.05\nCUDA UMD Version: 13.0\n" + gpuCheckSuccessMsg,
		},
		{
			// Lexical string compare would fail here ('.100' < '.99'); the
			// constraint evaluator must compare components numerically.
			name:       "numeric order beats lexical order",
			constraint: ">= 580.99.99",
			logs:       "NVIDIA-SMI\nDriver Version: 580.100.0\nCUDA Version: 12.8\n" + gpuCheckSuccessMsg,
		},
		{
			name:       "below floor fails",
			constraint: ">= 580.95.05",
			logs:       lowLogs,
			wantErrSub: "does not satisfy",
		},
		{
			name:       "constraint present but unreadable banner fails closed",
			constraint: ">= 580.95.05",
			logs:       noVersionLogs,
			wantErrSub: "could not parse the host driver version",
		},
		{
			name:       "invalid constraint expression",
			constraint: ">=",
			logs:       goodLogs,
			wantErrSub: "invalid Deployment.gpu-driver.version constraint",
		},
		{
			// Parses as >= with a non-version value; Evaluate returns
			// ErrCodeInvalidRequest, which must not be recoded Internal.
			name:       "unparseable floor value preserves InvalidRequest",
			constraint: ">= not-a-version",
			logs:       goodLogs,
			wantErrSub: "[INVALID_REQUEST] cannot parse expected version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := &validators.Context{
				ValidationInput: &validatorv1.ValidationInput{
					Config: validatorv1.ValidationConfig{},
				},
			}
			if tt.constraint != "" {
				ctx.ValidationInput.Config.Deployment = &validatorv1.ValidationPhase{
					Constraints: []recipe.Constraint{{
						Name:  gpuDriverVersionConstraint,
						Value: tt.constraint,
					}},
				}
			}

			err := enforceGPUDriverVersionFloor(ctx, tt.logs, "gpu-node-1")
			if tt.wantErrSub == "" {
				if err != nil {
					t.Fatalf("enforceGPUDriverVersionFloor() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("enforceGPUDriverVersionFloor() = nil, want error containing %q", tt.wantErrSub)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("enforceGPUDriverVersionFloor() error = %v, want it to contain %q", err, tt.wantErrSub)
			}
		})
	}
}

// TestCheckNvidiaSMI_SkipScenarios covers the enumeration/disclosure paths
// that return before any pod is scheduled: no GPU nodes at all must give a
// different, more generic skip reason than every GPU node being cordoned
// (issue #1668 — a cordon narrows scope, it must never look identical to
// "nothing to check"), and non-GPU nodes must never count toward either the
// schedulable or cordoned tally.
func TestCheckNvidiaSMI_SkipScenarios(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		nodes       []runtime.Object
		wantErrSubs []string
	}{
		{
			name:        "no GPU nodes at all",
			nodes:       []runtime.Object{},
			wantErrSubs: []string{"no GPU nodes found in the cluster"},
		},
		{
			name: "all GPU nodes cordoned",
			nodes: []runtime.Object{
				cordon(gpuNode("cordoned-1", 8, -1)),
				cordon(gpuNode("cordoned-2", 8, -1)),
			},
			wantErrSubs: []string{"all 2 GPU node(s) are cordoned"},
		},
		{
			name: "cordoned GPU node plus unrelated non-GPU node",
			nodes: []runtime.Object{
				cordon(gpuNode("cordoned-1", 8, -1)),
				gpuNode("non-gpu-1", -1, -1),
			},
			wantErrSubs: []string{"all 1 GPU node(s) are cordoned"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := newDeploymentTestContext(t, tt.nodes, nil, nil)

			err := checkNvidiaSMI(ctx)
			if !validators.IsSkip(err) {
				t.Fatalf("checkNvidiaSMI() error = %v, want a skip", err)
			}
			for _, sub := range tt.wantErrSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("checkNvidiaSMI() error = %v, want it to contain %q", err, sub)
				}
			}
		})
	}
}

// TestGpuNodeCoverage_MixedCordonedAndSchedulable exercises the disclosure
// for a cluster with both a schedulable and a cordoned GPU node: the header
// counts both, the cordoned node is marked SKIPPED (cordoned) rather than
// omitted, and the RESULT line reports the passed count against the total.
// This is a pure function of the coverage buckets (no fake clientset or pod
// lifecycle needed).
func TestGpuNodeCoverage_MixedCordonedAndSchedulable(t *testing.T) {
	t.Parallel()

	allNodes := []helper.GpuNode{
		{Node: v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "schedulable-1"}}, Cordoned: false},
		{Node: v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cordoned-1"}}, Cordoned: true},
	}

	coverage, err := partitionGpuNodes(context.Background(), allNodes)
	if err != nil {
		t.Fatalf("partitionGpuNodes() error = %v", err)
	}

	wantEnumeration := []string{
		"Found 2 GPU node(s), 1 schedulable, 1 cordoned:",
		"  cordoned-1: SKIPPED (cordoned)",
	}
	gotEnumeration := coverage.enumerationLines()
	if len(gotEnumeration) != len(wantEnumeration) {
		t.Fatalf("enumerationLines() = %v, want %v", gotEnumeration, wantEnumeration)
	}
	for i, want := range wantEnumeration {
		if gotEnumeration[i] != want {
			t.Errorf("enumerationLines()[%d] = %q, want %q", i, gotEnumeration[i], want)
		}
	}

	// The exit paths that verify nothing report 0 validated even though a
	// schedulable node exists — validated tracks nodes that passed, not nodes
	// attempted.
	if got, want := coverage.coverageLine(),
		"RESULT: nodesValidated: 0/2 (passed 0, failed 0, skipped 1: busy 0, cordoned 1)"; got != want {
		t.Errorf("coverageLine() = %q, want %q", got, want)
	}
	coverage.passed = 1
	if got, want := coverage.coverageLine(),
		"RESULT: nodesValidated: 1/2 (passed 1, failed 0, skipped 1: busy 0, cordoned 1)"; got != want {
		t.Errorf("coverageLine() = %q, want %q", got, want)
	}
}

// TestGpuNodeCoverage_CoverageLine pins the RESULT line for each mix of
// outcomes, including the busy-probe error bucket that appears only when
// nonzero.
func TestGpuNodeCoverage_CoverageLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		coverage gpuNodeCoverage
		want     string
	}{
		{
			name:     "one passed one busy",
			coverage: gpuNodeCoverage{total: 2, passed: 1, busy: []string{"b"}},
			want:     "RESULT: nodesValidated: 1/2 (passed 1, failed 0, skipped 1: busy 1, cordoned 0)",
		},
		{
			name:     "all busy",
			coverage: gpuNodeCoverage{total: 2, busy: []string{"a", "b"}},
			want:     "RESULT: nodesValidated: 0/2 (passed 0, failed 0, skipped 2: busy 2, cordoned 0)",
		},
		{
			name: "every outcome",
			coverage: gpuNodeCoverage{total: 5, passed: 1, failed: 1,
				busy: []string{"b"}, probeErrors: []string{"e"}, cordoned: []string{"c"}},
			want: "RESULT: nodesValidated: 1/5 (passed 1, failed 1, skipped 3: busy 1, busy-probe error 1, cordoned 1)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.coverage.coverageLine(); got != tt.want {
				t.Errorf("coverageLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGpuNodeCoverage_EdgeCasePhrasing pins the zero-count phrasing: an empty
// cluster gets a plain "Found 0 GPU node(s)." and "nodesValidated: 0/0"
// instead of a header over an empty list, while a nonempty cluster always
// carries the full breakdown so every RESULT line has the same shape.
func TestGpuNodeCoverage_EdgeCasePhrasing(t *testing.T) {
	t.Parallel()

	t.Run("zero total nodes", func(t *testing.T) {
		t.Parallel()
		coverage, err := partitionGpuNodes(context.Background(), nil)
		if err != nil {
			t.Fatalf("partitionGpuNodes() error = %v", err)
		}
		if got, want := coverage.enumerationLines(), []string{"Found 0 GPU node(s)."}; len(got) != 1 || got[0] != want[0] {
			t.Errorf("enumerationLines() = %v, want %v", got, want)
		}
		if got, want := coverage.coverageLine(), "RESULT: nodesValidated: 0/0"; got != want {
			t.Errorf("coverageLine() = %q, want %q", got, want)
		}
	})

	t.Run("nodes present but none skipped", func(t *testing.T) {
		t.Parallel()
		coverage := gpuNodeCoverage{total: 1, passed: 1}
		if got, want := coverage.coverageLine(),
			"RESULT: nodesValidated: 1/1 (passed 1, failed 0, skipped 0: busy 0, cordoned 0)"; got != want {
			t.Errorf("coverageLine() = %q, want %q", got, want)
		}
	})
}

// TestPartitionGpuNodes_ContextCanceled proves the partition loop honors
// cancellation instead of silently finishing over an already-fetched slice.
func TestPartitionGpuNodes_ContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := partitionGpuNodes(ctx, []helper.GpuNode{
		{Node: v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}},
	})
	if !strings.Contains(err.Error(), "canceled while partitioning GPU nodes") {
		t.Errorf("partitionGpuNodes() error = %v, want cancellation error", err)
	}
}

// TestCheckNvidiaSMI_NoGpuNodesApplicability applies the #2122 applicability
// contract to the "no GPU nodes found" exit path. gpu-operator supplies the GPU
// nodes this check verifies, so when the resolved recipe DECLARES it a cluster
// with zero GPU nodes is a declared-but-absent prerequisite that must fail
// closed — a GPU-less cluster must not PASS conformance by masquerading absence
// as an inapplicable Skip. A recipe that omits gpu-operator, disables it, or
// carries no ComponentRefs (the standalone #1327 shape) keeps the genuine
// inapplicability Skip.
func TestCheckNvidiaSMI_NoGpuNodesApplicability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		refs     []recipe.ComponentRef
		wantSkip bool
		wantSub  string
	}{
		{
			name:     "gpu-operator declared, zero GPU nodes fails closed",
			refs:     []recipe.ComponentRef{{Name: "gpu-operator"}},
			wantSkip: false,
			wantSub:  "recipe declares gpu-operator but the cluster has no GPU nodes",
		},
		{
			name:     "no recipe context (standalone #1327) skips",
			refs:     nil,
			wantSkip: true,
			wantSub:  "no GPU nodes found in the cluster",
		},
		{
			name:     "unrelated component declared skips",
			refs:     []recipe.ComponentRef{{Name: "kai-scheduler"}},
			wantSkip: true,
			wantSub:  "no GPU nodes found in the cluster",
		},
		{
			name: "gpu-operator declared but disabled skips",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator", Overrides: map[string]any{"enabled": false}},
			},
			wantSkip: true,
			wantSub:  "no GPU nodes found in the cluster",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := newDeploymentTestContext(t, []runtime.Object{}, nil, tt.refs)
			err := checkNvidiaSMI(ctx)

			if got := validators.IsSkip(err); got != tt.wantSkip {
				t.Fatalf("checkNvidiaSMI() IsSkip = %v (err=%v), want %v", got, err, tt.wantSkip)
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("checkNvidiaSMI() error = %v, want it to contain %q", err, tt.wantSub)
			}
			// The declared-but-absent conversion must be a blocking NotFound, not
			// any other verdict that a report consumer might read as benign.
			if !tt.wantSkip && !strings.Contains(err.Error(), "[NOT_FOUND]") {
				t.Errorf("declared-but-absent must fail closed with NotFound, got %v", err)
			}
		})
	}
}

// TestCheckNvidiaSMI_CordonedKeepsSkipEvenWhenDeclared proves the #2122 KEEP
// decision for the all-cordoned path: the GPU-node prerequisite IS satisfied
// (GPU nodes exist), so declaring gpu-operator must NOT convert the
// scope-narrowing cordon Skip into a failure. Cordoning is an intentional
// operator action, not an absent prerequisite or an infra probe error.
func TestCheckNvidiaSMI_CordonedKeepsSkipEvenWhenDeclared(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t, []runtime.Object{
		cordon(gpuNode("cordoned-1", 8, -1)),
		cordon(gpuNode("cordoned-2", 8, -1)),
	}, nil, []recipe.ComponentRef{{Name: "gpu-operator"}})

	err := checkNvidiaSMI(ctx)
	if !validators.IsSkip(err) {
		t.Fatalf("checkNvidiaSMI() error = %v, want a skip (cordon narrows scope, never fails closed)", err)
	}
	if !strings.Contains(err.Error(), "all 2 GPU node(s) are cordoned") {
		t.Errorf("checkNvidiaSMI() error = %v, want the cordon skip reason", err)
	}
}

// TestCheckNvidiaSMI_DeclaredFloorUnmeasurableFailsClosed proves #1995: a
// declared Deployment.gpu-driver.version must not ride the non-blocking Skip
// paths. Busy workloads and all-cordoned nodes are supported Skip reasons
// only when no floor is configured.
func TestCheckNvidiaSMI_DeclaredFloorUnmeasurableFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		nodes       []runtime.Object
		busy        bool
		wantErrSubs []string
	}{
		{
			name:  "no GPU nodes with floor fails closed",
			nodes: []runtime.Object{},
			wantErrSubs: []string{
				"[NOT_FOUND]",
				gpuDriverVersionConstraint,
				"could not be measured",
				"no GPU nodes found in the cluster",
			},
		},
		{
			name: "all GPU nodes cordoned with floor fails closed",
			nodes: []runtime.Object{
				cordon(gpuNode("cordoned-1", 8, -1)),
				cordon(gpuNode("cordoned-2", 8, -1)),
			},
			wantErrSubs: []string{
				"[NOT_FOUND]",
				gpuDriverVersionConstraint,
				"could not be measured",
				"all 2 GPU node(s) are cordoned",
			},
		},
		{
			name: "busy GPU node with floor fails closed",
			nodes: []runtime.Object{
				gpuNode("gpu-1", 8, -1),
			},
			busy: true,
			wantErrSubs: []string{
				"[NOT_FOUND]",
				gpuDriverVersionConstraint,
				"could not be measured",
				"GPU nodes busy with existing workloads",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := newDeploymentTestContext(t, tt.nodes, nil, nil)
			withGPUDriverFloor(ctx, ">= 580.95.05")
			if tt.busy {
				ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods",
					gpuBusyPodsReactor())
			}

			err := checkNvidiaSMI(ctx)
			if validators.IsSkip(err) {
				t.Fatalf("checkNvidiaSMI() = skip (%v), want fail closed", err)
			}
			if err == nil {
				t.Fatal("checkNvidiaSMI() = nil, want a blocking error")
			}
			for _, sub := range tt.wantErrSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("checkNvidiaSMI() error = %v, want it to contain %q", err, sub)
				}
			}
		})
	}
}

// withGPUDriverFloor installs Deployment.gpu-driver.version on a test context.
func withGPUDriverFloor(ctx *validators.Context, expr string) {
	ctx.ValidationInput.Config.Deployment = &validatorv1.ValidationPhase{
		Constraints: []recipe.Constraint{{
			Name:  gpuDriverVersionConstraint,
			Value: expr,
		}},
	}
}

// gpuBusyPodsReactor makes IsNodeGpuBusy report confirmed occupancy: a
// running pod with a nvidia.com/gpu limit, regardless of field selector
// (the fake clientset does not honor spec.nodeName).
func gpuBusyPodsReactor() clienttesting.ReactionFunc {
	qty := resource.MustParse("1")
	return func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &v1.PodList{Items: []v1.Pod{{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu-workload", Namespace: "default"},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{
					Name: "work",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceName(helper.GpuResourceName): qty,
						},
					},
				}},
			},
			Status: v1.PodStatus{Phase: v1.PodRunning},
		}}}, nil
	}
}

// TestCheckNvidiaSMI_BusyWithoutFloorStillSkips preserves the pre-#1995 Skip
// when occupancy is confirmed and no host-driver floor is declared.
func TestCheckNvidiaSMI_BusyWithoutFloorStillSkips(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t, []runtime.Object{
		gpuNode("gpu-1", 8, -1),
	}, nil, nil)
	ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods", gpuBusyPodsReactor())

	err := checkNvidiaSMI(ctx)
	if !validators.IsSkip(err) {
		t.Fatalf("checkNvidiaSMI() error = %v, want a skip when no floor is set", err)
	}
	if !strings.Contains(err.Error(), "GPU nodes busy with existing workloads") {
		t.Errorf("checkNvidiaSMI() error = %v, want the busy skip reason", err)
	}
}

// TestCheckNvidiaSMI_BusyProbeAllErrorsFailClosed applies the #2122 contract to
// the busy-probe path: when the GPU busy-probe ERRORS on every schedulable node
// (nothing CONFIRMED busy), the probe proved no occupancy, so the check must
// fail closed and preserve the probe's error class rather than skip — an
// unreadable cluster (RBAC denial, timeout, transport failure) must not PASS
// conformance by masquerading as "nodes busy".
func TestCheckNvidiaSMI_BusyProbeAllErrorsFailClosed(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t, []runtime.Object{
		gpuNode("gpu-1", 8, -1),
	}, nil, nil)

	// Force the busy-probe's pod List to fail on every node. helper.IsNodeGpuBusy
	// returns (busy=true, err); the loop records it in probeErrors, never as
	// CONFIRMED occupancy, so with no free node the all-errors branch fires.
	ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: "pods"}, "", nil)
		})

	err := checkNvidiaSMI(ctx)
	if validators.IsSkip(err) {
		t.Fatalf("checkNvidiaSMI() = skip, want fail closed on an all-probe-error busy check")
	}
	if err == nil {
		t.Fatal("checkNvidiaSMI() = nil, want a blocking error")
	}
	// Error class preserved: helper.IsNodeGpuBusy wraps the List failure as
	// [INTERNAL] and PropagateOrWrap keeps that code; the probe's own message
	// survives so operators see the underlying cause.
	if !strings.Contains(err.Error(), "[INTERNAL]") {
		t.Errorf("checkNvidiaSMI() error = %v, want the [INTERNAL] class preserved", err)
	}
	if !strings.Contains(err.Error(), "failed to list pods") {
		t.Errorf("checkNvidiaSMI() error = %v, want the underlying probe cause", err)
	}
}

// gpuBusyOnNodesReactor makes IsNodeGpuBusy report confirmed occupancy only on
// the named nodes, by reading the probe's spec.nodeName field selector (which
// the fake clientset does not apply itself).
func gpuBusyOnNodesReactor(busy ...string) clienttesting.ReactionFunc {
	busyPods := gpuBusyPodsReactor()
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		list, ok := action.(clienttesting.ListAction)
		if !ok {
			return false, nil, nil
		}
		node, _ := list.GetListRestrictions().Fields.RequiresExactMatch("spec.nodeName")
		for _, b := range busy {
			if node == b {
				return busyPods(action)
			}
		}
		return true, &v1.PodList{}, nil
	}
}

// TestRunNvidiaSMICheck_PartiallyBusy covers #3361: a busy GPU node is skipped
// per node rather than skipping the whole check, so every free schedulable
// node is still verified and decides the outcome.
func TestRunNvidiaSMICheck_PartiallyBusy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		nodes        []runtime.Object
		busy         []string
		failOn       string
		floor        string
		wantVerified []string
		wantSkip     bool
		wantErrSubs  []string
	}{
		{
			name:         "one free one busy passes on the free node",
			nodes:        []runtime.Object{gpuNode("free-1", 8, -1), gpuNode("busy-1", 8, -1)},
			busy:         []string{"busy-1"},
			wantVerified: []string{"free-1"},
		},
		{
			name: "free, busy, and cordoned nodes pass on the free node",
			nodes: []runtime.Object{
				gpuNode("free-1", 8, -1), gpuNode("busy-1", 8, -1), cordon(gpuNode("cordoned-1", 8, -1)),
			},
			busy:         []string{"busy-1"},
			wantVerified: []string{"free-1"},
		},
		{
			name:         "a failing free node fails the check",
			nodes:        []runtime.Object{gpuNode("free-1", 8, -1), gpuNode("free-2", 8, -1), gpuNode("busy-1", 8, -1)},
			busy:         []string{"busy-1"},
			failOn:       "free-2",
			wantVerified: []string{"free-1", "free-2"},
			wantErrSubs:  []string{"GPU verification failed on 1/2 verified nodes", "free-2"},
		},
		{
			name:         "every free node failing fails the check",
			nodes:        []runtime.Object{gpuNode("free-1", 8, -1), gpuNode("busy-1", 8, -1)},
			busy:         []string{"busy-1"},
			failOn:       "free-1",
			wantVerified: []string{"free-1"},
			wantErrSubs:  []string{"GPU verification failed on 1/1 verified nodes", "free-1"},
		},
		{
			name:         "declared floor does not fail closed when a free node is verified",
			nodes:        []runtime.Object{gpuNode("free-1", 8, -1), gpuNode("busy-1", 8, -1)},
			busy:         []string{"busy-1"},
			floor:        ">= 580.95.05",
			wantVerified: []string{"free-1"},
		},
		{
			name:        "all nodes busy keeps the whole-check skip",
			nodes:       []runtime.Object{gpuNode("busy-1", 8, -1), gpuNode("busy-2", 8, -1)},
			busy:        []string{"busy-1", "busy-2"},
			wantSkip:    true,
			wantErrSubs: []string{"GPU nodes busy with existing workloads", "busy-1", "busy-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := newDeploymentTestContext(t, tt.nodes, nil, nil)
			if tt.floor != "" {
				withGPUDriverFloor(ctx, tt.floor)
			}
			ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods",
				gpuBusyOnNodesReactor(tt.busy...))

			var verified []string
			err := runNvidiaSMICheck(ctx, func(nodeName string) error {
				verified = append(verified, nodeName)
				if nodeName == tt.failOn {
					return errors.New(errors.ErrCodeInternal, "nvidia-smi exited 1")
				}
				return nil
			})

			if got := validators.IsSkip(err); got != tt.wantSkip {
				t.Fatalf("runNvidiaSMICheck() IsSkip = %v (err=%v), want %v", got, err, tt.wantSkip)
			}
			if len(tt.wantErrSubs) == 0 && err != nil {
				t.Fatalf("runNvidiaSMICheck() error = %v, want pass", err)
			}
			for _, sub := range tt.wantErrSubs {
				if err == nil || !strings.Contains(err.Error(), sub) {
					t.Errorf("runNvidiaSMICheck() error = %v, want it to contain %q", err, sub)
				}
			}
			if strings.Join(verified, ",") != strings.Join(tt.wantVerified, ",") {
				t.Errorf("verified nodes = %v, want %v (busy and cordoned nodes must not be verified)",
					verified, tt.wantVerified)
			}
		})
	}
}

// TestRunNvidiaSMICheck_ProbeErrorWithFreeNodeSkipsOnlyThatNode proves a
// busy-probe error narrows scope per node once another node is free: the free
// node is verified and decides the outcome, and the all-probe-error fail-closed
// path (#2122) is reserved for a cluster where no node could be probed.
func TestRunNvidiaSMICheck_ProbeErrorWithFreeNodeSkipsOnlyThatNode(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t, []runtime.Object{
		gpuNode("free-1", 8, -1), gpuNode("unreadable-1", 8, -1),
	}, nil, nil)
	ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			list, ok := action.(clienttesting.ListAction)
			if !ok {
				return false, nil, nil
			}
			if node, _ := list.GetListRestrictions().Fields.RequiresExactMatch("spec.nodeName"); node == "unreadable-1" {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
			}
			return true, &v1.PodList{}, nil
		})

	var verified []string
	err := runNvidiaSMICheck(ctx, func(nodeName string) error {
		verified = append(verified, nodeName)
		return nil
	})
	if err != nil {
		t.Fatalf("runNvidiaSMICheck() error = %v, want pass on the free node", err)
	}
	if strings.Join(verified, ",") != "free-1" {
		t.Errorf("verified nodes = %v, want [free-1]", verified)
	}
}

// TestRunNvidiaSMICheck_BusyAndProbeErrorNoFreeNodeSkips proves that with no
// free node, one CONFIRMED busy node keeps the nodes-busy Skip even when the
// probe errored on the other, and the reason names both.
func TestRunNvidiaSMICheck_BusyAndProbeErrorNoFreeNodeSkips(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t, []runtime.Object{
		gpuNode("busy-1", 8, -1), gpuNode("unreadable-1", 8, -1),
	}, nil, nil)
	busy := gpuBusyPodsReactor()
	ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			list, ok := action.(clienttesting.ListAction)
			if !ok {
				return false, nil, nil
			}
			if node, _ := list.GetListRestrictions().Fields.RequiresExactMatch("spec.nodeName"); node == "unreadable-1" {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
			}
			return busy(action)
		})

	err := runNvidiaSMICheck(ctx, func(nodeName string) error {
		t.Errorf("verifyNode(%s) called, want no node verified", nodeName)
		return nil
	})
	if !validators.IsSkip(err) {
		t.Fatalf("runNvidiaSMICheck() error = %v, want a nodes-busy skip", err)
	}
	for _, sub := range []string{"GPU nodes busy with existing workloads", "busy-1", "unreadable-1"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("runNvidiaSMICheck() error = %v, want it to contain %q", err, sub)
		}
	}
}

// TestRunNvidiaSMICheck_CoverageExtra proves the structured counts reach the
// CTRF extra on every exit once GPU nodes are listed — partly busy, all busy,
// all cordoned, and the #2122 all-probe-error fail-closed path — where minimal
// redaction strips the RESULT stdout line.
// Not parallel: captureExtras swaps os.Stdout.
func TestRunNvidiaSMICheck_CoverageExtra(t *testing.T) {
	forbidden := func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
	}
	freeAndBusy := []runtime.Object{gpuNode("free-1", 8, -1), gpuNode("busy-1", 8, -1)}

	tests := []struct {
		name     string
		nodes    []runtime.Object
		reactor  clienttesting.ReactionFunc
		wantSkip bool
		wantErr  bool
		want     map[string]string
	}{
		{
			name:    "one free one busy",
			nodes:   freeAndBusy,
			reactor: gpuBusyOnNodesReactor("busy-1"),
			want:    map[string]string{"nodesValidated": "1", "nodesTotal": "2", "nodesFailed": "0", "nodesSkipped": "1"},
		},
		{
			name:     "every node busy",
			nodes:    []runtime.Object{gpuNode("busy-1", 8, -1), gpuNode("busy-2", 8, -1)},
			reactor:  gpuBusyOnNodesReactor("busy-1", "busy-2"),
			wantSkip: true,
			want:     map[string]string{"nodesValidated": "0", "nodesTotal": "2", "nodesFailed": "0", "nodesSkipped": "2"},
		},
		{
			name:     "no GPU nodes",
			reactor:  gpuBusyOnNodesReactor(),
			wantSkip: true,
			want:     map[string]string{"nodesValidated": "0", "nodesTotal": "0", "nodesFailed": "0", "nodesSkipped": "0", "skipReason": skipReasonNoGPUNodes},
		},
		{
			name:     "every node cordoned",
			nodes:    []runtime.Object{cordon(gpuNode("cordoned-1", 8, -1)), cordon(gpuNode("cordoned-2", 8, -1))},
			reactor:  gpuBusyOnNodesReactor(),
			wantSkip: true,
			want:     map[string]string{"nodesValidated": "0", "nodesTotal": "2", "nodesFailed": "0", "nodesSkipped": "2"},
		},
		{
			name:    "busy-probe errors on every node",
			nodes:   freeAndBusy,
			reactor: forbidden,
			wantErr: true,
			want:    map[string]string{"nodesValidated": "0", "nodesTotal": "2", "nodesFailed": "0", "nodesSkipped": "2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := newDeploymentTestContext(t, tt.nodes, nil, nil)
			ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods", tt.reactor)

			extra, err := captureExtras(t, func() error {
				return runNvidiaSMICheck(ctx, func(string) error { return nil })
			})
			if got := validators.IsSkip(err); got != tt.wantSkip {
				t.Fatalf("runNvidiaSMICheck() IsSkip = %v (err=%v), want %v", got, err, tt.wantSkip)
			}
			if !tt.wantSkip && (err != nil) != tt.wantErr {
				t.Fatalf("runNvidiaSMICheck() error = %v, wantErr %v", err, tt.wantErr)
			}
			assertExtraSubset(t, extra, tt.want)
		})
	}
}

// TestRunNvidiaSMICheck_PerNodeLines pins each node's outcome line in stdout,
// the only place node names appear.
// Not parallel: captureStdout swaps os.Stdout.
func TestRunNvidiaSMICheck_PerNodeLines(t *testing.T) {
	ctx := newDeploymentTestContext(t, []runtime.Object{
		gpuNode("busy-1", 8, -1), gpuNode("free-1", 8, -1), gpuNode("bad-1", 8, -1), gpuNode("unreadable-1", 8, -1),
	}, nil, nil)
	busy := gpuBusyOnNodesReactor("busy-1")
	ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			list, ok := action.(clienttesting.ListAction)
			if !ok {
				return false, nil, nil
			}
			if node, _ := list.GetListRestrictions().Fields.RequiresExactMatch("spec.nodeName"); node == "unreadable-1" {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
			}
			return busy(action)
		})

	out, err := captureStdout(t, func() error {
		return runNvidiaSMICheck(ctx, func(nodeName string) error {
			if nodeName == "bad-1" {
				return errors.New(errors.ErrCodeInternal, "nvidia-smi exited 1")
			}
			return nil
		})
	})
	if err == nil {
		t.Fatal("runNvidiaSMICheck() = nil, want the bad-1 failure")
	}
	for _, want := range []string{
		"  busy-1: SKIPPED (busy)",
		"  free-1: PASSED",
		"  bad-1: FAILED (",
		"  unreadable-1: SKIPPED (busy-probe error)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// TestRunNvidiaSMICheck_BusyBeforeItsTurnIsSkipped proves a node is probed
// immediately before it is verified: a job that claims a node's GPUs while an
// earlier node is verified makes it SKIPPED (busy), not FAILED.
func TestRunNvidiaSMICheck_BusyBeforeItsTurnIsSkipped(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t, []runtime.Object{
		gpuNode("gpu-1", 8, -1), gpuNode("gpu-2", 8, -1),
	}, nil, nil)
	var verified []string
	busyAfterFirst := gpuBusyOnNodesReactor("gpu-2")
	free := gpuBusyOnNodesReactor()
	ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			if len(verified) > 0 {
				return busyAfterFirst(action)
			}
			return free(action)
		})

	err := runNvidiaSMICheck(ctx, func(nodeName string) error {
		verified = append(verified, nodeName)
		return nil
	})
	if err != nil {
		t.Fatalf("runNvidiaSMICheck() error = %v, want pass on gpu-1 with gpu-2 skipped", err)
	}
	if strings.Join(verified, ",") != "gpu-1" {
		t.Errorf("verified nodes = %v, want [gpu-1] (gpu-2 became busy before its turn)", verified)
	}
}

// TestRunNvidiaSMICheck_DeadContextStopsWithoutCountingFailed proves an
// expired deadline stops the check with a timeout-class error and leaves the
// interrupted and unreached nodes out of nodesFailed.
// Not parallel: captureExtras swaps os.Stdout.
func TestRunNvidiaSMICheck_DeadContextStopsWithoutCountingFailed(t *testing.T) {
	ctx := newDeploymentTestContext(t, []runtime.Object{
		gpuNode("gpu-1", 8, -1), gpuNode("gpu-2", 8, -1), gpuNode("gpu-3", 8, -1),
	}, nil, nil)
	ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "pods", gpuBusyOnNodesReactor())
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx.Ctx = runCtx

	var verified []string
	extra, err := captureExtras(t, func() error {
		return runNvidiaSMICheck(ctx, func(nodeName string) error {
			verified = append(verified, nodeName)
			if nodeName == "gpu-2" {
				cancel()
				return errors.Wrap(errors.ErrCodeInternal, "pod wait interrupted", runCtx.Err())
			}
			return nil
		})
	})

	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if strings.Join(verified, ",") != "gpu-1,gpu-2" {
		t.Errorf("verified nodes = %v, want [gpu-1 gpu-2] (gpu-3 never reached)", verified)
	}
	assertExtraSubset(t, extra, map[string]string{
		"nodesValidated": "1", "nodesTotal": "3", "nodesFailed": "0", "nodesSkipped": "0",
	})
}
