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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// buildEnv creates environment variables for the validator container
func buildEnv(
	entry ValidatorEntry,
	runID string,
	version string,
	commit string,
	timeout time.Duration,
	nodeSelector map[string]string,
	tolerations []corev1.Toleration,
) []corev1.EnvVar {

	env := []corev1.EnvVar{
		{Name: "AICR_SNAPSHOT_PATH", Value: "/data/snapshot/snapshot.yaml"},
		{Name: "AICR_VALIDATION_PATH", Value: "/data/validation/validation.yaml"},
		{Name: "AICR_VALIDATOR_NAME", Value: entry.Name},
		{Name: "AICR_VALIDATOR_PHASE", Value: entry.Phase},
		{Name: "AICR_RUN_ID", Value: runID},
		{
			Name: "AICR_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{Name: "AICR_CHECK_TIMEOUT", Value: timeout.String()},
	}

	// Add scheduling overrides for inner workloads
	if len(nodeSelector) > 0 {
		env = append(env, corev1.EnvVar{
			Name:  "AICR_NODE_SELECTOR",
			Value: serializeNodeSelector(nodeSelector),
		})
	}
	if len(tolerations) > 0 {
		env = append(env, corev1.EnvVar{
			Name:  "AICR_TOLERATIONS",
			Value: serializeTolerations(tolerations),
		})
	}

	// Add CLI version and commit
	if version != "" {
		env = append(env, corev1.EnvVar{Name: "AICR_CLI_VERSION", Value: version})
	}
	if commit != "" {
		env = append(env, corev1.EnvVar{Name: "AICR_CLI_COMMIT", Value: commit})
	}

	// Add image registry/tag overrides from environment
	if override := os.Getenv("AICR_VALIDATOR_IMAGE_REGISTRY"); override != "" {
		env = append(env, corev1.EnvVar{Name: "AICR_VALIDATOR_IMAGE_REGISTRY", Value: override})
	}
	if tag := os.Getenv("AICR_VALIDATOR_IMAGE_TAG"); tag != "" {
		env = append(env, corev1.EnvVar{Name: "AICR_VALIDATOR_IMAGE_TAG", Value: tag})
	}

	// Add catalog entry's custom env vars
	for _, e := range entry.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}

	return env
}

// buildVolumes creates volumes for snapshot and validation ConfigMaps
func buildVolumes(runID string) []corev1.Volume {
	return []corev1.Volume{
		{
			Name: "snapshot",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: fmt.Sprintf("aicr-snapshot-%s", runID),
					},
				},
			},
		},
		{
			Name: "validation",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: fmt.Sprintf("aicr-validation-%s", runID),
					},
				},
			},
		},
	}
}

// buildVolumeMounts creates volume mounts for the validator container
func buildVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "snapshot", MountPath: "/data/snapshot", ReadOnly: true},
		{Name: "validation", MountPath: "/data/validation", ReadOnly: true},
	}
}

// buildResources creates resource requirements for the validator container
func buildResources(entry ValidatorEntry) corev1.ResourceRequirements {
	// Use catalog entry resources if specified, otherwise use defaults
	if entry.Resources != nil && entry.Resources.CPU != "" && entry.Resources.Memory != "" {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(entry.Resources.CPU),
				corev1.ResourceMemory: resource.MustParse(entry.Resources.Memory),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(entry.Resources.CPU),
				corev1.ResourceMemory: resource.MustParse(entry.Resources.Memory),
			},
		}
	}

	// Default resources
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

// serializeNodeSelector encodes a nodeSelector map as a comma-separated key=value string.
// Keys are sorted for deterministic output. This matches the format expected by
// snapshotter.ParseNodeSelectors on the receiving end.
func serializeNodeSelector(ns map[string]string) string {
	keys := make([]string, 0, len(ns))
	for k := range ns {
		keys = append(keys, k)
	}
	// Sort keys for deterministic output
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	pairs := make([]string, 0, len(ns))
	for _, k := range keys {
		pairs = append(pairs, k+"="+ns[k])
	}
	return strings.Join(pairs, ",")
}

// serializeTolerations encodes tolerations as a comma-separated list.
// Format per toleration: key=value:Effect or key:Effect (for tolerations without value).
// This matches the format expected by snapshotter.ParseTolerations on the receiving end.
func serializeTolerations(tols []corev1.Toleration) string {
	parts := make([]string, 0, len(tols))
	for _, t := range tols {
		var part string
		switch {
		case t.Key == "" && t.Operator == corev1.TolerationOpExists:
			part = "*"
		case t.Value != "":
			part = t.Key + "=" + t.Value + ":" + string(t.Effect)
		default:
			part = t.Key + ":" + string(t.Effect)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

// int32Ptr returns a pointer to an int32
func int32Ptr(i int32) *int32 {
	return &i
}

// int64Ptr returns a pointer to an int64
func int64Ptr(i int64) *int64 {
	return &i
}

// preferCPUNodeAffinity returns affinity rules that prefer CPU nodes.
// This matches the deployer implementation exactly.
func preferCPUNodeAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
				{
					Weight: 100,
					Preference: corev1.NodeSelectorTerm{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "nvidia.com/gpu.present",
								Operator: corev1.NodeSelectorOpDoesNotExist,
							},
						},
					},
				},
			},
		},
	}
}

// generateJobName creates a unique Kubernetes Job name for a validator.
// Format: aicr-{validatorName}-{random-hex} (e.g., "aicr-gpu-operator-health-a1b2c3d4").
func generateJobName(validatorName string) string {
	suffix := make([]byte, 4)
	if n, err := rand.Read(suffix); err != nil || n != len(suffix) {
		// Fallback to timestamp if random generation fails
		return fmt.Sprintf("aicr-%s-%d", validatorName, time.Now().Unix())
	}
	return fmt.Sprintf("aicr-%s-%s", validatorName, hex.EncodeToString(suffix))
}
