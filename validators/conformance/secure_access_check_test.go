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
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// markPodsSucceededOnCreate installs a reactor that marks every created pod
// as Succeeded — with a terminated exit-0 status for each of its containers —
// and pins it to node1, so pod-wait loops and container-level isolation
// verification complete immediately against the fake clientset. It returns a
// pointer to the slice of created pods for spec assertions.
func markPodsSucceededOnCreate(client *k8sfake.Clientset) *[]*corev1.Pod {
	created := &[]*corev1.Pod{}
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := createAction.GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		// Snapshot the pod AS SUBMITTED (before the fake-scheduler mutations
		// below) so tests can assert the created spec — e.g. that scheduling
		// is expressed via node affinity rather than a spec.nodeName pin.
		*created = append(*created, pod.DeepCopy())
		pod.Status.Phase = corev1.PodSucceeded
		for _, c := range pod.Spec.Containers {
			pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
				Name: c.Name,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
				},
			})
		}
		if pod.Spec.NodeName == "" {
			pod.Spec.NodeName = "node1"
		}
		// handled=false: fall through to the default reactor, which stores
		// the (mutated) pod in the tracker.
		return false, nil, nil
	})
	return created
}

func findPodByPrefix(pods []*corev1.Pod, prefix string) *corev1.Pod {
	for _, p := range pods {
		if strings.HasPrefix(p.Name, prefix) {
			return p
		}
	}
	return nil
}

// TestValidateDRAPatterns_ClaimReadErrorClassification pins the error-code
// mapping for ResourceClaim read failures in step 5 of validateDRAPatterns:
// only a true NotFound is ErrCodeNotFound; context cancellation/deadline and
// apiserver Timeout/ServerTimeout map to ErrCodeTimeout; RBAC and other
// transient errors stay ErrCodeInternal.
func TestValidateDRAPatterns_ClaimReadErrorClassification(t *testing.T) {
	claimsGR := schema.GroupResource{Group: apiGroupResourceK8sIO, Resource: "resourceclaims"}
	tests := []struct {
		name       string
		getErr     error
		wantTarget error
		wantMsg    string
	}{
		{
			name:       "NotFound keeps ErrCodeNotFound",
			getErr:     k8serrors.NewNotFound(claimsGR, "gpu-claim"),
			wantTarget: errors.New(errors.ErrCodeNotFound, ""),
			wantMsg:    "not found",
		},
		{
			name:       "Forbidden (RBAC) stays ErrCodeInternal",
			getErr:     k8serrors.NewForbidden(claimsGR, "gpu-claim", stderrors.New("rbac denied")),
			wantTarget: errors.New(errors.ErrCodeInternal, ""),
			wantMsg:    "failed to read ResourceClaim",
		},
		{
			name:       "context canceled maps to ErrCodeTimeout",
			getErr:     context.Canceled,
			wantTarget: errors.New(errors.ErrCodeTimeout, ""),
			wantMsg:    "timed out reading ResourceClaim",
		},
		{
			name:       "context deadline exceeded maps to ErrCodeTimeout",
			getErr:     context.DeadlineExceeded,
			wantTarget: errors.New(errors.ErrCodeTimeout, ""),
			wantMsg:    "timed out reading ResourceClaim",
		},
		{
			name:       "apiserver Timeout maps to ErrCodeTimeout",
			getErr:     k8serrors.NewTimeoutError("request did not complete", 1),
			wantTarget: errors.New(errors.ErrCodeTimeout, ""),
			wantMsg:    "timed out reading ResourceClaim",
		},
		{
			name:       "apiserver ServerTimeout maps to ErrCodeTimeout",
			getErr:     k8serrors.NewServerTimeout(claimsGR, "get", 1),
			wantTarget: errors.New(errors.ErrCodeTimeout, ""),
			wantMsg:    "timed out reading ResourceClaim",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, err := newGPUTestRun()
			if err != nil {
				t.Fatalf("newGPUTestRun() error = %v", err)
			}
			// buildDRATestPod satisfies steps 1-4 (resourceClaims present,
			// only the granted container references the claim, no GPU limits,
			// no hostPath) so the claim read in step 5 is reached.
			pod := buildDRATestPod(run, nil)

			dynClient := newDRAFakeDynamicClient()
			dynClient.PrependReactor("get", "resourceclaims",
				func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})

			_, err = validateDRAPatterns(context.Background(), dynClient, pod, run, "v1")
			if err == nil {
				t.Fatal("expected a claim-read failure")
			}
			if !stderrors.Is(err, tt.wantTarget) {
				t.Errorf("error = %v, want code %v to match", err, tt.wantTarget)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want message containing %q", err, tt.wantMsg)
			}
		})
	}
}

// TestCleanupInspectEquivalentIsReadOnly guards the cleanup artifact's
// "equivalent" command: it must be a read-only inspection, never an
// executable delete — a copy-pasted `kubectl delete namespace` would run
// WITHOUT the implementation's metadata.uid precondition and ownership-label
// guard and could delete a same-name replacement namespace.
func TestCleanupInspectEquivalentIsReadOnly(t *testing.T) {
	got := cleanupInspectEquivalent("aicr-gpu-test-abc")
	if strings.Contains(got, "kubectl delete") {
		t.Errorf("equivalent = %q — must not contain an executable delete", got)
	}
	if !strings.HasPrefix(got, "kubectl get namespace aicr-gpu-test-abc") {
		t.Errorf("equivalent = %q, want a read-only kubectl get namespace inspection", got)
	}
	// The note must stay STATE-NEUTRAL: no claim that cleanup always deletes
	// (foreign/rejected/ambiguous paths deliberately do not) and no claim
	// about what output to expect (same-name reuse legitimately shows one).
	for _, overstatement := range []string{"expect no output", "cleanup itself runs"} {
		if strings.Contains(got, overstatement) {
			t.Errorf("equivalent = %q — note must not overstate cleanup behavior (%q)", got, overstatement)
		}
	}
	if !strings.Contains(got, "artifact body") {
		t.Errorf("equivalent = %q, want deferral to the artifact body for the actual outcome", got)
	}
}

// markPodsTerminalOnCreate installs a reactor that drives every created pod
// straight to the phase/exit-code chosen by phaseFor(podName), pinning it to
// node1. Used by failure-path tests that need the GPU test pod and the
// no-allocation probe pod to reach different terminal states.
func markPodsTerminalOnCreate(client *k8sfake.Clientset, phaseFor func(name string) (corev1.PodPhase, int32)) {
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := createAction.GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		phase, exit := phaseFor(pod.Name)
		pod.Status.Phase = phase
		for _, c := range pod.Spec.Containers {
			pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
				Name: c.Name,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: exit},
				},
			})
		}
		if pod.Spec.NodeName == "" {
			pod.Spec.NodeName = "node1"
		}
		return false, nil, nil
	})
}

// podLogFetchCount counts pods/log subresource reads recorded by the fake
// clientset — the observable signal that container logs were fetched for
// artifacts (the fake records the action when GetLogs is invoked).
func podLogFetchCount(client *k8sfake.Clientset) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "pods" && a.GetSubresource() == "log" {
			n++
		}
	}
	return n
}

// captureStdout redirects os.Stdout around fn and returns everything written.
// Artifact recording (recordRawTextArtifact) writes to stdout — the CTRF
// stdout capture in production — so tests assert emitted evidence this way.
//
// Failure-safe by construction: restoration, the WRITABLE pipe-end close
// (checked, per the repo Close rule), and the reader drain all run in a
// defer, so a panic or t.Fatal/runtime.Goexit inside fn cannot leave stdout
// redirected or leak the reader goroutine; the result channel is buffered so
// the reader's send never blocks, and its read error is surfaced.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stdout = w

	type readResult struct {
		data string
		err  error
	}
	done := make(chan readResult, 1) // buffered: the reader can always complete its send
	go func() {
		b, readErr := io.ReadAll(r)
		_ = r.Close() // read-only end — no buffered data to flush
		done <- readResult{data: string(b), err: readErr}
	}()

	var out string
	func() {
		defer func() {
			os.Stdout = old
			// Writable end: closing flushes the pipe and unblocks the
			// reader — its error must be checked.
			if closeErr := w.Close(); closeErr != nil {
				t.Errorf("captureStdout: closing pipe writer: %v", closeErr)
			}
			res := <-done
			if res.err != nil {
				t.Errorf("captureStdout: reading captured output: %v", res.err)
			}
			out = res.data
		}()
		fn()
	}()
	return out
}

