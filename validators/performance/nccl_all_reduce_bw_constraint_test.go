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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
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
// guard for the namespace-cleanup defer's registration point. It must be
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

// TestRunNCCLTrainJob_AbortsIfExecutionLockLostBeforeApply checks that if
// another caller takes the execution lock over while Trainer install is
// running, runNCCLTrainJob aborts instead of applying resources under a
// lock it no longer holds. The reactor takes the lock over on the first
// Lease read after the initial claim.
func TestRunNCCLTrainJob_AbortsIfExecutionLockLostBeforeApply(t *testing.T) {
	dynamicClient := newTrainerFakeClient(completeTrainerInstall()...)
	runtimeApplied := false
	dynamicClient.PrependReactor("create", "trainingruntimes", func(k8stesting.Action) (bool, runtime.Object, error) {
		runtimeApplied = true
		return false, nil, nil
	})
	clientset := fake.NewClientset()
	clientset.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if ns, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace); ok && ns.UID == "" {
			ns.UID = testNamespaceUID
		}
		return false, nil, nil
	})

	leaseGVR := coordinationv1.SchemeGroupVersion.WithResource("leases")
	var takenOver bool
	clientset.PrependReactor("get", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if takenOver {
			return false, nil, nil
		}
		takenOver = true
		rivalHolder := "rival-holder"
		renew := metav1.NewMicroTime(time.Now())
		rival := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: action.GetNamespace(), ResourceVersion: "999"},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &rivalHolder, RenewTime: &renew},
		}
		if err := clientset.Tracker().Update(leaseGVR, rival, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return false, nil, nil // let the default reactor return the now-updated (rival) object
	})

	vctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: dynamicClient,
	}
	gpuConfig := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 4, TotalGPUCount: 8}

	_, err := runNCCLTrainJob(vctx, gpuConfig, "", "", variantDefault, fabricEFA, "")
	if err == nil {
		t.Fatal("expected a conflict error when the execution lock was taken over before apply, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
		t.Errorf("expected ErrCodeConflict, got %v", err)
	}
	if runtimeApplied {
		t.Error("expected no TrainingRuntime to be applied after losing the execution lock")
	}
}

// TestRunNCCLTrainJob_RollsBackNamespaceOnLeaseAdmissionFailure checks that
// a namespace ensureNamespace just created is deleted again if claiming the
// execution lock fails for a reason other than a concurrent caller already
// holding it, so a standalone run doesn't leak a namespace on every retry.
func TestRunNCCLTrainJob_RollsBackNamespaceOnLeaseAdmissionFailure(t *testing.T) {
	clientset := fake.NewClientset()
	clientset.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if ns, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace); ok && ns.UID == "" {
			ns.UID = testNamespaceUID
		}
		return false, nil, nil
	})
	clientset.PrependReactor("create", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
			ncclRunLockName, nil)
	})

	vctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newTrainerFakeClient(),
	}
	gpuConfig := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 4, TotalGPUCount: 8}

	_, err := runNCCLTrainJob(vctx, gpuConfig, "", "", variantDefault, fabricEFA, "")
	if err == nil {
		t.Fatal("expected an error from the failed Lease claim, got nil")
	}
	if gpuConfig.Namespace == "" {
		t.Fatal("gpuConfig.Namespace was never set; ensureNamespace apparently wasn't reached")
	}
	if _, getErr := clientset.CoreV1().Namespaces().Get(context.Background(), gpuConfig.Namespace, metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Errorf("namespace %q was not rolled back after the Lease claim failed: get err = %v", gpuConfig.Namespace, getErr)
	}
}

// TestRunNCCLTrainJob_KeepsReusedNamespaceOnLeaseAdmissionFailure checks
// that the rollback added for a failed Lease claim does not fire for a
// namespace ensureNamespace reused rather than created, since it may hold
// another execution's leftovers worth reclaiming on a later retry.
func TestRunNCCLTrainJob_KeepsReusedNamespaceOnLeaseAdmissionFailure(t *testing.T) {
	t.Setenv("AICR_RUN_ID", "test-run-id")
	ns := ncclRunNamespace(variantDefault)

	clientset := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID, Labels: map[string]string{
		labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf,
	}}})
	clientset.PrependReactor("create", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
			ncclRunLockName, nil)
	})

	vctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newTrainerFakeClient(),
	}
	gpuConfig := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 4, TotalGPUCount: 8}

	if _, err := runNCCLTrainJob(vctx, gpuConfig, "", "", variantDefault, fabricEFA, ""); err == nil {
		t.Fatal("expected an error from the failed Lease claim, got nil")
	}
	if _, getErr := clientset.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); getErr != nil {
		t.Errorf("reused namespace was rolled back after an unrelated Lease claim failure: %v", getErr)
	}
}

