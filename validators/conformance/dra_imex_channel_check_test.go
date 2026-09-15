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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// withCliqueLabel stamps the MNNVL clique label on a test node.
func withCliqueLabel() nodeOpt {
	return func(n *corev1.Node) {
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[labelNVIDIAGPUClique] = "0f8b6e2a-clique.1"
	}
}

// computeDomainSlice builds a usable compute-domain.nvidia.com ResourceSlice
// on the named node at the given served version.
func computeDomainSlice(version, node string) *unstructured.Unstructured {
	return testResourceSliceAt(apiGroupResourceK8sIO+"/"+version, "cd-"+node, draDriverComputeDomain, node, 1, 1,
		map[string]any{"nodeName": node},
		[]any{plainDevice("channel-0"), plainDevice("daemon-0")})
}

// reconcileComputeDomainsOnCreate mimics the NVIDIA DRA driver controller:
// whenever a ComputeDomain is created, the ResourceClaimTemplate named in
// spec.channel.resourceClaimTemplate.name appears in the same namespace at
// the served version. Returns a pointer to the created ComputeDomains for
// spec assertions.
func reconcileComputeDomainsOnCreate(t *testing.T, dyn *dynamicfake.FakeDynamicClient, version string) *[]*unstructured.Unstructured {
	t.Helper()
	created := &[]*unstructured.Unstructured{}
	dyn.PrependReactor("create", "computedomains", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		cd, ok := createAction.GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		*created = append(*created, cd.DeepCopy())
		rctName, _, _ := unstructured.NestedString(cd.Object, "spec", "channel", "resourceClaimTemplate", "name")
		rct := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": apiGroupResourceK8sIO + "/" + version,
			"kind":       "ResourceClaimTemplate",
			"metadata":   map[string]any{"name": rctName, "namespace": cd.GetNamespace()},
		}}
		if err := dyn.Tracker().Create(draGVRAt(version, "resourceclaimtemplates"), rct, cd.GetNamespace()); err != nil {
			t.Errorf("fake driver: create ResourceClaimTemplate: %v", err)
		}
		return false, nil, nil
	})
	return created
}

// withGeneratedClaimStatus installs a pod-create reactor (in addition to the
// terminal-state reactor) that stamps status.resourceClaimStatuses on the
// IMEX probe pod, naming generatedClaim for the pod-local imexPodClaimName
// claim — the kubelet's post-generation state. A decoy entry with a different
// pod-local name comes FIRST so a "take the first entry" bug is caught.
func withGeneratedClaimStatus(client *k8sfake.Clientset, generatedClaim string) {
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := createAction.GetObject().(*corev1.Pod)
		if !ok || !strings.HasPrefix(pod.Name, imexTestPodPrefix) {
			return false, nil, nil
		}
		decoy := "decoy-claim"
		pod.Status.ResourceClaimStatuses = []corev1.PodResourceClaimStatus{
			{Name: "other", ResourceClaimName: &decoy},
			{Name: imexPodClaimName, ResourceClaimName: &generatedClaim},
		}
		return false, nil, nil
	})
}

// imexTestContext wires a healthy driver, the given nodes, and a fake dynamic
// client at version with the given objects plus the fake driver reconciler.
func imexTestContext(t *testing.T, version string, nodes []runtime.Object, objects ...runtime.Object) (*validators.Context, *k8sfake.Clientset, *dynamicfake.FakeDynamicClient, *[]*unstructured.Unstructured) {
	t.Helper()
	client := k8sfake.NewClientset(append(healthyDRADriverObjects(), nodes...)...)
	withDRAAPIDiscoveryAt(t, client, apiGroupResourceK8sIO+"/"+version)
	dyn := newDRAFakeDynamicClientAt(version, objects...)
	createdCDs := reconcileComputeDomainsOnCreate(t, dyn, version)
	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     client,
		DynamicClient: dyn,
	}
	return ctx, client, dyn, createdCDs
}

func affinityNodeNames(t *testing.T, pod *corev1.Pod) []string {
	t.Helper()
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {

		t.Fatalf("pod %s has no required node affinity", pod.Name)
	}
	var names []string
	for _, term := range pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, req := range term.MatchFields {
			if req.Key != metav1.ObjectNameField || req.Operator != corev1.NodeSelectorOpIn {
				t.Errorf("unexpected affinity requirement %+v, want matchFields metadata.name In", req)
			}
			names = append(names, req.Values...)
		}
	}
	slices.Sort(names)
	return names
}

