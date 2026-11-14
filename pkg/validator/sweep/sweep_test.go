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

package sweep

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator/job"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
)

const (
	testNamespace = defaults.ValidationNamespace
	testOlderThan = 2 * time.Hour

	old   = 3 * time.Hour    // past the floor: eligible
	young = 30 * time.Minute // below the floor: kept

	// liveRun still has a validator Job (the liveness guard lists Jobs
	// cluster-wide); deadRun and deadRun2 do not.
	liveRun  = "20260514-123045-aaaabbbbccccdddd"
	deadRun  = "20260513-000000-1111222233334444"
	deadRun2 = "20260512-010203-5555666677778888"

	hexToken32a = "0123456789abcdef0123456789abcdef"
	hexToken32b = "fedcba9876543210fedcba9876543210"
	hexToken8   = "0123abcd"
)

var testPodGroupListKinds = map[schema.GroupVersionResource]string{podGroupGVR: "PodGroupList"}

func created(age time.Duration) metav1.Time {
	return metav1.NewTime(time.Now().Add(-age))
}

func testOptions() Options {
	return Options{Namespace: testNamespace, OlderThan: testOlderThan}
}

// --- object builders -------------------------------------------------------

func testNamespaceObj(name, token string, age time.Duration) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:              name,
		UID:               types.UID("uid-" + name),
		CreationTimestamp: created(age),
		Labels:            map[string]string{labels.GPUTestRun: token},
	}}
}

func validatorSA(runID string, age time.Duration) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:              job.ServiceAccountName(runID),
		Namespace:         testNamespace,
		UID:               types.UID("uid-sa-" + runID),
		CreationTimestamp: created(age),
		Labels:            map[string]string{labels.Name: labels.ValueValidator, labels.ManagedBy: labels.ValueAICR},
	}}
}

func validatorCRB(runID string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:              job.ClusterRoleBindingName(runID),
			UID:               types.UID("uid-crb-" + runID),
			CreationTimestamp: created(old),
			Labels:            map[string]string{labels.Name: labels.ValueValidator, labels.ManagedBy: labels.ValueAICR},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      job.ServiceAccountName(runID),
			Namespace: testNamespace,
		}},
	}
}

func dataConfigMap(name, runID string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:              name,
		Namespace:         testNamespace,
		UID:               types.UID("uid-cm-" + name),
		CreationTimestamp: created(old),
		Labels: map[string]string{
			labels.Name:      labels.ValueAICR,
			labels.Component: labels.ValueValidation,
			labels.ManagedBy: labels.ValueAICR,
			labels.RunID:     runID,
		},
	}}
}

func agentLabels(runID string) map[string]string {
	return map[string]string{
		labels.Name:      labels.ValueAICR,
		labels.Component: labels.ValueSnapshotAgent,
		labels.ManagedBy: labels.ValueAICR,
		labels.RunID:     runID,
	}
}

func agentSA(name, runID string, age time.Duration) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace, UID: types.UID("uid-" + name),
		CreationTimestamp: created(age), Labels: agentLabels(runID),
	}}
}

func agentRoleBinding(name, runID string, age time.Duration) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace, UID: types.UID("uid-" + name),
		CreationTimestamp: created(age), Labels: agentLabels(runID),
	}}
}

func agentClusterRole(name, runID string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: types.UID("uid-" + name),
		CreationTimestamp: created(old), Labels: agentLabels(runID),
	}}
}

func agentClusterRoleBinding(name, runID string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: types.UID("uid-" + name),
		CreationTimestamp: created(old), Labels: agentLabels(runID),
	}}
}

func validatorJob(namespace, runID string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:              "aicr-job-" + runID,
		Namespace:         namespace,
		CreationTimestamp: created(time.Minute),
		Labels:            map[string]string{labels.RunID: runID},
	}}
}

func gangPod(namespace, name, token string, age time.Duration) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, UID: types.UID("uid-" + namespace + "-" + name),
		CreationTimestamp: created(age),
		Labels:            map[string]string{labels.GPUTestRun: token, labels.GangSchedulingPhase: "negative"},
	}}
}

// podGroup builds the one Run:ai PodGroup the residue tests exercise: the
// negative-phase group of a run whose token is hexToken32a, in the Project
// namespace, old enough to sweep.
func podGroup() *unstructured.Unstructured {
	return podGroupNamed("gang-neg-group-"+hexToken32a, hexToken32a)
}

// podGroupNamed builds a Run:ai PodGroup of any name carrying token, in the
// Project namespace, old enough to sweep.
func podGroupNamed(name, token string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "scheduling.run.ai/v2alpha2",
		"kind":       "PodGroup",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         "runai-team-a",
			"uid":               "uid-pg-" + name,
			"creationTimestamp": created(old).UTC().Format(time.RFC3339),
			"labels": map[string]any{
				labels.GPUTestRun:          token,
				labels.GangSchedulingPhase: "negative",
			},
		},
	}}
}

func livePod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: created(time.Minute)},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// --- helpers ---------------------------------------------------------------

// planWith runs Plan against fakes holding the given typed and unstructured
// objects.
func planWith(t *testing.T, opts Options, typed []runtime.Object, dynamicObjs ...*unstructured.Unstructured) (*Report, *k8sfake.Clientset) {
	t.Helper()
	cs := k8sfake.NewSimpleClientset(typed...)
	dynObjs := make([]runtime.Object, 0, len(dynamicObjs))
	for _, o := range dynamicObjs {
		dynObjs = append(dynObjs, o)
	}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds, dynObjs...)
	report, err := Plan(context.Background(), cs, dyn, opts)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	return report, cs
}

// find returns the planned object for kind/name, or nil.
func find(r *Report, kind, name string) *Object {
	for i := range r.Objects {
		if r.Objects[i].Kind == kind && r.Objects[i].Name == name {
			return &r.Objects[i]
		}
	}
	return nil
}

// wantStatus asserts the one object's planned status and, optionally, that
// its reason contains a fragment.
func wantStatus(t *testing.T, r *Report, kind, name string, status Status, reasonFragment string) {
	t.Helper()
	obj := find(r, kind, name)
	if obj == nil {
		t.Fatalf("%s %s not in plan: %+v", kind, name, r.Objects)
	}
	if obj.Status != status {
		t.Errorf("%s %s status = %q, want %q (reason %q)", kind, name, obj.Status, status, obj.Reason)
	}
	if reasonFragment != "" && !strings.Contains(obj.Reason, reasonFragment) {
		t.Errorf("%s %s reason = %q, want it to contain %q", kind, name, obj.Reason, reasonFragment)
	}
}

// useSettle overrides the settle-observation knobs with test-scale values,
// restored at cleanup.
func useSettle(t *testing.T, window, pause time.Duration) {
	t.Helper()
	oldWindow, oldPause := settleWindow, settleRewatchPause
	settleWindow, settleRewatchPause = window, pause
	t.Cleanup(func() { settleWindow, settleRewatchPause = oldWindow, oldPause })
}

// noSettle removes the settle wait for tests that do not exercise it: a
// zero window elapses as soon as the observation starts, so Execute goes
// straight to the delete loop.
func noSettle(t *testing.T) {
	t.Helper()
	useSettle(t, 0, time.Second)
}

// deleteActions records the fake's delete calls in order.
func deleteActions(cs *k8sfake.Clientset) []k8stesting.DeleteAction {
	var out []k8stesting.DeleteAction
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok {
			out = append(out, d)
		}
	}
	return out
}

// --- Plan: namespaces ------------------------------------------------------