// TestRunNCCLTrainJob_KeepsNamespaceOnConcurrentClaimConflict checks that
// the rollback added for a failed Lease claim does not fire when the claim
// failed because a concurrent caller already holds the lock, since that
// namespace belongs to them, not to this caller.
func TestRunNCCLTrainJob_KeepsNamespaceOnConcurrentClaimConflict(t *testing.T) {
	t.Setenv("AICR_RUN_ID", "test-run-id")
	ns := ncclRunNamespace(variantDefault)
	concurrentHolder := "concurrent-holder"
	liveRenew := metav1.NewMicroTime(time.Now())
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID, Labels: map[string]string{
			labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf,
		}}},
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &concurrentHolder, RenewTime: &liveRenew},
		},
	)
	vctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newTrainerFakeClient(),
	}
	gpuConfig := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 4, TotalGPUCount: 8}

	_, err := runNCCLTrainJob(vctx, gpuConfig, "", "", variantDefault, fabricEFA, "")
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
		t.Fatalf("expected ErrCodeConflict, got %v", err)
	}
	if _, getErr := clientset.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); getErr != nil {
		t.Errorf("namespace was rolled back even though a concurrent caller holds the lock: %v", getErr)
	}
}

// TestNCCLRunNamespace_VariesByVariant is the regression guard for the
// finding that all three catalog checks share one AICR_RUN_ID per
// invocation, so deriveRunID's suffix alone would give them the identical
// namespace name. Folding the variant into the name keeps them distinct.
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
// finding. Only a namespace labeled ours, old enough to rule out an
// in-progress sibling variant, not already terminating, and not the one
// this run is about to use may be deleted. unlabeledMatchingNS and
// otherComponentNS are the cases that would have been wrongly deleted
// before the fix.
func TestPruneStaleNCCLNamespaces(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaults.NCCLStaleNamespacePruneAge))
	young := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	ownedLabels := map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf}

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
	// Also matches the name prefix and is AICR-managed, but for a different
	// component. Proves the List selector, not name shape, decides scope:
	// the Go-side loop no longer filters by prefix at all.
	otherComponentNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-notmine", CreationTimestamp: old,
		Labels: map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueInferencePerf},
	}}
	liveAgedNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-livebeef", CreationTimestamp: old, Labels: ownedLabels,
	}}
	liveAgedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "launcher-0", Namespace: liveAgedNS.Name},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// Aged past the prune age and podless, but admitted moments ago, still
	// installing Trainer or applying resources.
	admittedPodlessNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-admitted", CreationTimestamp: old, Labels: ownedLabels,
	}}
	admittedHolder := "admitted-holder"
	freshRenew := metav1.NewMicroTime(time.Now())
	admittedLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: admittedPodlessNS.Name},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &admittedHolder, RenewTime: &freshRenew},
	}

	client := fake.NewClientset(staleNS, youngNS, currentNS, terminatingNS, unrelatedNS,
		unlabeledMatchingNS, otherComponentNS, liveAgedNS, liveAgedPod, admittedPodlessNS, admittedLease)
	pruneStaleNCCLNamespaces(context.Background(), client, newTrainerFakeClient(), currentNS.Name)

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
		unlabeledMatchingNS.Name, otherComponentNS.Name, liveAgedNS.Name, admittedPodlessNS.Name} {
		if !names[keep] {
			t.Errorf("expected namespace %q to be left alone, but it was deleted", keep)
		}
	}
}

// TestPruneStaleNCCLNamespaces_TerminatingNamespaceBlocksTrainerReap checks
// that a namespace still cascading its own deletion holds back Trainer
// reaping. Its TrainJob/TrainingRuntime CRs still need the Trainer
// controller alive to clear their finalizers.
func TestPruneStaleNCCLNamespaces_TerminatingNamespaceBlocksTrainerReap(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaults.NCCLStaleNamespacePruneAge))
	terminatingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-cafef00d", CreationTimestamp: old,
		Labels:            map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf},
		DeletionTimestamp: &metav1.Time{Time: time.Now()}, Finalizers: []string{"kubernetes"},
	}}

	client := fake.NewClientset(terminatingNS)
	persistTrainerInstallManifest(context.Background(), client, []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment},
	})
	backdateTrainerInstallManifest(t, client)

	pruneStaleNCCLNamespaces(context.Background(), client, newTrainerFakeClient(), "aicr-nccl-perf-default-currentrun")

	if _, resources, _ := loadTrainerInstallManifest(context.Background(), client); resources == nil {
		t.Error("expected the Trainer install manifest to survive while a namespace is still terminating")
	}
}

// TestPruneStaleNCCLNamespaces_OwnDeleteBlocksTrainerReap checks that a
// namespace this sweep just deleted also holds back Trainer reaping in the
// same pass. The delete only starts the cascade, and the accepted
// namespace's TrainJob/TrainingRuntime CRs still need Trainer alive to
// clear their finalizers.
func TestPruneStaleNCCLNamespaces_OwnDeleteBlocksTrainerReap(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaults.NCCLStaleNamespacePruneAge))
	staleNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-deadbeef", CreationTimestamp: old,
		Labels: map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf},
	}}

	client := fake.NewClientset(staleNS)
	persistTrainerInstallManifest(context.Background(), client, []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment},
	})
	backdateTrainerInstallManifest(t, client)

	pruneStaleNCCLNamespaces(context.Background(), client, newTrainerFakeClient(), "aicr-nccl-perf-default-currentrun")

	if _, err := client.CoreV1().Namespaces().Get(context.Background(), staleNS.Name, metav1.GetOptions{}); err == nil {
		t.Fatalf("expected %q to be deleted by this sweep", staleNS.Name)
	}
	if _, resources, _ := loadTrainerInstallManifest(context.Background(), client); resources == nil {
		t.Error("expected the Trainer install manifest to survive: the delete only started the cascade")
	}
}