// TestCheckDRASupport_IMEXSubtestRunsOnCliqueNodes drives the behavioral
// IMEX subtest end to end against fakes at every served resource.k8s.io
// version: a clique-labeled node with a usable compute-domain slice causes
// the check to create a ComputeDomain, wait for the (fake-reconciled)
// ResourceClaimTemplate, and run a probe pod that consumes it — while the
// full-GPU subtest stays N/A (no gpu.nvidia.com DeviceClass).
func TestCheckDRASupport_IMEXSubtestRunsOnCliqueNodes(t *testing.T) {
	for _, version := range []string{"v1", "v1beta2", versionV1beta1} {
		t.Run(version, func(t *testing.T) {
			ctx, client, _, createdCDs := imexTestContext(t, version,
				[]runtime.Object{testNode("node1", withCliqueLabel())},
				computeDomainSlice(version, "node1"))
			createdPods := markPodsSucceededOnCreate(client)

			var err error
			out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
			if err != nil {
				t.Fatalf("CheckDRASupport() error = %v, want pass", err)
			}

			if len(*createdCDs) != 1 {
				t.Fatalf("ComputeDomains created = %d, want 1", len(*createdCDs))
			}
			cd := (*createdCDs)[0]
			if got := cd.GetAPIVersion(); got != computeDomainAPIGroup+"/"+versionV1beta1 {
				t.Errorf("ComputeDomain apiVersion = %q, want %s/%s", got, computeDomainAPIGroup, versionV1beta1)
			}
			numNodes, _, _ := unstructured.NestedInt64(cd.Object, "spec", "numNodes")
			mode, _, _ := unstructured.NestedString(cd.Object, "spec", "channel", "allocationMode")
			if numNodes != 0 || mode != computeDomainAllocationModeSingle {
				t.Errorf("ComputeDomain spec numNodes=%d allocationMode=%q, want 0/%s", numNodes, mode, computeDomainAllocationModeSingle)
			}
			rctName, _, _ := unstructured.NestedString(cd.Object, "spec", "channel", "resourceClaimTemplate", "name")

			pod := findPodByPrefix(*createdPods, imexTestPodPrefix)
			if pod == nil {
				t.Fatal("IMEX probe pod was not created")
			}
			if pod.Namespace != cd.GetNamespace() {
				t.Errorf("pod namespace %q != ComputeDomain namespace %q", pod.Namespace, cd.GetNamespace())
			}
			if len(pod.Spec.ResourceClaims) != 1 || pod.Spec.ResourceClaims[0].Name != imexPodClaimName ||
				pod.Spec.ResourceClaims[0].ResourceClaimTemplateName == nil ||
				*pod.Spec.ResourceClaims[0].ResourceClaimTemplateName != rctName {

				t.Errorf("pod resourceClaims = %+v, want one %q entry referencing template %q", pod.Spec.ResourceClaims, imexPodClaimName, rctName)
			}
			if len(pod.Spec.Containers) != 1 || len(pod.Spec.Containers[0].Resources.Claims) != 1 ||
				pod.Spec.Containers[0].Resources.Claims[0].Name != imexPodClaimName {

				t.Errorf("probe container must reference the pod-local claim %q: %+v", imexPodClaimName, pod.Spec.Containers)
			}
			if _, hasGPU := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; hasGPU {
				t.Error("IMEX probe must not request a GPU")
			}
			if got := affinityNodeNames(t, pod); !slices.Equal(got, []string{"node1"}) {
				t.Errorf("affinity nodes = %v, want [node1]", got)
			}
			if findPodByPrefix(*createdPods, gpuTestPodPrefix) != nil {
				t.Error("full-GPU allocation pod created, want N/A (no gpu.nvidia.com DeviceClass)")
			}
			for _, want := range []string{
				"--- IMEX candidate nodes ---",
				"--- Created IMEX test resources ---",
				"--- IMEX pod status ---",
				"--- IMEX pod logs (" + containerNameIMEXTest + ") ---",
				"--- Generated ResourceClaim status ---",
				"--- Delete IMEX test namespace ---",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("evidence missing %q", want)
				}
			}
		})
	}
}

