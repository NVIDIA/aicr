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

package job

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/NVIDIA/aicr/pkg/api/validator/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func testEntry() catalog.ValidatorEntry {
	return catalog.ValidatorEntry{
		Name:    "gpu-operator-health",
		Phase:   "deployment",
		Image:   "ghcr.io/nvidia/aicr-validators/gpu-operator:v1.0.0",
		Timeout: 2 * time.Minute,
		Args:    []string{"--verbose"},
		Env: []catalog.EnvVar{
			{Name: "CUSTOM_VAR", Value: "custom_value"},
		},
	}
}

// deployAndGet deploys a Job via SSA and returns the server-created Job object.
func deployAndGet(t *testing.T, d *Deployer) *batchv1.Job {
	t.Helper()
	ctx := context.Background()
	if err := d.DeployJob(ctx); err != nil {
		t.Fatalf("DeployJob() failed: %v", err)
	}
	job, err := testClientset.BatchV1().Jobs(d.namespace).Get(ctx, d.JobName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Job not found after deploy: %v", err)
	}
	return job
}

func TestJobNameEmptyBeforeDeploy(t *testing.T) {
	d := NewDeployer(nil, nil, "default", "run123", "", "", testEntry(), nil, nil, nil, "", "", nil)
	if d.JobName() != "" {
		t.Errorf("JobName() before deploy = %q, want empty", d.JobName())
	}
}

func TestGenerateJobName(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil)
	job := deployAndGet(t, d)

	if !strings.HasPrefix(job.Name, "aicr-gpu-operator-health-") {
		t.Errorf("Job name = %q, should have prefix %q", job.Name, "aicr-gpu-operator-health-")
	}
	if job.Name != d.JobName() {
		t.Errorf("Deployer.JobName() = %q, Job.Name = %q — mismatch", d.JobName(), job.Name)
	}
}

func TestDeployJobUniqueNames(t *testing.T) {
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	d1 := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil)
	d2 := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil)

	if err := d1.DeployJob(ctx); err != nil {
		t.Fatalf("first DeployJob() failed: %v", err)
	}
	if err := d2.DeployJob(ctx); err != nil {
		t.Fatalf("second DeployJob() failed: %v", err)
	}

	if d1.JobName() == d2.JobName() {
		t.Errorf("generateName should produce unique names, both got %q", d1.JobName())
	}
}

func TestDeployJobSSAFieldManager(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	found := false
	for _, ref := range job.ManagedFields {
		if ref.Manager == "aicr" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Job should have field manager 'aicr'")
	}
}

func TestDeployJobLabels(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	expectedLabels := map[string]string{
		"app.kubernetes.io/name":       "aicr",
		"app.kubernetes.io/component":  "validation",
		"app.kubernetes.io/managed-by": "aicr",
		"aicr.nvidia.com/job-type":     "validation",
		"aicr.nvidia.com/run-id":       "run1",
		"aicr.nvidia.com/validator":    "gpu-operator-health",
		"aicr.nvidia.com/phase":        "deployment",
	}
	for k, want := range expectedLabels {
		got := job.Labels[k]
		if got != want {
			t.Errorf("Job label %q = %q, want %q", k, got, want)
		}
	}

	// Pod template labels
	podLabels := job.Spec.Template.Labels
	if podLabels["aicr.nvidia.com/validator"] != "gpu-operator-health" {
		t.Errorf("Pod label validator = %q, want %q", podLabels["aicr.nvidia.com/validator"], "gpu-operator-health")
	}
}

