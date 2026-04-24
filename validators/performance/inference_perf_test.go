// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHasDynamoPlatform(t *testing.T) {
	tests := []struct {
		name string
		ctx  *validators.Context
		want bool
	}{
		{
			name: "nil recipe",
			ctx:  &validators.Context{Recipe: nil},
			want: false,
		},
		{
			name: "empty componentRefs",
			ctx: &validators.Context{Recipe: &recipe.RecipeResult{
				ComponentRefs: nil,
			}},
			want: false,
		},
		{
			name: "componentRefs without dynamo-platform",
			ctx: &validators.Context{Recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
					{Name: "kubeflow-trainer"},
				},
			}},
			want: false,
		},
		{
			name: "dynamo-platform present",
			ctx: &validators.Context{Recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
					{Name: "dynamo-platform"},
				},
			}},
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
	tests := []struct {
		name  string
		input *unstructured.Unstructured
		want  bool
	}{
		{
			name:  "nil object",
			input: nil,
			want:  false,
		},
		{
			name:  "no status",
			input: &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{}}},
			want:  false,
		},
		{
			name: "state != successful",
			input: &unstructured.Unstructured{Object: map[string]interface{}{
				"status": map[string]interface{}{"state": "pending"},
			}},
			want: false,
		},
		{
			name: "state == successful",
			input: &unstructured.Unstructured{Object: map[string]interface{}{
				"status": map[string]interface{}{"state": "successful"},
			}},
			want: true,
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
			"apiVersion": "nvidia.com/v1alpha1",
			"kind":       "DynamoGraphDeployment",
			"spec": map[string]interface{}{
				"services": map[string]interface{}{
					"Frontend": map[string]interface{}{
						"componentType": "frontend",
						"replicas":      int64(1),
					},
					"VllmDecodeWorker": map[string]interface{}{
						"componentType": "worker",
						"replicas":      int64(4),
					},
				},
			},
		}}
	}

	config := &inferenceWorkloadConfig{
		gpuCount:        4,
		gpuNodeSelector: map[string]string{"nodeGroup": "gpu-worker"},
		gpuTolerations: []v1.Toleration{
			{Key: "dedicated", Operator: v1.TolerationOpEqual, Value: "worker-workload", Effect: v1.TaintEffectNoSchedule},
		},
	}

	obj := newObj()
	if err := applyInferenceWorkerScheduling(obj, config); err != nil {
		t.Fatalf("applyInferenceWorkerScheduling() error: %v", err)
	}

	// Worker must have nodeSelector, tolerations, and resourceClaims.
	worker, _, _ := unstructured.NestedMap(obj.Object, "spec", "services", "VllmDecodeWorker", "extraPodSpec")
	if worker == nil {
		t.Fatal("VllmDecodeWorker extraPodSpec not set")
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
	claims, _, _ := unstructured.NestedSlice(worker, "resourceClaims")
	if len(claims) != 1 {
		t.Fatalf("worker resourceClaims count = %d, want 1", len(claims))
	}
	claim := claims[0].(map[string]interface{})
	if claim["name"] != "gpu" || claim["resourceClaimTemplateName"] != inferenceClaimTemplateName {
		t.Errorf("worker resourceClaim = %v, want name=gpu + template=%s", claim, inferenceClaimTemplateName)
	}

	// Frontend must have tolerations AND the same nodeSelector as worker —
	// they co-locate on the GPU node cohort so cross-namespace traffic stays
	// inside a single node-group Security Group on EKS. Frontend does NOT get
	// a ResourceClaim (it's CPU-only).
	frontend, _, _ := unstructured.NestedMap(obj.Object, "spec", "services", "Frontend", "extraPodSpec")
	if frontend == nil {
		t.Fatal("Frontend extraPodSpec not set")
	}
	frontTols, _, _ := unstructured.NestedSlice(frontend, "tolerations")
	if len(frontTols) != 1 {
		t.Errorf("frontend tolerations count = %d, want 1", len(frontTols))
	}
	frontNS, _, _ := unstructured.NestedMap(frontend, "nodeSelector")
	if frontNS["nodeGroup"] != "gpu-worker" {
		t.Errorf("frontend nodeSelector should match worker for SG co-location: got %v", frontNS)
	}
	if _, found, _ := unstructured.NestedSlice(frontend, "resourceClaims"); found {
		t.Error("frontend resourceClaims should not be set — only worker needs GPU allocation")
	}
}

