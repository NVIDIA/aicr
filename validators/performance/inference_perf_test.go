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
	batchv1 "k8s.io/api/batch/v1"
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

// TestParseDynamoTemplate_ScalarModelStaysString guards the quoting of
// value: "${MODEL}" at testdata/inference/dynamo-deployment.yaml:43. A
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
			envs, _, err := unstructured.NestedSlice(obj.Object, "spec", "services", "Frontend", "envs")
			if err != nil {
				t.Fatalf("read Frontend envs: %v", err)
			}
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
