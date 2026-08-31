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
	"bytes"
	"context"
	stderrors "errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestEmitDiagnosticBlock(t *testing.T) {
	tests := []struct {
		name         string
		label        string
		block        string
		wantLines    int // number of "diagnostics" records emitted
		wantContains []string
	}{
		{
			name:         "multi-line block emits one record per line",
			label:        "worker diagnostics",
			block:        "line one\nline two\nline three",
			wantLines:    3,
			wantContains: []string{"line one", "line two", "line three", "worker diagnostics"},
		},
		{
			name:         "empty block emits a single (empty) marker",
			label:        "launcher logs",
			block:        "   \n  ",
			wantLines:    1,
			wantContains: []string{"(empty)", "launcher logs"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
			defer slog.SetDefault(prev)

			emitDiagnosticBlock(tt.label, tt.block)

			out := buf.String()
			if got := strings.Count(out, "msg=diagnostics"); got != tt.wantLines {
				t.Errorf("emitted %d diagnostic records, want %d\noutput:\n%s", got, tt.wantLines, out)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\noutput:\n%s", want, out)
				}
			}
		})
	}
}

func TestLauncherLogsUnavailable(t *testing.T) {
	tests := []struct {
		name string
		logs string
		want bool
	}{
		{"empty", "", true},
		{"whitespace only", "  \n\t ", true},
		{"kubelet GC placeholder", "unable to retrieve container logs for containerd://abc123", true},
		{"placeholder amid text", "line1\nunable to retrieve container logs for containerd://x\n", true},
		{"real logs", "NCCL INFO Bootstrap : Using eth0\nsome output", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := launcherLogsUnavailable(tt.logs); got != tt.want {
				t.Errorf("launcherLogsUnavailable(%q) = %v, want %v", tt.logs, got, tt.want)
			}
		})
	}
}

func TestLauncherTerminationTail(t *testing.T) {
	const ns = "aicr-test"
	pod := func(name string, cs []corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Status:     corev1.PodStatus{ContainerStatuses: cs},
		}
	}
	terminated := func(msg string) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}}
	}

	tests := []struct {
		name    string
		pods    []runtime.Object
		podName string
		want    string
	}{
		{
			name:    "returns trimmed terminated message of node container",
			pods:    []runtime.Object{pod("launcher-a", []corev1.ContainerStatus{{Name: nodeJobName, State: terminated("  mpirun: ORTE failed\n")}})},
			podName: "launcher-a",
			want:    "mpirun: ORTE failed",
		},
		{
			name:    "empty when node container still running",
			pods:    []runtime.Object{pod("launcher-b", []corev1.ContainerStatus{{Name: nodeJobName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}})},
			podName: "launcher-b",
			want:    "",
		},
		{
			name: "ignores non-node container messages",
			pods: []runtime.Object{pod("launcher-c", []corev1.ContainerStatus{
				{Name: "sidecar", State: terminated("sidecar noise")},
			})},
			podName: "launcher-c",
			want:    "",
		},
		{
			name:    "empty when pod missing",
			pods:    nil,
			podName: "nope",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.pods...)
			if got := launcherTerminationTail(context.Background(), client, ns, tt.podName); got != tt.want {
				t.Errorf("launcherTerminationTail = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTailLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"fewer than n", "a\nb", 5, "a\nb"},
		{"exactly n", "a\nb\nc", 3, "a\nb\nc"},
		{"more than n keeps tail", "a\nb\nc\nd", 2, "c\nd"},
		{"single line", "only", 3, "only"},
		{"empty", "", 3, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tailLines(tt.in, tt.n); got != tt.want {
				t.Errorf("tailLines(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestCollectNCCLWorkerDiagnostics(t *testing.T) {
	const ns = "aicr-test"

	workerLabels := map[string]string{
		"jobset.sigs.k8s.io/jobset-name":        ncclTrainJobName,
		"jobset.sigs.k8s.io/replicatedjob-name": nodeJobName,
	}

	failedWorker := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nccl-all-reduce-tj-node-0-0-abcde",
			Namespace: ns,
			Labels:    workerLabels,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			// tcpxo-daemon is a native sidecar (init container).
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "tcpxo-daemon",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Reason:   "Error",
						ExitCode: 137,
						Message:  "fastrak init failed",
					},
				},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: nodeJobName,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "CrashLoopBackOff",
						Message: "back-off restarting",
					},
				},
			}},
		},
	}

	tests := []struct {
		name         string
		pods         []runtime.Object
		wantContains []string
	}{
		{
			name:         "no worker pods",
			pods:         nil,
			wantContains: []string{"no NCCL worker pods found"},
		},
		{
			name: "worker with failed and waiting containers",
			pods: []runtime.Object{failedWorker},
			wantContains: []string{
				failedWorker.Name,
				"phase=Failed",
				"container tcpxo-daemon: terminated reason=Error exitCode=137",
				"fastrak init failed",
				"container node: waiting reason=CrashLoopBackOff",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.pods...)
			got := collectNCCLWorkerDiagnostics(context.Background(), client, ns)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostics missing %q\nfull output:\n%s", want, got)
				}
			}
		})
	}
}