// TestPruneStaleNCCLNamespaces_LiveCurrentNamespaceBlocksTrainerReap checks
// that a live current namespace blocks Trainer reaping, even though prune's
// delete path never touches it. A retry sharing the same deterministic
// namespace name as a still-running peer must not reap Trainer out from
// under that peer's in-flight TrainJob.
func TestPruneStaleNCCLNamespaces_LiveCurrentNamespaceBlocksTrainerReap(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaults.NCCLStaleNamespacePruneAge))
	currentNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-currentrun", CreationTimestamp: old,
		Labels: map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf},
	}}
	livePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "launcher-0", Namespace: currentNS.Name},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	client := fake.NewClientset(currentNS, livePod)
	persistTrainerInstallManifest(context.Background(), client, []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment},
	})
	backdateTrainerInstallManifest(t, client)

	pruneStaleNCCLNamespaces(context.Background(), client, newTrainerFakeClient(), currentNS.Name)

	if _, resources, _ := loadTrainerInstallManifest(context.Background(), client); resources == nil {
		t.Error("expected the Trainer install manifest to survive while the current namespace is live")
	}
}

// TestPruneStaleNCCLNamespaces_SkipsDeleteOnConcurrentClaim checks that
// prune backs off if a caller claims the namespace's lock in the instant
// between prune's own liveness check and its delete, instead of deleting
// out from under a claim that didn't exist yet when prune first looked.
func TestPruneStaleNCCLNamespaces_SkipsDeleteOnConcurrentClaim(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaults.NCCLStaleNamespacePruneAge))
	ownedLabels := map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf}
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-raced", CreationTimestamp: old, Labels: ownedLabels,
	}}

	client := fake.NewClientset(targetNS)
	client.PrependReactor("create", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// A caller wins the claim moments after prune's own check found no
		// lease at all, racing prune's own claim attempt on the same Create.
		rivalHolder := "rival-holder"
		renew := metav1.NewMicroTime(time.Now())
		rival := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: action.GetNamespace()},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &rivalHolder, RenewTime: &renew},
		}
		if err := client.Tracker().Add(rival); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, ncclRunLockName)
	})

	pruneStaleNCCLNamespaces(context.Background(), client, newTrainerFakeClient(), "some-other-namespace")

	if _, err := client.CoreV1().Namespaces().Get(context.Background(), targetNS.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("namespace was deleted despite a concurrent caller claiming its lock first: %v", err)
	}
}

// TestPruneStaleNCCLNamespaces_ReleasesLockOnFailedDelete checks that a
// transient namespace-delete failure releases the fence Lease prune just
// claimed, instead of leaving it to block a same-run retry closed until it
// ages out on its own.
func TestPruneStaleNCCLNamespaces_ReleasesLockOnFailedDelete(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaults.NCCLStaleNamespacePruneAge))
	ownedLabels := map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf}
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-flaky", CreationTimestamp: old, Labels: ownedLabels,
	}}

	client := fake.NewClientset(targetNS)
	client.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
	})

	pruneStaleNCCLNamespaces(context.Background(), client, newTrainerFakeClient(), "some-other-namespace")

	if _, err := client.CoreV1().Namespaces().Get(context.Background(), targetNS.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("expected the namespace to survive a failed delete: %v", err)
	}
	if _, err := client.CoordinationV1().Leases(targetNS.Name).Get(context.Background(), ncclRunLockName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected the fence Lease to be released after the failed delete, got: %v", err)
	}

	// A same-run retry must be able to claim it immediately, not fail
	// closed until NCCLExecutionLockStaleAge passes.
	if _, err := claimNCCLExecutionLock(context.Background(), client, targetNS.Name); err != nil {
		t.Errorf("expected a retry to claim the released lock immediately, got: %v", err)
	}
}

// TestPruneStaleNCCLNamespaces_StopsOnContextCancelMidSweep checks that a
// context canceled partway through the sweep stops it from issuing calls
// for the remaining namespaces, instead of continuing regardless.
func TestPruneStaleNCCLNamespaces_StopsOnContextCancelMidSweep(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * defaults.NCCLStaleNamespacePruneAge))
	ownedLabels := map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf}
	nsA := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-aaaaaaaa", CreationTimestamp: old, Labels: ownedLabels,
	}}
	nsB := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "aicr-nccl-perf-default-bbbbbbbb", CreationTimestamp: old, Labels: ownedLabels,
	}}

	client := fake.NewClientset(nsA, nsB)
	ctx, cancel := context.WithCancel(context.Background())
	var podListCalls, nsDeleteCalls int
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		podListCalls++
		if podListCalls == 1 {
			// Cancel partway through processing the first namespace, before
			// the loop reaches the second.
			cancel()
		}
		return false, nil, nil
	})
	client.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		nsDeleteCalls++
		return false, nil, nil
	})

	pruneStaleNCCLNamespaces(ctx, client, newTrainerFakeClient(), "aicr-nccl-perf-default-currentrun")

	if podListCalls != 1 {
		t.Errorf("expected the sweep to stop after checking one namespace's pods, checked %d", podListCalls)
	}
	if nsDeleteCalls != 1 {
		t.Errorf("expected exactly one namespace deleted before the canceled context stopped the sweep, got %d", nsDeleteCalls)
	}
}