// TestCheckDRASupport_IMEXNotApplicableWithoutCliqueLabel: compute-domain
// slices on nodes WITHOUT the clique label (the observed H100 shape) keep
// today's verdict — pass, subtest recorded not applicable, no probe pod.
func TestCheckDRASupport_IMEXNotApplicableWithoutCliqueLabel(t *testing.T) {
	ctx, client, _, createdCDs := imexTestContext(t, "v1",
		[]runtime.Object{testNode("node1")},
		computeDomainSlice("v1", "node1"))
	createdPods := markPodsSucceededOnCreate(client)

	var err error
	out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
	if err != nil {
		t.Fatalf("CheckDRASupport() error = %v, want pass", err)
	}
	if len(*createdCDs) != 0 || len(*createdPods) != 0 {
		t.Errorf("created ComputeDomains=%d pods=%d, want none", len(*createdCDs), len(*createdPods))
	}
	if !strings.Contains(out, "--- "+artifactIMEXSubtest+" ---\nskipped (not applicable): no Ready, schedulable node carries the "+labelNVIDIAGPUClique) {
		t.Errorf("evidence missing the IMEX not-applicable record:\n%s", out)
	}
}

// TestCheckDRASupport_IMEXFailsWhenCliqueNodesLackUsableComputeDomainSlice:
// MNNVL nodes exist but the ComputeDomain driver serves none of them — a
// structural failure, not N/A. Covers both "no compute-domain slices at all
// (only gpu.nvidia.com)" and "compute-domain slices only on non-clique nodes".
func TestCheckDRASupport_IMEXFailsWhenCliqueNodesLackUsableComputeDomainSlice(t *testing.T) {
	tests := []struct {
		name    string
		nodes   []runtime.Object
		objects []runtime.Object
	}{
		{
			name:  "clique node, only full-GPU slices",
			nodes: []runtime.Object{testNode("node1", withCliqueLabel())},
			objects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("gpu-1", draDriverGPU, "node1", 1, 1,
					map[string]any{"nodeName": "node1"}, []any{plainDevice("gpu-0")}),
			},
		},
		{
			name:    "clique node, compute-domain slice only on a non-clique node",
			nodes:   []runtime.Object{testNode("node1", withCliqueLabel()), testNode("node2")},
			objects: []runtime.Object{computeDomainSlice("v1", "node2")},
		},
		{
			name:  "clique node, compute-domain slice on it is tainted",
			nodes: []runtime.Object{testNode("node1", withCliqueLabel()), testNode("node2")},
			objects: []runtime.Object{
				computeDomainSlice("v1", "node2"),
				testResourceSlice("cd-node1", draDriverComputeDomain, "node1", 1, 1,
					map[string]any{"nodeName": "node1"},
					[]any{taintedDevice("channel-0", string(corev1.TaintEffectNoSchedule))}),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, client, _, createdCDs := imexTestContext(t, "v1", tt.nodes, tt.objects...)
			createdPods := markPodsSucceededOnCreate(client)

			err := CheckDRASupport(ctx)
			if err == nil {
				t.Fatal("expected failure: clique-labeled node without a usable compute-domain slice")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
				t.Errorf("error = %v, want ErrCodeInternal", err)
			}
			if !strings.Contains(err.Error(), "[node1] carry the "+labelNVIDIAGPUClique) ||
				!strings.Contains(err.Error(), draDriverComputeDomain) {

				t.Errorf("error = %v, want the clique-node/compute-domain mismatch message", err)
			}
			if len(*createdCDs) != 0 || len(*createdPods) != 0 {
				t.Errorf("created ComputeDomains=%d pods=%d, want none before the gate", len(*createdCDs), len(*createdPods))
			}
		})
	}
}

