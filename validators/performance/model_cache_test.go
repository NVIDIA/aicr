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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestBuildModelCachePVC verifies the PVC sets an explicit StorageClass when
// provided (required on clusters with no default StorageClass — the bug that
// left the claim Pending) and leaves it nil (cluster default) when empty.
func TestBuildModelCachePVC(t *testing.T) {
	qty := resource.MustParse("100Gi")
	t.Run("explicit storage class", func(t *testing.T) {
		pvc := buildModelCachePVC("ns", qty, "gp2")
		if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "gp2" {
			t.Errorf("storageClassName = %v, want gp2", pvc.Spec.StorageClassName)
		}
		if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != v1.ReadWriteOnce {
			t.Errorf("accessModes = %v, want [ReadWriteOnce]", pvc.Spec.AccessModes)
		}
	})
	t.Run("empty storage class uses cluster default (nil)", func(t *testing.T) {
		pvc := buildModelCachePVC("ns", qty, "")
		if pvc.Spec.StorageClassName != nil {
			t.Errorf("storageClassName = %v, want nil", *pvc.Spec.StorageClassName)
		}
	})
}

func TestModelCacheEnabled(t *testing.T) {
	cases := []struct {
		size string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"100Gi", true},
	}
	for _, c := range cases {
		if got := modelCacheEnabled(&inferenceWorkloadConfig{modelCacheSize: c.size}); got != c.want {
			t.Errorf("modelCacheEnabled(%q) = %v, want %v", c.size, got, c.want)
		}
	}
}

// TestInjectModelCacheMounts verifies the cache PVC volume, read-only mount, and
// HF_HOME/HF_HUB_OFFLINE env are added to a component pod spec while preserving
// env the template already declares (e.g. HF_TOKEN).
func TestInjectModelCacheMounts(t *testing.T) {
	podSpec := map[string]interface{}{
		"containers": []interface{}{
			map[string]interface{}{
				"name":  mainContainerName,
				"image": "img",
				"env": []interface{}{
					map[string]interface{}{"name": "HF_TOKEN", "value": "x"},
				},
			},
			map[string]interface{}{
				"name":  "sidecar-frontend",
				"image": "img",
			},
		},
	}
	injectModelCacheMounts(podSpec)

	vols, _ := podSpec["volumes"].([]interface{})
	if len(vols) != 1 {
		t.Fatalf("want 1 volume, got %d", len(vols))
	}
	pvc, _ := vols[0].(map[string]interface{})["persistentVolumeClaim"].(map[string]interface{})
	if pvc["claimName"] != modelCachePVCName || pvc["readOnly"] != true {
		t.Errorf("pvc volume = %v", pvc)
	}

	containers := podSpec["containers"].([]interface{})
	for _, raw := range containers {
		container := raw.(map[string]interface{})
		mounts, _ := container["volumeMounts"].([]interface{})
		if len(mounts) != 1 {
			t.Fatalf("%s: want 1 volumeMount, got %d", container["name"], len(mounts))
		}
		m := mounts[0].(map[string]interface{})
		if m["mountPath"] != modelCacheMountPath || m["readOnly"] != true {
			t.Errorf("%s volumeMount = %v", container["name"], m)
		}
	}

	got := map[string]string{}
	mainContainer := containers[0].(map[string]interface{})
	for _, e := range mainContainer["env"].([]interface{}) {
		em := e.(map[string]interface{})
		if v, ok := em["value"].(string); ok {
			got[em["name"].(string)] = v
		}
	}
	if got["HF_TOKEN"] != "x" {
		t.Error("existing HF_TOKEN env was dropped")
	}
	if got["HF_HOME"] != modelCacheMountPath {
		t.Errorf("HF_HOME = %q, want %q", got["HF_HOME"], modelCacheMountPath)
	}
	if got["HF_HUB_OFFLINE"] != "1" {
		t.Errorf("HF_HUB_OFFLINE = %q, want 1", got["HF_HUB_OFFLINE"])
	}
}