// TestWaitForPodByLabelSelector_IgnoresStaleDeletedLauncher is the regression
// guard for the finding that any watch event, including a Deleted event for a
// stale pod, was returned as-is. applyNCCLResources's TrainJob admission
// retry (see TrainJobAdmissionRetryTimeout) can recreate the launcher under
// the same label selector, and the stale launcher's Deleted event must be
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

// TestWaitForLauncherPodAndGetLogs_TakenOverLockFailsClosed checks that a
// lock lost while the pod ran is caught right after it goes terminal,
// instead of being left until cleanup runs minutes later. A live pod
// already blocks a same-run retry on its own, but that protection ends
// once the pod finishes, so ownership needs revalidating here.
func TestWaitForLauncherPodAndGetLogs_TakenOverLockFailsClosed(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	launcherLabels := map[string]string{
		"jobset.sigs.k8s.io/jobset-name":        "nccl-all-reduce-tj",
		"jobset.sigs.k8s.io/replicatedjob-name": "launcher",
	}
	launcherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nccl-all-reduce-tj-launcher-0", Namespace: ns, Labels: launcherLabels},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}

	otherHolder := "some-other-holder"
	clientset := fake.NewClientset(launcherPod, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &otherHolder},
	})
	fakeWatch := watch.NewFake()
	clientset.PrependWatchReactor("pods", k8stesting.DefaultWatchReactor(fakeWatch, nil))

	// Flip the tracked pod to Succeeded up front, since
	// waitForPodByLabelSelector only reads phase off the watch event below.
	// WaitForPodSuccess re-Gets by name afterward and sees this update
	// directly, no second watch needed.
	succeeded := launcherPod.DeepCopy()
	succeeded.Status.Phase = corev1.PodSucceeded
	if _, err := clientset.CoreV1().Pods(ns).Update(context.Background(), succeeded, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to mark launcher pod succeeded: %v", err)
	}

	podHelper := &helper.PodLifecycle{ClientSet: clientset, Namespace: ns}
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	type waitResult struct {
		logs string
		err  error
	}
	resultCh := make(chan waitResult, 1)
	go func() {
		logs, err := waitForLauncherPodAndGetLogs(ctx, podHelper, testHolderID)
		resultCh <- waitResult{logs, err}
	}()

	// Give the goroutine above time to establish its watch, then hand it the
	// launcher pod. Its phase here only needs to pass the non-terminal
	// filter, the state that actually matters was already set above.
	time.Sleep(50 * time.Millisecond)
	fakeWatch.Add(launcherPod)

	select {
	case res := <-resultCh:
		if res.logs != "" {
			t.Errorf("expected no logs once the lock was taken over, got %q", res.logs)
		}
		if !stderrors.Is(res.err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
			t.Errorf("expected ErrCodeConflict for a taken-over lock, got: %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForLauncherPodAndGetLogs did not return")
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
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}},
		testHeldLease(ns),
	)
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

	if err := cleanupNCCLRun(clientset, dynamicClient, ns, testNamespaceUID, testHolderID, resources, nil); err != nil {
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
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}},
		testHeldLease(ns),
	)
	dynamicClient := newTrainerFakeClient()
	dynamicClient.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, trainerControllerDeployment, nil)
	})

	resources := []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment},
	}

	err := cleanupNCCLRun(clientset, dynamicClient, ns, testNamespaceUID, testHolderID, resources, nil)
	if err == nil {
		t.Fatal("expected the Trainer teardown failure to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), trainerControllerDeployment) {
		t.Errorf("expected error to name the failed resource %q, got: %v", trainerControllerDeployment, err)
	}
}

// TestCleanupNCCLRun_SkipsTrainerTeardownOnNamespaceDeleteFailure verifies
// that cleanupNCCLRun skips Trainer teardown, and fails the check, when the
// namespace delete itself fails.
func TestCleanupNCCLRun_SkipsTrainerTeardownOnNamespaceDeleteFailure(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}},
		testHeldLease(ns),
	)
	dynamicClient := newTrainerFakeClient()

	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, ns, nil)
	})
	trainerDeleted := false
	dynamicClient.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		trainerDeleted = true
		return false, nil, nil
	})

	resources := []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment},
	}

	err := cleanupNCCLRun(clientset, dynamicClient, ns, testNamespaceUID, testHolderID, resources, nil)
	if err == nil {
		t.Fatal("expected the namespace delete failure to fail the check, got nil")
	}
	if trainerDeleted {
		t.Error("expected Trainer teardown to be skipped after the namespace delete failed, but deleteTrainer ran")
	}
}

// TestCleanupNCCLRun_SkipsTrainerTeardownWhenOtherNCCLNamespaceExists checks
// that cleanupNCCLRun leaves a self-installed Trainer alone while another
// AICR-owned NCCL benchmark namespace exists. A concurrent run using a
// different AICR_RUN_ID gets its own namespace but can still be using this
// same shared Trainer install, since ensureTrainerInstalled's reuse path
// returns no resources for that peer to track and delete on its own.
func TestCleanupNCCLRun_SkipsTrainerTeardownWhenOtherNCCLNamespaceExists(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	peerNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "aicr-nccl-perf-other-cafef00d",
		Labels: map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf},
	}}
	holder := testHolderID
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}},
		peerNS,
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
		},
	)
	dynamicClient := newTrainerFakeClient(readyTrainerDeployment())

	resources := []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment},
	}
	persistTrainerInstallManifest(context.Background(), clientset, resources)

	if err := cleanupNCCLRun(clientset, dynamicClient, ns, testNamespaceUID, testHolderID, resources, nil); err != nil {
		t.Fatalf("cleanupNCCLRun failed: %v", err)
	}

	if _, err := dynamicClient.Resource(trainerDeploymentGVR).Namespace(trainerNamespace).
		Get(context.Background(), trainerControllerDeployment, metav1.GetOptions{}); err != nil {
		t.Errorf("expected the Trainer Deployment to survive while a peer namespace exists: %v", err)
	}
	if _, got, _ := loadTrainerInstallManifest(context.Background(), clientset); got == nil {
		t.Error("expected the Trainer install manifest to survive while a peer namespace exists")
	}
}

