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

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestNvidiaSMICoverageExtra(t *testing.T) {
	tests := []struct {
		name          string
		validated     int
		total         int
		wantValidated string
		wantTotal     string
	}{
		{"full coverage", 2, 2, "2", "2"},
		{"reduced coverage one cordoned", 1, 2, "1", "2"},
		{"single node", 1, 1, "1", "1"},
		{"partial failure", 1, 2, "1", "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extra := nvidiaSMICoverageExtra(tt.validated, tt.total)
			if extra["nodesValidated"] != tt.wantValidated {
				t.Errorf("nodesValidated = %q, want %q", extra["nodesValidated"], tt.wantValidated)
			}
			if got := extra["nodesTotal"]; got != tt.wantTotal {
				t.Errorf("nodesTotal = %q, want %q", got, tt.wantTotal)
			}
			if len(extra) != 2 {
				t.Errorf("coverage extra must carry exactly the two count keys, got %v", extra)
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

// TestGpuNodeCoverage_MixedCordonedAndSchedulable exercises the headline
// behavior of this change directly: a cluster with both a schedulable and a
// cordoned GPU node must list the schedulable node plainly, mark the
// cordoned node "skipped (cordoned)" rather than omitting it, and report a
// nodesValidated coverage line reflecting the actual validated count. This is
// a pure function of the partition (no fake clientset or pod lifecycle
// needed), factored out of checkNvidiaSMI specifically so this scenario is
// testable without simulating a full pod-watch/log round trip.
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
		"  schedulable-1",
		"  cordoned-1: skipped (cordoned)",
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

	if got, want := coverage.coverageLine(1), "RESULT: nodesValidated: 1/2 (1 cordoned, skipped)"; got != want {
		t.Errorf("coverageLine(1) = %q, want %q", got, want)
	}
	// The busy/all-cordoned/failed-before-completion exit paths report 0
	// validated even though a schedulable node exists — validated tracks
	// nodes actually confirmed, not nodes attempted.
	if got, want := coverage.coverageLine(0), "RESULT: nodesValidated: 0/2 (1 cordoned, skipped)"; got != want {
		t.Errorf("coverageLine(0) = %q, want %q", got, want)
	}
}

// TestGpuNodeCoverage_EdgeCasePhrasing proves the two zero-count phrasing
// nits: an empty cluster gets a plain "Found 0 GPU node(s)." instead of a
// header with a trailing colon over an empty list, and a coverage line with
// no cordoned nodes omits the "(0 cordoned, skipped)" parenthetical instead
// of stating a count of zero as if it were informative.
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
		if got, want := coverage.coverageLine(0), "RESULT: nodesValidated: 0/0"; got != want {
			t.Errorf("coverageLine(0) = %q, want %q", got, want)
		}
	})

	t.Run("nodes present but none cordoned", func(t *testing.T) {
		t.Parallel()
		coverage, err := partitionGpuNodes(context.Background(), []helper.GpuNode{
			{Node: v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "schedulable-1"}}, Cordoned: false},
		})
		if err != nil {
			t.Fatalf("partitionGpuNodes() error = %v", err)
		}
		if got, want := coverage.coverageLine(1), "RESULT: nodesValidated: 1/1"; got != want {
			t.Errorf("coverageLine(1) = %q, want %q", got, want)
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
	// returns (busy=true, err); the loop records it as a probe error, never as
	// CONFIRMED occupancy, so confirmedBusy stays false and the all-errors branch
	// fires.
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