func TestDeployJobTimeouts(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 120 {
		t.Errorf("ActiveDeadlineSeconds = %v, want 120", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}

	expectedTTL := int32(defaults.JobTTLAfterFinished.Seconds())
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != expectedTTL {
		t.Errorf("TTLSecondsAfterFinished = %v, want %d", job.Spec.TTLSecondsAfterFinished, expectedTTL)
	}

	expectedGrace := int64(defaults.ValidatorTerminationGracePeriod.Seconds())
	podSpec := job.Spec.Template.Spec
	if podSpec.TerminationGracePeriodSeconds == nil || *podSpec.TerminationGracePeriodSeconds != expectedGrace {
		t.Errorf("TerminationGracePeriodSeconds = %v, want %d", podSpec.TerminationGracePeriodSeconds, expectedGrace)
	}
}

func TestDeployJobDefaultTimeout(t *testing.T) {
	ns := createUniqueNamespace(t)
	entry := testEntry()
	entry.Timeout = 0
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", entry, nil, nil, nil, "", "", nil))

	expected := int64(defaults.ValidatorDefaultTimeout.Seconds())
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != expected {
		t.Errorf("ActiveDeadlineSeconds = %v, want %d (default)", job.Spec.ActiveDeadlineSeconds, expected)
	}
}

func TestDeployJobContainer(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("containers count = %d, want 1", len(containers))
	}

	c := containers[0]
	if c.Name != "validator" {
		t.Errorf("container name = %q, want %q", c.Name, "validator")
	}
	if c.Image != "ghcr.io/nvidia/aicr-validators/gpu-operator:v1.0.0" {
		t.Errorf("container image = %q", c.Image)
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("ImagePullPolicy = %q, want %q", c.ImagePullPolicy, corev1.PullIfNotPresent)
	}
	if len(c.Args) != 1 || c.Args[0] != "--verbose" {
		t.Errorf("container args = %v, want [--verbose]", c.Args)
	}
	if c.TerminationMessagePath != "/dev/termination-log" {
		t.Errorf("TerminationMessagePath = %q", c.TerminationMessagePath)
	}
	if c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Errorf("TerminationMessagePolicy = %q", c.TerminationMessagePolicy)
	}
}

func TestDeployJobResources(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	c := job.Spec.Template.Spec.Containers[0]
	if c.Resources.Requests.Cpu().String() != "1" {
		t.Errorf("CPU request = %q, want %q", c.Resources.Requests.Cpu().String(), "1")
	}
	if c.Resources.Requests.Memory().String() != "1Gi" {
		t.Errorf("Memory request = %q, want %q", c.Resources.Requests.Memory().String(), "1Gi")
	}
	if c.Resources.Limits.Cpu().String() != "1" {
		t.Errorf("CPU limit = %q, want %q", c.Resources.Limits.Cpu().String(), "1")
	}
	if c.Resources.Limits.Memory().String() != "1Gi" {
		t.Errorf("Memory limit = %q, want %q", c.Resources.Limits.Memory().String(), "1Gi")
	}
}

func TestDeployJobEnvVars(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	env := job.Spec.Template.Spec.Containers[0].Env
	envMap := make(map[string]corev1.EnvVar)
	for _, e := range env {
		envMap[e.Name] = e
	}

	if envMap["AICR_SNAPSHOT_PATH"].Value != "/data/snapshot/snapshot.yaml" {
		t.Errorf("AICR_SNAPSHOT_PATH = %q", envMap["AICR_SNAPSHOT_PATH"].Value)
	}
	if envMap["AICR_VALIDATION_PATH"].Value != "/data/validation/validation.yaml" {
		t.Errorf("AICR_VALIDATION_PATH = %q", envMap["AICR_VALIDATION_PATH"].Value)
	}
	if envMap["AICR_VALIDATOR_NAME"].Value != "gpu-operator-health" {
		t.Errorf("AICR_VALIDATOR_NAME = %q", envMap["AICR_VALIDATOR_NAME"].Value)
	}
	if envMap["AICR_VALIDATOR_PHASE"].Value != "deployment" {
		t.Errorf("AICR_VALIDATOR_PHASE = %q", envMap["AICR_VALIDATOR_PHASE"].Value)
	}
	if envMap["AICR_RUN_ID"].Value != "run1" {
		t.Errorf("AICR_RUN_ID = %q", envMap["AICR_RUN_ID"].Value)
	}

	nsEnv := envMap["AICR_NAMESPACE"]
	if nsEnv.ValueFrom == nil || nsEnv.ValueFrom.FieldRef == nil || nsEnv.ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
		t.Error("AICR_NAMESPACE should use downward API metadata.namespace")
	}

	// AICR_CHECK_TIMEOUT propagates the entry's catalog-level timeout to
	// validators.checkTimeoutFromEnv so the inner parent context matches
	// the Job's ActiveDeadlineSeconds. Value is time.Duration.String().
	timeoutEnv, ok := envMap["AICR_CHECK_TIMEOUT"]
	if !ok {
		t.Error("AICR_CHECK_TIMEOUT must be injected")
	} else if got, want := timeoutEnv.Value, testEntry().Timeout.String(); got != want {
		t.Errorf("AICR_CHECK_TIMEOUT = %q, want %q", got, want)
	}

	if envMap["CUSTOM_VAR"].Value != "custom_value" {
		t.Errorf("CUSTOM_VAR = %q, want %q", envMap["CUSTOM_VAR"].Value, "custom_value")
	}

	// Empty cliVersion — AICR_CLI_VERSION env var should not be injected.
	if _, ok := envMap["AICR_CLI_VERSION"]; ok {
		t.Errorf("AICR_CLI_VERSION should not be set when cliVersion is empty; got %q",
			envMap["AICR_CLI_VERSION"].Value)
	}

	// Empty cliCommit — AICR_CLI_COMMIT env var should not be injected.
	if _, ok := envMap["AICR_CLI_COMMIT"]; ok {
		t.Errorf("AICR_CLI_COMMIT should not be set when cliCommit is empty; got %q",
			envMap["AICR_CLI_COMMIT"].Value)
	}
}