// TestCheckSecureAcceleratorAccess_FailurePathsStillFetchLogs pins the
// diagnostic ordering in BOTH secure-access modes: container logs are fetched
// and their artifacts emitted BEFORE the pass/fail verdict, so a failing GPU
// test pod and a failing no-allocation isolation probe both still ship their
// logs (they would otherwise be missing exactly when the check fails).
func TestCheckSecureAcceleratorAccess_FailurePathsStillFetchLogs(t *testing.T) {
	tests := []struct {
		name string
		// draMode: full-GPU DRA usable (DeviceClass + validated slice) vs
		// device-plugin mode (scalar allocatable only).
		draMode bool
		// phaseFor drives the GPU test pod and the no-alloc probe pod to
		// different terminal states by name prefix.
		phaseFor func(name string) (corev1.PodPhase, int32)
		wantMsg  string
		// wantMinLogFetches: failing gpu pod → its 2 containers fetched;
		// failing probe → gpu pod's 2 containers + the probe's own logs.
		wantMinLogFetches int
		// wantArtifacts must appear in the emitted evidence despite the failure.
		wantArtifacts []string
	}{
		{
			name: "device plugin: GPU test pod fails, its container logs are still emitted",
			phaseFor: func(name string) (corev1.PodPhase, int32) {
				if strings.HasPrefix(name, gpuTestPodPrefix) {
					return corev1.PodFailed, 1
				}
				return corev1.PodSucceeded, 0
			},
			// Both containers exit 1, so the unauthorized-sibling gate trips
			// first — the point here is only that the failure happens AFTER
			// the pod's logs were fetched.
			wantMsg:           "container-level isolation broken",
			wantMinLogFetches: 2,
			wantArtifacts: []string{
				"--- GPU test pod logs (" + containerNameGPUTest + ") ---",
				"--- GPU test pod logs (" + containerNameUnauthorized + ") ---",
			},
		},
		{
			name:    "DRA: GPU test pod fails, its container logs are still emitted",
			draMode: true,
			phaseFor: func(name string) (corev1.PodPhase, int32) {
				if strings.HasPrefix(name, gpuTestPodPrefix) {
					return corev1.PodFailed, 1
				}
				return corev1.PodSucceeded, 0
			},
			wantMsg:           "container-level isolation broken",
			wantMinLogFetches: 2,
			wantArtifacts: []string{
				"--- GPU test pod logs (" + containerNameGPUTest + ") ---",
				"--- GPU test pod logs (" + containerNameUnauthorized + ") ---",
			},
		},
		{
			name: "device plugin: isolation probe fails, probe logs are still emitted",
			phaseFor: func(name string) (corev1.PodPhase, int32) {
				if strings.HasPrefix(name, noAllocProbePrefix) {
					return corev1.PodFailed, 1
				}
				return corev1.PodSucceeded, 0
			},
			wantMsg:           "isolation broken",
			wantMinLogFetches: 3,
			wantArtifacts:     []string{"--- No-allocation probe pod logs ---"},
		},
		{
			name:    "DRA: isolation probe fails, probe logs are still emitted",
			draMode: true,
			phaseFor: func(name string) (corev1.PodPhase, int32) {
				if strings.HasPrefix(name, noAllocProbePrefix) {
					return corev1.PodFailed, 1
				}
				return corev1.PodSucceeded, 0
			},
			wantMsg:           "isolation broken",
			wantMinLogFetches: 3,
			wantArtifacts:     []string{"--- No-allocation probe pod logs ---"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var node runtime.Object = testNode("node1", withGPUAllocatable("8"))
			dynClient := newDRAFakeDynamicClient() // no DeviceClass → device-plugin mode
			if tt.draMode {
				node = testNode("node1") // no scalar allocatable → DRA-only
				dynClient = newDRAFakeDynamicClient(
					testDeviceClass(draDriverGPU),
					testResourceSlice("gpu-1", draDriverGPU, "node1", 1, 1,
						map[string]interface{}{"nodeName": "node1"},
						[]interface{}{plainDevice("gpu-0")}),
				)
			}
			clientset := k8sfake.NewClientset(node)
			withDRAAPIDiscovery(t, clientset)
			markPodsTerminalOnCreate(clientset, tt.phaseFor)

			ctx := &validators.Context{
				Ctx:           context.Background(),
				Clientset:     clientset,
				DynamicClient: dynClient,
			}

			var err error
			out := captureStdout(t, func() { err = CheckSecureAcceleratorAccess(ctx) })
			if err == nil {
				t.Fatal("expected the check to fail")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want message containing %q", err, tt.wantMsg)
			}
			if got := podLogFetchCount(clientset); got < tt.wantMinLogFetches {
				t.Errorf("pods/log fetches = %d, want >= %d — failure paths must still record log artifacts",
					got, tt.wantMinLogFetches)
			}
			for _, want := range tt.wantArtifacts {
				if !strings.Contains(out, want) {
					t.Errorf("emitted evidence missing %q despite the failure", want)
				}
			}
		})
	}
}

func TestCheckSecureAcceleratorAccess_NeitherUsable(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node1")) // Ready node, no GPUs
	withDRAAPIDiscovery(t, clientset)
	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newDRAFakeDynamicClient(), // no DeviceClass, no slices
	}

	err := CheckSecureAcceleratorAccess(ctx)
	if err == nil {
		t.Fatal("expected failure when neither DRA nor device plugin is usable")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
		t.Errorf("error code = %v, want ErrCodeUnavailable", err)
	}
	if !strings.Contains(err.Error(), "no usable GPU allocation mechanism") {
		t.Errorf("error = %v, want message about no usable GPU allocation mechanism", err)
	}
	// The failure must explain both sides so operators can fix the environment.
	if !strings.Contains(err.Error(), draDriverGPU) || !strings.Contains(err.Error(), resourceNVIDIAGPU) {
		t.Errorf("error = %v, want details for both DRA and device plugin", err)
	}
}

// TestCheckSecureAcceleratorAccess_FailsFastWhenDeadlineTooShortForCleanup
// verifies the dynamic work-budget gate: a check context whose deadline
// cannot fit the cleanup reserve plus a minimum of work must fail with
// ErrCodeTimeout BEFORE creating any resource — on this device-plugin-capable
// cluster the test pod would otherwise be created and hold a GPU.
func TestCheckSecureAcceleratorAccess_FailsFastWhenDeadlineTooShortForCleanup(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	created := markPodsSucceededOnCreate(clientset)

	deadlineCtx, cancel := context.WithTimeout(context.Background(), gpuCheckMinWorkBudget)
	defer cancel()
	ctx := &validators.Context{
		Ctx:           deadlineCtx,
		Clientset:     clientset,
		DynamicClient: newDRAFakeDynamicClient(),
	}

	err := CheckSecureAcceleratorAccess(ctx)
	if err == nil {
		t.Fatal("expected fail-fast when the deadline cannot fit the cleanup reserve")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error code = %v, want ErrCodeTimeout", err)
	}
	if !strings.Contains(err.Error(), "too short to guarantee cleanup") {
		t.Errorf("error = %v, want message about the timeout being too short to guarantee cleanup", err)
	}
	if len(*created) != 0 {
		t.Errorf("fail-fast path created %d pod(s) — must not create any resource", len(*created))
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" {
			t.Errorf("fail-fast path issued create on %s — must not create any resource", action.GetResource().Resource)
		}
	}
}

// requireNodeNameAffinity asserts the pod carries REQUIRED node affinity
// constraining metadata.name (via matchFields, NOT the kubernetes.io/hostname
// label — node names and hostname labels can differ) to exactly the given
// node names, expressed as OR-ed single-node terms.
func requireNodeNameAffinity(t *testing.T, pod *corev1.Pod, wantNodes []string) {
	t.Helper()
	if pod.Spec.NodeName != "" {
		t.Errorf("pod nodeName = %q, want empty — scheduling must go through node affinity, not a spec.nodeName pin",
			pod.Spec.NodeName)
	}
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {

		t.Fatal("pod is missing required node affinity")
	}
	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != len(wantNodes) {
		t.Fatalf("node affinity terms = %+v, want one OR-ed matchFields term per node %v", terms, wantNodes)
	}
	for i, n := range wantNodes {
		if len(terms[i].MatchExpressions) != 0 {
			t.Errorf("term[%d] uses matchExpressions %+v — node names must match metadata.name via matchFields, not a label",
				i, terms[i].MatchExpressions)
		}
		if len(terms[i].MatchFields) != 1 {
			t.Fatalf("term[%d] matchFields = %+v, want exactly one requirement", i, terms[i].MatchFields)
		}
		field := terms[i].MatchFields[0]
		if field.Key != metav1.ObjectNameField || field.Operator != corev1.NodeSelectorOpIn {
			t.Errorf("term[%d] matchFields = %+v, want %s In [%s]", i, field, metav1.ObjectNameField, n)
		}
		if len(field.Values) != 1 || field.Values[0] != n {
			t.Errorf("term[%d] values = %v, want [%s] (matchFields In takes exactly one value)", i, field.Values, n)
		}
	}
}