// TestRunNCCLTrainJob_TrainerInstallFailureCleansUpNamespace is the regression
// guard for the namespace-cleanup defer's registration point: it must be
// registered right after ensureNamespace succeeds, not after
// ensureTrainerInstalled, or a Trainer-install failure returns before the
// defer is ever registered and leaks the per-run namespace forever.
func TestRunNCCLTrainJob_TrainerInstallFailureCleansUpNamespace(t *testing.T) {
	dynamicClient := newTrainerFakeClient(completeTrainerInstall()...)
	dynamicClient.PrependReactor("get", resourceCRDs, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})

	clientset := fake.NewClientset()
	// A real apiserver always stamps a UID on create. The fake tracker
	// doesn't, so mimic it here since cleanupNCCLResources now requires one
	// to authorize its delete (see testNamespaceUID).
	clientset.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if ns, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace); ok && ns.UID == "" {
			ns.UID = testNamespaceUID
		}
		return false, nil, nil
	})
	vctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: dynamicClient,
	}
	gpuConfig := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 4, TotalGPUCount: 8}

	_, err := runNCCLTrainJob(vctx, gpuConfig, "", "", variantDefault, fabricEFA, "")
	if err == nil {
		t.Fatal("expected an error from the failed Trainer install probe, got nil")
	}
	if gpuConfig.Namespace == "" {
		t.Fatal("gpuConfig.Namespace was never set; ensureNamespace apparently wasn't reached")
	}

	if _, getErr := clientset.CoreV1().Namespaces().Get(context.Background(), gpuConfig.Namespace, metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Errorf("namespace %q was not cleaned up after Trainer install failure: get err = %v", gpuConfig.Namespace, getErr)
	}
}

// TestNCCLRunNamespace_VariesByVariant is the regression guard for the
// finding that all three catalog checks (nccl-all-reduce-bw, -net, -nvls)
// share one AICR_RUN_ID within a single aicr validate invocation, so
// deriveRunID's suffix alone would give them the identical namespace name.
// Today that is safe because the checks run serially and cleanup fully
// drains each namespace before the next starts, but folding the variant
// into the name removes the dependency on that ordering, which pkg/validator
// has a TODO to parallelize.
func TestNCCLRunNamespace_VariesByVariant(t *testing.T) {
	t.Setenv("AICR_RUN_ID", "test-run-id")

	seen := map[string]ncclVariant{}
	for _, variant := range []ncclVariant{variantDefault, variantNET, variantNVLS} {
		ns := ncclRunNamespace(variant)
		if owner, ok := seen[ns]; ok {
			t.Fatalf("variant %q and %q derived the same namespace %q", owner, variant, ns)
		}
		seen[ns] = variant

		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			t.Errorf("namespace %q for variant %q is not a valid DNS-1123 label: %v", ns, variant, errs)
		}
	}
}

