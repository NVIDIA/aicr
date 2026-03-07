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

package job

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	d := NewDeployer(nil, nil, "default", "run123", testEntry(), nil, nil)
	if d.JobName() != "" {
		t.Errorf("JobName() before deploy = %q, want empty", d.JobName())
	}
}

func TestGenerateJobName(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil)
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

	d1 := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil)
	d2 := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil)

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
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil))

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
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil))

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
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil))

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
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", entry, nil, nil))

	expected := int64(defaults.ValidatorDefaultTimeout.Seconds())
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != expected {
		t.Errorf("ActiveDeadlineSeconds = %v, want %d (default)", job.Spec.ActiveDeadlineSeconds, expected)
	}
}

func TestDeployJobContainer(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil))

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
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil))

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
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil))

	env := job.Spec.Template.Spec.Containers[0].Env
	envMap := make(map[string]corev1.EnvVar)
	for _, e := range env {
		envMap[e.Name] = e
	}

	if envMap["AICR_SNAPSHOT_PATH"].Value != "/data/snapshot/snapshot.yaml" {
		t.Errorf("AICR_SNAPSHOT_PATH = %q", envMap["AICR_SNAPSHOT_PATH"].Value)
	}
	if envMap["AICR_RECIPE_PATH"].Value != "/data/recipe/recipe.yaml" {
		t.Errorf("AICR_RECIPE_PATH = %q", envMap["AICR_RECIPE_PATH"].Value)
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

	if envMap["CUSTOM_VAR"].Value != "custom_value" {
		t.Errorf("CUSTOM_VAR = %q, want %q", envMap["CUSTOM_VAR"].Value, "custom_value")
	}
}

func TestDeployJobVolumes(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil))

	volumes := job.Spec.Template.Spec.Volumes
	if len(volumes) != 2 {
		t.Fatalf("volumes count = %d, want 2", len(volumes))
	}

	if volumes[0].Name != "snapshot" || volumes[0].ConfigMap.Name != "aicr-snapshot-run1" {
		t.Errorf("snapshot volume = %v", volumes[0])
	}
	if volumes[1].Name != "recipe" || volumes[1].ConfigMap.Name != "aicr-recipe-run1" {
		t.Errorf("recipe volume = %v", volumes[1])
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
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil))

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
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), secrets, nil))

	ips := job.Spec.Template.Spec.ImagePullSecrets
	if len(ips) != 2 {
		t.Fatalf("imagePullSecrets count = %d, want 2", len(ips))
	}
	if ips[0].Name != "registry-creds" {
		t.Errorf("imagePullSecrets[0] = %q, want %q", ips[0].Name, "registry-creds")
	}
}

func TestDeployJobTolerations(t *testing.T) {
	ns := createUniqueNamespace(t)
	tolerations := []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, tolerations))

	tols := job.Spec.Template.Spec.Tolerations
	if len(tols) != 1 || tols[0].Operator != corev1.TolerationOpExists {
		t.Errorf("tolerations = %v, want tolerate-all", tols)
	}
}

func TestDeployJobPodSpec(t *testing.T) {
	ns := createUniqueNamespace(t)
	job := deployAndGet(t, NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil))

	podSpec := job.Spec.Template.Spec
	if podSpec.ServiceAccountName != ServiceAccountName {
		t.Errorf("ServiceAccountName = %q, want %q", podSpec.ServiceAccountName, ServiceAccountName)
	}
	if podSpec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want %q", podSpec.RestartPolicy, corev1.RestartPolicyNever)
	}
}

func TestCleanupJob(t *testing.T) {
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil)
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
	d := NewDeployer(testClientset, nil, "default", "run1", testEntry(), nil, nil)
	// jobName is empty — CleanupJob should return nil
	if err := d.CleanupJob(context.Background()); err != nil {
		t.Fatalf("CleanupJob() on empty jobName should not error, got: %v", err)
	}
}

func TestCheckJobTerminalComplete(t *testing.T) {
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
				Reason: "Completed",
			}},
		},
	}
	terminal, reason := checkJobTerminal(job)
	if !terminal {
		t.Error("expected terminal=true for Complete Job")
	}
	if reason != "Completed" {
		t.Errorf("reason = %q, want %q", reason, "Completed")
	}
}

func TestCheckJobTerminalFailed(t *testing.T) {
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobFailed,
				Status: corev1.ConditionTrue,
				Reason: "DeadlineExceeded",
			}},
		},
	}
	terminal, reason := checkJobTerminal(job)
	if !terminal {
		t.Error("expected terminal=true for Failed Job")
	}
	if reason != "DeadlineExceeded" {
		t.Errorf("reason = %q, want %q", reason, "DeadlineExceeded")
	}
}

func TestCheckJobTerminalRunning(t *testing.T) {
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{},
		},
	}
	terminal, _ := checkJobTerminal(job)
	if terminal {
		t.Error("expected terminal=false for running Job")
	}
}

func TestCheckJobTerminalConditionFalse(t *testing.T) {
	job := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionFalse,
			}},
		},
	}
	terminal, _ := checkJobTerminal(job)
	if terminal {
		t.Error("expected terminal=false when condition status is False")
	}
}

func TestWaitForCompletionFastPath(t *testing.T) {
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil)
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

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil)
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

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil)
	d.jobName = "nonexistent-job"

	err := d.WaitForCompletion(context.Background(), 1*time.Minute)
	if err == nil {
		t.Fatal("expected error for nonexistent Job")
	}
}

func TestWaitForCompletionTimeout(t *testing.T) {
	ns := createUniqueNamespace(t)

	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", testEntry(), nil, nil)
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