// TestDeployJobCLIVersionInjected exercises the production code path where
// validator.go passes v.Version (non-empty) so the inner validator container
// can forward the CLI version to child workloads (e.g., resolveAiperfImage).
// A regression in this injection would otherwise leave the other tests green
// because they all pass "" for cliVersion.
func TestDeployJobCLIVersionInjected(t *testing.T) {
	ns := createUniqueNamespace(t)
	const wantVersion = "v0.11.1-test"
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", wantVersion, "", testEntry(), nil, nil, nil, "", "", nil))

	env := job.Spec.Template.Spec.Containers[0].Env
	var got *corev1.EnvVar
	for i := range env {
		if env[i].Name == "AICR_CLI_VERSION" {
			got = &env[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("AICR_CLI_VERSION not found in env; have %d vars", len(env))
	}
	if got.Value != wantVersion {
		t.Errorf("AICR_CLI_VERSION = %q, want %q", got.Value, wantVersion)
	}
}

// TestDeployJobImageTagOverrideForwarding asserts that AICR_VALIDATOR_IMAGE_TAG
// is forwarded from the constructor parameter into the validator container's env
// ONLY when non-empty, and that it is strictly omitted when empty. Forwarding is
// load-bearing for feature-branch dogfooding: validators that resolve inner
// workload images at runtime (e.g. inference-perf's aiperf-bench Job) call
// catalog.ResolveImage with the pod's env. Without this forwarding the outer
// validator would get :latest while the inner benchmark pod would still
// resolve to the unpublished :sha-<commit> and ImagePullBackOff. Omission
// on the default paths is equally load-bearing — the release / main-branch
// flows must not inadvertently pin inner pods to the wrong tag.
func TestDeployJobImageTagOverrideForwarding(t *testing.T) {
	tests := []struct {
		name        string
		tagOverride string
		wantSet     bool
		wantValue   string
	}{
		{name: "set — forwarded into validator container", tagOverride: "latest", wantSet: true, wantValue: "latest"},
		{name: "empty — omitted (default release / main-branch paths untouched)", tagOverride: "", wantSet: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := createUniqueNamespace(t)
			job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", tt.tagOverride, nil))

			env := job.Spec.Template.Spec.Containers[0].Env
			var got *corev1.EnvVar
			for i := range env {
				if env[i].Name == "AICR_VALIDATOR_IMAGE_TAG" {
					got = &env[i]
					break
				}
			}

			if tt.wantSet {
				if got == nil {
					t.Fatalf("AICR_VALIDATOR_IMAGE_TAG not found in env; have %d vars", len(env))
				}
				if got.Value != tt.wantValue {
					t.Errorf("AICR_VALIDATOR_IMAGE_TAG = %q, want %q", got.Value, tt.wantValue)
				}
				return
			}

			if got != nil {
				t.Errorf("AICR_VALIDATOR_IMAGE_TAG should be omitted when override is empty; got %q", got.Value)
			}
		})
	}
}

// TestDeployJobImageRegistryOverrideForwarding asserts that
// AICR_VALIDATOR_IMAGE_REGISTRY is forwarded from the constructor parameter
// into the validator container's env ONLY when non-empty, and omitted when empty.
func TestDeployJobImageRegistryOverrideForwarding(t *testing.T) {
	tests := []struct {
		name             string
		registryOverride string
		wantSet          bool
		wantValue        string
	}{
		{name: "set — forwarded into validator container", registryOverride: "localhost:5001", wantSet: true, wantValue: "localhost:5001"},
		{name: "empty — omitted", registryOverride: "", wantSet: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := createUniqueNamespace(t)
			job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, tt.registryOverride, "", nil))

			env := job.Spec.Template.Spec.Containers[0].Env
			var got *corev1.EnvVar
			for i := range env {
				if env[i].Name == "AICR_VALIDATOR_IMAGE_REGISTRY" {
					got = &env[i]
					break
				}
			}

			if tt.wantSet {
				if got == nil {
					t.Fatalf("AICR_VALIDATOR_IMAGE_REGISTRY not found in env; have %d vars", len(env))
				}
				if got.Value != tt.wantValue {
					t.Errorf("AICR_VALIDATOR_IMAGE_REGISTRY = %q, want %q", got.Value, tt.wantValue)
				}
				return
			}

			if got != nil {
				t.Errorf("AICR_VALIDATOR_IMAGE_REGISTRY should be omitted when override is empty; got %q", got.Value)
			}
		})
	}
}