// TestPruneStaleNCCLNamespaces is the regression guard for the ownership
// finding. Name, age, and pod occupancy alone must never qualify a namespace
// for deletion. The prune must delete only namespaces that are all of:
// labeled as ours (see ensureNamespace's labels.ManagedBy stamp),
// name-matching, old enough to rule out an in-progress sibling variant, not
// already terminating, and not the namespace this run is about to use.
// unlabeledMatchingNS is the case that would have been wrongly deleted
// before the fix.
func TestPruneStaleNCCLNamespaces(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaults.NCCLStaleNamespacePruneAge))
	young := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	ownedLabels := map[string]string{labels.ManagedBy: labels.ValueValidator}

	staleNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-deadbeef", CreationTimestamp: old, Labels: ownedLabels,
	}}
	youngNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-abad1dea", CreationTimestamp: young, Labels: ownedLabels,
	}}
	currentNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-currentrun", CreationTimestamp: old, Labels: ownedLabels,
	}}
	terminatingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-cafef00d", CreationTimestamp: old, Labels: ownedLabels,
		DeletionTimestamp: &metav1.Time{Time: time.Now()}, Finalizers: []string{"kubernetes"},
	}}
	unrelatedNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "some-other-namespace", CreationTimestamp: old,
	}}
	// Matches the name prefix, is old enough, and has no live pod. Without
	// the ownership label check, the pre-fix prune would have deleted it.
	unlabeledMatchingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-notours", CreationTimestamp: old,
	}}
	liveAgedNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-livebeef", CreationTimestamp: old, Labels: ownedLabels,
	}}
	liveAgedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "launcher-0", Namespace: liveAgedNS.Name},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	client := fake.NewClientset(staleNS, youngNS, currentNS, terminatingNS, unrelatedNS,
		unlabeledMatchingNS, liveAgedNS, liveAgedPod)
	pruneStaleNCCLNamespaces(context.Background(), client, currentNS.Name)

	remaining, err := client.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list namespaces after sweep: %v", err)
	}
	names := map[string]bool{}
	for _, ns := range remaining.Items {
		names[ns.Name] = true
	}

	if names[staleNS.Name] {
		t.Errorf("expected stale namespace %q to be deleted", staleNS.Name)
	}
	for _, keep := range []string{youngNS.Name, currentNS.Name, terminatingNS.Name, unrelatedNS.Name,
		unlabeledMatchingNS.Name, liveAgedNS.Name} {
		if !names[keep] {
			t.Errorf("expected namespace %q to be left alone, but it was deleted", keep)
		}
	}
}

// TestWaitForPodByLabelSelector_IgnoresStaleDeletedLauncher is the regression
// guard for the finding that any watch event, including a Deleted event for a
// stale pod, was returned as-is. applyNCCLResources's TrainJob admission
// retry (see TrainJobAdmissionRetryTimeout) can recreate the launcher under
// the same label selector: the stale launcher's Deleted event must be
// skipped so the wait continues until the replacement's Added event arrives.
func TestWaitForPodByLabelSelector_IgnoresStaleDeletedLauncher(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	const selector = "jobset.sigs.k8s.io/jobset-name=nccl-all-reduce-tj,jobset.sigs.k8s.io/replicatedjob-name=launcher"

	clientset := fake.NewClientset()
	fakeWatch := watch.NewFake()
	clientset.PrependWatchReactor("pods", k8stesting.DefaultWatchReactor(fakeWatch, nil))

	staleLauncher := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nccl-all-reduce-tj-launcher-0", Namespace: ns,
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
			Finalizers:        []string{"kubernetes"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	replacementLauncher := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nccl-all-reduce-tj-launcher-1", Namespace: ns},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}

	type waitResult struct {
		pod *corev1.Pod
		err error
	}
	resultCh := make(chan waitResult, 1)
	go func() {
		pod, err := waitForPodByLabelSelector(context.Background(), clientset, ns, selector, 5*time.Second)
		resultCh <- waitResult{pod, err}
	}()

	// Give the goroutine above time to establish its watch before pushing
	// events into it.
	time.Sleep(50 * time.Millisecond)
	fakeWatch.Delete(staleLauncher)
	fakeWatch.Add(replacementLauncher)

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("waitForPodByLabelSelector failed: %v", res.err)
		}
		if res.pod.Name != replacementLauncher.Name {
			t.Fatalf("expected the replacement launcher %q, got %q", replacementLauncher.Name, res.pod.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForPodByLabelSelector did not return after the replacement launcher appeared")
	}
}

// TestCleanupNCCLRun_DeletesNamespaceBeforeTrainer is the regression guard for
// the reversed-defer-order finding on the self-install fallback path
// (installedResources non-empty): deleteTrainer removes the Trainer
// controller and its TrainJob/TrainingRuntime CRDs, so running it before the
// namespace's own TrainJob/TrainingRuntime CRs are deleted can leave those
// CRs' controller-serviced finalizers stuck forever. cleanupNCCLRun must
// always delete the namespace first.
func TestCleanupNCCLRun_DeletesNamespaceBeforeTrainer(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	clientset := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}})
	dynamicClient := newTrainerFakeClient()

	var order []string
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "namespace")
		return false, nil, nil // let the default reactor perform the actual delete too.
	})
	dynamicClient.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "trainer")
		return false, nil, nil
	})

	resources := []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment},
	}

	if err := cleanupNCCLRun(clientset, dynamicClient, ns, testNamespaceUID, resources, nil); err != nil {
		t.Fatalf("cleanupNCCLRun failed: %v", err)
	}

	if len(order) != 2 || order[0] != "namespace" || order[1] != "trainer" {
		t.Fatalf("expected namespace delete before trainer delete, got order %v", order)
	}
}

