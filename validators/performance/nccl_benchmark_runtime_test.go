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

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

// validBenchmarkRuntime is a minimal but structurally complete Kubeflow
// TrainingRuntime a recipe could carry inline via nccl-benchmark-runtime. It
// declares the "node" replicatedJob applyNCCLWorkerScheduling requires and a
// ${GPU_COUNT_PER_NODE} placeholder to exercise render substitution.
const validBenchmarkRuntime = `apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime
metadata:
  name: whatever-the-author-named-it
spec:
  template:
    spec:
      replicatedJobs:
        - name: node
          template:
            spec:
              template:
                spec:
                  containers:
                    - name: node
                      image: example.com/nccl:latest
                      resources:
                        limits:
                          nvidia.com/gpu: "${GPU_COUNT_PER_NODE}"
`

func runtimeConstraint(v string) recipe.Constraint {
	return recipe.Constraint{Name: perfConstraintNCCLBenchmarkRuntime, Value: v}
}

// TestBenchmarkRuntimeIdentityLocked pins the pod's apply-side identifiers to the
// shared shape-check constants in pkg/validator/v1. The shape validator
// (v1.ValidateBenchmarkRuntime) accepts a runtime by these; the pod then applies
// it through trainingRuntimeGVR and injects scheduling into nodeJobName. If the
// two drift, the validator would accept a runtime it then can't apply — this
// test fails first.
func TestBenchmarkRuntimeIdentityLocked(t *testing.T) {
	if got := trainingRuntimeGVR.Group + "/" + trainingRuntimeGVR.Version; got != v1.BenchmarkRuntimeAPIVersion {
		t.Errorf("apply GVR apiVersion = %q, want v1.BenchmarkRuntimeAPIVersion %q", got, v1.BenchmarkRuntimeAPIVersion)
	}
	if nodeJobName != v1.BenchmarkRuntimeNodeJob {
		t.Errorf("nodeJobName = %q, want v1.BenchmarkRuntimeNodeJob %q", nodeJobName, v1.BenchmarkRuntimeNodeJob)
	}
}