// TestClaimNCCLExecutionLock_ConcurrentCallersOneWins verifies that when
// several callers race to claim a namespace with no existing lock, exactly
// one wins and the rest get ErrCodeConflict.
func TestClaimNCCLExecutionLock_ConcurrentCallersOneWins(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	clientset := fake.NewClientset()
	holders, errs := raceClaimNCCLExecutionLock(clientset, ns, 8)

	if len(holders) != 1 {
		t.Fatalf("expected exactly one winner, got %d: %v", len(holders), holders)
	}
	assertAllConflict(t, errs, 7)
}

// TestClaimNCCLExecutionLock_ConcurrentTakeoverOneWins verifies that when
// several callers race to take over an abandoned lock, exactly one wins and
// the rest get ErrCodeConflict, instead of all of them proceeding. The
// fake ObjectTracker doesn't enforce optimistic concurrency on Update, so a
// reactor emulates the real apiserver's resourceVersion check.
func TestClaimNCCLExecutionLock_ConcurrentTakeoverOneWins(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	oldHolder := "stale-holder"
	oldRenew := metav1.NewMicroTime(time.Now().Add(-2 * defaults.NCCLExecutionLockStaleAge))
	clientset := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns, ResourceVersion: "1"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &oldHolder,
			RenewTime:      &oldRenew,
		},
	})

	var (
		mu        sync.Mutex
		currentRV = 1
	)
	clientset.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		mu.Lock()
		defer mu.Unlock()
		if lease.ResourceVersion != strconv.Itoa(currentRV) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
				ncclRunLockName, stderrors.New("resourceVersion mismatch"))
		}
		currentRV++
		return true, lease, nil
	})

	holders, errs := raceClaimNCCLExecutionLock(clientset, ns, 8)

	if len(holders) != 1 {
		t.Fatalf("expected exactly one winner, got %d: %v", len(holders), holders)
	}
	assertAllConflict(t, errs, 7)
}

// TestClaimNCCLExecutionLock_RefusesLiveLease verifies a lock renewed
// recently is treated as a live execution's claim, not taken over.
func TestClaimNCCLExecutionLock_RefusesLiveLease(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	liveHolder := "live-holder"
	liveRenew := metav1.NewMicroTime(time.Now())
	clientset := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &liveHolder,
			RenewTime:      &liveRenew,
		},
	})

	if _, err := claimNCCLExecutionLock(context.Background(), clientset, ns); !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
		t.Errorf("expected ErrCodeConflict against a live lease, got: %v", err)
	}
}

// TestClaimNCCLExecutionLock_ReclaimsWellBeforeNamespacePruneAge verifies a
// retry after a hard kill reclaims its dead lock promptly, well under
// NCCLStaleNamespacePruneAge, not after waiting out that much longer age.
func TestClaimNCCLExecutionLock_ReclaimsWellBeforeNamespacePruneAge(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	deadHolder := "dead-holder"
	deadRenew := metav1.NewMicroTime(time.Now().Add(-2 * defaults.NCCLExecutionLockStaleAge))
	if time.Since(deadRenew.Time) >= defaults.NCCLStaleNamespacePruneAge {
		t.Fatalf("test fixture is not a useful regression guard: 2x NCCLExecutionLockStaleAge "+
			"(%s ago) already exceeds NCCLStaleNamespacePruneAge (%s)",
			time.Since(deadRenew.Time), defaults.NCCLStaleNamespacePruneAge)
	}
	clientset := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &deadHolder,
			RenewTime:      &deadRenew,
		},
	})

	if _, err := claimNCCLExecutionLock(context.Background(), clientset, ns); err != nil {
		t.Errorf("expected the dead holder's lock to be reclaimed, got: %v", err)
	}
}

// TestNcclExecutionLockHeldBy_MissingLeaseFailsClosed checks that a missing
// Lease reports the lock not held, not held by default.
func TestNcclExecutionLockHeldBy_MissingLeaseFailsClosed(t *testing.T) {
	clientset := fake.NewClientset()

	held, err := ncclExecutionLockHeldBy(context.Background(), clientset, "aicr-nccl-perf-deadbeef", testHolderID)
	if err != nil {
		t.Fatalf("ncclExecutionLockHeldBy() error = %v", err)
	}
	if held {
		t.Error("expected a missing Lease to report the lock not held")
	}
}

// TestCleanupNCCLRun_MissingLeaseSkipsDelete checks that cleanupNCCLRun
// leaves the namespace alone, instead of deleting it, when its Lease is
// missing.
func TestCleanupNCCLRun_MissingLeaseSkipsDelete(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	clientset := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}})
	dynamicClient := newTrainerFakeClient()

	if err := cleanupNCCLRun(clientset, dynamicClient, ns, testNamespaceUID, testHolderID, nil, nil); err != nil {
		t.Fatalf("cleanupNCCLRun failed: %v", err)
	}

	if _, err := clientset.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); err != nil {
		t.Errorf("expected the namespace to survive a missing Lease, got: %v", err)
	}
}