// TestCheckDRASupport_IMEXAffinityIsCandidateIntersection: only nodes that
// BOTH carry the clique label AND have a usable compute-domain slice appear
// in the probe pod's required node affinity.
func TestCheckDRASupport_IMEXAffinityIsCandidateIntersection(t *testing.T) {
	ctx, client, _, _ := imexTestContext(t, "v1",
		[]runtime.Object{
			testNode("node1", withCliqueLabel()),                      // clique + slice → candidate
			testNode("node2", withCliqueLabel()),                      // clique, no slice
			testNode("node3"),                                         // slice, no clique
			testNode("node4", withCliqueLabel(), withUnschedulable()), // clique + slice, cordoned
		},
		computeDomainSlice("v1", "node1"),
		computeDomainSlice("v1", "node3"),
		computeDomainSlice("v1", "node4"))
	createdPods := markPodsSucceededOnCreate(client)

	var err error
	out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
	if err != nil {
		t.Fatalf("CheckDRASupport() error = %v, want pass", err)
	}
	pod := findPodByPrefix(*createdPods, imexTestPodPrefix)
	if pod == nil {
		t.Fatal("IMEX probe pod was not created")
	}
	if got := affinityNodeNames(t, pod); !slices.Equal(got, []string{"node1"}) {
		t.Errorf("affinity nodes = %v, want [node1] (intersection only)", got)
	}
	if !strings.Contains(out, "Clique-labeled nodes:         node1,node2\nWith usable compute-domain:   node1") {
		t.Errorf("candidate-node evidence missing or wrong:\n%s", out)
	}
}

// TestCheckDRASupport_IMEXNotApplicableStillRunsFullGPUSubtest: without a
// clique label the IMEX subtest is N/A, and the existing full-GPU behavioral
// subtest still runs when gpu.nvidia.com DRA is usable.
func TestCheckDRASupport_IMEXNotApplicableStillRunsFullGPUSubtest(t *testing.T) {
	ctx, client, _, createdCDs := imexTestContext(t, "v1",
		[]runtime.Object{testNode("node1")},
		testDeviceClass(draDriverGPU),
		testResourceSlice("gpu-1", draDriverGPU, "node1", 1, 1,
			map[string]any{"nodeName": "node1"}, []any{plainDevice("gpu-0")}))
	createdPods := markPodsSucceededOnCreate(client)

	if err := CheckDRASupport(ctx); err != nil {
		t.Fatalf("CheckDRASupport() error = %v, want pass", err)
	}
	if len(*createdCDs) != 0 || findPodByPrefix(*createdPods, imexTestPodPrefix) != nil {
		t.Error("IMEX resources created without a clique-labeled node, want N/A")
	}
	if findPodByPrefix(*createdPods, gpuTestPodPrefix) == nil {
		t.Error("full-GPU allocation pod was not created despite full-GPU DRA being usable")
	}
}

// TestCheckDRASupport_IMEXTemplateNeverGenerated: a driver that never
// reconciles the ComputeDomain fails the check with ErrCodeTimeout, and no
// probe pod is created (the kubelet would reject a pod referencing a missing
// template anyway).
func TestCheckDRASupport_IMEXTemplateNeverGenerated(t *testing.T) {
	saved := imexClaimTemplateTimeout
	imexClaimTemplateTimeout = 300 * time.Millisecond
	t.Cleanup(func() { imexClaimTemplateTimeout = saved })

	client := k8sfake.NewClientset(append(healthyDRADriverObjects(), testNode("node1", withCliqueLabel()))...)
	withDRAAPIDiscovery(t, client)
	createdPods := markPodsSucceededOnCreate(client)
	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     client,
		DynamicClient: newDRAFakeDynamicClient(computeDomainSlice("v1", "node1")), // no reconciler
	}

	var err error
	out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
	if err == nil {
		t.Fatal("expected a timeout waiting for the ResourceClaimTemplate")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error = %v, want ErrCodeTimeout", err)
	}
	if !strings.Contains(err.Error(), "did not reconcile ComputeDomain") {
		t.Errorf("error = %v, want the reconcile-timeout message", err)
	}
	if findPodByPrefix(*createdPods, imexTestPodPrefix) != nil {
		t.Error("probe pod created despite the template never appearing")
	}
	if !strings.Contains(out, "--- Delete IMEX test namespace ---") {
		t.Error("cleanup evidence missing after the template timeout")
	}
}

