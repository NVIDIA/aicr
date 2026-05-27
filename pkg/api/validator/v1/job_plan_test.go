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

package v1

import (
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGenerateJobName(t *testing.T) {
	validatorName := "gpu-operator-health"

	// Generate multiple job names to ensure they're unique
	name1 := generateJobName(validatorName)
	name2 := generateJobName(validatorName)

	// Check format
	expectedPrefix := "aicr-" + validatorName + "-"
	if !strings.HasPrefix(name1, expectedPrefix) {
		t.Errorf("generateJobName() = %q, should have prefix %q", name1, expectedPrefix)
	}
	if !strings.HasPrefix(name2, expectedPrefix) {
		t.Errorf("generateJobName() = %q, should have prefix %q", name2, expectedPrefix)
	}

	// Check uniqueness
	if name1 == name2 {
		t.Errorf("generateJobName() should produce unique names, both got %q", name1)
	}

	// Check suffix format (should be hex)
	suffix1 := strings.TrimPrefix(name1, expectedPrefix)
	if len(suffix1) != 8 { // 4 bytes = 8 hex chars
		t.Errorf("generateJobName() suffix length = %d, want 8 (4 bytes as hex)", len(suffix1))
	}
	for _, c := range suffix1 {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("generateJobName() suffix %q contains non-hex character %c", suffix1, c)
			break
		}
	}
}

func TestGenerateRunID(t *testing.T) {
	id1 := GenerateRunID()
	id2 := GenerateRunID()

	if id1 == "" {
		t.Error("RunID should not be empty")
	}
	if id1 == id2 {
		t.Error("RunIDs should be unique")
	}
	if len(id1) < 20 {
		t.Errorf("RunID too short: %q", id1)
	}
}

// TestImagePullPolicy covers the shared helper that both the outer
// validator Deployer and the inner aiperf-bench Job call, so they stay in
// lockstep. The digest-pin case is the specific Codex P3 concern: forcing
// PullAlways on a digest-pinned ref (e.g. an external catalog entry that
// stayed `name@sha256:…`) would break disconnected/private clusters by
// making kubelet re-contact the registry every run, for no correctness
// benefit (the digest is cryptographically immutable).
func TestImagePullPolicy(t *testing.T) {
	tests := []struct {
		name   string
		image  string
		envTag string // AICR_VALIDATOR_IMAGE_TAG — empty means unset
		want   corev1.PullPolicy
	}{
		// ----- side-loaded refs win unconditionally -----
		{name: "ko.local → Never", image: "ko.local/aicr-validators/x:latest", want: corev1.PullNever},
		{name: "kind.local → Never", image: "kind.local/aicr-validators/x:latest", want: corev1.PullNever},
		{name: "ko.local + override still Never", image: "ko.local/aicr-validators/x:edge", envTag: "edge", want: corev1.PullNever},
		// The side-load check must anchor on the full registry segment
		// (trailing slash) so a real registry like `ko.localhost:5000/...`
		// is not misread as `ko.local/...` and forced to PullNever —
		// kubelet would then be unable to pull from the real registry.
		{name: "ko.localhost:5000 registry → not treated as side-load", image: "ko.localhost:5000/aicr-validators/x:v1", want: corev1.PullIfNotPresent},
		{name: "kind.localhost:5000 registry → not treated as side-load", image: "kind.localhost:5000/aicr-validators/x:v1", want: corev1.PullIfNotPresent},

		// ----- digest pins are immutable → IfNotPresent -----
		{
			name:  "digest-only ref → IfNotPresent (immutable by construction)",
			image: "ghcr.io/foo/bar@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			want:  corev1.PullIfNotPresent,
		},
		{
			// Codex P3: the tag override must NOT upgrade a digest ref to
			// PullAlways. Doing so would make disconnected/air-gapped
			// clusters re-contact the registry every run for no gain.
			name:   "digest-only ref + override → IfNotPresent (override does not apply)",
			image:  "ghcr.io/foo/bar@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			envTag: "latest",
			want:   corev1.PullIfNotPresent,
		},
		{
			name:  "mixed ref name:tag@digest → IfNotPresent (digest wins)",
			image: "ghcr.io/foo/bar:v1.0.0@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			want:  corev1.PullIfNotPresent,
		},

		// ----- override forces Always on non-digest refs -----
		{
			name:   "override with :edge → Always (avoid stale cache on mutable tag)",
			image:  "ghcr.io/nvidia/aicr-validators/performance:edge",
			envTag: "edge",
			want:   corev1.PullAlways,
		},
		{
			name:   "override with release :v0.11.0 → Always (safe over-pull, not a regression)",
			image:  "ghcr.io/nvidia/aicr-validators/performance:v0.11.0",
			envTag: "v0.11.0",
			want:   corev1.PullAlways,
		},

		// ----- default policy (no override) -----
		{name: ":latest → Always", image: "ghcr.io/nvidia/aicr-validators/performance:latest", want: corev1.PullAlways},
		{name: ":vX.Y.Z → IfNotPresent", image: "ghcr.io/nvidia/aicr-validators/performance:v1.0.0", want: corev1.PullIfNotPresent},
		{name: ":sha-<commit> → IfNotPresent (main-branch dev default)", image: "ghcr.io/nvidia/aicr-validators/performance:sha-abc1234", want: corev1.PullIfNotPresent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ImagePullPolicy(tt.image, tt.envTag); got != tt.want {
				t.Errorf("ImagePullPolicy(%q, %q) = %q, want %q", tt.image, tt.envTag, got, tt.want)
			}
		})
	}
}