// TestDeployJobCLICommitInjected exercises the production code path where
// validator.go passes v.Commit (non-empty) so the inner validator container
// can resolve SHA-based image tags in dev builds via AICR_CLI_COMMIT.
func TestDeployJobCLICommitInjected(t *testing.T) {
	ns := createUniqueNamespace(t)
	const wantCommit = "abc1234"
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", wantCommit, testEntry(), nil, nil, nil, "", "", nil))

	env := job.Spec.Template.Spec.Containers[0].Env
	var got *corev1.EnvVar
	for i := range env {
		if env[i].Name == "AICR_CLI_COMMIT" {
			got = &env[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("AICR_CLI_COMMIT not found in env; have %d vars", len(env))
	}
	if got.Value != wantCommit {
		t.Errorf("AICR_CLI_COMMIT = %q, want %q", got.Value, wantCommit)
	}
}

func TestDeployJobVolumes(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	volumes := job.Spec.Template.Spec.Volumes
	if len(volumes) != 2 {
		t.Fatalf("volumes count = %d, want 2", len(volumes))
	}

	if volumes[0].Name != "snapshot" || volumes[0].ConfigMap.Name != "aicr-snapshot-run1" {
		t.Errorf("snapshot volume = %v", volumes[0])
	}
	if volumes[1].Name != "validation" || volumes[1].ConfigMap.Name != "aicr-validation-run1" {
		t.Errorf("validation volume = %v", volumes[1])
	}

	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts
	if len(mounts) != 2 {
		t.Fatalf("volumeMounts count = %d, want 2", len(mounts))
	}
	if !mounts[0].ReadOnly || !mounts[1].ReadOnly {
		t.Error("volume mounts should be read-only")
	}
}

func TestDeployJobAffinity(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	affinity := job.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil {
		t.Fatal("affinity should be set")
	}

	prefs := affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(prefs) != 1 || prefs[0].Weight != 100 {
		t.Fatalf("preferred scheduling = %v, want weight=100", prefs)
	}

	exprs := prefs[0].Preference.MatchExpressions
	if len(exprs) != 1 || exprs[0].Key != "nvidia.com/gpu.present" {
		t.Errorf("affinity key = %v, want nvidia.com/gpu.present", exprs)
	}
	if exprs[0].Operator != corev1.NodeSelectorOpDoesNotExist {
		t.Errorf("affinity operator = %q, want %q", exprs[0].Operator, corev1.NodeSelectorOpDoesNotExist)
	}
}

func TestDeployJobImagePullSecrets(t *testing.T) {
	ns := createUniqueNamespace(t)
	secrets := []string{"registry-creds", "other-secret"}
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), secrets, nil, nil, "", "", nil))

	ips := job.Spec.Template.Spec.ImagePullSecrets
	if len(ips) != 2 {
		t.Fatalf("imagePullSecrets count = %d, want 2", len(ips))
	}
	if ips[0].Name != "registry-creds" {
		t.Errorf("imagePullSecrets[0] = %q, want %q", ips[0].Name, "registry-creds")
	}
}