func TestPlanCheckNamespaces(t *testing.T) {
	tests := []struct {
		name           string
		namespace      *corev1.Namespace
		pods           []*corev1.Pod
		wantStatus     Status
		reasonFragment string
	}{
		{
			name:           "young namespace kept",
			namespace:      testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, young),
			wantStatus:     StatusSkipped,
			reasonFragment: "below the --older-than floor",
		},
		{
			name:       "old GPU test namespace selected",
			namespace:  testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old),
			wantStatus: StatusWouldDelete,
		},
		{
			name:       "old gang-scheduling namespace selected",
			namespace:  testNamespaceObj(defaults.GangSchedulingTestNamespacePrefix+hexToken32b, hexToken32b, old),
			wantStatus: StatusWouldDelete,
		},
		{
			name:       "old cluster-autoscaling namespace selected",
			namespace:  testNamespaceObj(defaults.ClusterAutoscalingTestNamespacePrefix+hexToken8, hexToken8, old),
			wantStatus: StatusWouldDelete,
		},
		{
			name:           "cluster-autoscaling token with 32-hex shape kept",
			namespace:      testNamespaceObj(defaults.ClusterAutoscalingTestNamespacePrefix+hexToken32a, hexToken32a, old),
			wantStatus:     StatusSkipped,
			reasonFragment: reasonForeignNamespace,
		},
		{
			name:           "GPU prefix with 8-hex token kept",
			namespace:      testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken8, hexToken8, old),
			wantStatus:     StatusSkipped,
			reasonFragment: reasonForeignNamespace,
		},
		{
			name:           "name does not reconstruct from label value",
			namespace:      testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32b, old),
			wantStatus:     StatusSkipped,
			reasonFragment: reasonForeignNamespace,
		},
		{
			name:           "labeled namespace without a known prefix kept",
			namespace:      testNamespaceObj("team-production", hexToken32a, old),
			wantStatus:     StatusSkipped,
			reasonFragment: reasonForeignNamespace,
		},
		{
			name:           "non-hex label value kept",
			namespace:      testNamespaceObj(defaults.GPUTestNamespacePrefix+"ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ", "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ", old),
			wantStatus:     StatusSkipped,
			reasonFragment: reasonForeignNamespace,
		},
		{
			name: "Run:ai Project namespace never swept even with a valid name",
			namespace: func() *corev1.Namespace {
				ns := testNamespaceObj(defaults.GangSchedulingTestNamespacePrefix+hexToken32a, hexToken32a, old)
				ns.Labels[labels.RunAIQueue] = "team-a"
				return ns
			}(),
			wantStatus:     StatusSkipped,
			reasonFragment: "Run:ai Project namespace is never swept",
		},
		{
			name:           "namespace with a live pod kept",
			namespace:      testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old),
			pods:           []*corev1.Pod{livePod(defaults.GPUTestNamespacePrefix+hexToken32a, "gpu-alloc-test-x")},
			wantStatus:     StatusSkipped,
			reasonFragment: "still has live pods",
		},
		{
			name:      "namespace with only terminal pods selected",
			namespace: testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old),
			pods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "done", Namespace: defaults.GPUTestNamespacePrefix + hexToken32a}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
				{ObjectMeta: metav1.ObjectMeta{Name: "failed", Namespace: defaults.GPUTestNamespacePrefix + hexToken32a}, Status: corev1.PodStatus{Phase: corev1.PodFailed}},
			},
			wantStatus: StatusWouldDelete,
		},
		{
			name: "namespace with a running pod already being deleted selected",
			namespace: func() *corev1.Namespace {
				return testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old)
			}(),
			pods: []*corev1.Pod{func() *corev1.Pod {
				p := livePod(defaults.GPUTestNamespacePrefix+hexToken32a, "terminating-pod")
				p.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				return p
			}()},
			wantStatus: StatusWouldDelete,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typed := []runtime.Object{tt.namespace}
			for _, p := range tt.pods {
				typed = append(typed, p)
			}
			report, _ := planWith(t, testOptions(), typed)
			wantStatus(t, report, kindNamespace, tt.namespace.Name, tt.wantStatus, tt.reasonFragment)
		})
	}
}

func TestPlanTerminatingNamespaceReportedNotDeleted(t *testing.T) {
	ns := testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old)
	ts := metav1.NewTime(time.Now().Add(-time.Hour))
	ns.DeletionTimestamp = &ts
	ns.Spec.Finalizers = []corev1.FinalizerName{"kubernetes", "example.com/hold"}
	ns.Status.Conditions = []corev1.NamespaceCondition{
		{Type: corev1.NamespaceDeletionContentFailure, Status: corev1.ConditionTrue},
	}

	report, cs := planWith(t, testOptions(), []runtime.Object{ns})
	obj := find(report, kindNamespace, ns.Name)
	if obj == nil {
		t.Fatalf("terminating namespace not reported: %+v", report.Objects)
	}
	if obj.Status != StatusTerminating {
		t.Errorf("status = %q, want %q", obj.Status, StatusTerminating)
	}
	for _, fragment := range []string{"kubernetes", "example.com/hold", "NamespaceDeletionContentFailure=True"} {
		if !strings.Contains(obj.Reason, fragment) {
			t.Errorf("reason %q does not name %s", obj.Reason, fragment)
		}
	}
	if len(deleteActions(cs)) != 0 {
		t.Errorf("a plan must not delete; got deletes %v", deleteActions(cs))
	}
}

// --- Plan: run-scoped RBAC and ConfigMaps ----------------------------------

func TestPlanValidatorRBAC(t *testing.T) {
	tests := []struct {
		name           string
		objects        []runtime.Object
		kind           string
		objName        string
		wantStatus     Status
		reasonFragment string
	}{
		{
			name:       "dead run ServiceAccount selected",
			objects:    []runtime.Object{validatorSA(deadRun, old)},
			kind:       kindServiceAccount,
			objName:    job.ServiceAccountName(deadRun),
			wantStatus: StatusWouldDelete,
		},
		{
			name:       "dead run ClusterRoleBinding selected",
			objects:    []runtime.Object{validatorCRB(deadRun)},
			kind:       kindClusterRoleBinding,
			objName:    job.ClusterRoleBindingName(deadRun),
			wantStatus: StatusWouldDelete,
		},
		{
			name:           "live run ServiceAccount kept",
			objects:        []runtime.Object{validatorSA(liveRun, old), validatorJob(testNamespace, liveRun)},
			kind:           kindServiceAccount,
			objName:        job.ServiceAccountName(liveRun),
			wantStatus:     StatusSkipped,
			reasonFragment: "still has a validator Job",
		},
		{
			name:           "live run ClusterRoleBinding kept",
			objects:        []runtime.Object{validatorCRB(liveRun), validatorJob(testNamespace, liveRun)},
			kind:           kindClusterRoleBinding,
			objName:        job.ClusterRoleBindingName(liveRun),
			wantStatus:     StatusSkipped,
			reasonFragment: "still has a validator Job",
		},
		{
			name:           "young ServiceAccount kept",
			objects:        []runtime.Object{validatorSA(deadRun, young)},
			kind:           kindServiceAccount,
			objName:        job.ServiceAccountName(deadRun),
			wantStatus:     StatusSkipped,
			reasonFragment: "below the --older-than floor",
		},
		{
			name: "ClusterRoleBinding with a wrong roleRef kept",
			objects: []runtime.Object{func() *rbacv1.ClusterRoleBinding {
				crb := validatorCRB(deadRun)
				crb.RoleRef.Name = "edit"
				return crb
			}()},
			kind:           kindClusterRoleBinding,
			objName:        job.ClusterRoleBindingName(deadRun),
			wantStatus:     StatusSkipped,
			reasonFragment: "roleRef or subjects",
		},
		{
			// The apiserver rejects a ClusterRoleBinding whose roleRef kind or
			// apiGroup differs, so these two cases cannot occur on a real
			// cluster; the sweeper still fails closed on them.
			name: "ClusterRoleBinding with a non-ClusterRole roleRef kind kept",
			objects: []runtime.Object{func() *rbacv1.ClusterRoleBinding {
				crb := validatorCRB(deadRun)
				crb.RoleRef.Kind = "Role"
				return crb
			}()},
			kind:           kindClusterRoleBinding,
			objName:        job.ClusterRoleBindingName(deadRun),
			wantStatus:     StatusSkipped,
			reasonFragment: "roleRef or subjects",
		},
		{
			name: "ClusterRoleBinding with a foreign roleRef apiGroup kept",
			objects: []runtime.Object{func() *rbacv1.ClusterRoleBinding {
				crb := validatorCRB(deadRun)
				crb.RoleRef.APIGroup = "example.com"
				return crb
			}()},
			kind:           kindClusterRoleBinding,
			objName:        job.ClusterRoleBindingName(deadRun),
			wantStatus:     StatusSkipped,
			reasonFragment: "roleRef or subjects",
		},
		{
			name: "ClusterRoleBinding with two subjects kept",
			objects: []runtime.Object{func() *rbacv1.ClusterRoleBinding {
				crb := validatorCRB(deadRun)
				crb.Subjects = append(crb.Subjects, rbacv1.Subject{Kind: "User", Name: "mallory"})
				return crb
			}()},
			kind:           kindClusterRoleBinding,
			objName:        job.ClusterRoleBindingName(deadRun),
			wantStatus:     StatusSkipped,
			reasonFragment: "roleRef or subjects",
		},
		{
			name: "ClusterRoleBinding with a subject in another namespace kept",
			objects: []runtime.Object{func() *rbacv1.ClusterRoleBinding {
				crb := validatorCRB(deadRun)
				crb.Subjects[0].Namespace = "elsewhere"
				return crb
			}()},
			kind:           kindClusterRoleBinding,
			objName:        job.ClusterRoleBindingName(deadRun),
			wantStatus:     StatusSkipped,
			reasonFragment: "roleRef or subjects",
		},
		{
			name: "validator-labeled ServiceAccount with an unrecognized name kept",
			objects: []runtime.Object{func() *corev1.ServiceAccount {
				sa := validatorSA(deadRun, old)
				sa.Name = "aicr-validator"
				return sa
			}()},
			kind:           kindServiceAccount,
			objName:        "aicr-validator",
			wantStatus:     StatusSkipped,
			reasonFragment: "not a validator run name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, _ := planWith(t, testOptions(), tt.objects)
			wantStatus(t, report, tt.kind, tt.objName, tt.wantStatus, tt.reasonFragment)
		})
	}
}