func TestBuildJobPlan(t *testing.T) {
	entry := ValidatorEntry{
		Name:        "test-validator",
		Phase:       "deployment",
		Image:       "test-image:v1.0.0",
		Args:        []string{"--test"},
		Timeout:     5 * time.Minute,
		Env:         []EnvVar{{Name: "CUSTOM", Value: "value"}},
		Resources:   &ResourceRequirements{CPU: "2", Memory: "2Gi"},
		Description: "Test validator",
	}

	nodeSelector := map[string]string{
		"gpu":  "true",
		"zone": "us-west",
	}
	tolerations := []corev1.Toleration{
		{Key: "gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}

	plan, err := BuildJobPlan(
		entry,
		"test-run-123",
		"test-ns",
		"1.0.0",
		"abc123",
		"test-sa",
		[]string{"my-secret"},
		tolerations,
		nodeSelector,
		"",  // imageRegistryOverride
		"",  // imageTagOverride
		nil, // componentRefs
	)
	if err != nil {
		t.Fatalf("unexpected error from BuildJobPlan: %v", err)
	}

	// Verify basic fields
	if plan.ValidatorName != "test-validator" {
		t.Errorf("ValidatorName = %q, want test-validator", plan.ValidatorName)
	}
	if plan.Phase != "deployment" {
		t.Errorf("Phase = %q, want deployment", plan.Phase)
	}
	if plan.Image != "test-image:v1.0.0" {
		t.Errorf("Image = %q, want test-image:v1.0.0", plan.Image)
	}
	if plan.ServiceAccount != "test-sa" {
		t.Errorf("ServiceAccount = %q, want test-sa", plan.ServiceAccount)
	}
	if len(plan.ImagePullSecrets) != 1 || plan.ImagePullSecrets[0] != "my-secret" {
		t.Errorf("ImagePullSecrets = %v, want [my-secret]", plan.ImagePullSecrets)
	}

	// Verify JobName is generated
	if plan.JobName == "" {
		t.Error("JobName should not be empty")
	}
	if !strings.HasPrefix(plan.JobName, "aicr-test-validator-") {
		t.Errorf("JobName = %q, should have prefix aicr-test-validator-", plan.JobName)
	}

	// Verify volumes (buildVolumes)
	if len(plan.Volumes) != 2 {
		t.Errorf("Volumes length = %d, want 2", len(plan.Volumes))
	}
	foundSnapshot := false
	foundValidation := false
	for _, v := range plan.Volumes {
		if v.Name == "snapshot" {
			foundSnapshot = true
			if v.ConfigMap == nil {
				t.Error("Snapshot volume should have ConfigMap")
			} else if v.ConfigMap.Name != "aicr-snapshot-test-run-123" {
				t.Errorf("Snapshot ConfigMap name = %q, want aicr-snapshot-test-run-123", v.ConfigMap.Name)
			}
		}
		if v.Name == "validation" {
			foundValidation = true
			if v.ConfigMap == nil {
				t.Error("Validation volume should have ConfigMap")
			} else if v.ConfigMap.Name != "aicr-validation-test-run-123" {
				t.Errorf("Validation ConfigMap name = %q, want aicr-validation-test-run-123", v.ConfigMap.Name)
			}
		}
	}
	if !foundSnapshot {
		t.Error("Snapshot volume not found")
	}
	if !foundValidation {
		t.Error("Validation volume not found")
	}

	// Verify volume mounts (buildVolumeMounts)
	if len(plan.VolumeMounts) != 2 {
		t.Errorf("VolumeMounts length = %d, want 2", len(plan.VolumeMounts))
	}

	// Verify resources (buildResources)
	if plan.Resources.Requests.Cpu().String() != "2" {
		t.Errorf("CPU request = %q, want 2", plan.Resources.Requests.Cpu().String())
	}
	if plan.Resources.Requests.Memory().String() != "2Gi" {
		t.Errorf("Memory request = %q, want 2Gi", plan.Resources.Requests.Memory().String())
	}

	// Verify environment variables include custom, version, and node selector (buildEnv, serializeNodeSelector)
	envMap := make(map[string]string)
	for _, e := range plan.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	if envMap["CUSTOM"] != "value" {
		t.Errorf("CUSTOM env = %q, want value", envMap["CUSTOM"])
	}
	if envMap["AICR_CLI_VERSION"] != "1.0.0" {
		t.Errorf("AICR_CLI_VERSION = %q, want 1.0.0", envMap["AICR_CLI_VERSION"])
	}
	if envMap["AICR_CLI_COMMIT"] != "abc123" {
		t.Errorf("AICR_CLI_COMMIT = %q, want abc123", envMap["AICR_CLI_COMMIT"])
	}
	if envMap["AICR_NODE_SELECTOR"] == "" {
		t.Error("AICR_NODE_SELECTOR should be set")
	}
	// Verify node selector serialization (should contain both keys)
	if !strings.Contains(envMap["AICR_NODE_SELECTOR"], "gpu=true") {
		t.Errorf("AICR_NODE_SELECTOR = %q, should contain gpu=true", envMap["AICR_NODE_SELECTOR"])
	}
	if !strings.Contains(envMap["AICR_NODE_SELECTOR"], "zone=us-west") {
		t.Errorf("AICR_NODE_SELECTOR = %q, should contain zone=us-west", envMap["AICR_NODE_SELECTOR"])
	}

	// Verify tolerations serialization (serializeTolerations)
	if envMap["AICR_TOLERATIONS"] == "" {
		t.Error("AICR_TOLERATIONS should be set")
	}
}

func TestBuildJobPlanWithDefaults(t *testing.T) {
	// Test with minimal entry (no custom resources, no tolerations, no node selector)
	entry := ValidatorEntry{
		Name:  "minimal-validator",
		Phase: "performance",
		Image: "minimal-image:latest",
	}

	plan, err := BuildJobPlan(entry, "run-456", "ns", "", "", "sa", nil, nil, nil, "", "", nil)
	if err != nil {
		t.Fatalf("unexpected error from BuildJobPlan: %v", err)
	}

	// Should have default resources (buildResources default path)
	if plan.Resources.Requests.Cpu().Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("Default CPU = %v, want 1", plan.Resources.Requests.Cpu())
	}
	if plan.Resources.Requests.Memory().Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("Default Memory = %v, want 1Gi", plan.Resources.Requests.Memory())
	}

	// Should not have node selector or tolerations env vars
	envMap := make(map[string]string)
	for _, e := range plan.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	if _, exists := envMap["AICR_NODE_SELECTOR"]; exists {
		t.Error("AICR_NODE_SELECTOR should not be set when nodeSelector is nil")
	}
	if _, exists := envMap["AICR_TOLERATIONS"]; exists {
		t.Error("AICR_TOLERATIONS should not be set when tolerations is nil")
	}
}

