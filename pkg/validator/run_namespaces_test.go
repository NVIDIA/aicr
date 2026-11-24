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

package validator

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/validator/catalog"
	"aicr.run/pkg/validator/ctrf"
	"aicr.run/pkg/validator/labels"
	v1 "aicr.run/pkg/validator/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const testRunID = "20261106-173024-0123456789abcdef"

var namespacesGVR = corev1.SchemeGroupVersion.WithResource("namespaces")

// ownedBy returns the labels a performance check's ensureNamespace stamps on
// the namespace it creates for runID.
func ownedBy(runID, component string) map[string]string {
	return map[string]string{
		labels.RunID:     runID,
		labels.ManagedBy: labels.ValueValidator,
		labels.Component: component,
	}
}

func testNamespace(name string, lbls map[string]string, terminating bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Labels: lbls}}
	if terminating {
		now := metav1.Now()
		ns.DeletionTimestamp = &now
		ns.Finalizers = []string{"kubernetes"}
	}
	return ns
}

func runSuffix(runID string) string { return v1.WorkloadNamespaceSuffix(runID) }

// inferenceNS and ncclNS are a run's namespaces, named and labeled as the
// performance checks create them; inferenceNS is always testRunID's.
func inferenceNS() *corev1.Namespace {
	return testNamespace(v1.InferencePerfNamespace(runSuffix(testRunID)), ownedBy(testRunID, labels.ValueInferencePerf), false)
}

func ncclNS(runID string) *corev1.Namespace {
	return testNamespace(v1.NCCLPerfNamespace("default", runSuffix(runID)), ownedBy(runID, labels.ValueNCCLPerf), false)
}