func TestPlanConfigMapsAndAgentObjectsGroupByRun(t *testing.T) {
	objects := []runtime.Object{
		validatorJob(testNamespace, liveRun),
		// Dead run: everything selected.
		dataConfigMap("aicr-snapshot-"+deadRun, deadRun),
		dataConfigMap("aicr-validation-"+deadRun, deadRun),
		dataConfigMap("aicr-ctrf-"+deadRun+"-deployment", deadRun),
		agentSA("aicr-validate-"+deadRun, deadRun, old),
		agentRoleBinding("aicr-validate-"+deadRun, deadRun, old),
		agentClusterRole("aicr-node-reader-"+deadRun, deadRun),
		// Live run: everything kept.
		dataConfigMap("aicr-snapshot-"+liveRun, liveRun),
		agentSA("aicr-validate-"+liveRun, liveRun, old),
		agentClusterRole("aicr-node-reader-"+liveRun, liveRun),
		// No run-id label: kept, the liveness guard cannot run.
		func() *corev1.ConfigMap {
			cm := dataConfigMap("aicr-snapshot-unlabeled", "")
			delete(cm.Labels, labels.RunID)
			return cm
		}(),
	}
	report, _ := planWith(t, testOptions(), objects)

	for _, name := range []string{
		"aicr-snapshot-" + deadRun, "aicr-validation-" + deadRun, "aicr-ctrf-" + deadRun + "-deployment",
	} {
		wantStatus(t, report, kindConfigMap, name, StatusWouldDelete, "")
	}
	for kind, name := range map[string]string{
		kindServiceAccount: "aicr-validate-" + deadRun,
		kindRoleBinding:    "aicr-validate-" + deadRun,
		kindClusterRole:    "aicr-node-reader-" + deadRun,
	} {
		wantStatus(t, report, kind, name, StatusWouldDelete, "")
	}
	wantStatus(t, report, kindConfigMap, "aicr-snapshot-"+liveRun, StatusSkipped, "still has a validator Job")
	wantStatus(t, report, kindServiceAccount, "aicr-validate-"+liveRun, StatusSkipped, "still has a validator Job")
	wantStatus(t, report, kindClusterRole, "aicr-node-reader-"+liveRun, StatusSkipped, "still has a validator Job")
	wantStatus(t, report, kindConfigMap, "aicr-snapshot-unlabeled", StatusSkipped, "missing the "+labels.RunID+" label")
}

func TestPlanExcludesOperatorOwnedAgentRBAC(t *testing.T) {
	// The RBAC that `aicr snapshot --add-roles-to-service-account` renders is
	// operator-owned (component=agent-rbac) and carries no run ID; the
	// component label alone must keep it out of the plan entirely.
	agentRBAC := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name:              "aicr-node-reader",
		CreationTimestamp: created(old),
		Labels: map[string]string{
			labels.Name:      labels.ValueAICR,
			labels.Component: labels.ValueAgentRBAC,
			labels.ManagedBy: labels.ValueAICR,
		},
	}}
	report, _ := planWith(t, testOptions(), []runtime.Object{agentRBAC})
	if obj := find(report, kindClusterRole, "aicr-node-reader"); obj != nil {
		t.Errorf("operator-owned agent RBAC entered the plan: %+v", obj)
	}
}

// --- Plan: Run:ai residue --------------------------------------------------

func TestPlanRunAIResidue(t *testing.T) {
	projectNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:              "runai-team-a",
		CreationTimestamp: created(100 * 24 * time.Hour),
		Labels:            map[string]string{labels.RunAIQueue: "team-a"},
	}}
	checkNS := testNamespaceObj(defaults.GangSchedulingTestNamespacePrefix+hexToken32b, hexToken32b, young)

	objects := []runtime.Object{
		projectNS, checkNS,
		gangPod("runai-team-a", "gang-neg-worker-"+hexToken32a+"-0", hexToken32a, old),
		gangPod("runai-team-a", "gang-neg-worker-"+hexToken32a+"-1", hexToken32a, young),
		gangPod(defaults.GangSchedulingTestNamespacePrefix+hexToken32b, "gang-worker-"+hexToken32b+"-0", hexToken32b, old),
		gangPod("runai-team-a", "gang-bad-token-0", hexToken8, old),
	}
	groups := []*unstructured.Unstructured{
		podGroup(),
	}

	report, _ := planWith(t, testOptions(), objects, groups...)

	wantStatus(t, report, kindPod, "gang-neg-worker-"+hexToken32a+"-0", StatusWouldDelete, "")
	wantStatus(t, report, kindPodGroup, "gang-neg-group-"+hexToken32a, StatusWouldDelete, "")
	wantStatus(t, report, kindPod, "gang-neg-worker-"+hexToken32a+"-1", StatusSkipped, "below the --older-than floor")
	wantStatus(t, report, kindPod, "gang-worker-"+hexToken32b+"-0", StatusSkipped, "inside a validator test namespace")
	wantStatus(t, report, kindPod, "gang-bad-token-0", StatusSkipped, reasonForeignNamespace)

	// The Project namespace itself is never a candidate.
	for _, o := range report.Objects {
		if o.Kind == kindNamespace && o.Name == "runai-team-a" {
			t.Errorf("Run:ai Project namespace entered the plan: %+v", o)
		}
	}
}