func TestRenderPlan(t *testing.T) {
	plan := JobPlan{
		ValidatorName:    "test-validator",
		Phase:            "deployment",
		JobName:          "test-job-abc123",
		Namespace:        "test-ns",
		Image:            "test-image:v1.0.0",
		Args:             []string{"--test"},
		Env:              []corev1.EnvVar{{Name: "TEST", Value: "value"}},
		Volumes:          []corev1.Volume{{Name: "snapshot"}},
		VolumeMounts:     []corev1.VolumeMount{{Name: "snapshot", MountPath: "/data"}},
		Resources:        corev1.ResourceRequirements{},
		Timeout:          300,
		ServiceAccount:   "test-sa",
		Tolerations:      []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		ImagePullSecrets: []string{"my-secret"},
		Labels:           map[string]string{"test": "label"},
	}

	job := RenderPlan(plan)

	if job == nil {
		t.Fatal("RenderPlan returned nil")
	}
	if job.Name != "test-job-abc123" {
		t.Errorf("Job.Name = %q, want test-job-abc123", job.Name)
	}
	if job.Namespace != "test-ns" {
		t.Errorf("Job.Namespace = %q, want test-ns", job.Namespace)
	}
	if *job.Spec.ActiveDeadlineSeconds != 300 {
		t.Errorf("ActiveDeadlineSeconds = %d, want 300", *job.Spec.ActiveDeadlineSeconds)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit = %d, want 0", *job.Spec.BackoffLimit)
	}

	// Verify int32Ptr and int64Ptr usage
	if job.Spec.BackoffLimit == nil {
		t.Error("BackoffLimit should not be nil (tests int32Ptr)")
	}
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Error("ActiveDeadlineSeconds should not be nil (tests int64Ptr)")
	}

	// Verify preferCPUNodeAffinity
	if job.Spec.Template.Spec.Affinity == nil {
		t.Error("Affinity should not be nil (tests preferCPUNodeAffinity)")
	}
	if job.Spec.Template.Spec.Affinity.NodeAffinity == nil {
		t.Error("NodeAffinity should not be nil")
	}

	// Verify container
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("Expected 1 container, got %d", len(job.Spec.Template.Spec.Containers))
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Name != "validator" {
		t.Errorf("Container.Name = %q, want validator", container.Name)
	}
	if container.Image != "test-image:v1.0.0" {
		t.Errorf("Container.Image = %q, want test-image:v1.0.0", container.Image)
	}

	// Verify ImagePullPolicy is set
	if container.ImagePullPolicy == "" {
		t.Error("ImagePullPolicy should be set")
	}

	// Verify ImagePullSecrets
	if len(job.Spec.Template.Spec.ImagePullSecrets) != 1 {
		t.Errorf("Expected 1 ImagePullSecret, got %d", len(job.Spec.Template.Spec.ImagePullSecrets))
	}
}