func TestResolveNCCLBenchmarkRuntime(t *testing.T) {
	tests := []struct {
		name       string
		ctx        *validators.Context
		want       string
		wantErr    bool
		wantErrSub string
	}{
		{
			name: "absent constraint → no runtime",
			ctx:  ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel"),
			want: "",
		},
		{
			name: "blank value → no runtime",
			ctx:  ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel", runtimeConstraint("   ")),
			want: "",
		},
		{
			name: "valid TrainingRuntime is returned verbatim (trimmed)",
			ctx:  ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel", runtimeConstraint(validBenchmarkRuntime)),
			want: strings.TrimSpace(validBenchmarkRuntime),
		},
		{
			name:       "wrong kind fails closed",
			ctx:        ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel", runtimeConstraint("apiVersion: trainer.kubeflow.org/v1alpha1\nkind: TrainJob\n")),
			wantErr:    true,
			wantErrSub: "must be a",
		},
		{
			name:       "wrong apiVersion fails closed",
			ctx:        ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel", runtimeConstraint("apiVersion: v1\nkind: TrainingRuntime\n")),
			wantErr:    true,
			wantErrSub: "must be a",
		},
		{
			name: "missing node replicatedJob fails closed",
			ctx: ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel", runtimeConstraint(
				"apiVersion: trainer.kubeflow.org/v1alpha1\nkind: TrainingRuntime\nspec:\n  template:\n    spec:\n      replicatedJobs:\n        - name: worker\n")),
			wantErr:    true,
			wantErrSub: "must declare a \"node\" replicatedJob",
		},
		{
			name: "node replicatedJob without worker pod spec fails closed",
			ctx: ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel", runtimeConstraint(
				"apiVersion: trainer.kubeflow.org/v1alpha1\nkind: TrainingRuntime\nspec:\n  template:\n    spec:\n      replicatedJobs:\n        - name: node\n")),
			wantErr:    true,
			wantErrSub: "must define the worker pod spec",
		},
		{
			name:       "unparseable YAML fails closed",
			ctx:        ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel", runtimeConstraint("kind: [unterminated")),
			wantErr:    true,
			wantErrSub: "not parseable as YAML",
		},
		{
			name: "unresolved ref (no carrier) fails closed",
			ctx: ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel",
				recipe.Constraint{Name: perfConstraintNCCLBenchmarkRuntimeRef, Value: "gb200/mycloud"}),
			wantErr:    true,
			wantErrSub: "reached the validator unresolved",
		},
		{
			name: "resolved carrier alongside its (leftover) ref still runs",
			ctx: ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel",
				recipe.Constraint{Name: perfConstraintNCCLBenchmarkRuntimeRef, Value: "gb200/mycloud"},
				runtimeConstraint(validBenchmarkRuntime)),
			want: strings.TrimSpace(validBenchmarkRuntime),
		},
		{
			name: "duplicate carriers fail closed (blank first)",
			ctx: ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel",
				runtimeConstraint(""), runtimeConstraint(validBenchmarkRuntime)),
			wantErr:    true,
			wantErrSub: "at most one",
		},
		{
			name: "duplicate carriers fail closed (blank second)",
			ctx: ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel",
				runtimeConstraint(validBenchmarkRuntime), runtimeConstraint("")),
			wantErr:    true,
			wantErrSub: "at most one",
		},
		{
			name: "duplicate refs fail closed",
			ctx: ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel",
				recipe.Constraint{Name: perfConstraintNCCLBenchmarkRuntimeRef, Value: ""},
				recipe.Constraint{Name: perfConstraintNCCLBenchmarkRuntimeRef, Value: "gb200/mycloud"}),
			wantErr:    true,
			wantErrSub: "at most one",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveNCCLBenchmarkRuntime(tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSub)
				}
				return
			}
			if got != tt.want {
				t.Errorf("runtime = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestValidateNcclAllReduceBwCustomRuntimeGate drives the recipe-supplied-runtime
// path through validateNcclAllReduceBw without a cluster, proving: a valid
// runtime bypasses the compiled applicability gate for an external service (a
// bare private criteria pair would otherwise skip), a runtime and a profile are
// rejected together, and a malformed runtime fails closed before any cluster
// access.
func TestValidateNcclAllReduceBwCustomRuntimeGate(t *testing.T) {
	t.Setenv(ncclFabricEnv, "") // isolate from ambient AICR_NCCL_FABRIC

	thresholdC := func(name, v string) recipe.Constraint { return recipe.Constraint{Name: name, Value: v} }

	tests := []struct {
		name        string
		threshold   recipe.Constraint
		extra       []recipe.Constraint // additional performance constraints (runtime, profile)
		variant     ncclVariant
		wantErrCode bool   // expect ErrCodeInvalidRequest
		wantErrSub  string // substring the error must contain
	}{
		{
			name:        "runtime bypasses applicability gate and reaches threshold parse",
			threshold:   thresholdC(checkNameNCCLAllReduceBW, "not-a-number"),
			extra:       []recipe.Constraint{runtimeConstraint(validBenchmarkRuntime)},
			variant:     variantDefault,
			wantErrCode: true,
			wantErrSub:  "invalid threshold",
		},
		{
			name:        "runtime and profile are mutually exclusive",
			threshold:   thresholdC(checkNameNCCLAllReduceBW, ">= 450"),
			extra:       []recipe.Constraint{runtimeConstraint(validBenchmarkRuntime), profileConstraint("gb200/eks")},
			variant:     variantDefault,
			wantErrCode: true,
			wantErrSub:  "mutually exclusive",
		},
		{
			name:        "malformed runtime fails closed before cluster access",
			threshold:   thresholdC(checkNameNCCLAllReduceBW, ">= 450"),
			extra:       []recipe.Constraint{runtimeConstraint("apiVersion: trainer.kubeflow.org/v1alpha1\nkind: TrainJob\n")},
			variant:     variantDefault,
			wantErrCode: true,
			wantErrSub:  "must be a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := append([]recipe.Constraint{tt.threshold}, tt.extra...)
			ctx := ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel", cs...)
			msg, passed, err := validateNcclAllReduceBw(ctx, tt.threshold, tt.variant)
			if err == nil {
				t.Fatalf("expected error, got (%q, %v)", msg, passed)
			}
			if tt.wantErrCode && !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
			if tt.wantErrSub != "" && !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSub)
			}
		})
	}
}