func TestDeployJobOrchestratorToleratesTolerateAll(t *testing.T) {
	// The orchestrator Job must always have tolerate-all so it can schedule on
	// any CPU node, regardless of what tolerations are passed for inner workloads.
	tests := []struct {
		name        string
		tolerations []corev1.Toleration
	}{
		{"nil tolerations", nil},
		{"narrow GPU toleration", []corev1.Toleration{{Key: "gpu-type", Value: "h100", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual}}},
		{"explicit tolerate-all", []corev1.Toleration{{Operator: corev1.TolerationOpExists}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := createUniqueNamespace(t)
			job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, tt.tolerations, nil, "", "", nil))
			tols := job.Spec.Template.Spec.Tolerations
			if len(tols) != 1 || tols[0].Operator != corev1.TolerationOpExists || tols[0].Key != "" {
				t.Errorf("orchestrator tolerations = %v, want single tolerate-all {Operator: Exists}", tols)
			}
		})
	}
}

func TestDeployJobNodeSelectorEnvVar(t *testing.T) {
	ns := createUniqueNamespace(t)
	// Use a single-key selector to avoid map ordering issues in serialization.
	nodeSelector := map[string]string{"my-org/gpu-pool": "true"}
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nodeSelector, "", "", nil))

	env := job.Spec.Template.Spec.Containers[0].Env
	envMap := make(map[string]corev1.EnvVar)
	for _, e := range env {
		envMap[e.Name] = e
	}

	// AICR_NODE_SELECTOR must be set so the validator container can apply it to inner workloads.
	if envMap["AICR_NODE_SELECTOR"].Value != "my-org/gpu-pool=true" {
		t.Errorf("AICR_NODE_SELECTOR = %q, want %q", envMap["AICR_NODE_SELECTOR"].Value, "my-org/gpu-pool=true")
	}

	// The orchestrator Job pod spec must NOT have a nodeSelector — scheduling of the
	// orchestrator is handled by preferCPUNodeAffinityApply(), not the user flag.
	if len(job.Spec.Template.Spec.NodeSelector) != 0 {
		t.Errorf("orchestrator pod spec nodeSelector should be empty, got %v", job.Spec.Template.Spec.NodeSelector)
	}
}

func TestDeployJobNodeSelectorEnvVarAbsent(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "AICR_NODE_SELECTOR" {
			t.Errorf("AICR_NODE_SELECTOR should be absent when nodeSelector is nil, got %q", e.Value)
		}
	}
}

func TestDeployJobTolerationsEnvVar(t *testing.T) {
	ns := createUniqueNamespace(t)
	tolerations := []corev1.Toleration{
		{Key: "gpu-type", Value: "h100", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual},
	}
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, tolerations, nil, "", "", nil))

	env := job.Spec.Template.Spec.Containers[0].Env
	envMap := make(map[string]corev1.EnvVar)
	for _, e := range env {
		envMap[e.Name] = e
	}

	// AICR_TOLERATIONS must be set so validators can apply it to inner workloads.
	if envMap["AICR_TOLERATIONS"].Value != "gpu-type=h100:NoSchedule" {
		t.Errorf("AICR_TOLERATIONS = %q, want %q", envMap["AICR_TOLERATIONS"].Value, "gpu-type=h100:NoSchedule")
	}
}

func TestDeployJobPodSpec(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil))

	podSpec := job.Spec.Template.Spec
	wantSA := ServiceAccountName("run1")
	if podSpec.ServiceAccountName != wantSA {
		t.Errorf("ServiceAccountName = %q, want %q", podSpec.ServiceAccountName, wantSA)
	}
	if podSpec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want %q", podSpec.RestartPolicy, corev1.RestartPolicyNever)
	}
}

func TestCleanupJob(t *testing.T) {
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil)
	if err := d.DeployJob(ctx); err != nil {
		t.Fatalf("DeployJob() failed: %v", err)
	}

	if err := d.CleanupJob(ctx); err != nil {
		t.Fatalf("CleanupJob() failed: %v", err)
	}

	// Foreground propagation sets deletionTimestamp; envtest has no GC controller
	// to finalize deletion, so verify the deletion was requested.
	job, err := testClientset.BatchV1().Jobs(ns).Get(ctx, d.JobName(), metav1.GetOptions{})
	if err != nil {
		return // already fully deleted
	}
	if job.DeletionTimestamp == nil {
		t.Error("Job should have a deletionTimestamp after CleanupJob")
	}
}