// TestPlanRunAIResidueNameShapes pins the name half of the residue
// selector: both labels AND a name the gang-scheduling check writes for the
// label's token, or the object is foreign and kept.
func TestPlanRunAIResidueNameShapes(t *testing.T) {
	projectNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:              "runai-team-a",
		CreationTimestamp: created(100 * 24 * time.Hour),
		Labels:            map[string]string{labels.RunAIQueue: "team-a"},
	}}

	tests := []struct {
		name           string
		kind           string
		objName        string
		token          string
		wantStatus     Status
		reasonFragment string
	}{
		{"positive worker pod", kindPod, "gang-worker-" + hexToken32a + "-0", hexToken32a, StatusWouldDelete, ""},
		{"negative worker pod", kindPod, "gang-neg-worker-" + hexToken32a + "-1", hexToken32a, StatusWouldDelete, ""},
		{"positive group", kindPodGroup, "gang-group-" + hexToken32a, hexToken32a, StatusWouldDelete, ""},
		{"negative group", kindPodGroup, "gang-neg-group-" + hexToken32a, hexToken32a, StatusWouldDelete, ""},
		{"user pod carrying both labels", kindPod, "my-training-pod", hexToken32a, StatusSkipped, reasonForeignNamespace},
		{"label value does not match the name suffix", kindPod, "gang-worker-" + hexToken32b + "-0", hexToken32a, StatusSkipped, reasonForeignNamespace},
		{"non-numeric member index", kindPod, "gang-worker-" + hexToken32a + "-x", hexToken32a, StatusSkipped, reasonForeignNamespace},
		{"missing member index", kindPod, "gang-worker-" + hexToken32a + "-", hexToken32a, StatusSkipped, reasonForeignNamespace},
		{"group name with an extra suffix", kindPodGroup, "gang-group-" + hexToken32a + "-0", hexToken32a, StatusSkipped, reasonForeignNamespace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var typed []runtime.Object
			var groups []*unstructured.Unstructured
			switch tt.kind {
			case kindPod:
				typed = []runtime.Object{projectNS, gangPod("runai-team-a", tt.objName, tt.token, old)}
			default:
				typed = []runtime.Object{projectNS}
				groups = []*unstructured.Unstructured{podGroupNamed(tt.objName, tt.token)}
			}
			report, _ := planWith(t, testOptions(), typed, groups...)
			wantStatus(t, report, tt.kind, tt.objName, tt.wantStatus, tt.reasonFragment)
		})
	}
}

func TestPlanToleratesAbsentPodGroupCRD(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(gangPod("runai-team-a", "gang-neg-worker-"+hexToken32a+"-0", hexToken32a, old))
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
	dyn.PrependReactor("list", "podgroups", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: podGroupGVR.Group, Resource: podGroupGVR.Resource}, "")
	})

	report, err := Plan(context.Background(), cs, dyn, testOptions())
	if err != nil {
		t.Fatalf("Plan() with an absent PodGroup CRD = %v, want nil", err)
	}
	wantStatus(t, report, kindPod, "gang-neg-worker-"+hexToken32a+"-0", StatusWouldDelete, "")
}

// --- Plan: validation, classification, no deletes --------------------------

func TestPlanRejectsNonPositiveOlderThan(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Hour} {
		t.Run(fmt.Sprintf("olderThan=%v", d), func(t *testing.T) {
			cs := k8sfake.NewSimpleClientset()
			dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
			_, err := Plan(context.Background(), cs, dyn, Options{Namespace: testNamespace, OlderThan: d})
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

func TestPlanDefaultsNamespace(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
	report, err := Plan(context.Background(), cs, dyn, Options{OlderThan: testOlderThan})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if report.Namespace != defaults.ValidationNamespace {
		t.Errorf("Namespace = %q, want %q", report.Namespace, defaults.ValidationNamespace)
	}
}

func TestPlanForbiddenListIsUnauthorized(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", fmt.Errorf("denied"))
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
	_, err := Plan(context.Background(), cs, dyn, testOptions())
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnauthorized)
	if !strings.Contains(err.Error(), "list") || !strings.Contains(err.Error(), "namespaces") {
		t.Errorf("error %q should name the verb and resource", err)
	}
}

func TestPlanCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cs := k8sfake.NewSimpleClientset()
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
	_, err := Plan(ctx, cs, dyn, testOptions())
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
}

func TestPlanDeletesNothing(t *testing.T) {
	objects := []runtime.Object{
		testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old),
		validatorSA(deadRun, old),
		validatorCRB(deadRun),
		dataConfigMap("aicr-snapshot-"+deadRun, deadRun),
		agentClusterRole("aicr-node-reader-"+deadRun, deadRun),
		gangPod("runai-team-a", "gang-neg-worker-"+hexToken32a+"-0", hexToken32a, old),
	}
	report, cs := planWith(t, testOptions(), objects, podGroup())
	if got := len(report.Candidates()); got != 7 {
		t.Fatalf("candidates = %d, want 7 (%+v)", got, report.Objects)
	}
	if deletes := deleteActions(cs); len(deletes) != 0 {
		t.Errorf("Plan deleted %d objects; a plan must be read-only", len(deletes))
	}
}

// --- Execute ---------------------------------------------------------------

// executeFixture builds a plan against fakes and returns everything an
// Execute assertion needs.
type executeFixture struct {
	cs     *k8sfake.Clientset
	report *Report
}

func newExecuteFixture(t *testing.T) *executeFixture {
	t.Helper()
	objects := []runtime.Object{
		testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old),
		validatorSA(deadRun, old),
		validatorCRB(deadRun),
		dataConfigMap("aicr-snapshot-"+deadRun, deadRun),
		agentClusterRole("aicr-node-reader-"+deadRun, deadRun),
		gangPod("runai-team-a", "gang-neg-worker-"+hexToken32a+"-0", hexToken32a, old),
	}
	report, cs := planWith(t, testOptions(), objects,
		podGroup())
	return &executeFixture{cs: cs, report: report}
}

func TestExecuteDeletesWithUIDPreconditionsInOrder(t *testing.T) {
	noSettle(t)
	f := newExecuteFixture(t)

	// The fakes ignore delete preconditions, so capture them: every delete
	// must pin exactly the UID the plan observed for that object.
	wantUID := make(map[string]types.UID)
	for _, o := range f.report.Objects {
		if o.Status == StatusWouldDelete {
			wantUID[o.Namespace+"/"+o.Name] = o.UID
		}
	}
	checkPreconditions := func(d k8stesting.DeleteAction) {
		t.Helper()
		key := d.GetNamespace() + "/" + d.GetName()
		want, planned := wantUID[key]
		pre := d.GetDeleteOptions().Preconditions
		switch {
		case !planned:
			t.Errorf("unexpected delete of %s %s", d.GetResource().Resource, key)
		case pre == nil || pre.UID == nil:
			t.Errorf("delete %s carried no UID precondition", key)
		case *pre.UID != want:
			t.Errorf("delete %s pinned UID %q, want the planned %q", key, *pre.UID, want)
		}
		if pp := d.GetDeleteOptions().PropagationPolicy; pp == nil || *pp != metav1.DeletePropagationBackground {
			t.Errorf("delete %s propagation = %v, want background", key, pp)
		}
	}

	var pinned []string
	f.cs.PrependReactor("delete", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		d := a.(k8stesting.DeleteAction)
		checkPreconditions(d)
		pinned = append(pinned, fmt.Sprintf("%s/%s/%s", d.GetResource().Resource, d.GetNamespace(), d.GetName()))
		return false, nil, nil
	})

	// The dynamic client handles the PodGroup; the typed reactor above does
	// not see it, which the count below accounts for.
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds,
		podGroup())
	dyn.PrependReactor("delete", "podgroups", func(a k8stesting.Action) (bool, runtime.Object, error) {
		checkPreconditions(a.(k8stesting.DeleteAction))
		return false, nil, nil
	})

	if err := Execute(context.Background(), f.cs, dyn, f.report); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// CRB before SA before ConfigMap before namespace before residue objects.
	wantOrder := []string{
		"clusterrolebindings//" + job.ClusterRoleBindingName(deadRun),
		"clusterroles//aicr-node-reader-" + deadRun,
		"serviceaccounts/" + testNamespace + "/" + job.ServiceAccountName(deadRun),
		"configmaps/" + testNamespace + "/aicr-snapshot-" + deadRun,
		"namespaces//" + defaults.GPUTestNamespacePrefix + hexToken32a,
		"pods/runai-team-a/gang-neg-worker-" + hexToken32a + "-0",
	}
	if len(pinned) != len(wantOrder) {
		t.Fatalf("typed deletes = %v, want %v", pinned, wantOrder)
	}
	for i, want := range wantOrder {
		if pinned[i] != want {
			t.Errorf("delete %d = %s, want %s (full order %v)", i, pinned[i], want, pinned)
		}
	}

	for _, o := range f.report.Objects {
		want := StatusDeleted
		if o.Kind == kindNamespace {
			want = StatusDeleting
		}
		if o.Status != want {
			t.Errorf("%s %s status = %q, want %q", o.Kind, o.Name, o.Status, want)
		}
	}

	// The PodGroup went through the dynamic client and must be gone.
	if _, err := dyn.Resource(podGroupGVR).Namespace("runai-team-a").Get(context.Background(),
		"gang-neg-group-"+hexToken32a, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("PodGroup still present after Execute: err=%v", err)
	}
}