// TestBuildModelCachePopulateJob verifies the one-time download Job is pinned to
// the workers' node, mounts the cache PVC, and carries the model + HF token env.
func TestBuildModelCachePopulateJob(t *testing.T) {
	// Set a different env model to prove the populate Job downloads the
	// recipe-resolved config.model (set on the config), NOT the env/default —
	// otherwise an overlay's inference-model and the cached weights diverge and
	// the offline workers fail to find their model.
	t.Setenv(envModel, "Qwen/Qwen3-0.6B")
	cfg := &inferenceWorkloadConfig{
		runID:           "run-1",
		namespace:       "ns",
		model:           "Qwen/Qwen3-32B",
		gpuNodeSelector: map[string]string{"kubernetes.io/hostname": "node-a"},
	}
	pullSecrets := []v1.LocalObjectReference{{Name: "regcred"}}
	job := buildModelCachePopulateJob("aicr-model-cache-populate-run-1", cfg, pullSecrets)

	spec := job.Spec.Template.Spec
	// imagePullSecrets propagate from the validator pod so a private-mirror /
	// air-gapped cluster can pull cacheWorkerImage (parity with the AIPerf Job).
	if len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "regcred" {
		t.Errorf("ImagePullSecrets = %v, want [{regcred}]", spec.ImagePullSecrets)
	}
	if spec.NodeSelector["kubernetes.io/hostname"] != "node-a" {
		t.Errorf("node pin missing: %v", spec.NodeSelector)
	}
	if spec.RestartPolicy != v1.RestartPolicyNever {
		t.Errorf("restartPolicy = %v, want Never", spec.RestartPolicy)
	}
	if len(spec.Volumes) != 1 {
		t.Fatalf("want 1 volume, got %d", len(spec.Volumes))
	}
	if pvc := spec.Volumes[0].PersistentVolumeClaim; pvc == nil || pvc.ClaimName != modelCachePVCName {
		t.Errorf("cache PVC volume missing: %v", spec.Volumes)
	}
	c := spec.Containers[0]
	if c.Image != cacheWorkerImage {
		t.Errorf("image = %q, want %q", c.Image, cacheWorkerImage)
	}
	if !strings.Contains(strings.Join(c.Command, " "), "snapshot_download") {
		t.Errorf("command missing snapshot_download: %v", c.Command)
	}
	envVal := map[string]string{}
	hasTokenRef := false
	for _, e := range c.Env {
		if e.Value != "" {
			envVal[e.Name] = e.Value
		}
		if e.Name == "HF_TOKEN" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			hasTokenRef = true
		}
	}
	if envVal["AICR_MODEL"] != "Qwen/Qwen3-32B" {
		t.Errorf("AICR_MODEL = %q, want config.model (Qwen/Qwen3-32B), not env/default", envVal["AICR_MODEL"])
	}
	if envVal["HF_HOME"] != modelCacheMountPath {
		t.Errorf("HF_HOME = %q, want %q", envVal["HF_HOME"], modelCacheMountPath)
	}
	if !hasTokenRef {
		t.Error("HF_TOKEN secretKeyRef missing")
	}
	// Explicit requests so the populate pod schedules under ResourceQuota /
	// LimitRange admission.
	if got := c.Resources.Requests.Cpu().String(); got != cacheJobCPURequest {
		t.Errorf("CPU request = %q, want %q", got, cacheJobCPURequest)
	}
	if got := c.Resources.Requests.Memory().String(); got != cacheJobMemoryRequest {
		t.Errorf("memory request = %q, want %q", got, cacheJobMemoryRequest)
	}
	// Deliberately NO memory limit — it OOMKills large-model downloads via page
	// cache on cgroup v2 (caught on a live 8B run). A limit here is a regression.
	if _, ok := c.Resources.Limits[v1.ResourceMemory]; ok {
		t.Errorf("populate container must NOT set a memory limit (page-cache OOMKill on large models); got %v", c.Resources.Limits)
	}
}