func TestCheckSecureAcceleratorAccess_DevicePluginPath(t *testing.T) {
	clientset := k8sfake.NewClientset(
		testNode("node1", withGPUAllocatable("8")),
		testNode("node2", withGPUAllocatable("8")),
	)
	withDRAAPIDiscovery(t, clientset)
	created := markPodsSucceededOnCreate(clientset)
	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newDRAFakeDynamicClient(), // no gpu.nvidia.com DeviceClass → device plugin mode
	}

	if err := CheckSecureAcceleratorAccess(ctx); err != nil {
		t.Fatalf("CheckSecureAcceleratorAccess() error = %v", err)
	}

	gpuPod := findPodByPrefix(*created, gpuTestPodPrefix)
	if gpuPod == nil {
		t.Fatalf("device plugin test pod (prefix %q) was not created; created=%d", gpuTestPodPrefix, len(*created))
	}
	// The scheduler may place the pod on ANY detected device-plugin node
	// (required node affinity over the full set) — a single-node pin would
	// false-fail when that node's GPUs are all in use.
	requireNodeNameAffinity(t, gpuPod, []string{"node1", "node2"})
	if len(gpuPod.Spec.ResourceClaims) != 0 {
		t.Errorf("device plugin test pod uses %d resourceClaims, want 0", len(gpuPod.Spec.ResourceClaims))
	}
	qty, ok := gpuPod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]
	if !ok || qty.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("device plugin test pod limits = %v, want %s: 1", gpuPod.Spec.Containers[0].Resources.Limits, resourceNVIDIAGPU)
	}
	// Positive probe: the granted container must verify EXACTLY ONE usable
	// GPU via nvidia-smi (a /dev/nvidia* listing would also pass when every
	// GPU — or only control devices — is exposed).
	grantedScript := gpuPod.Spec.Containers[0].Command[2]
	if grantedScript != gpuExclusiveGrantProbeScript {
		t.Errorf("granted container script = %q, want gpuExclusiveGrantProbeScript", grantedScript)
	}
	if !strings.Contains(grantedScript, "nvidia-smi") || !strings.Contains(grantedScript, "exactly 1 usable GPU") {
		t.Errorf("granted container script must count GPUs via nvidia-smi and require exactly 1: %q", grantedScript)
	}
	// Multi-container isolation subtest: an identical unauthorized sibling
	// container that is granted nothing (ai-conformance#75 parity).
	if len(gpuPod.Spec.Containers) != 2 {
		t.Fatalf("device plugin test pod has %d containers, want 2 (granted + unauthorized)", len(gpuPod.Spec.Containers))
	}
	unauthorized := gpuPod.Spec.Containers[1]
	if unauthorized.Name != containerNameUnauthorized {
		t.Errorf("second container = %q, want %q", unauthorized.Name, containerNameUnauthorized)
	}
	if _, hasGPU := unauthorized.Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; hasGPU {
		t.Errorf("unauthorized container must not request %s limits", resourceNVIDIAGPU)
	}
	if len(unauthorized.Resources.Claims) != 0 {
		t.Error("unauthorized container must not reference resource claims")
	}
	// Negative probe stays device-file based: unauthorized containers must
	// not rely on nvidia-smi presence to prove GPU absence.
	if unauthorized.Command[2] != gpuAbsenceProbeScript {
		t.Errorf("unauthorized container script = %q, want gpuAbsenceProbeScript", unauthorized.Command[2])
	}
	for _, vol := range gpuPod.Spec.Volumes {
		if vol.HostPath != nil {
			t.Errorf("device plugin test pod must not use hostPath volumes, found %v", vol.HostPath)
		}
	}

	noAllocPod := findPodByPrefix(*created, noAllocProbePrefix)
	if noAllocPod == nil {
		t.Fatal("no-allocation probe pod was not created")
	}
	if len(noAllocPod.Spec.ResourceClaims) != 0 {
		t.Errorf("no-allocation probe pod uses resourceClaims, want none")
	}
	if _, hasGPU := noAllocPod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; hasGPU {
		t.Error("no-allocation probe pod must not request nvidia.com/gpu")
	}

	// Cleanup runs on success too: the per-run test namespace must be gone.
	namespaces, err := clientset.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list namespaces: %v", err)
	}
	for _, ns := range namespaces.Items {
		if strings.HasPrefix(ns.Name, gpuTestNamespacePrefix) {
			t.Errorf("per-run test namespace %s leaked after a successful run", ns.Name)
		}
	}
}

// TestCheckSecureAcceleratorAccess_DevicePluginDRABackedExtendedResource
// verifies sound attribution under KEP-5004: when a DeviceClass maps
// nvidia.com/gpu to DRA via spec.extendedResourceName and full-GPU DRA is not
// usable, the device plugin test must still RUN, constrained via required
// node affinity to the nodes with scalar allocatable nvidia.com/gpu — on such
// nodes the request is device-plugin-served by definition, so the mapping is
// recorded evidence, not a failure.
func TestCheckSecureAcceleratorAccess_DevicePluginDRABackedExtendedResource(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	created := markPodsSucceededOnCreate(clientset)
	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: clientset,
		// DeviceClass maps the extended resource to DRA, but full-GPU DRA is
		// not usable (no gpu.nvidia.com DeviceClass, no ResourceSlices).
		DynamicClient: newDRAFakeDynamicClient(
			testDeviceClassWithExtendedResource("gpu-er.nvidia.com", resourceNVIDIAGPU),
		),
	}

	if err := CheckSecureAcceleratorAccess(ctx); err != nil {
		t.Fatalf("CheckSecureAcceleratorAccess() error = %v, want the device plugin test to run", err)
	}
	gpuPod := findPodByPrefix(*created, gpuTestPodPrefix)
	if gpuPod == nil {
		t.Fatalf("device plugin test pod (prefix %q) was not created; created=%d", gpuTestPodPrefix, len(*created))
	}
	requireNodeNameAffinity(t, gpuPod, []string{"node1"})
	if len(gpuPod.Spec.ResourceClaims) != 0 {
		t.Errorf("device plugin test pod uses %d resourceClaims, want 0", len(gpuPod.Spec.ResourceClaims))
	}
}

// TestCheckSecureAcceleratorAccess_DRAPathUnchanged verifies the DRA path
// still runs the ResourceClaim-based isolation test when full-GPU DRA is
// usable.
func TestCheckSecureAcceleratorAccess_DRAPathUnchanged(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node1")) // no allocatable GPUs → DRA only
	withDRAAPIDiscovery(t, clientset)
	created := markPodsSucceededOnCreate(clientset)
	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: clientset,
		DynamicClient: newDRAFakeDynamicClient(
			testDeviceClass(draDriverGPU),
			testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("gpu-0")}),
		),
	}

	if err := CheckSecureAcceleratorAccess(ctx); err != nil {
		t.Fatalf("CheckSecureAcceleratorAccess() error = %v", err)
	}

	gpuPod := findPodByPrefix(*created, gpuTestPodPrefix)
	if gpuPod == nil {
		t.Fatalf("DRA test pod (prefix %q) was not created; created=%d", gpuTestPodPrefix, len(*created))
	}
	if len(gpuPod.Spec.ResourceClaims) != 1 {
		t.Errorf("DRA test pod resourceClaims = %d, want 1", len(gpuPod.Spec.ResourceClaims))
	}
	if _, hasGPU := gpuPod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; hasGPU {
		t.Error("DRA test pod must not request nvidia.com/gpu limits")
	}
	// Multi-container isolation subtest: only the granted (first) container
	// references the claim; the unauthorized sibling references nothing.
	if len(gpuPod.Spec.Containers) != 2 {
		t.Fatalf("DRA test pod has %d containers, want 2 (granted + unauthorized)", len(gpuPod.Spec.Containers))
	}
	if len(gpuPod.Spec.Containers[0].Resources.Claims) != 1 {
		t.Errorf("granted container claims = %d, want 1", len(gpuPod.Spec.Containers[0].Resources.Claims))
	}
	// Positive probe: exactly one usable GPU, counted via nvidia-smi.
	if gpuPod.Spec.Containers[0].Command[2] != gpuExclusiveGrantProbeScript {
		t.Errorf("granted container script = %q, want gpuExclusiveGrantProbeScript",
			gpuPod.Spec.Containers[0].Command[2])
	}
	unauthorized := gpuPod.Spec.Containers[1]
	if unauthorized.Name != containerNameUnauthorized {
		t.Errorf("second container = %q, want %q", unauthorized.Name, containerNameUnauthorized)
	}
	if len(unauthorized.Resources.Claims) != 0 {
		t.Error("unauthorized container must not reference the ResourceClaim")
	}
	if unauthorized.Command[2] != gpuAbsenceProbeScript {
		t.Errorf("unauthorized container script = %q, want gpuAbsenceProbeScript", unauthorized.Command[2])
	}
	if findPodByPrefix(*created, noAllocProbePrefix) == nil {
		t.Error("no-allocation probe pod was not created on the DRA path")
	}
}

// TestCheckSecureAcceleratorAccess_DRAPathBetaVersions verifies the DRA
// secure-access path on beta-only clusters (K8s 1.32/1.33): the probe detects
// DRA as usable at the served beta version and the test ResourceClaim is
// created at that group-version with the version-correct request shape —
// v1beta2 wraps the request detail in `exactly`, v1beta1 carries
// deviceClassName/allocationMode/count directly on the request.
func TestCheckSecureAcceleratorAccess_DRAPathBetaVersions(t *testing.T) {
	tests := []struct {
		version     string
		wantExactly bool
	}{
		{version: "v1beta2", wantExactly: true},
		{version: versionV1beta1, wantExactly: false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			apiVersion := apiGroupResourceK8sIO + "/" + tt.version
			clientset := k8sfake.NewClientset(testNode("node1")) // no allocatable GPUs → DRA only
			withDRAAPIDiscoveryAt(t, clientset, apiVersion)
			created := markPodsSucceededOnCreate(clientset)

			device := plainDevice("gpu-0")
			if tt.version == versionV1beta1 {
				device = basicWrappedDevice("gpu-0", map[string]interface{}{})
			}
			dynClient := newDRAFakeDynamicClientAt(tt.version,
				testDeviceClassAt(apiVersion, draDriverGPU),
				testResourceSliceAt(apiVersion, "s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{device}),
			)
			var createdClaim *unstructured.Unstructured
			dynClient.PrependReactor("create", "resourceclaims",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					createAction, ok := action.(k8stesting.CreateAction)
					if !ok {
						return false, nil, nil
					}
					if u, ok := createAction.GetObject().(*unstructured.Unstructured); ok {
						createdClaim = u.DeepCopy()
					}
					return false, nil, nil
				})

			ctx := &validators.Context{
				Ctx:           context.Background(),
				Clientset:     clientset,
				DynamicClient: dynClient,
			}
			if err := CheckSecureAcceleratorAccess(ctx); err != nil {
				t.Fatalf("CheckSecureAcceleratorAccess() error = %v, want the DRA path to run on a %s-only cluster",
					err, tt.version)
			}
			if findPodByPrefix(*created, gpuTestPodPrefix) == nil {
				t.Fatalf("DRA test pod (prefix %q) was not created on a %s-only cluster", gpuTestPodPrefix, tt.version)
			}
			if createdClaim == nil {
				t.Fatal("no ResourceClaim was created at the served beta version")
			}
			if got := createdClaim.GetAPIVersion(); got != apiVersion {
				t.Errorf("claim apiVersion = %q, want %q", got, apiVersion)
			}
			requests, found, err := unstructured.NestedSlice(createdClaim.Object, "spec", "devices", "requests")
			if err != nil || !found || len(requests) != 1 {
				t.Fatalf("claim requests = %v (found=%t err=%v), want exactly one request", requests, found, err)
			}
			request, ok := requests[0].(map[string]interface{})
			if !ok {
				t.Fatalf("claim request has unexpected type %T", requests[0])
			}
			if tt.wantExactly {
				exactly, ok := request["exactly"].(map[string]interface{})
				if !ok {
					t.Fatalf("claim request = %v, want the `exactly` wrapper at %s", request, tt.version)
				}
				if exactly["deviceClassName"] != draDriverGPU {
					t.Errorf("exactly.deviceClassName = %v, want %s", exactly["deviceClassName"], draDriverGPU)
				}
				return
			}
			if _, hasExactly := request["exactly"]; hasExactly {
				t.Errorf("claim request = %v, must NOT use the `exactly` wrapper at %s", request, tt.version)
			}
			if request["deviceClassName"] != draDriverGPU {
				t.Errorf("request.deviceClassName = %v, want %s (direct field at %s)",
					request["deviceClassName"], draDriverGPU, tt.version)
			}
			if request["allocationMode"] != "ExactCount" || request["count"] != int64(1) {
				t.Errorf("request allocationMode/count = %v/%v, want ExactCount/1",
					request["allocationMode"], request["count"])
			}
		})
	}
}