// TestCleanupNCCLRun_LockCheckErrorSkipsCleanupWithoutFailingBenchmark checks
// that a lock-check error at cleanup is treated like a confirmed takeover,
// skipping cleanup without failing a benchmark that already passed.
func TestCleanupNCCLRun_LockCheckErrorSkipsCleanupWithoutFailingBenchmark(t *testing.T) {
	benchFailure := aicrErrors.New(aicrErrors.ErrCodeInternal, "benchmark already failed")
	tests := []struct {
		name       string
		benchErr   error
		wantErrNil bool
	}{
		{"passing benchmark stays passing", nil, true},
		{"failing benchmark keeps its own error", benchFailure, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const ns = "aicr-nccl-perf-deadbeef"
			holder := testHolderID
			clientset := fake.NewClientset(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}},
				&coordinationv1.Lease{
					ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns},
					Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
				},
			)
			dynamicClient := newTrainerFakeClient()
			clientset.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
			})

			err := cleanupNCCLRun(clientset, dynamicClient, ns, testNamespaceUID, testHolderID, nil, tt.benchErr)
			if tt.wantErrNil && err != nil {
				t.Errorf("expected a transient lock-check error not to fail an otherwise-passing benchmark, got: %v", err)
			}
			if !tt.wantErrNil && !stderrors.Is(err, benchFailure) {
				t.Errorf("expected the original benchmark failure to survive, got: %v", err)
			}
			if _, err := clientset.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); err != nil {
				t.Errorf("expected the namespace to survive a lock-check error, got: %v", err)
			}
		})
	}
}

// TestNcclExecutionLockHeldBy_ExpiredHolderCannotResumeAfterTakeover checks
// that a holder whose lock went stale and was taken over by another caller
// is correctly told it no longer holds the lock.
func TestNcclExecutionLockHeldBy_ExpiredHolderCannotResumeAfterTakeover(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	pausedHolder := "paused-holder"
	staleRenew := metav1.NewMicroTime(time.Now().Add(-2 * defaults.NCCLExecutionLockStaleAge))
	clientset := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &pausedHolder, RenewTime: &staleRenew},
	})

	newHolder, err := claimNCCLExecutionLock(context.Background(), clientset, ns)
	if err != nil {
		t.Fatalf("expected the stale lock to be taken over, got: %v", err)
	}

	held, err := ncclExecutionLockHeldBy(context.Background(), clientset, ns, pausedHolder)
	if err != nil {
		t.Fatalf("ncclExecutionLockHeldBy() error = %v", err)
	}
	if held {
		t.Errorf("expected paused holder %q to no longer hold the lock after %q took it over", pausedHolder, newHolder)
	}
}

// TestNcclExecutionLockHeldBy_LosesRaceToTakeover verifies that a takeover
// landing between ncclExecutionLockHeldBy's read and its renewal makes it
// report the lock not held, instead of the stale holder's cleanup going on
// to delete the new holder's live namespace. A reactor on the Lease Get
// applies a rival takeover to the tracker before returning the stale
// holder's view, then a second reactor emulates the apiserver's
// resourceVersion check the fake ObjectTracker skips on Update, so the
// renewal that follows the Get sees the same conflict a real cluster would.
func TestNcclExecutionLockHeldBy_LosesRaceToTakeover(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	holderA, holderB := "holder-a", "holder-b"
	leaseGVR := coordinationv1.SchemeGroupVersion.WithResource("leases")
	clientset := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns, ResourceVersion: "1"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holderA},
	})

	var (
		mu        sync.Mutex
		currentRV = 1
		tookOver  bool
	)
	clientset.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if tookOver {
			return false, nil, nil
		}
		tookOver = true
		// Return this caller its stale, pre-takeover view...
		staleView := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns, ResourceVersion: "1"},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holderA},
		}
		// ...while a rival's takeover lands underneath it, the interleaving
		// this test exists to prove ncclExecutionLockHeldBy's renewal
		// detects instead of proceeding on the stale view.
		renew := metav1.NewMicroTime(time.Now())
		currentRV++
		rival := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns, ResourceVersion: strconv.Itoa(currentRV)},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holderB, RenewTime: &renew},
		}
		if err := clientset.Tracker().Update(leaseGVR, rival, ns); err != nil {
			return true, nil, err
		}
		return true, staleView, nil
	})
	clientset.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		mu.Lock()
		defer mu.Unlock()
		if lease.ResourceVersion != strconv.Itoa(currentRV) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
				ncclRunLockName, stderrors.New("resourceVersion mismatch"))
		}
		currentRV++
		return true, lease, nil
	})

	held, err := ncclExecutionLockHeldBy(context.Background(), clientset, ns, holderA)
	if err != nil {
		t.Fatalf("ncclExecutionLockHeldBy() error = %v", err)
	}
	if held {
		t.Error("expected the renewal to lose the race to the takeover and report the lock not held")
	}
}

