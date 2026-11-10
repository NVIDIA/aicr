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
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestRecipeHasComponent(t *testing.T) {
	tests := []struct {
		name      string
		recipe    *recipe.RecipeResult
		component string
		want      bool
	}{
		{
			name:      "nil recipe",
			recipe:    nil,
			component: "kubeflow-trainer",
			want:      false,
		},
		{
			name:      "empty componentRefs",
			recipe:    &recipe.RecipeResult{},
			component: "kubeflow-trainer",
			want:      false,
		},
		{
			name: "component present",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "cert-manager"},
					{Name: "kubeflow-trainer"},
					{Name: "gpu-operator"},
				},
			},
			component: "kubeflow-trainer",
			want:      true,
		},
		{
			name: "component disabled",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:      "kubeflow-trainer",
						Overrides: map[string]any{"enabled": false},
					},
				},
			},
			component: "kubeflow-trainer",
			want:      false,
		},
		{
			name: "component not present",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "cert-manager"},
					{Name: "gpu-operator"},
				},
			},
			component: "kubeflow-trainer",
			want:      false,
		},
		{
			name: "dynamo-platform present",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "dynamo-platform"},
					{Name: "gpu-operator"},
				},
			},
			component: "dynamo-platform",
			want:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validation := v1.ToValidationInput(tt.recipe)
			ctx := &validators.Context{ValidationInput: validation}
			got := recipeHasComponent(ctx, tt.component)
			if got != tt.want {
				t.Errorf("recipeHasComponent(%q) = %v, want %v", tt.component, got, tt.want)
			}
		})
	}
}

func TestCheckRobustControllerRouting(t *testing.T) {
	tests := []struct {
		name           string
		recipe         *recipe.RecipeResult
		expectSkip     bool
		expectContains string // substring in error or skip message
	}{
		{
			name:           "nil recipe skips",
			recipe:         nil,
			expectSkip:     true,
			expectContains: "no supported AI operator",
		},
		{
			name:           "empty recipe skips",
			recipe:         &recipe.RecipeResult{},
			expectSkip:     true,
			expectContains: "no supported AI operator",
		},
		{
			name: "recipe with only gpu-operator skips",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
					{Name: "cert-manager"},
				},
			},
			expectSkip:     true,
			expectContains: "no supported AI operator",
		},
		{
			name: "recipe with kubeflow-trainer routes to kubeflow check",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "kubeflow-trainer"},
					{Name: "gpu-operator"},
				},
			},
			expectSkip: false,
			// Will fail because fake clientset has no deployments, but proves routing works
			expectContains: "Kubeflow Trainer controller not found",
		},
		{
			name: "recipe with disabled kubeflow-trainer skips",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:      "kubeflow-trainer",
						Overrides: map[string]any{"enabled": false},
					},
					{Name: "gpu-operator"},
				},
			},
			expectSkip:     true,
			expectContains: "no supported AI operator",
		},
		{
			name: "recipe with dynamo-platform routes to dynamo check",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "dynamo-platform"},
					{Name: "gpu-operator"},
				},
			},
			expectSkip: false,
			// Will fail because fake clientset has no deployments, but proves routing works
			expectContains: "Dynamo operator controller not found",
		},
		{
			// The Kubeflow path is the one that reconciles a CR to a terminal
			// condition; the Dynamo path records reconciliation as an
			// observation only, so a recipe shipping both is evidenced by
			// Kubeflow.
			name: "recipe with both prefers kubeflow",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "dynamo-platform"},
					{Name: "kubeflow-trainer"},
				},
			},
			expectSkip:     false,
			expectContains: "Kubeflow Trainer controller not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validation := v1.ToValidationInput(tt.recipe)
			ctx := &validators.Context{
				Ctx:             context.Background(),
				Clientset:       k8sfake.NewClientset(),
				ValidationInput: validation,
			}

			err := CheckRobustController(ctx)

			if tt.expectSkip {
				if err == nil {
					t.Fatal("expected skip error, got nil")
				}
				if !strings.Contains(err.Error(), "skip") {
					t.Errorf("expected skip error, got: %v", err)
				}
				if tt.expectContains != "" && !strings.Contains(err.Error(), tt.expectContains) {
					t.Errorf("expected error to contain %q, got: %v", tt.expectContains, err)
				}
				return
			}

			// Non-skip: expect an error (because fake clientset has no resources)
			// but the error should indicate the correct operator was targeted
			if err == nil {
				t.Fatal("expected error from fake clientset, got nil")
			}
			if tt.expectContains != "" && !strings.Contains(err.Error(), tt.expectContains) {
				t.Errorf("expected error to contain %q, got: %v", tt.expectContains, err)
			}
		})
	}
}

