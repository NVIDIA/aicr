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
	"k8s.io/apimachinery/pkg/runtime"
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
	podSpec := map[string]any{
		"containers": []any{
			map[string]any{
				"name":  mainContainerName,
				"image": "img",
				"env": []any{
					map[string]any{"name": "HF_TOKEN", "value": "x"},
				},
			},
			map[string]any{
				"name":  "sidecar-frontend",
				"image": "img",
			},
		},
	}
	injectModelCacheMounts(podSpec)

	vols, _ := podSpec["volumes"].([]any)
	if len(vols) != 1 {
		t.Fatalf("want 1 volume, got %d", len(vols))
	}
	pvc, _ := vols[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)
	if pvc["claimName"] != modelCachePVCName || pvc["readOnly"] != true {
		t.Errorf("pvc volume = %v", pvc)
	}

	containers := podSpec["containers"].([]any)
	for _, raw := range containers {
		container := raw.(map[string]any)
		mounts, _ := container["volumeMounts"].([]any)
		if len(mounts) != 1 {
			t.Fatalf("%s: want 1 volumeMount, got %d", container["name"], len(mounts))
		}
		m := mounts[0].(map[string]any)
		if m["mountPath"] != modelCacheMountPath || m["readOnly"] != true {
			t.Errorf("%s volumeMount = %v", container["name"], m)
		}
	}

	got := map[string]string{}
	mainContainer := containers[0].(map[string]any)
	for _, e := range mainContainer["env"].([]any) {
		em := e.(map[string]any)
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

// TestDefaultStorageClass verifies detection of a default-annotated
// StorageClass (the cache pre-flight's signal). When more than one
// StorageClass is annotated default, Kubernetes' own admission controller
// picks the most recently created one — the two "multiple defaults" cases
// below list the same pair in both orders to prove selection follows
// CreationTimestamp, not list order.
func TestDefaultStorageClass(t *testing.T) {
	older := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := metav1.NewTime(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	tests := []struct {
		name     string
		classes  []runtime.Object
		wantName string // "" means want nil (no default)
	}{
		{
			name: "has default",
			classes: []runtime.Object{
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
					Name: "gp3", Annotations: map[string]string{defaultStorageClassAnnotation: "true"}}},
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp2"}},
			},
			wantName: "gp3",
		},
		{
			name: "no default",
			classes: []runtime.Object{
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp2"}},
			},
			wantName: "",
		},
		{
			name: "legacy beta annotation counts as default",
			classes: []runtime.Object{
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
					Name: "gp2", Annotations: map[string]string{defaultStorageClassAnnotationBeta: "true"}}},
			},
			wantName: "gp2",
		},
		{
			name: "multiple defaults, newer listed first: newer still wins",
			classes: []runtime.Object{
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
					Name: "newer", CreationTimestamp: newer, Annotations: map[string]string{defaultStorageClassAnnotation: "true"}}},
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
					Name: "older", CreationTimestamp: older, Annotations: map[string]string{defaultStorageClassAnnotation: "true"}}},
			},
			wantName: "newer",
		},
		{
			name: "multiple defaults, older listed first: newer still wins",
			classes: []runtime.Object{
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
					Name: "older", CreationTimestamp: older, Annotations: map[string]string{defaultStorageClassAnnotation: "true"}}},
				&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
					Name: "newer", CreationTimestamp: newer, Annotations: map[string]string{defaultStorageClassAnnotation: "true"}}},
			},
			wantName: "newer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(tt.classes...)}
			got, err := defaultStorageClass(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotName := ""
			if got != nil {
				gotName = got.Name
			}
			if gotName != tt.wantName {
				t.Errorf("got %q, want %q", gotName, tt.wantName)
			}
		})
	}
}