// TestValidateNcclAllReduceBwCustomRuntimeIgnoresFabric proves a recipe-supplied
// runtime is not gated on AICR_NCCL_FABRIC: a malformed fabric env (which fails
// the baked-in path at ncclFabric) must not abort a custom runtime, which owns
// its own fabric. The check reaches threshold parsing instead of erroring on
// the env.
func TestValidateNcclAllReduceBwCustomRuntimeIgnoresFabric(t *testing.T) {
	t.Setenv(ncclFabricEnv, "not-a-real-fabric")

	threshold := recipe.Constraint{Name: checkNameNCCLAllReduceBW, Value: "not-a-number"}
	ctx := ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel",
		threshold, runtimeConstraint(validBenchmarkRuntime))

	_, _, err := validateNcclAllReduceBw(ctx, threshold, variantDefault)
	if err == nil {
		t.Fatal("expected an error")
	}
	// Must reach threshold parsing (past the fabric env), not fail on the fabric.
	if !strings.Contains(err.Error(), "invalid threshold") {
		t.Errorf("custom runtime should ignore AICR_NCCL_FABRIC; got %v", err)
	}
}

// TestValidateNcclAllReduceBwCustomRuntimeSizesByRuntimeSelector proves the
// worker cohort is sized against the SAME nodes the runtime's own nodeSelector
// will place workers on: two GPU nodes, a runtime that pins workers to pool=a
// (matching only one), so WorkerCount is 1 and the 2-node skip fires. Without
// runtime-selector-aware sizing, WorkerCount would be 2 and the check would
// proceed to deploy against nodes the runtime then excludes.
func TestValidateNcclAllReduceBwCustomRuntimeSizesByRuntimeSelector(t *testing.T) {
	t.Setenv(ncclFabricEnv, "")

	node := func(name, pool string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{gpuProductLabel: "PRIVATE-ACCEL", "pool": pool},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
			},
		}
	}

	const runtimeWithSelector = `apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime
spec:
  template:
    spec:
      replicatedJobs:
        - name: node
          template:
            spec:
              template:
                spec:
                  nodeSelector:
                    pool: a
                  containers:
                    - name: node
                      image: example.com/nccl:latest
`
	threshold := recipe.Constraint{Name: checkNameNCCLAllReduceBW, Value: ">= 100"}
	ctx := ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel",
		threshold, runtimeConstraint(runtimeWithSelector))
	ctx.Ctx = context.Background()
	ctx.Clientset = fake.NewClientset(node("gpu-a", "a"), node("gpu-b", "b"))

	msg, passed, err := validateNcclAllReduceBw(ctx, threshold, variantDefault)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "skipped - requires at least 2 GPU nodes for EW fabric test"
	if !passed || msg != want {
		t.Errorf("got (%q, %v), want (%q, true) — sizing must narrow by the runtime's own nodeSelector", msg, passed, want)
	}
}

// TestValidateNcclAllReduceBwCustomRuntimeClusterPath proves the runtime path
// reaches cluster node sizing keyed on the recipe's OWN private criteria — a
// combination the compiled matrix does not cover. A single GPU node produces
// the "requires at least 2 GPU nodes" skip, which is only reachable past the
// applicability gate: a bare private criteria pair would have skipped earlier
// with "requires Service + Accelerator to be implemented".
func TestValidateNcclAllReduceBwCustomRuntimeClusterPath(t *testing.T) {
	t.Setenv(ncclFabricEnv, "") // isolate from ambient AICR_NCCL_FABRIC

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-a",
			Labels: map[string]string{gpuProductLabel: "PRIVATE-ACCEL"},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
		},
	}

	threshold := recipe.Constraint{Name: checkNameNCCLAllReduceBW, Value: ">= 100"}
	ctx := ctxWithCriteriaAndPerfConstraints("custom-svc", "customaccel",
		threshold, runtimeConstraint(validBenchmarkRuntime))
	ctx.Ctx = context.Background()
	ctx.Clientset = fake.NewClientset(node)

	msg, passed, err := validateNcclAllReduceBw(ctx, threshold, variantDefault)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "skipped - requires at least 2 GPU nodes for EW fabric test"
	if !passed || msg != want {
		t.Errorf("got (%q, %v), want (%q, true)", msg, passed, want)
	}
}