// TestExecuteNotFoundAndConflictAreNotFailures: NotFound means the object
// was already gone (deleted), while a Conflict means the delete was refused
// and the object may still exist (skipped, re-run to retry). Neither fails
// the sweep.
func TestExecuteNotFoundAndConflictAreNotFailures(t *testing.T) {
	noSettle(t)
	f := newExecuteFixture(t)
	f.cs.PrependReactor("delete", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, "gone")
	})
	f.cs.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "replaced", fmt.Errorf("uid mismatch"))
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds,
		podGroup())

	if err := Execute(context.Background(), f.cs, dyn, f.report); err != nil {
		t.Fatalf("Execute() error = %v, want nil (already-gone is not a failure)", err)
	}
	wantStatus(t, f.report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusDeleted, "already gone")
	wantStatus(t, f.report, kindConfigMap, "aicr-snapshot-"+deadRun, StatusSkipped, "delete refused with a conflict")
}

func TestExecutePartialFailureDeletesTheRest(t *testing.T) {
	noSettle(t)
	f := newExecuteFixture(t)
	f.cs.PrependReactor("delete", "clusterrolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("apiserver is shutting down")
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds,
		podGroup())

	err := Execute(context.Background(), f.cs, dyn, f.report)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	if !strings.Contains(err.Error(), job.ClusterRoleBindingName(deadRun)) {
		t.Errorf("error %q should name the failed object", err)
	}
	wantStatus(t, f.report, kindClusterRoleBinding, job.ClusterRoleBindingName(deadRun), StatusFailed, "")
	// Everything else is still deleted.
	wantStatus(t, f.report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusDeleted, "")
	wantStatus(t, f.report, kindNamespace, defaults.GPUTestNamespacePrefix+hexToken32a, StatusDeleting, "")
}

func TestExecuteForbiddenDeleteIsUnauthorizedCause(t *testing.T) {
	noSettle(t)
	f := newExecuteFixture(t)
	f.cs.PrependReactor("delete", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "x", fmt.Errorf("denied"))
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds,
		podGroup())

	err := Execute(context.Background(), f.cs, dyn, f.report)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	// The Forbidden cause stays inspectable through the join, but the code a
	// consumer reads — and the CLI exit code — is the partial-failure
	// Internal (exit 8), not Unauthorized (exit 4).
	if got := errorstest.ReportedCode(err); got != errors.ErrCodeInternal {
		t.Fatalf("reported code = %v, want %v", got, errors.ErrCodeInternal)
	}
	if got := errors.ExitCodeFromError(err); got != errors.ExitInternal {
		t.Errorf("exit code = %d, want %d (a forbidden delete exits 8, not 4)", got, errors.ExitInternal)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeUnauthorized, "")) {
		t.Errorf("joined error lost the Forbidden delete's Unauthorized cause: %v", err)
	}
	if !strings.Contains(err.Error(), "serviceaccounts") {
		t.Errorf("error %q should name the failed object", err)
	}
}

func TestExecuteStopsOnCanceledContext(t *testing.T) {
	noSettle(t)
	f := newExecuteFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.cs.PrependReactor("delete", "clusterrolebindings", func(a k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds,
		podGroup())

	err := Execute(ctx, f.cs, dyn, f.report)
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)

	// Nothing after the canceled point was deleted: the CRB delete landed,
	// the rest still exist.
	if _, getErr := f.cs.CoreV1().ServiceAccounts(testNamespace).Get(context.Background(),
		job.ServiceAccountName(deadRun), metav1.GetOptions{}); getErr != nil {
		t.Errorf("ServiceAccount deleted despite cancellation: %v", getErr)
	}
}

// TestExecuteCancelDuringLastDeleteReportsCanceled: an interruption that
// lands while the LAST candidate's delete is in flight leaves no further
// loop iteration to notice the dead context, so Execute must still report
// the cancellation (exit 9) after the loop rather than the partial-failure
// Internal (exit 8). The report keeps the per-object statuses.
func TestExecuteCancelDuringLastDeleteReportsCanceled(t *testing.T) {
	noSettle(t)
	f := newExecuteFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	// The gang pod is the fixture's last candidate in deletion order.
	f.cs.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds,
		podGroup())

	err := Execute(ctx, f.cs, dyn, f.report)
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	wantStatus(t, f.report, kindPod, "gang-neg-worker-"+hexToken32a+"-0", StatusDeleted, "")
}

// TestExecuteReplansAgainstCurrentClusterState drives the liveness-guard
// scenarios through the re-plan Execute runs: an object that was safe at
// plan time but is guarded now is not selected by the fresh plan, so it is
// skipped with the fresh plan's own guard reason, never deleted.
func TestExecuteReplansAgainstCurrentClusterState(t *testing.T) {
	t.Run("namespace gained a live pod since planning", func(t *testing.T) {
		noSettle(t)
		ns := testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old)
		report, cs := planWith(t, testOptions(), []runtime.Object{ns})
		wantStatus(t, report, kindNamespace, ns.Name, StatusWouldDelete, "")

		if err := cs.Tracker().Add(livePod(ns.Name, "late-pod")); err != nil {
			t.Fatal(err)
		}
		dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
		if err := Execute(context.Background(), cs, dyn, report); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		wantStatus(t, report, kindNamespace, ns.Name, StatusSkipped, "still has live pods")
		if _, err := cs.CoreV1().Namespaces().Get(context.Background(), ns.Name, metav1.GetOptions{}); err != nil {
			t.Errorf("namespace with a live pod was deleted: %v", err)
		}
	})

	t.Run("run gained a Job since planning", func(t *testing.T) {
		noSettle(t)
		report, cs := planWith(t, testOptions(), []runtime.Object{validatorSA(deadRun, old)})
		wantStatus(t, report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusWouldDelete, "")

		if err := cs.Tracker().Add(validatorJob(testNamespace, deadRun)); err != nil {
			t.Fatal(err)
		}
		dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
		if err := Execute(context.Background(), cs, dyn, report); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		wantStatus(t, report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusSkipped, "still has a validator Job")
	})

	t.Run("run gained a Job in another namespace since planning", func(t *testing.T) {
		noSettle(t)
		// The snapshot agent's ClusterRole is cluster-scoped: a run in a
		// different namespace than Options.Namespace still owns it, so the
		// re-plan's liveness guard must see its Job anywhere.
		report, cs := planWith(t, testOptions(), []runtime.Object{agentClusterRole("aicr-node-reader-"+deadRun, deadRun)})
		wantStatus(t, report, kindClusterRole, "aicr-node-reader-"+deadRun, StatusWouldDelete, "")

		if err := cs.Tracker().Add(validatorJob("other-validation", deadRun)); err != nil {
			t.Fatal(err)
		}
		dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
		if err := Execute(context.Background(), cs, dyn, report); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		wantStatus(t, report, kindClusterRole, "aicr-node-reader-"+deadRun, StatusSkipped, "still has a validator Job")
		if _, err := cs.RbacV1().ClusterRoles().Get(context.Background(), "aicr-node-reader-"+deadRun, metav1.GetOptions{}); err != nil {
			t.Errorf("cluster-scoped agent RBAC deleted despite the live Job in another namespace: %v", err)
		}
	})
}

// --- Execute: settle observation -------------------------------------------

