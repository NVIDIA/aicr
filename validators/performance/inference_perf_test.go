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
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/allocmode"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	resourcev1beta2 "k8s.io/api/resource/v1beta2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestHasDynamoPlatform(t *testing.T) {
	tests := []struct {
		name string
		ctx  *validators.Context
		want bool
	}{
		{
			name: "nil validation",
			ctx:  &validators.Context{ValidationInput: nil},
			want: false,
		},
		{
			name: "empty componentRefs",
			ctx: &validators.Context{ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{
				ComponentRefs: nil,
			})},
			want: false,
		},
		{
			name: "componentRefs without dynamo-platform",
			ctx: &validators.Context{ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
					{Name: "kubeflow-trainer"},
				},
			})},
			want: false,
		},
		{
			name: "dynamo-platform present",
			ctx: &validators.Context{ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
					{Name: "dynamo-platform"},
				},
			})},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasDynamoPlatform(tt.ctx); got != tt.want {
				t.Errorf("hasDynamoPlatform() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInferServicePort(t *testing.T) {
	tests := []struct {
		name string
		svc  v1.Service
		want int32
	}{
		{
			name: "port 8000 present",
			svc: v1.Service{Spec: v1.ServiceSpec{Ports: []v1.ServicePort{
				{Name: "grpc", Port: 9000},
				{Name: "http", Port: 8000},
			}}},
			want: 8000,
		},
		{
			name: "no 8000, named http wins over first port",
			svc: v1.Service{Spec: v1.ServiceSpec{Ports: []v1.ServicePort{
				{Name: "grpc", Port: 9000},
				{Name: "http", Port: 8080},
			}}},
			want: 8080,
		},
		{
			name: "no 8000, no named http — first port",
			svc: v1.Service{Spec: v1.ServiceSpec{Ports: []v1.ServicePort{
				{Name: "grpc", Port: 9000},
				{Name: "metrics", Port: 9090},
			}}},
			want: 9000,
		},
		{
			name: "no ports — default 8000",
			svc:  v1.Service{Spec: v1.ServiceSpec{Ports: nil}},
			want: 8000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inferServicePort(tt.svc); got != tt.want {
				t.Errorf("inferServicePort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDeriveRunID(t *testing.T) {
	tests := []struct {
		name          string
		runID         string
		wantLen       int
		wantHex       bool
		wantStable    bool   // if true, call twice with the same AICR_RUN_ID and confirm the two return values are equal (hash is deterministic)
		wantDifferent string // if set, a second derivation with this AICR_RUN_ID must differ from the first
		wantUnique    bool   // if true, call twice without AICR_RUN_ID and confirm the two return values differ
	}{
		{
			name:          "hashes AICR_RUN_ID to short suffix",
			runID:         "20260422-145927-2e674d7ee93860ac",
			wantLen:       8,
			wantHex:       true,
			wantStable:    true,
			wantDifferent: "20260422-145927-different-run-id",
		},
		{
			name:       "empty AICR_RUN_ID picks a random 8-hex suffix",
			runID:      "",
			wantLen:    8,
			wantHex:    true,
			wantUnique: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICR_RUN_ID", tt.runID)
			got := deriveRunID()
			if got == "" {
				t.Fatalf("deriveRunID() returned empty string")
			}
			if tt.wantLen > 0 && len(got) != tt.wantLen {
				t.Errorf("deriveRunID() len = %d, want %d (got %q)", len(got), tt.wantLen, got)
			}
			if tt.wantHex {
				if _, err := hex.DecodeString(got); err != nil {
					t.Errorf("deriveRunID() = %q, expected valid hex: %v", got, err)
				}
			}
			if tt.wantStable {
				if other := deriveRunID(); got != other {
					t.Errorf("deriveRunID() not deterministic: %q vs %q", got, other)
				}
			}
			if tt.wantDifferent != "" {
				t.Setenv("AICR_RUN_ID", tt.wantDifferent)
				if other := deriveRunID(); got == other {
					t.Errorf("deriveRunID() returned same suffix for different AICR_RUN_IDs: %q", got)
				}
			}
			if tt.wantUnique {
				other := deriveRunID()
				if got == other {
					t.Errorf("deriveRunID() returned same random value twice: %q", got)
				}
			}
		})
	}
}

func TestBuildTolerations(t *testing.T) {
	tests := []struct {
		name   string
		taints []v1.Taint
		want   []v1.Toleration
	}{
		{
			name:   "no taints — nil tolerations",
			taints: nil,
			want:   nil,
		},
		{
			name: "single taint — equal operator with value and effect",
			taints: []v1.Taint{
				{Key: "dedicated", Value: "worker-workload", Effect: v1.TaintEffectNoSchedule},
			},
			want: []v1.Toleration{
				{Key: "dedicated", Operator: v1.TolerationOpEqual, Value: "worker-workload", Effect: v1.TaintEffectNoSchedule},
			},
		},
		{
			name: "kubelet-managed node.kubernetes.io/* filtered out",
			taints: []v1.Taint{
				{Key: "node.kubernetes.io/not-ready", Value: "", Effect: v1.TaintEffectNoExecute},
				{Key: "nvidia.com/gpu", Value: "present", Effect: v1.TaintEffectNoSchedule},
			},
			want: []v1.Toleration{
				{Key: "nvidia.com/gpu", Operator: v1.TolerationOpEqual, Value: "present", Effect: v1.TaintEffectNoSchedule},
			},
		},
		{
			name: "taint value with YAML-special characters survives (typed, not templated)",
			taints: []v1.Taint{
				{Key: "group", Value: "a:b#c - d", Effect: v1.TaintEffectNoExecute},
			},
			want: []v1.Toleration{
				{Key: "group", Operator: v1.TolerationOpEqual, Value: "a:b#c - d", Effect: v1.TaintEffectNoExecute},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := v1.Node{Spec: v1.NodeSpec{Taints: tt.taints}}
			got := buildTolerations(node)
			if len(got) != len(tt.want) {
				t.Fatalf("buildTolerations() returned %d tolerations, want %d: got=%v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("buildTolerations()[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseAIPerfOutput(t *testing.T) {
	validJSON := `{
		"output_token_throughput": {"unit": "tokens/sec", "avg": 5667.5},
		"time_to_first_token": {"unit": "ms", "avg": 45.2, "p99": 84.1, "min": 20.0, "max": 95.3}
	}`

	tests := []struct {
		name           string
		logs           string
		wantThroughput float64
		wantTTFT       float64
		wantErrSubstr  string
	}{
		{
			name: "clean happy path",
			logs: fmt.Sprintf("some pip output\n%s\n%s\n%s\nmore noise",
				aiperfResultSentinel, validJSON, aiperfResultSentinel),
			wantThroughput: 5667.5,
			wantTTFT:       84.1,
		},
		{
			name: "JSON surrounded by noisy lines containing braces",
			logs: fmt.Sprintf("DEPRECATION: pip {something}\nfoo\n%s\n%s\n%s\n{trailing noise}",
				aiperfResultSentinel, validJSON, aiperfResultSentinel),
			wantThroughput: 5667.5,
			wantTTFT:       84.1,
		},
		{
			name:          "missing start sentinel — benchmark failed",
			logs:          "pip install failed: unable to reach PyPI\n",
			wantErrSubstr: "sentinel",
		},
		{
			name:          "start sentinel but no end — truncated logs",
			logs:          aiperfResultSentinel + "\n" + validJSON,
			wantErrSubstr: "end sentinel",
		},
		{
			name:          "malformed JSON between sentinels",
			logs:          fmt.Sprintf("%s\n{not valid json\n%s", aiperfResultSentinel, aiperfResultSentinel),
			wantErrSubstr: "parse",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAIPerfOutput(tt.logs)
			if tt.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("parseAIPerfOutput() expected error containing %q, got nil (result=%+v)",
						tt.wantErrSubstr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Errorf("parseAIPerfOutput() error %q does not contain %q", err, tt.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAIPerfOutput() unexpected error: %v", err)
			}
			if got.throughput != tt.wantThroughput {
				t.Errorf("throughput = %v, want %v", got.throughput, tt.wantThroughput)
			}
			if got.ttftP99Ms != tt.wantTTFT {
				t.Errorf("ttftP99Ms = %v, want %v", got.ttftP99Ms, tt.wantTTFT)
			}
			if got.status != "ok" {
				t.Errorf("status = %q, want %q", got.status, "ok")
			}
		})
	}
}

func TestIsDynamoDeploymentReady(t *testing.T) {
	// dgd builds a DynamoGraphDeployment with the given spec replica counts and
	// status.components entries (each entry is a field->count map, e.g.
	// {"replicas": 8, "readyReplicas": 8}).
	dgd := func(state string, spec map[string]int64, status map[string]map[string]int64) *unstructured.Unstructured {
		specComponents := make([]interface{}, 0, len(spec))
		for name, r := range spec {
			specComponents = append(specComponents, map[string]interface{}{keyName: name, "replicas": r})
		}
		statusComponents := map[string]interface{}{}
		for name, fields := range status {
			m := map[string]interface{}{}
			for k, v := range fields {
				m[k] = v
			}
			statusComponents[name] = m
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"spec":   map[string]interface{}{"components": specComponents},
			"status": map[string]interface{}{"state": state, "components": statusComponents},
		}}
	}
	tests := []struct {
		name  string
		input *unstructured.Unstructured
		want  bool
	}{
		{name: "nil object", input: nil, want: false},
		{
			name:  "no status",
			input: &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{}}},
			want:  false,
		},
		{
			name:  "state != successful",
			input: dgd("pending", map[string]int64{"VllmDecodeWorker": 8}, map[string]map[string]int64{"VllmDecodeWorker": {"replicas": 8, "readyReplicas": 8}}),
			want:  false,
		},
		{
			name:  "successful but status.components empty",
			input: dgd("successful", map[string]int64{"Frontend": 1, "VllmDecodeWorker": 8}, map[string]map[string]int64{}),
			want:  false,
		},
		{
			// Codex review gap: operator populates status.components
			// incrementally; the worker component is not represented yet.
			name:  "successful but worker absent from status",
			input: dgd("successful", map[string]int64{"Frontend": 1, "VllmDecodeWorker": 8}, map[string]map[string]int64{"Frontend": {"replicas": 1, "readyReplicas": 1}}),
			want:  false,
		},
		{
			name:  "successful but worker not all ready (5/8)",
			input: dgd("successful", map[string]int64{"Frontend": 1, "VllmDecodeWorker": 8}, map[string]map[string]int64{"Frontend": {"replicas": 1, "readyReplicas": 1}, "VllmDecodeWorker": {"replicas": 8, "readyReplicas": 5}}),
			want:  false,
		},
		{
			// Scale-up window: status.replicas lags spec (6 of 8 created), all
			// 6 ready. Comparing against spec (8) must still report not-ready.
			name:  "successful but worker still scaling up (6/8 desired)",
			input: dgd("successful", map[string]int64{"Frontend": 1, "VllmDecodeWorker": 8}, map[string]map[string]int64{"Frontend": {"replicas": 1, "readyReplicas": 1}, "VllmDecodeWorker": {"replicas": 6, "readyReplicas": 6}}),
			want:  false,
		},
		{
			name:  "successful and all desired components ready (readyReplicas)",
			input: dgd("successful", map[string]int64{"Frontend": 1, "VllmDecodeWorker": 8}, map[string]map[string]int64{"Frontend": {"replicas": 1, "readyReplicas": 1}, "VllmDecodeWorker": {"replicas": 8, "readyReplicas": 8}}),
			want:  true,
		},
		{
			name:  "successful with scaling-group availableReplicas fallback",
			input: dgd("successful", map[string]int64{"VllmDecodeWorker": 8}, map[string]map[string]int64{"VllmDecodeWorker": {"replicas": 8, "availableReplicas": 8}}),
			want:  true,
		},
		{
			// spec replicas omitted → defaults to 1; one ready replica satisfies it.
			name: "spec replicas omitted defaults to 1",
			input: &unstructured.Unstructured{Object: map[string]interface{}{
				"spec": map[string]interface{}{"components": []interface{}{
					map[string]interface{}{keyName: "VllmDecodeWorker"}, // no replicas field
				}},
				"status": map[string]interface{}{
					"state": "successful",
					"components": map[string]interface{}{
						"VllmDecodeWorker": map[string]interface{}{"replicas": int64(1), "readyReplicas": int64(1)},
					},
				},
			}},
			want: true,
		},
		{
			// Present-but-wrong-typed spec replicas must fail closed, not default to 1.
			name: "spec replicas wrong type fails closed",
			input: &unstructured.Unstructured{Object: map[string]interface{}{
				"spec": map[string]interface{}{"components": []interface{}{
					map[string]interface{}{keyName: "VllmDecodeWorker", "replicas": "eight"},
				}},
				"status": map[string]interface{}{
					"state": "successful",
					"components": map[string]interface{}{
						"VllmDecodeWorker": map[string]interface{}{"replicas": int64(8), "readyReplicas": int64(8)},
					},
				},
			}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDynamoDeploymentReady(tt.input); got != tt.want {
				t.Errorf("isDynamoDeploymentReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyInferenceWorkerScheduling(t *testing.T) {
	// Minimal DynamoGraphDeployment skeleton matching testdata/inference/dynamo-deployment.yaml structure.
	newObj := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "nvidia.com/v1beta1",
			"kind":       "DynamoGraphDeployment",
			"spec": map[string]interface{}{
				"components": []interface{}{
					map[string]interface{}{
						keyName:    "Frontend",
						"type":     "frontend",
						"replicas": int64(1),
						"podTemplate": map[string]interface{}{
							"spec": map[string]interface{}{
								"containers": []interface{}{map[string]interface{}{keyName: mainContainerName}},
							},
						},
					},
					map[string]interface{}{
						keyName:    "VllmDecodeWorker",
						"type":     "worker",
						"replicas": int64(4),
						"podTemplate": map[string]interface{}{
							"spec": map[string]interface{}{
								"containers": []interface{}{map[string]interface{}{keyName: mainContainerName}},
							},
						},
					},
				},
			},
		}}
	}

	// Table over the two worker GPU wiring modes (allocmode dispatch): DRA
	// resourceClaims when full-GPU DRA is usable, device-plugin limits
	// otherwise. nodeSelector/toleration/frontend behavior is mode-independent
	// and asserted in both cases.
	tests := []struct {
		name string
		mode *allocmode.Mode
		// draWiring is the NODE-LOCAL wiring decision buildInferenceConfig
		// stores on the config (chosen node ∈ Mode.DRANodes).
		draWiring bool
		// wantDRAWiring: worker carries podSpec.resourceClaims + container
		// resources.claims and NO scalar GPU limit; inverted otherwise.
		wantDRAWiring bool
	}{
		{
			name:          "device-plugin wiring (full-GPU DRA not usable)",
			mode:          &allocmode.Mode{DevicePluginUsable: true},
			draWiring:     false,
			wantDRAWiring: false,
		},
		{
			name:          "zero-value config falls back to device-plugin wiring",
			mode:          nil,
			draWiring:     false,
			wantDRAWiring: false,
		},
		{
			name:          "DRA wiring (chosen node advertises usable DRA devices)",
			mode:          &allocmode.Mode{DRAUsable: true, APIVersion: "v1", DRANodes: []string{"node1"}},
			draWiring:     true,
			wantDRAWiring: true,
		},
		{
			name: "global DRA usable but chosen node is device-plugin-only: plugin wiring",
			mode: &allocmode.Mode{DRAUsable: true, DevicePluginUsable: true,
				APIVersion: "v1", DRANodes: []string{"other-node"}},
			draWiring:     false,
			wantDRAWiring: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &inferenceWorkloadConfig{
				gpuCount:        4,
				gpuNodeSelector: map[string]string{"nodeGroup": "gpu-worker"},
				gpuTolerations: []v1.Toleration{
					{Key: "dedicated", Operator: v1.TolerationOpEqual, Value: "worker-workload", Effect: v1.TaintEffectNoSchedule},
				},
				gpuAllocMode:    tt.mode,
				draWorkerWiring: tt.draWiring,
			}

			obj := newObj()
			if err := applyInferenceWorkerScheduling(obj, config); err != nil {
				t.Fatalf("applyInferenceWorkerScheduling() error: %v", err)
			}

			// Worker must have nodeSelector and tolerations in every mode.
			worker := componentPodSpec(t, obj, "VllmDecodeWorker")
			if worker == nil {
				t.Fatal("VllmDecodeWorker podTemplate.spec not set")
			}
			ns, _, _ := unstructured.NestedMap(worker, "nodeSelector")
			if ns["nodeGroup"] != "gpu-worker" {
				t.Errorf("worker nodeSelector = %v, want nodeGroup=gpu-worker", ns)
			}
			tols, _, _ := unstructured.NestedSlice(worker, "tolerations")
			if len(tols) != 1 {
				t.Fatalf("worker tolerations count = %d, want 1", len(tols))
			}
			tol := tols[0].(map[string]interface{})
			if tol["key"] != "dedicated" || tol["value"] != "worker-workload" || tol["effect"] != "NoSchedule" {
				t.Errorf("worker toleration = %v, unexpected fields", tol)
			}

			claims, claimsFound, _ := unstructured.NestedSlice(worker, "resourceClaims")
			gpuLimit := mainContainerGPULimit(t, worker)
			if tt.wantDRAWiring {
				// DRA wiring: pod-level resourceClaims bound to the template,
				// container resources.claims referencing it, NO scalar limit.
				if !claimsFound || len(claims) != 1 {
					t.Fatalf("worker resourceClaims = %v (found=%t), want exactly one DRA claim binding", claims, claimsFound)
				}
				binding := claims[0].(map[string]interface{})
				if binding["resourceClaimTemplateName"] != inferenceClaimTemplateName {
					t.Errorf("claim binding template = %v, want %s", binding["resourceClaimTemplateName"], inferenceClaimTemplateName)
				}
				if gpuLimit != nil {
					t.Errorf("worker main container carries %s limit %v in DRA mode — must allocate via the claim only", gpuResourceName, gpuLimit)
				}
				if refs := mainContainerResourceClaims(t, worker); len(refs) != 1 {
					t.Errorf("worker main container resources.claims = %v, want exactly one reference", refs)
				}
			} else {
				// Device-plugin wiring: scalar limit, no claims anywhere.
				if claimsFound {
					t.Error("worker resourceClaims should not be set — GPUs are requested via device-plugin limits (#1327)")
				}
				if gpuLimit != "1" {
					t.Errorf("worker main container %s limit = %v, want \"1\"", gpuResourceName, gpuLimit)
				}
				if refs := mainContainerResourceClaims(t, worker); len(refs) != 0 {
					t.Errorf("worker main container resources.claims = %v, want none in device-plugin mode", refs)
				}
			}

			// Frontend must have tolerations AND the same nodeSelector as
			// worker — they co-locate on the GPU node cohort so cross-namespace
			// traffic stays inside a single node-group Security Group on EKS.
			// Frontend gets NO GPU wiring in either mode (it's CPU-only).
			frontend := componentPodSpec(t, obj, "Frontend")
			if frontend == nil {
				t.Fatal("Frontend podTemplate.spec not set")
			}
			frontTols, _, _ := unstructured.NestedSlice(frontend, "tolerations")
			if len(frontTols) != 1 {
				t.Errorf("frontend tolerations count = %d, want 1", len(frontTols))
			}
			frontNS, _, _ := unstructured.NestedMap(frontend, "nodeSelector")
			if frontNS["nodeGroup"] != "gpu-worker" {
				t.Errorf("frontend nodeSelector should match worker for SG co-location: got %v", frontNS)
			}
			if got := mainContainerGPULimit(t, frontend); got != nil {
				t.Errorf("frontend main container should not carry a GPU limit — only worker needs GPU allocation; got %v", got)
			}
			if _, found, _ := unstructured.NestedSlice(frontend, "resourceClaims"); found {
				t.Error("frontend resourceClaims should never be set — only worker needs GPU allocation")
			}
		})
	}
}

// mainContainerResourceClaims returns the main container's resources.claims
// entries (nil when absent).
func mainContainerResourceClaims(t *testing.T, podSpec map[string]interface{}) []interface{} {
	t.Helper()
	containers, _, _ := unstructured.NestedSlice(podSpec, "containers")
	for _, raw := range containers {
		container, ok := raw.(map[string]interface{})
		if !ok || container[keyName] != mainContainerName {
			continue
		}
		claims, _, _ := unstructured.NestedSlice(container, "resources", "claims")
		return claims
	}
	return nil
}

func TestApplyInferenceWorkerScheduling_MissingServices(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{},
	}}
	err := applyInferenceWorkerScheduling(obj, &inferenceWorkloadConfig{})
	if err == nil {
		t.Fatal("applyInferenceWorkerScheduling() expected error for missing spec.components, got nil")
	}
}

func TestEnsureMainContainerGPULimit_AppendsMainWhenMissing(t *testing.T) {
	podSpec := map[string]interface{}{
		"containers": []interface{}{
			map[string]interface{}{keyName: "sidecar-frontend"},
		},
	}
	ensureMainContainerGPULimit(podSpec, 1)

	containers, _, err := unstructured.NestedSlice(podSpec, "containers")
	if err != nil {
		t.Fatalf("read containers: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("containers count = %d, want 2: %v", len(containers), containers)
	}
	sidecar := containers[0].(map[string]interface{})
	if sidecar[keyName] != "sidecar-frontend" {
		t.Fatalf("first container = %v, want original sidecar preserved", sidecar)
	}
	if _, found, _ := unstructured.NestedString(sidecar, "resources", "limits", gpuResourceName); found {
		t.Fatal("sidecar unexpectedly received a GPU limit")
	}
	main := containers[1].(map[string]interface{})
	if main[keyName] != mainContainerName {
		t.Fatalf("appended container name = %v, want %s", main[keyName], mainContainerName)
	}
	limit, found, err := unstructured.NestedString(main, "resources", "limits", gpuResourceName)
	if err != nil || !found {
		t.Fatalf("read appended main resources.limits[%s]: found=%v err=%v", gpuResourceName, found, err)
	}
	if limit != "1" {
		t.Fatalf("appended main GPU limit = %q, want \"1\"", limit)
	}
}

// TestEnsureMainContainerGPULimit_PreservesExistingResources verifies the GPU
// limit is merged into an existing resources block (e.g. a template-supplied
// memory limit) rather than replacing it.
func TestEnsureMainContainerGPULimit_PreservesExistingResources(t *testing.T) {
	podSpec := map[string]interface{}{
		"containers": []interface{}{
			map[string]interface{}{
				keyName: mainContainerName,
				"resources": map[string]interface{}{
					"limits": map[string]interface{}{"memory": "2Gi"},
				},
			},
		},
	}
	ensureMainContainerGPULimit(podSpec, 1)

	containers, _, err := unstructured.NestedSlice(podSpec, "containers")
	if err != nil || len(containers) != 1 {
		t.Fatalf("read containers: err=%v count=%d", err, len(containers))
	}
	main := containers[0].(map[string]interface{})
	mem, found, err := unstructured.NestedString(main, "resources", "limits", "memory")
	if err != nil || !found || mem != "2Gi" {
		t.Errorf("existing memory limit lost: %q found=%v err=%v", mem, found, err)
	}
	gpu, found, err := unstructured.NestedString(main, "resources", "limits", gpuResourceName)
	if err != nil || !found || gpu != "1" {
		t.Errorf("GPU limit = %q found=%v err=%v, want \"1\"", gpu, found, err)
	}
}

func componentPodSpec(t *testing.T, obj *unstructured.Unstructured, name string) map[string]interface{} {
	t.Helper()
	components, _, err := unstructured.NestedSlice(obj.Object, "spec", "components")
	if err != nil {
		t.Fatalf("read spec.components: %v", err)
	}
	for _, raw := range components {
		component, ok := raw.(map[string]interface{})
		if !ok || component[keyName] != name {
			continue
		}
		podSpec, _, err := unstructured.NestedMap(component, "podTemplate", "spec")
		if err != nil {
			t.Fatalf("read %s podTemplate.spec: %v", name, err)
		}
		return podSpec
	}
	t.Fatalf("component %q not found", name)
	return nil
}

// mainContainerGPULimit returns the main container's
// resources.limits["nvidia.com/gpu"] value, or nil when the limit (or the
// main container's resources block) is absent.
func mainContainerGPULimit(t *testing.T, podSpec map[string]interface{}) interface{} {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(podSpec, "containers")
	if err != nil {
		t.Fatalf("read containers: %v", err)
	}
	for _, raw := range containers {
		container, ok := raw.(map[string]interface{})
		if !ok || container[keyName] != mainContainerName {
			continue
		}
		limits, found, err := unstructured.NestedMap(container, "resources", "limits")
		if err != nil {
			t.Fatalf("read main container resources.limits: %v", err)
		}
		if !found {
			return nil
		}
		return limits[gpuResourceName]
	}
	t.Fatal("main container not found")
	return nil
}

// TestParseDynamoTemplate_ScalarModelStaysString guards the quoting of
// value: "${MODEL}" in testdata/inference/dynamo-deployment.yaml. A
// scalar-looking model ID (pure-numeric / boolean-like / null-like) that
// passes validateModelID must round-trip through ${MODEL} substitution and
// YAML unmarshal as a *string*, not a YAML int/bool/null — otherwise the
// DynamoGraphDeployment would carry a typed SERVED_MODEL_NAME the controller
// rejects. If someone unquotes the template again, this fails.
func TestParseDynamoTemplate_ScalarModelStaysString(t *testing.T) {
	deployPath := filepath.Join("testdata", "inference", "dynamo-deployment.yaml")
	// Each model ID below is accepted by validateModelID yet looks like a
	// non-string YAML scalar when left unquoted.
	for _, model := range []string{"123", "1.5", "true", "null"} {
		t.Run(model, func(t *testing.T) {
			if err := validateModelID(model); err != nil {
				t.Fatalf("precondition: validateModelID(%q) = %v, want nil", model, err)
			}
			obj, err := parseYAMLTemplate(deployPath, map[string]string{
				"NAMESPACE":       "aicr-test",
				"MODEL":           model,
				"GPU_COUNT":       "1",
				"DEPLOYMENT_NAME": "aicr-inference",
			})
			if err != nil {
				t.Fatalf("parseYAMLTemplate() error: %v", err)
			}
			envs := componentContainerEnv(t, obj, "Frontend", mainContainerName)
			var got interface{}
			var found bool
			for _, e := range envs {
				m, ok := e.(map[string]interface{})
				if ok && m["name"] == "SERVED_MODEL_NAME" {
					got, found = m["value"], true
					break
				}
			}
			if !found {
				t.Fatal("SERVED_MODEL_NAME env not found in Frontend envs")
			}
			s, ok := got.(string)
			if !ok {
				t.Fatalf("SERVED_MODEL_NAME value = %T(%v), want string", got, got)
			}
			if s != model {
				t.Errorf("SERVED_MODEL_NAME = %q, want %q", s, model)
			}
		})
	}
}

func componentContainerEnv(t *testing.T, obj *unstructured.Unstructured, componentName, containerName string) []interface{} {
	t.Helper()
	podSpec := componentPodSpec(t, obj, componentName)
	containers, _, err := unstructured.NestedSlice(podSpec, "containers")
	if err != nil {
		t.Fatalf("read %s containers: %v", componentName, err)
	}
	for _, raw := range containers {
		container, ok := raw.(map[string]interface{})
		if !ok || container[keyName] != containerName {
			continue
		}
		env, _, err := unstructured.NestedSlice(container, "env")
		if err != nil {
			t.Fatalf("read %s/%s env: %v", componentName, containerName, err)
		}
		return env
	}
	t.Fatalf("container %s/%s not found", componentName, containerName)
	return nil
}

func TestBuildAIPerfJob_PrebuiltImageAndSentinel(t *testing.T) {
	// Isolate from the caller's environment: buildAIPerfJob resolves the
	// container image through resolveAiperfImage() which honors
	// AICR_CLI_VERSION (version pin), AICR_CLI_COMMIT (dev-build pin),
	// AICR_VALIDATOR_IMAGE_REGISTRY (registry override), and
	// AICR_VALIDATOR_IMAGE_TAG (tag override). A developer running
	// `go test` with any of these set would otherwise see spurious
	// failures on the image-equality assertion — the exact feature-branch
	// dogfooding workflow this override was added for.
	t.Setenv("AICR_CLI_VERSION", "")
	t.Setenv("AICR_CLI_COMMIT", "")
	t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", "")
	t.Setenv("AICR_VALIDATOR_IMAGE_TAG", "")
	// Neutralize tuning knobs so an AICR_INFERENCE_PERF_* value exported by the
	// runner can't make buildAIPerfJob error out before these image/sentinel
	// assertions run.
	clearTuningEnvs(t)

	pullSecrets := []v1.LocalObjectReference{
		{Name: "ghcr-mirror-pull"},
		{Name: "nvcr-pull"},
	}
	const jobName = "aicr-aiperf-run-42"
	job := mustBuildAIPerfJob(t, "test-ns", jobName, "http://frontend.test-ns.svc:8000", 16, pullSecrets)
	if job.Name != jobName {
		t.Errorf("job name = %q, want %q", job.Name, jobName)
	}
	if job.Namespace != "test-ns" {
		t.Errorf("job namespace = %q, want %q", job.Namespace, "test-ns")
	}
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(job.Spec.Template.Spec.Containers))
	}
	container := job.Spec.Template.Spec.Containers[0]

	if container.Image != aiperfBaseImage {
		t.Errorf("container image = %q, want %q", container.Image, aiperfBaseImage)
	}
	if !strings.HasPrefix(aiperfBaseImage, "ghcr.io/nvidia/aicr-validators/aiperf-bench") {
		t.Errorf("aiperfBaseImage %q should be the pre-built ghcr image", aiperfBaseImage)
	}

	script := container.Args[0]
	if strings.Contains(script, "pip install") {
		t.Errorf("script should not pip install at runtime — aiperf is baked into the image; got:\n%s", script)
	}
	if !strings.Contains(script, aiperfResultSentinel) {
		t.Errorf("script missing result sentinel %q", aiperfResultSentinel)
	}
	if strings.Contains(script, "2>&1") || strings.Contains(script, "> /dev/null") {
		t.Errorf("script should not silence stderr/stdout — benchmark errors must surface in pod logs")
	}
	// /bin/sh is sufficient (POSIX) and avoids a bash install in the image.
	if len(container.Command) == 0 || container.Command[0] != "/bin/sh" {
		t.Errorf("container.Command[0] = %v, want /bin/sh", container.Command)
	}

	// Pull secrets from the outer pod must propagate to the inner aiperf pod
	// so authenticated private-registry setups work end-to-end.
	got := job.Spec.Template.Spec.ImagePullSecrets
	if len(got) != len(pullSecrets) {
		t.Fatalf("pod ImagePullSecrets count = %d, want %d", len(got), len(pullSecrets))
	}
	for i, ref := range pullSecrets {
		if got[i].Name != ref.Name {
			t.Errorf("pod ImagePullSecrets[%d].Name = %q, want %q", i, got[i].Name, ref.Name)
		}
	}
}

func TestBuildAIPerfJob_NoPullSecrets(t *testing.T) {
	// nil/empty pullSecrets must not break construction; the field stays empty
	// and public-registry pulls work unchanged.
	clearTuningEnvs(t)
	job := mustBuildAIPerfJob(t, "test-ns", "aicr-aiperf-run-0", "http://ep:8000", 16, nil)
	if len(job.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("nil pullSecrets should yield empty ImagePullSecrets; got %v",
			job.Spec.Template.Spec.ImagePullSecrets)
	}
}

// TestBuildAIPerfJob_ImagePullPolicy asserts the inner aiperf container
// stays in lockstep with the outer validator Job's pull policy. Without
// this, setting AICR_VALIDATOR_IMAGE_TAG=edge on the CLI would re-pull
// the outer validator (Always) while the aiperf pod — lacking an explicit
// ImagePullPolicy — would default to IfNotPresent and serve a stale
// cached `:edge` image, defeating the motivating feature-branch workflow.
func TestBuildAIPerfJob_ImagePullPolicy(t *testing.T) {
	// Isolate from caller's environment so resolveAiperfImage is deterministic
	// across cases.
	t.Setenv("AICR_CLI_VERSION", "")
	t.Setenv("AICR_CLI_COMMIT", "")
	t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", "")
	// A runner-exported AICR_INFERENCE_PERF_* knob must not fail this
	// pull-policy test before the policy assertion runs.
	clearTuningEnvs(t)

	tests := []struct {
		name   string
		envTag string
		want   v1.PullPolicy
	}{
		{
			// Default path: aiperfBaseImage ends with :latest, so policy is Always
			// whether or not the override is set.
			name:   "no override — :latest → Always",
			envTag: "",
			want:   v1.PullAlways,
		},
		{
			name:   "override with mutable :edge tag → Always (no stale cache)",
			envTag: "edge",
			want:   v1.PullAlways,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICR_VALIDATOR_IMAGE_TAG", tt.envTag)
			job := mustBuildAIPerfJob(t, "ns", "aicr-aiperf-run-0", "http://ep:8000", 16, nil)
			got := job.Spec.Template.Spec.Containers[0].ImagePullPolicy
			if got != tt.want {
				t.Errorf("aiperf ImagePullPolicy = %q, want %q", got, tt.want)
			}
		})
	}
}

// mustBuildAIPerfJob calls buildAIPerfJob and fails the test on error, keeping
// the many default-path assertions terse. Cases that intentionally exercise a
// malformed knob assert the error from validatePerfTuningEnvs / intFromEnv
// directly instead.
func mustBuildAIPerfJob(t *testing.T, namespace, jobName, endpoint string, concurrency int, pullSecrets []v1.LocalObjectReference) *batchv1.Job {
	t.Helper()
	job, _, err := buildAIPerfJob(namespace, jobName, endpoint, "Qwen/Qwen3-8B", concurrency, pullSecrets)
	if err != nil {
		t.Fatalf("buildAIPerfJob: unexpected error: %v", err)
	}
	return job
}

// clearTuningEnvs neutralizes the AICR_INFERENCE_PERF_* knobs for the duration
// of the test so default-output assertions stay hermetic even when the runner
// environment exports them. intFromEnv treats an empty value as unset, so the
// constant defaults apply. t.Setenv restores prior values after the test.
func clearTuningEnvs(t *testing.T) {
	t.Helper()
	for _, e := range []string{
		envConcurrencyPerGPU, envWarmupPerConcurrency, envMinRequests,
		envRequestsPerConcurrency, envInputTokensMean, envOutputTokensMean,
		envModel, envWorkloadReadyTimeout, envHealthTimeout, envModelCacheSize,
	} {
		t.Setenv(e, "")
	}
}

// TestBuildAIPerfJob_ModelViaEnvNotShell verifies the model is passed through a
// container env var and referenced as "$AICR_MODEL" in the script, never
// interpolated into the /bin/sh -c text — so a model containing shell
// metacharacters cannot be command-substituted before the benchmark runs.
func TestBuildAIPerfJob_ModelViaEnvNotShell(t *testing.T) {
	clearTuningEnvs(t)
	malicious := "$(touch /tmp/pwned)"
	job, _, err := buildAIPerfJob("ns", "aicr-aiperf-run-0", "http://ep:8000", malicious, 16, nil)
	if err != nil {
		t.Fatalf("buildAIPerfJob: %v", err)
	}
	ctr := job.Spec.Template.Spec.Containers[0]
	script := ctr.Args[0]
	if strings.Contains(script, malicious) {
		t.Errorf("script must not interpolate the model verbatim (injection risk); script:\n%s", script)
	}
	if !strings.Contains(script, `"$AICR_MODEL"`) {
		t.Errorf("script must reference the model via \"$AICR_MODEL\"; script:\n%s", script)
	}
	var got string
	found := false
	for _, e := range ctr.Env {
		if e.Name == "AICR_MODEL" {
			got, found = e.Value, true
		}
	}
	if !found || got != malicious {
		t.Errorf("AICR_MODEL env = %q (found=%v), want the raw model %q carried as data", got, found, malicious)
	}
}

func TestBuildAIPerfJob_RequestCountFloor(t *testing.T) {
	clearTuningEnvs(t)
	tests := []struct {
		name        string
		concurrency int
		wantMinReqs int
	}{
		{"low concurrency — floor at aiperfMinRequests", 16, aiperfMinRequests},
		{"high concurrency — scaled by aiperfRequestsPerConcurrency", 500, 500 * aiperfRequestsPerConcurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := mustBuildAIPerfJob(t, "ns", "aicr-aiperf-run-0", "http://ep:8000", tt.concurrency, nil)
			script := job.Spec.Template.Spec.Containers[0].Args[0]
			needle := fmt.Sprintf("--request-count %d", tt.wantMinReqs)
			if !strings.Contains(script, needle) {
				t.Errorf("script missing %q; script:\n%s", needle, script)
			}
		})
	}
}

// TestBuildAIPerfJob_Warmup verifies a warmup-request-count is emitted and
// scales with concurrency, so vLLM's one-time compile cost is excluded from the
// measured p99 TTFT.
func TestBuildAIPerfJob_Warmup(t *testing.T) {
	clearTuningEnvs(t)
	tests := []struct {
		name        string
		concurrency int
	}{
		{"low concurrency", 16},
		{"medium concurrency", 128},
		{"high concurrency", 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := mustBuildAIPerfJob(t, "ns", "aicr-aiperf-run-0", "http://ep:8000", tt.concurrency, nil)
			script := job.Spec.Template.Spec.Containers[0].Args[0]
			needle := fmt.Sprintf("--warmup-request-count %d", tt.concurrency*aiperfWarmupPerConcurrency)
			if !strings.Contains(script, needle) {
				t.Errorf("concurrency=%d: script missing %q; script:\n%s", tt.concurrency, needle, script)
			}
		})
	}
}

// TestIntFromEnv verifies the catalog-tuning env reader: an unset knob returns
// the default, a valid positive integer is parsed, and a non-integer / zero /
// negative value returns an error so a typo in the catalog entry can't silently
// ship a benchmark run under unintended settings.
func TestIntFromEnv(t *testing.T) {
	const (
		env = "AICR_INFERENCE_PERF_TEST_KNOB"
		def = 42
	)
	tests := []struct {
		name    string
		val     string
		want    int
		wantErr bool
	}{
		{"empty/unset → default", "", def, false},
		{"valid positive → override", "7", 7, false},
		{"zero → error", "0", 0, true},
		{"negative → error", "-3", 0, true},
		{"non-integer → error", "abc", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Always t.Setenv (never leave it to the inherited environment):
			// it overrides any runner-exported value and restores it after the
			// subtest, and "" makes intFromEnv treat the knob as unset. This
			// keeps every case hermetic.
			t.Setenv(env, tt.val)
			got, err := intFromEnv(env, def)
			if (err != nil) != tt.wantErr {
				t.Errorf("intFromEnv(%q=%q) err = %v, wantErr %v", env, tt.val, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("intFromEnv(%q=%q) = %d, want %d", env, tt.val, got, tt.want)
			}
		})
	}
}

// TestValidatePerfTuningEnvs verifies the up-front gate fails closed
// (ErrCodeInvalidRequest) on a malformed knob and passes when knobs are unset
// or valid — so a typo aborts before the benchmark workload is deployed.
func TestValidatePerfTuningEnvs(t *testing.T) {
	t.Run("all unset → ok", func(t *testing.T) {
		clearTuningEnvs(t)
		if err := validatePerfTuningEnvs(); err != nil {
			t.Errorf("unexpected error with all knobs unset: %v", err)
		}
	})
	t.Run("all valid → ok", func(t *testing.T) {
		clearTuningEnvs(t)
		t.Setenv(envMinRequests, "2000")
		t.Setenv(envConcurrencyPerGPU, "8")
		if err := validatePerfTuningEnvs(); err != nil {
			t.Errorf("unexpected error with valid knobs: %v", err)
		}
	})
	t.Run("malformed → ErrCodeInvalidRequest", func(t *testing.T) {
		clearTuningEnvs(t)
		t.Setenv(envWarmupPerConcurrency, "lots")
		err := validatePerfTuningEnvs()
		if err == nil {
			t.Fatal("expected an error for a non-integer knob")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
		}
	})
	t.Run("malformed timeout knob → ErrCodeInvalidRequest", func(t *testing.T) {
		clearTuningEnvs(t)
		t.Setenv(envWorkloadReadyTimeout, "soon") // not a Go duration
		err := validatePerfTuningEnvs()
		if err == nil {
			t.Fatal("expected an error for a malformed duration knob")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
		}
	})
	t.Run("malformed health-timeout knob → ErrCodeInvalidRequest", func(t *testing.T) {
		clearTuningEnvs(t)
		t.Setenv(envHealthTimeout, "soon") // not a Go duration
		err := validatePerfTuningEnvs()
		if err == nil {
			t.Fatal("expected an error for a malformed health-timeout knob")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
		}
	})
	t.Run("valid health-timeout knob → ok", func(t *testing.T) {
		clearTuningEnvs(t)
		t.Setenv(envHealthTimeout, "15m")
		if err := validatePerfTuningEnvs(); err != nil {
			t.Errorf("unexpected error for valid health timeout: %v", err)
		}
	})
	t.Run("malformed model-cache size → ErrCodeInvalidRequest", func(t *testing.T) {
		clearTuningEnvs(t)
		t.Setenv(envModelCacheSize, "lots-of-space") // not a quantity
		err := validatePerfTuningEnvs()
		if err == nil {
			t.Fatal("expected an error for a malformed cache-size knob")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
		}
	})
	t.Run("valid model-cache size → ok", func(t *testing.T) {
		clearTuningEnvs(t)
		t.Setenv(envModelCacheSize, "100Gi")
		if err := validatePerfTuningEnvs(); err != nil {
			t.Errorf("unexpected error for valid cache size: %v", err)
		}
	})
}

// TestResolveInferenceModel verifies the model knob: unset/empty/whitespace
// falls back to the default smoke-test model, and a set value overrides it
// (trimmed). This is what lets characterization runs select a larger model
// without rebuilding the validator image.
func TestResolveInferenceModel(t *testing.T) {
	tests := []struct {
		name string
		val  string
		set  bool
		want string
	}{
		{"unset → default", "", false, inferenceModel},
		{"empty → default", "", true, inferenceModel},
		{"whitespace → default", "   ", true, inferenceModel},
		{"override", "Qwen/Qwen3-8B", true, "Qwen/Qwen3-8B"},
		{"override trimmed", "  Qwen/Qwen3-32B  ", true, "Qwen/Qwen3-32B"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(envModel, tt.val)
			} else {
				t.Setenv(envModel, "")
			}
			if got := resolveInferenceModel(); got != tt.want {
				t.Errorf("resolveInferenceModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ctxWithPerfConstraints builds a validators.Context whose recipe carries the
// given performance constraints, for exercising the recipe > env > default
// resolution.
func ctxWithPerfConstraints(cs ...recipe.Constraint) *validators.Context {
	return &validators.Context{ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{
		Validation: &recipe.ValidationConfig{
			Performance: &recipe.ValidationPhase{Constraints: cs},
		},
	})}
}

// TestResolveModel verifies the model resolution precedence recipe > env >
// default: a per-accelerator `inference-model` constraint wins over the env
// knob, the env knob wins over the compiled default, and a blank/absent recipe
// value falls through.
func TestResolveModel(t *testing.T) {
	modelC := func(v string) recipe.Constraint { return recipe.Constraint{Name: perfConstraintModel, Value: v} }
	tests := []struct {
		name   string
		ctx    *validators.Context
		envVal string
		want   string
	}{
		{"recipe wins over env", ctxWithPerfConstraints(modelC("Qwen/Qwen3-32B")), "Qwen/Qwen3-8B", "Qwen/Qwen3-32B"},
		{"recipe trimmed", ctxWithPerfConstraints(modelC("  meta-llama/Llama-3.1-70B  ")), "", "meta-llama/Llama-3.1-70B"},
		{"no recipe → env", ctxWithPerfConstraints(), "Qwen/Qwen3-14B", "Qwen/Qwen3-14B"},
		{"blank recipe → env", ctxWithPerfConstraints(modelC("   ")), "Qwen/Qwen3-14B", "Qwen/Qwen3-14B"},
		{"no recipe, no env → default", ctxWithPerfConstraints(), "", inferenceModel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envModel, tt.envVal)
			if got := resolveModel(tt.ctx); got != tt.want {
				t.Errorf("resolveModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestValidateModelID accepts well-formed Hugging Face model IDs and rejects
// values with YAML/shell metacharacters that could break the Dynamo deploy YAML.
func TestValidateModelID(t *testing.T) {
	for _, ok := range []string{"Qwen/Qwen3-8B", "meta-llama/Llama-3.1-70B-Instruct", "gpt2", "org_x/model.v2"} {
		if err := validateModelID(ok); err != nil {
			t.Errorf("validateModelID(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"$(touch x)", "a:b", `a"b`, "a b", "a\nb", "", "../etc"} {
		if err := validateModelID(bad); err == nil {
			t.Errorf("validateModelID(%q) = nil, want ErrCodeInvalidRequest", bad)
		} else if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("validateModelID(%q) error code = %v, want ErrCodeInvalidRequest", bad, err)
		}
	}
}

// TestResolveConcurrencyPerGPU verifies recipe > env > default precedence and
// that an invalid recipe value fails closed with ErrCodeInvalidRequest.
func TestResolveConcurrencyPerGPU(t *testing.T) {
	concC := func(v string) recipe.Constraint {
		return recipe.Constraint{Name: perfConstraintConcurrency, Value: v}
	}
	tests := []struct {
		name    string
		ctx     *validators.Context
		envVal  string
		want    int
		wantErr bool
	}{
		{"recipe wins over env", ctxWithPerfConstraints(concC("256")), "999", 256, false},
		{"recipe trimmed", ctxWithPerfConstraints(concC("  128  ")), "", 128, false},
		{"no recipe → env", ctxWithPerfConstraints(), "64", 64, false},
		{"blank recipe → env", ctxWithPerfConstraints(concC("   ")), "64", 64, false},
		{"no recipe, no env → default", ctxWithPerfConstraints(), "", aiperfConcurrencyPerGPU, false},
		{"recipe non-integer → error", ctxWithPerfConstraints(concC("lots")), "", 0, true},
		{"recipe zero → error", ctxWithPerfConstraints(concC("0")), "", 0, true},
		{"recipe negative → error", ctxWithPerfConstraints(concC("-8")), "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envConcurrencyPerGPU, tt.envVal)
			got, err := resolveConcurrencyPerGPU(tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveConcurrencyPerGPU() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if got != tt.want {
				t.Errorf("resolveConcurrencyPerGPU() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveRoutingMode(t *testing.T) {
	routingC := func(v string) recipe.Constraint {
		return recipe.Constraint{Name: perfConstraintRoutingMode, Value: v}
	}
	tests := []struct {
		name    string
		ctx     *validators.Context
		want    inferenceRoutingMode
		wantErr bool
	}{
		{"no recipe defaults to dynamo-router", ctxWithPerfConstraints(), inferenceRoutingModeDynamoRouter, false},
		{"blank recipe defaults to dynamo-router", ctxWithPerfConstraints(routingC("   ")), inferenceRoutingModeDynamoRouter, false},
		{"explicit dynamo-router", ctxWithPerfConstraints(routingC("dynamo-router")), inferenceRoutingModeDynamoRouter, false},
		{"explicit gateway-epp", ctxWithPerfConstraints(routingC("gateway-epp")), inferenceRoutingModeGatewayEPP, false},
		{"invalid mode fails closed", ctxWithPerfConstraints(routingC("load-only")), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRoutingMode(tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveRoutingMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if got != tt.want {
				t.Errorf("resolveRoutingMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDynamoDeploymentTemplate(t *testing.T) {
	tests := []struct {
		name string
		mode inferenceRoutingMode
		want string
	}{
		{"default router template", inferenceRoutingModeDynamoRouter, "dynamo-deployment.yaml"},
		{"gateway epp template", inferenceRoutingModeGatewayEPP, "dynamo-deployment-gateway-epp.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dynamoDeploymentTemplate(tt.mode); got != tt.want {
				t.Errorf("dynamoDeploymentTemplate(%q) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

func TestResolveInferenceEndpoint(t *testing.T) {
	const ns = "aicr-inference-perf-test"
	frontendSvc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: inferenceDeploymentName + "-frontend", Namespace: ns},
		Spec:       v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 9000}}},
	}
	gatewaySvc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: inferenceGatewayName, Namespace: inferenceGatewayNamespace},
		Spec:       v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 8080}}},
	}

	t.Run("dynamo-router uses frontend service", func(t *testing.T) {
		ctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(frontendSvc, gatewaySvc)}
		config := &inferenceWorkloadConfig{namespace: ns, routingMode: inferenceRoutingModeDynamoRouter}
		want := "http://aicr-inference-perf-frontend.aicr-inference-perf-test.svc:9000"
		got, err := resolveInferenceEndpoint(ctx, config)
		if err != nil {
			t.Fatalf("resolveInferenceEndpoint() error: %v", err)
		}
		if got != want {
			t.Errorf("resolveInferenceEndpoint() = %q, want %q", got, want)
		}
	})

	t.Run("gateway-epp uses inference gateway service", func(t *testing.T) {
		ctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(frontendSvc, gatewaySvc)}
		config := &inferenceWorkloadConfig{namespace: ns, routingMode: inferenceRoutingModeGatewayEPP}
		want := "http://inference-gateway.agentgateway-system.svc:8080"
		got, err := resolveInferenceEndpoint(ctx, config)
		if err != nil {
			t.Fatalf("resolveInferenceEndpoint() error: %v", err)
		}
		if got != want {
			t.Errorf("resolveInferenceEndpoint() = %q, want %q", got, want)
		}
	})

	t.Run("gateway-epp falls back to conventional endpoint", func(t *testing.T) {
		ctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset()}
		config := &inferenceWorkloadConfig{namespace: ns, routingMode: inferenceRoutingModeGatewayEPP}
		got, err := resolveInferenceEndpoint(ctx, config)
		if err != nil {
			t.Fatalf("resolveInferenceEndpoint() error: %v", err)
		}
		if got != defaultGatewayEndpoint() {
			t.Errorf("resolveInferenceEndpoint() = %q, want %q", got, defaultGatewayEndpoint())
		}
	})

	t.Run("gateway-epp surfaces service list errors", func(t *testing.T) {
		client := fake.NewClientset()
		client.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stderrors.New("service list denied")
		})
		ctx := &validators.Context{Ctx: context.Background(), Clientset: client}
		config := &inferenceWorkloadConfig{namespace: ns, routingMode: inferenceRoutingModeGatewayEPP}
		got, err := resolveInferenceEndpoint(ctx, config)
		if err == nil {
			t.Fatalf("resolveInferenceEndpoint() = %q, want error", got)
		}
		if !strings.Contains(err.Error(), "failed to list inference gateway services") {
			t.Fatalf("resolveInferenceEndpoint() error = %v, want gateway list context", err)
		}
	})
}

// TestDurationFromEnv verifies the duration knob reader: unset → default, a
// valid Go duration parses, and a malformed / non-positive value returns
// ErrCodeInvalidRequest so a typo aborts the check rather than silently
// running under the default.
func TestDurationFromEnv(t *testing.T) {
	const env = "AICR_INFERENCE_PERF_TEST_DURATION"
	def := 10 * time.Minute
	tests := []struct {
		name    string
		val     string
		want    time.Duration
		wantErr bool
	}{
		{"empty/unset → default", "", def, false},
		{"valid → override", "30m", 30 * time.Minute, false},
		{"zero → error", "0s", 0, true},
		{"negative → error", "-5m", 0, true},
		{"malformed → error", "soon", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(env, tt.val)
			got, err := durationFromEnv(env, def)
			if (err != nil) != tt.wantErr {
				t.Errorf("durationFromEnv(%q=%q) err = %v, wantErr %v", env, tt.val, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("durationFromEnv(%q=%q) = %v, want %v", env, tt.val, got, tt.want)
			}
		})
	}
}

// TestEnsureHFTokenSecret verifies the optional HF-token Secret is provisioned
// only when HF_TOKEN is set in the validator env, holds the token under the
// expected key, and is updated (not duplicated/erroring) when it already exists
// from a re-used namespace.
func TestEnsureHFTokenSecret(t *testing.T) {
	const ns = "aicr-inference-perf-test"

	t.Run("no token → no secret", func(t *testing.T) {
		t.Setenv(envHFToken, "")
		client := fake.NewClientset()
		ctx := &validators.Context{Ctx: context.Background(), Clientset: client}
		if err := ensureHFTokenSecret(ctx, ns); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := client.CoreV1().Secrets(ns).Get(context.Background(), hfTokenSecretName, metav1.GetOptions{}); err == nil {
			t.Error("secret should not exist when HF_TOKEN is unset")
		}
	})

	t.Run("token set → secret created with token", func(t *testing.T) {
		t.Setenv(envHFToken, "hf_testtoken")
		client := fake.NewClientset()
		ctx := &validators.Context{Ctx: context.Background(), Clientset: client}
		if err := ensureHFTokenSecret(ctx, ns); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		sec, err := client.CoreV1().Secrets(ns).Get(context.Background(), hfTokenSecretName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("secret not created: %v", err)
		}
		if got := sec.StringData[hfTokenSecretKey]; got != "hf_testtoken" {
			// fake client preserves StringData; real API moves it to Data.
			if gotData := string(sec.Data[hfTokenSecretKey]); gotData != "hf_testtoken" && got != "hf_testtoken" {
				t.Errorf("secret %s=%q/%q, want hf_testtoken", hfTokenSecretKey, got, gotData)
			}
		}
	})

	t.Run("existing secret → updated to new token", func(t *testing.T) {
		t.Setenv(envHFToken, "hf_new")
		client := fake.NewClientset(&v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: hfTokenSecretName, Namespace: ns, ResourceVersion: "7"},
			StringData: map[string]string{hfTokenSecretKey: "hf_old"},
		})
		ctx := &validators.Context{Ctx: context.Background(), Clientset: client}
		if err := ensureHFTokenSecret(ctx, ns); err != nil {
			t.Fatalf("unexpected error updating existing secret: %v", err)
		}
		sec, err := client.CoreV1().Secrets(ns).Get(context.Background(), hfTokenSecretName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("secret missing after update: %v", err)
		}
		// fake client keeps StringData; a real API moves it to Data — accept either.
		if got, gotData := sec.StringData[hfTokenSecretKey], string(sec.Data[hfTokenSecretKey]); got != "hf_new" && gotData != "hf_new" {
			t.Errorf("token after update = %q/%q, want hf_new (stale hf_old must be replaced)", got, gotData)
		}
	})

	t.Run("unset token clears a stale secret", func(t *testing.T) {
		// A reused per-run namespace must not silently inject an old token via
		// the workers' optional secretKeyRefs when this run is anonymous.
		t.Setenv(envHFToken, "")
		client := fake.NewClientset(&v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: hfTokenSecretName, Namespace: ns},
			StringData: map[string]string{hfTokenSecretKey: "hf_stale"},
		})
		ctx := &validators.Context{Ctx: context.Background(), Clientset: client}
		if err := ensureHFTokenSecret(ctx, ns); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := client.CoreV1().Secrets(ns).Get(context.Background(), hfTokenSecretName, metav1.GetOptions{}); err == nil {
			t.Error("stale HF token secret should be deleted when HF_TOKEN is unset")
		}
	})
}

// TestBuildAIPerfJob_EnvOverrides verifies the AICR_INFERENCE_PERF_* knobs flow
// into the AIPerf invocation so operators can retune without an image rebuild.
func TestBuildAIPerfJob_EnvOverrides(t *testing.T) {
	t.Setenv(envMinRequests, "2000")
	t.Setenv(envRequestsPerConcurrency, "4") // 100*4=400 < 2000 floor
	t.Setenv(envWarmupPerConcurrency, "3")   // 100*3=300
	t.Setenv(envInputTokensMean, "64")
	t.Setenv(envOutputTokensMean, "256")

	job := mustBuildAIPerfJob(t, "ns", "run-0", "http://ep:8000", 100, nil)
	script := job.Spec.Template.Spec.Containers[0].Args[0]
	for _, needle := range []string{
		"--request-count 2000",
		"--warmup-request-count 300",
		"--prompt-input-tokens-mean 64",
		"--prompt-output-tokens-mean 256",
	} {
		if !strings.Contains(script, needle) {
			t.Errorf("script missing %q; script:\n%s", needle, script)
		}
	}
}

// TestBuildAIPerfJob_ReturnedParams verifies buildAIPerfJob reports the resolved
// request/warmup counts it baked into the script, so runAIPerfJob can log the
// values actually sent to aiperf instead of the bare constant defaults.
func TestBuildAIPerfJob_ReturnedParams(t *testing.T) {
	t.Run("defaults scale with concurrency", func(t *testing.T) {
		clearTuningEnvs(t)
		// 128*8 = 1024 exceeds the 1000 floor, so the count is the scaled value
		// — exactly the case the old log (which printed the 1000 constant) got
		// wrong.
		_, params, err := buildAIPerfJob("ns", "run-0", "http://ep:8000", "Qwen/Qwen3-8B", 128, nil)
		if err != nil {
			t.Fatalf("buildAIPerfJob: unexpected error: %v", err)
		}
		if params.requestCount != 128*aiperfRequestsPerConcurrency {
			t.Errorf("requestCount = %d, want %d", params.requestCount, 128*aiperfRequestsPerConcurrency)
		}
		if params.warmupCount != 128*aiperfWarmupPerConcurrency {
			t.Errorf("warmupCount = %d, want %d", params.warmupCount, 128*aiperfWarmupPerConcurrency)
		}
	})
	t.Run("honors env overrides", func(t *testing.T) {
		clearTuningEnvs(t)
		t.Setenv(envMinRequests, "2000")
		t.Setenv(envRequestsPerConcurrency, "4") // 100*4=400 < 2000 floor
		t.Setenv(envWarmupPerConcurrency, "3")   // 100*3=300
		_, params, err := buildAIPerfJob("ns", "run-0", "http://ep:8000", "Qwen/Qwen3-8B", 100, nil)
		if err != nil {
			t.Fatalf("buildAIPerfJob: unexpected error: %v", err)
		}
		if params.requestCount != 2000 {
			t.Errorf("requestCount = %d, want 2000", params.requestCount)
		}
		if params.warmupCount != 300 {
			t.Errorf("warmupCount = %d, want 300", params.warmupCount)
		}
	})
}

func TestResolveAiperfImage(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{
			name:    "no version — returns hardcoded base image unchanged",
			version: "",
			want:    aiperfBaseImage,
		},
		{
			name:    "dev build does not rewrite",
			version: "dev",
			want:    aiperfBaseImage,
		},
		{
			name:    "release version rewrites :latest to :vX.Y.Z",
			version: "v0.12.0",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:v0.12.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICR_CLI_VERSION", tt.version)
			t.Setenv("AICR_CLI_COMMIT", "")
			t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", "")
			t.Setenv("AICR_VALIDATOR_IMAGE_TAG", "")
			if got := resolveAiperfImage(); got != tt.want {
				t.Errorf("resolveAiperfImage() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("registry override applies", func(t *testing.T) {
		t.Setenv("AICR_CLI_VERSION", "dev")
		t.Setenv("AICR_CLI_COMMIT", "")
		t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", "localhost:5001")
		t.Setenv("AICR_VALIDATOR_IMAGE_TAG", "")
		want := "localhost:5001/aicr-validators/aiperf-bench:latest"
		if got := resolveAiperfImage(); got != want {
			t.Errorf("resolveAiperfImage() = %q, want %q", got, want)
		}
	})
}

func TestNodesMatchingSelector(t *testing.T) {
	h100 := v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "h100-a",
		Labels: map[string]string{"nodeGroup": "gpu-h100", "zone": "us-east-1a"}}}
	h100b := v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "h100-b",
		Labels: map[string]string{"nodeGroup": "gpu-h100", "zone": "us-east-1b"}}}
	b200 := v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "b200-a",
		Labels: map[string]string{"nodeGroup": "gpu-b200"}}}
	nodes := []v1.Node{h100, h100b, b200}

	tests := []struct {
		name     string
		selector map[string]string
		wantLen  int
		wantName string // first returned name, if wantLen > 0
	}{
		{"nil selector returns all", nil, 3, "h100-a"},
		{"empty selector returns all", map[string]string{}, 3, "h100-a"},
		{"single key matches subset", map[string]string{"nodeGroup": "gpu-h100"}, 2, "h100-a"},
		{"multi-key narrows further", map[string]string{"nodeGroup": "gpu-h100", "zone": "us-east-1b"}, 1, "h100-b"},
		{"no match returns empty", map[string]string{"nodeGroup": "gpu-a100"}, 0, ""},
		{"key absent from node returns empty", map[string]string{"missing": "x"}, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nodesMatchingSelector(nodes, tt.selector)
			if len(got) != tt.wantLen {
				t.Fatalf("got %d matches, want %d: %v", len(got), tt.wantLen, got)
			}
			if tt.wantLen > 0 && got[0].Name != tt.wantName {
				t.Errorf("first match = %q, want %q", got[0].Name, tt.wantName)
			}
		})
	}
}

// countUsedGPUsByNodeMerged folds the per-ledger maps back into the
// aggregate view the pre-split table cases assert; the scalar/DRA split
// itself is pinned by TestCountUsedGPUsByNode_LedgerSplit.
func countUsedGPUsByNodeMerged(ctx context.Context, clientset kubernetes.Interface, poolNodes map[string]string) (map[string]int, error) {
	scalar, dra, err := countUsedGPUsByNode(ctx, clientset, poolNodes)
	if err != nil {
		return nil, err
	}
	merged := make(map[string]int, len(scalar)+len(dra))
	for k, v := range scalar {
		merged[k] += v
	}
	for k, v := range dra {
		merged[k] += v
	}
	return merged, nil
}

// TestCountUsedGPUsByNode_LedgerSplit pins the per-allocator separation the
// cross-ledger fix depends on (#1620 review): scalar device-plugin requests
// and DRA allocations on the same node land in DIFFERENT maps, so
// selectWorkerNode can reject DRA candidates with scalar occupancy instead
// of subtracting counts across ledgers.
func TestCountUsedGPUsByNode_LedgerSplit(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "scalar-holder", Namespace: "default"},
		Spec: v1.PodSpec{NodeName: "node-a", Containers: []v1.Container{{
			Name: "main",
			Resources: v1.ResourceRequirements{Limits: v1.ResourceList{
				v1.ResourceName(gpuResourceName): *resource.NewQuantity(1, resource.DecimalSI),
			}},
		}}},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "dra-holder", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{
			Requests: []resourcev1.DeviceRequest{{
				Name:    "r0",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: gpuDRADriverName},
			}},
		}},
		Status: resourcev1.ResourceClaimStatus{Allocation: &resourcev1.AllocationResult{
			Devices: resourcev1.DeviceAllocationResult{Results: []resourcev1.DeviceRequestAllocationResult{
				{Request: "r0", Device: "gpu-0", Driver: gpuDRADriverName, Pool: "node-a"},
			}},
		}},
	}
	client := fake.NewClientset(pod, claim)
	scalar, dra, err := countUsedGPUsByNode(context.Background(), client,
		map[string]string{"node-a": "node-a"})
	if err != nil {
		t.Fatalf("countUsedGPUsByNode() unexpected error: %v", err)
	}
	if scalar["node-a"] != 1 {
		t.Errorf("scalarUsed[node-a] = %d, want 1 (device-plugin pod request)", scalar["node-a"])
	}
	if dra["node-a"] != 1 {
		t.Errorf("draUsed[node-a] = %d, want 1 (DRA allocation)", dra["node-a"])
	}
}

func TestCountUsedGPUsByNode(t *testing.T) {
	makeClaim := func(ns, name string, results []resourcev1.DeviceRequestAllocationResult) *resourcev1.ResourceClaim {
		c := &resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		}
		if len(results) > 0 {
			c.Status.Allocation = &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{Results: results},
			}
		}
		return c
	}
	// withClaimClasses appends one exactly-form device request per class to
	// the claim's spec — the shape the class screen reads (kai classifies
	// claim demand by these names).
	withClaimClasses := func(c *resourcev1.ResourceClaim, classes ...string) *resourcev1.ResourceClaim {
		for i, class := range classes {
			c.Spec.Devices.Requests = append(c.Spec.Devices.Requests, resourcev1.DeviceRequest{
				Name:    fmt.Sprintf("r%d", i),
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: class},
			})
		}
		return c
	}
	// makePod builds a pod bound to node (empty = unscheduled) in the given
	// phase, whose single container requests gpus GPUs via limits — the
	// device-plugin form manifests actually carry (requests defaulted from
	// limits by the API server; countUsedGPUsByNode must fall back to limits).
	makePod := func(name, node string, phase v1.PodPhase, gpus int64) *v1.Pod {
		container := v1.Container{Name: "main"}
		if gpus > 0 {
			container.Resources = v1.ResourceRequirements{
				Limits: v1.ResourceList{
					v1.ResourceName(gpuResourceName): *resource.NewQuantity(gpus, resource.DecimalSI),
				},
			}
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       v1.PodSpec{NodeName: node, Containers: []v1.Container{container}},
			Status:     v1.PodStatus{Phase: phase},
		}
	}

	// Slice-derived pool → node attribution map (see scanGPUSliceTopology):
	// in the supported NVIDIA layout each node publishes one pool named
	// after itself, which this default mirrors. Cases override it to pin
	// non-node-named pools and unknown-pool rejection.
	defaultPoolNodes := map[string]string{"node-a": "node-a", "node-b": "node-b"}

	tests := []struct {
		name string
		objs []runtime.Object
		// poolNodes overrides the slice-derived pool → node map
		// (defaultPoolNodes when nil).
		poolNodes map[string]string
		want      map[string]int
		// wantErr, when non-empty, asserts countUsedGPUsByNode fails and the
		// error message contains this substring (fail-fast rejected state).
		wantErr string
	}{
		{
			name: "device-plugin pods only — running pods summed per node",
			objs: []runtime.Object{
				makePod("w0", "node-a", v1.PodRunning, 4),
				makePod("w1", "node-a", v1.PodRunning, 2),
				makePod("w2", "node-b", v1.PodRunning, 8),
			},
			want: map[string]int{"node-a": 6, "node-b": 8},
		},
		{
			name: "terminal and unbound pods do not count",
			objs: []runtime.Object{
				makePod("done", "node-a", v1.PodSucceeded, 8),
				makePod("crashed", "node-a", v1.PodFailed, 8),
				makePod("queued", "", v1.PodPending, 8),
				makePod("cpu-only", "node-a", v1.PodRunning, 0),
			},
			want: map[string]int{},
		},
		{
			name: "bound pending pod counts — device is committed before Running",
			objs: []runtime.Object{
				makePod("starting", "node-a", v1.PodPending, 2),
			},
			want: map[string]int{"node-a": 2},
		},
		{
			name: "DRA-only allocations counted per pool",
			objs: []runtime.Object{
				makeClaim("dynamo", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Device: "gpu-1", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-b"},
				}),
			},
			want: map[string]int{"node-a": 2, "node-b": 1},
		},
		{
			name: "non-GPU DRA drivers are ignored",
			objs: []runtime.Object{
				makeClaim("ns", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Device: "tpu-0", Driver: "tpu.google.com", Pool: "node-a"},
				}),
			},
			want: map[string]int{"node-a": 1},
		},
		{
			name: "unallocated claim — nothing counted",
			objs: []runtime.Object{
				makeClaim("ns", "pending", nil),
			},
			want: map[string]int{},
		},
		{
			name: "device-plugin and DRA usage combine on the same node",
			objs: []runtime.Object{
				makePod("w0", "node-a", v1.PodRunning, 3),
				makeClaim("dynamo", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Device: "gpu-6", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Device: "gpu-7", Driver: "gpu.nvidia.com", Pool: "node-a"},
				}),
				makePod("w1", "node-b", v1.PodRunning, 1),
			},
			want: map[string]int{"node-a": 5, "node-b": 1},
		},
		{
			// KEP-5004 (DRAExtendedResource): the pod's nvidia.com/gpu request
			// is satisfied by DRA via a scheduler-generated ResourceClaim, so
			// the same 4 devices appear as both the pod request and the
			// claim's allocation. Status.ExtendedResourceClaimStatus maps the
			// nvidia.com/gpu request to a request in the generated claim;
			// that request's allocation must be skipped — counted once.
			name: "KEP-5004 extended-resource claim not double-counted",
			objs: []runtime.Object{
				func() *v1.Pod {
					p := makePod("erc-pod", "node-a", v1.PodRunning, 4)
					p.Status.ExtendedResourceClaimStatus = &v1.PodExtendedResourceClaimStatus{
						ResourceClaimName: "erc-pod-gpu-claim",
						RequestMappings: []v1.ContainerExtendedResourceRequest{
							{ContainerName: "main", ResourceName: gpuResourceName, RequestName: "container-main-gpu"},
						},
					}
					return p
				}(),
				makeClaim("default", "erc-pod-gpu-claim", []resourcev1.DeviceRequestAllocationResult{
					{Request: "container-main-gpu", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Request: "container-main-gpu", Device: "gpu-1", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Request: "container-main-gpu", Device: "gpu-2", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Request: "container-main-gpu", Device: "gpu-3", Driver: "gpu.nvidia.com", Pool: "node-a"},
				}),
			},
			want: map[string]int{"node-a": 4},
		},
		{
			// A single generated claim can back MULTIPLE extended resources
			// (one RequestMapping per backed resource). Only the requests
			// mapped to nvidia.com/gpu are deduped; allocation results for the
			// other DRA-backed resource in the SAME claim must still count.
			name: "KEP-5004 mixed-resource claim — only gpu-mapped requests deduped",
			objs: []runtime.Object{
				func() *v1.Pod {
					p := makePod("mixed-pod", "node-a", v1.PodRunning, 2)
					p.Status.ExtendedResourceClaimStatus = &v1.PodExtendedResourceClaimStatus{
						ResourceClaimName: "mixed-pod-claim",
						RequestMappings: []v1.ContainerExtendedResourceRequest{
							{ContainerName: "main", ResourceName: gpuResourceName, RequestName: "container-main-gpu"},
							{ContainerName: "main", ResourceName: "nvidia.com/other", RequestName: "container-main-other"},
						},
					}
					return p
				}(),
				makeClaim("default", "mixed-pod-claim", []resourcev1.DeviceRequestAllocationResult{
					// nvidia.com/gpu-mapped request — already attributed via
					// the pod-request sum, must be skipped.
					{Request: "container-main-gpu", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Request: "container-main-gpu", Device: "gpu-1", Driver: "gpu.nvidia.com", Pool: "node-a"},
					// Other DRA-backed resource served by the same driver —
					// not covered by the pod's nvidia.com/gpu request, counts.
					{Request: "container-main-other", Device: "gpu-2", Driver: "gpu.nvidia.com", Pool: "node-a"},
				}),
			},
			want: map[string]int{"node-a": 3},
		},
		{
			// The KEP-5004 skip-set is keyed namespace/name: an unrelated claim
			// that merely shares the generated claim's name (and request name)
			// in another namespace must still be counted.
			name: "KEP-5004 skip is namespace-scoped",
			objs: []runtime.Object{
				func() *v1.Pod {
					p := makePod("erc-pod", "node-a", v1.PodRunning, 2)
					p.Status.ExtendedResourceClaimStatus = &v1.PodExtendedResourceClaimStatus{
						ResourceClaimName: "shared-claim-name",
						RequestMappings: []v1.ContainerExtendedResourceRequest{
							{ContainerName: "main", ResourceName: gpuResourceName, RequestName: "container-main-gpu"},
						},
					}
					return p
				}(),
				makeClaim("default", "shared-claim-name", []resourcev1.DeviceRequestAllocationResult{
					{Request: "container-main-gpu", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Request: "container-main-gpu", Device: "gpu-1", Driver: "gpu.nvidia.com", Pool: "node-a"},
				}),
				makeClaim("other", "shared-claim-name", []resourcev1.DeviceRequestAllocationResult{
					{Request: "container-main-gpu", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-b"},
				}),
			},
			want: map[string]int{"node-a": 2, "node-b": 1},
		},
		{
			// firstAvailable subrequest results carry Request in the
			// "<main request>/<subrequest>" form; the KEP-5004 mapping names
			// the main request, so the prefix must match for dedup.
			name: "KEP-5004 subrequest-form Request deduped by main-request prefix",
			objs: []runtime.Object{
				func() *v1.Pod {
					p := makePod("sub-pod", "node-a", v1.PodRunning, 1)
					p.Status.ExtendedResourceClaimStatus = &v1.PodExtendedResourceClaimStatus{
						ResourceClaimName: "sub-pod-claim",
						RequestMappings: []v1.ContainerExtendedResourceRequest{
							{ContainerName: "main", ResourceName: gpuResourceName, RequestName: "container-main-gpu"},
						},
					}
					return p
				}(),
				makeClaim("default", "sub-pod-claim", []resourcev1.DeviceRequestAllocationResult{
					{Request: "container-main-gpu/sub-0", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
				}),
			},
			want: map[string]int{"node-a": 1},
		},
		{
			// AdminAccess allocations (DRAAdminAccess) grant monitoring or
			// administrative access to devices that stay allocatable to real
			// workloads — they hold no capacity and must not count as
			// occupancy, even when they cover every GPU on the node.
			name: "admin-access allocations are not occupancy",
			objs: []runtime.Object{
				makeClaim("monitoring", "admin-claim", []resourcev1.DeviceRequestAllocationResult{
					{Request: "all-gpus", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a", AdminAccess: ptr.To(true)},
					{Request: "all-gpus", Device: "gpu-1", Driver: "gpu.nvidia.com", Pool: "node-a", AdminAccess: ptr.To(true)},
					{Request: "all-gpus", Device: "gpu-2", Driver: "gpu.nvidia.com", Pool: "node-a", AdminAccess: ptr.To(true)},
					{Request: "all-gpus", Device: "gpu-3", Driver: "gpu.nvidia.com", Pool: "node-a", AdminAccess: ptr.To(true)},
				}),
			},
			want: map[string]int{},
		},
		{
			// AdminAccess=false (explicitly set) is a real allocation.
			name: "explicit AdminAccess=false still counts",
			objs: []runtime.Object{
				makeClaim("ns", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Request: "r0", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a", AdminAccess: ptr.To(false)},
				}),
			},
			want: map[string]int{"node-a": 1},
		},
		{
			name: "no pods or claims at all",
			objs: nil,
			want: map[string]int{},
		},
		{
			// Rejected state (#1652): a gpu.nvidia.com allocation from a pool
			// no node-local slice publishes cannot be attributed to a node.
			// Occupancy attribution would be a guess — fail fast instead.
			name: "unknown-pool allocation fails fast",
			objs: []runtime.Object{
				makeClaim("dynamo", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "rack-7-shared"},
				}),
			},
			wantErr: `pool "rack-7-shared", which no node-local gpu.nvidia.com ResourceSlice publishes`,
		},
		{
			// The K8s API does not require pool names to be node names: a
			// pool named "node-b" can be published by node-a's slices. The
			// slice-derived map must win over name equality — attributing by
			// pool name would charge the wrong (existing!) node.
			name: "pool named after a DIFFERENT node attributes via the slice map",
			objs: []runtime.Object{
				makeClaim("dynamo", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-b"},
					{Device: "gpu-1", Driver: "gpu.nvidia.com", Pool: "node-b"},
				}),
			},
			poolNodes: map[string]string{"node-b": "node-a"},
			want:      map[string]int{"node-a": 2},
		},
		{
			// Rejected state (#1652): kai classifies claim demand by the
			// REQUEST's DeviceClass name (lowercase-contains "gpu"), so an
			// allocated claim for a foreign "gpu"-named class consumes kai's
			// GPU capacity even when its allocation's DRIVER carries no
			// "gpu" substring (which the result loop would skip) — fail fast.
			name: "allocated claim for a foreign gpu-named DeviceClass fails fast",
			objs: []runtime.Object{
				withClaimClasses(makeClaim("dynamo", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Request: "r0", Device: "acc-0", Driver: "accelerator.vendor.example", Pool: "shelf-1"},
				}), "gpu.vendor.example"),
			},
			wantErr: `requests DeviceClass "gpu.vendor.example"`,
		},
		{
			// Supported classes pass the screen: gpu.nvidia.com counts
			// normally and a ComputeDomain claim (no "gpu" in the class) is
			// ignored by both the class screen and the driver filter.
			name: "nvidia and compute-domain class claims unaffected by the class screen",
			objs: []runtime.Object{
				withClaimClasses(makeClaim("dynamo", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Request: "r0", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
				}), "gpu.nvidia.com"),
				withClaimClasses(makeClaim("dynamo", "cd", []resourcev1.DeviceRequestAllocationResult{
					{Request: "r0", Device: "ch-0", Driver: "compute-domain.nvidia.com", Pool: "cd-pool"},
				}), "compute-domain.nvidia.com"),
			},
			want: map[string]int{"node-a": 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			poolNodes := tt.poolNodes
			if poolNodes == nil {
				poolNodes = defaultPoolNodes
			}
			client := fake.NewClientset(tt.objs...)
			got, err := countUsedGPUsByNodeMerged(context.Background(), client, poolNodes)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("countUsedGPUsByNode() = %v, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("countUsedGPUsByNode() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("countUsedGPUsByNode() unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("countUsedGPUsByNode() size = %d (%v), want %d (%v)",
					len(got), got, len(tt.want), tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("countUsedGPUsByNode()[%q] = %d, want %d", k, got[k], v)
				}
			}
		})
	}

	t.Run("pod list failure fails closed", func(t *testing.T) {
		client := fake.NewClientset()
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stderrors.New("pods list denied")
		})
		got, err := countUsedGPUsByNodeMerged(context.Background(), client, defaultPoolNodes)
		if err == nil {
			t.Fatalf("countUsedGPUsByNode() = %v, want error — a list failure must not report GPUs as free", got)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
			t.Errorf("error code = %v, want ErrCodeInternal", err)
		}
	})

	t.Run("DRA claim list failure fails closed", func(t *testing.T) {
		client := fake.NewClientset(makePod("w0", "node-a", v1.PodRunning, 2))
		client.PrependReactor("list", "resourceclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceclaims"}, "", stderrors.New("RBAC denied"))
		})
		got, err := countUsedGPUsByNodeMerged(context.Background(), client, defaultPoolNodes)
		if err == nil {
			t.Fatalf("countUsedGPUsByNode() = %v, want error — an ambiguous DRA lookup must not be treated as zero usage", got)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
			t.Errorf("error code = %v, want ErrCodeInternal", err)
		}
	})

	// Context cancellation / deadline exhaustion during either List is a
	// deadline condition, not an infrastructure fault: both paths must wrap
	// as ErrCodeTimeout, not ErrCodeInternal. The fake clientset does not
	// honor the request context, so each reactor surfaces ctx.Err() the way
	// a real client would.
	t.Run("canceled or deadline-exceeded context maps to ErrCodeTimeout", func(t *testing.T) {
		canceledCtx := func(t *testing.T) context.Context {
			t.Helper()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}
		expiredCtx := func(t *testing.T) context.Context {
			t.Helper()
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
			t.Cleanup(cancel)
			return ctx
		}
		cases := []struct {
			name     string
			resource string
			makeCtx  func(*testing.T) context.Context
		}{
			{"pod list with canceled context", "pods", canceledCtx},
			{"pod list with exceeded deadline", "pods", expiredCtx},
			{"claim list with canceled context", "resourceclaims", canceledCtx},
			{"claim list with exceeded deadline", "resourceclaims", expiredCtx},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ctx := tc.makeCtx(t)
				client := fake.NewClientset()
				client.PrependReactor("list", tc.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, ctx.Err()
				})
				got, err := countUsedGPUsByNodeMerged(ctx, client, defaultPoolNodes)
				if err == nil {
					t.Fatalf("countUsedGPUsByNode() = %v, want error — a canceled scan must not report GPUs as free", got)
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
					t.Errorf("error code = %v, want ErrCodeTimeout for %v", err, ctx.Err())
				}
			})
		}
	})

	t.Run("DRA API not served — device-plugin counts still returned", func(t *testing.T) {
		client := fake.NewClientset(makePod("w0", "node-a", v1.PodRunning, 2))
		client.PrependReactor("list", "resourceclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceclaims"}, "")
		})
		got, err := countUsedGPUsByNodeMerged(context.Background(), client, defaultPoolNodes)
		if err != nil {
			t.Fatalf("countUsedGPUsByNode() unexpected error when DRA API is absent: %v", err)
		}
		if got["node-a"] != 2 {
			t.Errorf("countUsedGPUsByNode()[node-a] = %d, want 2 (device-plugin usage must survive missing DRA API)", got["node-a"])
		}
	})

	// A cancellation landing AFTER List returns must still abort the scan —
	// the normalization loop checks ctx per claim, so a canceled occupancy
	// scan cannot masquerade as a successful (possibly empty) result.
	t.Run("cancellation during claim normalization maps to ErrCodeTimeout", func(t *testing.T) {
		cctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		client := fake.NewClientset(makeClaim("ns", "c1", []resourcev1.DeviceRequestAllocationResult{
			{Request: "r0", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
		}))
		client.PrependReactor("list", "resourceclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			cancel() // cancel between the List returning and normalization
			return false, nil, nil
		})
		got, err := countUsedGPUsByNodeMerged(cctx, client, defaultPoolNodes)
		if err == nil {
			t.Fatalf("countUsedGPUsByNode() = %v, want error — a canceled scan must not report success", got)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error code = %v, want ErrCodeTimeout", err)
		}
	})

	// An empty claim list must not bypass the cancellation check: with no
	// items the per-item checks never run, so the post-List check has to
	// catch a cancellation that landed after List returned.
	t.Run("cancellation with an empty claim list still maps to ErrCodeTimeout", func(t *testing.T) {
		cctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		client := fake.NewClientset() // no claims at all
		client.PrependReactor("list", "resourceclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			cancel() // cancel between the (empty) List returning and normalization
			return false, nil, nil
		})
		got, err := countUsedGPUsByNodeMerged(cctx, client, defaultPoolNodes)
		if err == nil {
			t.Fatalf("countUsedGPUsByNode() = %v, want error — a canceled scan with an empty List must not report success", got)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error code = %v, want ErrCodeTimeout", err)
		}
	})

	// Beta-only clusters (K8s 1.32/1.33) serve resource.k8s.io at v1beta2 or
	// v1beta1 only. Their DRA allocations are just as real — the claim list
	// must fall back through the served versions instead of flattening a v1
	// NotFound into "no DRA allocations" and over-admitting.
	t.Run("v1beta2-only cluster — beta claims still counted", func(t *testing.T) {
		client := fake.NewClientset(
			makePod("w0", "node-a", v1.PodRunning, 1),
			&resourcev1beta2.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "dynamo"},
				Status: resourcev1beta2.ResourceClaimStatus{
					Allocation: &resourcev1beta2.AllocationResult{
						Devices: resourcev1beta2.DeviceAllocationResult{
							Results: []resourcev1beta2.DeviceRequestAllocationResult{
								{Request: "r0", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
								{Request: "r0", Device: "gpu-1", Driver: "gpu.nvidia.com", Pool: "node-b"},
							},
						},
					},
				},
			},
		)
		client.PrependReactor("list", "resourceclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.GetResource().Version == "v1" {
				return true, nil, apierrors.NewNotFound(
					schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceclaims"}, "")
			}
			return false, nil, nil // fall through to the tracker at beta versions
		})
		got, err := countUsedGPUsByNodeMerged(context.Background(), client, defaultPoolNodes)
		if err != nil {
			t.Fatalf("countUsedGPUsByNode() unexpected error on a v1beta2-only cluster: %v", err)
		}
		if got["node-a"] != 2 || got["node-b"] != 1 {
			t.Errorf("countUsedGPUsByNode() = %v, want node-a:2 (1 pod + 1 DRA) node-b:1 — beta claims must count", got)
		}
	})

	t.Run("v1beta1-only cluster — beta claims still counted", func(t *testing.T) {
		client := fake.NewClientset(
			&resourcev1beta1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "dynamo"},
				Status: resourcev1beta1.ResourceClaimStatus{
					Allocation: &resourcev1beta1.AllocationResult{
						Devices: resourcev1beta1.DeviceAllocationResult{
							Results: []resourcev1beta1.DeviceRequestAllocationResult{
								{Request: "r0", Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
							},
						},
					},
				},
			},
		)
		client.PrependReactor("list", "resourceclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if v := action.GetResource().Version; v == "v1" || v == "v1beta2" {
				return true, nil, apierrors.NewNotFound(
					schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceclaims"}, "")
			}
			return false, nil, nil // fall through to the tracker at v1beta1
		})
		got, err := countUsedGPUsByNodeMerged(context.Background(), client, defaultPoolNodes)
		if err != nil {
			t.Fatalf("countUsedGPUsByNode() unexpected error on a v1beta1-only cluster: %v", err)
		}
		if got["node-a"] != 1 {
			t.Errorf("countUsedGPUsByNode()[node-a] = %d, want 1 — v1beta1 claims must count", got["node-a"])
		}
	})
}

func TestPodEffectiveGPURequest(t *testing.T) {
	gpu := v1.ResourceName(gpuResourceName)
	qty := func(n int64) resource.Quantity { return *resource.NewQuantity(n, resource.DecimalSI) }
	always := v1.ContainerRestartPolicyAlways

	tests := []struct {
		name string
		pod  *v1.Pod
		want int
	}{
		{
			name: "regular containers sum",
			pod: &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{
				{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(2)}}},
				{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(1)}}},
			}}},
			want: 3,
		},
		{
			name: "requests absent — falls back to limits",
			pod: &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{
				{Resources: v1.ResourceRequirements{Limits: v1.ResourceList{gpu: qty(4)}}},
			}}},
			want: 4,
		},
		{
			name: "one-shot init container floors the effective request",
			pod: &v1.Pod{Spec: v1.PodSpec{
				InitContainers: []v1.Container{
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(8)}}},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(2)}}},
				},
			}},
			want: 8,
		},
		{
			name: "sidecar init container adds to the sum",
			pod: &v1.Pod{Spec: v1.PodSpec{
				InitContainers: []v1.Container{
					{RestartPolicy: &always, Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(1)}}},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(2)}}},
				},
			}},
			want: 3,
		},
		{
			// The sidecar (1) is already running when the one-shot init (4)
			// runs, so the init phase demands 1+4=5, above the steady state
			// of 1 (sidecar) + 2 (regular) = 3. Comparing the one-shot init
			// alone (4) would undercount.
			name: "one-shot init after sidecar stacks on the sidecar's request",
			pod: &v1.Pod{Spec: v1.PodSpec{
				InitContainers: []v1.Container{
					{RestartPolicy: &always, Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(1)}}},
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(4)}}},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(2)}}},
				},
			}},
			want: 5,
		},
		{
			// Order matters: the one-shot init (4) completes before the
			// sidecar (1) starts, so they never overlap — the init phase
			// peaks at 4, the steady state at 1+2=3.
			name: "sidecar after one-shot init does not stack",
			pod: &v1.Pod{Spec: v1.PodSpec{
				InitContainers: []v1.Container{
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(4)}}},
					{RestartPolicy: &always, Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(1)}}},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(2)}}},
				},
			}},
			want: 4,
		},
		{
			// Multiple sidecars accumulate: both (1+2=3) are running when the
			// one-shot init (4) runs → init peak 7; steady state 3+2=5.
			name: "multiple sidecars accumulate under a later one-shot init",
			pod: &v1.Pod{Spec: v1.PodSpec{
				InitContainers: []v1.Container{
					{RestartPolicy: &always, Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(1)}}},
					{RestartPolicy: &always, Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(2)}}},
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(4)}}},
				},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(2)}}},
				},
			}},
			want: 7,
		},
		{
			name: "no GPU resources — zero",
			pod: &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{
				{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{"cpu": qty(4)}}},
			}}},
			want: 0,
		},
		{
			// Kubernetes adds pod spec.overhead (RuntimeClass) AFTER
			// max(steadyState, initMax), so it always increases the demand.
			name: "RuntimeClass overhead added on top of container requests",
			pod: &v1.Pod{Spec: v1.PodSpec{
				Overhead: v1.ResourceList{gpu: qty(1)},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{gpu: qty(1)}}},
				},
			}},
			want: 2,
		},
		{
			name: "overhead-only pod counts the overhead",
			pod: &v1.Pod{Spec: v1.PodSpec{
				Overhead: v1.ResourceList{gpu: qty(1)},
				Containers: []v1.Container{
					{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{"cpu": qty(4)}}},
				},
			}},
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podEffectiveGPURequest(tt.pod); got != tt.want {
				t.Errorf("podEffectiveGPURequest() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestSelectWorkerNode pins the capacity-first (node, mechanism) pair
// scoring: the pair with the most FREE GPUs wins (documented contract), ties
// prefer DRA then lexicographic node name, DRA capacity comes from
// Mode.DRANodeDevices (never scalar allocatable), and eligibility is
// TOCTOU-guarded by the fresh node Ready condition plus the probe's sets.
func TestSelectWorkerNode(t *testing.T) {
	node := func(name string, scalar string, ready bool) v1.Node {
		status := v1.ConditionTrue
		if !ready {
			status = v1.ConditionFalse
		}
		return v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{"nvidia.com/gpu": resource.MustParse(scalar)},
				Conditions:  []v1.NodeCondition{{Type: v1.NodeReady, Status: status}},
			},
		}
	}
	draNode := node("dra-node", "8", true)
	pluginNode := node("plugin-node", "8", true)
	pluginNodeB := node("plugin-node-b", "8", true)
	notReadyPlugin := node("notready-plugin", "8", false)
	dualNode := node("dual-node", "4", true) // scalar says 4, DRA-usable says 2
	dualMode := &allocmode.Mode{
		DRAUsable: true, DevicePluginUsable: true, APIVersion: "v1",
		DRANodes:          []string{"dra-node", "dual-node"},
		DRANodeDevices:    map[string]int{"dra-node": 8, "dual-node": 2},
		DevicePluginNodes: []string{"dra-node", "dual-node", "plugin-node", "plugin-node-b", "notready-plugin"},
		// Dual-advertised nodes carry node-local slices kai counts raw.
		NodeLocalGPUSliceDevices: map[string]int{"dra-node": 8, "dual-node": 2},
	}

	tests := []struct {
		name       string
		candidates []v1.Node
		mode       *allocmode.Mode
		used       map[string]int // DRA-ledger occupancy (draUsed)
		scalarUsed map[string]int // device-plugin-ledger occupancy
		wantOK     bool
		wantNode   string
		wantDRA    bool
		wantAlloc  int
		wantFree   int
	}{
		{
			// Cross-ledger regression (#1620 review): the device plugin's
			// physical device assignments are invisible to the DRA
			// allocator, so a DRA candidate carrying ANY scalar occupancy
			// must be skipped — never sized "around" the scalar count.
			name:       "DRA candidate with scalar GPU occupancy is skipped; clean plugin node wins",
			candidates: []v1.Node{draNode, pluginNode},
			mode:       dualMode,
			scalarUsed: map[string]int{"dra-node": 1}, // 8 DRA devices, 1 plugin-held GPU
			wantOK:     true, wantNode: "plugin-node", wantDRA: false, wantAlloc: 8, wantFree: 8,
		},
		{
			name:       "DRA candidate with scalar GPU occupancy and no clean alternative: not eligible",
			candidates: []v1.Node{draNode},
			mode:       dualMode,
			scalarUsed: map[string]int{"dra-node": 1},
			wantOK:     false,
		},
		{
			name:       "DRA candidate subtracts same-ledger DRA occupancy normally",
			candidates: []v1.Node{draNode},
			mode:       dualMode,
			used:       map[string]int{"dra-node": 3}, // DRA allocations only
			wantOK:     true, wantNode: "dra-node", wantDRA: true, wantAlloc: 8, wantFree: 5,
		},
		{
			name:       "idle plugin node beats nearly-saturated DRA node: plugin wiring (most free wins)",
			candidates: []v1.Node{draNode, pluginNode},
			mode:       dualMode,
			used:       map[string]int{"dra-node": 7}, // 1 free DRA vs 8 free plugin
			wantOK:     true, wantNode: "plugin-node", wantDRA: false, wantAlloc: 8, wantFree: 8,
		},
		{
			name:       "equal free capacity: DRA wins the tie",
			candidates: []v1.Node{pluginNode, draNode}, // both 8 free
			mode:       dualMode,
			wantOK:     true, wantNode: "dra-node", wantDRA: true, wantAlloc: 8, wantFree: 8,
		},
		{
			name:       "same-mechanism tie: lexicographic node name",
			candidates: []v1.Node{pluginNodeB, pluginNode}, // both plugin, both 8 free
			mode: &allocmode.Mode{DevicePluginUsable: true,
				DevicePluginNodes: []string{"plugin-node", "plugin-node-b"}},
			wantOK: true, wantNode: "plugin-node", wantDRA: false, wantAlloc: 8, wantFree: 8,
		},
		{
			name:       "--node-selector forced plugin-only node while DRA exists elsewhere: plugin wiring",
			candidates: []v1.Node{pluginNode}, // post-selector-filter pool
			mode:       dualMode,
			wantOK:     true, wantNode: "plugin-node", wantDRA: false, wantAlloc: 8, wantFree: 8,
		},
		{
			name:       "dual node sizes from usable DRA devices, not scalar (no oversizing)",
			candidates: []v1.Node{dualNode},
			mode:       dualMode,
			wantOK:     true, wantNode: "dual-node", wantDRA: true, wantAlloc: 2, wantFree: 2,
		},
		{
			name:       "TOCTOU: probe says Ready but fresh node object is NotReady — excluded",
			candidates: []v1.Node{notReadyPlugin}, // in DevicePluginNodes, currently NotReady
			mode:       dualMode,
			wantOK:     false,
		},
		{
			name:       "nil mode: plugin wiring from fresh Ready nodes",
			candidates: []v1.Node{notReadyPlugin, pluginNode},
			mode:       nil,
			wantOK:     true, wantNode: "plugin-node", wantDRA: false, wantAlloc: 8, wantFree: 8,
		},
		{
			name:       "probe ran but candidate absent from its sets: excluded",
			candidates: []v1.Node{node("unknown-node", "8", true)},
			mode:       dualMode,
			wantOK:     false,
		},
		{
			name:       "stale over-allocation clamps free at 0, never negative",
			candidates: []v1.Node{dualNode},
			mode:       dualMode,
			used:       map[string]int{"dual-node": 5}, // > DRA capacity 2
			wantOK:     true, wantNode: "dual-node", wantDRA: true, wantAlloc: 2, wantFree: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			used := tt.used
			if used == nil {
				used = map[string]int{}
			}
			scalarUsed := tt.scalarUsed
			if scalarUsed == nil {
				scalarUsed = map[string]int{}
			}
			chosen, draWiring, alloc, free, ok := selectWorkerNode(tt.candidates, tt.mode, validatorv1.GPUAllocationPolicyUnspecified, scalarUsed, used)
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if chosen.Name != tt.wantNode {
				t.Errorf("chosen = %q, want %q", chosen.Name, tt.wantNode)
			}
			if draWiring != tt.wantDRA {
				t.Errorf("draWiring = %t, want %t", draWiring, tt.wantDRA)
			}
			if alloc != tt.wantAlloc || free != tt.wantFree {
				t.Errorf("alloc/free = %d/%d, want %d/%d", alloc, free, tt.wantAlloc, tt.wantFree)
			}
		})
	}
}

func TestNodeGPUCount(t *testing.T) {
	tests := []struct {
		name string
		node v1.Node
		want int
	}{
		{
			name: "8 GPUs",
			node: v1.Node{Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
			}},
			want: 8,
		},
		{
			name: "1 GPU",
			node: v1.Node{Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
			}},
			want: 1,
		},
		{
			name: "no GPU resource",
			node: v1.Node{Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{"cpu": resource.MustParse("16")},
			}},
			want: 0,
		},
		{
			name: "empty allocatable",
			node: v1.Node{},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeGPUCount(tt.node); got != tt.want {
				t.Errorf("nodeGPUCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestScaledThroughputThreshold(t *testing.T) {
	tests := []struct {
		name            string
		threshold       float64
		gpuCount        int
		gpuCountPerNode int
		want            float64
	}{
		{"full node is a no-op", 50000, 8, 8, 50000},
		{"half node scales by half", 50000, 4, 8, 25000},
		{"two of eight GPUs", 50000, 2, 8, 12500},
		{"unknown node count unchanged", 50000, 2, 0, 50000},
		{"zero gpuCount unchanged", 50000, 0, 8, 50000},
		{"over-count clamps to no-op", 50000, 9, 8, 50000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scaledThroughputThreshold(tt.threshold, tt.gpuCount, tt.gpuCountPerNode)
			if got != tt.want {
				t.Errorf("scaledThroughputThreshold(%v, %d, %d) = %v, want %v",
					tt.threshold, tt.gpuCount, tt.gpuCountPerNode, got, tt.want)
			}
		})
	}
}

func TestRequireComparatorPrefix(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		want      string // required leading prefix
		wantError bool
	}{
		// Throughput must use `>=` — every other form is rejected because
		// parseThreshold would strip it and the evaluator would silently
		// coerce it to `>= threshold*0.9`, reinterpreting the written meaning.
		{"throughput: >= 5000 accepted", ">= 5000", ">=", false},
		{"throughput: >= 5000 tok/s accepted with units", ">= 5000 tok/s", ">=", false},
		{"throughput: > 5000 rejected (strict-greater reinterpreted)", "> 5000", ">=", true},
		{"throughput: == 5000 rejected (equality reinterpreted)", "== 5000", ">=", true},
		{"throughput: != 5000 rejected (not-equal reinterpreted)", "!= 5000", ">=", true},
		{"throughput: bare 5000 rejected (implicit exact reinterpreted)", "5000", ">=", true},
		{"throughput: <= 5000 rejected (inverted)", "<= 5000", ">=", true},
		{"throughput: < 5000 rejected (inverted strict)", "< 5000", ">=", true},

		// TTFT must use `<=` — same rule as throughput with opposite direction.
		{"ttft: <= 200 accepted", "<= 200", "<=", false},
		{"ttft: <= 200 ms accepted with units", "<= 200 ms", "<=", false},
		{"ttft: < 200 rejected (strict-less reinterpreted)", "< 200", "<=", true},
		{"ttft: == 200 rejected (equality reinterpreted)", "== 200", "<=", true},
		{"ttft: bare 200 rejected", "200", "<=", true},
		{"ttft: >= 200 rejected (inverted)", ">= 200", "<=", true},
		{"ttft: > 200 rejected (inverted strict)", "> 200", "<=", true},

		// Whitespace handling.
		{"throughput: leading whitespace tolerated (accepted)", "  >= 5000", ">=", false},
		{"throughput: leading whitespace tolerated (rejected)", "  > 5000", ">=", true},

		// Malformed operator continuations — HasPrefix alone would accept
		// these; the boundary check must reject so parseThreshold's blanket
		// strip of `><=! ` (includes space) doesn't silently coerce them.
		{"throughput: >== 5000 rejected (extra = after >=)", ">== 5000", ">=", true},
		{"throughput: >=! 5000 rejected (extra ! after >=)", ">=! 5000", ">=", true},
		{"throughput: >=< 5000 rejected (mixed operator chars)", ">=< 5000", ">=", true},
		{"ttft: <== 200 rejected (extra = after <=)", "<== 200", "<=", true},
		{"ttft: <=> 200 rejected (mixed operator chars)", "<=> 200", "<=", true},

		// Space-separated continuations — parseThreshold also strips spaces
		// from the leading run, so `>= =5000` silently parses to 5000.
		{"throughput: >= =5000 rejected (space-separated extra =)", ">= =5000", ">=", true},
		{"throughput: >=  >5000 rejected (space-separated extra >)", ">=  >5000", ">=", true},
		{"ttft: <=   !200 rejected (space-separated extra !)", "<=   !200", "<=", true},
		{"ttft: <= <200 rejected (space-separated extra <)", "<= <200", "<=", true},

		// Boundary corner cases that should still be accepted.
		{"throughput: >=5000 (no space) accepted", ">=5000", ">=", false},
		{"throughput: >=. accepted (digit-ish)", ">=.5", ">=", false},
		{"ttft: <=200.5 (decimal) accepted", "<=200.5", "<=", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireComparatorPrefix(tt.value, tt.want, "test-metric")
			if (err != nil) != tt.wantError {
				t.Errorf("requireComparatorPrefix(%q, %q) error = %v, wantError = %v",
					tt.value, tt.want, err, tt.wantError)
			}
		})
	}
}

// TestWaitForEndpointReady_AcceptsOnFirstRealCompletion covers the warmup race
// the function exists to handle: Dynamo's frontend responds 200 to /health
// before backend workers register, so a /health-only probe lets AIPerf launch
// against an endpoint that completes requests with zero tokens. The probe must
// only accept once /v1/chat/completions returns a non-empty completion — every
// other shape (404, 503, 200-empty-content, 200-but-no-choices) must be
// retried, not treated as ready.
func TestWaitForEndpointReady_AcceptsOnFirstRealCompletion(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("probe method = %q, want %q", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("probe hit %q, expected /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1: // backend not registered yet
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2: // accepted but no completion produced (the failure mode we're guarding against)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
		case 3: // worker connected, real completion
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
		default:
			t.Errorf("unexpected extra probe call %d after success", n)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
		}
	}))
	defer srv.Close()

	// Bound the success-path probe so a regression that breaks the accept
	// condition fails the test in milliseconds rather than blocking up to
	// InferenceHealthTimeout (5 m). 250 ms is comfortable headroom over the
	// 3-call/1 ms expected critical path while still tight enough to surface
	// a stuck loop.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := waitForEndpointReadyWithInterval(ctx, srv.URL, "test-model", time.Millisecond, defaults.InferenceHealthTimeout); err != nil {
		t.Fatalf("waitForEndpointReady returned error: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("probe call count = %d, want 3 (503 → empty 200 → real 200)", got)
	}
}

// TestWaitForEndpointReady_TimesOutWhenAlwaysEmpty ensures the probe doesn't
// silently treat persistent "200 with empty completion" as ready — that's the
// exact failure mode (frontend up, workers absent) the function exists to
// detect. Use a tiny ctx deadline so the test stays fast.
func TestWaitForEndpointReady_TimesOutWhenAlwaysEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := waitForEndpointReadyWithInterval(ctx, srv.URL, "test-model", time.Millisecond, defaults.InferenceHealthTimeout)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error code = %v, want ErrCodeTimeout (err=%v)", err, err)
	}
}

// TestApplyWorkerClaimTemplate_VersionDispatch pins the served-version
// handling of the DRA worker ResourceClaimTemplate: the object is created at
// the allocmode probe's detected resource.k8s.io version, with the
// version-appropriate request shape — `exactly:`-wrapped for v1/v1beta2,
// inline fields for v1beta1 (whose DeviceRequest has no `exactly` wrapper).
func TestApplyWorkerClaimTemplate_VersionDispatch(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		wantExactly bool
	}{
		{name: "v1 uses the exactly wrapper", version: "v1", wantExactly: true},
		{name: "v1beta2 uses the exactly wrapper", version: "v1beta2", wantExactly: true},
		{name: "v1beta1 uses inline request fields", version: "v1beta1", wantExactly: false},
		{name: "empty version defaults to v1", version: "", wantExactly: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			effective := tt.version
			if effective == "" {
				effective = "v1"
			}
			scheme := runtime.NewScheme()
			dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
				map[schema.GroupVersionResource]string{
					allocmode.GVRAt(effective, "resourceclaimtemplates"): "ResourceClaimTemplateList",
				})
			ctx := &validators.Context{Ctx: context.Background(), DynamicClient: dynClient}
			config := &inferenceWorkloadConfig{
				namespace:    "test-ns",
				gpuAllocMode: &allocmode.Mode{DRAUsable: true, APIVersion: tt.version},
			}

			if err := applyWorkerClaimTemplate(ctx, config, map[string]string{
				"NAMESPACE":           config.namespace,
				"CLAIM_TEMPLATE_NAME": inferenceClaimTemplateName,
			}); err != nil {
				t.Fatalf("applyWorkerClaimTemplate() error = %v", err)
			}

			created, err := dynClient.Resource(allocmode.GVRAt(effective, "resourceclaimtemplates")).
				Namespace("test-ns").Get(context.Background(), inferenceClaimTemplateName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("template not created at resource.k8s.io/%s: %v", effective, err)
			}
			if got := created.GetAPIVersion(); got != "resource.k8s.io/"+effective {
				t.Errorf("apiVersion = %q, want resource.k8s.io/%s", got, effective)
			}
			requests, found, _ := unstructured.NestedSlice(created.Object, "spec", "spec", "devices", "requests")
			if !found || len(requests) != 1 {
				t.Fatalf("spec.spec.devices.requests = %v (found=%t), want exactly one request", requests, found)
			}
			req := requests[0].(map[string]interface{})
			_, hasExactly := req["exactly"]
			if hasExactly != tt.wantExactly {
				t.Errorf("request has exactly wrapper = %t, want %t (version %s)", hasExactly, tt.wantExactly, effective)
			}
			fields := req
			if tt.wantExactly {
				fields = req["exactly"].(map[string]interface{})
			}
			if fields["deviceClassName"] != "gpu.nvidia.com" {
				t.Errorf("deviceClassName = %v, want gpu.nvidia.com", fields["deviceClassName"])
			}
		})
	}
}

// TestSelectWorkerNode_KaiCompatibilityGuard pins the kai raw-slice guard: a
// node bearing raw node-local gpu.nvidia.com ResourceSlices that AICR could
// NOT validate (not in DRANodes) is ineligible for device-plugin wiring —
// kai-scheduler would reject scalar GPU pods there — and when that leaves no
// eligible pair, the fail-fast message names the offending node.
// (Non-node-local gpu.nvidia.com topologies never reach selection — they are
// rejected earlier by rejectUnsupportedGPUTopology; see #1652.)
func TestSelectWorkerNode_KaiCompatibilityGuard(t *testing.T) {
	node := func(name string) v1.Node {
		return v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
				Conditions:  []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			},
		}
	}
	badSliceNode := node("bad-slice-node")

	t.Run("unvalidatable raw slices block plugin wiring entirely + fail-fast names the node", func(t *testing.T) {
		mode := &allocmode.Mode{
			DevicePluginUsable: true,
			DevicePluginNodes:  []string{"bad-slice-node"},
			// NOT in DRANodes: AICR could not validate the slices...
			DRANodes: nil, DRANodeDevices: map[string]int{},
			// ...but kai counts them raw.
			NodeLocalGPUSliceDevices: map[string]int{"bad-slice-node": 4},
		}
		_, _, _, _, ok := selectWorkerNode([]v1.Node{badSliceNode}, mode, validatorv1.GPUAllocationPolicyUnspecified, map[string]int{}, map[string]int{})
		if ok {
			t.Fatal("ok = true, want false — kai would reject scalar pods on the raw-slice node")
		}
		msg := describeNoEligibleWorkerNode([]v1.Node{badSliceNode}, mode, validatorv1.GPUAllocationPolicyUnspecified, nil)
		for _, want := range []string{"bad-slice-node", "4 raw device(s)", "kai-scheduler counts but AICR cannot validate", "fix the ResourceSlices or free a different node"} {
			if !strings.Contains(msg, want) {
				t.Errorf("fail-fast message missing %q:\n%s", want, msg)
			}
		}
	})

	t.Run("DRA-capable raw-slice node stays DRA-eligible (guard only blocks plugin wiring)", func(t *testing.T) {
		mode := &allocmode.Mode{
			DRAUsable: true, DevicePluginUsable: true,
			DRANodes:                 []string{"bad-slice-node"},
			DRANodeDevices:           map[string]int{"bad-slice-node": 4},
			DevicePluginNodes:        []string{"bad-slice-node"},
			NodeLocalGPUSliceDevices: map[string]int{"bad-slice-node": 4},
		}
		chosen, draWiring, _, free, ok := selectWorkerNode([]v1.Node{badSliceNode}, mode, validatorv1.GPUAllocationPolicyUnspecified, map[string]int{}, map[string]int{})
		if !ok || !draWiring || chosen.Name != "bad-slice-node" || free != 4 {
			t.Errorf("got (node=%s dra=%t free=%d ok=%t), want DRA wiring with 4 free", chosen.Name, draWiring, free, ok)
		}
	})

	t.Run("scalar-occupied DRA node names the cross-ledger cause in the fail-fast message", func(t *testing.T) {
		mode := &allocmode.Mode{
			DRAUsable: true, DevicePluginUsable: true,
			DRANodes:                 []string{"bad-slice-node"},
			DRANodeDevices:           map[string]int{"bad-slice-node": 8},
			DevicePluginNodes:        []string{"bad-slice-node"},
			NodeLocalGPUSliceDevices: map[string]int{"bad-slice-node": 8},
		}
		scalarUsed := map[string]int{"bad-slice-node": 1}
		_, _, _, _, ok := selectWorkerNode([]v1.Node{badSliceNode}, mode, validatorv1.GPUAllocationPolicyUnspecified, scalarUsed, map[string]int{})
		if ok {
			t.Fatal("ok = true, want false — a DRA candidate with scalar occupancy must be skipped")
		}
		msg := describeNoEligibleWorkerNode([]v1.Node{badSliceNode}, mode, validatorv1.GPUAllocationPolicyUnspecified, scalarUsed)
		for _, want := range []string{"bad-slice-node", "1 scalar GPU(s) in use", "DRA allocator cannot see or avoid"} {
			if !strings.Contains(msg, want) {
				t.Errorf("fail-fast message missing %q:\n%s", want, msg)
			}
		}
	})
}

// TestRejectUnsupportedGPUTopology pins the fail-fast gate for GPU DRA
// configurations outside AICR's supported matrix (#1327): foreign gpu-named
// DRA drivers, non-node-local gpu.nvidia.com slice topologies, and pools
// published by multiple nodes are rejected with ErrCodeInvalidRequest and
// actionable messages citing #1652, before any workload is built. The two
// supported states pass through untouched.
func TestRejectUnsupportedGPUTopology(t *testing.T) {
	tests := []struct {
		name string
		mode *allocmode.Mode
		// wantErr lists substrings the rejection message must contain;
		// empty means the configuration is accepted.
		wantErr []string
	}{
		{
			name: "nil mode (probe unavailable) is accepted",
			mode: nil,
		},
		{
			name: "supported: node-local NVIDIA slices only",
			mode: &allocmode.Mode{
				DRAUsable:                true,
				DRANodes:                 []string{"node-a"},
				DRANodeDevices:           map[string]int{"node-a": 8},
				NodeLocalGPUSliceDevices: map[string]int{"node-a": 8},
			},
		},
		{
			name: "supported: ComputeDomain-only / no full-GPU slices",
			mode: &allocmode.Mode{DevicePluginUsable: true, DevicePluginNodes: []string{"node-a"}},
		},
		{
			name: "rejected: foreign gpu-named DRA driver",
			mode: &allocmode.Mode{
				DevicePluginUsable:     true,
				ForeignGPUSliceDrivers: []string{"gpu.amd.com"},
			},
			wantErr: []string{"gpu.amd.com", "non-NVIDIA GPU driver", "#1327", "#1652"},
		},
		{
			name: "rejected: non-node-local gpu.nvidia.com topology",
			mode: &allocmode.Mode{
				DRAUsable:             true,
				NonNodeLocalGPUSlices: []string{"gpu-slice-selector"},
			},
			wantErr: []string{"gpu-slice-selector", "non-node-local topologies", "#1327", "#1652"},
		},
		{
			name: "rejected: both present — foreign driver reported first",
			mode: &allocmode.Mode{
				ForeignGPUSliceDrivers: []string{"gpu.amd.com", "xpu.intel.com/gpu"},
				NonNodeLocalGPUSlices:  []string{"gpu-slice-selector"},
			},
			wantErr: []string{"gpu.amd.com, xpu.intel.com/gpu", "non-NVIDIA GPU driver"},
		},
		{
			name: "rejected: pool published by multiple nodes (ambiguous attribution)",
			mode: &allocmode.Mode{
				DRAUsable:         true,
				AmbiguousGPUPools: []string{"pool-dup"},
			},
			wantErr: []string{"pool-dup", "published by node-local slices of multiple nodes", "#1327", "#1652"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rejectUnsupportedGPUTopology(tt.mode)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("rejectUnsupportedGPUTopology() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("rejectUnsupportedGPUTopology() = nil, want fail-fast error")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("rejection message missing %q:\n%v", want, err)
				}
			}
		})
	}
}