func namespaceExists(t *testing.T, cs *fake.Clientset, name string) bool {
	t.Helper()
	_, err := cs.Tracker().Get(namespacesGVR, "", name)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func namespaceDeleted(cs *fake.Clientset) bool {
	for _, a := range cs.Actions() {
		if a.Matches("delete", "namespaces") {
			return true
		}
	}
	return false
}

func TestCleanupRunNamespaces(t *testing.T) {
	ours, ours2 := inferenceNS(), ncclNS(testRunID)
	otherRun := ncclNS("another-run")
	unlabeled := testNamespace("unlabeled", nil, false)
	// Carries the run's labels but a name no check derives for the run.
	decoyName := testNamespace("workloads", ownedBy(testRunID, labels.ValueNCCLPerf), false)
	// Named for the run but missing the ownership labels.
	decoyUnowned := testNamespace(v1.NCCLPerfNamespace("net", runSuffix(testRunID)), map[string]string{labels.RunID: testRunID}, false)
	emptyLabel := testNamespace("empty-run-id", ownedBy("", labels.ValueNCCLPerf), false)

	tests := []struct {
		name        string
		runID       string
		seed        []runtime.Object
		reactor     func(*testing.T, *fake.Clientset)
		wantCode    errors.ErrorCode // "" = success
		wantNamed   string           // namespace the error must name
		wantGone    []string
		wantKept    []string
		wantSkipped []string
		wantNoAPI   bool
	}{
		{
			name:     "deletes only this run's namespaces",
			runID:    testRunID,
			seed:     []runtime.Object{ours, ours2, otherRun, unlabeled},
			wantGone: []string{ours.Name, ours2.Name},
			wantKept: []string{otherRun.Name, unlabeled.Name},
		},
		{
			// A label alone proves nothing: the name must reconstruct from
			// the run ID, and the ownership labels must be present.
			name:        "decoys survive and the labeled one is reported",
			runID:       testRunID,
			seed:        []runtime.Object{ours, decoyName, decoyUnowned},
			wantGone:    []string{ours.Name},
			wantKept:    []string{decoyName.Name, decoyUnowned.Name},
			wantSkipped: []string{decoyName.Name},
		},
		{
			name:     "nothing labeled with the run is a no-op",
			runID:    testRunID,
			seed:     []runtime.Object{otherRun, unlabeled},
			wantKept: []string{otherRun.Name, unlabeled.Name},
		},
		{
			// An empty selector value would match a namespace labeled with
			// an empty run ID, which no run owns.
			name:      "empty run ID touches nothing",
			runID:     "",
			seed:      []runtime.Object{emptyLabel, unlabeled},
			wantKept:  []string{emptyLabel.Name, unlabeled.Name},
			wantNoAPI: true,
		},
		{
			name:  "refused delete names the namespace left behind",
			runID: testRunID,
			seed:  []runtime.Object{ours, ours2},
			reactor: func(_ *testing.T, cs *fake.Clientset) {
				cs.PrependReactor("delete", "namespaces", func(a clienttesting.Action) (bool, runtime.Object, error) {
					if a.(clienttesting.DeleteAction).GetName() != ours.Name {
						return false, nil, nil
					}
					return true, nil, apierrors.NewForbidden(corev1.Resource("namespaces"), ours.Name, nil)
				})
			},
			wantCode:  errors.ErrCodeUnauthorized,
			wantNamed: ours.Name,
			wantGone:  []string{ours2.Name},
			wantKept:  []string{ours.Name},
		},
		{
			name:  "refused list is reported with its code",
			runID: testRunID,
			seed:  []runtime.Object{ours},
			reactor: func(_ *testing.T, cs *fake.Clientset) {
				cs.PrependReactor("list", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(corev1.Resource("namespaces"), "", nil)
				})
			},
			wantCode: errors.ErrCodeUnauthorized,
			wantKept: []string{ours.Name},
		},
		{
			name:  "already-gone namespace is success",
			runID: testRunID,
			seed:  []runtime.Object{ours},
			reactor: func(t *testing.T, cs *fake.Clientset) {
				cs.PrependReactor("delete", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
					if err := cs.Tracker().Delete(namespacesGVR, "", ours.Name); err != nil {
						t.Error(err)
					}
					return true, nil, apierrors.NewNotFound(corev1.Resource("namespaces"), ours.Name)
				})
			},
			wantGone: []string{ours.Name},
		},
		{
			// The UID precondition failed: the name now holds a namespace
			// this run did not list, which is neither deleted nor waited for.
			name:  "replaced namespace is left alone",
			runID: testRunID,
			seed:  []runtime.Object{ours},
			reactor: func(t *testing.T, cs *fake.Clientset) {
				cs.PrependReactor("delete", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
					replacement := ours.DeepCopy()
					replacement.UID = "replacement-uid"
					if err := cs.Tracker().Update(namespacesGVR, replacement, ""); err != nil {
						t.Error(err)
					}
					return true, nil, apierrors.NewConflict(corev1.Resource("namespaces"), ours.Name, nil)
				})
			},
			wantKept: []string{ours.Name},
		},
		{
			name:  "transient wait-phase list failure is retried",
			runID: testRunID,
			seed:  []runtime.Object{ours},
			reactor: func(_ *testing.T, cs *fake.Clientset) {
				var lists atomic.Int32
				cs.PrependReactor("list", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
					if lists.Add(1) == 2 {
						return true, nil, apierrors.NewServiceUnavailable("apiserver restarting")
					}
					return false, nil, nil
				})
			},
			wantGone: []string{ours.Name},
		},
		{
			name:  "refused wait-phase list names the namespace",
			runID: testRunID,
			seed:  []runtime.Object{testNamespace(ours.Name, ours.Labels, true)},
			reactor: func(_ *testing.T, cs *fake.Clientset) {
				var lists atomic.Int32
				cs.PrependReactor("list", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
					if lists.Add(1) >= 2 {
						return true, nil, apierrors.NewForbidden(corev1.Resource("namespaces"), "", nil)
					}
					return false, nil, nil
				})
			},
			wantCode:  errors.ErrCodeUnauthorized,
			wantNamed: ours.Name,
			wantKept:  []string{ours.Name},
		},
	}
	old := runNamespaceRetryBackoff
	runNamespaceRetryBackoff = time.Millisecond
	t.Cleanup(func() { runNamespaceRetryBackoff = old })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tt.seed...)
			if tt.reactor != nil {
				tt.reactor(t, cs)
			}
			v := New(WithRunID(tt.runID))

			res, err := v.cleanupRunNamespaces(t.Context(), cs)

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantNamed) {
					t.Errorf("error %q does not name %q", err, tt.wantNamed)
				}
			}
			if !slices.Equal(res.Skipped, tt.wantSkipped) || len(res.Terminating) != 0 {
				t.Errorf("skipped=%v terminating=%v, want skipped=%v and none terminating", res.Skipped, res.Terminating, tt.wantSkipped)
			}
			for _, name := range tt.wantGone {
				if namespaceExists(t, cs, name) {
					t.Errorf("namespace %s survived", name)
				}
			}
			for _, name := range tt.wantKept {
				if !namespaceExists(t, cs, name) {
					t.Errorf("namespace %s was deleted", name)
				}
			}
			if tt.wantNoAPI && len(cs.Actions()) != 0 {
				t.Errorf("made API calls with no run ID: %v", cs.Actions())
			}
		})
	}
}