func TestApplyInferenceWorkerScheduling_MissingServices(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{},
	}}
	err := applyInferenceWorkerScheduling(obj, &inferenceWorkloadConfig{})
	if err == nil {
		t.Fatal("applyInferenceWorkerScheduling() expected error for missing spec.services, got nil")
	}
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

	pullSecrets := []v1.LocalObjectReference{
		{Name: "ghcr-mirror-pull"},
		{Name: "nvcr-pull"},
	}
	const jobName = "aicr-aiperf-run-42"
	job := buildAIPerfJob("test-ns", jobName, "http://frontend.test-ns.svc:8000", 16, pullSecrets)
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
	job := buildAIPerfJob("test-ns", "aicr-aiperf-run-0", "http://ep:8000", 16, nil)
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
			job := buildAIPerfJob("ns", "aicr-aiperf-run-0", "http://ep:8000", 16, nil)
			got := job.Spec.Template.Spec.Containers[0].ImagePullPolicy
			if got != tt.want {
				t.Errorf("aiperf ImagePullPolicy = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildAIPerfJob_RequestCountFloor(t *testing.T) {
	tests := []struct {
		name        string
		concurrency int
		wantMinReqs int
	}{
		{"low concurrency — floor at aiperfMinRequests", 16, aiperfMinRequests},
		{"high concurrency — 2x concurrency", 500, 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := buildAIPerfJob("ns", "aicr-aiperf-run-0", "http://ep:8000", tt.concurrency, nil)
			script := job.Spec.Template.Spec.Containers[0].Args[0]
			needle := fmt.Sprintf("--request-count %d", tt.wantMinReqs)
			if !strings.Contains(script, needle) {
				t.Errorf("script missing %q; script:\n%s", needle, script)
			}
		})
	}
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

	tests := []struct {
		name   string
		claims []*resourcev1.ResourceClaim
		want   map[string]int
	}{
		{
			name: "one GPU on one node",
			claims: []*resourcev1.ResourceClaim{
				makeClaim("dynamo", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Device: "gpu-3", Driver: "gpu.nvidia.com", Pool: "node-a", Request: "gpu"},
				}),
			},
			want: map[string]int{"node-a": 1},
		},
		{
			name: "multiple results on same claim accumulate per pool",
			claims: []*resourcev1.ResourceClaim{
				makeClaim("ns", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Device: "gpu-1", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-b"},
				}),
			},
			want: map[string]int{"node-a": 2, "node-b": 1},
		},
		{
			name: "non-GPU drivers are ignored",
			claims: []*resourcev1.ResourceClaim{
				makeClaim("ns", "c1", []resourcev1.DeviceRequestAllocationResult{
					{Device: "gpu-0", Driver: "gpu.nvidia.com", Pool: "node-a"},
					{Device: "tpu-0", Driver: "tpu.google.com", Pool: "node-a"},
				}),
			},
			want: map[string]int{"node-a": 1},
		},
		{
			name: "unallocated claim — nothing counted",
			claims: []*resourcev1.ResourceClaim{
				makeClaim("ns", "pending", nil),
			},
			want: map[string]int{},
		},
		{
			name:   "no claims at all",
			claims: nil,
			want:   map[string]int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tt.claims))
			for _, c := range tt.claims {
				objs = append(objs, c)
			}
			client := fake.NewClientset(objs...)
			got := countUsedGPUsByNode(context.Background(), client)
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
}

func TestPickCandidateWithMostFreeGPUs(t *testing.T) {
	n8 := func(name string) v1.Node {
		return v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: v1.NodeStatus{Allocatable: v1.ResourceList{
				"nvidia.com/gpu": resource.MustParse("8"),
			}},
		}
	}

	tests := []struct {
		name            string
		candidates      []v1.Node
		used            map[string]int
		wantNode        string
		wantAllocatable int
		wantFree        int
	}{
		{
			name:            "no in-use DRA allocations — first candidate, full capacity",
			candidates:      []v1.Node{n8("a"), n8("b")},
			used:            nil,
			wantNode:        "a",
			wantAllocatable: 8,
			wantFree:        8,
		},
		{
			name:            "first candidate saturated — picks second with more free",
			candidates:      []v1.Node{n8("a"), n8("b")},
			used:            map[string]int{"a": 8},
			wantNode:        "b",
			wantAllocatable: 8,
			wantFree:        8,
		},
		{
			name:            "first candidate partially used — still wins if second is more used",
			candidates:      []v1.Node{n8("a"), n8("b")},
			used:            map[string]int{"a": 1, "b": 5},
			wantNode:        "a",
			wantAllocatable: 8,
			wantFree:        7,
		},
		{
			name:            "all saturated — returns zero free (caller decides to fail)",
			candidates:      []v1.Node{n8("a"), n8("b")},
			used:            map[string]int{"a": 8, "b": 8},
			wantNode:        "a", // ties break on original order
			wantAllocatable: 8,
			wantFree:        0,
		},
		{
			name:            "negative free clamped to 0 (stale/mismatched claim)",
			candidates:      []v1.Node{n8("a")},
			used:            map[string]int{"a": 99},
			wantNode:        "a",
			wantAllocatable: 8,
			wantFree:        0,
		},
		{
			name:            "empty candidates — safe zero return (caller already guards)",
			candidates:      nil,
			used:            map[string]int{"a": 5},
			wantNode:        "",
			wantAllocatable: 0,
			wantFree:        0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chosen, alloc, free := pickCandidateWithMostFreeGPUs(tt.candidates, tt.used)
			if chosen.Name != tt.wantNode {
				t.Errorf("chosen = %q, want %q", chosen.Name, tt.wantNode)
			}
			if alloc != tt.wantAllocatable {
				t.Errorf("allocatable = %d, want %d", alloc, tt.wantAllocatable)
			}
			if free != tt.wantFree {
				t.Errorf("free = %d, want %d", free, tt.wantFree)
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