func TestValidateDevicePluginPatterns(t *testing.T) {
	validPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "dp-test"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: containerNameGPUTest,
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceName(resourceNVIDIAGPU): resource.MustParse("1"),
							},
						},
					},
					{
						Name: containerNameUnauthorized,
					},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  containerNameGPUTest,
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
					},
					{
						Name:  containerNameUnauthorized,
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
					},
				},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*corev1.Pod)
		wantErr string
	}{
		{
			name:   "valid device plugin pod",
			mutate: func(*corev1.Pod) {},
		},
		{
			name: "pod with resourceClaims rejected",
			mutate: func(p *corev1.Pod) {
				p.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: gpuClaimName}}
			},
			wantErr: "resourceClaims",
		},
		{
			name: "pod without gpu limits rejected",
			mutate: func(p *corev1.Pod) {
				p.Spec.Containers[0].Resources.Limits = nil
			},
			wantErr: "does not request",
		},
		{
			name: "unauthorized container with gpu limits rejected",
			mutate: func(p *corev1.Pod) {
				p.Spec.Containers[1].Resources.Limits = corev1.ResourceList{
					corev1.ResourceName(resourceNVIDIAGPU): resource.MustParse("1"),
				}
			},
			wantErr: "unauthorized container",
		},
		{
			name: "hostPath to GPU devices rejected",
			mutate: func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{{
					Name: "dev",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: "/dev/nvidia0"},
					},
				}}
			},
			wantErr: "hostPath",
		},
		{
			name: "failed pod rejected",
			mutate: func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
			},
			wantErr: "phase=Failed",
		},
		{
			name: "unauthorized container saw GPU rejected",
			mutate: func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				p.Status.ContainerStatuses[1].State.Terminated.ExitCode = 1
			},
			wantErr: "container-level isolation broken",
		},
		{
			name: "missing unauthorized container status fails closed",
			mutate: func(p *corev1.Pod) {
				p.Status.ContainerStatuses = p.Status.ContainerStatuses[:1]
			},
			wantErr: "cannot verify container-level isolation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := validPod()
			tt.mutate(pod)
			report, err := validateDevicePluginPatterns(pod)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateDevicePluginPatterns() error = %v, want nil", err)
				}
				if report.GPULimitsCount != 1 || report.ResourceClaimCount != 0 {
					t.Errorf("report = %+v, want GPULimitsCount=1 ResourceClaimCount=0", report)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateDevicePluginPatterns() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestBuildDevicePluginTestPod(t *testing.T) {
	run, err := newGPUTestRun()
	if err != nil {
		t.Fatalf("newGPUTestRun() error = %v", err)
	}

	nodes := []string{"gpu-node-1", "gpu-node-2"}
	pod := buildDevicePluginTestPod(run, nil, nodes)
	if pod.Name != run.podName || pod.Namespace != run.namespace {
		t.Errorf("pod = %s/%s, want %s/%s", pod.Namespace, pod.Name, run.namespace, run.podName)
	}
	if !strings.HasPrefix(run.namespace, gpuTestNamespacePrefix) {
		t.Errorf("run namespace = %q, want per-run namespace with prefix %q", run.namespace, gpuTestNamespacePrefix)
	}
	// Required node affinity over ALL detected device-plugin nodes — not a
	// single spec.nodeName pin (which false-fails when that node's GPUs are
	// all in use).
	requireNodeNameAffinity(t, pod, nodes)
	if len(pod.Spec.ResourceClaims) != 0 {
		t.Errorf("ResourceClaims = %d, want 0", len(pod.Spec.ResourceClaims))
	}
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("containers = %d, want 2 (granted + unauthorized)", len(pod.Spec.Containers))
	}
	qty, ok := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]
	if !ok || qty.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("limits = %v, want %s: 1", pod.Spec.Containers[0].Resources.Limits, resourceNVIDIAGPU)
	}
	unauthorized := pod.Spec.Containers[1]
	if unauthorized.Name != containerNameUnauthorized {
		t.Errorf("second container = %q, want %q", unauthorized.Name, containerNameUnauthorized)
	}
	if len(unauthorized.Resources.Limits) != 0 || len(unauthorized.Resources.Claims) != 0 {
		t.Errorf("unauthorized container must be granted nothing: limits=%v claims=%v",
			unauthorized.Resources.Limits, unauthorized.Resources.Claims)
	}
	if unauthorized.Image != cudaTestImage {
		t.Errorf("unauthorized image = %q, want identical %q", unauthorized.Image, cudaTestImage)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("default tolerations = %v, want tolerate-all", pod.Spec.Tolerations)
	}
	if pod.Spec.Containers[0].Image != cudaTestImage {
		t.Errorf("image = %q, want %q", pod.Spec.Containers[0].Image, cudaTestImage)
	}
	// Positive probe: exactly one usable GPU counted via nvidia-smi; negative
	// probe stays /dev/nvidia*-based on the unauthorized sibling.
	if pod.Spec.Containers[0].Command[2] != gpuExclusiveGrantProbeScript {
		t.Errorf("granted container script = %q, want gpuExclusiveGrantProbeScript", pod.Spec.Containers[0].Command[2])
	}
	if unauthorized.Command[2] != gpuAbsenceProbeScript {
		t.Errorf("unauthorized container script = %q, want gpuAbsenceProbeScript", unauthorized.Command[2])
	}

	custom := []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	pod = buildDevicePluginTestPod(run, custom, nodes)
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "nvidia.com/gpu" {
		t.Errorf("custom tolerations not applied: %v", pod.Spec.Tolerations)
	}
}

// ambiguousPodCreate installs a reactor that PERSISTS every created pod in
// the fake tracker and then reports a timeout — modeling an apiserver that
// committed the object but lost the response. An error return from a Create
// therefore does NOT prove the object is absent.
func ambiguousPodCreate(client *k8sfake.Clientset) {
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := createAction.GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		if err := client.Tracker().Add(pod.DeepCopy()); err != nil {
			return true, nil, err
		}
		return true, nil, k8serrors.NewTimeoutError("request timed out; object may have been persisted", 1)
	})
}

// cascadeNamespaceDelete installs a reactor that models the namespace
// deletion controller for the fake clientset: deleting a namespace also
// removes the tracked pods (and, when dynClient is non-nil, the
// resource.k8s.io/v1 ResourceClaims) in that namespace. The fake tracker
// performs no cascading deletion of its own, so without this the
// namespace-based cleanup would confirm namespace absence while orphaned
// tracked objects linger — masking exactly the leaks the assertions below
// look for.
func cascadeNamespaceDelete(t *testing.T, client *k8sfake.Clientset, dynClient *dynamicfake.FakeDynamicClient) {
	t.Helper()
	podsGVR := corev1.SchemeGroupVersion.WithResource("pods")
	podsGVK := corev1.SchemeGroupVersion.WithKind("Pod")
	client.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction, ok := action.(k8stesting.DeleteAction)
		if !ok {
			return false, nil, nil
		}
		ns := deleteAction.GetName()
		// Reactors run under the Fake's action lock — go through the
		// trackers directly (their locks are separate), not the clientset.
		obj, err := client.Tracker().List(podsGVR, podsGVK, ns)
		if err != nil {
			return true, nil, err
		}
		podList, ok := obj.(*corev1.PodList)
		if !ok {
			t.Errorf("tracker pod list has unexpected type %T", obj)
			return false, nil, nil
		}
		for i := range podList.Items {
			if delErr := client.Tracker().Delete(podsGVR, ns, podList.Items[i].Name); delErr != nil {
				return true, nil, delErr
			}
		}
		if dynClient != nil {
			claimsGVR := draGVRAt("v1", "resourceclaims")
			claimsGVK := schema.GroupVersionKind{Group: apiGroupResourceK8sIO, Version: "v1", Kind: "ResourceClaim"}
			if claimObj, listErr := dynClient.Tracker().List(claimsGVR, claimsGVK, ns); listErr == nil {
				if ul, listOK := claimObj.(*unstructured.UnstructuredList); listOK {
					for i := range ul.Items {
						_ = dynClient.Tracker().Delete(claimsGVR, ns, ul.Items[i].GetName())
					}
				}
			}
		}
		// Fall through to the default reactor, which removes the namespace
		// object itself.
		return false, nil, nil
	})
}

