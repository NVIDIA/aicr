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
	stderrors "errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	batchclient "k8s.io/client-go/kubernetes/typed/batch/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	clienttesting "k8s.io/client-go/testing"
)

// Fake reactors do not receive contexts; these wrappers expose the actual
// lifecycle contexts without changing production deployment interfaces.
type observingClient struct {
	kubernetes.Interface
	observe func(string, context.Context)
}

func (c observingClient) BatchV1() batchclient.BatchV1Interface {
	return observingBatch{c.Interface.BatchV1(), c.observe}
}
func (c observingClient) CoreV1() coreclient.CoreV1Interface {
	return observingCore{c.Interface.CoreV1(), c.observe}
}

type observingBatch struct {
	batchclient.BatchV1Interface
	observe func(string, context.Context)
}

func (c observingBatch) Jobs(ns string) batchclient.JobInterface {
	return observingJobs{c.BatchV1Interface.Jobs(ns), c.observe}
}

type observingJobs struct {
	batchclient.JobInterface
	observe func(string, context.Context)
}

func (c observingJobs) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	c.observe("delete job", ctx)
	return c.JobInterface.Delete(ctx, name, opts)
}
func (c observingJobs) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	c.observe("watch job", ctx)
	return c.JobInterface.Watch(ctx, opts)
}

type observingCore struct {
	coreclient.CoreV1Interface
	observe func(string, context.Context)
}

func (c observingCore) Pods(ns string) coreclient.PodInterface {
	return observingPods{c.CoreV1Interface.Pods(ns), c.observe}
}
func (c observingCore) ConfigMaps(ns string) coreclient.ConfigMapInterface {
	return observingConfigMaps{c.CoreV1Interface.ConfigMaps(ns), c.observe}
}

type observingPods struct {
	coreclient.PodInterface
	observe func(string, context.Context)
}

func (c observingPods) List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error) {
	c.observe("capture pod", ctx)
	return c.PodInterface.List(ctx, opts)
}
func (c observingPods) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	c.observe("wait pod", ctx)
	return c.PodInterface.Watch(ctx, opts)
}

type observingConfigMaps struct {
	coreclient.ConfigMapInterface
	observe func(string, context.Context)
}

func (c observingConfigMaps) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*corev1.ConfigMap, error) {
	c.observe("write report", ctx)
	return c.ConfigMapInterface.Patch(ctx, name, pt, data, opts, subresources...)
}
func (c observingConfigMaps) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	c.observe("delete configmap", ctx)
	return c.ConfigMapInterface.Delete(ctx, name, opts)
}