// TestCleanupNCCLRun_PropagatesTrainerCleanupFailure verifies a Trainer
// teardown failure on the self-install fallback path still fails the check,
// not just the namespace half of cleanup.
func TestCleanupNCCLRun_PropagatesTrainerCleanupFailure(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	clientset := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}})
	dynamicClient := newTrainerFakeClient()
	dynamicClient.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, trainerControllerDeployment, nil)
	})

	resources := []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment},
	}

	err := cleanupNCCLRun(clientset, dynamicClient, ns, testNamespaceUID, resources, nil)
	if err == nil {
		t.Fatal("expected the Trainer teardown failure to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), trainerControllerDeployment) {
		t.Errorf("expected error to name the failed resource %q, got: %v", trainerControllerDeployment, err)
	}
}

// TestVerifyNCCLNamespaceNotLive covers the ownership gate that decides
// whether an already-existing per-run namespace is safe to adopt. Regression
// guard for the MAJOR finding that silently reusing any active namespace let
// a retry collide with (and later delete) a still-live execution's
// resources.
func TestVerifyNCCLNamespaceNotLive(t *testing.T) {
	const ns = "aicr-nccl-bench-deadbeef"
	terminatingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:              ns,
		DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
		Finalizers:        []string{"kubernetes"},
	}}
	activeNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	livePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "launcher-0", Namespace: ns},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	terminalPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "launcher-0", Namespace: ns},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}

	tests := []struct {
		name    string
		objs    []runtime.Object
		wantErr bool
	}{
		{name: "namespace does not exist yet", objs: nil, wantErr: false},
		{name: "namespace terminating from a prior cleanup", objs: []runtime.Object{terminatingNS}, wantErr: false},
		{name: "namespace active with no pods (empty stale leftover)", objs: []runtime.Object{activeNS}, wantErr: false},
		{
			name:    "namespace active with only terminal pods (stale same-run resources)",
			objs:    []runtime.Object{activeNS, terminalPod},
			wantErr: false,
		},
		{
			name:    "namespace active with a live pod (foreign/concurrent execution)",
			objs:    []runtime.Object{activeNS, livePod},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.objs...)
			err := verifyNCCLNamespaceNotLive(context.Background(), client, ns)
			if (err != nil) != tt.wantErr {
				t.Errorf("verifyNCCLNamespaceNotLive() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
				t.Errorf("expected ErrCodeConflict, got %v", err)
			}
		})
	}
}

// TestRunNCCLTrainJob_RefusesLiveForeignNamespace is the end-to-end
// regression guard for the same MAJOR finding: a retry with the same
// AICR_RUN_ID (or a rare random-suffix collision) must not silently adopt,
// and must never let its deferred cleanup delete, a namespace a different,
// still-running execution owns.
func TestRunNCCLTrainJob_RefusesLiveForeignNamespace(t *testing.T) {
	t.Setenv("AICR_RUN_ID", "test-run-id")
	ns := ncclRunNamespace(variantDefault)

	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "launcher-0", Namespace: ns},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	vctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newTrainerFakeClient(),
	}
	gpuConfig := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 4, TotalGPUCount: 8}

	_, err := runNCCLTrainJob(vctx, gpuConfig, "", "", variantDefault, fabricEFA, "")
	if err == nil {
		t.Fatal("expected a conflict error for a live foreign namespace, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
		t.Errorf("expected ErrCodeConflict, got %v", err)
	}

	nsAfter, getErr := clientset.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("foreign namespace was removed (or errored reading it back) instead of left alone: %v", getErr)
	}
	if nsAfter.DeletionTimestamp != nil {
		t.Error("foreign namespace was marked for deletion; cleanup must never touch a namespace it doesn't own")
	}
	if _, getErr := clientset.CoreV1().Pods(ns).Get(context.Background(), "launcher-0", metav1.GetOptions{}); getErr != nil {
		t.Errorf("foreign execution's live pod was removed: %v", getErr)
	}
}

