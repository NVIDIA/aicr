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
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/hasher"
	"sigs.k8s.io/kustomize/api/resource"
)

func TestRewriteJobSetStagingImage(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantChanged bool
	}{
		{
			name:        "staging image with v0.11.0 tag is repointed",
			input:       "        image: us-central1-docker.pkg.dev/k8s-staging-images/jobset/jobset:v0.11.0\n",
			wantChanged: true,
		},
		{
			name:        "staging image with arbitrary tag is repointed (tag-agnostic)",
			input:       "image: us-central1-docker.pkg.dev/k8s-staging-images/jobset/jobset:v0.99.9",
			wantChanged: true,
		},
		{
			name:        "already-promoted image is left untouched",
			input:       "image: registry.k8s.io/jobset/jobset:v0.11.0",
			wantChanged: false,
		},
		{
			name:        "unrelated resource is left untouched",
			input:       "image: nvcr.io/nvidia/some-image:latest",
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(rewriteJobSetStagingImage([]byte(tt.input)))

			if strings.Contains(got, jobSetStagingImageRepo) {
				t.Errorf("output still references staging repo %q: %s", jobSetStagingImageRepo, got)
			}

			changed := got != tt.input
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v (output: %s)", changed, tt.wantChanged, got)
			}

			if tt.wantChanged && !strings.Contains(got, jobSetPromotedImageRepo) {
				t.Errorf("output does not reference promoted repo %q: %s", jobSetPromotedImageRepo, got)
			}
		})
	}
}

// TestRewriteJobSetStagingImage_PreservesTag verifies the rewrite is a repo-prefix swap
// that preserves the original tag.
func TestRewriteJobSetStagingImage_PreservesTag(t *testing.T) {
	in := "image: " + jobSetStagingImageRepo + ":v0.11.0"
	want := "image: " + jobSetPromotedImageRepo + ":v0.11.0"
	if got := string(rewriteJobSetStagingImage([]byte(in))); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// deploymentFixture returns a minimal unstructured Deployment, optionally with an
// existing tolerations list, for exercising applyControllerTolerations.
func deploymentFixture(name string, existingTolerations []any) *unstructured.Unstructured {
	podSpec := map[string]any{
		"containers": []any{
			map[string]any{"name": "manager", "image": "example/manager:latest"},
		},
	}
	if existingTolerations != nil {
		podSpec["tolerations"] = existingTolerations
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": podSpec,
			},
		},
	}}
}

// TestApplyControllerTolerations_Isolation pins that the two controllers do
// not share a live toleration slice: mutating one Deployment's stamped
// tolerations in place must not affect the other, or the shared
// controllerTolerateAll package-level global.
func TestApplyControllerTolerations_Isolation(t *testing.T) {
	trainerObj := deploymentFixture(trainerControllerDeployment, nil)
	jobSetObj := deploymentFixture(jobSetControllerDeployment, nil)

	if err := applyControllerTolerations(trainerObj); err != nil {
		t.Fatalf("applyControllerTolerations(trainer) error = %v", err)
	}
	if err := applyControllerTolerations(jobSetObj); err != nil {
		t.Fatalf("applyControllerTolerations(jobset) error = %v", err)
	}

	// NestedSlice deep-copies (runtime.DeepCopyJSONValue), so mutating its
	// result can never observe aliasing back into the live object or the
	// shared global. NestedFieldNoCopy returns the same map reference
	// podSpec["tolerations"] actually holds, which is what this guard needs
	// to exercise to be anything but a false green.
	trainerTol := liveToleration(t, trainerObj)
	trainerTol["key"] = "mutated-for-trainer-only"

	jobSetTol := liveToleration(t, jobSetObj)
	if _, mutated := jobSetTol["key"]; mutated {
		t.Errorf("mutating the Trainer Deployment's toleration leaked into the JobSet Deployment: %v", jobSetTol)
	}
	if _, mutated := controllerTolerateAll[0].(map[string]any)["key"]; mutated {
		t.Errorf("mutating a stamped toleration leaked into the shared controllerTolerateAll global: %v", controllerTolerateAll[0])
	}
}