// assertNoLeakedTestResources verifies cleanup left nothing behind: no test
// pods in any namespace and no per-run test namespace. Callers must install
// cascadeNamespaceDelete first so the fake models the namespace controller's
// cascading deletion.
func assertNoLeakedTestResources(t *testing.T, clientset *k8sfake.Clientset) {
	t.Helper()
	pods, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list pods: %v", err)
	}
	for _, p := range pods.Items {
		t.Errorf("pod %s/%s leaked after failed deploy — an ambiguously created pod stays bound and holds a GPU",
			p.Namespace, p.Name)
	}
	namespaces, err := clientset.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list namespaces: %v", err)
	}
	for _, ns := range namespaces.Items {
		if strings.HasPrefix(ns.Name, gpuTestNamespacePrefix) {
			t.Errorf("per-run test namespace %s leaked after cleanup", ns.Name)
		}
	}
}

// TestCheckSecureAcceleratorAccess_AmbiguousPodCreateCleansUp verifies the
// leak guard for AMBIGUOUS creates on both allocation paths: the apiserver
// persists the test pod but the Create call returns an error. Cleanup is
// registered BEFORE the first Create and deletes the per-run namespace, so
// the persisted pod (and, on the DRA path, the ResourceClaim) must be gone
// even though deploy failed.
func TestCheckSecureAcceleratorAccess_AmbiguousPodCreateCleansUp(t *testing.T) {
	t.Run("device plugin path", func(t *testing.T) {
		clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
		withDRAAPIDiscovery(t, clientset)
		ambiguousPodCreate(clientset)
		cascadeNamespaceDelete(t, clientset, nil)
		ctx := &validators.Context{
			Ctx:           context.Background(),
			Clientset:     clientset,
			DynamicClient: newDRAFakeDynamicClient(),
		}

		if err := CheckSecureAcceleratorAccess(ctx); err == nil {
			t.Fatal("expected failure when the pod create times out")
		}
		assertNoLeakedTestResources(t, clientset)
	})

	t.Run("DRA path", func(t *testing.T) {
		clientset := k8sfake.NewClientset(testNode("node1")) // no allocatable GPUs → DRA only
		withDRAAPIDiscovery(t, clientset)
		ambiguousPodCreate(clientset)
		dynClient := newDRAFakeDynamicClient(
			testDeviceClass(draDriverGPU),
			testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("gpu-0")}),
		)
		cascadeNamespaceDelete(t, clientset, dynClient)
		ctx := &validators.Context{
			Ctx:           context.Background(),
			Clientset:     clientset,
			DynamicClient: dynClient,
		}

		if err := CheckSecureAcceleratorAccess(ctx); err == nil {
			t.Fatal("expected failure when the pod create times out")
		}
		assertNoLeakedTestResources(t, clientset)
		claims, err := dynClient.Resource(draGVRAt("v1", "resourceclaims")).List(
			context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatalf("failed to list ResourceClaims: %v", err)
		}
		for _, c := range claims.Items {
			t.Errorf("ResourceClaim %s leaked after failed deploy", c.GetName())
		}
	})
}

// observedTestRun builds a gpuTestRun modeling a namespace this run created
// and positively observed (Create succeeded): ownership token and UID known.
func observedTestRun(nsName string, uid types.UID) *gpuTestRun {
	return &gpuTestRun{
		token:      "0123456789abcdef0123456789abcdef",
		namespace:  nsName,
		nsObserved: true,
		nsUID:      uid,
	}
}

// ownedNamespace returns the namespace object matching an observedTestRun:
// same name, same UID, carrying the run's ownership label.
func ownedNamespace(run *gpuTestRun) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   run.namespace,
		UID:    run.nsUID,
		Labels: map[string]string{gpuTestRunLabel: run.token},
	}}
}

// TestCleanupGPUTestNamespace_ReconcilesUntilAbsent verifies cleanup keeps
// reconciling across its whole budget instead of sampling a fixed number of
// times: the namespace survives the first Delete (Terminating — content
// still being garbage-collected server-side) and disappears only on a later
// poll; cleanup must confirm absence. Late-committing creates need no
// client-side reconciliation in this design: once the namespace is
// Terminating the NamespaceLifecycle admission plugin rejects subsequent
// Creates, and the namespace deletion controller durably garbage-collects
// content that still lands before finalizing.
func TestCleanupGPUTestNamespace_ReconcilesUntilAbsent(t *testing.T) {
	run := observedTestRun(gpuTestNamespacePrefix+"reconcile", "uid-reconcile")
	clientset := k8sfake.NewClientset(ownedNamespace(run))
	deletes := 0
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		deletes++
		if deletes < 2 {
			// Terminating: the delete is accepted but the namespace is
			// still finalizing — it stays visible to Get.
			return true, nil, nil
		}
		return false, nil, nil // finalized: the default reactor removes it
	})

	status, cleanupErr := cleanupGPUTestNamespace(clientset, run)
	if cleanupErr != nil {
		t.Errorf("cleanup error = %v, want nil for a confirmed deletion", cleanupErr)
	}
	if !strings.Contains(status, "confirmed absent") {
		t.Errorf("status = %q, want confirmed absence", status)
	}
	if _, err := clientset.CoreV1().Namespaces().Get(
		context.Background(), run.namespace, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("namespace still present after cleanup (get err = %v)", err)
	}
}

// TestCleanupGPUTestNamespace_BudgetBounded verifies cleanup runs under a
// SINGLE overall deadline and, when the deletion is ACCEPTED but the
// namespace outlives the budget (e.g. a DRA-finalizer-stuck Terminating
// namespace), reports deletion-in-progress (server-side continuation)
// instead of claiming success — and without stacking per-resource budgets
// that could exceed the validator Job's activeDeadlineSeconds.
func TestCleanupGPUTestNamespace_BudgetBounded(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	run := observedTestRun(gpuTestNamespacePrefix+"stuck", "uid-stuck")
	clientset := k8sfake.NewClientset(ownedNamespace(run))
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil // deletion accepted; the namespace never finalizes
	})

	start := time.Now()
	status, cleanupErr := cleanupGPUTestNamespace(clientset, run)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cleanup ran %v, want it bounded near the %v budget", elapsed, cleanupTimeout)
	}
	if strings.Contains(status, "confirmed absent") {
		t.Errorf("status = %q — must NOT claim success for an unconfirmed deletion", status)
	}
	if !strings.Contains(status, "continues server-side") {
		t.Errorf("status = %q, want a deletion-in-progress (server-side continuation) report", status)
	}
	if strings.Contains(status, "CLEANUP FAILED") {
		t.Errorf("status = %q — an ACCEPTED delete is in-progress, not a cleanup failure", status)
	}
	if cleanupErr != nil {
		t.Errorf("cleanup error = %v, want nil — server-side continuation is not a terminal cleanup failure", cleanupErr)
	}
}

// TestCleanupGPUTestNamespace_PersistentDeleteErrorReportsFailure verifies the
// fail-closed terminal state: when every Delete is rejected (persistent
// Forbidden) and the namespace never enters Terminating, cleanup must report
// CLEANUP FAILED with the last error — not the "deletion continues
// server-side" artifact, which would mask a real leak as progress.
func TestCleanupGPUTestNamespace_PersistentDeleteErrorReportsFailure(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	run := observedTestRun(gpuTestNamespacePrefix+"forbidden", "uid-forbidden")
	clientset := k8sfake.NewClientset(ownedNamespace(run))
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"},
			run.namespace, stderrors.New("RBAC denies namespace deletion"))
	})

	status, cleanupErr := cleanupGPUTestNamespace(clientset, run)
	if !strings.Contains(status, "CLEANUP FAILED") {
		t.Errorf("status = %q, want a CLEANUP FAILED report", status)
	}
	if !strings.Contains(status, "RBAC denies namespace deletion") {
		t.Errorf("status = %q, want the last Delete error surfaced", status)
	}
	if strings.Contains(status, "continues server-side") || strings.Contains(status, "confirmed absent") {
		t.Errorf("status = %q — a never-accepted deletion must not report progress or success", status)
	}
	if cleanupErr == nil {
		t.Error("cleanup error = nil, want non-nil so a passing check cannot report PASS over a leaked namespace")
	} else if !strings.Contains(cleanupErr.Error(), "RBAC denies namespace deletion") {
		t.Errorf("cleanup error = %v, want the last Delete error preserved in the chain", cleanupErr)
	}
}

// TestCleanupGPUTestNamespace_AmbiguousCreateSurfacesLate verifies the
// ambiguous-create settle loop: the namespace Create returned an error but
// the write committed AFTER cleanup's first Get (which reports NotFound).
// Cleanup must NOT treat that first NotFound as confirmed absence; it keeps
// re-Getting within the budget, adopts the namespace once it surfaces with
// this run's ownership label, and deletes it.
func TestCleanupGPUTestNamespace_AmbiguousCreateSurfacesLate(t *testing.T) {
	run := &gpuTestRun{
		token:     "0123456789abcdef0123456789abcdef",
		namespace: gpuTestNamespacePrefix + "late",
		// nsObserved=false: the Create was ambiguous, nothing was ever
		// positively observed.
	}
	lateNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   run.namespace,
		UID:    "uid-late",
		Labels: map[string]string{gpuTestRunLabel: run.token},
	}}
	clientset := k8sfake.NewClientset()
	gets := 0
	clientset.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			// The ambiguous create commits only after this first NotFound.
			if err := clientset.Tracker().Add(lateNS.DeepCopy()); err != nil {
				return true, nil, err
			}
			return true, nil, k8serrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, run.namespace)
		}
		return false, nil, nil // fall through to the tracker
	})

	status, cleanupErr := cleanupGPUTestNamespace(clientset, run)
	if cleanupErr != nil {
		t.Errorf("cleanup error = %v, want nil for a confirmed deletion", cleanupErr)
	}
	if !strings.Contains(status, "confirmed absent") {
		t.Errorf("status = %q, want confirmed absence after the late-surfacing namespace is deleted", status)
	}
	if _, err := clientset.CoreV1().Namespaces().Get(
		context.Background(), run.namespace, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("late-committed namespace leaked after cleanup (get err = %v)", err)
	}
}

