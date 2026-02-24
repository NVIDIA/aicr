// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package agent

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildJobSpec(t *testing.T) {
	deployer, _ := createDeployer()

	job := deployer.buildJobSpec()

	// Verify Job metadata
	if job.Name != deployer.config.JobName {
		t.Errorf("expected Job name %q, got %q", deployer.config.JobName, job.Name)
	}
	if job.Namespace != deployer.config.Namespace {
		t.Errorf("expected namespace %q, got %q", deployer.config.Namespace, job.Namespace)
	}

	// Verify labels
	expectedLabels := map[string]string{
		"app.kubernetes.io/name":      "aicr",
		"app.kubernetes.io/component": "validation",
		"aicr.nvidia.com/job-type":    "validation",
	}
	for key, expectedValue := range expectedLabels {
		if value, ok := job.Labels[key]; !ok || value != expectedValue {
			t.Errorf("expected label %s=%q, got %q", key, expectedValue, value)
		}
	}

	// Verify pod labels
	expectedPodLabels := map[string]string{
		"app.kubernetes.io/name":      "aicr",
		"app.kubernetes.io/component": "validation",
		"aicr.nvidia.com/job":         deployer.config.JobName,
	}
	for key, expectedValue := range expectedPodLabels {
		if value, ok := job.Spec.Template.Labels[key]; !ok || value != expectedValue {
			t.Errorf("expected pod label %s=%q, got %q", key, expectedValue, value)
		}
	}

	// Verify backoffLimit
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("expected backoffLimit 0, got %v", job.Spec.BackoffLimit)
	}

	// Verify TTL
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("expected TTLSecondsAfterFinished 3600, got %v", job.Spec.TTLSecondsAfterFinished)
	}

	// Verify pod spec
	podSpec := job.Spec.Template.Spec
	if podSpec.ServiceAccountName != deployer.config.ServiceAccountName {
		t.Errorf("expected ServiceAccountName %q, got %q", deployer.config.ServiceAccountName, podSpec.ServiceAccountName)
	}
	if podSpec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("expected RestartPolicy Never, got %v", podSpec.RestartPolicy)
	}

	// Verify containers
	if len(podSpec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(podSpec.Containers))
	}
	container := podSpec.Containers[0]
	if container.Name != "validator" {
		t.Errorf("expected container name validator, got %q", container.Name)
	}
	if container.Image != deployer.config.Image {
		t.Errorf("expected image %q, got %q", deployer.config.Image, container.Image)
	}
	if container.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("expected ImagePullPolicy IfNotPresent, got %v", container.ImagePullPolicy)
	}

	// Verify volumes
	if len(podSpec.Volumes) != 2 {
		t.Errorf("expected 2 volumes, got %d", len(podSpec.Volumes))
	}
	snapshotVol := podSpec.Volumes[0]
	if snapshotVol.Name != "snapshot" {
		t.Errorf("expected volume name snapshot, got %q", snapshotVol.Name)
	}
	if snapshotVol.ConfigMap == nil || snapshotVol.ConfigMap.Name != deployer.config.SnapshotConfigMap {
		t.Errorf("expected ConfigMap %q, got %v", deployer.config.SnapshotConfigMap, snapshotVol.ConfigMap)
	}

	// Verify volume mounts
	if len(container.VolumeMounts) != 2 {
		t.Errorf("expected 2 volume mounts, got %d", len(container.VolumeMounts))
	}
	snapshotMount := container.VolumeMounts[0]
	if snapshotMount.Name != "snapshot" {
		t.Errorf("expected volume mount name snapshot, got %q", snapshotMount.Name)
	}
	if snapshotMount.MountPath != "/data/snapshot" {
		t.Errorf("expected mount path /data/snapshot, got %q", snapshotMount.MountPath)
	}
	if !snapshotMount.ReadOnly {
		t.Error("expected volume mount to be read-only")
	}
}

func TestBuildJobSpec_WithNodeSelector(t *testing.T) {
	deployer, _ := createDeployer()
	deployer.config.NodeSelector = map[string]string{
		"nodeGroup":            "gpu-nodes",
		"kubernetes.io/arch":   "amd64",
		"nvidia.com/gpu.count": "8",
	}

	job := deployer.buildJobSpec()

	// Verify node selector
	nodeSelector := job.Spec.Template.Spec.NodeSelector
	if len(nodeSelector) != 3 {
		t.Errorf("expected 3 node selector entries, got %d", len(nodeSelector))
	}
	if nodeSelector["nodeGroup"] != "gpu-nodes" {
		t.Errorf("expected nodeGroup=gpu-nodes, got %q", nodeSelector["nodeGroup"])
	}
	if nodeSelector["kubernetes.io/arch"] != "amd64" {
		t.Errorf("expected kubernetes.io/arch=amd64, got %q", nodeSelector["kubernetes.io/arch"])
	}
	if nodeSelector["nvidia.com/gpu.count"] != "8" {
		t.Errorf("expected nvidia.com/gpu.count=8, got %q", nodeSelector["nvidia.com/gpu.count"])
	}
}