// TestPopulateJobTimeout guards the #1859 decoupling: the model-cache populate
// wait uses the dedicated ModelCachePopulateTimeout, NOT the DynamoGraphDeployment
// workload-ready budget. A regression swapping the constant/env back would be
// caught here.
func TestPopulateJobTimeout(t *testing.T) {
	// The decoupling is meaningful only if the two budgets differ; guard it once
	// for every case below.
	if defaults.ModelCachePopulateTimeout == defaults.InferenceWorkloadReadyTimeout {
		t.Fatal("populate budget must be decoupled from InferenceWorkloadReadyTimeout")
	}

	tests := []struct {
		name        string
		populateEnv string // AICR_INFERENCE_PERF_MODEL_CACHE_POPULATE_TIMEOUT
		workloadEnv string // AICR_INFERENCE_PERF_WORKLOAD_READY_TIMEOUT
		want        time.Duration
	}{
		{
			name: "default is the dedicated populate budget",
			want: defaults.ModelCachePopulateTimeout,
		},
		{
			name:        "widening workload-ready does not leak into the populate budget",
			workloadEnv: "59m",
			want:        defaults.ModelCachePopulateTimeout,
		},
		{
			name:        "dedicated populate knob overrides",
			populateEnv: "20m",
			workloadEnv: "59m",
			want:        20 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envModelCachePopulateTimeout, tt.populateEnv)
			t.Setenv(envWorkloadReadyTimeout, tt.workloadEnv)
			got, err := populateJobTimeout()
			if err != nil {
				t.Fatalf("populateJobTimeout() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("populateJobTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWrapPopulateJobError verifies the populate-Job failure classifier carries
// the actionable remediation on the timeout path (the case #1859 targets) while
// propagating other coded failures unchanged.
func TestWrapPopulateJobError(t *testing.T) {
	const timeout = 13 * time.Minute

	tests := []struct {
		name            string
		input           error
		wantCode        error
		wantRemediation bool // timeout path must add the HF-token/env remediation
		wantUnchanged   bool // non-timeout path must return the exact input instance
		wantText        []string
	}{
		{
			name:            "timeout carries remediation and stays timeout-coded",
			input:           errors.New(errors.ErrCodeTimeout, "context deadline exceeded"),
			wantCode:        errors.New(errors.ErrCodeTimeout, ""),
			wantRemediation: true,
			wantText:        []string{"13m0s", "HF-token", envModelCachePopulateTimeout},
		},
		{
			name:          "non-timeout coded error propagates unchanged (no remediation)",
			input:         errors.New(errors.ErrCodeUnavailable, "watch closed unexpectedly"),
			wantCode:      errors.New(errors.ErrCodeUnavailable, ""),
			wantUnchanged: true,
			wantText:      []string{"watch closed unexpectedly"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapPopulateJobError(tt.input, timeout)
			if !stderrors.Is(got, tt.wantCode) {
				t.Fatalf("want code %v, got %v", tt.wantCode, got)
			}
			// A coded non-timeout error must be returned as the same instance,
			// not re-wrapped — the code+substring checks alone would still pass
			// if wrapPopulateJobError rewrapped it in a fresh coded error.
			if tt.wantUnchanged && got != tt.input { //nolint:errorlint // identity check: verify the exact instance is propagated unchanged
				t.Errorf("non-timeout error should be returned unchanged, got %#v", got)
			}
			for _, want := range tt.wantText {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("error %q missing %q", got.Error(), want)
				}
			}
			if hasRemediation := strings.Contains(got.Error(), "HF-token"); hasRemediation != tt.wantRemediation {
				t.Errorf("remediation present = %v, want %v: %q", hasRemediation, tt.wantRemediation, got.Error())
			}
		})
	}
}

// TestEnsureModelCache_DisabledNoop verifies that with the cache disabled no PVC
// or Job is created — the default behavior is unchanged.

func TestEnsureModelCache_DisabledNoop(t *testing.T) {
	client := fake.NewClientset()
	ctx := &validators.Context{Ctx: context.Background(), Clientset: client}
	cfg := &inferenceWorkloadConfig{namespace: "ns", modelCacheSize: ""}
	if err := ensureModelCache(ctx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pvcs, _ := client.CoreV1().PersistentVolumeClaims("ns").List(context.Background(), metav1.ListOptions{})
	if len(pvcs.Items) != 0 {
		t.Errorf("no PVC should be created when cache disabled, got %d", len(pvcs.Items))
	}
}

// TestParseModelCacheSize verifies the on-by-default policy: unset → default
// size (enabled), the disable sentinels → disabled, an explicit quantity passes
// through, and garbage fails closed.
func TestParseModelCacheSize(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantSize    string
		wantEnabled bool
		wantErr     bool
	}{
		{"unset → default on", "", defaultModelCacheSize, true, false},
		{"whitespace → default on", "  ", defaultModelCacheSize, true, false},
		{"explicit size", "200Gi", "200Gi", true, false},
		{"off → disabled", "off", "", false, false},
		{"OFF case-insensitive", "OFF", "", false, false},
		{"0 → disabled", "0", "", false, false},
		{"none → disabled", "none", "", false, false},
		{"disabled → disabled", "disabled", "", false, false},
		{"garbage → error", "lots-of-space", "", false, true},
		{"zero quantity → error", "0Gi", "", false, true},
		{"negative quantity → error", "-1Gi", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, enabled, err := parseModelCacheSize(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if size != tt.wantSize || enabled != tt.wantEnabled {
				t.Errorf("parseModelCacheSize(%q) = (%q,%v), want (%q,%v)", tt.raw, size, enabled, tt.wantSize, tt.wantEnabled)
			}
		})
	}
}

// TestClusterHasDefaultStorageClass verifies detection of a default-annotated
// StorageClass (the cache pre-flight's signal).
func TestClusterHasDefaultStorageClass(t *testing.T) {
	def := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
		Name: "gp3", Annotations: map[string]string{defaultStorageClassAnnotation: "true"}}}
	nondef := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp2"}}

	t.Run("has default", func(t *testing.T) {
		ctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(def, nondef)}
		got, err := clusterHasDefaultStorageClass(ctx)
		if err != nil || !got {
			t.Errorf("got (%v,%v), want (true,nil)", got, err)
		}
	})
	t.Run("no default", func(t *testing.T) {
		ctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(nondef)}
		got, err := clusterHasDefaultStorageClass(ctx)
		if err != nil || got {
			t.Errorf("got (%v,%v), want (false,nil)", got, err)
		}
	})
	t.Run("legacy beta annotation counts as default", func(t *testing.T) {
		beta := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
			Name: "gp2", Annotations: map[string]string{defaultStorageClassAnnotationBeta: "true"}}}
		ctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(beta)}
		got, err := clusterHasDefaultStorageClass(ctx)
		if err != nil || !got {
			t.Errorf("got (%v,%v), want (true,nil) for beta is-default-class annotation", got, err)
		}
	})
}

