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
	stderrors "errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// gpuNode builds a schedulable node. gpu >= 0 advertises that many allocatable
// nvidia.com/gpu (gpu < 0 omits the resource → a non-GPU node); rdma >= 0 also
// advertises that many rdma/hca_shared_devices_a (rdma < 0 omits it). The node
// carries NO Mellanox NIC label by default — use withIB to mark it Mellanox RDMA-capable.
func gpuNode(name string, gpu, rdma int64) *corev1.Node {
	alloc := corev1.ResourceList{}
	if gpu >= 0 {
		alloc[corev1.ResourceName(helper.GpuResourceName)] = *resource.NewQuantity(gpu, resource.DecimalSI)
	}
	if rdma >= 0 {
		alloc[corev1.ResourceName(helper.AKSRdmaSharedResource)] = *resource.NewQuantity(rdma, resource.DecimalSI)
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Allocatable: alloc},
	}
}

// withMellanoxNIC marks a node Mellanox RDMA-capable (the NicClusterPolicy nodeAffinity label the
// fabric is placed on), so the RDMA fabric gate includes it in the cohort.
func withMellanoxNIC(n *corev1.Node) *corev1.Node {
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels[helper.PCIMellanoxPresentLabel] = "true"
	return n
}

// cordon marks a node unschedulable (draining), which excludes it from the
// schedulable GPU cohort the gate spans.
func cordon(n *corev1.Node) *corev1.Node {
	n.Spec.Unschedulable = true
	return n
}

// rdmaGPUNode is the common case: a schedulable, Mellanox RDMA-capable GPU node with `gpu`
// allocatable GPUs and `rdma` allocatable shared-fabric units (rdma < 0 omits).
func rdmaGPUNode(name string, gpu, rdma int64) *corev1.Node {
	return withMellanoxNIC(gpuNode(name, gpu, rdma))
}

// TestRecipeDeclaresRDMAFabric proves the gate engages only when the
// network-operator ComponentRef declares the AKS NicClusterPolicy manifest, and
// discriminates it from near-miss / OCP / non-fabric manifests.
func TestRecipeDeclaresRDMAFabric(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		manifestFiles []string
		want          bool
	}{
		{
			name:          "AKS nic-cluster-policy manifest present",
			manifestFiles: []string{"components/network-operator/manifests/nic-cluster-policy-aks.yaml"},
			want:          true,
		},
		{
			name: "marker present among several manifests",
			manifestFiles: []string{
				"components/network-operator/manifests/nfd-network-rule.yaml",
				"components/network-operator/manifests/nic-cluster-policy-aks.yaml",
				"components/network-operator/manifests/nvidia-peermem-reloader.yaml",
			},
			want: true,
		},
		{
			name:          "OCP nicclusterpolicy (no hyphens) does not match",
			manifestFiles: []string{"components/network-operator-ocp/manifests/nicclusterpolicy.yaml"},
			want:          false,
		},
		{
			name:          "near-miss cluster-policy without nic- prefix does not match",
			manifestFiles: []string{"components/somewhere/manifests/cluster-policy.yaml"},
			want:          false,
		},
		{
			name:          "talos namespace-only ref (no fabric)",
			manifestFiles: []string{"components/network-operator/manifests/talos-namespace.yaml"},
			want:          false,
		},
		{
			name:          "no manifests",
			manifestFiles: nil,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref := recipe.ComponentRef{Name: networkOperatorComponent, ManifestFiles: tt.manifestFiles}
			if got := recipeDeclaresRDMAFabric(ref); got != tt.want {
				t.Fatalf("recipeDeclaresRDMAFabric(%v) = %v, want %v", tt.manifestFiles, got, tt.want)
			}
		})
	}
}

