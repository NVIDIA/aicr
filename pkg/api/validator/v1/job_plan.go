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
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applybatchv1 "k8s.io/client-go/applyconfigurations/batch/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

// JobPlan contains all components needed to build a validator Job.
// External controllers can use these components to build custom Jobs
// or call RenderPlan() to get an AICR-identical Job.
type JobPlan struct {
	// ValidatorName is the unique validator identifier
	ValidatorName string

	// Phase is the validation phase ("deployment", "performance", "conformance")
	Phase string

	// JobName is the generated Kubernetes Job name (unique per invocation)
	JobName string

	// Namespace is the Kubernetes namespace for the Job
	Namespace string

	// Image is the validator container image
	Image string

	// Args are container arguments
	Args []string

	// Env are environment variables for the container
	Env []corev1.EnvVar

	// Volumes are pod volumes (ConfigMaps for snapshot and validation data)
	Volumes []corev1.Volume

	// VolumeMounts are container volume mounts
	VolumeMounts []corev1.VolumeMount

	// Resources are container resource requirements
	Resources corev1.ResourceRequirements

	// Timeout is the maximum execution time (Job activeDeadlineSeconds)
	Timeout int64

	// ServiceAccount is the Kubernetes ServiceAccount name
	ServiceAccount string

	// Tolerations are pod tolerations for scheduling
	Tolerations []corev1.Toleration

	// ImagePullSecrets are secret names for pulling images (empty = no secrets)
	ImagePullSecrets []string

	// Labels are labels to apply to the Job and Pod
	Labels map[string]string
}

// GenerateRunID creates a unique run identifier for validation sessions.
// Format: {timestamp}-{random-hex} (e.g., "20260514-123045-abc123def456").
// External controllers should use this to generate runIDs before creating
// ConfigMaps and rendering Jobs.
//
// Panics if the system's random number generator fails. Entropy failures are
// exceptional and we prefer to fail fast rather than generate predictable IDs
// that could collide across concurrent runs.
func GenerateRunID() string {
	timestamp := time.Now().Format("20060102-150405")
	randomBytes := make([]byte, 8)
	n, err := rand.Read(randomBytes)
	if err != nil {
		panic(fmt.Sprintf("failed to generate random bytes for runID: %v", err))
	}
	if n != len(randomBytes) {
		panic(fmt.Sprintf("failed to generate runID: read %d bytes, expected %d", n, len(randomBytes)))
	}
	return fmt.Sprintf("%s-%s", timestamp, hex.EncodeToString(randomBytes))
}