// TestCreateUnstructured_ReclaimsStaleResource_UpdatesInPlace is the
// regression guard for the MAJOR finding that a same-run retry could hit
// AlreadyExists on a stale fixed-name TrainingRuntime instead of recovering.
func TestCreateUnstructured_ReclaimsStaleResource_UpdatesInPlace(t *testing.T) {
	const ns = "aicr-nccl-bench-deadbeef"
	listKinds := map[schema.GroupVersionResource]string{trainingRuntimeGVR: "TrainingRuntimeList"}

	stale := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": trainingRuntimeGVR.GroupVersion().String(),
		"kind":       "TrainingRuntime",
		"metadata": map[string]interface{}{
			"name":            ncclTrainingRuntimeName,
			"namespace":       ns,
			"resourceVersion": "1",
		},
		"spec": map[string]interface{}{"stale": true},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, stale)

	fresh := stale.DeepCopy()
	fresh.SetResourceVersion("")
	if err := unstructured.SetNestedField(fresh.Object, false, "spec", "stale"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}

	if err := createUnstructured(context.Background(), dynamicClient, trainingRuntimeGVR, ns, fresh); err != nil {
		t.Fatalf("createUnstructured() on an AlreadyExists fixed-name resource = %v, want reclaim via update", err)
	}

	got, err := dynamicClient.Resource(trainingRuntimeGVR).Namespace(ns).Get(context.Background(), ncclTrainingRuntimeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() after reclaim: %v", err)
	}
	if staleVal, _, _ := unstructured.NestedBool(got.Object, "spec", "stale"); staleVal {
		t.Error("reclaimed resource still has the stale prior-run spec, update did not apply")
	}
}

// TestCreateUnstructured_ReclaimsStaleTrainJob_DeletesAndRecreates is the
// regression guard for the finding that reclaiming a stale TrainJob by
// updating it in place does not actually recover a same-run retry. Kubeflow
// Trainer treats most of the TrainJob spec as immutable once created and
// rejects an in-place update to those fields, and even a permitted update
// would not make the controller recreate the underlying JobSet/pods a
// hard-killed run left behind. TrainJob must be reclaimed by delete then
// recreate instead.
func TestCreateUnstructured_ReclaimsStaleTrainJob_DeletesAndRecreates(t *testing.T) {
	const ns = "aicr-nccl-bench-deadbeef"
	listKinds := map[schema.GroupVersionResource]string{trainJobGVR: "TrainJobList"}

	stale := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": trainJobGVR.GroupVersion().String(),
		"kind":       "TrainJob",
		"metadata": map[string]interface{}{
			"name":            ncclTrainJobName,
			"namespace":       ns,
			"resourceVersion": "1",
		},
		"spec": map[string]interface{}{"stale": true},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, stale)

	var order []string
	dynamicClient.PrependReactor("delete", "trainjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "delete")
		return false, nil, nil // let the default reactor perform the real delete too.
	})
	dynamicClient.PrependReactor("create", "trainjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "create")
		return false, nil, nil
	})

	fresh := stale.DeepCopy()
	fresh.SetResourceVersion("")
	if err := unstructured.SetNestedField(fresh.Object, false, "spec", "stale"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}

	if err := createUnstructured(context.Background(), dynamicClient, trainJobGVR, ns, fresh); err != nil {
		t.Fatalf("createUnstructured() on an AlreadyExists TrainJob = %v, want reclaim via delete-then-recreate", err)
	}

	// The first "create" is createUnstructured's own initial attempt, which
	// hits AlreadyExists and triggers the delete-then-recreate path below.
	if want := []string{"create", "delete", "create"}; len(order) != len(want) || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("expected the stale TrainJob to be deleted then recreated, got call order %v, want %v", order, want)
	}

	got, err := dynamicClient.Resource(trainJobGVR).Namespace(ns).Get(context.Background(), ncclTrainJobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() after reclaim: %v", err)
	}
	if staleVal, _, _ := unstructured.NestedBool(got.Object, "spec", "stale"); staleVal {
		t.Error("reclaimed TrainJob still has the stale prior-run spec, recreate did not apply")
	}
}