// TestMachineFamily verifies the family segment extracted from a
// node.kubernetes.io/instance-type value.
func TestMachineFamily(t *testing.T) {
	tests := []struct {
		instanceType string
		want         string
	}{
		{"a4x-highgpu-4g", "a4x"},
		{"n2-standard-4", "n2"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := machineFamily(tt.instanceType); got != tt.want {
			t.Errorf("machineFamily(%q) = %q, want %q", tt.instanceType, got, tt.want)
		}
	}
}

// TestCheckStorageClassNodeCompatibility verifies the rule-table lookup: a
// machine family listed under a rule can only attach a StorageClass whose
// parameters.type carries that rule's compatibleTypePrefix; every other
// family/provisioner/type combination passes.
func TestCheckStorageClassNodeCompatibility(t *testing.T) {
	pdBalanced := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "standard-rwo"},
		Provisioner: "pd.csi.storage.gke.io",
		Parameters:  map[string]string{"type": "pd-balanced"},
	}
	hyperdiskBalanced := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "hyperdisk-balanced"},
		Provisioner: "pd.csi.storage.gke.io",
		Parameters:  map[string]string{"type": "hyperdisk-balanced"},
	}
	dynamicSelect := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "dynamic-volume"},
		Provisioner: "pd.csi.storage.gke.io",
		Parameters:  map[string]string{"type": "dynamic", "pd-type": "pd-balanced", "hyperdisk-type": "hyperdisk-balanced"},
	}
	otherProvisioner := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "gp3"},
		Provisioner: "ebs.csi.aws.com",
		Parameters:  map[string]string{"type": "gp3"},
	}

	tests := []struct {
		name         string
		instanceType string
		sc           *storagev1.StorageClass
		wantErr      bool
	}{
		{"a4x with Persistent Disk is rejected", "a4x-highgpu-4g", pdBalanced, true},
		{"a4x with explicit Hyperdisk selection is fine", "a4x-highgpu-4g", hyperdiskBalanced, false},
		{"a4x with dynamic disk-type selection is fine", "a4x-highgpu-4g", dynamicSelect, false},
		{"non-a4x family with Persistent Disk is fine", "n2-standard-4", pdBalanced, false},
		{"non-a4x family with dynamic disk-type selection is fine", "n2-standard-4", dynamicSelect, false},
		{"a4x with an unrelated provisioner is fine", "a4x-highgpu-4g", otherProvisioner, false},
		{"nil StorageClass is fine (not this function's concern)", "a4x-highgpu-4g", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkStorageClassNodeCompatibility(tt.instanceType, tt.sc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestEnsureModelCache covers the pre-flight's error paths: disabled is a
// no-op, an enabled cache with no resolvable StorageClass errors, and an
// enabled cache whose resolved StorageClass (cluster-default or explicit
// override) is incompatible with the node's machine family errors too,
// in every case before a PVC is created, rather than that surfacing later
// as a Pending claim or FailedAttachVolume.
func TestEnsureModelCache(t *testing.T) {
	pdBalanced := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "standard-rwo", Annotations: map[string]string{defaultStorageClassAnnotation: "true"}},
		Provisioner: "pd.csi.storage.gke.io",
		Parameters:  map[string]string{"type": "pd-balanced"},
	}

	tests := []struct {
		name    string
		classes []runtime.Object
		cfg     *inferenceWorkloadConfig
		wantErr bool
	}{
		{
			name: "disabled is a no-op",
			cfg:  &inferenceWorkloadConfig{namespace: "ns", modelCacheSize: ""},
		},
		{
			name:    "no default StorageClass errors",
			cfg:     &inferenceWorkloadConfig{namespace: "ns", model: "Qwen/Qwen3-8B", modelCacheSize: defaultModelCacheSize},
			wantErr: true,
		},
		{
			name:    "incompatible cluster default is rejected",
			classes: []runtime.Object{pdBalanced},
			cfg: &inferenceWorkloadConfig{
				namespace: "ns", model: "Qwen/Qwen3-8B", modelCacheSize: defaultModelCacheSize,
				gpuNodeInstanceType: "a4x-highgpu-4g",
			},
			wantErr: true,
		},
		{
			name:    "incompatible explicit override is rejected",
			classes: []runtime.Object{pdBalanced},
			cfg: &inferenceWorkloadConfig{
				namespace: "ns", model: "Qwen/Qwen3-8B", modelCacheSize: defaultModelCacheSize,
				gpuNodeInstanceType: "a4x-highgpu-4g", modelCacheStorageClass: "standard-rwo",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.classes...)
			ctx := &validators.Context{Ctx: context.Background(), Clientset: client}
			err := ensureModelCache(ctx, tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			pvcs, _ := client.CoreV1().PersistentVolumeClaims(tt.cfg.namespace).List(context.Background(), metav1.ListOptions{})
			if len(pvcs.Items) != 0 {
				t.Errorf("no PVC should be created, got %d", len(pvcs.Items))
			}
		})
	}
}

// TestEnsureModelCachePinsImplicitDefaultStorageClass verifies the PVC created
// for an implicit (cluster-default) StorageClass is pinned to the exact
// default resolved and validated above, rather than left nil. A nil
// StorageClassName lets the apiserver re-resolve the default at admission
// time, which could pick a different (possibly incompatible) default if it
// changed between this pre-flight check and PVC creation.
func TestEnsureModelCachePinsImplicitDefaultStorageClass(t *testing.T) {
	def := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "standard-rwo", Annotations: map[string]string{defaultStorageClassAnnotation: "true"}},
		Provisioner: "pd.csi.storage.gke.io",
		Parameters:  map[string]string{"type": "pd-balanced"},
	}
	// Non-a4x family: compatible with pd-balanced, so the pre-flight check
	// passes and execution reaches PVC creation.
	cfg := &inferenceWorkloadConfig{
		namespace: "ns", model: "Qwen/Qwen3-8B", modelCacheSize: defaultModelCacheSize,
		gpuNodeInstanceType: "n2-standard-4", runID: "run1",
	}
	// Force the populate-Job wait to fail fast instead of hanging on the fake
	// clientset's Job status, which never progresses to Complete.
	t.Setenv(envModelCachePopulateTimeout, "1ms")

	client := fake.NewClientset(def)
	ctx := &validators.Context{Ctx: context.Background(), Clientset: client}
	if err := ensureModelCache(ctx, cfg); err == nil {
		t.Fatal("expected populate-Job wait to time out on the fake clientset")
	}

	pvc, err := client.CoreV1().PersistentVolumeClaims(cfg.namespace).Get(context.Background(), modelCachePVCName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PVC to be created before the populate-Job wait, got: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != def.Name {
		t.Errorf("PVC StorageClassName = %v, want pinned to resolved default %q", pvc.Spec.StorageClassName, def.Name)
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