func TestRunPhaseCancellation(t *testing.T) {
	for _, persistFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted", true: "persistence fails"}[persistFailure], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cs := fake.NewSimpleClientset()
			var applied []string
			var persisted *ctrf.Report
			cs.PrependReactor("patch", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
				var j batchv1.Job
				if err := json.Unmarshal(a.(clienttesting.PatchAction).GetPatch(), &j); err != nil {
					t.Fatal(err)
				}
				applied = append(applied, j.Name)
				if len(applied) == 1 {
					j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				}
				if err := cs.Tracker().Add(&j); err != nil {
					t.Fatal(err)
				}
				state := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
				if len(applied) == 1 {
					state = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "completed"}}
				}
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-pod", Namespace: j.Namespace, Labels: map[string]string{"batch.kubernetes.io/job-name": j.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "validator", State: state}}}}
				if err := cs.Tracker().Add(p); err != nil {
					t.Fatal(err)
				}
				return true, &j, nil
			})
			cs.PrependWatchReactor("jobs", func(clienttesting.Action) (bool, watch.Interface, error) { return true, watch.NewRaceFreeFake(), nil })
			cs.PrependReactor("delete", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
				name := a.(clienttesting.DeleteAction).GetName()
				obj, err := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), a.GetNamespace(), name+"-pod")
				if err != nil {
					t.Error(err)
					return true, nil, err
				}
				p := obj.(*corev1.Pod).DeepCopy()
				p.Status.Phase = corev1.PodSucceeded
				if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
					t.Error(err)
					return true, nil, err
				}
				return false, nil, nil
			})
			cs.PrependReactor("patch", "configmaps", func(a clienttesting.Action) (bool, runtime.Object, error) {
				if persistFailure {
					return true, nil, errors.New(errors.ErrCodeUnavailable, "report store unavailable")
				}
				var cm corev1.ConfigMap
				if err := json.Unmarshal(a.(clienttesting.PatchAction).GetPatch(), &cm); err != nil {
					t.Fatal(err)
				}
				persisted = &ctrf.Report{}
				if err := json.Unmarshal([]byte(cm.Data[ctrf.ConfigMapKeyReport]), persisted); err != nil {
					t.Fatal(err)
				}
				return true, &cm, nil
			})
			var cleanupDeadline time.Time
			var order []string
			observing := observingClient{Interface: cs, observe: func(op string, c context.Context) {
				if op == "watch job" {
					cancel()
					return
				}
				if ctx.Err() == nil {
					return
				}
				order = append(order, op)
				if c.Err() != nil {
					t.Errorf("%s used canceled context: %v", op, c.Err())
				}
				deadline, ok := c.Deadline()
				if !ok || time.Until(deadline) > defaults.K8sCleanupTimeout {
					t.Errorf("%s cleanup budget=%v", op, time.Until(deadline))
				}
				if cleanupDeadline.IsZero() {
					cleanupDeadline = deadline
				} else if !deadline.Equal(cleanupDeadline) {
					t.Errorf("%s reset shared deadline: %v != %v", op, deadline, cleanupDeadline)
				}
			}}
			cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
				{Name: "completed", Phase: "deployment", Image: "example.com/check:v1"},
				{Name: "interrupted", Phase: "deployment", Image: "example.com/check:v1"},
				{Name: "not-started", Phase: "deployment", Image: "example.com/check:v1"},
			}}
			v := New(WithRunID("cancel"), WithCleanup(true))
			partial, err := v.runPhase(ctx, observing, nil, cat, PhaseDeployment, validationWithChecks(map[Phase][]string{PhaseDeployment: {"completed", "interrupted", "not-started"}}))
			errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
			if partial == nil || partial.Status != ctrf.StatusOther {
				t.Fatalf("partial=%+v", partial)
			}
			summary := partial.Report.Results.Summary
			if summary.Tests != 3 || summary.Passed != 1 || summary.Other != 1 || summary.Skipped != 1 {
				t.Errorf("summary=%+v", summary)
			}
			tests := partial.Report.Results.Tests
			if tests[0].Message != "completed" || !strings.HasPrefix(tests[1].Message, "canceled:") || tests[2].Message != notRunCanceledReason {
				t.Errorf("tests=%+v", tests)
			}
			if !persistFailure && (persisted == nil || persisted.Results.Summary != summary) {
				t.Errorf("persisted=%+v", persisted)
			}
			if len(applied) != 2 {
				t.Errorf("deployed %d Jobs", len(applied))
			}
			deleted := 0
			for _, a := range cs.Actions() {
				if a.Matches("delete", "jobs") {
					deleted++
				}
			}
			if deleted != 2 {
				t.Errorf("deleted %d Jobs", deleted)
			}
			write, wait := -1, -1
			for i, op := range order {
				if op == "write report" {
					write = i
				}
				if op == "capture pod" && write >= 0 {
					wait = i
				}
			}
			if write < 0 || wait <= write {
				t.Errorf("report must precede pod wait: %v", order)
			}
		})
	}
}

func TestRunPhaseCanceledBeforeEntries(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cs := fake.NewSimpleClientset()
	v := New()
	cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{{Name: "not-started", Phase: "deployment"}}}
	result, err := v.runPhase(ctx, cs, nil, cat, PhaseDeployment, validationWithChecks(map[Phase][]string{PhaseDeployment: {"not-started"}}))
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if result == nil || result.Status != ctrf.StatusOther || result.Report.Results.Summary.Skipped != 1 {
		t.Fatalf("partial=%+v", result)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "jobs" {
			t.Errorf("unexpected Job action: %v", a)
		}
	}
}

func TestRunPhasesPreservesInterruptedPhase(t *testing.T) {
	v := New(WithNoCluster(true))
	calls := 0
	partial := &PhaseResult{Phase: PhaseConformance, Status: ctrf.StatusOther}
	results, err := v.runPhases(t.Context(), func(p Phase) (*PhaseResult, error) {
		calls++
		if calls == 2 {
			return partial, errors.WrapCtxErr(context.Canceled, errors.ErrCodeTimeout, "interrupted")
		}
		return &PhaseResult{Phase: p, Status: ctrf.StatusPassed}, nil
	}, nil, PhaseOrder)
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if calls != 2 || len(results) != 2 || results[1] != partial {
		t.Fatalf("calls=%d results=%v", calls, results)
	}
}