// ImagePullPolicy determines the pull policy for a container image.
// Returns Never for local side-loaded images (ko.local, kind.local),
// Always for :latest tag or when imageTagOverride is set,
// IfNotPresent for digest-pinned or versioned tags.
func ImagePullPolicy(image string, imageTagOverride string) corev1.PullPolicy {
	// Trailing slash anchors the match to the full registry segment so a
	// real registry like `ko.localhost:5000/...` is not mistaken for a
	// side-loaded `ko.local/...` ref and wrongly forced to PullNever.
	if strings.HasPrefix(image, "ko.local/") || strings.HasPrefix(image, "kind.local/") {
		return corev1.PullNever
	}
	if strings.Contains(image, "@") {
		// Digest pin — immutable by construction. Caching is safe and
		// also required for disconnected/air-gapped deployments.
		return corev1.PullIfNotPresent
	}
	if imageTagOverride != "" {
		return corev1.PullAlways
	}
	if strings.HasSuffix(image, ":latest") {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

// Plan generates job plans for all validators across all phases.
// Returns a flat list of JobPlans where each plan contains all components
// needed to build a validator Job. Controllers can group by Phase field.
func Plan(
	cat *ValidatorCatalog,
	validationInput *ValidationInput,
	runID string,
	namespace string,
	version string,
	commit string,
	serviceAccount string,
	imagePullSecrets []string,
	tolerations []corev1.Toleration,
	nodeSelector map[string]string,
	imageRegistryOverride string,
	imageTagOverride string,
) []JobPlan {

	var plans []JobPlan

	// Guard against nil catalog
	if cat == nil {
		return plans
	}

	// Iterate through all phases
	phases := []Phase{PhaseDeployment, PhasePerformance, PhaseConformance}
	for _, phase := range phases {
		// Get all entries for this phase
		allEntries := cat.ForPhase(phase)

		// Filter by validation input
		entries := FilterEntriesByValidation(allEntries, phase, validationInput)

		// Create a plan for each entry
		for _, entry := range entries {
			plan := BuildJobPlan(entry, runID, namespace, version, commit,
				serviceAccount, imagePullSecrets, tolerations, nodeSelector,
				imageRegistryOverride, imageTagOverride)
			plans = append(plans, plan)
		}
	}

	return plans
}

// BuildJobPlan creates a JobPlan from a validator entry.
// Exposed as public for verification and testing purposes.
//
// The tolerations and nodeSelector parameters apply to inner workloads spawned
// by validators (e.g., GPU benchmarks, NCCL tests) and are forwarded via
// AICR_TOLERATIONS and AICR_NODE_SELECTOR environment variables. The orchestrator
// Job Pod itself always uses tolerate-all scheduling ({Operator: TolerationOpExists})
// and no node selector to ensure it can schedule on any available CPU node.
func BuildJobPlan(
	entry ValidatorEntry,
	runID string,
	namespace string,
	version string,
	commit string,
	serviceAccount string,
	imagePullSecrets []string,
	tolerations []corev1.Toleration,
	nodeSelector map[string]string,
	imageRegistryOverride string,
	imageTagOverride string,
) JobPlan {

	timeout := entry.Timeout
	if timeout == 0 {
		timeout = defaults.ValidatorDefaultTimeout
	}

	// Build environment variables
	env := buildEnv(entry, runID, version, commit, timeout, nodeSelector, tolerations,
		imageRegistryOverride, imageTagOverride)

	// Build volumes
	volumes := buildVolumes(runID)

	// Build volume mounts
	volumeMounts := buildVolumeMounts()

	// Build resources
	resources := buildResources(entry)

	// Build labels
	jobLabels := map[string]string{
		labels.Name:      labels.ValueAICR,
		labels.Component: labels.ValueValidation,
		labels.ManagedBy: labels.ValueAICR,
		labels.JobType:   labels.ValueValidation,
		labels.RunID:     runID,
		labels.Validator: entry.Name,
		labels.Phase:     entry.Phase,
	}

	return JobPlan{
		ValidatorName:    entry.Name,
		Phase:            entry.Phase,
		JobName:          generateJobName(entry.Name),
		Namespace:        namespace,
		Image:            entry.Image,
		Args:             entry.Args,
		Env:              env,
		Volumes:          volumes,
		VolumeMounts:     volumeMounts,
		Resources:        resources,
		Timeout:          int64(timeout.Seconds()),
		ServiceAccount:   serviceAccount,
		Tolerations:      []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		ImagePullSecrets: imagePullSecrets,
		Labels:           jobLabels,
	}
}

// RenderPlan renders a complete Kubernetes Job from a JobPlan.
// The returned Job spec matches exactly what the current deployer produces.
func RenderPlan(plan JobPlan) *batchv1.Job {
	// Build image pull secrets
	imagePullSecrets := make([]corev1.LocalObjectReference, 0, len(plan.ImagePullSecrets))
	for _, secret := range plan.ImagePullSecrets {
		imagePullSecrets = append(imagePullSecrets, corev1.LocalObjectReference{Name: secret})
	}

	// Determine image pull policy
	// Note: imageTagOverride already applied to plan.Image during BuildJobPlan
	pullPolicy := ImagePullPolicy(plan.Image, "")

	jobObj := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      plan.JobName,
			Namespace: plan.Namespace,
			Labels:    plan.Labels,
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   &plan.Timeout,
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(int32(defaults.JobTTLAfterFinished.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						labels.Name:      labels.ValueAICR,
						labels.Component: labels.ValueValidation,
						labels.Validator: plan.ValidatorName,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            plan.ServiceAccount,
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: int64Ptr(int64(defaults.ValidatorTerminationGracePeriod.Seconds())),
					ImagePullSecrets:              imagePullSecrets,
					Tolerations:                   plan.Tolerations,
					Affinity:                      preferCPUNodeAffinity(),
					Containers: []corev1.Container{
						{
							Name:                     "validator",
							Image:                    plan.Image,
							ImagePullPolicy:          pullPolicy,
							Args:                     plan.Args,
							Env:                      plan.Env,
							Resources:                plan.Resources,
							TerminationMessagePath:   "/dev/termination-log",
							TerminationMessagePolicy: corev1.TerminationMessageReadFile,
							VolumeMounts:             plan.VolumeMounts,
						},
					},
					Volumes: plan.Volumes,
				},
			},
		},
	}

	return jobObj
}