// TestCleanupGPUTestNamespace_AmbiguousCreateNeverSurfaces verifies the
// distinct terminal artifact when an ambiguously created namespace never
// becomes visible within the cleanup budget: the report must say it may
// surface later and name the ownership label an external sweeper can key on
// — not claim confirmed absence and not report CLEANUP FAILED.
func TestCleanupGPUTestNamespace_AmbiguousCreateNeverSurfaces(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	run := &gpuTestRun{
		token:     "0123456789abcdef0123456789abcdef",
		namespace: gpuTestNamespacePrefix + "never",
	}
	clientset := k8sfake.NewClientset() // the namespace never appears

	status, cleanupErr := cleanupGPUTestNamespace(clientset, run)
	if !strings.Contains(status, "may still surface later") {
		t.Errorf("status = %q, want a report that the namespace may surface later", status)
	}
	if !strings.Contains(status, gpuTestRunLabel+"="+run.token) {
		t.Errorf("status = %q, want the ownership label %s=%s named for external sweeping",
			status, gpuTestRunLabel, run.token)
	}
	if strings.Contains(status, "confirmed absent") || strings.Contains(status, "CLEANUP FAILED") {
		t.Errorf("status = %q — never-observed must be its own terminal state", status)
	}
	if cleanupErr != nil {
		t.Errorf("cleanup error = %v, want nil — the failed Create is already the check's primary error", cleanupErr)
	}
}

// TestCleanupGPUTestNamespace_ConflictNotConfirmedAbsent verifies a 409 from
// the namespace Delete is never taken as proof of absence on its own:
// admission webhooks and the storage layer can return Conflict while this
// run's namespace still exists. Only the follow-up Get decides — a persistent
// Conflict with the same UID still present must end as CLEANUP FAILED, while
// a Conflict where the Get shows a DIFFERENT UID is confirmed absence (ours
// is gone, the name was reused).
func TestCleanupGPUTestNamespace_ConflictNotConfirmedAbsent(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	conflictReactor := func(clientset *k8sfake.Clientset, nsName string) {
		clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, k8serrors.NewConflict(schema.GroupResource{Resource: "namespaces"},
				nsName, stderrors.New("the object has been modified"))
		})
	}

	t.Run("same UID still present → cleanup failed, not absence", func(t *testing.T) {
		run := observedTestRun(gpuTestNamespacePrefix+"conflict", "uid-conflict")
		clientset := k8sfake.NewClientset(ownedNamespace(run))
		conflictReactor(clientset, run.namespace)

		status, cleanupErr := cleanupGPUTestNamespace(clientset, run)
		if strings.Contains(status, "confirmed absent") {
			t.Errorf("status = %q — a 409 with our UID still present must NOT be treated as absence", status)
		}
		if !strings.Contains(status, "CLEANUP FAILED") {
			t.Errorf("status = %q, want CLEANUP FAILED when the namespace never leaves", status)
		}
		if cleanupErr == nil {
			t.Error("cleanup error = nil, want non-nil for a persistent Conflict with the namespace still present")
		}
		if _, err := clientset.CoreV1().Namespaces().Get(
			context.Background(), run.namespace, metav1.GetOptions{}); err != nil {
			t.Errorf("namespace should still exist (get err = %v)", err)
		}
	})

	t.Run("different UID at the name → confirmed absent, foreign left untouched", func(t *testing.T) {
		run := observedTestRun(gpuTestNamespacePrefix+"reused", "uid-ours")
		// The name is now occupied by a DIFFERENT namespace (recreated by
		// someone else) — the UID precondition 409s and the Get proves ours
		// is gone.
		foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: run.namespace,
			UID:  "uid-someone-else",
		}}
		clientset := k8sfake.NewClientset(foreign)
		conflictReactor(clientset, run.namespace)

		status, cleanupErr := cleanupGPUTestNamespace(clientset, run)
		if cleanupErr != nil {
			t.Errorf("cleanup error = %v, want nil — our namespace is confirmed gone", cleanupErr)
		}
		if !strings.Contains(status, "confirmed absent") {
			t.Errorf("status = %q, want confirmed absence via the Get's UID mismatch", status)
		}
		if _, err := clientset.CoreV1().Namespaces().Get(
			context.Background(), run.namespace, metav1.GetOptions{}); err != nil {
			t.Errorf("the reused (foreign) namespace must survive (get err = %v)", err)
		}
	})
}

// TestCleanupGPUTestNamespace_DefinitiveCreateRejection verifies finding
// hygiene for DEFINITIVE namespace-create rejections (Forbidden, Invalid,
// ...): the apiserver rendered a verdict before storage, so the namespace
// cannot exist — cleanup must return immediately with a "never created"
// report instead of burning its budget settling and warning that the
// namespace "may still surface".
func TestCleanupGPUTestNamespace_DefinitiveCreateRejection(t *testing.T) {
	run := &gpuTestRun{
		token:     "0123456789abcdef0123456789abcdef",
		namespace: gpuTestNamespacePrefix + "rejected",
	}
	clientset := k8sfake.NewClientset()
	clientset.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"},
			run.namespace, stderrors.New("namespace creation denied"))
	})

	if err := createGPUTestNamespace(context.Background(), clientset, run); err == nil {
		t.Fatal("expected the Forbidden create to fail")
	}
	if !run.nsNeverCreated {
		t.Fatal("nsNeverCreated = false, want true — Forbidden is a definitive rejection, not an ambiguous write")
	}

	// NOTE: cleanupTimeout is left at its full default deliberately — the
	// wall-clock bound proves cleanup short-circuits instead of settling.
	start := time.Now()
	status, cleanupErr := cleanupGPUTestNamespace(clientset, run)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cleanup ran %v — a definitive rejection must not settle across the %v budget", elapsed, cleanupTimeout)
	}
	if cleanupErr != nil {
		t.Errorf("cleanup error = %v, want nil — the rejected Create is already the check's primary error", cleanupErr)
	}
	if !strings.Contains(status, "never created") {
		t.Errorf("status = %q, want a never-created report", status)
	}
	if strings.Contains(status, "may still surface later") {
		t.Errorf("status = %q — a definitively rejected create must not warn about late surfacing", status)
	}
}

// TestCreateGPUTestNamespace_AmbiguousErrorStaysSettleable verifies the
// counterpart boundary: a namespace-create error that is NOT a definitive
// rejection (a plain timeout) must leave nsNeverCreated false so cleanup
// keeps its settle loop — the write may have committed despite the error.
func TestCreateGPUTestNamespace_AmbiguousErrorStaysSettleable(t *testing.T) {
	run := &gpuTestRun{
		token:     "0123456789abcdef0123456789abcdef",
		namespace: gpuTestNamespacePrefix + "ambiguous",
	}
	clientset := k8sfake.NewClientset()
	clientset.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewTimeoutError("request timed out; object may have been persisted", 1)
	})

	if err := createGPUTestNamespace(context.Background(), clientset, run); err == nil {
		t.Fatal("expected the timed-out create to fail")
	}
	if run.nsNeverCreated {
		t.Error("nsNeverCreated = true, want false — a timeout may have persisted the namespace and needs the settle loop")
	}
}

// TestCheckSecureAcceleratorAccess_CleanupFailureFailsPassingCheck verifies
// the top-level contract for terminal cleanup failures: a run whose GPU test
// PASSED must still return an error when its namespace cleanup terminally
// fails — otherwise the check exits 0 while retaining the namespace, test
// pod, and allocated GPU claim indefinitely.
func TestCheckSecureAcceleratorAccess_CleanupFailureFailsPassingCheck(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	markPodsSucceededOnCreate(clientset) // the primary test passes
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"},
			"", stderrors.New("RBAC denies namespace deletion"))
	})
	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newDRAFakeDynamicClient(), // no DeviceClass → device plugin mode
	}

	err := CheckSecureAcceleratorAccess(ctx)
	if err == nil {
		t.Fatal("expected the check to FAIL when cleanup terminally fails — PASS over a leaked namespace/pod/claim is not a pass")
	}
	if !strings.Contains(err.Error(), "CLEANUP FAILED") {
		t.Errorf("error = %v, want the terminal cleanup failure surfaced", err)
	}
}

// TestCheckSecureAcceleratorAccess_PrimaryErrorNotMaskedByCleanupError
// verifies error precedence: when the primary test fails AND cleanup
// terminally fails, the check returns the PRIMARY error — the cleanup error
// is recorded as an artifact but never overwrites the real test result.
func TestCheckSecureAcceleratorAccess_PrimaryErrorNotMaskedByCleanupError(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	ambiguousPodCreate(clientset) // the primary test fails at deploy
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"},
			"", stderrors.New("RBAC denies namespace deletion"))
	})
	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newDRAFakeDynamicClient(), // no DeviceClass → device plugin mode
	}

	err := CheckSecureAcceleratorAccess(ctx)
	if err == nil {
		t.Fatal("expected the deploy failure to surface")
	}
	if !strings.Contains(err.Error(), "failed to create device plugin test pod") {
		t.Errorf("error = %v, want the PRIMARY deploy error preserved", err)
	}
	if strings.Contains(err.Error(), "CLEANUP FAILED") {
		t.Errorf("error = %v — the cleanup error must not mask the primary test failure", err)
	}
}