// TestCheckDRASupport_IMEXPodFailureStillRecordsEvidence: a probe pod that
// exits non-zero (e.g. zero or two channel devices) fails the check, and the
// pod status, logs, and generated-claim evidence are recorded BEFORE the
// verdict.
func TestCheckDRASupport_IMEXPodFailureStillRecordsEvidence(t *testing.T) {
	ctx, client, _, _ := imexTestContext(t, "v1",
		[]runtime.Object{testNode("node1", withCliqueLabel())},
		computeDomainSlice("v1", "node1"))
	markPodsTerminalOnCreate(client, func(string) (corev1.PodPhase, int32) {
		return corev1.PodFailed, 1
	})

	var err error
	out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
	if err == nil {
		t.Fatal("expected the IMEX probe failure to fail the check")
	}
	if !strings.Contains(err.Error(), "IMEX channel test pod phase=Failed") {
		t.Errorf("error = %v, want the pod-phase failure", err)
	}
	for _, want := range []string{
		"--- IMEX pod status ---",
		"--- IMEX pod logs (" + containerNameIMEXTest + ") ---",
		"--- Generated ResourceClaim status ---",
		"--- Delete IMEX test namespace ---",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("emitted evidence missing %q despite the failure", want)
		}
	}
	if got := podLogFetchCount(client); got < 1 {
		t.Error("failing IMEX probe pod must still record its log artifact")
	}
}

// TestCheckDRASupport_IMEXGeneratedClaimEvidenceIsBestEffort pins the
// evidence-only semantics of the post-terminal claim read: the verdict is
// PodSucceeded plus the script's assertion, so a generated claim that the
// resource-claim controller already deleted, one whose name never surfaced in
// pod.status.resourceClaimStatuses, and one still present all PASS — and the
// evidence says which state was observed. The status entry is matched by
// pod-local claim name (a decoy entry comes first).
func TestCheckDRASupport_IMEXGeneratedClaimEvidenceIsBestEffort(t *testing.T) {
	const generated = "imex-alloc-test-generated-abc12"
	tests := []struct {
		name         string
		statusClaim  string // "" → no resourceClaimStatuses stamped
		claimPresent bool
		wantEvidence []string
	}{
		{
			name:         "generated claim already deleted",
			statusClaim:  generated,
			wantEvidence: []string{"post-terminal claim state unavailable", "already deleted"},
		},
		{
			name:         "no generated claim name in pod status",
			wantEvidence: []string{"post-terminal claim state unavailable", "carries no generated claim name"},
		},
		{
			name:         "generated claim still present",
			statusClaim:  generated,
			claimPresent: true,
			wantEvidence: []string{"Name:        ", "/" + generated, "Allocated:   true", "ReservedFor: 1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, client, dyn, createdCDs := imexTestContext(t, "v1",
				[]runtime.Object{testNode("node1", withCliqueLabel())},
				computeDomainSlice("v1", "node1"))
			markPodsSucceededOnCreate(client)
			if tt.statusClaim != "" {
				withGeneratedClaimStatus(client, tt.statusClaim)
			}
			if tt.claimPresent {
				// The claim lives in the per-run namespace, which is only
				// known once the ComputeDomain is created — seed it from
				// the reconciler's observation.
				dyn.PrependReactor("create", "computedomains", func(action k8stesting.Action) (bool, runtime.Object, error) {
					ns := action.GetNamespace()
					claim := &unstructured.Unstructured{Object: map[string]any{
						"apiVersion": draAPIGroupVersion,
						"kind":       "ResourceClaim",
						"metadata":   map[string]any{"name": generated, "namespace": ns},
						"status": map[string]any{
							"allocation":  map[string]any{"devices": map[string]any{}},
							"reservedFor": []any{map[string]any{"resource": "pods", "name": "p", "uid": "u"}},
						},
					}}
					if err := dyn.Tracker().Create(draGVRAt("v1", "resourceclaims"), claim, ns); err != nil {
						t.Errorf("seed generated claim: %v", err)
					}
					return false, nil, nil
				})
			}

			var err error
			out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
			if err != nil {
				t.Fatalf("CheckDRASupport() error = %v, want pass (claim evidence must not gate the verdict)", err)
			}
			if len(*createdCDs) != 1 {
				t.Fatalf("ComputeDomains created = %d, want 1", len(*createdCDs))
			}
			for _, want := range tt.wantEvidence {
				if !strings.Contains(out, want) {
					t.Errorf("evidence missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "decoy-claim") {
				t.Error("generated-claim lookup used the decoy (first) resourceClaimStatuses entry instead of matching by pod-local name")
			}
		})
	}
}

// TestIMEXCandidateNodes pins the gate's set algebra.
func TestIMEXCandidateNodes(t *testing.T) {
	eligible := map[string]*corev1.Node{
		"a": testNode("a", withCliqueLabel()),
		"b": testNode("b", withCliqueLabel()),
		"c": testNode("c"),
	}
	tests := []struct {
		name           string
		cdNodes        map[string]struct{}
		wantCandidates []string
		wantClique     []string
	}{
		{"intersection", map[string]struct{}{"a": {}, "c": {}}, []string{"a"}, []string{"a", "b"}},
		{"no compute-domain nodes", nil, nil, []string{"a", "b"}},
		{"all clique nodes covered", map[string]struct{}{"a": {}, "b": {}}, []string{"a", "b"}, []string{"a", "b"}},
		{"no eligible nodes at all", map[string]struct{}{"a": {}}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			el := eligible
			if tt.name == "no eligible nodes at all" {
				el = map[string]*corev1.Node{}
			}
			candidates, clique := imexCandidateNodes(el, tt.cdNodes)
			if !slices.Equal(candidates, tt.wantCandidates) {
				t.Errorf("candidates = %v, want %v", candidates, tt.wantCandidates)
			}
			if !slices.Equal(clique, tt.wantClique) {
				t.Errorf("cliqueNodes = %v, want %v", clique, tt.wantClique)
			}
		})
	}
}