// liveToleration returns the live (non-deep-copied) first toleration map
// stamped onto obj, so a caller can mutate it in place to test for aliasing.
func liveToleration(t *testing.T, obj *unstructured.Unstructured) map[string]any {
	t.Helper()

	raw, found, err := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template", "spec", "tolerations")
	if err != nil || !found {
		t.Fatalf("tolerations not found on %q: found=%v err=%v", obj.GetName(), found, err)
	}
	tols, ok := raw.([]any)
	if !ok || len(tols) == 0 {
		t.Fatalf("tolerations on %q have unexpected shape: %v", obj.GetName(), raw)
	}
	tol, ok := tols[0].(map[string]any)
	if !ok {
		t.Fatalf("toleration[0] on %q has unexpected shape: %v", obj.GetName(), tols[0])
	}
	return tol
}

// TestApplyControllerTolerations covers both controller names, the two
// mutation-failure paths, and that an unrelated Deployment is left untouched.
func TestApplyControllerTolerations(t *testing.T) {
	tests := []struct {
		name    string
		obj     *unstructured.Unstructured
		wantErr bool
		// wantTolerations is checked only when wantErr is false. nil means "the
		// tolerations field must not be present at all" (untouched, not merely
		// empty).
		wantTolerations []any
	}{
		{
			name: "Trainer controller Deployment with no tolerations gets tolerate-all",
			obj:  deploymentFixture(trainerControllerDeployment, nil),
			wantTolerations: []any{
				map[string]any{"operator": "Exists"},
			},
		},
		{
			name: "JobSet controller Deployment with no tolerations gets tolerate-all",
			obj:  deploymentFixture(jobSetControllerDeployment, nil),
			wantTolerations: []any{
				map[string]any{"operator": "Exists"},
			},
		},
		{
			name: "Deployment with existing tolerations is left untouched",
			obj: deploymentFixture(trainerControllerDeployment, []any{
				map[string]any{"key": "dedicated", "operator": "Equal", "value": "trainer", "effect": "NoSchedule"},
			}),
			wantTolerations: []any{
				map[string]any{"key": "dedicated", "operator": "Equal", "value": "trainer", "effect": "NoSchedule"},
			},
		},
		{
			name:            "non-controller Deployment is left untouched",
			obj:             deploymentFixture("some-other-deployment", nil),
			wantTolerations: nil,
		},
		{
			// found && len(existing) > 0 is the guard: an explicit empty slice is
			// "present but empty," which must fall through to being stamped, not
			// be treated the same as "already tolerated." Pins this boundary
			// against a future refactor that flips the guard to `found` alone.
			name: "Deployment with present-but-empty tolerations gets tolerate-all",
			obj:  deploymentFixture(trainerControllerDeployment, []any{}),
			wantTolerations: []any{
				map[string]any{"operator": "Exists"},
			},
		},
		{
			name: "non-Deployment resource is left untouched",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": trainerControllerDeployment},
				"spec":       map[string]any{},
			}},
			wantTolerations: nil,
		},
		{
			name: "Deployment-kind resource in a non-apps group is left untouched",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "example.com/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": trainerControllerDeployment},
				"spec":       map[string]any{},
			}},
			wantTolerations: nil,
		},
		{
			name: "missing pod spec fails closed",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": trainerControllerDeployment},
				"spec":       map[string]any{},
			}},
			wantErr: true,
		},
		{
			name: "malformed tolerations field fails closed",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": trainerControllerDeployment},
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							// A string, not a slice: NestedSlice's type assertion fails.
							"tolerations": "not-a-slice",
						},
					},
				},
			}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := applyControllerTolerations(tt.obj)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			got, found, _ := unstructured.NestedSlice(tt.obj.Object, "spec", "template", "spec", "tolerations")
			if tt.wantTolerations == nil {
				if found {
					t.Errorf("expected no tolerations field, got %v", got)
				}
				return
			}
			if !found {
				t.Fatalf("expected tolerations %v, found none", tt.wantTolerations)
			}
			if len(got) != len(tt.wantTolerations) {
				t.Fatalf("got %d toleration(s) %v, want %d %v", len(got), got, len(tt.wantTolerations), tt.wantTolerations)
			}
			for i := range got {
				gotTol, _ := got[i].(map[string]any)
				wantTol, _ := tt.wantTolerations[i].(map[string]any)
				for k, v := range wantTol {
					if gotTol[k] != v {
						t.Errorf("toleration[%d][%q] = %v, want %v", i, k, gotTol[k], v)
					}
				}
			}
		})
	}
}