// countDeleteActions counts "delete" actions recorded for the given
// resource. Actions() is the fake client's own call history, so this reads
// real counts without a separate counter.
func countDeleteActions(actions []k8stesting.Action, resource string) int {
	n := 0
	for _, a := range actions {
		if a.GetVerb() == "delete" && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

// TestCreateUnstructured_WaitsForFinalizerHeldTrainJobBeforeRecreate is the
// regression guard for the finalizer-race finding. Delete only stamps
// DeletionTimestamp while a Trainer v2 / JobSet ownership finalizer is still
// clearing, so an immediate Create would hit AlreadyExists again. Before the
// fix, createUnstructured issued Create immediately after Delete with no
// wait in between.
func TestCreateUnstructured_WaitsForFinalizerHeldTrainJobBeforeRecreate(t *testing.T) {
	const ns = "aicr-nccl-bench-deadbeef"
	const holdFinalizer = 200 * time.Millisecond
	listKinds := map[schema.GroupVersionResource]string{trainJobGVR: "TrainJobList"}

	stale := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": trainJobGVR.GroupVersion().String(),
		"kind":       "TrainJob",
		"metadata": map[string]interface{}{
			"name":            ncclTrainJobName,
			"namespace":       ns,
			"resourceVersion": "1",
		},
		"spec": map[string]interface{}{"stale": true},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, stale)

	dynamicClient.PrependReactor("delete", "trainjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		// Branch on the object's own DeletionTimestamp, not an invocation
		// counter, matching how a real apiserver decides. This also avoids
		// calling Actions() from inside a reactor, which deadlocks. Invokes
		// holds the Fake's write lock for the whole reactor chain, and
		// Actions() takes that same lock to read.
		existing, getErr := dynamicClient.Tracker().Get(trainJobGVR, ns, ncclTrainJobName)
		obj, ok := existing.(*unstructured.Unstructured)
		if getErr == nil && ok && obj.GetDeletionTimestamp() == nil {
			// On the first delete, simulate a still-cascading finalizer by
			// stamping DeletionTimestamp via the tracker instead of
			// actually removing the object, then accept the request
			// (handled=true, err=nil) as a real apiserver would.
			held := obj.DeepCopy()
			held.SetFinalizers([]string{"trainer.kubeflow.org/finalizer"})
			now := metav1.Now()
			held.SetDeletionTimestamp(&now)
			if err := dynamicClient.Tracker().Update(trainJobGVR, held, ns); err != nil {
				return true, nil, err
			}
			return true, nil, nil
		}
		// Already marked for deletion. The goroutine below fires this once
		// the "finalizer" clears, so let the default reactor delete it for
		// real, which emits the watch.Deleted event waitForResourceGone is
		// blocked on.
		return false, nil, nil
	})

	// Captured before the goroutine launches, so elapsed below can't dip
	// under holdFinalizer merely because the goroutine's sleep started
	// first.
	start := time.Now()
	go func() {
		time.Sleep(holdFinalizer)
		_ = dynamicClient.Resource(trainJobGVR).Namespace(ns).Delete(context.Background(), ncclTrainJobName, metav1.DeleteOptions{})
	}()

	fresh := stale.DeepCopy()
	fresh.SetResourceVersion("")
	if err := unstructured.SetNestedField(fresh.Object, false, "spec", "stale"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}

	if err := createUnstructured(context.Background(), dynamicClient, trainJobGVR, ns, fresh); err != nil {
		t.Fatalf("createUnstructured() should succeed once the finalizer clears, got: %v", err)
	}
	elapsed := time.Since(start)

	// A delete count of 2 alone doesn't prove the wait blocked. The
	// goroutine above fires the second delete unconditionally at
	// t=holdFinalizer regardless of what createUnstructured does. The
	// elapsed-time check below is the real guard. A pre-fix immediate
	// recreate would hit AlreadyExists and fail before that goroutine ever
	// ran, with the delete count still at 1.
	if got := countDeleteActions(dynamicClient.Actions(), "trainjobs"); got < 2 {
		t.Fatalf("expected createUnstructured to observe the stale TrainJob still present and wait for a second delete, got %d delete call(s)", got)
	}
	if elapsed < holdFinalizer {
		t.Errorf("createUnstructured recreated after %v, want it to have blocked at least %v for the finalizer to clear", elapsed, holdFinalizer)
	}
}
