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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