// watchJobs drives the settle observation's Watch from the test: the
// returned FakeWatcher is safe to feed before Execute starts, because its
// channel buffers the events until the observation's watch consumes them.
func watchJobs(t *testing.T, cs *k8sfake.Clientset, size int) *watch.FakeWatcher {
	t.Helper()
	fw := watch.NewFakeWithChanSize(size, false)
	cs.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})
	return fw
}

// TestExecuteSettleWindowKeepsARunThatReappears: a run whose validator Job
// appears while the sweep is observing the settle window keeps every one of
// its run-scoped objects, while a run that stays quiet is swept.
func TestExecuteSettleWindowKeepsARunThatReappears(t *testing.T) {
	useSettle(t, 200*time.Millisecond, 5*time.Millisecond)
	objects := []runtime.Object{
		validatorSA(deadRun, old),
		validatorCRB(deadRun),
		dataConfigMap("aicr-snapshot-"+deadRun, deadRun),
		validatorSA(deadRun2, old),
		validatorCRB(deadRun2),
	}
	report, cs := planWith(t, testOptions(), objects)
	if got := len(report.Candidates()); got != 5 {
		t.Fatalf("candidates = %d, want 5", got)
	}

	fw := watchJobs(t, cs, 1)
	fw.Add(validatorJob(testNamespace, deadRun))
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	if err := Execute(context.Background(), cs, dyn, report); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// The reappearing run's objects were kept, the quiet run's were swept.
	wantStatus(t, report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusSkipped,
		"started a validator Job while the sweep waited")
	wantStatus(t, report, kindClusterRoleBinding, job.ClusterRoleBindingName(deadRun), StatusSkipped,
		"started a validator Job while the sweep waited")
	wantStatus(t, report, kindConfigMap, "aicr-snapshot-"+deadRun, StatusSkipped,
		"started a validator Job while the sweep waited")
	wantStatus(t, report, kindServiceAccount, job.ServiceAccountName(deadRun2), StatusDeleted, "")
	wantStatus(t, report, kindClusterRoleBinding, job.ClusterRoleBindingName(deadRun2), StatusDeleted, "")

	for _, d := range deleteActions(cs) {
		if strings.Contains(d.GetName(), deadRun) {
			t.Errorf("deleted %s %s of a run that reappeared during the settle window", d.GetResource().Resource, d.GetName())
		}
	}
	if _, err := cs.CoreV1().ServiceAccounts(testNamespace).Get(context.Background(),
		job.ServiceAccountName(deadRun), metav1.GetOptions{}); err != nil {
		t.Errorf("ServiceAccount of the reappearing run was deleted: %v", err)
	}
}

// TestExecuteSettleWindowSeesABriefJob: a Job created AND deleted inside the
// window — the gap a poll can miss entirely — is still observed through the
// watch, so its run keeps its objects.
func TestExecuteSettleWindowSeesABriefJob(t *testing.T) {
	useSettle(t, 200*time.Millisecond, 5*time.Millisecond)
	report, cs := planWith(t, testOptions(), []runtime.Object{
		validatorSA(deadRun, old),
		validatorCRB(deadRun),
	})

	fw := watchJobs(t, cs, 2)
	j := validatorJob(testNamespace, deadRun)
	fw.Add(j)
	fw.Delete(j)
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	if err := Execute(context.Background(), cs, dyn, report); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	wantStatus(t, report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusSkipped,
		"started a validator Job while the sweep waited")
	wantStatus(t, report, kindClusterRoleBinding, job.ClusterRoleBindingName(deadRun), StatusSkipped,
		"started a validator Job while the sweep waited")
	if deletes := deleteActions(cs); len(deletes) != 0 {
		t.Errorf("a run whose Job lived only inside the window was swept: %v", deletes)
	}
}

// TestExecuteSettleListKeepsARunThatAppearedAfterTheReplan: a Job created
// after the re-plan listed Jobs but before the settle observation began is
// not in the watch (which resumes after the list), so the observation's
// initial list is the only thing that sees it.
func TestExecuteSettleListKeepsARunThatAppearedAfterTheReplan(t *testing.T) {
	useSettle(t, 200*time.Millisecond, 5*time.Millisecond)
	report, cs := planWith(t, testOptions(), []runtime.Object{
		validatorSA(deadRun, old),
		validatorCRB(deadRun),
	})
	watchJobs(t, cs, 1)

	replanListed := false
	cs.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !replanListed {
			// The first Jobs list after planning is the re-plan's own.
			replanListed = true
			return true, &batchv1.JobList{}, nil
		}
		return true, &batchv1.JobList{Items: []batchv1.Job{*validatorJob(testNamespace, deadRun)}}, nil
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	if err := Execute(context.Background(), cs, dyn, report); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	wantStatus(t, report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusSkipped,
		"started a validator Job while the sweep waited")
	wantStatus(t, report, kindClusterRoleBinding, job.ClusterRoleBindingName(deadRun), StatusSkipped,
		"started a validator Job while the sweep waited")
	if deletes := deleteActions(cs); len(deletes) != 0 {
		t.Errorf("a run whose Job appeared after the re-plan was swept: %v", deletes)
	}
}

// TestExecuteSettleWatchResumesFromTheListResourceVersion: the watch must
// start at the list's resourceVersion, or a Job created and deleted between
// the list and the watch is never reported.
func TestExecuteSettleWatchResumesFromTheListResourceVersion(t *testing.T) {
	useSettle(t, 50*time.Millisecond, 5*time.Millisecond)
	report, cs := planWith(t, testOptions(), []runtime.Object{validatorSA(deadRun, old)})

	const listVersion = "4242"
	cs.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &batchv1.JobList{ListMeta: metav1.ListMeta{ResourceVersion: listVersion}}, nil
	})
	watchedFrom := ""
	cs.PrependWatchReactor("jobs", func(a k8stesting.Action) (bool, watch.Interface, error) {
		watchedFrom = a.(k8stesting.WatchAction).GetWatchRestrictions().ResourceVersion
		return true, watch.NewFake(), nil
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	if err := Execute(context.Background(), cs, dyn, report); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if watchedFrom != listVersion {
		t.Errorf("settle watch resourceVersion = %q, want the list's %q", watchedFrom, listVersion)
	}
}

// TestExecuteSettleWindowRewatchesAfterClose: a watch channel that closes
// mid-window is re-established from a fresh list, and an event the second
// watch delivers is still honored.
func TestExecuteSettleWindowRewatchesAfterClose(t *testing.T) {
	useSettle(t, 500*time.Millisecond, 5*time.Millisecond)
	report, cs := planWith(t, testOptions(), []runtime.Object{validatorSA(deadRun, old)})

	first := watch.NewFake()
	first.Stop() // the first watch's channel is closed from the start
	second := watch.NewFakeWithChanSize(1, false)
	second.Add(validatorJob(testNamespace, deadRun))
	watches := 0
	cs.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		watches++
		if watches == 1 {
			return true, first, nil
		}
		return true, second, nil
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	if err := Execute(context.Background(), cs, dyn, report); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	wantStatus(t, report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusSkipped,
		"started a validator Job while the sweep waited")
	if watches != 2 {
		t.Errorf("watch establishments = %d, want 2 (the closed watch re-listed and re-watched)", watches)
	}
}

// TestExecuteSettleWindowWatchFailureFailsClosed: a watch that cannot be
// established means quiet was not proven — Execute returns the classified
// error and deletes nothing.
func TestExecuteSettleWindowWatchFailureFailsClosed(t *testing.T) {
	useSettle(t, 200*time.Millisecond, 5*time.Millisecond)
	report, cs := planWith(t, testOptions(), []runtime.Object{validatorSA(deadRun, old)})
	cs.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, "", fmt.Errorf("denied"))
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	err := Execute(context.Background(), cs, dyn, report)
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnauthorized)
	if deletes := deleteActions(cs); len(deletes) != 0 {
		t.Errorf("Execute deleted %d objects with the settle watch unproven: %v", len(deletes), deletes)
	}
}