// TestVerifyRDMAFabricReady_Poll drives the fabric readiness poll through a
// scripted per-node sequence (each entry is the rdma/hca_shared_devices_a
// allocatable count on gpu-node-1 for one List; -1 => absent, 0 => present-but-
// zero; the last entry repeats). Node gpu-node-0 always has the fabric.
func TestVerifyRDMAFabricReady_Poll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		node1RDMA  []int64
		wantErrSub string // "" => expect success
		minLists   int
	}{
		{
			name:      "rides through transient partial rollout",
			node1RDMA: []int64{-1, -1, 1000},
			minLists:  3,
		},
		{
			name:       "times out while one node's fabric is absent (names the node)",
			node1RDMA:  []int64{-1},
			wantErrSub: "not yet allocatable on 1 of 2 RDMA GPU node(s): [gpu-node-1]",
		},
		{
			name:       "times out while one node advertises the fabric as zero (present-but-zero)",
			node1RDMA:  []int64{0},
			wantErrSub: "not yet allocatable on 1 of 2 RDMA GPU node(s): [gpu-node-1]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			seq := tt.node1RDMA
			clientset := k8sfake.NewClientset()
			var lists atomic.Int32
			clientset.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
				idx := int(lists.Add(1)) - 1
				if idx >= len(seq) {
					idx = len(seq) - 1
				}
				return true, &corev1.NodeList{Items: []corev1.Node{
					// gpu-node-1 is a degraded 7-GPU node (issue #1858) — proving
					// the fabric gate spans it regardless of GPU count.
					*rdmaGPUNode("gpu-node-0", 8, 1000),
					*rdmaGPUNode("gpu-node-1", 7, seq[idx]),
				}}, nil
			})
			ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

			err := verifyRDMAFabricReady(ctx, helper.AKSRdmaSharedResource)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("verifyRDMAFabricReady() error = nil, want error containing %q", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("verifyRDMAFabricReady() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyRDMAFabricReady() error = %v, want nil (should ride through the transient partial rollout)", err)
			}
			if got := int(lists.Load()); got < tt.minLists {
				t.Fatalf("expected the poll to list nodes at least %d times (through the partial rollout), got %d", tt.minLists, got)
			}
		})
	}
}

// TestVerifyRDMAFabricReady_EagerDisclosureFloor locks the every-terminal-outcome
// coverage contract against the mid-poll SIGKILL race: the catalog timeout feeds
// both the Job's activeDeadlineSeconds and this poll's budget with no margin, so
// a never-ready poll can be killed at the deadline before the terminal emit runs.
// The eager floor must have already emitted the structured coverage — including
// the cordoned node that narrowed the cohort — on the first observation, with
// validated=0 (nothing is certified mid-poll). parseExtraSentinels keeps the last
// valid sentinel, so on a clean exit the terminal emit overwrites the floor.
func TestVerifyRDMAFabricReady_EagerDisclosureFloor(t *testing.T) {
	t.Parallel()

	type emitCall struct{ validated, total int }

	tests := []struct {
		name          string
		schedRDMA     int64 // allocatable fabric on the schedulable node (-1 => absent)
		wantErr       bool
		wantTerminalV int // validated on the terminal (settled) emit
	}{
		{
			// The schedulable node's fabric never appears → the gate never
			// certifies and times out. The eager floor must still have disclosed
			// the 2-node total (incl. the cordoned node) so a deadline kill leaves
			// the coverage in the logs instead of nothing.
			name:          "never ready fails closed but floor disclosed the cordoned total",
			schedRDMA:     -1,
			wantErr:       true,
			wantTerminalV: 0,
		},
		{
			// Fabric present+uniform → the gate certifies. The eager floor emits
			// validated=0 first; the terminal emit reflects the certified cohort.
			name:          "ready certifies after floor",
			schedRDMA:     1000,
			wantErr:       false,
			wantTerminalV: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clientset := k8sfake.NewClientset(
				rdmaGPUNode("rdma-gpu-0", 8, tt.schedRDMA), // schedulable RDMA node
				cordon(rdmaGPUNode("rdma-drain-0", 8, -1)), // cordoned RDMA node → disclosed, not dropped
			)
			ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

			// pollUntilStable runs the probe synchronously in this goroutine, so
			// the injected emit is never called concurrently — no lock needed.
			var calls []emitCall
			err := verifyRDMAFabricReadyEmit(ctx, helper.AKSRdmaSharedResource, func(validated, total int) {
				calls = append(calls, emitCall{validated, total})
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("verifyRDMAFabricReadyEmit() error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(calls) < 2 {
				t.Fatalf("expected an eager floor emit AND a terminal emit, got %d: %+v", len(calls), calls)
			}
			// The FIRST emit is the eager floor: validated=0 (nothing certified
			// mid-poll) and total=2 (the schedulable node + the disclosed cordoned
			// node). This is the sentinel a deadline SIGKILL would leave behind.
			if want := (emitCall{validated: 0, total: 2}); calls[0] != want {
				t.Errorf("eager floor emit = %+v, want %+v (cordoned node disclosed before the terminal emit)", calls[0], want)
			}
			// The terminal emit reflects the settled outcome.
			if want := (emitCall{validated: tt.wantTerminalV, total: 2}); calls[len(calls)-1] != want {
				t.Errorf("terminal emit = %+v, want %+v", calls[len(calls)-1], want)
			}
		})
	}
}

// TestRDMAFabricProbe_FailsClosedOnListError proves a node-list failure is
// surfaced (not read as "fabric ready"), so the poll fails closed and retries.
func TestRDMAFabricProbe_FailsClosedOnListError(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewClientset()
	clientset.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("apiserver unavailable")
	})
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	cov, err := rdmaFabricProbeCoverage(ctx, helper.AKSRdmaSharedResource)
	if err == nil {
		t.Fatal("expected an error when listing nodes fails, got nil (must fail closed)")
	}
	if cov.schedulable != 0 {
		t.Fatalf("expected 0 nodes on list error, got %d", cov.schedulable)
	}
	// FindGpuNodes' error path now flows through errors.PropagateOrWrap: a plain
	// List failure carries no code, so it is wrapped ErrCodeInternal (a coded
	// inner error — e.g. ErrCodeTimeout from a canceled node scan — would instead
	// propagate unchanged). Assert the propagated code, not the removed
	// gate-context message, so the fail-closed contract is pinned to the code.
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("expected ErrCodeInternal on plain list failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "failed to list nodes") {
		t.Fatalf("expected the underlying list-failure context, got %v", err)
	}
}