func TestPerRunCleanupRetainsSeparateBudget(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var deadline time.Time
	c := observingClient{Interface: cs, observe: func(op string, ctx context.Context) {
		if op != "delete configmap" {
			return
		}
		d, ok := ctx.Deadline()
		if !ok || ctx.Err() != nil || time.Until(d) > defaults.K8sCleanupTimeout {
			t.Errorf("unbounded or canceled cleanup context: %v", ctx)
		}
		if deadline.IsZero() {
			deadline = d
		} else if !d.Equal(deadline) {
			t.Error("per-run cleanup budget reset")
		}
	}}
	v := New(WithCleanup(true))
	if err := v.deferClusterCleanup(c); err != nil {
		t.Fatal(err)
	}
	if deadline.IsZero() {
		t.Fatal("per-run cleanup did not execute")
	}
}

func TestRunPhaseCanceledDuringDeploy(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("patch", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		cancel()
		return true, nil, context.Canceled
	})
	deleted := false
	c := observingClient{Interface: cs, observe: func(op string, ctx context.Context) {
		if op == "delete job" {
			deleted = true
			if ctx.Err() != nil {
				t.Errorf("delete used canceled context: %v", ctx.Err())
			}
		}
	}}
	v := New()
	cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{{Name: "interrupted", Phase: "deployment", Image: "example.com/check:v1"}, {Name: "not-started", Phase: "deployment", Image: "example.com/check:v1"}}}
	result, err := v.runPhase(ctx, c, nil, cat, PhaseDeployment, validationWithChecks(map[Phase][]string{PhaseDeployment: {"interrupted", "not-started"}}))
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if !deleted {
		t.Error("ambiguous Job apply was not cleaned up")
	}
	if result == nil || result.Report.Results.Summary.Other != 1 || result.Report.Results.Summary.Skipped != 1 {
		t.Fatalf("partial=%+v", result)
	}
}

func TestValidateCancellationWithRBACCleanup(t *testing.T) {
	for _, singlePhase := range []bool{false, true} {
		for _, cleanupFails := range []bool{false, true} {
			name := map[bool]string{false: "ValidatePhases", true: "ValidatePhase"}[singlePhase] + "/" + map[bool]string{false: "revoked", true: "revocation fails"}[cleanupFails]
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cs := newFakeClusterClient()
				cs.PrependReactor("patch", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
					// Cancellation after setup preserves the partial phase and
					// exercises the public entry point's deferred RBAC cleanup.
					cancel()
					return true, &corev1.ConfigMap{}, nil
				})
				revokeErr := stderrors.New("injected privileged revocation failure")
				if cleanupFails {
					cs.PrependReactor("delete", "clusterrolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
						return true, nil, revokeErr
					})
				}
				v := New(WithRunID("cancel-cleanup"), WithKubeconfig("in-memory"))
				v.kubeClientFactory = func(string) (kubernetes.Interface, error) { return cs, nil }
				var results []*PhaseResult
				var err error
				if singlePhase {
					var result *PhaseResult
					result, err = v.ValidatePhase(ctx, PhaseDeployment, nil, nil)
					results = []*PhaseResult{result}
				} else {
					results, err = v.ValidatePhases(ctx, PhaseOrder, nil, nil)
				}
				wantCode, wantExit := errors.ErrCodeCanceled, errors.ExitCanceled
				if cleanupFails {
					wantCode, wantExit = errors.ErrCodeInternal, errors.ExitInternal
					if !stderrors.Is(err, revokeErr) || !strings.Contains(err.Error(), "cluster-admin binding may be orphaned") {
						t.Errorf("privileged cleanup failure lost: %v", err)
					}
				}
				errorstest.WantReportedCode(t, err, wantCode)
				if !stderrors.Is(err, context.Canceled) || errors.ExitCodeFromError(err) != wantExit {
					t.Errorf("cancellation cause or exit code lost: %v (exit %d)", err, errors.ExitCodeFromError(err))
				}
				if len(results) != 1 || results[0] == nil || results[0].Report == nil || results[0].Status != ctrf.StatusOther {
					t.Fatalf("partial results lost or later phases ran: %+v", results)
				}
			})
		}
	}
}