// TestDecodeTrainerObjects exercises decodeTrainerObjects end to end: the
// toleration lands only on a controller Deployment resource, not on an
// unrelated resource in the same manifest set, and the Kind=="" skip ordering
// above the applyControllerTolerations call does not interfere.
func TestDecodeTrainerObjects(t *testing.T) {
	rf := resource.NewFactory(&hasher.Hasher{})

	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  template:
    spec:
      containers:
      - name: manager
        image: example/trainer:latest
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: some-config
data:
  foo: bar
`, trainerControllerDeployment)

	resources, err := rf.SliceFromBytes([]byte(manifest))
	if err != nil {
		t.Fatalf("SliceFromBytes() error = %v", err)
	}

	objs, err := decodeTrainerObjects(resources)
	if err != nil {
		t.Fatalf("decodeTrainerObjects() error = %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d decoded object(s), want 2", len(objs))
	}

	var trainerObj, unrelatedObj *unstructured.Unstructured
	for _, o := range objs {
		switch o.GetName() {
		case trainerControllerDeployment:
			trainerObj = o
		case "some-config":
			unrelatedObj = o
		}
	}
	if trainerObj == nil {
		t.Fatal("Trainer controller Deployment missing from decoded objects")
	}
	tols, found, _ := unstructured.NestedSlice(trainerObj.Object, "spec", "template", "spec", "tolerations")
	if !found || len(tols) != 1 {
		t.Errorf("Trainer Deployment tolerations = %v, want a single blanket tolerate-all entry", tols)
	}

	if unrelatedObj == nil {
		t.Fatal("unrelated ConfigMap missing from decoded objects")
	}
	if _, found, _ := unstructured.NestedSlice(unrelatedObj.Object, "spec", "template", "spec", "tolerations"); found {
		t.Error("unrelated ConfigMap must not receive a toleration")
	}
}

// TestDecodeTrainerObjects_PropagatesTolerationError verifies a malformed
// resource that fails applyControllerTolerations aborts decoding rather than
// leaving a partial object list.
func TestDecodeTrainerObjects_PropagatesTolerationError(t *testing.T) {
	rf := resource.NewFactory(&hasher.Hasher{})

	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  template:
    spec:
      tolerations: not-a-slice
`, trainerControllerDeployment)

	resources, err := rf.SliceFromBytes([]byte(manifest))
	if err != nil {
		t.Fatalf("SliceFromBytes() error = %v", err)
	}

	if _, err := decodeTrainerObjects(resources); err == nil {
		t.Fatal("decodeTrainerObjects() expected error for a malformed tolerations field, got nil")
	}
}

// TestNCCLExecutionLockStaleAge_CoversSelfInstallWorstCase checks that
// NCCLExecutionLockStaleAge stays ahead of the self-install path's own
// worst case: the archive download, then waitForTrainerReady's CRD wait
// and its two controller-ready waits (Trainer's own controller, then
// JobSet's), all run before runNCCLTrainJob's first post-install lock
// renewal. A change to any of those timeouts that closes this margin
// should fail here, rather than let a same-run retry steal the lock from
// an install that is still in progress.
func TestNCCLExecutionLockStaleAge_CoversSelfInstallWorstCase(t *testing.T) {
	worstCase := defaults.NCCLTrainerArchiveDownloadTimeout +
		defaults.TrainerCRDEstablishedTimeout +
		2*defaults.TrainerControllerReadyTimeout

	if defaults.NCCLExecutionLockStaleAge <= worstCase {
		t.Errorf("NCCLExecutionLockStaleAge (%s) must exceed the self-install worst case (%s)",
			defaults.NCCLExecutionLockStaleAge, worstCase)
	}
}