func TestCleanupJobNotFound(t *testing.T) {
	d := NewDeployer(testClientset, nil, "default", "run1", "", "", testEntry(), nil, nil, nil, "", "", nil)
	// jobName is empty — CleanupJob should return nil
	if err := d.CleanupJob(context.Background()); err != nil {
		t.Fatalf("CleanupJob() on empty jobName should not error, got: %v", err)
	}
}

// The following four tests previously exercised the in-package
// `checkJobTerminal` helper directly. The terminal-state logic now lives in
// `pod.WaitForJobTerminal` (pkg/k8s/pod). The tests are kept and adapted to
// validate the integration: they drive a fake clientset preloaded with a Job
// in the desired terminal-state fixture and assert that
// `pod.WaitForJobTerminal` returns the expected result.

func TestJobTerminal_Complete(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
				Reason: "Completed",
			}},
		},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(job)
	got, err := pod.WaitForJobTerminal(context.Background(), client, "default", "j", time.Second)
	if err != nil {
		t.Fatalf("expected nil error for Complete Job, got: %v", err)
	}
	if got == nil {
		t.Fatal("expected job to be returned")
	}
}

func TestJobTerminal_Failed(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobFailed,
				Status: corev1.ConditionTrue,
				Reason: "DeadlineExceeded",
			}},
		},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(job)
	got, err := pod.WaitForJobTerminal(context.Background(), client, "default", "j", time.Second)
	if err != nil {
		t.Fatalf("expected nil error for Failed Job (terminal helper treats Failed as terminal), got: %v", err)
	}
	if got == nil {
		t.Fatal("expected job to be returned")
	}
}

func TestJobTerminal_Running(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{}},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(job)
	_, err := pod.WaitForJobTerminal(context.Background(), client, "default", "j", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error for running Job")
	}
}

func TestJobTerminal_ConditionFalse(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionFalse,
			}},
		},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(job)
	_, err := pod.WaitForJobTerminal(context.Background(), client, "default", "j", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error when condition status is False (not terminal)")
	}
}

func TestWaitForCompletionFastPath(t *testing.T) {
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil)
	if err := d.DeployJob(ctx); err != nil {
		t.Fatalf("DeployJob() failed: %v", err)
	}

	// Mark Job as Complete — real API server requires full status.
	now := metav1.Now()
	job, _ := testClientset.BatchV1().Jobs(ns).Get(ctx, d.JobName(), metav1.GetOptions{})
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	// K8s 1.33+ requires SuccessCriteriaMet before Complete on Jobs with SuccessPolicy.
	// Set both conditions; if the API server rejects SuccessCriteriaMet (older K8s),
	// fall back to Complete only.
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	_, err := testClientset.BatchV1().Jobs(ns).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	if err != nil {
		// Older K8s may reject SuccessCriteriaMet — retry with Complete only.
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		_, err = testClientset.BatchV1().Jobs(ns).UpdateStatus(ctx, job, metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("failed to update Job status: %v", err)
		}
	}

	if err := d.WaitForCompletion(ctx, 1*time.Minute); err != nil {
		t.Fatalf("WaitForCompletion() failed: %v", err)
	}
}

func TestWaitForCompletionFastPathFailed(t *testing.T) {
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil)
	if err := d.DeployJob(ctx); err != nil {
		t.Fatalf("DeployJob() failed: %v", err)
	}

	// Mark Job as Failed — real API server requires FailureTarget + startTime.
	now := metav1.Now()
	job, _ := testClientset.BatchV1().Jobs(ns).Get(ctx, d.JobName(), metav1.GetOptions{})
	job.Status.StartTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", LastTransitionTime: now},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", LastTransitionTime: now},
	}
	_, err := testClientset.BatchV1().Jobs(ns).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Job status: %v", err)
	}

	if err := d.WaitForCompletion(ctx, 1*time.Minute); err != nil {
		t.Fatalf("WaitForCompletion() should return nil for Failed Job, got: %v", err)
	}
}

func TestWaitForCompletionJobNotFound(t *testing.T) {
	ns := createUniqueNamespace(t)

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil)
	d.jobName = "nonexistent-job"

	err := d.WaitForCompletion(context.Background(), 1*time.Minute)
	if err == nil {
		t.Fatal("expected error for nonexistent Job")
	}
}