func TestClusterCleanupErrorPrecedence(t *testing.T) {
	cleanupErr := errors.New(errors.ErrCodeInternal, "revocation failed")
	for _, tt := range []struct {
		name       string
		runErr     error
		cleanupErr error
		code       errors.ErrorCode
	}{
		{"success with cleanup failure", nil, cleanupErr, errors.ErrCodeInternal},
		{"ordinary cancellation", errors.WrapCtxErr(context.Canceled, errors.ErrCodeTimeout, "canceled"), nil, errors.ErrCodeCanceled},
		{"timeout precedence unchanged", errors.WrapCtxErr(context.DeadlineExceeded, errors.ErrCodeTimeout, "timeout"), cleanupErr, errors.ErrCodeTimeout},
		{"outer unavailable overrides nested cancellation", errors.Wrap(errors.ErrCodeUnavailable, "unavailable", errors.WrapCtxErr(context.Canceled, errors.ErrCodeTimeout, "inner")), cleanupErr, errors.ErrCodeUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := New().clusterCleanupError(tt.runErr, tt.cleanupErr)
			errorstest.WantReportedCode(t, err, tt.code)
			if tt.runErr != nil && tt.cleanupErr != nil && stderrors.Is(err, cleanupErr) {
				t.Errorf("non-cancellation error precedence changed: %v", err)
			}
		})
	}
}

func TestValidateCancellationWaitsForTerminatingPodBeforeRBAC(t *testing.T) {
	for _, exhaustBudget := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal observation", true: "shared budget exhausted"}[exhaustBudget], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cs := newFakeClusterClient()
			var p *corev1.Pod
			cs.PrependReactor("patch", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
				var j batchv1.Job
				if err := json.Unmarshal(a.(clienttesting.PatchAction).GetPatch(), &j); err != nil {
					t.Error(err)
					return true, nil, err
				}
				if err := cs.Tracker().Add(&j); err != nil {
					t.Error(err)
					return true, nil, err
				}
				p = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-pod", Namespace: j.Namespace, Labels: map[string]string{"batch.kubernetes.io/job-name": j.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
				if err := cs.Tracker().Add(p); err != nil {
					t.Error(err)
					return true, nil, err
				}
				return true, &j, nil
			})
			cs.PrependWatchReactor("jobs", func(clienttesting.Action) (bool, watch.Interface, error) {
				return true, watch.NewRaceFreeFake(), nil
			})
			cs.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
				p = p.DeepCopy()
				now := metav1.Now()
				p.DeletionTimestamp = &now
				if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
					t.Error(err)
					return true, nil, err
				}
				return false, nil, nil // The Job disappears, but its pod is still Running.
			})
			reported := false
			cs.PrependReactor("patch", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
				reported = true
				return true, &corev1.ConfigMap{}, nil
			})
			w := watch.NewRaceFreeFake()
			defer w.Stop()
			cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
				return true, w, nil
			})
			ready := make(chan context.Context, 1)
			var waitCtx context.Context
			var cleanupDeadline time.Time
			observing := observingClient{Interface: cs, observe: func(op string, c context.Context) {
				if op == "watch job" {
					cancel()
					return
				}
				if ctx.Err() == nil || op == "delete configmap" {
					return
				}
				d, ok := c.Deadline()
				if !ok || c.Err() != nil || time.Until(d) > defaults.K8sCleanupTimeout {
					t.Errorf("%s used canceled/unbounded cleanup context", op)
				}
				if cleanupDeadline.IsZero() {
					cleanupDeadline = d
				} else if !cleanupDeadline.Equal(d) {
					t.Errorf("%s renewed shared budget", op)
				}
				if op == "wait pod" {
					if !reported {
						t.Error("termination wait overtook report persistence")
					}
					waitCtx = c
					ready <- c
				}
			}}
			revoked := make(chan struct{}, 1)
			cs.PrependReactor("delete", "clusterrolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
				switch {
				case waitCtx == nil:
					t.Error("RBAC cleanup overtook termination wait")
				case exhaustBudget:
					if !stderrors.Is(waitCtx.Err(), context.DeadlineExceeded) {
						t.Errorf("RBAC cleanup preceded real budget exhaustion: %v", waitCtx.Err())
					}
				default:
					obj, err := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), p.Namespace, p.Name)
					if err != nil || obj.(*corev1.Pod).Status.Phase != corev1.PodSucceeded {
						t.Errorf("RBAC cleanup preceded terminal pod: %v %v", obj, err)
					}
				}
				revoked <- struct{}{}
				return false, nil, nil
			})
			v := New(WithRunID("terminating-pod"), WithKubeconfig("in-memory"))
			v.kubeClientFactory = func(string) (kubernetes.Interface, error) { return observing, nil }
			type outcome struct {
				result *PhaseResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := v.ValidatePhase(ctx, PhaseDeployment, validationWithChecks(map[Phase][]string{PhaseDeployment: {"gpu-operator-version"}}), nil)
				done <- outcome{result, err}
			}()
			// Every exit releases and joins the validation goroutine, including
			// a broken implementation that never opens its termination watch.
			defer func() {
				w.Stop()
				select {
				case <-done:
				case <-time.After(defaults.K8sCleanupTimeout + 5*time.Second):
					t.Error("validation goroutine did not stop")
				}
			}()
			select {
			case <-ready:
			case got := <-done:
				done <- got
				t.Fatalf("returned before waiting for terminating Running pod: %v", got.err)
			case <-time.After(5 * time.Second):
				t.Fatal("termination watch not opened")
			}
			select {
			case <-revoked:
				t.Fatal("RBAC revoked while pod still Running")
			default:
			}
			if !exhaustBudget {
				terminal := p.DeepCopy()
				terminal.Status.Phase = corev1.PodSucceeded
				if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), terminal, terminal.Namespace); err != nil {
					t.Fatal(err)
				}
				w.Modify(terminal)
			}
			select {
			case got := <-done:
				done <- got
				errorstest.WantReportedCode(t, got.err, errors.ErrCodeCanceled)
				if got.result == nil || got.result.Status != ctrf.StatusOther {
					t.Fatalf("partial results lost: %+v", got.result)
				}
			case <-time.After(defaults.K8sCleanupTimeout + 5*time.Second):
				t.Fatal("termination wait exceeded shared cleanup budget")
			}
			select {
			case <-revoked:
			default:
				t.Fatal("privileged RBAC cleanup was skipped")
			}
		})
	}
}