// TestWatchOnceBoundsAWatchTheServerNeverAnswers: the fake clientset's watch
// reactors never see the context, so this uses a real client against a
// server that accepts the watch request and then stays silent. The settle
// window bounds the call: it fails with a timeout instead of hanging until
// the operator interrupts.
func TestWatchOnceBoundsAWatchTheServerNeverAnswers(t *testing.T) {
	useSettle(t, 100*time.Millisecond, 5*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("NewForConfig() error = %v", err)
	}
	window := time.NewTimer(time.Minute)
	defer window.Stop()
	pending := map[string][]*Object{deadRun: {{Kind: kindServiceAccount}}}

	start := time.Now()
	done, err := watchOnce(context.Background(), kube, "1", window, pending)
	if !done {
		t.Error("watchOnce() done = false, want true: an unestablished watch must not be retried")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("watchOnce() took %v, want it bounded by the %v settle window", elapsed, settleWindow)
	}
}

// TestExecuteSettleTrustsTheFreshRunID: a report whose run-scoped objects
// carry no run ID — or another run's ID — cannot smuggle a live run's
// objects past the settle observation. Execute takes every surviving
// candidate's RunID from the fresh re-plan, so the Job appearing in the
// window is matched to the run it actually belongs to.
func TestExecuteSettleTrustsTheFreshRunID(t *testing.T) {
	for _, tamper := range []struct {
		name  string
		runID string
	}{
		{"run ID cleared", ""},
		{"run ID altered to another run", deadRun2},
	} {
		t.Run(tamper.name, func(t *testing.T) {
			useSettle(t, 200*time.Millisecond, 5*time.Millisecond)
			report, cs := planWith(t, testOptions(), []runtime.Object{
				validatorSA(deadRun, old),
				validatorCRB(deadRun),
				dataConfigMap("aicr-snapshot-"+deadRun, deadRun),
			})
			for i := range report.Objects {
				report.Objects[i].RunID = tamper.runID
			}

			fw := watchJobs(t, cs, 1)
			fw.Add(validatorJob(testNamespace, deadRun))
			dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

			if err := Execute(context.Background(), cs, dyn, report); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			for _, o := range report.Objects {
				if o.Status != StatusSkipped || !strings.Contains(o.Reason, "started a validator Job") {
					t.Errorf("%s %s = %q (%q), want skipped because its run reappeared", o.Kind, o.Name, o.Status, o.Reason)
				}
				if o.RunID != deadRun {
					t.Errorf("%s %s RunID = %q, want the fresh plan's %q", o.Kind, o.Name, o.RunID, deadRun)
				}
			}
			if deletes := deleteActions(cs); len(deletes) != 0 {
				t.Errorf("a tampered RunID let the sweep delete %d objects of a live run: %v", len(deletes), deletes)
			}
		})
	}
}

// TestExecuteSettleWindowQuietDeletes: nothing reappears before the window
// elapses, so the run-scoped candidates are deleted.
func TestExecuteSettleWindowQuietDeletes(t *testing.T) {
	useSettle(t, 30*time.Millisecond, 5*time.Millisecond)
	report, cs := planWith(t, testOptions(), []runtime.Object{
		validatorSA(deadRun, old),
		validatorCRB(deadRun),
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	if err := Execute(context.Background(), cs, dyn, report); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	wantStatus(t, report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusDeleted, "")
	wantStatus(t, report, kindClusterRoleBinding, job.ClusterRoleBindingName(deadRun), StatusDeleted, "")
}

// TestExecuteSettleWindowCanceled: canceling mid-window aborts before any
// delete, with the canceled classification.
func TestExecuteSettleWindowCanceled(t *testing.T) {
	// A window long enough that only cancellation can end it.
	useSettle(t, time.Hour, 5*time.Millisecond)
	report, cs := planWith(t, testOptions(), []runtime.Object{validatorSA(deadRun, old)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		// The re-plan and the settle list have completed; cancel while the
		// observation's watch is being established.
		cancel()
		return true, watch.NewFake(), nil
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	err := Execute(ctx, cs, dyn, report)
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if deletes := deleteActions(cs); len(deletes) != 0 {
		t.Errorf("canceled settle window deleted %d objects: %v", len(deletes), deletes)
	}
}

// TestExecuteWithoutRunScopedCandidatesDoesNotObserve: namespaces and Run:ai
// residue are guarded by live pods and age, not by the Job signal, so their
// apply runs no settle observation at all — the only Jobs list is the
// re-plan's own.
func TestExecuteWithoutRunScopedCandidatesDoesNotObserve(t *testing.T) {
	// A window that would stall the test if the observation ran.
	useSettle(t, time.Hour, time.Hour)
	report, cs := planWith(t, testOptions(), []runtime.Object{
		testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old),
		gangPod("runai-team-a", "gang-neg-worker-"+hexToken32a+"-0", hexToken32a, old),
	}, podGroup())
	if got := len(report.Candidates()); got != 3 {
		t.Fatalf("candidates = %d, want 3", got)
	}

	listCalls := 0
	cs.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		listCalls++
		return false, nil, nil
	})
	cs.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		t.Error("the settle observation watched Jobs without run-scoped candidates")
		return true, watch.NewFake(), nil
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds,
		podGroup())

	if err := Execute(context.Background(), cs, dyn, report); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if listCalls != 1 {
		t.Errorf("jobs list calls = %d, want exactly 1 (the re-plan's); the settle observation must not run without run-scoped candidates", listCalls)
	}
	wantStatus(t, report, kindNamespace, defaults.GPUTestNamespacePrefix+hexToken32a, StatusDeleting, "")
	wantStatus(t, report, kindPod, "gang-neg-worker-"+hexToken32a+"-0", StatusDeleted, "")
}

// --- Execute: tampered plans ------------------------------------------------

// TestExecuteTamperedPlanDeletesOnlyTheReplannedIntersection: flipping a
// skipped entry to would-delete cannot widen the deletion set — the fresh
// plan still keeps it, and its guard's own reason replaces the tampered
// entry's.
func TestExecuteTamperedPlanDeletesOnlyTheReplannedIntersection(t *testing.T) {
	tests := []struct {
		name       string
		objects    []runtime.Object
		kind       string
		objName    string
		wantReason string
	}{
		{
			name: "Run:ai Project namespace flipped to would-delete",
			objects: []runtime.Object{func() *corev1.Namespace {
				ns := testNamespaceObj(defaults.GangSchedulingTestNamespacePrefix+hexToken32a, hexToken32a, old)
				ns.Labels[labels.RunAIQueue] = "team-a"
				return ns
			}()},
			kind:       kindNamespace,
			objName:    defaults.GangSchedulingTestNamespacePrefix + hexToken32a,
			wantReason: "never swept",
		},
		{
			name:       "foreign-labeled namespace flipped to would-delete",
			objects:    []runtime.Object{testNamespaceObj("team-production", hexToken32a, old)},
			kind:       kindNamespace,
			objName:    "team-production",
			wantReason: reasonForeignNamespace,
		},
		{
			name:       "young namespace flipped to would-delete",
			objects:    []runtime.Object{testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, young)},
			kind:       kindNamespace,
			objName:    defaults.GPUTestNamespacePrefix + hexToken32a,
			wantReason: "below the --older-than floor",
		},
		{
			name: "ClusterRoleBinding with a wrong roleRef flipped to would-delete",
			objects: []runtime.Object{func() *rbacv1.ClusterRoleBinding {
				crb := validatorCRB(deadRun)
				crb.RoleRef.Name = "edit"
				return crb
			}()},
			kind:       kindClusterRoleBinding,
			objName:    job.ClusterRoleBindingName(deadRun),
			wantReason: "roleRef or subjects",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			noSettle(t)
			report, cs := planWith(t, testOptions(), tt.objects)
			obj := find(report, tt.kind, tt.objName)
			if obj == nil || obj.Status != StatusSkipped {
				t.Fatalf("fixture object status = %+v, want skipped", obj)
			}

			// The tamper: mark the kept object for deletion.
			obj.Status = StatusWouldDelete
			obj.Reason = ""

			dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
			if err := Execute(context.Background(), cs, dyn, report); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			wantStatus(t, report, tt.kind, tt.objName, StatusSkipped, tt.wantReason)
			if deletes := deleteActions(cs); len(deletes) != 0 {
				t.Errorf("a tampered plan deleted %d objects: %v", len(deletes), deletes)
			}
		})
	}
}