// TestClaimNCCLExecutionLock_CreateError checks that a Create failure other
// than AlreadyExists surfaces as an error, instead of falling through to the
// takeover path meant for an existing Lease.
func TestClaimNCCLExecutionLock_CreateError(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	clientset := fake.NewClientset()
	clientset.PrependReactor("create", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
	})

	if _, err := claimNCCLExecutionLock(context.Background(), clientset, ns); err == nil {
		t.Fatal("expected a Create failure to surface as an error")
	}
}

// TestClaimNCCLExecutionLock_UpdateError checks that a CAS-Update failure
// other than a Conflict surfaces as an error, not the same ErrCodeConflict a
// losing takeover gets.
func TestClaimNCCLExecutionLock_UpdateError(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	staleHolder := "stale-holder"
	staleRenew := metav1.NewMicroTime(time.Now().Add(-2 * defaults.NCCLExecutionLockStaleAge))
	clientset := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &staleHolder, RenewTime: &staleRenew},
	})
	clientset.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
	})

	_, err := claimNCCLExecutionLock(context.Background(), clientset, ns)
	if err == nil {
		t.Fatal("expected an Update failure to surface as an error")
	}
	if stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
		t.Errorf("expected a plain Update failure, not ErrCodeConflict, got: %v", err)
	}
}

// TestNcclExecutionLockHeldBy_GetError checks that a Lease read failure
// other than NotFound surfaces as an error, instead of being treated the
// same as a missing lock.
func TestNcclExecutionLockHeldBy_GetError(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	clientset := fake.NewClientset()
	clientset.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
	})

	if _, err := ncclExecutionLockHeldBy(context.Background(), clientset, ns, testHolderID); err == nil {
		t.Fatal("expected a Lease read failure to surface as an error")
	}
}

// TestNcclExecutionLockHeldBy_RenewError checks that a renew failure other
// than a Conflict surfaces as an error, instead of quietly reporting the
// lock as not held.
func TestNcclExecutionLockHeldBy_RenewError(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	holder := testHolderID
	clientset := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: ns},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	})
	clientset.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
	})

	if _, err := ncclExecutionLockHeldBy(context.Background(), clientset, ns, testHolderID); err == nil {
		t.Fatal("expected a renew failure to surface as an error")
	}
}

// TestRollbackNCCLNamespace_ToleratesDeleteFailure checks that a rollback
// delete failure is only logged, not propagated. This is a leak-avoidance
// nicety, not correctness-critical, since pruneStaleNCCLNamespaces reclaims
// an orphaned namespace on a later run regardless.
func TestRollbackNCCLNamespace_ToleratesDeleteFailure(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	clientset := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}})
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, ns, nil)
	})

	rollbackNCCLNamespace(clientset, ns, testNamespaceUID) // must not panic

	if _, err := clientset.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); err != nil {
		t.Errorf("expected the namespace to survive a failed rollback delete, got: %v", err)
	}
}

// raceClaimNCCLExecutionLock calls claimNCCLExecutionLock concurrently from
// n goroutines against the same namespace and returns the winning holder
// IDs and the errors from the rest.
func raceClaimNCCLExecutionLock(clientset kubernetes.Interface, namespace string, n int) (holders []string, errs []error) {
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			holderID, err := claimNCCLExecutionLock(context.Background(), clientset, namespace)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else {
				holders = append(holders, holderID)
			}
		}()
	}
	wg.Wait()
	return holders, errs
}

// assertAllConflict fails the test unless errs has wantCount entries, all
// ErrCodeConflict.
func assertAllConflict(t *testing.T, errs []error, wantCount int) {
	t.Helper()
	if len(errs) != wantCount {
		t.Fatalf("expected %d losing callers, got %d: %v", wantCount, len(errs), errs)
	}
	for _, err := range errs {
		if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
			t.Errorf("expected ErrCodeConflict for a losing caller, got: %v", err)
		}
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
		nsArg   *corev1.Namespace
		objs    []runtime.Object
		wantErr bool
	}{
		{name: "namespace does not exist yet", nsArg: nil, objs: nil, wantErr: false},
		{name: "namespace terminating from a prior cleanup", nsArg: terminatingNS, objs: []runtime.Object{terminatingNS}, wantErr: false},
		{name: "namespace active with no pods (empty stale leftover)", nsArg: activeNS, objs: []runtime.Object{activeNS}, wantErr: false},
		{
			name:    "namespace active with only terminal pods (stale same-run resources)",
			nsArg:   activeNS,
			objs:    []runtime.Object{activeNS, terminalPod},
			wantErr: false,
		},
		{
			name:    "namespace active with a live pod (foreign/concurrent execution)",
			nsArg:   activeNS,
			objs:    []runtime.Object{activeNS, livePod},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.objs...)
			err := verifyNCCLNamespaceNotLive(context.Background(), client, tt.nsArg)
			if (err != nil) != tt.wantErr {
				t.Errorf("verifyNCCLNamespaceNotLive() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
				t.Errorf("expected ErrCodeConflict, got %v", err)
			}
		})
	}
}

// TestVerifyNCCLNamespaceNotLive_ListError checks that a Pods().List
// failure surfaces as a plain error, instead of being treated the same as
// an empty, safe-to-adopt namespace.
func TestVerifyNCCLNamespaceNotLive_ListError(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	activeNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	client := fake.NewClientset(activeNS)
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
	})

	err := verifyNCCLNamespaceNotLive(context.Background(), client, activeNS)
	if err == nil {
		t.Fatal("expected a Pods().List failure to surface as an error")
	}
	if stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
		t.Errorf("expected a plain read failure, not ErrCodeConflict, got: %v", err)
	}
}

