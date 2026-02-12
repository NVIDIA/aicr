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
	"fmt"
	"log/slog"

	"github.com/NVIDIA/eidos/pkg/k8s"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ensureJob creates or recreates the validation Job.
func (d *Deployer) ensureJob(ctx context.Context) error {
	// Delete existing Job if present
	if err := d.deleteJob(ctx); err != nil {
		return err
	}

	// Build Job spec
	job := d.buildJobSpec()

	// Create the Job
	_, err := d.clientset.BatchV1().Jobs(d.config.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create Job: %w", err)
	}

	slog.Debug("validation Job created",
		"job", d.config.JobName,
		"namespace", d.config.Namespace)

	return nil
}

// deleteJob deletes the validation Job if it exists.
func (d *Deployer) deleteJob(ctx context.Context) error {
	propagationPolicy := metav1.DeletePropagationBackground
	err := d.clientset.BatchV1().Jobs(d.config.Namespace).Delete(ctx, d.config.JobName, metav1.DeleteOptions{
		PropagationPolicy: &propagationPolicy,
	})
	return k8s.IgnoreNotFound(err)
}

// buildJobSpec constructs the Kubernetes Job specification.
func (d *Deployer) buildJobSpec() *batchv1.Job {
	// Build command to run tests
	testCommand := d.buildTestCommand()

	// Build container
	container := corev1.Container{
		Name:            "validator",
		Image:           d.config.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c"},
		Args:            []string{testCommand},
		Env: []corev1.EnvVar{
			{
				Name:  "EIDOS_SNAPSHOT_PATH",
				Value: "/data/snapshot/snapshot.yaml",
			},
			{
				Name:  "EIDOS_RECIPE_PATH",
				Value: "/data/recipe/recipe.yaml",
			},
			{
				Name: "EIDOS_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "snapshot",
				MountPath: "/data/snapshot",
				ReadOnly:  true,
			},
			{
				Name:      "recipe",
				MountPath: "/data/recipe",
				ReadOnly:  true,
			},
		},
	}

	// Add debug flag if enabled
	if d.config.Debug {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "EIDOS_DEBUG",
			Value: "true",
		})
	}

	// Build volumes from ConfigMaps
	volumes := []corev1.Volume{
		{
			Name: "snapshot",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: d.config.SnapshotConfigMap,
					},
				},
			},
		},
		{
			Name: "recipe",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: d.config.RecipeConfigMap,
					},
				},
			},
		},
	}

	// Build image pull secrets
	//nolint:prealloc // Size unknown at compile time
	var imagePullSecrets []corev1.LocalObjectReference
	for _, secret := range d.config.ImagePullSecrets {
		imagePullSecrets = append(imagePullSecrets, corev1.LocalObjectReference{
			Name: secret,
		})
	}

	// Build pod spec
	podSpec := corev1.PodSpec{
		ServiceAccountName: d.config.ServiceAccountName,
		RestartPolicy:      corev1.RestartPolicyNever,
		Containers:         []corev1.Container{container},
		Volumes:            volumes,
		ImagePullSecrets:   imagePullSecrets,
	}

	// Add node selector if specified
	if len(d.config.NodeSelector) > 0 {
		podSpec.NodeSelector = d.config.NodeSelector
	}

	// Add tolerations if specified
	if len(d.config.Tolerations) > 0 {
		podSpec.Tolerations = d.config.Tolerations
	}

	// Build Job
	backoffLimit := int32(0)               // No retries - validation should be deterministic
	ttlSecondsAfterFinished := int32(3600) // Keep for 1 hour for debugging

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.config.JobName,
			Namespace: d.config.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "eidos",
				"app.kubernetes.io/component": "validation",
				"eidos.nvidia.com/job-type":   "validation",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":      "eidos",
						"app.kubernetes.io/component": "validation",
						"eidos.nvidia.com/job":        d.config.JobName,
					},
				},
				Spec: podSpec,
			},
		},
	}
}

// buildTestCommand constructs the shell command to run tests.
// Test output (JSON format) is sent to stdout for the validator to parse from Job logs.
func (d *Deployer) buildTestCommand() string {
	// Command structure:
	// 1. Run go test with JSON output to stdout
	// 2. Tee output to /tmp for debugging
	// 3. Exit with test exit code

	// Build test command - only include -run flag if pattern is specified
	testCmd := fmt.Sprintf("go test -v -json %s", d.config.TestPackage)
	if d.config.TestPattern != "" {
		// Quote the pattern to handle special shell characters like ()
		testCmd = fmt.Sprintf("%s -run '%s'", testCmd, d.config.TestPattern)
	}

	return fmt.Sprintf(`
set -e
echo "Running validation tests..."
echo "Package: %s"
echo "Pattern: %s"
echo "--- BEGIN TEST OUTPUT ---"

# Run tests with JSON output
# Tee to /tmp/test-output.json for debugging, also send to stdout
%s 2>&1 | tee /tmp/test-output.json || TEST_EXIT=$?

echo "--- END TEST OUTPUT ---"

# Exit with test exit code
exit ${TEST_EXIT:-0}
`, d.config.TestPackage, d.config.TestPattern, testCmd)
}