func TestWaitForCompletionTimeout(t *testing.T) {
	ns := createUniqueNamespace(t)

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", testEntry(), nil, nil, nil, "", "", nil)
	if err := d.DeployJob(context.Background()); err != nil {
		t.Fatalf("DeployJob() failed: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := d.WaitForCompletion(canceledCtx, 1*time.Minute)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// TestWaitForPodTerminationPropagatesNonNotFound verifies that WaitForPodTermination
// only swallows NotFound errors from getPodForJob. Other failures (e.g. RBAC
// Forbidden) must propagate so the validator can decide retry/escalation
// instead of silently skipping the termination wait.
func TestWaitForPodTerminationPropagatesNonNotFound(t *testing.T) {
	t.Parallel()

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "*", stderrors.New("forbidden"))
	})

	d := &Deployer{
		clientset: cs,
		namespace: "default",
		jobName:   "test-job",
	}

	err := d.WaitForPodTermination(context.Background())
	if err == nil {
		t.Fatal("expected error to propagate (Forbidden), got nil")
	}
	var sErr *aicrerrors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *StructuredError, got %T", err)
	}
	if sErr.Code == aicrerrors.ErrCodeNotFound {
		t.Errorf("expected non-NotFound error code, got %v (Forbidden was swallowed!)", sErr.Code)
	}
}

func TestDeployer_BuildJobPlanRequiredMissing(t *testing.T) {
	entry := catalog.ValidatorEntry{
		Name:        "ai-service-metrics",
		Phase:       "conformance",
		Description: "x",
		Image:       "ghcr.io/x:latest",
		Timeout:     5 * time.Minute,
		DependencyAffinity: []v1.DependencyAffinity{{
			ComponentRef:     "kube-prometheus-stack",
			PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
			Requirement:      v1.DependencyRequirementRequired,
		}},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	cs := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(cs, 0)
	d := NewDeployer(cs, factory, "aicr-validation", "run-1", "", "", entry, nil, nil, nil, "", "", nil)

	err := d.DeployJob(context.Background())
	if err == nil {
		t.Fatal("expected error when required dependency component is missing")
	}
	var sErr *aicrerrors.StructuredError
	if !stderrors.As(err, &sErr) || sErr.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
	if !strings.Contains(sErr.Error(), "kube-prometheus-stack") {
		t.Errorf("error message should name the missing componentRef, got %v", sErr)
	}
}

func TestScanMissingPodAffinityDeps(t *testing.T) {
	prometheusPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-0",
			Namespace: "monitoring",
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
	}

	tests := []struct {
		name           string
		pa             *corev1.PodAffinity
		existingPods   []*corev1.Pod
		wantCount      int
		wantReason     string
		wantNamespace  string
		wantSelectorIn string // substring expected within Selector
	}{
		{
			name:      "nil PodAffinity returns nil",
			pa:        nil,
			wantCount: 0,
		},
		{
			name:      "empty terms returns nil",
			pa:        &corev1.PodAffinity{},
			wantCount: 0,
		},
		{
			name: "required term matched by existing pod returns no warning",
			pa: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"}},
					Namespaces:    []string{"monitoring"},
					TopologyKey:   "kubernetes.io/hostname",
				}},
			},
			existingPods: []*corev1.Pod{prometheusPod},
			wantCount:    0,
		},
		{
			name: "required term with no matching pods emits zero-match warning",
			pa: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"}},
					Namespaces:    []string{"monitoring"},
					TopologyKey:   "kubernetes.io/hostname",
				}},
			},
			wantCount:      1,
			wantReason:     affinityScanReasonZeroMatch,
			wantNamespace:  "monitoring",
			wantSelectorIn: "app.kubernetes.io/name=prometheus",
		},
		{
			name: "preferred term with no matching pods emits zero-match warning",
			pa: &corev1.PodAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"}},
						Namespaces:    []string{"monitoring"},
						TopologyKey:   "kubernetes.io/hostname",
					},
				}},
			},
			wantCount:      1,
			wantReason:     affinityScanReasonZeroMatch,
			wantNamespace:  "monitoring",
			wantSelectorIn: "app.kubernetes.io/name=prometheus",
		},
		{
			name: "mixed required+preferred where preferred has no match",
			pa: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"}},
					Namespaces:    []string{"monitoring"},
					TopologyKey:   "kubernetes.io/hostname",
				}},
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "dcgm-exporter"}},
						Namespaces:    []string{"gpu-operator"},
						TopologyKey:   "kubernetes.io/hostname",
					},
				}},
			},
			existingPods:   []*corev1.Pod{prometheusPod}, // matches required, not preferred
			wantCount:      1,
			wantReason:     affinityScanReasonZeroMatch,
			wantNamespace:  "gpu-operator",
			wantSelectorIn: "app=dcgm-exporter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tt.existingPods))
			for _, p := range tt.existingPods {
				objs = append(objs, p)
			}
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			cs := fake.NewSimpleClientset(objs...)
			got := scanMissingPodAffinityDeps(context.Background(), cs, tt.pa)
			if len(got) != tt.wantCount {
				t.Fatalf("expected %d warnings, got %d: %+v", tt.wantCount, len(got), got)
			}
			if tt.wantReason != "" {
				if got[0].Reason != tt.wantReason {
					t.Errorf("Reason = %q, want %q", got[0].Reason, tt.wantReason)
				}
				if tt.wantNamespace != "" && got[0].Namespace != tt.wantNamespace {
					t.Errorf("Namespace = %q, want %q", got[0].Namespace, tt.wantNamespace)
				}
				if tt.wantSelectorIn != "" && !strings.Contains(got[0].Selector, tt.wantSelectorIn) {
					t.Errorf("Selector = %q, want substring %q", got[0].Selector, tt.wantSelectorIn)
				}
				if got[0].Message == "" {
					t.Errorf("Message must be non-empty for emitted warning")
				}
			}
		})
	}
}