// RenderPlanToApplyConfig renders a Kubernetes Job ApplyConfiguration from a JobPlan.
// This is used for server-side apply deployment strategy. External controllers can
// use this to get field ownership tracking and idempotent apply semantics.
//
// The jobName parameter must be provided by the caller (unlike RenderPlan which uses plan.JobName).
// This allows controllers to use deterministic names for idempotent re-runs.
func RenderPlanToApplyConfig(plan JobPlan, jobName string) *applybatchv1.JobApplyConfiguration {
	// Build image pull secrets
	imagePullSecrets := make([]*applycorev1.LocalObjectReferenceApplyConfiguration, 0, len(plan.ImagePullSecrets))
	for _, secret := range plan.ImagePullSecrets {
		imagePullSecrets = append(imagePullSecrets,
			applycorev1.LocalObjectReference().WithName(secret))
	}

	// Determine image pull policy
	// Note: imageTagOverride already applied to plan.Image during BuildJobPlan
	pullPolicy := ImagePullPolicy(plan.Image, "")

	// Build environment variables
	envApply := buildEnvVarApply(plan.Env)

	// Build volume mounts
	volumeMountsApply := make([]*applycorev1.VolumeMountApplyConfiguration, 0, len(plan.VolumeMounts))
	for _, vm := range plan.VolumeMounts {
		volumeMountsApply = append(volumeMountsApply, applycorev1.VolumeMount().
			WithName(vm.Name).
			WithMountPath(vm.MountPath).
			WithReadOnly(vm.ReadOnly))
	}

	// Build volumes
	volumesApply := buildVolumesApply(plan.Volumes)

	// Build resources
	resourcesApply := applycorev1.ResourceRequirements()
	if plan.Resources.Requests != nil {
		resourcesApply = resourcesApply.WithRequests(plan.Resources.Requests)
	}
	if plan.Resources.Limits != nil {
		resourcesApply = resourcesApply.WithLimits(plan.Resources.Limits)
	}

	// Build tolerations
	tolerationsApply := make([]*applycorev1.TolerationApplyConfiguration, 0, len(plan.Tolerations))
	for _, t := range plan.Tolerations {
		toleration := applycorev1.Toleration().WithOperator(t.Operator)
		if t.Key != "" {
			toleration = toleration.WithKey(t.Key)
		}
		if t.Value != "" {
			toleration = toleration.WithValue(t.Value)
		}
		if t.Effect != "" {
			toleration = toleration.WithEffect(t.Effect)
		}
		if t.TolerationSeconds != nil {
			toleration = toleration.WithTolerationSeconds(*t.TolerationSeconds)
		}
		tolerationsApply = append(tolerationsApply, toleration)
	}

	// Build affinity (prefer CPU nodes)
	affinityApply := applycorev1.Affinity().
		WithNodeAffinity(applycorev1.NodeAffinity().
			WithPreferredDuringSchedulingIgnoredDuringExecution(
				applycorev1.PreferredSchedulingTerm().
					WithWeight(100).
					WithPreference(applycorev1.NodeSelectorTerm().
						WithMatchExpressions(
							applycorev1.NodeSelectorRequirement().
								WithKey("nvidia.com/gpu.present").
								WithOperator(corev1.NodeSelectorOpDoesNotExist),
						),
					),
			),
		)

	// Build the Job ApplyConfiguration
	return applybatchv1.Job(jobName, plan.Namespace).
		WithLabels(plan.Labels).
		WithSpec(applybatchv1.JobSpec().
			WithActiveDeadlineSeconds(plan.Timeout).
			WithBackoffLimit(0).
			WithTTLSecondsAfterFinished(int32(defaults.JobTTLAfterFinished.Seconds())).
			WithTemplate(applycorev1.PodTemplateSpec().
				WithLabels(map[string]string{
					labels.Name:      labels.ValueAICR,
					labels.Component: labels.ValueValidation,
					labels.Validator: plan.ValidatorName,
				}).
				WithSpec(applycorev1.PodSpec().
					WithServiceAccountName(plan.ServiceAccount).
					WithRestartPolicy(corev1.RestartPolicyNever).
					WithTerminationGracePeriodSeconds(int64(defaults.ValidatorTerminationGracePeriod.Seconds())).
					WithImagePullSecrets(imagePullSecrets...).
					WithTolerations(tolerationsApply...).
					WithAffinity(affinityApply).
					WithContainers(
						applycorev1.Container().
							WithName("validator").
							WithImage(plan.Image).
							WithImagePullPolicy(pullPolicy).
							WithArgs(plan.Args...).
							WithEnv(envApply...).
							WithResources(resourcesApply).
							WithTerminationMessagePath("/dev/termination-log").
							WithTerminationMessagePolicy(corev1.TerminationMessageReadFile).
							WithVolumeMounts(volumeMountsApply...),
					).
					WithVolumes(volumesApply...),
				),
			),
		)
}