// TestExecuteChangedUIDIsSkipped: an object replaced since planning is not
// recognized by the re-plan intersection — and the delete's UID precondition
// would have refused it anyway.
func TestExecuteChangedUIDIsSkipped(t *testing.T) {
	noSettle(t)
	report, cs := planWith(t, testOptions(), []runtime.Object{validatorSA(deadRun, old)})
	obj := find(report, kindServiceAccount, job.ServiceAccountName(deadRun))
	if obj == nil || obj.Status != StatusWouldDelete {
		t.Fatalf("fixture object status = %+v, want would-delete", obj)
	}
	obj.UID = types.UID("uid-forged")

	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
	if err := Execute(context.Background(), cs, dyn, report); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	wantStatus(t, report, kindServiceAccount, job.ServiceAccountName(deadRun), StatusSkipped, reasonNoLongerCandidate)
	if _, err := cs.CoreV1().ServiceAccounts(testNamespace).Get(context.Background(),
		job.ServiceAccountName(deadRun), metav1.GetOptions{}); err != nil {
		t.Errorf("replaced ServiceAccount was deleted: %v", err)
	}
}

// TestExecuteHandBuiltPlanWithoutFloorRejected: a hand-built report with no
// age floor fails the re-plan's own validation, before anything is deleted.
func TestExecuteHandBuiltPlanWithoutFloorRejected(t *testing.T) {
	noSettle(t)
	cs := k8sfake.NewSimpleClientset(validatorSA(deadRun, old))
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	plan := &Report{Namespace: testNamespace, Objects: []Object{{
		Kind: kindServiceAccount, Namespace: testNamespace,
		Name: job.ServiceAccountName(deadRun), UID: types.UID("uid-sa-" + deadRun),
		Status: StatusWouldDelete,
	}}}
	err := Execute(context.Background(), cs, dyn, plan)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if deletes := deleteActions(cs); len(deletes) != 0 {
		t.Errorf("a floor-less hand-built plan deleted %d objects: %v", len(deletes), deletes)
	}
}

func TestExecuteNilPlanRejected(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)
	errorstest.WantReportedCode(t, Execute(context.Background(), cs, dyn, nil), errors.ErrCodeInvalidRequest)
}

// TestPlanAgentClusterRBACGuardedByJobInAnyNamespace pins the cluster-wide
// liveness guard: `aicr validate -n other-ns` runs its agent's Jobs in
// other-ns while the agent's ClusterRole/ClusterRoleBinding are
// cluster-scoped, so a Job in another namespace must keep them.
func TestPlanAgentClusterRBACGuardedByJobInAnyNamespace(t *testing.T) {
	objects := []runtime.Object{
		agentClusterRole("aicr-node-reader-"+deadRun, deadRun),
		agentClusterRoleBinding("aicr-node-reader-"+deadRun, deadRun),
		validatorJob("other-validation", deadRun),
	}
	report, _ := planWith(t, testOptions(), objects)
	wantStatus(t, report, kindClusterRole, "aicr-node-reader-"+deadRun, StatusSkipped, "still has a validator Job")
	wantStatus(t, report, kindClusterRoleBinding, "aicr-node-reader-"+deadRun, StatusSkipped, "still has a validator Job")
}

// TestPlanAgentClusterRBACSelectedWhenNoJobAnywhere is the complement: with
// no Job carrying the run ID in any namespace (an unrelated live run's Job
// does not count), the dead run's cluster-scoped agent RBAC is swept.
func TestPlanAgentClusterRBACSelectedWhenNoJobAnywhere(t *testing.T) {
	objects := []runtime.Object{
		agentClusterRole("aicr-node-reader-"+deadRun, deadRun),
		agentClusterRoleBinding("aicr-node-reader-"+deadRun, deadRun),
		validatorJob(testNamespace, liveRun),
	}
	report, _ := planWith(t, testOptions(), objects)
	wantStatus(t, report, kindClusterRole, "aicr-node-reader-"+deadRun, StatusWouldDelete, "")
	wantStatus(t, report, kindClusterRoleBinding, "aicr-node-reader-"+deadRun, StatusWouldDelete, "")
}

// TestExecuteReplanFailureAbortsBeforeDeleting pins the fail-closed apply
// path: a liveness input that turns unreadable between plan and apply fails
// the re-plan, and Execute propagates the classified error with nothing
// deleted — an unreadable guard must never masquerade as a pass.
func TestExecuteReplanFailureAbortsBeforeDeleting(t *testing.T) {
	noSettle(t)
	ns := testNamespaceObj(defaults.GPUTestNamespacePrefix+hexToken32a, hexToken32a, old)
	report, cs := planWith(t, testOptions(), []runtime.Object{ns})
	wantStatus(t, report, kindNamespace, ns.Name, StatusWouldDelete, "")

	// The guard's pod list turns Forbidden between plan and apply.
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", fmt.Errorf("denied"))
	})
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), testPodGroupListKinds)

	err := Execute(context.Background(), cs, dyn, report)
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnauthorized)
	if deletes := deleteActions(cs); len(deletes) != 0 {
		t.Errorf("Execute deleted despite the unreadable guard: %v", deletes)
	}
	if _, getErr := cs.CoreV1().Namespaces().Get(context.Background(), ns.Name, metav1.GetOptions{}); getErr != nil {
		t.Errorf("namespace deleted despite the unreadable liveness guard: %v", getErr)
	}
}

// --- selectors --------------------------------------------------------------

func TestAcceptTestNamespace(t *testing.T) {
	tests := []struct {
		name  string
		ns    string
		token string
		want  bool
	}{
		{"GPU namespace", defaults.GPUTestNamespacePrefix + hexToken32a, hexToken32a, true},
		{"gang namespace", defaults.GangSchedulingTestNamespacePrefix + hexToken32a, hexToken32a, true},
		{"cluster-autoscaling namespace", defaults.ClusterAutoscalingTestNamespacePrefix + hexToken8, hexToken8, true},
		{"token differs from name", defaults.GPUTestNamespacePrefix + hexToken32a, hexToken32b, false},
		{"wrong hex length for prefix", defaults.GPUTestNamespacePrefix + hexToken8, hexToken8, false},
		{"cluster-autoscaling with 32 hex", defaults.ClusterAutoscalingTestNamespacePrefix + hexToken32a, hexToken32a, false},
		{"unknown prefix", "production", hexToken32a, false},
		{"uppercase hex", defaults.GPUTestNamespacePrefix + strings.ToUpper(hexToken32a), strings.ToUpper(hexToken32a), false},
		{"empty token", defaults.GPUTestNamespacePrefix, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := acceptTestNamespace(tt.ns, tt.token); got != tt.want {
				t.Errorf("acceptTestNamespace(%q, %q) = %v, want %v", tt.ns, tt.token, got, tt.want)
			}
		})
	}
}

func TestValidatorRunName(t *testing.T) {
	tests := []struct {
		name   string
		obj    string
		wantID string
		wantOK bool
	}{
		{"validator ServiceAccount", job.ServiceAccountName(liveRun), liveRun, true},
		{"validator ClusterRoleBinding", job.ClusterRoleBindingName(liveRun), liveRun, true},
		{"caller-supplied run ID shape", job.ServiceAccountName("e2e-pinned"), "e2e-pinned", true},
		{"bare prefix", "aicr-validator-", "", false},
		{"unrelated name", "other-serviceaccount", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := validatorRunName(tt.obj)
			if id != tt.wantID || ok != tt.wantOK {
				t.Errorf("validatorRunName(%q) = (%q, %v), want (%q, %v)", tt.obj, id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}