func TestCleanupRunNamespacesDeleteAndSelector(t *testing.T) {
	ns := inferenceNS()
	cs := fake.NewSimpleClientset(ns)
	var opts metav1.DeleteOptions
	var selector string
	cs.PrependReactor("delete", "namespaces", func(a clienttesting.Action) (bool, runtime.Object, error) {
		opts = a.(clienttesting.DeleteAction).GetDeleteOptions()
		return false, nil, nil
	})
	cs.PrependReactor("list", "namespaces", func(a clienttesting.Action) (bool, runtime.Object, error) {
		selector = a.(clienttesting.ListAction).GetListRestrictions().Labels.String()
		return false, nil, nil
	})

	if _, err := New(WithRunID(testRunID)).cleanupRunNamespaces(t.Context(), cs); err != nil {
		t.Fatal(err)
	}
	want := "aicr.run/run-id=" + testRunID +
		",app.kubernetes.io/component in (inference-perf,nccl-perf),app.kubernetes.io/managed-by=aicr-validator"
	if selector != want {
		t.Errorf("selector = %q, want %q", selector, want)
	}
	if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != ns.UID {
		t.Errorf("delete preconditions = %+v, want UID %s", opts.Preconditions, ns.UID)
	}
	if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationBackground {
		t.Errorf("propagation = %v, want Background", opts.PropagationPolicy)
	}
}