func TestRenderPlanToApplyConfig(t *testing.T) {
	plan := JobPlan{
		ValidatorName:    "test-validator",
		Phase:            "deployment",
		JobName:          "test-job-xyz789",
		Namespace:        "apply-ns",
		Image:            "test-image:v2.0.0",
		Args:             []string{"--apply-test"},
		Env:              []corev1.EnvVar{{Name: "TEST", Value: "apply-value"}},
		Volumes:          []corev1.Volume{{Name: "snapshot"}},
		VolumeMounts:     []corev1.VolumeMount{{Name: "snapshot", MountPath: "/data"}},
		Resources:        corev1.ResourceRequirements{},
		Timeout:          600,
		ServiceAccount:   "apply-sa",
		Tolerations:      []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		ImagePullSecrets: []string{"apply-secret"},
		Labels:           map[string]string{"apply": "test"},
	}

	jobApply := RenderPlanToApplyConfig(plan, "apply-job-name")

	if jobApply == nil {
		t.Fatal("RenderPlanToApplyConfig returned nil")
	}
	if *jobApply.Name != "apply-job-name" {
		t.Errorf("Job name = %q, want apply-job-name", *jobApply.Name)
	}
	if *jobApply.Namespace != "apply-ns" {
		t.Errorf("Job namespace = %q, want apply-ns", *jobApply.Namespace)
	}
	if jobApply.Labels["apply"] != "test" {
		t.Errorf("Job label apply = %q, want test", jobApply.Labels["apply"])
	}
	if jobApply.Spec == nil {
		t.Fatal("Job spec is nil")
	}
	if *jobApply.Spec.ActiveDeadlineSeconds != 600 {
		t.Errorf("ActiveDeadlineSeconds = %d, want 600", *jobApply.Spec.ActiveDeadlineSeconds)
	}
	if *jobApply.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit = %d, want 0", *jobApply.Spec.BackoffLimit)
	}
}