// newTrainerDynamicFake returns a dynamic fake serving the Trainer GVRs the
// reconciliation case reads and writes.
func newTrainerDynamicFake(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			trainJobGVR:               "TrainJobList",
			clusterTrainingRuntimeGVR: "ClusterTrainingRuntimeList",
		}, objects...)
}

func testClusterTrainingRuntime(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		keyAPIVersion: "trainer.kubeflow.org/v1alpha1",
		keyKind:       "ClusterTrainingRuntime",
		keyMetadata:   map[string]any{keyName: name},
		keySpec:       map[string]any{"mlPolicy": map[string]any{"numNodes": int64(1)}},
	}}
}

// testTrainJobCRD returns the TrainJob CRD with the given status conditions
// (none: no status at all).
func testTrainJobCRD(conditions ...map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		keyAPIVersion: apiGroupAPIExtensions + "/v1",
		keyKind:       "CustomResourceDefinition",
		keyMetadata:   map[string]any{keyName: testTrainJobCRDName},
	}
	if conditions != nil {
		conds := make([]any, 0, len(conditions))
		for _, c := range conditions {
			conds = append(conds, c)
		}
		obj["status"] = map[string]any{"conditions": conds}
	}
	return &unstructured.Unstructured{Object: obj}
}

// setTrainJobStatusOnCreate installs a reactor that stamps every created
// TrainJob with the given conditions, standing in for the Trainer controller.
func setTrainJobStatusOnCreate(dyn *dynamicfake.FakeDynamicClient, conditions ...map[string]any) {
	dyn.PrependReactor("create", "trainjobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		tj, ok := createAction.GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		conds := make([]any, 0, len(conditions))
		for _, c := range conditions {
			conds = append(conds, c)
		}
		_ = unstructured.SetNestedSlice(tj.Object, conds, "status", "conditions")
		return false, nil, nil
	})
}

// trainJobCondition returns a True condition of the given type.
func trainJobCondition(condType, reason string) map[string]any {
	return map[string]any{"type": condType, "status": "True", "reason": reason, "message": reason + " message"}
}

const testTrainJobCRDName = "trainjobs.trainer.kubeflow.org"