func TestRenderYAMLTemplate(t *testing.T) {
	obj, err := renderYAMLTemplate(
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ${NAME}\n  namespace: ${NAMESPACE}\n",
		map[string]string{"NAME": "rendered", "NAMESPACE": "validation"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := obj.GetName(); got != "rendered" {
		t.Errorf("name = %q, want %q", got, "rendered")
	}
	if got := obj.GetNamespace(); got != "validation" {
		t.Errorf("namespace = %q, want %q", got, "validation")
	}
}

// TestBuildNCCLRuntimeObjectCustom covers the custom-runtime branch of
// buildNCCLRuntimeObject: the identity is force-set to what the shared TrainJob
// expects and confined to the validator namespace (regardless of the author's
// metadata.name), and generic ${VAR} placeholders are substituted.
func TestBuildNCCLRuntimeObjectCustom(t *testing.T) {
	obj, err := buildNCCLRuntimeObject(validBenchmarkRuntime,
		"customaccel", "custom-svc", variantDefault, fabricEFA,
		"aicr-validation", map[string]string{"GPU_COUNT_PER_NODE": "8"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := obj.GetName(); got != ncclTrainingRuntimeName {
		t.Errorf("name = %q, want forced %q", got, ncclTrainingRuntimeName)
	}
	if got := obj.GetNamespace(); got != "aicr-validation" {
		t.Errorf("namespace = %q, want confined %q", got, "aicr-validation")
	}
	// Assert ${GPU_COUNT_PER_NODE} was substituted to "8" in the container limit.
	containers, _, _ := unstructured.NestedSlice(workerPodSpec(t, obj), "containers")
	if len(containers) == 0 {
		t.Fatal("no containers in rendered worker pod spec")
	}
	c0, _ := containers[0].(map[string]interface{})
	gpu, _, _ := unstructured.NestedString(c0, "resources", "limits", "nvidia.com/gpu")
	if gpu != "8" {
		t.Errorf("nvidia.com/gpu limit = %q, want %q (\\${GPU_COUNT_PER_NODE} not substituted)", gpu, "8")
	}
}

// TestApplyNCCLResourcesCustomRuntimeEKSNoClobber is the regression guard for the
// service-scheduling gate: a recipe-supplied runtime paired with a RECOGNIZED
// service (here eks) must keep its OWN worker nodeSelector. Before the fix, the
// platform default ran unconditionally and stamped {instance-type: ""} —
// instanceType is never discovered on the custom path — matching zero nodes and
// clobbering the runtime's selector.
func TestApplyNCCLResourcesCustomRuntimeEKSNoClobber(t *testing.T) {
	const ns = "aicr-validation"
	const runtimeWithSelector = `apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime
metadata:
  name: author-chose-this
spec:
  template:
    spec:
      replicatedJobs:
        - name: node
          template:
            spec:
              template:
                spec:
                  nodeSelector:
                    my.io/private-gpu: "true"
                  containers:
                    - name: node
                      image: example.com/nccl:latest
`
	fakeClient := newFakeDynamicClient()
	ctx := &validators.Context{Ctx: context.Background(), DynamicClient: fakeClient, Namespace: ns}
	config := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8, TotalGPUCount: 16, Namespace: ns}

	// service=eks would, before the fix, stamp {instance-type: ""} onto the node
	// job. No --node-selector override, so the runtime's own selector must win.
	if err := applyNCCLResources(ctx, fakeClient, config,
		recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceEKS, variantDefault, fabricEFA,
		runtimeWithSelector); err != nil {
		t.Fatalf("applyNCCLResources (custom runtime) failed: %v", err)
	}

	got, err := fakeClient.Resource(trainingRuntimeGVR).Namespace(ns).
		Get(context.Background(), ncclTrainingRuntimeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("TrainingRuntime not created: %v", err)
	}
	sel, _, _ := unstructured.NestedStringMap(workerPodSpec(t, got), "nodeSelector")
	if _, clobbered := sel[instanceTypeLabel]; clobbered {
		t.Errorf("worker nodeSelector was clobbered with %q: %v", instanceTypeLabel, sel)
	}
	if sel["my.io/private-gpu"] != "true" {
		t.Errorf("runtime's own nodeSelector not preserved: %v", sel)
	}
}

// workerPodSpec navigates a rendered NCCL TrainingRuntime to the "node"
// replicatedJob's worker pod spec (spec.template.spec.replicatedJobs[node]
// .template.spec.template.spec) — the same path applyNCCLWorkerScheduling uses.
func workerPodSpec(t *testing.T, obj *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	jobs, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	if err != nil || !found {
		t.Fatalf("replicatedJobs not found (found=%v err=%v)", found, err)
	}
	for _, raw := range jobs {
		jobMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(jobMap, "name"); name != nodeJobName {
			continue
		}
		spec, found, err := unstructured.NestedMap(jobMap, "template", "spec", "template", "spec")
		if err != nil || !found {
			t.Fatalf("worker pod spec not found in node job (found=%v err=%v)", found, err)
		}
		return spec
	}
	t.Fatalf("no %q replicatedJob in runtime", nodeJobName)
	return nil
}