func TestRenderPlanToApplyConfig_EnvAndVolumeTypes(t *testing.T) {
	// Test plan with various env var sources and volume types
	plan := JobPlan{
		ValidatorName: "test-validator",
		Phase:         "deployment",
		JobName:       "test-job",
		Namespace:     "test-ns",
		Image:         "test:latest",
		Env: []corev1.EnvVar{
			{Name: "PLAIN", Value: "value"},
			{Name: "FROM_FIELD", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			}},
			{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
					Key:                  "password",
				},
			}},
			{Name: "FROM_CONFIGMAP", ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "my-config"},
					Key:                  "config.yaml",
				},
			}},
			{Name: "FROM_RESOURCE", ValueFrom: &corev1.EnvVarSource{
				ResourceFieldRef: &corev1.ResourceFieldSelector{
					ContainerName: "validator",
					Resource:      "limits.memory",
				},
			}},
		},
		Volumes: []corev1.Volume{
			{
				Name: "configmap-vol",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "cm"},
					},
				},
			},
			{
				Name: "secret-vol",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: "secret"},
				},
			},
			{
				Name: "emptydir-vol",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
			{
				Name: "hostpath-vol",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/host/path"},
				},
			},
			{
				Name: "pvc-vol",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pvc"},
				},
			},
		},
		VolumeMounts:     []corev1.VolumeMount{{Name: "configmap-vol", MountPath: "/data"}},
		Resources:        corev1.ResourceRequirements{},
		Timeout:          300,
		ServiceAccount:   "sa",
		Tolerations:      []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		ImagePullSecrets: []string{"secret"},
		Labels:           map[string]string{"test": "true"},
	}

	jobApply := RenderPlanToApplyConfig(plan, "test-job")

	if jobApply == nil {
		t.Fatal("RenderPlanToApplyConfig returned nil")
	}

	// Verify env vars are present
	podSpec := jobApply.Spec.Template.Spec
	if podSpec == nil {
		t.Fatal("PodSpec is nil")
	}
	if len(podSpec.Containers) == 0 {
		t.Fatal("No containers in pod spec")
	}
	container := podSpec.Containers[0]
	if len(container.Env) != 5 {
		t.Errorf("Expected 5 env vars, got %d", len(container.Env))
	}

	// Verify all env var types are handled
	envMap := make(map[string]*string)
	for _, env := range container.Env {
		if env.Name != nil {
			envMap[*env.Name] = env.Value
		}
	}
	if _, ok := envMap["PLAIN"]; !ok {
		t.Error("Plain value env var not found")
	}
	if _, ok := envMap["FROM_FIELD"]; !ok {
		t.Error("FieldRef env var not found")
	}
	if _, ok := envMap["FROM_SECRET"]; !ok {
		t.Error("SecretKeyRef env var not found")
	}
	if _, ok := envMap["FROM_CONFIGMAP"]; !ok {
		t.Error("ConfigMapKeyRef env var not found")
	}
	if _, ok := envMap["FROM_RESOURCE"]; !ok {
		t.Error("ResourceFieldRef env var not found")
	}

	// Verify all volume types are handled
	if len(podSpec.Volumes) != 5 {
		t.Errorf("Expected 5 volumes, got %d", len(podSpec.Volumes))
	}

	volumeMap := make(map[string]bool)
	for _, vol := range podSpec.Volumes {
		if vol.Name != nil {
			volumeMap[*vol.Name] = true
		}
	}
	expectedVolumes := []string{"configmap-vol", "secret-vol", "emptydir-vol", "hostpath-vol", "pvc-vol"}
	for _, name := range expectedVolumes {
		if !volumeMap[name] {
			t.Errorf("Volume %s not found in rendered spec", name)
		}
	}
}