func TestBuildJobSpec_WithTolerations(t *testing.T) {
	deployer, _ := createDeployer()
	deployer.config.Tolerations = []corev1.Toleration{
		{
			Key:      "nvidia.com/gpu",
			Operator: corev1.TolerationOpEqual,
			Value:    "present",
			Effect:   corev1.TaintEffectNoSchedule,
		},
		{
			Key:      "dedicated",
			Operator: corev1.TolerationOpEqual,
			Value:    "validation",
			Effect:   corev1.TaintEffectNoExecute,
		},
	}

	job := deployer.buildJobSpec()

	// Verify tolerations
	tolerations := job.Spec.Template.Spec.Tolerations
	if len(tolerations) != 2 {
		t.Errorf("expected 2 tolerations, got %d", len(tolerations))
	}

	tol0 := tolerations[0]
	if tol0.Key != "nvidia.com/gpu" {
		t.Errorf("expected key nvidia.com/gpu, got %q", tol0.Key)
	}
	if tol0.Operator != corev1.TolerationOpEqual {
		t.Errorf("expected operator Equal, got %v", tol0.Operator)
	}
	if tol0.Value != "present" {
		t.Errorf("expected value present, got %q", tol0.Value)
	}
	if tol0.Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("expected effect NoSchedule, got %v", tol0.Effect)
	}

	tol1 := tolerations[1]
	if tol1.Key != "dedicated" {
		t.Errorf("expected key dedicated, got %q", tol1.Key)
	}
	if tol1.Effect != corev1.TaintEffectNoExecute {
		t.Errorf("expected effect NoExecute, got %v", tol1.Effect)
	}
}

func TestBuildJobSpec_WithImagePullSecrets(t *testing.T) {
	deployer, _ := createDeployer()
	deployer.config.ImagePullSecrets = []string{"regcred", "dockerhub-secret", "gcr-secret"}

	job := deployer.buildJobSpec()

	// Verify imagePullSecrets
	secrets := job.Spec.Template.Spec.ImagePullSecrets
	if len(secrets) != 3 {
		t.Errorf("expected 3 imagePullSecrets, got %d", len(secrets))
	}
	if secrets[0].Name != "regcred" {
		t.Errorf("expected secret name regcred, got %q", secrets[0].Name)
	}
	if secrets[1].Name != "dockerhub-secret" {
		t.Errorf("expected secret name dockerhub-secret, got %q", secrets[1].Name)
	}
	if secrets[2].Name != "gcr-secret" {
		t.Errorf("expected secret name gcr-secret, got %q", secrets[2].Name)
	}
}

func TestBuildJobSpec_WithDebug(t *testing.T) {
	deployer, _ := createDeployer()
	deployer.config.Debug = true

	job := deployer.buildJobSpec()

	// Verify AICR_DEBUG env var is set
	container := job.Spec.Template.Spec.Containers[0]
	found := false
	for _, env := range container.Env {
		if env.Name == "AICR_DEBUG" {
			found = true
			if env.Value != "true" {
				t.Errorf("expected AICR_DEBUG=true, got %q", env.Value)
			}
			break
		}
	}
	if !found {
		t.Error("AICR_DEBUG environment variable not found")
	}
}

func TestBuildJobSpec_EnvironmentVariables(t *testing.T) {
	deployer, _ := createDeployer()

	job := deployer.buildJobSpec()
	container := job.Spec.Template.Spec.Containers[0]

	expectedEnvVars := map[string]string{
		"AICR_SNAPSHOT_PATH": "/data/snapshot/snapshot.yaml",
		"AICR_RECIPE_PATH":   "/data/recipe/recipe.yaml",
	}

	for name, expectedValue := range expectedEnvVars {
		found := false
		for _, env := range container.Env {
			if env.Name == name {
				found = true
				if env.Value != expectedValue {
					t.Errorf("expected %s=%q, got %q", name, expectedValue, env.Value)
				}
				break
			}
		}
		if !found {
			t.Errorf("environment variable %s not found", name)
		}
	}

	// Verify AICR_NAMESPACE is set from field ref
	found := false
	for _, env := range container.Env {
		if env.Name == "AICR_NAMESPACE" {
			found = true
			if env.ValueFrom == nil || env.ValueFrom.FieldRef == nil {
				t.Error("AICR_NAMESPACE should be set from FieldRef")
			} else if env.ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
				t.Errorf("expected FieldPath metadata.namespace, got %q", env.ValueFrom.FieldRef.FieldPath)
			}
			break
		}
	}
	if !found {
		t.Error("AICR_NAMESPACE environment variable not found")
	}
}