// TestCleanupRunNamespacesWaitsForTermination covers a namespace still
// terminating after its delete was accepted: the cleanup waits on the watch
// until it is gone, re-lists when the watch closes or expires without
// cancellation rather than failing, fails on a watch error it cannot retry,
// and reports the namespace as terminating when the deadline comes first.
func TestCleanupRunNamespacesWaitsForTermination(t *testing.T) {
	old := runNamespaceRetryBackoff
	runNamespaceRetryBackoff = time.Millisecond
	t.Cleanup(func() { runNamespaceRetryBackoff = old })

	tests := []struct {
		name string
		// end is how the first watch ends: "deleted", "closed", "expired"
		// (the Watch call itself returns 410), "error" (a non-410
		// watch.Error event) or "never".
		end             string
		wantCode        errors.ErrorCode
		wantTerminating bool
	}{
		{name: "deleted event ends the wait", end: "deleted"},
		{name: "closed watch is re-listed, not failed", end: "closed"},
		{name: "expired watch is re-established", end: "expired"},
		{name: "non-410 watch error fails with its code", end: "error", wantCode: errors.ErrCodeUnauthorized},
		{name: "deadline reports the namespace terminating", end: "never", wantTerminating: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := testNamespace(v1.InferencePerfNamespace(runSuffix(testRunID)), ownedBy(testRunID, labels.ValueInferencePerf), true)
			cs := fake.NewSimpleClientset(ns)
			watches := make(chan *watch.RaceFreeFakeWatcher, 4)
			var watchCalls atomic.Int32
			cs.PrependWatchReactor("namespaces", func(clienttesting.Action) (bool, watch.Interface, error) {
				if tt.end == "expired" && watchCalls.Add(1) == 1 {
					return true, nil, apierrors.NewResourceExpired("too old resource version")
				}
				w := watch.NewRaceFreeFake()
				watches <- w
				return true, w, nil
			})
			ctx := t.Context()
			if tt.end == "never" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			type outcome struct {
				res runNamespaceCleanup
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				res, err := New(WithRunID(testRunID)).cleanupRunNamespaces(ctx, cs)
				done <- outcome{res, err}
			}()

			w := <-watches
			if namespaceDeleted(cs) {
				t.Error("re-deleted a namespace already terminating")
			}
			switch tt.end {
			case "deleted", "closed", "expired":
				if err := cs.Tracker().Delete(namespacesGVR, "", ns.Name); err != nil {
					t.Fatal(err)
				}
				if tt.end == "closed" {
					w.Stop()
				} else {
					w.Delete(ns)
				}
			case "error":
				w.Error(&apierrors.NewForbidden(corev1.Resource("namespaces"), "", nil).ErrStatus)
			}

			got := <-done
			switch {
			case tt.wantCode != "":
				errorstest.WantReportedCode(t, got.err, tt.wantCode)
				if !apierrors.IsForbidden(got.err) || !strings.Contains(got.err.Error(), ns.Name) {
					t.Errorf("error %v, want the Forbidden cause and %s named", got.err, ns.Name)
				}
			case got.err != nil:
				t.Fatalf("unexpected error: %v", got.err)
			case tt.wantTerminating:
				if !slices.Equal(got.res.Terminating, []string{ns.Name}) {
					t.Errorf("terminating = %v, want [%s]", got.res.Terminating, ns.Name)
				}
			case len(got.res.Terminating) != 0:
				t.Errorf("terminating = %v, want none", got.res.Terminating)
			}
		})
	}
}

