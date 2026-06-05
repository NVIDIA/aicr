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
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applybatchv1 "k8s.io/client-go/applyconfigurations/batch/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	applymetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
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

	// ImageTagOverride is the value of AICR_VALIDATOR_IMAGE_TAG when set
	// (empty otherwise). The override is already applied to Image; this field
	// preserves the "override in effect" signal so the renderers force
	// PullAlways for a mutable override tag (e.g. :edge), matching the AIPerf
	// sidecar. Without it a node silently reuses a cached older :edge image on
	// the main validator container. See #1177.
	ImageTagOverride string

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

	// Affinity is the orchestrator pod's full affinity (NodeAffinity for
	// prefer-CPU plus any PodAffinity terms derived from the catalog entry's
	// DependencyAffinity). If nil, the renderer falls back to the default
	// prefer-CPU NodeAffinity.
	Affinity *corev1.Affinity
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
	componentRefs []recipe.ComponentRef,
) ([]JobPlan, error) {

	var plans []JobPlan

	// Guard against nil catalog
	if cat == nil {
		return plans, nil
	}

	// Iterate through all phases
	phases := []Phase{PhaseDeployment, PhaseConformance, PhasePerformance}
	for _, phase := range phases {
		// Get all entries for this phase
		allEntries := cat.ForPhase(phase)

		// Filter by validation input
		entries := FilterEntriesByValidation(allEntries, phase, validationInput)

		// Create a plan for each entry
		for _, entry := range entries {
			plan, err := BuildJobPlan(entry, runID, namespace, version, commit,
				serviceAccount, imagePullSecrets, tolerations, nodeSelector,
				imageRegistryOverride, imageTagOverride, componentRefs)
			if err != nil {
				return nil, err
			}
			plans = append(plans, plan)
		}
	}

	return plans, nil
}

// BuildJobPlan creates a JobPlan from a validator entry.
// Exposed as public for verification and testing purposes.
//
// The tolerations and nodeSelector parameters apply to inner workloads spawned
// by validators (e.g., GPU benchmarks, NCCL tests) and are forwarded via
// AICR_TOLERATIONS and AICR_NODE_SELECTOR environment variables. The orchestrator
// Job Pod itself always uses tolerate-all scheduling ({Operator: TolerationOpExists})
// and gets affinity from BuildOrchestratorAffinity (prefer-CPU NodeAffinity, plus
// PodAffinity per entry.DependencyAffinity if any). componentRefs is the resolved
// recipe's component list and is used to resolve dependencyAffinity componentRefs
// to namespaces.
//
// Returns ErrCodeInvalidRequest when entry.DependencyAffinity declares a
// "required" component that is not present in componentRefs.
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
	componentRefs []recipe.ComponentRef,
) (JobPlan, error) {

	affinity, err := BuildOrchestratorAffinity(entry.DependencyAffinity, componentRefs)
	if err != nil {
		return JobPlan{}, err
	}

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
	resources, err := buildResources(entry)
	if err != nil {
		return JobPlan{}, err
	}

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
		ImageTagOverride: imageTagOverride,
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
		Affinity:         affinity,
	}, nil
}

// RenderPlan renders a complete Kubernetes Job from a JobPlan.
// The returned Job spec matches exactly what the current deployer produces.
func RenderPlan(plan JobPlan) *batchv1.Job {
	// Build image pull secrets
	imagePullSecrets := make([]corev1.LocalObjectReference, 0, len(plan.ImagePullSecrets))
	for _, secret := range plan.ImagePullSecrets {
		imagePullSecrets = append(imagePullSecrets, corev1.LocalObjectReference{Name: secret})
	}

	// Determine image pull policy. Pass plan.ImageTagOverride (not "") so a
	// mutable override tag like :edge forces PullAlways and the node does not
	// silently reuse a cached older image on the main validator. See #1177.
	pullPolicy := ImagePullPolicy(plan.Image, plan.ImageTagOverride)

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
					Affinity:                      affinityForPlan(plan),
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

	// Determine image pull policy. Pass plan.ImageTagOverride (not "") so a
	// mutable override tag like :edge forces PullAlways and the node does not
	// silently reuse a cached older image on the main validator. See #1177.
	pullPolicy := ImagePullPolicy(plan.Image, plan.ImageTagOverride)

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

	// Convert the plan's full affinity (NodeAffinity + any PodAffinity for
	// dependency co-location) into the apply-config types.
	affinityApply := affinityToApplyConfig(affinityForPlan(plan))

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

// affinityForPlan returns the orchestrator pod's affinity. When plan.Affinity
// is set (the common case after BuildJobPlan), it is used directly. When nil
// (external callers that constructed a JobPlan manually pre-Task-4), we fall
// back to the default prefer-CPU NodeAffinity to preserve behavior.
func affinityForPlan(plan JobPlan) *corev1.Affinity {
	if plan.Affinity != nil {
		return plan.Affinity
	}
	return preferCPUNodeAffinity()
}

// affinityToApplyConfig converts a *corev1.Affinity to the
// applyconfigurations/core/v1 type used by server-side apply. We hand-write the
// walk because client-go does not expose a generated converter for this pair.
// A nil or empty Affinity falls back to preferCPUNodeAffinity() to preserve
// pre-PR behavior for external callers that construct a JobPlan manually.
func affinityToApplyConfig(a *corev1.Affinity) *applycorev1.AffinityApplyConfiguration {
	if a == nil || (a.NodeAffinity == nil && a.PodAffinity == nil && a.PodAntiAffinity == nil) {
		return affinityToApplyConfig(preferCPUNodeAffinity())
	}
	out := applycorev1.Affinity()
	if a.NodeAffinity != nil {
		out = out.WithNodeAffinity(nodeAffinityToApplyConfig(a.NodeAffinity))
	}
	if a.PodAffinity != nil {
		out = out.WithPodAffinity(podAffinityToApplyConfig(a.PodAffinity))
	}
	if a.PodAntiAffinity != nil {
		out = out.WithPodAntiAffinity(podAntiAffinityToApplyConfig(a.PodAntiAffinity))
	}
	return out
}

func nodeAffinityToApplyConfig(na *corev1.NodeAffinity) *applycorev1.NodeAffinityApplyConfiguration {
	out := applycorev1.NodeAffinity()
	if na.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		req := applycorev1.NodeSelector()
		for _, term := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			t := term
			req = req.WithNodeSelectorTerms(nodeSelectorTermToApplyConfig(&t))
		}
		out = out.WithRequiredDuringSchedulingIgnoredDuringExecution(req)
	}
	for _, term := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		t := term
		out = out.WithPreferredDuringSchedulingIgnoredDuringExecution(
			applycorev1.PreferredSchedulingTerm().
				WithWeight(t.Weight).
				WithPreference(nodeSelectorTermToApplyConfig(&t.Preference)),
		)
	}
	return out
}