// TestRDMAFabricProbe_FailsClosedWithoutRDMANodes proves the probe fails closed
// (does not vacuously pass) when no schedulable RDMA GPU node is observed — a GPU
// node without the Mellanox NIC label is not the fabric cohort, and "no cohort" must never
// read as "fabric ready".
func TestRDMAFabricProbe_FailsClosedWithoutRDMANodes(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewClientset(
		gpuNode("gpu-node-0", 8, -1), // GPU node but NOT Mellanox-NIC-labeled → not in cohort
		gpuNode("cpu-node-0", -1, -1),
	)
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	cov, err := rdmaFabricProbeCoverage(ctx, helper.AKSRdmaSharedResource)
	if err == nil {
		t.Fatal("expected an error when no RDMA GPU nodes are present, got nil (must fail closed)")
	}
	if cov.schedulable != 0 {
		t.Fatalf("expected 0 cohort nodes, got %d", cov.schedulable)
	}
	if !strings.Contains(err.Error(), "no schedulable Mellanox RDMA-capable GPU nodes observed yet") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRDMAFabricProbe_ExcludesCordonedNonRDMAAndCPU proves the gate scopes to
// schedulable RDMA GPU nodes only (issue #1862 crux): a cordoned RDMA node, a non-RDMA
// GPU node, a zero-GPU node, and a CPU node are all excluded — so a node the NCCL
// check would never run on cannot wedge the gate. Only the one healthy RDMA GPU
// node is required to (and does) carry the fabric.
func TestRDMAFabricProbe_ExcludesCordonedNonRDMAAndCPU(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewClientset(
		rdmaGPUNode("rdma-gpu-0", 8, 1000),           // in cohort, fabric ready
		gpuNode("nordma-gpu-0", 8, -1),               // GPU but no Mellanox NIC label → excluded
		cordon(rdmaGPUNode("rdma-gpu-drain", 8, -1)), // Mellanox NIC but cordoned → excluded
		rdmaGPUNode("rdma-zerogpu", 0, -1),           // Mellanox NIC but nvidia.com/gpu:0 → excluded by helper
		gpuNode("cpu-0", -1, -1),                     // CPU node → excluded
	)
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	cov, err := rdmaFabricProbeCoverage(ctx, helper.AKSRdmaSharedResource)
	if err != nil {
		t.Fatalf("rdmaFabricProbeCoverage() error = %v, want nil (only the schedulable RDMA GPU node is required to carry the fabric)", err)
	}
	if cov.schedulable != 1 {
		t.Fatalf("rdmaFabricProbeCoverage() cohort size = %d, want 1 (cordoned/non-RDMA/zero-GPU/CPU excluded)", cov.schedulable)
	}
}

// TestRDMAFabricProbe_NonUniformCountFails proves the gate matches the NCCL
// consumer's uniformFabricResourceCount: two RDMA nodes both advertising the
// fabric but with different counts (1000 vs 500) is "still settling", not ready.
func TestRDMAFabricProbe_NonUniformCountFails(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewClientset(
		rdmaGPUNode("rdma-gpu-0", 8, 1000),
		rdmaGPUNode("rdma-gpu-1", 8, 500),
	)
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	cov, err := rdmaFabricProbeCoverage(ctx, helper.AKSRdmaSharedResource)
	if err == nil {
		t.Fatal("expected an error on non-uniform fabric counts, got nil")
	}
	if cov.schedulable != 2 {
		t.Fatalf("expected cohort size 2, got %d", cov.schedulable)
	}
	if !strings.Contains(err.Error(), "non-uniform") || !strings.Contains(err.Error(), "rdma-gpu-1=500") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRDMAFabricProbe_PassesWhenUniform proves the probe returns the cohort size
// and no error once every RDMA GPU node advertises the same positive fabric count.
func TestRDMAFabricProbe_PassesWhenUniform(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewClientset(
		rdmaGPUNode("rdma-gpu-0", 8, 1000),
		rdmaGPUNode("rdma-gpu-1", 7, 1000), // degraded 7-GPU node still carries the fabric
	)
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	cov, err := rdmaFabricProbeCoverage(ctx, helper.AKSRdmaSharedResource)
	if err != nil {
		t.Fatalf("rdmaFabricProbeCoverage() error = %v, want nil (fabric uniform on all RDMA GPU nodes)", err)
	}
	if cov.schedulable != 2 {
		t.Fatalf("rdmaFabricProbeCoverage() cohort size = %d, want 2", cov.schedulable)
	}
}

// TestVerifyGPUReadinessSignals_RDMADispatch proves the gate is wired to fire
// only when the recipe enables network-operator AND declares the nic-cluster-
// policy manifest — the dispatch conjunction, not just the two predicates in
// isolation. (a) fabric recipe + a fabric-missing RDMA node → a fabric failure is
// returned; (b) network-operator without the fabric manifest → the gate never
// runs even though the same node lacks the fabric.
func TestVerifyGPUReadinessSignals_RDMADispatch(t *testing.T) {
	t.Parallel()

	newCtx := func() *validators.Context {
		clientset := k8sfake.NewClientset(rdmaGPUNode("rdma-gpu-0", 8, -1)) // fabric absent
		return &validators.Context{Ctx: context.Background(), Clientset: clientset}
	}
	const fabricErrFragment = "not yet allocatable"

	t.Run("fires on network-operator + nic-cluster-policy manifest", func(t *testing.T) {
		t.Parallel()
		refs := []recipe.ComponentRef{{
			Name:          networkOperatorComponent,
			ManifestFiles: []string{"components/network-operator/manifests/nic-cluster-policy-aks.yaml"},
		}}
		failures, _ := verifyGPUReadinessSignals(newCtx(), refs)
		if !containsSubstr(failures, fabricErrFragment) {
			t.Fatalf("expected a fabric failure in %v", failures)
		}
	})

	t.Run("does not fire without the fabric manifest", func(t *testing.T) {
		t.Parallel()
		refs := []recipe.ComponentRef{{
			Name:          networkOperatorComponent,
			ManifestFiles: []string{"components/network-operator/manifests/talos-namespace.yaml"},
		}}
		failures, _ := verifyGPUReadinessSignals(newCtx(), refs)
		if containsSubstr(failures, fabricErrFragment) {
			t.Fatalf("fabric gate must not run without the nic-cluster-policy manifest; got %v", failures)
		}
	})
}

func containsSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