// TestRunNCCLTrainJob_RefusesLiveForeignNamespace is the end-to-end
// regression guard for the same MAJOR finding. A retry with the same
// AICR_RUN_ID (or a rare random-suffix collision) must not silently adopt,
// and must never let its deferred cleanup delete, a namespace a different,
// still-running execution owns.
func TestRunNCCLTrainJob_RefusesLiveForeignNamespace(t *testing.T) {
	t.Setenv("AICR_RUN_ID", "test-run-id")
	ns := ncclRunNamespace(variantDefault)

	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{
			labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf,
		}}},
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

// TestCreateUnstructured_RecreatesAfterNotFoundRace checks that a resource
// deleted between the AlreadyExists Create and the follow-up Get falls
// through to a fresh Create, instead of surfacing that race as a spurious
// internal error.
func TestCreateUnstructured_RecreatesAfterNotFoundRace(t *testing.T) {
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
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, stale)

	var raced bool
	dynamicClient.PrependReactor("get", "trainingruntimes", func(k8stesting.Action) (bool, runtime.Object, error) {
		if raced {
			return false, nil, nil // second Get, from the fallback Create's caller, uses the default reactor.
		}
		raced = true
		if err := dynamicClient.Tracker().Delete(trainingRuntimeGVR, ns, ncclTrainingRuntimeName); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewNotFound(trainingRuntimeGVR.GroupResource(), ncclTrainingRuntimeName)
	})

	fresh := stale.DeepCopy()
	fresh.SetResourceVersion("")

	if err := createUnstructured(context.Background(), dynamicClient, trainingRuntimeGVR, ns, fresh); err != nil {
		t.Fatalf("createUnstructured() after a NotFound race = %v, want a fresh Create", err)
	}

	if _, err := dynamicClient.Resource(trainingRuntimeGVR).Namespace(ns).Get(context.Background(), ncclTrainingRuntimeName, metav1.GetOptions{}); err != nil {
		t.Fatalf("expected the resource to exist after the fallback Create: %v", err)
	}
}

// TestCreateUnstructured_ReclaimsStaleTrainJob_DeletesAndRecreates is the
// regression guard for the finding that reclaiming a stale TrainJob by
// updating it in place does not recover a same-run retry (see
// createUnstructured's doc comment for why). It must be reclaimed by
// delete then recreate instead.
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

// TestCreateUnstructured_TrainJobUIDMismatchPreventsDelete verifies the
// reclaim delete is pinned to the TrainJob actually observed, not just its
// name. A "get" reactor hands back a stale, already-observed UID once,
// simulating a concurrent delete-and-recreate in the gap between
// createUnstructured's own Get and Delete. client-go's fake ObjectTracker
// ignores Delete preconditions, so a second reactor emulates the real
// apiserver's precondition check to prove the UID mismatch surfaces as an
// error instead of deleting the replacement regardless.
func TestCreateUnstructured_TrainJobUIDMismatchPreventsDelete(t *testing.T) {
	const ns = "aicr-nccl-bench-deadbeef"
	const observedUID = types.UID("observed-before-replace")
	const actualUID = types.UID("actual-owner-after-replace")
	listKinds := map[schema.GroupVersionResource]string{trainJobGVR: "TrainJobList"}

	stale := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": trainJobGVR.GroupVersion().String(),
		"kind":       "TrainJob",
		"metadata": map[string]interface{}{
			"name":            ncclTrainJobName,
			"namespace":       ns,
			"uid":             string(actualUID),
			"resourceVersion": "1",
		},
		"spec": map[string]interface{}{"stale": true},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, stale)

	getObservedStaleUID := true
	dynamicClient.PrependReactor("get", "trainjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !getObservedStaleUID {
			return false, nil, nil
		}
		getObservedStaleUID = false
		observed := stale.DeepCopy()
		observed.SetUID(observedUID)
		return true, observed, nil
	})
	dynamicClient.PrependReactor("delete", "trainjobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction, ok := action.(k8stesting.DeleteAction)
		if !ok {
			return false, nil, nil
		}
		preconditions := deleteAction.GetDeleteOptions().Preconditions
		if preconditions == nil || preconditions.UID == nil || *preconditions.UID == actualUID {
			return false, nil, nil
		}
		return true, nil, apierrors.NewConflict(trainJobGVR.GroupResource(), ncclTrainJobName,
			stderrors.New("uid in precondition does not match uid in record"))
	})

	fresh := stale.DeepCopy()
	fresh.SetUID("")
	fresh.SetResourceVersion("")
	if err := unstructured.SetNestedField(fresh.Object, false, "spec", "stale"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}

	err := createUnstructured(context.Background(), dynamicClient, trainJobGVR, ns, fresh)
	if err == nil {
		t.Fatal("expected a conflict error for a mismatched owning UID, got nil")
	}

	got, getErr := dynamicClient.Resource(trainJobGVR).Namespace(ns).Get(context.Background(), ncclTrainJobName, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("TrainJob should be left alone on a UID mismatch, got err=%v", getErr)
	}
	if staleVal, _, _ := unstructured.NestedBool(got.Object, "spec", "stale"); !staleVal {
		t.Error("TrainJob was replaced despite the UID mismatch")
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