func TestScanMissingPodAffinityDeps_ListErrorReturnsWarning(t *testing.T) {
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver flaky")
	})
	pa := &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"}},
			Namespaces:    []string{"monitoring"},
			TopologyKey:   "kubernetes.io/hostname",
		}},
	}
	got := scanMissingPodAffinityDeps(context.Background(), cs, pa)
	if len(got) != 1 {
		t.Fatalf("expected 1 warning on List error, got %d: %+v", len(got), got)
	}
	if got[0].Reason != affinityScanReasonLookupFailed {
		t.Errorf("Reason = %q, want %q", got[0].Reason, affinityScanReasonLookupFailed)
	}
	if got[0].Err == nil {
		t.Errorf("Err must be set for lookup-failed reason")
	}
}

// TestScanMissingPodAffinityDeps_MalformedSelectorEmitsWarning verifies the
// LabelSelectorAsSelector path: a selector that doesn't parse (e.g., a
// MatchExpressions entry with an invalid operator) is reported as a
// malformed-selector warning instead of bypassing the diagnostic with
// FormatLabelSelector's "<error>" sentinel.
func TestScanMissingPodAffinityDeps_MalformedSelectorEmitsWarning(t *testing.T) {
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	cs := fake.NewSimpleClientset()
	pa := &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "app",
					Operator: metav1.LabelSelectorOperator("Bogus"), // invalid operator
					Values:   []string{"x"},
				}},
			},
			Namespaces:  []string{"monitoring"},
			TopologyKey: "kubernetes.io/hostname",
		}},
	}
	got := scanMissingPodAffinityDeps(context.Background(), cs, pa)
	if len(got) != 1 {
		t.Fatalf("expected 1 warning on malformed selector, got %d: %+v", len(got), got)
	}
	if got[0].Reason != affinityScanReasonMalformedSelector {
		t.Errorf("Reason = %q, want %q", got[0].Reason, affinityScanReasonMalformedSelector)
	}
	if got[0].Err == nil {
		t.Errorf("Err must be set for malformed-selector reason")
	}
}

// TestScanMissingPodAffinityDeps_StopsOnCancellation verifies the scan
// returns early once the parent ctx is canceled instead of emitting one
// "selector lookup failed; context canceled" warning per remaining
// (term, namespace) pair.
func TestScanMissingPodAffinityDeps_StopsOnCancellation(t *testing.T) {
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	cs := fake.NewSimpleClientset()
	pa := &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
			{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
				Namespaces:    []string{"ns-a-1", "ns-a-2"},
				TopologyKey:   "kubernetes.io/hostname",
			},
			{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}},
				Namespaces:    []string{"ns-b-1", "ns-b-2"},
				TopologyKey:   "kubernetes.io/hostname",
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the outer-loop guard trips before any List runs
	got := scanMissingPodAffinityDeps(ctx, cs, pa)
	if len(got) != 0 {
		t.Errorf("expected no warnings after pre-canceled ctx, got %d: %v", len(got), got)
	}
}