// TestCheckSecureAcceleratorAccess_ForeignNamespaceCollision verifies the
// namespace-collision guard: when the per-run namespace Create hits
// AlreadyExists against a namespace that does NOT carry this run's ownership
// label, the check must FAIL with ErrCodeConflict and cleanup must not issue
// any namespace Delete — the pre-existing foreign namespace survives.
func TestCheckSecureAcceleratorAccess_ForeignNamespaceCollision(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	var foreignName string
	clientset.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		ns, ok := createAction.GetObject().(*corev1.Namespace)
		if !ok {
			return false, nil, nil
		}
		// Model a pre-existing FOREIGN namespace at the same name: present in
		// the tracker WITHOUT the run's ownership label.
		foreignName = ns.Name
		foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.Name, UID: "foreign-uid"}}
		if err := clientset.Tracker().Add(foreign); err != nil {
			return true, nil, err
		}
		return true, nil, k8serrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, ns.Name)
	})
	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newDRAFakeDynamicClient(), // no DeviceClass → device plugin mode
	}

	err := CheckSecureAcceleratorAccess(ctx)
	if err == nil {
		t.Fatal("expected failure on a foreign-namespace name collision")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeConflict, "")) {
		t.Errorf("error code = %v, want ErrCodeConflict", err)
	}
	if !strings.Contains(err.Error(), "refusing to reuse or delete a foreign namespace") {
		t.Errorf("error = %v, want the foreign-namespace refusal", err)
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "namespaces" {
			t.Errorf("cleanup issued a namespace Delete despite the foreign collision: %+v", action)
		}
	}
	if _, getErr := clientset.CoreV1().Namespaces().Get(
		context.Background(), foreignName, metav1.GetOptions{}); getErr != nil {
		t.Errorf("pre-existing foreign namespace %s must survive the run (get err = %v)", foreignName, getErr)
	}
}

// TestCheckSecureAcceleratorAccess_CleanupDeleteCarriesUIDPrecondition
// verifies every namespace Delete issued by cleanup carries a
// metav1.Preconditions{UID} guard matching the namespace this run created —
// the apiserver-side guarantee that a recreated or foreign namespace at the
// same name can never be deleted.
func TestCheckSecureAcceleratorAccess_CleanupDeleteCarriesUIDPrecondition(t *testing.T) {
	const nsUID = types.UID("ns-uid-123")
	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	markPodsSucceededOnCreate(clientset)
	clientset.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		if ns, ok := createAction.GetObject().(*corev1.Namespace); ok {
			ns.UID = nsUID // fake apiserver: stamp a UID, then fall through
		}
		return false, nil, nil
	})
	var deleteOpts []metav1.DeleteOptions
	clientset.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction, ok := action.(k8stesting.DeleteActionImpl)
		if !ok {
			return false, nil, nil
		}
		deleteOpts = append(deleteOpts, deleteAction.DeleteOptions)
		return false, nil, nil // fall through to the tracker
	})
	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newDRAFakeDynamicClient(), // no DeviceClass → device plugin mode
	}

	if err := CheckSecureAcceleratorAccess(ctx); err != nil {
		t.Fatalf("CheckSecureAcceleratorAccess() error = %v", err)
	}
	if len(deleteOpts) == 0 {
		t.Fatal("cleanup issued no namespace Delete")
	}
	for i, opts := range deleteOpts {
		if opts.Preconditions == nil || opts.Preconditions.UID == nil {
			t.Errorf("namespace Delete #%d carries no UID precondition: %+v", i, opts)
			continue
		}
		if *opts.Preconditions.UID != nsUID {
			t.Errorf("namespace Delete #%d precondition UID = %q, want %q", i, *opts.Preconditions.UID, nsUID)
		}
	}
}

// TestNewGPUTestRun_TokenAndNameLimits pins the collision-resistance and
// naming contract: a 128-bit (32 hex chars) token, unique per run, stamped
// into every generated name, with all names within the 63-char DNS-1123
// limit for namespaces and pods.
func TestNewGPUTestRun_TokenAndNameLimits(t *testing.T) {
	run, err := newGPUTestRun()
	if err != nil {
		t.Fatalf("newGPUTestRun() error = %v", err)
	}
	if len(run.token) != 32 {
		t.Errorf("token length = %d, want 32 hex chars (128 bits)", len(run.token))
	}
	for _, name := range []string{run.namespace, run.podName, run.claimName, run.noAllocPodName} {
		if len(name) > 63 {
			t.Errorf("generated name %q is %d chars, exceeds the 63-char DNS-1123 limit", name, len(name))
		}
		if !strings.HasSuffix(name, run.token) {
			t.Errorf("generated name %q does not carry the run token %q", name, run.token)
		}
	}
	other, err := newGPUTestRun()
	if err != nil {
		t.Fatalf("newGPUTestRun() error = %v", err)
	}
	if other.token == run.token {
		t.Error("two runs generated the same token")
	}
}

// TestBuildResourceClaim verifies the version-correct ResourceClaim shapes:
// v1 and v1beta2 wrap the request detail in `exactly` (ExactDeviceRequest);
// v1beta1 carries deviceClassName/allocationMode/count directly on the
// request (no wrapper).
func TestBuildResourceClaim(t *testing.T) {
	run, err := newGPUTestRun()
	if err != nil {
		t.Fatalf("newGPUTestRun() error = %v", err)
	}

	tests := []struct {
		version     string
		wantExactly bool
	}{
		{version: "v1", wantExactly: true},
		{version: "v1beta2", wantExactly: true},
		{version: versionV1beta1, wantExactly: false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			claim := buildResourceClaim(run, tt.version)
			if got, want := claim.GetAPIVersion(), apiGroupResourceK8sIO+"/"+tt.version; got != want {
				t.Errorf("apiVersion = %q, want %q", got, want)
			}
			requests, found, nestedErr := unstructured.NestedSlice(claim.Object, "spec", "devices", "requests")
			if nestedErr != nil || !found || len(requests) != 1 {
				t.Fatalf("requests = %v (found=%t err=%v), want exactly one", requests, found, nestedErr)
			}
			request, ok := requests[0].(map[string]interface{})
			if !ok {
				t.Fatalf("request has unexpected type %T", requests[0])
			}
			if request[keyName] != gpuClaimName {
				t.Errorf("request name = %v, want %q", request[keyName], gpuClaimName)
			}
			exactly, hasExactly := request["exactly"].(map[string]interface{})
			if hasExactly != tt.wantExactly {
				t.Fatalf("request `exactly` wrapper present = %t, want %t (request: %v)", hasExactly, tt.wantExactly, request)
			}
			fields := request
			if tt.wantExactly {
				fields = exactly
			}
			if fields["deviceClassName"] != draDriverGPU ||
				fields["allocationMode"] != "ExactCount" || fields["count"] != int64(1) {

				t.Errorf("request detail = %v, want deviceClassName=%s allocationMode=ExactCount count=1",
					fields, draDriverGPU)
			}
		})
	}
}

