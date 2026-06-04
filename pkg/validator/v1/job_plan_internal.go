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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

const (
	// InferencePerfCheckName is the catalog name of the inference performance check.
	// Used to scope HF_TOKEN forwarding to that validator only (it is the sole
	// check that downloads models from Hugging Face). Exported so the catalog
	// package can assert (in TestEmbeddedCatalog_InferencePerfEntryExists) that an
	// embedded entry with this exact name exists — renaming the catalog entry
	// without updating this constant would otherwise silently no-op HF_TOKEN
	// forwarding with no test failure.
	InferencePerfCheckName = "inference-perf"

	// InferenceGatewayCheckName is the catalog name of the inference gateway
	// check. Used to scope AICR_REQUIRE_SCOPED_INFERENCE_GATEWAY forwarding to
	// that validator only. Exported so the catalog package can assert (in
	// TestEmbeddedCatalog_InferenceGatewayEntryExists) that an embedded entry
	// with this exact name exists — renaming the catalog entry without updating
	// this constant would otherwise silently no-op enforcement forwarding with
	// no test failure.
	InferenceGatewayCheckName = "inference-gateway"

	requireScopedInferenceGatewayEnv = "AICR_REQUIRE_SCOPED_INFERENCE_GATEWAY"

	// inferencePerfNoCleanupEnv, when truthy, makes the inference-perf validator
	// leave its namespace/DGD/workers/frontend/AIPerf Job in place after the run
	// for post-mortem inspection. Forwarded only to the inference-perf pod.
	inferencePerfNoCleanupEnv = "AICR_INFERENCE_PERF_NO_CLEANUP"
)

// hfTokenEnvVar is the environment variable name used to forward the
// Hugging Face API token to the inference performance validator.
const hfTokenEnvVar = "HF_TOKEN" //nolint:gosec // G101: env var name, not a hardcoded credential