// allocatedComputeDomainClaim builds a ResourceClaim whose allocation holds
// the compute-domain.nvidia.com channel device of the given pool — the shape
// a standing ComputeDomain (e.g. Slinky Slurm's slinky-slurm-imex-channels
// template) or a running MNNVL workload leaves on a node.
func allocatedComputeDomainClaim(version, namespace, name, pool string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiGroupResourceK8sIO + "/" + version,
		"kind":       "ResourceClaim",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"status": map[string]any{
			"allocation": map[string]any{
				"devices": map[string]any{
					"results": []any{map[string]any{
						"request": "channel", "driver": draDriverComputeDomain, "pool": pool, "device": "channel-0",
					}},
				},
			},
			"reservedFor": []any{map[string]any{"resource": "pods", "name": "slurmd-0", "uid": "u1"}},
		},
	}}
}

// TestCheckDRASupport_IMEXSkipsNodesWithAllocatedChannel: a node whose
// compute-domain channel is already held by another claim is excluded from
// the probe's affinity; when every candidate is occupied the subtest is
// recorded not applicable, names the holders, and creates nothing.
func TestCheckDRASupport_IMEXSkipsNodesWithAllocatedChannel(t *testing.T) {
	t.Run("one of two candidates occupied → probe pinned to the free node", func(t *testing.T) {
		ctx, client, _, createdCDs := imexTestContext(t, "v1",
			[]runtime.Object{testNode("node1", withCliqueLabel()), testNode("node2", withCliqueLabel())},
			computeDomainSlice("v1", "node1"), computeDomainSlice("v1", "node2"),
			allocatedComputeDomainClaim("v1", "slurm", "slinky-slurm-imex-channels-abc", "node1"))
		createdPods := markPodsSucceededOnCreate(client)

		var err error
		out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
		if err != nil {
			t.Fatalf("CheckDRASupport() error = %v, want pass", err)
		}
		if len(*createdCDs) != 1 {
			t.Fatalf("ComputeDomains created = %d, want 1", len(*createdCDs))
		}
		pod := findPodByPrefix(*createdPods, imexTestPodPrefix)
		if pod == nil {
			t.Fatal("IMEX probe pod was not created")
		}
		if got := affinityNodeNames(t, pod); !slices.Equal(got, []string{"node2"}) {
			t.Errorf("affinity nodes = %v, want [node2] (node1's channel is held)", got)
		}
		if !strings.Contains(out, "Channel already allocated:    node1 held by slurm/slinky-slurm-imex-channels-abc") {
			t.Errorf("occupancy evidence missing:\n%s", out)
		}
	})
	t.Run("every candidate occupied → not applicable, nothing created", func(t *testing.T) {
		ctx, client, _, createdCDs := imexTestContext(t, "v1",
			[]runtime.Object{testNode("node1", withCliqueLabel())},
			computeDomainSlice("v1", "node1"),
			allocatedComputeDomainClaim("v1", "slurm", "slinky-slurm-imex-channels-abc", "node1"))
		createdPods := markPodsSucceededOnCreate(client)

		var err error
		out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
		if err != nil {
			t.Fatalf("CheckDRASupport() error = %v, want pass", err)
		}
		if len(*createdCDs) != 0 || len(*createdPods) != 0 {
			t.Errorf("created ComputeDomains=%d pods=%d, want none", len(*createdCDs), len(*createdPods))
		}
		if !strings.Contains(out, "skipped (not applicable): every MNNVL candidate node already holds a "+draDriverComputeDomain+" channel claim (node1 held by slurm/slinky-slurm-imex-channels-abc)") {
			t.Errorf("evidence missing the occupied not-applicable record:\n%s", out)
		}
	})
	t.Run("unallocated claim is not an occupant", func(t *testing.T) {
		pending := allocatedComputeDomainClaim("v1", "slurm", "pending-claim", "node1")
		unstructured.RemoveNestedField(pending.Object, "status", "allocation")
		ctx, client, _, createdCDs := imexTestContext(t, "v1",
			[]runtime.Object{testNode("node1", withCliqueLabel())},
			computeDomainSlice("v1", "node1"), pending)
		markPodsSucceededOnCreate(client)
		if err := CheckDRASupport(ctx); err != nil {
			t.Fatalf("CheckDRASupport() error = %v, want pass", err)
		}
		if len(*createdCDs) != 1 {
			t.Errorf("ComputeDomains created = %d, want 1 (pending claim must not block the probe)", len(*createdCDs))
		}
	})
}