func TestPlan(t *testing.T) {
	// Create a test catalog with validators for different phases
	cat := &ValidatorCatalog{
		Validators: []ValidatorEntry{
			{
				Name:        "operator-health",
				Phase:       "deployment",
				Image:       "test-image:v1.0.0",
				Timeout:     5 * time.Minute,
				Description: "Test deployment validator",
			},
			{
				Name:        "perf-test",
				Phase:       "performance",
				Image:       "perf-image:v1.0.0",
				Timeout:     10 * time.Minute,
				Description: "Test performance validator",
			},
		},
	}

	// Create validation input with deployment checks
	validationInput := &ValidationInput{
		Config: ValidationConfig{
			Deployment: &ValidationPhase{
				Checks: []string{"operator-health"},
			},
		},
	}

	// Generate plans
	plans, err := Plan(cat, validationInput, "test-run-123", "test-ns", "1.0.0", "abc123",
		"test-service-account", []string{"my-secret"}, nil, nil, "", "", nil)
	if err != nil {
		t.Fatalf("Plan() returned unexpected error: %v", err)
	}

	// Should have exactly one plan for deployment phase (operator-health)
	if len(plans) != 1 {
		t.Fatalf("Plan() returned %d plans, want 1", len(plans))
	}

	// Verify the plan
	plan := plans[0]
	if plan.ValidatorName != "operator-health" {
		t.Errorf("Plan.ValidatorName = %q, want operator-health", plan.ValidatorName)
	}
	if plan.Phase != "deployment" {
		t.Errorf("Plan.Phase = %q, want deployment", plan.Phase)
	}
	if plan.Image != "test-image:v1.0.0" {
		t.Errorf("Plan.Image = %q, want test-image:v1.0.0", plan.Image)
	}
	if len(plan.Env) == 0 {
		t.Error("Plan has no environment variables")
	}
	if len(plan.Volumes) != 2 {
		t.Errorf("Plan has %d volumes, want 2", len(plan.Volumes))
	}
	if len(plan.VolumeMounts) != 2 {
		t.Errorf("Plan has %d volume mounts, want 2", len(plan.VolumeMounts))
	}
	if plan.ServiceAccount != "test-service-account" {
		t.Errorf("Plan.ServiceAccount = %q, want test-service-account", plan.ServiceAccount)
	}
}

func TestPlanMultiplePhases(t *testing.T) {
	// Create a catalog with validators in multiple phases
	cat := &ValidatorCatalog{
		Validators: []ValidatorEntry{
			{Name: "deploy-1", Phase: "deployment", Image: "img1:v1"},
			{Name: "deploy-2", Phase: "deployment", Image: "img2:v1"},
			{Name: "perf-1", Phase: "performance", Image: "img3:v1"},
		},
	}

	// Request all phases
	validationInput := &ValidationInput{
		Config: ValidationConfig{
			Deployment: &ValidationPhase{
				Checks: []string{"deploy-1", "deploy-2"},
			},
			Performance: &ValidationPhase{
				Checks: []string{"perf-1"},
			},
		},
	}

	plans, err := Plan(cat, validationInput, "run-1", "ns", "1.0", "abc", "sa", nil, nil, nil, "", "", nil)
	if err != nil {
		t.Fatalf("Plan() returned unexpected error: %v", err)
	}

	// Should have 3 plans total (2 deployment + 1 performance)
	if len(plans) != 3 {
		t.Errorf("Plan() returned %d plans, want 3", len(plans))
	}

	// Count by phase
	deployCount := 0
	perfCount := 0
	for _, p := range plans {
		switch p.Phase {
		case "deployment":
			deployCount++
		case "performance":
			perfCount++
		}
	}
	if deployCount != 2 {
		t.Errorf("Got %d deployment plans, want 2", deployCount)
	}
	if perfCount != 1 {
		t.Errorf("Got %d performance plans, want 1", perfCount)
	}
}