// TestCleanupAbandonedCheckMessages pins how each cleanup outcome is worded
// on the abandoned check's CTRF entry: a refusal is a cleanup failure, an
// accepted deletion that outlasts the budget is reported as still
// terminating, a run-labeled namespace not named for the run is reported as
// left in place, and the status never changes.
func TestCleanupAbandonedCheckMessages(t *testing.T) {
	tests := []struct {
		name    string
		seed    []runtime.Object
		reactor func(*testing.T, *fake.Clientset)
		want    string
		notWant string
		// wantNoDelete asserts that no namespace delete was sent.
		wantNoDelete bool
	}{
		{
			// The budget runs out between the List and the first Delete: a
			// namespace whose deletion was never sent is a failure, not one
			// "still terminating".
			name: "budget ending before the deletes is a cleanup failure",
			seed: []runtime.Object{inferenceNS(), ncclNS(testRunID)},
			reactor: func(_ *testing.T, cs *fake.Clientset) {
				var listed atomic.Bool
				cs.PrependReactor("list", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
					if !listed.Swap(true) {
						time.Sleep(defaults.ValidatorWorkloadNamespaceCleanupTimeout + time.Second)
					}
					return false, nil, nil
				})
			},
			want: "workload namespace cleanup failed: [TIMEOUT] deletion not attempted: " +
				inferenceNS().Name + ", " + ncclNS(testRunID).Name,
			notWant:      "deletion requested",
			wantNoDelete: true,
		},
		{
			name: "refused delete is a cleanup failure",
			seed: []runtime.Object{inferenceNS()},
			reactor: func(_ *testing.T, cs *fake.Clientset) {
				cs.PrependReactor("delete", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(corev1.Resource("namespaces"), "", nil)
				})
			},
			want:    "workload namespace cleanup failed: ",
			notWant: "still terminating",
		},
		{
			// The apiserver accepts the delete, but finalizers hold the
			// namespace past the budget.
			name: "accepted delete that outlasts the budget is still terminating",
			seed: []runtime.Object{inferenceNS()},
			reactor: func(t *testing.T, cs *fake.Clientset) {
				cs.PrependReactor("delete", "namespaces", func(a clienttesting.Action) (bool, runtime.Object, error) {
					obj, err := cs.Tracker().Get(namespacesGVR, "", a.(clienttesting.DeleteAction).GetName())
					if err != nil {
						t.Error(err)
						return true, nil, err
					}
					ns := obj.(*corev1.Namespace).DeepCopy()
					now := metav1.Now()
					ns.DeletionTimestamp = &now
					ns.Finalizers = []string{"kubernetes"}
					if err := cs.Tracker().Update(namespacesGVR, ns, ""); err != nil {
						t.Error(err)
					}
					return true, nil, nil
				})
			},
			want: "workload namespace deletion requested but still terminating after " +
				defaults.ValidatorWorkloadNamespaceCleanupTimeout.String() + ": " + inferenceNS().Name,
			notWant: "cleanup failed",
		},
		{
			name:    "labeled namespace not named for the run is reported",
			seed:    []runtime.Object{testNamespace("workloads", ownedBy(testRunID, labels.ValueNCCLPerf), false)},
			want:    "run-labeled namespaces not named for this run left in place: workloads",
			notWant: "cleanup failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cs := fake.NewSimpleClientset(tt.seed...)
				if tt.reactor != nil {
					tt.reactor(t, cs)
				}
				builder := ctrf.NewBuilder("aicr", "test", string(PhasePerformance))
				result := &ctrf.ValidatorResult{Name: "inference-perf", Phase: string(PhasePerformance), ExitCode: -1, TerminationMsg: "validation failed: x"}
				builder.AddResult(result)

				New(WithRunID(testRunID)).cleanupAbandonedCheck(t.Context(), cs, builder, result)

				got := builder.Build().Results.Tests[0]
				if !strings.HasPrefix(got.Message, "validation failed: x; ") || !strings.Contains(got.Message, tt.want) ||
					strings.Contains(got.Message, tt.notWant) {

					t.Errorf("message = %q, want %q and not %q", got.Message, tt.want, tt.notWant)
				}
				if got.Status != ctrf.StatusOther {
					t.Errorf("status = %s, want %s unchanged", got.Status, ctrf.StatusOther)
				}
				if tt.wantNoDelete && namespaceDeleted(cs) {
					t.Error("sent a namespace delete after the budget ran out")
				}
			})
		})
	}
}

// abandonClient is a fake cluster whose validator Jobs either complete with a
// passing pod or, when waitFails, cannot be watched, so the orchestrator
// abandons them with no pod ever found.
func abandonClient(t *testing.T, waitFails bool, objs ...runtime.Object) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	cs.PrependReactor("patch", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		var j batchv1.Job
		if err := json.Unmarshal(a.(clienttesting.PatchAction).GetPatch(), &j); err != nil {
			t.Error(err)
			return true, nil, err
		}
		if !waitFails {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-pod", Namespace: j.Namespace, Labels: map[string]string{"batch.kubernetes.io/job-name": j.Name}},
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
					Name: "validator", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "completed"}},
				}}},
			}
			if err := cs.Tracker().Add(p); err != nil {
				t.Error(err)
				return true, nil, err
			}
		}
		if err := cs.Tracker().Add(&j); err != nil {
			t.Error(err)
			return true, nil, err
		}
		return true, &j, nil
	})
	if waitFails {
		cs.PrependWatchReactor("jobs", func(clienttesting.Action) (bool, watch.Interface, error) {
			return true, nil, apierrors.NewServiceUnavailable("injected wait failure")
		})
	}
	cs.PrependReactor("patch", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.ConfigMap{}, nil
	})
	return cs
}