// closeOnceWatch is a watch.Interface whose result channel is already closed
// — the apiserver/LB "accepts the watch and drops it" hiccup.
type closeOnceWatch struct{ ch chan watch.Event }

func (w *closeOnceWatch) Stop()                          {}
func (w *closeOnceWatch) ResultChan() <-chan watch.Event { return w.ch }

func newClosedWatch() *closeOnceWatch {
	w := &closeOnceWatch{ch: make(chan watch.Event)}
	close(w.ch)
	return w
}

// TestWaitForIMEXClaimTemplate_HiccupPaths pins the restart loop: a watch
// channel that closes without cancellation, a transient Watch setup error,
// and a transient Get error are all retried (with backoff) until the template
// appears, while a permanent Get error fails immediately.
func TestWaitForIMEXClaimTemplate_HiccupPaths(t *testing.T) {
	saved := imexClaimTemplateTimeout
	imexClaimTemplateTimeout = 5 * time.Second
	t.Cleanup(func() { imexClaimTemplateTimeout = saved })
	rctGR := schema.GroupResource{Group: apiGroupResourceK8sIO, Resource: "resourceclaimtemplates"}
	rct := func(run *gpuTestRun) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": draAPIGroupVersion, "kind": "ResourceClaimTemplate",
			"metadata": map[string]any{"name": run.claimTemplateName, "namespace": run.namespace},
		}}
	}

	tests := []struct {
		name       string
		install    func(dyn *dynamicfake.FakeDynamicClient, run *gpuTestRun)
		wantErr    bool
		wantTarget error
		minCalls   int
	}{
		{
			name: "watch closes immediately twice, template appears on the third pass",
			install: func(dyn *dynamicfake.FakeDynamicClient, run *gpuTestRun) {
				closes := 0
				dyn.PrependWatchReactor("resourceclaimtemplates", func(k8stesting.Action) (bool, watch.Interface, error) {
					closes++
					if closes == 2 {
						// Reconciled during the second closure window.
						_ = dyn.Tracker().Create(draGVRAt("v1", "resourceclaimtemplates"), rct(run), run.namespace)
					}
					return true, newClosedWatch(), nil
				})
			},
			minCalls: 2,
		},
		{
			name: "transient watch setup error is retried",
			install: func(dyn *dynamicfake.FakeDynamicClient, run *gpuTestRun) {
				calls := 0
				dyn.PrependWatchReactor("resourceclaimtemplates", func(k8stesting.Action) (bool, watch.Interface, error) {
					calls++
					if calls == 1 {
						return true, nil, k8serrors.NewTooManyRequests("throttled", 1)
					}
					_ = dyn.Tracker().Create(draGVRAt("v1", "resourceclaimtemplates"), rct(run), run.namespace)
					return true, newClosedWatch(), nil
				})
			},
			minCalls: 2,
		},
		{
			name: "transient get error is retried",
			install: func(dyn *dynamicfake.FakeDynamicClient, run *gpuTestRun) {
				gets := 0
				dyn.PrependReactor("get", "resourceclaimtemplates", func(k8stesting.Action) (bool, runtime.Object, error) {
					gets++
					if gets == 1 {
						return true, nil, k8serrors.NewServiceUnavailable("apiserver restarting")
					}
					if gets == 2 {
						_ = dyn.Tracker().Create(draGVRAt("v1", "resourceclaimtemplates"), rct(run), run.namespace)
					}
					return false, nil, nil
				})
			},
			minCalls: 2,
		},
		{
			name: "permanent get error fails immediately",
			install: func(dyn *dynamicfake.FakeDynamicClient, _ *gpuTestRun) {
				dyn.PrependReactor("get", "resourceclaimtemplates", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, k8serrors.NewForbidden(rctGR, "x", stderrors.New("rbac"))
				})
			},
			wantErr:    true,
			wantTarget: errors.New(errors.ErrCodeInternal, ""),
		},
		{
			name: "permanent watch setup error fails immediately",
			install: func(dyn *dynamicfake.FakeDynamicClient, _ *gpuTestRun) {
				dyn.PrependWatchReactor("resourceclaimtemplates", func(k8stesting.Action) (bool, watch.Interface, error) {
					return true, nil, k8serrors.NewForbidden(rctGR, "x", stderrors.New("rbac"))
				})
			},
			wantErr:    true,
			wantTarget: errors.New(errors.ErrCodeInternal, ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, err := newGPUTestRun()
			if err != nil {
				t.Fatal(err)
			}
			dyn := newDRAFakeDynamicClient()
			tt.install(dyn, run)
			start := time.Now()
			err = waitForIMEXClaimTemplate(context.Background(), dyn, "v1", run)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !stderrors.Is(err, tt.wantTarget) {
					t.Errorf("error = %v, want code of %v", err, tt.wantTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("waitForIMEXClaimTemplate() error = %v, want nil after retry", err)
			}
			// At least one 250ms backoff must have elapsed for every retried
			// pass — the loop must not hot-spin.
			if elapsed := time.Since(start); elapsed < 250*time.Millisecond*time.Duration(tt.minCalls-1) {
				t.Errorf("elapsed %s, want >= %s of backoff", elapsed, 250*time.Millisecond*time.Duration(tt.minCalls-1))
			}
		})
	}
}

// TestCheckDRASupport_IMEXStuckPodStillRecordsEvidence: a probe pod that
// never reaches a terminal phase (ImagePullBackOff) fails the check through
// the wait error, and its status and logs are still recorded.
func TestCheckDRASupport_IMEXStuckPodStillRecordsEvidence(t *testing.T) {
	ctx, client, _, _ := imexTestContext(t, "v1",
		[]runtime.Object{testNode("node1", withCliqueLabel())},
		computeDomainSlice("v1", "node1"))
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: containerNameIMEXTest, Image: pod.Spec.Containers[0].Image,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull denied"}},
		}}
		return false, nil, nil
	})

	var err error
	out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
	if err == nil {
		t.Fatal("expected the stuck probe to fail the check")
	}
	if !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Errorf("error = %v, want the stuck reason", err)
	}
	for _, want := range []string{
		"--- IMEX pod status ---",
		"Stuck:     ImagePullBackOff",
		"--- IMEX pod logs (" + containerNameIMEXTest + ") ---",
		"--- Generated ResourceClaim status ---",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("evidence missing %q despite the stuck pod:\n%s", want, out)
		}
	}
}