// buildEnv creates environment variables for the validator container.
func buildEnv(
	entry ValidatorEntry,
	runID string,
	version string,
	commit string,
	timeout time.Duration,
	nodeSelector map[string]string,
	tolerations []corev1.Toleration,
	imageRegistryOverride string,
	imageTagOverride string,
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

	// Add image registry/tag overrides
	if imageRegistryOverride != "" {
		env = append(env, corev1.EnvVar{Name: "AICR_VALIDATOR_IMAGE_REGISTRY", Value: imageRegistryOverride})
	}
	if imageTagOverride != "" {
		env = append(env, corev1.EnvVar{Name: "AICR_VALIDATOR_IMAGE_TAG", Value: imageTagOverride})
	}

	// Forward a Hugging Face token from the orchestrator's environment (never
	// the in-repo catalog) so inference-perf can provision it for model
	// downloads that would otherwise hit HF anonymous rate limits. Only set
	// when present; absent leaves workers on anonymous downloads. Scoped to the
	// inference-perf entry so the credential is not injected into unrelated
	// validator Pods (deployment/conformance checks) that never use it.
	//
	// Accepted trade-off (deliberate, reviewed): the token lands here as a
	// plaintext EnvVar.Value on the validator pod spec, whereas the workers and
	// the model-cache populate Job consume it via secretKeyRef from the
	// aicr-hf-token Secret (which this validator mints from this env). The
	// "cleaner" pattern — have the orchestrator create the Secret and reference
	// it via secretKeyRef on the validator pod too — was considered and
	// declined: the upside is narrow (keeps the value out of the pod spec, where
	// the broadly-granted `get pods` permission would otherwise expose it, vs.
	// the narrower `get secrets`) and does NOT remove the plaintext (the running
	// container still exposes HF_TOKEN via env/`kubectl exec`; the Secret is
	// likewise base64-in-etcd absent encryption-at-rest). Given a
	// low-sensitivity, easily-rotated rate-limit token and a per-run ephemeral
	// namespace, the exposure does not justify the orchestrator-side Secret
	// plumbing. Revisit if a higher-privilege credential ever flows this path.
	if tok := os.Getenv(hfTokenEnvVar); tok != "" && entry.Name == InferencePerfCheckName {
		env = append(env, corev1.EnvVar{Name: hfTokenEnvVar, Value: tok})
	}

	// Forward the enforcement toggle for the inference-gateway exposure check
	// into that validator pod. The check runs inside the Kubernetes Job, so it
	// cannot observe the CLI process environment unless the orchestrator carries
	// this value across here.
	if v, ok := os.LookupEnv(requireScopedInferenceGatewayEnv); ok && v != "" && entry.Name == InferenceGatewayCheckName {
		env = append(env, corev1.EnvVar{Name: requireScopedInferenceGatewayEnv, Value: v})
	}

	// Forward the inference-perf no-cleanup debug toggle into that validator pod.
	// Cleanup runs inside the Job, so it can't see the CLI process environment
	// unless the orchestrator carries the value across. Scoped to the
	// inference-perf entry. Gated on strconv.ParseBool — the same truthiness the
	// runtime cleanup gate uses — so a value like "yes"/"2" can't be forwarded
	// here yet parsed false there (half-enabling the toggle). Forwards a
	// canonical "1" so the pod-side check is unambiguous. When set, a
	// failed/anomalous run leaves the namespace, DGD, workers, frontend, and
	// AIPerf Job in place for inspection.
	if on, _ := strconv.ParseBool(os.Getenv(inferencePerfNoCleanupEnv)); on && entry.Name == InferencePerfCheckName {
		env = append(env, corev1.EnvVar{Name: inferencePerfNoCleanupEnv, Value: "1"})
	}

	// Add catalog entry's custom env vars. Orchestrator-controlled env vars are
	// deliberately skipped here: they must come only from the process
	// environment (forwarded above), never from the in-repo catalog. Appending
	// them from entry.Env would let a catalog value silently override the
	// forwarded value (k8s takes the last duplicate), breaking that trust
	// boundary.
	for _, e := range entry.Env {
		if e.Name == hfTokenEnvVar || e.Name == requireScopedInferenceGatewayEnv || e.Name == inferencePerfNoCleanupEnv {
			continue
		}
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

// buildResources creates resource requirements for the validator container.
// Fails closed on invalid resource quantities — a typo in validators/catalog.yaml
// must not silently substitute defaults and ship a misconfigured workload.
func buildResources(entry ValidatorEntry) (corev1.ResourceRequirements, error) {
	if entry.Resources != nil && (entry.Resources.CPU != "" || entry.Resources.Memory != "") {
		// Both fields must be set when overriding defaults — partial overrides
		// are ambiguous and almost always a config error.
		if entry.Resources.CPU == "" || entry.Resources.Memory == "" {
			return corev1.ResourceRequirements{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator %q: resources must specify both cpu and memory (got cpu=%q memory=%q)",
					entry.Name, entry.Resources.CPU, entry.Resources.Memory))
		}
		cpu, cpuErr := resource.ParseQuantity(entry.Resources.CPU)
		memory, memErr := resource.ParseQuantity(entry.Resources.Memory)
		if cpuErr != nil || memErr != nil {
			return corev1.ResourceRequirements{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator %q: invalid resource quantities (cpu=%q err=%v; memory=%q err=%v)",
					entry.Name, entry.Resources.CPU, cpuErr, entry.Resources.Memory, memErr))
		}
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    cpu,
				corev1.ResourceMemory: memory,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    cpu,
				corev1.ResourceMemory: memory,
			},
		}, nil
	}

	// Default resources when entry.Resources is nil or both fields empty.
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}, nil
}