func TestBuildTestCommand(t *testing.T) {
	tests := []struct {
		name        string
		testPackage string
		testPattern string
		wantContain []string
	}{
		{
			name:        "basic test command",
			testPackage: "./pkg/validator/checks/deployment",
			testPattern: "TestOperatorHealth",
			wantContain: []string{
				"deployment.test",
				"-test.v",
				"-test.run 'TestOperatorHealth'",
				"| test2json |",
				"tee /tmp/test-output.json",
				"--- BEGIN TEST OUTPUT ---",
			},
		},
		{
			name:        "different package",
			testPackage: "./pkg/validator/checks/performance",
			testPattern: "TestGpuPerformance",
			wantContain: []string{
				"performance.test",
				"-test.run 'TestGpuPerformance'",
				"| test2json |",
			},
		},
		{
			name:        "pattern with regex",
			testPackage: "./pkg/validator/checks/deployment",
			testPattern: "TestGpu.*",
			wantContain: []string{
				"deployment.test",
				"-test.run 'TestGpu.*'",
			},
		},
		{
			name:        "no pattern",
			testPackage: "./pkg/validator/checks/deployment",
			testPattern: "",
			wantContain: []string{
				"deployment.test -test.v",
				"| test2json |",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployer, _ := createDeployer()
			deployer.config.TestPackage = tt.testPackage
			deployer.config.TestPattern = tt.testPattern

			cmd := deployer.buildTestCommand()

			for _, want := range tt.wantContain {
				if !strings.Contains(cmd, want) {
					t.Errorf("buildTestCommand() should contain %q, got:\n%s", want, cmd)
				}
			}
		})
	}
}

func TestTestBinaryName(t *testing.T) {
	tests := []struct {
		testPackage string
		want        string
	}{
		{"./pkg/validator/checks/deployment", "deployment.test"},
		{"./pkg/validator/checks/deployment", "deployment.test"},
		{"./pkg/validator/checks/performance", "performance.test"},
		{"./pkg/validator/checks/conformance", "conformance.test"},
	}
	for _, tt := range tests {
		t.Run(tt.testPackage, func(t *testing.T) {
			got := testBinaryName(tt.testPackage)
			if got != tt.want {
				t.Errorf("testBinaryName(%q) = %q, want %q", tt.testPackage, got, tt.want)
			}
		})
	}
}

func TestEnsureJob(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create required ConfigMaps
	snapshotCM := createTestConfigMap(deployer.config.SnapshotConfigMap, deployer.config.Namespace, "snapshot.yaml", "test data")
	recipeCM := createTestConfigMap(deployer.config.RecipeConfigMap, deployer.config.Namespace, "recipe.yaml", "test data")

	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create snapshot ConfigMap: %v", err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create recipe ConfigMap: %v", err)
	}

	t.Run("create Job", func(t *testing.T) {
		if err := deployer.ensureJob(ctx); err != nil {
			t.Fatalf("ensureJob() failed: %v", err)
		}

		job, err := clientset.BatchV1().Jobs(deployer.config.Namespace).
			Get(ctx, deployer.config.JobName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Job not found: %v", err)
		}

		if job.Name != deployer.config.JobName {
			t.Errorf("expected Job name %q, got %q", deployer.config.JobName, job.Name)
		}
	})

	t.Run("recreate Job deletes old one", func(t *testing.T) {
		// Create Job first time
		if err := deployer.ensureJob(ctx); err != nil {
			t.Fatalf("first ensureJob() failed: %v", err)
		}

		// Create Job second time - should delete and recreate
		if err := deployer.ensureJob(ctx); err != nil {
			t.Fatalf("second ensureJob() failed: %v", err)
		}

		// Verify Job still exists
		_, err := clientset.BatchV1().Jobs(deployer.config.Namespace).
			Get(ctx, deployer.config.JobName, metav1.GetOptions{})
		if err != nil {
			t.Errorf("Job should exist after recreate: %v", err)
		}
	})
}

func TestDeleteJob(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create required ConfigMaps
	snapshotCM := createTestConfigMap(deployer.config.SnapshotConfigMap, deployer.config.Namespace, "snapshot.yaml", "test data")
	recipeCM := createTestConfigMap(deployer.config.RecipeConfigMap, deployer.config.Namespace, "recipe.yaml", "test data")

	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create snapshot ConfigMap: %v", err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create recipe ConfigMap: %v", err)
	}

	t.Run("delete existing Job", func(t *testing.T) {
		// Create first
		if err := deployer.ensureJob(ctx); err != nil {
			t.Fatalf("failed to create Job: %v", err)
		}

		// Delete
		if err := deployer.deleteJob(ctx); err != nil {
			t.Fatalf("deleteJob() failed: %v", err)
		}

		// Verify it's gone
		_, err := clientset.BatchV1().Jobs(deployer.config.Namespace).
			Get(ctx, deployer.config.JobName, metav1.GetOptions{})
		if err == nil {
			t.Error("Job should be deleted")
		}
	})

	t.Run("delete non-existent Job", func(t *testing.T) {
		// Delete without creating - should not error (not found is ignored)
		if err := deployer.deleteJob(ctx); err != nil {
			t.Errorf("deleteJob() should ignore not found: %v", err)
		}
	})
}

// Helper function to create test ConfigMaps
func createTestConfigMap(name, namespace, key, data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string]string{
			key: data,
		},
	}
}