func TestBuildJobPlan_PropagatesDependencyAffinity(t *testing.T) {
	entry := ValidatorEntry{
		Name:        "ai-service-metrics",
		Phase:       "conformance",
		Description: "x",
		Image:       "ghcr.io/x:latest",
		Timeout:     5 * time.Minute,
		DependencyAffinity: []DependencyAffinity{{
			ComponentRef:     "kube-prometheus-stack",
			PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
			Requirement:      DependencyRequirementRequired,
		}},
	}
	refs := []recipe.ComponentRef{
		{Name: "kube-prometheus-stack", Namespace: "monitoring"},
	}

	plan, err := BuildJobPlan(entry, "run-1", "ns", "", "", "sa", nil, nil, nil, "", "", refs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if plan.Affinity == nil || plan.Affinity.PodAffinity == nil {
		t.Fatal("expected PodAffinity on plan.Affinity")
	}
	if len(plan.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Errorf("expected 1 required term, got %d",
			len(plan.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution))
	}
	term := plan.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if len(term.Namespaces) != 1 || term.Namespaces[0] != "monitoring" {
		t.Errorf("expected term.Namespaces = [monitoring], got %v", term.Namespaces)
	}
	if term.LabelSelector == nil || term.LabelSelector.MatchLabels["app.kubernetes.io/name"] != "prometheus" {
		t.Errorf("expected app.kubernetes.io/name=prometheus selector, got %+v", term.LabelSelector)
	}
}

func TestBuildJobPlan_RequiredMissingReturnsError(t *testing.T) {
	entry := ValidatorEntry{
		Name:        "ai-service-metrics",
		Phase:       "conformance",
		Description: "x",
		Image:       "ghcr.io/x:latest",
		Timeout:     5 * time.Minute,
		DependencyAffinity: []DependencyAffinity{{
			ComponentRef:     "kube-prometheus-stack",
			PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
			Requirement:      DependencyRequirementRequired,
		}},
	}

	_, err := BuildJobPlan(entry, "run-1", "ns", "", "", "sa", nil, nil, nil, "", "", nil)
	if err == nil {
		t.Fatal("expected error when required component is missing")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}

func TestBuildJobPlan_NilComponentRefsBackwardCompat(t *testing.T) {
	entry := ValidatorEntry{
		Name:        "operator-health",
		Phase:       "deployment",
		Description: "x",
		Image:       "ghcr.io/x:latest",
		Timeout:     2 * time.Minute,
	}
	plan, err := BuildJobPlan(entry, "run-1", "ns", "", "", "sa", nil, nil, nil, "", "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if plan.Affinity == nil || plan.Affinity.NodeAffinity == nil {
		t.Fatal("backward compat: prefer-CPU NodeAffinity must remain on every plan")
	}
	if plan.Affinity.PodAffinity != nil {
		t.Errorf("expected nil PodAffinity for entry with no DependencyAffinity")
	}
}

func TestAffinityToApplyConfig_RoundTripsAllFields(t *testing.T) {
	cpuKey := "cpu"
	in := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
						},
						MatchFields: []corev1.NodeSelectorRequirement{
							{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a"}},
						},
					},
				},
			},
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
				{
					Weight: 50,
					Preference: corev1.NodeSelectorTerm{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: cpuKey, Operator: corev1.NodeSelectorOpExists},
						},
					},
				},
			},
		},
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "prom"},
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"backend"}},
						},
					},
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "obs"}},
					Namespaces:        []string{"monitoring"},
					TopologyKey:       "kubernetes.io/hostname",
				},
			},
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 90,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "dcgm"}},
						Namespaces:    []string{"gpu-operator"},
						TopologyKey:   "kubernetes.io/hostname",
					},
				},
			},
		},
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 10,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "noisy"}},
						TopologyKey:   "kubernetes.io/hostname",
					},
				},
			},
		},
	}

	got := affinityToApplyConfig(in)
	if got == nil {
		t.Fatal("expected non-nil apply config")
	}
	if got.NodeAffinity == nil {
		t.Fatal("NodeAffinity missing")
	}
	if got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Error("NodeAffinity.Required dropped")
	}
	if len(got.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Errorf("expected 1 preferred node term, got %d", len(got.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution))
	}
	if got.PodAffinity == nil {
		t.Fatal("PodAffinity missing")
	}
	if len(got.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Errorf("expected 1 required pod term, got %d", len(got.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution))
	}
	req := got.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if req.LabelSelector == nil {
		t.Fatal("PodAffinity required term LabelSelector dropped")
	}
	if len(req.LabelSelector.MatchExpressions) != 1 {
		t.Error("PodAffinity required term LabelSelector.MatchExpressions dropped")
	}
	if req.NamespaceSelector == nil {
		t.Error("PodAffinity required term NamespaceSelector dropped")
	}
	if got.PodAntiAffinity == nil {
		t.Fatal("PodAntiAffinity dropped entirely")
	}
}