// TestCreatePodWhenSAReady pins the ServiceAccount-race guard on direct pod
// creation in fresh per-run namespaces: the specific "serviceaccount not
// found" admission rejection is retried until the SA controller provisions
// `default`; other errors propagate immediately; a namespace whose SA never
// appears fails with ErrCodeTimeout after the bounded window.
func TestCreatePodWhenSAReady(t *testing.T) {
	oldTimeout := saProvisionTimeout
	saProvisionTimeout = 150 * time.Millisecond // short: the reactors below scale off it
	t.Cleanup(func() { saProvisionTimeout = oldTimeout })

	saMissingErr := func() error {
		return k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p",
			stderrors.New(`error looking up service account test-ns/default: serviceaccount "default" not found`))
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}

	t.Run("SA appears after two rejections: create succeeds", func(t *testing.T) {
		client := k8sfake.NewClientset()
		rejections := 0
		client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			if rejections < 2 {
				rejections++
				return true, nil, saMissingErr()
			}
			return false, nil, nil // fall through to the tracker: create succeeds
		})

		if err := createPodWhenSAReady(context.Background(), client, "test-ns", pod); err != nil {
			t.Fatalf("createPodWhenSAReady() error = %v, want success after SA provisioning", err)
		}
		if rejections != 2 {
			t.Errorf("rejections = %d, want 2 retried SA-admission failures", rejections)
		}
		if _, err := client.CoreV1().Pods("test-ns").Get(context.Background(), "p", metav1.GetOptions{}); err != nil {
			t.Errorf("pod not created after retries: %v", err)
		}
	})

	t.Run("SA never provisioned: ErrCodeTimeout", func(t *testing.T) {
		client := k8sfake.NewClientset()
		client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, saMissingErr()
		})

		err := createPodWhenSAReady(context.Background(), client, "test-ns", pod)
		if err == nil {
			t.Fatal("expected timeout when the default SA never appears")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error code = %v, want ErrCodeTimeout", err)
		}
		if !strings.Contains(err.Error(), "ServiceAccount was not provisioned") {
			t.Errorf("error = %v, want SA-provisioning message", err)
		}
	})

	t.Run("missing CUSTOM serviceaccount is NOT retried (config error, not the race)", func(t *testing.T) {
		client := k8sfake.NewClientset()
		calls := 0
		client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p",
				stderrors.New(`error looking up service account test-ns/custom-sa: serviceaccount "custom-sa" not found`))
		})

		err := createPodWhenSAReady(context.Background(), client, "test-ns", pod)
		if err == nil {
			t.Fatal("expected the missing-custom-SA error to propagate")
		}
		if calls != 1 {
			t.Errorf("create attempts = %d, want exactly 1 — only the DEFAULT SA race is retryable", calls)
		}
		if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error = %v — a custom-SA config error must not be classified as timeout", err)
		}
	})

	t.Run("window expires with NO SA rejection observed (apiserver stall): generic timeout, not the SA message", func(t *testing.T) {
		client := k8sfake.NewClientset()
		client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			// The fake ignores contexts, so emulate a stalled apiserver:
			// block past the (shortened) window, then fail the way an expired
			// context surfaces from client-go — no SA rejection ever seen.
			time.Sleep(3 * saProvisionTimeout)
			return true, nil, context.DeadlineExceeded
		})

		err := createPodWhenSAReady(context.Background(), client, "test-ns", pod)
		if err == nil {
			t.Fatal("expected the expired retry window to fail")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error code = %v, want ErrCodeTimeout", err)
		}
		if !strings.Contains(err.Error(), "timed out waiting on namespace provisioning") {
			t.Errorf("error = %v, want the generic bounded-create timeout", err)
		}
		if strings.Contains(err.Error(), "ServiceAccount was not provisioned") {
			t.Errorf("error = %v — claiming an SA race without an observed SA rejection is fabricated diagnosis", err)
		}
	})

	t.Run("window expires AFTER an SA rejection: the friendly SA-provisioning timeout", func(t *testing.T) {
		client := k8sfake.NewClientset()
		calls := 0
		client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls == 1 {
				return true, nil, saMissingErr() // one observed SA rejection...
			}
			time.Sleep(3 * saProvisionTimeout) // ...then the window expires mid-Create
			return true, nil, context.DeadlineExceeded
		})

		err := createPodWhenSAReady(context.Background(), client, "test-ns", pod)
		if err == nil {
			t.Fatal("expected the expired retry window to fail")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error code = %v, want ErrCodeTimeout", err)
		}
		if !strings.Contains(err.Error(), "ServiceAccount was not provisioned") {
			t.Errorf("error = %v, want the friendly SA-provisioning message (an SA rejection WAS observed)", err)
		}
	})

	t.Run("non-SA error propagates immediately without retry", func(t *testing.T) {
		client := k8sfake.NewClientset()
		calls := 0
		client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p",
				stderrors.New("rbac: user cannot create pods"))
		})

		err := createPodWhenSAReady(context.Background(), client, "test-ns", pod)
		if err == nil {
			t.Fatal("expected the RBAC error to propagate")
		}
		if calls != 1 {
			t.Errorf("create attempts = %d, want exactly 1 (no retry for non-SA errors)", calls)
		}
		if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error = %v — a non-SA error must not be classified as timeout", err)
		}
	})
}

// TestWaitForTerminalPod_WatchRestartBackoff pins the watch-restart backoff:
// an apiserver that accepts watches and closes them immediately must not turn
// the restart loop into a hot GET/WATCH spin — restarts are spaced by bounded
// exponential backoff (250ms base, doubling), so two forced closes cost at
// least ~750ms and only a handful of watch calls, not thousands.
func TestWaitForTerminalPod_WatchRestartBackoff(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "test-ns"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	client := k8sfake.NewClientset(pending)

	podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	watchCalls := 0
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		watchCalls++
		if watchCalls == 2 {
			// After the second forced close, let the next re-Get observe a
			// terminal pod so the wait completes.
			done := pending.DeepCopy()
			done.Status.Phase = corev1.PodSucceeded
			done.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  containerNameGPUTest,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}}
			if err := client.Tracker().Update(podsGVR, done, "test-ns"); err != nil {
				t.Errorf("tracker update: %v", err)
			}
		}
		fw := watch.NewFake()
		fw.Stop() // channel closed immediately — the hiccup under test
		return true, fw, nil
	})

	start := time.Now()
	pod, err := waitForTerminalPod(context.Background(), client, "test-ns", "p", "backoff test pod")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waitForTerminalPod() error = %v", err)
	}
	if pod.Status.Phase != corev1.PodSucceeded {
		t.Errorf("phase = %s, want Succeeded", pod.Status.Phase)
	}
	if watchCalls != 2 {
		t.Errorf("watch calls = %d, want exactly 2 — immediate closes must not spin the restart loop", watchCalls)
	}
	// Two restarts → backoffs of 250ms then 500ms before the terminal re-Get.
	if elapsed < 700*time.Millisecond {
		t.Errorf("elapsed = %v, want >= ~750ms of accumulated backoff between watch restarts", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("elapsed = %v — backoff should stay bounded", elapsed)
	}
}

// TestWaitForTerminalPod_WatchSetupErrorRetried pins the watch-SETUP retry
// (#1620 review): a transient error establishing the watch — e.g. 410
// ResourceExpired from a stale ResourceVersion, an apiserver timeout, or an
// LB drop — must get the same bounded backoff + re-Get treatment as a closed
// watch channel, not fail the check immediately.
func TestWaitForTerminalPod_WatchSetupErrorRetried(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "test-ns"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	client := k8sfake.NewClientset(pending)

	podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	watchCalls := 0
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		watchCalls++
		// Before failing the setup, make the pod terminal so the re-Get
		// after the backoff observes completion without a second watch.
		done := pending.DeepCopy()
		done.Status.Phase = corev1.PodSucceeded
		done.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  containerNameGPUTest,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}
		if err := client.Tracker().Update(podsGVR, done, "test-ns"); err != nil {
			t.Errorf("tracker update: %v", err)
		}
		return true, nil, k8serrors.NewResourceExpired("too old resource version")
	})

	pod, err := waitForTerminalPod(context.Background(), client, "test-ns", "p", "watch setup test pod")
	if err != nil {
		t.Fatalf("waitForTerminalPod() error = %v — a transient watch-setup failure must be retried, not returned", err)
	}
	if pod.Status.Phase != corev1.PodSucceeded {
		t.Errorf("phase = %s, want Succeeded", pod.Status.Phase)
	}
	if watchCalls != 1 {
		t.Errorf("watch calls = %d, want exactly 1 (setup failed once; re-Get completed the wait)", watchCalls)
	}
}

// TestWaitForTerminalPod_ResourceExpiredRefreshesFromList pins the 410
// recovery path (#1620 review round 2): an unchanged pod's OBJECT
// resourceVersion can sit outside the apiserver watch window indefinitely,
// so after a 410 the next watch must start from a fresh List's COLLECTION
// resourceVersion — proving a second watch can actually be established while
// the pod is still Pending, and that the wait then completes through that
// second watch's events.
func TestWaitForTerminalPod_ResourceExpiredRefreshesFromList(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "test-ns", ResourceVersion: "stale-7"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	client := k8sfake.NewClientset(pending)

	const freshRV = "fresh-42"
	listCalls := 0
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		listCalls++
		return true, &corev1.PodList{
			ListMeta: metav1.ListMeta{ResourceVersion: freshRV},
			Items:    []corev1.Pod{*pending.DeepCopy()}, // still Pending — no Get shortcut
		}, nil
	})

	watchCalls := 0
	var secondWatchRV string
	client.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		watchCalls++
		if watchCalls == 1 {
			return true, nil, k8serrors.NewResourceExpired("too old resource version")
		}
		if wa, ok := action.(k8stesting.WatchActionImpl); ok {
			secondWatchRV = wa.GetWatchRestrictions().ResourceVersion
		}
		done := pending.DeepCopy()
		done.Status.Phase = corev1.PodSucceeded
		done.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  containerNameGPUTest,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}
		fw := watch.NewFakeWithChanSize(1, false)
		fw.Modify(done) // completion arrives through the SECOND watch
		return true, fw, nil
	})

	pod, err := waitForTerminalPod(context.Background(), client, "test-ns", "p", "410 refresh test pod")
	if err != nil {
		t.Fatalf("waitForTerminalPod() error = %v", err)
	}
	if pod.Status.Phase != corev1.PodSucceeded {
		t.Errorf("phase = %s, want Succeeded (delivered by the second watch)", pod.Status.Phase)
	}
	if watchCalls != 2 {
		t.Errorf("watch calls = %d, want 2 (410, then re-established watch)", watchCalls)
	}
	if listCalls == 0 {
		t.Error("no pod List after 410 — the refresh must come from a collection resourceVersion")
	}
	if secondWatchRV != freshRV {
		t.Errorf("second watch resourceVersion = %q, want %q (the List's collection RV, not the pod's stale object RV)", secondWatchRV, freshRV)
	}
}

// TestWaitForTerminalPod_ForbiddenWatchFailsFast pins the permanent-error
// split (#1620 review round 2): an authorization failure establishing the
// watch is not transient — it must surface immediately with its real cause,
// not be retried until the deadline converts it into a timeout.
func TestWaitForTerminalPod_ForbiddenWatchFailsFast(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "test-ns"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	client := k8sfake.NewClientset(pending)

	watchCalls := 0
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		watchCalls++
		return true, nil, k8serrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "p", fmt.Errorf("RBAC: watch denied"))
	})

	_, err := waitForTerminalPod(context.Background(), client, "test-ns", "p", "forbidden watch test pod")
	if err == nil {
		t.Fatal("expected the Forbidden watch error to propagate")
	}
	if watchCalls != 1 {
		t.Errorf("watch calls = %d, want exactly 1 (permanent errors must not be retried)", watchCalls)
	}
	if !strings.Contains(err.Error(), "failed to watch") || !strings.Contains(err.Error(), "RBAC: watch denied") {
		t.Errorf("error = %v, want the watch failure with its RBAC cause", err)
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error = %v — a permanent watch failure must not be classified as timeout", err)
	}
}