// TestRunPhaseAbandonCleansRunNamespaces covers issue #3709: a check the
// orchestrator abandons after a wait failure has its pod killed with its Job,
// so the workload namespace it created for the run is deleted by the
// orchestrator, and a failure to do so is recorded on the check's result.
func TestRunPhaseAbandonCleansRunNamespaces(t *testing.T) {
	const ncclCheck = "nccl-all-reduce-bw"
	tests := []struct {
		name           string
		check          string
		waitFails      bool
		noCleanup      bool
		retain         string // AICR_INFERENCE_PERF_NO_CLEANUP
		deleteFails    bool
		wantGone       bool
		wantCleanupMsg bool
	}{
		{name: "abandoned check's namespace is deleted", check: ncclCheck, waitFails: true, wantGone: true},
		{name: "delete failure is recorded on the result", check: ncclCheck, waitFails: true, deleteFails: true, wantCleanupMsg: true},
		{name: "retained inference-perf namespace is kept", check: v1.InferencePerfCheckName, waitFails: true, retain: "1"},
		{name: "no-cleanup keeps the abandoned check's namespace", check: ncclCheck, waitFails: true, noCleanup: true},
		{name: "completed check leaves cleanup to the check", check: ncclCheck},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICR_INFERENCE_PERF_NO_CLEANUP", tt.retain)
			logs := captureSlog(t)
			ours := ncclNS(testRunID)
			otherRun := ncclNS("another-run")
			unlabeled := testNamespace("unlabeled", nil, false)
			cs := abandonClient(t, tt.waitFails, ours, otherRun, unlabeled)
			if tt.deleteFails {
				cs.PrependReactor("delete", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(corev1.Resource("namespaces"), ours.Name, nil)
				})
			}
			cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
				{Name: tt.check, Phase: "performance", Image: "example.com/check:v1"},
			}}
			v := New(WithRunID(testRunID), WithCleanup(!tt.noCleanup))

			got, err := v.runPhase(t.Context(), cs, nil, cat, PhasePerformance,
				validationWithChecks(map[Phase][]string{PhasePerformance: {tt.check}}))
			if err != nil {
				t.Fatalf("runPhase: %v", err)
			}

			result := got.Report.Results.Tests[0]
			wantStatus := ctrf.StatusPassed
			if tt.waitFails {
				wantStatus = ctrf.StatusOther
				if !strings.Contains(result.Message, "injected wait failure") {
					t.Errorf("message=%q, want the wait cause", result.Message)
				}
				if rec := findLogRecord(t, logs.Bytes(), "validator wait failed"); rec["runID"] != testRunID || rec["name"] != tt.check {
					t.Errorf("wait failure log = %v, want runID %s and name %s", rec, testRunID, tt.check)
				}
			}
			if result.Status != wantStatus {
				t.Errorf("status=%s, want %s", result.Status, wantStatus)
			}
			if has := strings.Contains(result.Message, "workload namespace cleanup failed: "); has != tt.wantCleanupMsg {
				t.Errorf("message=%q, cleanup failure recorded = %v, want %v", result.Message, has, tt.wantCleanupMsg)
			}
			if tt.wantCleanupMsg {
				rec := findLogRecord(t, logs.Bytes(), "failed to clean up workload namespaces of abandoned validator")
				if rec["runID"] != testRunID {
					t.Errorf("cleanup failure log = %v, want runID %s", rec, testRunID)
				}
			}
			if exists := namespaceExists(t, cs, ours.Name); exists == tt.wantGone {
				t.Errorf("run namespace exists = %v, want gone = %v", exists, tt.wantGone)
			}
			if !tt.wantGone && !tt.deleteFails && namespaceDeleted(cs) {
				t.Error("issued a namespace delete")
			}
			for _, name := range []string{otherRun.Name, unlabeled.Name} {
				if !namespaceExists(t, cs, name) {
					t.Errorf("namespace %s not owned by the run was deleted", name)
				}
			}
		})
	}
}