func TestValidateCancellationDuringPodTerminationWait(t *testing.T) {
	for _, tt := range []struct {
		name        string
		cancel      bool
		terminating bool
		disposition string
	}{
		{"running to terminal", true, false, "terminal"},
		{"terminating to deleted", true, true, "deleted"},
		{"terminating exhausts shared budget", true, true, "deadline"},
		{"normal wait unchanged", false, true, "terminal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cs := newFakeClusterClient()
				var p *corev1.Pod
				applied := 0
				cs.PrependReactor("patch", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
					var j batchv1.Job
					if err := json.Unmarshal(a.(clienttesting.PatchAction).GetPatch(), &j); err != nil {
						t.Error(err)
						return true, nil, err
					}
					applied++
					if err := cs.Tracker().Add(&j); err != nil {
						t.Error(err)
						return true, nil, err
					}
					p = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-pod", Namespace: j.Namespace, Labels: map[string]string{"batch.kubernetes.io/job-name": j.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
					if err := cs.Tracker().Add(p); err != nil {
						t.Error(err)
						return true, nil, err
					}
					return true, &j, nil
				})
				cs.PrependWatchReactor("jobs", func(clienttesting.Action) (bool, watch.Interface, error) {
					return true, nil, apierrors.NewServiceUnavailable("injected wait failure")
				})
				deleted := false
				cs.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
					deleted = true
					if tt.terminating {
						p = p.DeepCopy()
						now := metav1.Now()
						p.DeletionTimestamp = &now
						if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
							t.Error(err)
							return true, nil, err
						}
					}
					return false, nil, nil
				})
				var reportCtx context.Context
				var persisted *ctrf.Report
				cs.PrependReactor("patch", "configmaps", func(a clienttesting.Action) (bool, runtime.Object, error) {
					var cm corev1.ConfigMap
					if err := json.Unmarshal(a.(clienttesting.PatchAction).GetPatch(), &cm); err != nil {
						t.Error(err)
						return true, nil, err
					}
					persisted = &ctrf.Report{}
					if err := json.Unmarshal([]byte(cm.Data[ctrf.ConfigMapKeyReport]), persisted); err != nil {
						t.Error(err)
						return true, nil, err
					}
					// Virtual I/O latency consumes the real production context's
					// budget without sleeping or changing its 30s allowance.
					<-time.After(5 * time.Second)
					return true, &cm, nil
				})
				type podWatch struct {
					ctx context.Context
					w   *watch.RaceFreeFakeWatcher
				}
				ready := make(chan podWatch, 2)
				var waitCtx context.Context
				observing := observingClient{Interface: cs, observe: func(op string, c context.Context) {
					switch op {
					case "write report":
						reportCtx = c
					case "wait pod":
						waitCtx = c
						if ctx.Err() != nil {
							if persisted == nil || reportCtx != c {
								t.Error("detached wait did not follow report persistence in the same context")
							}
						}
					}
				}}
				cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
					w := watch.NewRaceFreeFake()
					ready <- podWatch{waitCtx, w}
					return true, w, nil
				})
				var revoked atomic.Bool
				cs.PrependReactor("delete", "clusterrolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
					revoked.Store(true)
					obj, err := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), p.Namespace, p.Name)
					switch tt.disposition {
					case "deadline":
						if waitCtx == nil || !stderrors.Is(waitCtx.Err(), context.DeadlineExceeded) {
							t.Error("RBAC overtook honest shared deadline exhaustion")
						}
					case "deleted":
						if !apierrors.IsNotFound(err) {
							t.Errorf("RBAC overtook pod deletion: %v", err)
						}
					default:
						if err != nil || obj.(*corev1.Pod).Status.Phase != corev1.PodSucceeded {
							t.Errorf("RBAC overtook terminal pod: %v %v", obj, err)
						}
					}
					return false, nil, nil
				})
				v := New(WithRunID("midwait"), WithKubeconfig("in-memory"))
				v.kubeClientFactory = func(string) (kubernetes.Interface, error) { return observing, nil }
				checks := []string{"gpu-operator-version"}
				if tt.cancel {
					checks = append(checks, "check-nvidia-smi")
				}
				type outcome struct {
					result *PhaseResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					result, err := v.ValidatePhase(ctx, PhaseDeployment, validationWithChecks(map[Phase][]string{PhaseDeployment: checks}), nil)
					done <- outcome{result, err}
				}()
				nextWatch := func() podWatch {
					t.Helper()
					select {
					case opened := <-ready:
						return opened
					case got := <-done:
						t.Fatalf("returned without cooperative pod wait: %v", got.err)
						return podWatch{}
					}
				}
				opened := nextWatch()
				synctest.Wait() // The real termination watch is active, not a stubbed wait.
				if !deleted || ctx.Err() != nil || persisted != nil || revoked.Load() {
					t.Fatal("first watch was not the normal live-parent wait after deletion")
				}
				if d, ok := opened.ctx.Deadline(); !ok || time.Until(d) != defaults.K8sPodTerminationWaitTimeout {
					t.Fatalf("normal wait deadline=%v", d)
				}
				if tt.cancel {
					<-time.After(7 * time.Second)
					canceledAt := time.Now()
					cancel()
					opened = nextWatch()
					synctest.Wait()
					deadline, ok := opened.ctx.Deadline()
					if !ok || !deadline.Equal(canceledAt.Add(defaults.K8sCleanupTimeout)) || time.Until(deadline) != defaults.K8sCleanupTimeout-5*time.Second || opened.ctx.Err() != nil {
						t.Fatalf("continuation did not share remaining detached report budget: %v (%v)", deadline, opened.ctx.Err())
					}
				}
				if revoked.Load() {
					t.Fatal("RBAC revoked while pod was nonterminal")
				}
				switch tt.disposition {
				case "deleted":
					if err := cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), p.Namespace, p.Name); err != nil {
						t.Fatal(err)
					}
					opened.w.Delete(p)
				case "terminal":
					terminal := p.DeepCopy()
					terminal.Status.Phase = corev1.PodSucceeded
					if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), terminal, terminal.Namespace); err != nil {
						t.Fatal(err)
					}
					opened.w.Modify(terminal)
				}
				got := <-done // The deadline case advances virtual time, not wall time.
				if tt.cancel {
					errorstest.WantReportedCode(t, got.err, errors.ErrCodeCanceled)
					if !stderrors.Is(got.err, context.Canceled) {
						t.Errorf("original cancellation cause lost: %v", got.err)
					}
				} else if got.err != nil {
					t.Fatal(got.err)
				}
				if !revoked.Load() || applied != 1 || got.result == nil || got.result.Status != ctrf.StatusOther || persisted == nil {
					t.Fatalf("revoked=%v applied=%d result=%+v persisted=%+v", revoked.Load(), applied, got.result, persisted)
				}
				tests := got.result.Report.Results.Tests
				if !strings.Contains(tests[0].Message, "injected wait failure") || persisted.Results.Summary != got.result.Report.Results.Summary {
					t.Errorf("captured infrastructure result or partial persistence lost: %+v", tests)
				}
				if tt.cancel && (len(tests) != 2 || tests[1].Status != ctrf.StatusSkipped || tests[1].Message != notRunCanceledReason) {
					t.Errorf("unstarted check not retained: %+v", tests)
				}
			})
		})
	}
}