// serializeNodeSelector encodes a nodeSelector map as a comma-separated key=value string.
// Keys are sorted for deterministic output. This matches the format expected by
// snapshotter.ParseNodeSelectors on the receiving end.
func serializeNodeSelector(ns map[string]string) string {
	keys := make([]string, 0, len(ns))
	for k := range ns {
		keys = append(keys, k)
	}
	sort.Strings(keys)
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
					Weight: preferredAffinityWeight,
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

// dns1123LabelRegex validates DNS-1123 label format (RFC 1123).
// Must be lowercase alphanumeric or '-', start/end with alphanumeric, max 63 chars.
var dns1123LabelRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// generateJobName creates a unique Kubernetes Job name for a validator.
// Format: aicr-{validatorName}-{random-hex} (e.g., "aicr-gpu-operator-health-a1b2c3d4").
//
// Panics if:
// - validatorName contains invalid DNS-1123 characters (uppercase, underscore, etc.)
// - the generated name exceeds 63 characters (Kubernetes limit)
// - random number generation fails
func generateJobName(validatorName string) string {
	// Validate that validatorName is DNS-1123 compliant
	if !dns1123LabelRegex.MatchString(validatorName) {
		panic(fmt.Sprintf("invalid validator name %q: must be lowercase alphanumeric or '-', start/end with alphanumeric", validatorName))
	}

	suffix := make([]byte, 4)
	n, err := rand.Read(suffix)
	if err != nil {
		panic(fmt.Sprintf("failed to generate random bytes for job name: %v", err))
	}
	if n != len(suffix) {
		panic(fmt.Sprintf("failed to generate job name: read %d bytes, expected %d", n, len(suffix)))
	}

	jobName := fmt.Sprintf("aicr-%s-%s", validatorName, hex.EncodeToString(suffix))

	// Validate total length (Kubernetes limit is 63 characters for names)
	if len(jobName) > 63 {
		panic(fmt.Sprintf("generated job name %q exceeds 63 characters (length: %d)", jobName, len(jobName)))
	}

	return jobName
}

// buildEnvVarApply builds environment variable apply configurations from a list of EnvVars.
// Handles all four ValueFrom source types: FieldRef, SecretKeyRef, ConfigMapKeyRef, ResourceFieldRef.
// Preserves all fields including Optional flags.
func buildEnvVarApply(envVars []corev1.EnvVar) []*applycorev1.EnvVarApplyConfiguration {
	envApply := make([]*applycorev1.EnvVarApplyConfiguration, 0, len(envVars))
	for _, e := range envVars {
		if e.ValueFrom != nil {
			// Handle all four ValueFrom source types
			envVarSource := applycorev1.EnvVarSource()
			switch {
			case e.ValueFrom.FieldRef != nil:
				fieldRef := applycorev1.ObjectFieldSelector().
					WithFieldPath(e.ValueFrom.FieldRef.FieldPath)
				if e.ValueFrom.FieldRef.APIVersion != "" {
					fieldRef = fieldRef.WithAPIVersion(e.ValueFrom.FieldRef.APIVersion)
				}
				envVarSource = envVarSource.WithFieldRef(fieldRef)
			case e.ValueFrom.SecretKeyRef != nil:
				secretRef := applycorev1.SecretKeySelector().
					WithName(e.ValueFrom.SecretKeyRef.Name).
					WithKey(e.ValueFrom.SecretKeyRef.Key)
				if e.ValueFrom.SecretKeyRef.Optional != nil {
					secretRef = secretRef.WithOptional(*e.ValueFrom.SecretKeyRef.Optional)
				}
				envVarSource = envVarSource.WithSecretKeyRef(secretRef)
			case e.ValueFrom.ConfigMapKeyRef != nil:
				cmRef := applycorev1.ConfigMapKeySelector().
					WithName(e.ValueFrom.ConfigMapKeyRef.Name).
					WithKey(e.ValueFrom.ConfigMapKeyRef.Key)
				if e.ValueFrom.ConfigMapKeyRef.Optional != nil {
					cmRef = cmRef.WithOptional(*e.ValueFrom.ConfigMapKeyRef.Optional)
				}
				envVarSource = envVarSource.WithConfigMapKeyRef(cmRef)
			case e.ValueFrom.ResourceFieldRef != nil:
				resRef := applycorev1.ResourceFieldSelector().
					WithResource(e.ValueFrom.ResourceFieldRef.Resource)
				if e.ValueFrom.ResourceFieldRef.ContainerName != "" {
					resRef = resRef.WithContainerName(e.ValueFrom.ResourceFieldRef.ContainerName)
				}
				if !e.ValueFrom.ResourceFieldRef.Divisor.IsZero() {
					resRef = resRef.WithDivisor(e.ValueFrom.ResourceFieldRef.Divisor)
				}
				envVarSource = envVarSource.WithResourceFieldRef(resRef)
			}
			envApply = append(envApply, applycorev1.EnvVar().
				WithName(e.Name).
				WithValueFrom(envVarSource))
		} else {
			envApply = append(envApply, applycorev1.EnvVar().
				WithName(e.Name).
				WithValue(e.Value))
		}
	}
	return envApply
}

// buildKeyToPathItems converts KeyToPath items to ApplyConfiguration.
func buildKeyToPathItems(items []corev1.KeyToPath) []*applycorev1.KeyToPathApplyConfiguration {
	result := make([]*applycorev1.KeyToPathApplyConfiguration, 0, len(items))
	for _, item := range items {
		ktp := applycorev1.KeyToPath().WithKey(item.Key).WithPath(item.Path)
		if item.Mode != nil {
			ktp = ktp.WithMode(*item.Mode)
		}
		result = append(result, ktp)
	}
	return result
}

// buildDownwardAPIItems converts DownwardAPIVolumeFile items to ApplyConfiguration.
func buildDownwardAPIItems(items []corev1.DownwardAPIVolumeFile) []*applycorev1.DownwardAPIVolumeFileApplyConfiguration {
	result := make([]*applycorev1.DownwardAPIVolumeFileApplyConfiguration, 0, len(items))
	for _, item := range items {
		dItem := applycorev1.DownwardAPIVolumeFile().WithPath(item.Path)
		if item.FieldRef != nil {
			fieldRef := applycorev1.ObjectFieldSelector().WithFieldPath(item.FieldRef.FieldPath)
			if item.FieldRef.APIVersion != "" {
				fieldRef = fieldRef.WithAPIVersion(item.FieldRef.APIVersion)
			}
			dItem = dItem.WithFieldRef(fieldRef)
		}
		if item.ResourceFieldRef != nil {
			resRef := applycorev1.ResourceFieldSelector().WithResource(item.ResourceFieldRef.Resource)
			if item.ResourceFieldRef.ContainerName != "" {
				resRef = resRef.WithContainerName(item.ResourceFieldRef.ContainerName)
			}
			if !item.ResourceFieldRef.Divisor.IsZero() {
				resRef = resRef.WithDivisor(item.ResourceFieldRef.Divisor)
			}
			dItem = dItem.WithResourceFieldRef(resRef)
		}
		if item.Mode != nil {
			dItem = dItem.WithMode(*item.Mode)
		}
		result = append(result, dItem)
	}
	return result
}

// buildProjectedSources converts VolumeProjection sources to ApplyConfiguration.
func buildProjectedSources(sources []corev1.VolumeProjection) []*applycorev1.VolumeProjectionApplyConfiguration {
	result := make([]*applycorev1.VolumeProjectionApplyConfiguration, 0, len(sources))
	for _, src := range sources {
		source := applycorev1.VolumeProjection()
		switch {
		case src.ConfigMap != nil:
			cm := applycorev1.ConfigMapProjection().WithName(src.ConfigMap.Name)
			if src.ConfigMap.Optional != nil {
				cm = cm.WithOptional(*src.ConfigMap.Optional)
			}
			if len(src.ConfigMap.Items) > 0 {
				cm = cm.WithItems(buildKeyToPathItems(src.ConfigMap.Items)...)
			}
			source = source.WithConfigMap(cm)
		case src.Secret != nil:
			secret := applycorev1.SecretProjection().WithName(src.Secret.Name)
			if src.Secret.Optional != nil {
				secret = secret.WithOptional(*src.Secret.Optional)
			}
			if len(src.Secret.Items) > 0 {
				secret = secret.WithItems(buildKeyToPathItems(src.Secret.Items)...)
			}
			source = source.WithSecret(secret)
		case src.DownwardAPI != nil:
			downwardAPI := applycorev1.DownwardAPIProjection()
			if len(src.DownwardAPI.Items) > 0 {
				downwardAPI = downwardAPI.WithItems(buildDownwardAPIItems(src.DownwardAPI.Items)...)
			}
			source = source.WithDownwardAPI(downwardAPI)
		case src.ServiceAccountToken != nil:
			token := applycorev1.ServiceAccountTokenProjection().
				WithPath(src.ServiceAccountToken.Path)
			if src.ServiceAccountToken.Audience != "" {
				token = token.WithAudience(src.ServiceAccountToken.Audience)
			}
			if src.ServiceAccountToken.ExpirationSeconds != nil {
				token = token.WithExpirationSeconds(*src.ServiceAccountToken.ExpirationSeconds)
			}
			source = source.WithServiceAccountToken(token)
		}
		result = append(result, source)
	}
	return result
}

// buildVolumesApply builds volume apply configurations from a list of Volumes.
// Handles all volume source types: ConfigMap, Secret, EmptyDir, HostPath, PVC, Projected, DownwardAPI.
// Preserves all fields including Items, DefaultMode, Optional, ReadOnly, etc.
func buildVolumesApply(volumes []corev1.Volume) []*applycorev1.VolumeApplyConfiguration {
	volumesApply := make([]*applycorev1.VolumeApplyConfiguration, 0, len(volumes))
	for _, v := range volumes {
		volApply := applycorev1.Volume().WithName(v.Name)

		// Handle all volume source types
		switch {
		case v.ConfigMap != nil:
			cm := applycorev1.ConfigMapVolumeSource().WithName(v.ConfigMap.Name)
			if v.ConfigMap.DefaultMode != nil {
				cm = cm.WithDefaultMode(*v.ConfigMap.DefaultMode)
			}
			if v.ConfigMap.Optional != nil {
				cm = cm.WithOptional(*v.ConfigMap.Optional)
			}
			if len(v.ConfigMap.Items) > 0 {
				cm = cm.WithItems(buildKeyToPathItems(v.ConfigMap.Items)...)
			}
			volApply = volApply.WithConfigMap(cm)
		case v.Secret != nil:
			secret := applycorev1.SecretVolumeSource().WithSecretName(v.Secret.SecretName)
			if v.Secret.DefaultMode != nil {
				secret = secret.WithDefaultMode(*v.Secret.DefaultMode)
			}
			if v.Secret.Optional != nil {
				secret = secret.WithOptional(*v.Secret.Optional)
			}
			if len(v.Secret.Items) > 0 {
				secret = secret.WithItems(buildKeyToPathItems(v.Secret.Items)...)
			}
			volApply = volApply.WithSecret(secret)
		case v.EmptyDir != nil:
			emptyDir := applycorev1.EmptyDirVolumeSource()
			if v.EmptyDir.Medium != "" {
				emptyDir = emptyDir.WithMedium(v.EmptyDir.Medium)
			}
			if v.EmptyDir.SizeLimit != nil && !v.EmptyDir.SizeLimit.IsZero() {
				emptyDir = emptyDir.WithSizeLimit(*v.EmptyDir.SizeLimit)
			}
			volApply = volApply.WithEmptyDir(emptyDir)
		case v.HostPath != nil:
			hostPath := applycorev1.HostPathVolumeSource().WithPath(v.HostPath.Path)
			if v.HostPath.Type != nil {
				hostPath = hostPath.WithType(*v.HostPath.Type)
			}
			volApply = volApply.WithHostPath(hostPath)
		case v.PersistentVolumeClaim != nil:
			pvc := applycorev1.PersistentVolumeClaimVolumeSource().
				WithClaimName(v.PersistentVolumeClaim.ClaimName)
			if v.PersistentVolumeClaim.ReadOnly {
				pvc = pvc.WithReadOnly(v.PersistentVolumeClaim.ReadOnly)
			}
			volApply = volApply.WithPersistentVolumeClaim(pvc)
		case v.Projected != nil:
			projected := applycorev1.ProjectedVolumeSource()
			if v.Projected.DefaultMode != nil {
				projected = projected.WithDefaultMode(*v.Projected.DefaultMode)
			}
			projected = projected.WithSources(buildProjectedSources(v.Projected.Sources)...)
			volApply = volApply.WithProjected(projected)
		case v.DownwardAPI != nil:
			downwardAPI := applycorev1.DownwardAPIVolumeSource()
			if v.DownwardAPI.DefaultMode != nil {
				downwardAPI = downwardAPI.WithDefaultMode(*v.DownwardAPI.DefaultMode)
			}
			if len(v.DownwardAPI.Items) > 0 {
				downwardAPI = downwardAPI.WithItems(buildDownwardAPIItems(v.DownwardAPI.Items)...)
			}
			volApply = volApply.WithDownwardAPI(downwardAPI)
		}

		volumesApply = append(volumesApply, volApply)
	}
	return volumesApply
}