func TestRequireCRDEstablished(t *testing.T) {
	const crd = testTrainJobCRDName
	crdGVR := schema.GroupVersionResource{Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs}
	tests := []struct {
		name     string
		objects  []runtime.Object
		getErr   error
		wantCode errors.ErrorCode
		wantMsg  string
	}{
		{
			name:    "Established=True passes",
			objects: []runtime.Object{testTrainJobCRD(map[string]any{"type": "Established", "status": "True", "reason": "InitialNamesAccepted"})},
		},
		{
			name:     "Established=False fails",
			objects:  []runtime.Object{testTrainJobCRD(map[string]any{"type": "Established", "status": "False", "reason": "Installing"})},
			wantCode: errors.ErrCodeInternal,
			wantMsg:  "not Established",
		},
		{
			name:     "no Established condition fails",
			objects:  []runtime.Object{testTrainJobCRD(map[string]any{"type": "NamesAccepted", "status": "True"})},
			wantCode: errors.ErrCodeInternal,
			wantMsg:  "no Established condition",
		},
		{
			name:     "no status at all fails",
			objects:  []runtime.Object{testTrainJobCRD()},
			wantCode: errors.ErrCodeInternal,
			wantMsg:  "no Established condition",
		},
		{
			name:     "missing CRD is NotFound",
			wantCode: errors.ErrCodeNotFound,
			wantMsg:  "not found",
		},
		{
			// classifyK8sReadError keeps an RBAC denial Internal (not a
			// Skip-eligible NotFound); the point is that it is not NotFound.
			name:     "RBAC-denied read is not NotFound",
			getErr:   k8serrors.NewForbidden(crdGVR.GroupResource(), crd, stderrors.New("rbac denied")),
			wantCode: errors.ErrCodeInternal,
			wantMsg:  "rbac denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, tt.objects...)
			if tt.getErr != nil {
				dyn.PrependReactor("get", resourceCRDs, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})
			}
			cond, err := requireCRDEstablished(context.Background(), dyn, crd)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("requireCRDEstablished() error = %v", err)
				}
				if cond == nil || cond.Status != "True" {
					t.Fatalf("requireCRDEstablished() condition = %+v, want Established=True", cond)
				}
				return
			}
			if err == nil {
				t.Fatal("requireCRDEstablished() = nil, want error")
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

// TestBuildValidTrainJob pins the reconciliation case's CR: the default
// runtime, one node, no accelerator, the probe image with a command that
// exits 0.
func TestBuildValidTrainJob(t *testing.T) {
	tj := buildValidTrainJob("ns", nil)
	if tj.GetKind() != "TrainJob" || tj.GetNamespace() != "ns" || tj.GetName() != robustTrainJobName {
		t.Fatalf("unexpected TrainJob identity: kind=%s ns=%s name=%s", tj.GetKind(), tj.GetNamespace(), tj.GetName())
	}
	runtimeName, _, _ := unstructured.NestedString(tj.Object, "spec", "runtimeRef", "name")
	runtimeKind, _, _ := unstructured.NestedString(tj.Object, "spec", "runtimeRef", "kind")
	if runtimeName != trainJobDefaultRuntime || runtimeKind != "ClusterTrainingRuntime" {
		t.Errorf("runtimeRef = %s/%s, want ClusterTrainingRuntime/%s", runtimeKind, runtimeName, trainJobDefaultRuntime)
	}
	image, _, _ := unstructured.NestedString(tj.Object, "spec", "trainer", "image")
	if image != defaults.ProbeImage {
		t.Errorf("trainer image = %q, want %q", image, defaults.ProbeImage)
	}
	numNodes, _, _ := unstructured.NestedInt64(tj.Object, "spec", "trainer", "numNodes")
	if numNodes != 1 {
		t.Errorf("trainer numNodes = %d, want 1", numNodes)
	}
	if _, found, _ := unstructured.NestedMap(tj.Object, "spec", "trainer", "resourcesPerNode"); found {
		t.Error("trainer must not request resources (no accelerator)")
	}
	command, _, _ := unstructured.NestedStringSlice(tj.Object, "spec", "trainer", "command")
	if len(command) == 0 || command[0] != "sh" {
		t.Errorf("trainer command = %v, want a shell command that exits 0", command)
	}
}

func TestWaitForTrainJobReconciled(t *testing.T) {
	tests := []struct {
		name       string
		conditions []map[string]any
		wantCode   errors.ErrorCode
		wantMsg    string
	}{
		{
			name:       "Complete=True is reconciled",
			conditions: []map[string]any{trainJobCondition("Created", "TrainJobCreated"), trainJobCondition("Complete", "AllJobsCompleted")},
		},
		{
			name:       "Failed=True is a terminal failure",
			conditions: []map[string]any{trainJobCondition("Failed", "JobSetFailed")},
			wantCode:   errors.ErrCodeInternal,
			wantMsg:    "Failed=True",
		},
		{
			name: "Failed=True wins over Complete=True",
			conditions: []map[string]any{
				trainJobCondition("Complete", "AllJobsCompleted"),
				trainJobCondition("Failed", "JobSetFailed"),
			},
			wantCode: errors.ErrCodeInternal,
			wantMsg:  "Failed=True",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dyn := newTrainerDynamicFake()
			setTrainJobStatusOnCreate(dyn, tt.conditions...)
			if _, err := dyn.Resource(trainJobGVR).Namespace("ns").Create(context.Background(),
				buildValidTrainJob("ns", nil), metav1.CreateOptions{}); err != nil {
				t.Fatalf("create: %v", err)
			}
			report, err := waitForTrainJobReconciled(context.Background(), dyn, "ns")
			if len(report.Conditions) != len(tt.conditions) {
				t.Errorf("report carries %d conditions, want %d", len(report.Conditions), len(tt.conditions))
			}
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("waitForTrainJobReconciled() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("waitForTrainJobReconciled() = nil, want error")
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

// TestWaitForTrainJobReconciled_Bound pins the reconcile-bound verdict: a
// TrainJob that never reaches a terminal condition reports Timeout, names
// the conditions it did observe, and asserts no cause the wait did not see.
func TestWaitForTrainJobReconciled_Bound(t *testing.T) {
	saved := trainJobReconcileTimeout
	trainJobReconcileTimeout = 200 * time.Millisecond
	t.Cleanup(func() { trainJobReconcileTimeout = saved })

	dyn := newTrainerDynamicFake()
	setTrainJobStatusOnCreate(dyn, trainJobCondition("Created", "TrainJobCreated"))
	if _, err := dyn.Resource(trainJobGVR).Namespace("ns").Create(context.Background(),
		buildValidTrainJob("ns", nil), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	report, err := waitForTrainJobReconciled(context.Background(), dyn, "ns")
	if err == nil {
		t.Fatal("waitForTrainJobReconciled() = nil, want the bound error")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
	for _, want := range []string{"did not reach Complete=True within", "Created=True", "TrainJob pods artifacts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "did not reconcile") {
		t.Errorf("error %q asserts a cause the wait did not observe", err.Error())
	}
	if len(report.Conditions) != 1 {
		t.Errorf("report carries %d conditions, want the observed Created condition", len(report.Conditions))
	}
}

// TestWaitForTrainJobReconciled_ParentCanceled pins that an operator abort
// reports Canceled (never a retryable Timeout, never a reconciliation
// failure) and still names the conditions observed.
func TestWaitForTrainJobReconciled_ParentCanceled(t *testing.T) {
	dyn := newTrainerDynamicFake()
	if _, err := dyn.Resource(trainJobGVR).Namespace("ns").Create(context.Background(),
		buildValidTrainJob("ns", nil), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := waitForTrainJobReconciled(ctx, dyn, "ns")
	if err == nil {
		t.Fatal("waitForTrainJobReconciled() = nil, want cancellation error")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if !strings.Contains(err.Error(), "conditions observed:") {
		t.Errorf("error %q should name the observed conditions", err.Error())
	}
}

// TestReconcileKubeflowTrainJob drives the reconciliation case end to end
// against fakes: the runtime must exist (its absence is the NotFound the
// acceptance criterion names), the TrainJob lands in a per-run namespace, and
// that namespace is gone afterwards whatever the verdict.
func TestReconcileKubeflowTrainJob(t *testing.T) {
	tests := []struct {
		name       string
		runtime    bool
		conditions []map[string]any
		createErr  error
		pendingPod bool
		wantCode   errors.ErrorCode
		wantMsg    string
	}{
		{
			name:       "runtime present, controller completes the TrainJob",
			runtime:    true,
			conditions: []map[string]any{trainJobCondition("Complete", "AllJobsCompleted")},
		},
		{
			name:       "runtime present, controller fails the TrainJob",
			runtime:    true,
			conditions: []map[string]any{trainJobCondition("Failed", "JobSetFailed")},
			wantCode:   errors.ErrCodeInternal,
			wantMsg:    "Failed=True",
		},
		{
			name:     "runtime missing",
			runtime:  false,
			wantCode: errors.ErrCodeNotFound,
			wantMsg:  "ClusterTrainingRuntime torch-distributed not found",
		},
		{
			name:      "webhook denies the valid TrainJob",
			runtime:   true,
			createErr: k8serrors.NewForbidden(trainJobGVR.GroupResource(), robustTrainJobName, stderrors.New(`admission webhook "validator.trainer.kubeflow.org" denied the request`)),
			wantCode:  errors.ErrCodeInternal,
			wantMsg:   "was rejected by admission or validation",
		},
		{
			name:      "RBAC denies the create",
			runtime:   true,
			createErr: k8serrors.NewForbidden(trainJobGVR.GroupResource(), robustTrainJobName, stderrors.New(`User "system:serviceaccount:aicr:validator" cannot create resource "trainjobs"`)),
			wantCode:  errors.ErrCodeUnauthorized,
			wantMsg:   "RBAC denied",
		},
		{
			name:      "webhook unavailable: ambiguous create keeps the classified code",
			runtime:   true,
			createErr: k8serrors.NewServerTimeout(trainJobGVR.GroupResource(), "create", 1),
			wantCode:  errors.ErrCodeTimeout,
			wantMsg:   "did not complete (ambiguous",
		},
		{
			name:       "controller created the pod but it stalled Pending",
			runtime:    true,
			conditions: []map[string]any{trainJobCondition("Created", "TrainJobCreated")},
			pendingPod: true,
			wantCode:   errors.ErrCodeTimeout,
			wantMsg:    "Unschedulable: 0/2 nodes are available: untolerated taint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.pendingPod {
				saved := trainJobReconcileTimeout
				trainJobReconcileTimeout = 200 * time.Millisecond
				t.Cleanup(func() { trainJobReconcileTimeout = saved })
			}
			var objects []runtime.Object
			if tt.runtime {
				objects = append(objects, testClusterTrainingRuntime(trainJobDefaultRuntime))
			}
			dyn := newTrainerDynamicFake(objects...)
			clientset := k8sfake.NewClientset()
			if tt.createErr != nil {
				dyn.PrependReactor("create", "trainjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.createErr
				})
			}
			setTrainJobStatusOnCreate(dyn, tt.conditions...)
			if tt.pendingPod {
				// Stand in for the controller's JobSet → Job → Pod chain with a
				// pod the scheduler refused, so the verdict can be checked for
				// the pod state it must carry.
				dyn.PrependReactor("create", "trainjobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
					_, podErr := clientset.CoreV1().Pods(action.GetNamespace()).Create(context.Background(), &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{Name: robustTrainJobName + "-node-0-0"},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "node", Image: defaults.ProbeImage}}},
						Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
							Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
							Message: "0/2 nodes are available: untolerated taint {nvidia.com/gpu: present}",
						}}},
					}, metav1.CreateOptions{})
					return false, nil, podErr
				})
			}
			vctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: dyn}

			var err error
			out := captureStdout(t, func() { err = reconcileKubeflowTrainJob(vctx, dyn) })

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("reconcileKubeflowTrainJob() error = %v\n%s", err, out)
				}
				for _, want := range []string{"TrainJob reconciliation", "reconciled to Complete=True", "Accelerator: none requested", "Cleanup test namespace"} {
					if !strings.Contains(out, want) {
						t.Errorf("output missing %q:\n%s", want, out)
					}
				}
			} else {
				if err == nil {
					t.Fatalf("reconcileKubeflowTrainJob() = nil, want error\n%s", out)
				}
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error %q should contain %q", err.Error(), tt.wantMsg)
				}
			}
			namespaces, listErr := clientset.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
			if listErr != nil {
				t.Fatalf("list namespaces: %v", listErr)
			}
			for _, ns := range namespaces.Items {
				if strings.HasPrefix(ns.Name, gpuTestNamespacePrefix) {
					t.Errorf("per-run namespace %s leaked", ns.Name)
				}
			}
			if !tt.runtime {
				if strings.Contains(out, "Cleanup test namespace") {
					t.Errorf("no namespace may be created before the runtime check passes:\n%s", out)
				}
			}
		})
	}
}

func TestTrainJobConditions(t *testing.T) {
	tj := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"conditions": []any{
			trainJobCondition("Complete", "AllJobsCompleted"),
			"not-a-map",
			map[string]any{"type": "Suspended"},
		}},
	}}
	got := trainJobConditions(tj)
	if len(got) != 2 {
		t.Fatalf("trainJobConditions() = %d conditions, want 2", len(got))
	}
	if got[0].Type != "Complete" || got[0].Status != "True" || got[0].Reason != "AllJobsCompleted" {
		t.Errorf("first condition = %+v", got[0])
	}
	if got[1].Type != "Suspended" || got[1].Status != statusUnknown {
		t.Errorf("sparse condition = %+v, want unknown status", got[1])
	}
	if got := trainJobConditions(&unstructured.Unstructured{Object: map[string]any{}}); got != nil {
		t.Errorf("no status → %v, want nil", got)
	}
}