func nodeSelectorTermToApplyConfig(t *corev1.NodeSelectorTerm) *applycorev1.NodeSelectorTermApplyConfiguration {
	out := applycorev1.NodeSelectorTerm()
	for _, expr := range t.MatchExpressions {
		e := expr
		req := applycorev1.NodeSelectorRequirement().
			WithKey(e.Key).
			WithOperator(e.Operator)
		for _, v := range e.Values {
			req = req.WithValues(v)
		}
		out = out.WithMatchExpressions(req)
	}
	for _, expr := range t.MatchFields {
		e := expr
		req := applycorev1.NodeSelectorRequirement().
			WithKey(e.Key).
			WithOperator(e.Operator)
		for _, v := range e.Values {
			req = req.WithValues(v)
		}
		out = out.WithMatchFields(req)
	}
	return out
}

func podAffinityToApplyConfig(pa *corev1.PodAffinity) *applycorev1.PodAffinityApplyConfiguration {
	out := applycorev1.PodAffinity()
	for _, term := range pa.RequiredDuringSchedulingIgnoredDuringExecution {
		t := term
		out = out.WithRequiredDuringSchedulingIgnoredDuringExecution(podAffinityTermToApplyConfig(&t))
	}
	for _, w := range pa.PreferredDuringSchedulingIgnoredDuringExecution {
		wt := w
		out = out.WithPreferredDuringSchedulingIgnoredDuringExecution(
			applycorev1.WeightedPodAffinityTerm().
				WithWeight(wt.Weight).
				WithPodAffinityTerm(podAffinityTermToApplyConfig(&wt.PodAffinityTerm)),
		)
	}
	return out
}

func podAntiAffinityToApplyConfig(paa *corev1.PodAntiAffinity) *applycorev1.PodAntiAffinityApplyConfiguration {
	out := applycorev1.PodAntiAffinity()
	for _, term := range paa.RequiredDuringSchedulingIgnoredDuringExecution {
		t := term
		out = out.WithRequiredDuringSchedulingIgnoredDuringExecution(podAffinityTermToApplyConfig(&t))
	}
	for _, w := range paa.PreferredDuringSchedulingIgnoredDuringExecution {
		wt := w
		out = out.WithPreferredDuringSchedulingIgnoredDuringExecution(
			applycorev1.WeightedPodAffinityTerm().
				WithWeight(wt.Weight).
				WithPodAffinityTerm(podAffinityTermToApplyConfig(&wt.PodAffinityTerm)),
		)
	}
	return out
}

func podAffinityTermToApplyConfig(t *corev1.PodAffinityTerm) *applycorev1.PodAffinityTermApplyConfiguration {
	out := applycorev1.PodAffinityTerm().WithTopologyKey(t.TopologyKey)
	if t.LabelSelector != nil {
		ls := applymetav1.LabelSelector()
		if len(t.LabelSelector.MatchLabels) > 0 {
			ls = ls.WithMatchLabels(t.LabelSelector.MatchLabels)
		}
		for _, expr := range t.LabelSelector.MatchExpressions {
			e := expr
			req := applymetav1.LabelSelectorRequirement().
				WithKey(e.Key).
				WithOperator(e.Operator)
			for _, v := range e.Values {
				req = req.WithValues(v)
			}
			ls = ls.WithMatchExpressions(req)
		}
		out = out.WithLabelSelector(ls)
	}
	if t.NamespaceSelector != nil {
		ns := applymetav1.LabelSelector()
		if len(t.NamespaceSelector.MatchLabels) > 0 {
			ns = ns.WithMatchLabels(t.NamespaceSelector.MatchLabels)
		}
		for _, expr := range t.NamespaceSelector.MatchExpressions {
			e := expr
			req := applymetav1.LabelSelectorRequirement().
				WithKey(e.Key).
				WithOperator(e.Operator)
			for _, v := range e.Values {
				req = req.WithValues(v)
			}
			ns = ns.WithMatchExpressions(req)
		}
		out = out.WithNamespaceSelector(ns)
	}
	for _, ns := range t.Namespaces {
		out = out.WithNamespaces(ns)
	}
	for _, key := range t.MatchLabelKeys {
		out = out.WithMatchLabelKeys(key)
	}
	for _, key := range t.MismatchLabelKeys {
		out = out.WithMismatchLabelKeys(key)
	}
	return out
}