// TestEnsureModelCache_NoDefaultStorageClassFailsFast verifies that with the
// cache enabled, no explicit StorageClass, and no cluster default, the validator
// fails fast (ErrCodeInvalidRequest) without creating the PVC — rather than
// leaving it Pending until the populate-Job timeout.
func TestEnsureModelCache_NoDefaultStorageClassFailsFast(t *testing.T) {
	ctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset()}
	cfg := &inferenceWorkloadConfig{namespace: "ns", model: "Qwen/Qwen3-8B", modelCacheSize: defaultModelCacheSize}
	err := ensureModelCache(ctx, cfg)
	if err == nil {
		t.Fatal("expected fast-fail error when cache enabled with no default StorageClass")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
	}
	pvcs, _ := ctx.Clientset.CoreV1().PersistentVolumeClaims("ns").List(context.Background(), metav1.ListOptions{})
	if len(pvcs.Items) != 0 {
		t.Errorf("no PVC should be created on fast-fail, got %d", len(pvcs.Items))
	}
}

// TestCacheWorkerImageMatchesTemplate guards the cacheWorkerImage constant
// against drifting from the worker image in the Dynamo deploy template. The
// populate Job must download with the same vLLM runtime the workers use: the
// workers load the populated HF_HOME offline (HF_HUB_OFFLINE=1), so a layout
// mismatch fails closed. The constant is kept in sync by comment only, so this
// test fails loudly if a template bump isn't mirrored on the constant.
func TestCacheWorkerImageMatchesTemplate(t *testing.T) {
	for _, path := range []string{
		"testdata/inference/dynamo-deployment.yaml",
		"testdata/inference/dynamo-deployment-gateway-epp.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read deploy template: %v", err)
			}
			if !strings.Contains(string(data), cacheWorkerImage) {
				t.Errorf("cacheWorkerImage %q not found in %s; "+
					"the populate-Job image has drifted from the worker image — update cacheWorkerImage to match", cacheWorkerImage, path)
			}
		})
	}
}
