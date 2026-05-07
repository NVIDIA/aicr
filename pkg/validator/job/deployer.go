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
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applybatchv1 "k8s.io/client-go/applyconfigurations/batch/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

// Deployer manages the lifecycle of a single validator Job.
type Deployer struct {
	clientset        kubernetes.Interface
	factory          informers.SharedInformerFactory
	namespace        string
	runID            string
	cliVersion       string // CLI version — forwarded to validator containers via AICR_CLI_VERSION for inner-image resolution
	cliCommit        string // CLI commit SHA — forwarded via AICR_CLI_COMMIT for dev-build image resolution
	entry            catalog.ValidatorEntry
	jobName          string // Unique name generated client-side (set by DeployJob)
	imagePullSecrets []string
	tolerations      []corev1.Toleration
	nodeSelector     map[string]string // passed through to inner workloads via AICR_NODE_SELECTOR env var
}

// NewDeployer creates a Deployer for a single validator catalog entry.
// The factory must be a namespace-scoped SharedInformerFactory started by the caller.
// cliVersion is the CLI's own version string; empty is acceptable for dev builds
// and is forwarded to the validator container via the AICR_CLI_VERSION env var so
// the validator can resolve images it references outside the catalog (e.g. the
// AIPerf benchmark image used by inference-perf) using the same rewriting
// rules as catalog.Load. cliCommit is the git commit SHA, forwarded via
// AICR_CLI_COMMIT for SHA-based image tag resolution in dev builds.
func NewDeployer(
	clientset kubernetes.Interface,
	factory informers.SharedInformerFactory,
	namespace, runID, cliVersion, cliCommit string,
	entry catalog.ValidatorEntry,
	imagePullSecrets []string,
	tolerations []corev1.Toleration,
	nodeSelector map[string]string,
) *Deployer {

	return &Deployer{
		clientset:        clientset,
		factory:          factory,
		namespace:        namespace,
		runID:            runID,
		cliVersion:       cliVersion,
		cliCommit:        cliCommit,
		entry:            entry,
		imagePullSecrets: imagePullSecrets,
		tolerations:      tolerations,
		nodeSelector:     nodeSelector,
	}
}

// JobName returns the Kubernetes Job name assigned by the API server.
// Empty until DeployJob is called.
func (d *Deployer) JobName() string {
	return d.jobName
}

// DeployJob applies the validator Job using server-side apply.
// A unique name is generated client-side and stored in d.jobName.
func (d *Deployer) DeployJob(ctx context.Context) error {
	d.jobName = generateJobName(d.entry.Name)
	job := d.buildApplyConfig()

	created, err := d.clientset.BatchV1().Jobs(d.namespace).Apply(
		ctx, job, metav1.ApplyOptions{FieldManager: labels.ValueAICR, Force: true},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to apply Job with prefix %s", d.entry.Name), err)
	}

	d.jobName = created.Name

	slog.Debug("validator Job applied",
		"job", d.jobName,
		"validator", d.entry.Name,
		"namespace", d.namespace)

	return nil
}

// generateJobName produces a unique Job name with a random suffix.
func generateJobName(validatorName string) string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return fmt.Sprintf("aicr-%s-%s", validatorName, hex.EncodeToString(suffix))
}

// CleanupJob deletes the validator Job with foreground propagation
// (waits for pod deletion).
func (d *Deployer) CleanupJob(ctx context.Context) error {
	if d.jobName == "" {
		return nil
	}
	propagation := metav1.DeletePropagationForeground
	err := d.clientset.BatchV1().Jobs(d.namespace).Delete(ctx, d.jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	return k8s.IgnoreNotFound(err)
}

// resolvedTimeout returns the catalog entry's timeout, falling back to the
// package default when unset (catalog Timeout == 0).
func (d *Deployer) resolvedTimeout() time.Duration {
	if d.entry.Timeout == 0 {
		return defaults.ValidatorDefaultTimeout
	}
	return d.entry.Timeout
}

func (d *Deployer) buildApplyConfig() *applybatchv1.JobApplyConfiguration {
	timeout := d.resolvedTimeout()

	return applybatchv1.Job(d.jobName, d.namespace).
		WithLabels(map[string]string{
			labels.Name:      labels.ValueAICR,
			labels.Component: labels.ValueValidation,
			labels.ManagedBy: labels.ValueAICR,
			labels.JobType:   labels.ValueValidation,
			labels.RunID:     d.runID,
			labels.Validator: d.entry.Name,
			labels.Phase:     d.entry.Phase,
		}).
		WithSpec(applybatchv1.JobSpec().
			WithActiveDeadlineSeconds(int64(timeout.Seconds())).
			WithBackoffLimit(0).
			WithTTLSecondsAfterFinished(int32(defaults.JobTTLAfterFinished.Seconds())).
			WithTemplate(applycorev1.PodTemplateSpec().
				WithLabels(map[string]string{
					labels.Name:      labels.ValueAICR,
					labels.Component: labels.ValueValidation,
					labels.Validator: d.entry.Name,
				}).
				WithSpec(d.buildPodSpecApply().
					WithContainers(applycorev1.Container().
						WithName("validator").
						WithImage(d.entry.Image).
						WithImagePullPolicy(d.imagePullPolicy()).
						WithArgs(d.entry.Args...).
						WithEnv(d.buildEnvApply()...).
						WithResources(applycorev1.ResourceRequirements().
							WithRequests(corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							}).
							WithLimits(corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							}),
						).
						WithTerminationMessagePath("/dev/termination-log").
						WithTerminationMessagePolicy(corev1.TerminationMessageReadFile).
						WithVolumeMounts(
							applycorev1.VolumeMount().WithName("snapshot").WithMountPath("/data/snapshot").WithReadOnly(true),
							applycorev1.VolumeMount().WithName("recipe").WithMountPath("/data/recipe").WithReadOnly(true),
						),
					).
					WithVolumes(
						applycorev1.Volume().WithName("snapshot").
							WithConfigMap(applycorev1.ConfigMapVolumeSource().WithName(fmt.Sprintf("aicr-snapshot-%s", d.runID))),
						applycorev1.Volume().WithName("recipe").
							WithConfigMap(applycorev1.ConfigMapVolumeSource().WithName(fmt.Sprintf("aicr-recipe-%s", d.runID))),
					),
				),
			),
		)
}

// orchestratorEnvMax upper-bounds the env vars buildEnvApply injects
// before appending the catalog entry's own Env slice. Used only as a
// capacity hint for the backing slice — append() resizes on overflow.
// Breakdown: 7 always-injected (SNAPSHOT_PATH, RECIPE_PATH, VALIDATOR_NAME,
// VALIDATOR_PHASE, RUN_ID, NAMESPACE, CHECK_TIMEOUT) plus up to 5
// conditionally-injected (NODE_SELECTOR, TOLERATIONS, CLI_VERSION,
// CLI_COMMIT, VALIDATOR_IMAGE_REGISTRY). Bump in lockstep when the
// injection list changes.
const orchestratorEnvMax = 12

func (d *Deployer) buildEnvApply() []*applycorev1.EnvVarApplyConfiguration {
	timeout := d.resolvedTimeout()
	env := make([]*applycorev1.EnvVarApplyConfiguration, 0, orchestratorEnvMax+len(d.entry.Env))
	env = append(env,
		applycorev1.EnvVar().WithName("AICR_SNAPSHOT_PATH").WithValue("/data/snapshot/snapshot.yaml"),
		applycorev1.EnvVar().WithName("AICR_RECIPE_PATH").WithValue("/data/recipe/recipe.yaml"),
		applycorev1.EnvVar().WithName("AICR_VALIDATOR_NAME").WithValue(d.entry.Name),
		applycorev1.EnvVar().WithName("AICR_VALIDATOR_PHASE").WithValue(d.entry.Phase),
		applycorev1.EnvVar().WithName("AICR_RUN_ID").WithValue(d.runID),
		applycorev1.EnvVar().WithName("AICR_NAMESPACE").
			WithValueFrom(applycorev1.EnvVarSource().
				WithFieldRef(applycorev1.ObjectFieldSelector().WithFieldPath("metadata.namespace"))),
		applycorev1.EnvVar().WithName("AICR_CHECK_TIMEOUT").WithValue(timeout.String()),
	)
	// Pass scheduling overrides to the validator container so it can apply them
	// to the inner workloads it creates (e.g., NCCL benchmark pods). These env
	// vars are NOT used to schedule the orchestrator Job itself.
	if len(d.nodeSelector) > 0 {
		env = append(env, applycorev1.EnvVar().WithName("AICR_NODE_SELECTOR").WithValue(serializeNodeSelector(d.nodeSelector)))
	}
	if len(d.tolerations) > 0 {
		env = append(env, applycorev1.EnvVar().WithName("AICR_TOLERATIONS").WithValue(serializeTolerations(d.tolerations)))
	}
	// Forward CLI version, image-registry override, and image-tag override so
	// the validator can resolve images it references outside the catalog
	// (e.g. inference-perf's aiperf-bench benchmark image) with the same
	// resolution semantics that catalog.Load applies to catalog entries.
	// All three must travel together: if a feature-branch dev build set
	// AICR_VALIDATOR_IMAGE_TAG=latest on the CLI side to get a published
	// outer validator image, the inner benchmark pod needs the same
	// override or it will resolve to the same unpublished :sha-<commit>
	// the outer pod would have hit without the override.
	if d.cliVersion != "" {
		env = append(env, applycorev1.EnvVar().WithName("AICR_CLI_VERSION").WithValue(d.cliVersion))
	}
	if d.cliCommit != "" {
		env = append(env, applycorev1.EnvVar().WithName("AICR_CLI_COMMIT").WithValue(d.cliCommit))
	}
	if override := os.Getenv("AICR_VALIDATOR_IMAGE_REGISTRY"); override != "" {
		env = append(env, applycorev1.EnvVar().WithName("AICR_VALIDATOR_IMAGE_REGISTRY").WithValue(override))
	}
	if tag := os.Getenv("AICR_VALIDATOR_IMAGE_TAG"); tag != "" {
		env = append(env, applycorev1.EnvVar().WithName("AICR_VALIDATOR_IMAGE_TAG").WithValue(tag))
	}
	for _, e := range d.entry.Env {
		env = append(env, applycorev1.EnvVar().WithName(e.Name).WithValue(e.Value))
	}
	return env
}

// serializeNodeSelector encodes a nodeSelector map as a comma-separated key=value string.
// Keys are sorted for deterministic output. This matches the format expected by
// snapshotter.ParseNodeSelectors on the receiving end.
func serializeNodeSelector(ns map[string]string) string {
	keys := make([]string, 0, len(ns))
	for k := range ns {
		keys = append(keys, k)
	}
	slices.Sort(keys)
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

// imagePullPolicy returns the appropriate pull policy for the outer validator
// Job. Delegates to catalog.ImagePullPolicy so the outer Job and any inner
// workload Jobs (e.g. inference-perf's aiperf-bench Job) apply the same
// rules — side-load / digest-pin / AICR_VALIDATOR_IMAGE_TAG override /
// :latest / versioned — without drifting.
func (d *Deployer) imagePullPolicy() corev1.PullPolicy {
	return catalog.ImagePullPolicy(d.entry.Image)
}

func (d *Deployer) buildImagePullSecretsApply() []*applycorev1.LocalObjectReferenceApplyConfiguration {
	refs := make([]*applycorev1.LocalObjectReferenceApplyConfiguration, 0, len(d.imagePullSecrets))
	for _, name := range d.imagePullSecrets {
		refs = append(refs, applycorev1.LocalObjectReference().WithName(name))
	}
	return refs
}

func (d *Deployer) buildPodSpecApply() *applycorev1.PodSpecApplyConfiguration {
	// The orchestrator Job always tolerates all taints so it can schedule on any
	// available CPU node. User-provided tolerations (--toleration flag) are forwarded
	// to inner workloads via AICR_TOLERATIONS and do not affect orchestrator scheduling.
	return applycorev1.PodSpec().
		WithServiceAccountName(ServiceAccountName).
		WithRestartPolicy(corev1.RestartPolicyNever).
		WithTerminationGracePeriodSeconds(int64(defaults.ValidatorTerminationGracePeriod.Seconds())).
		WithImagePullSecrets(d.buildImagePullSecretsApply()...).
		WithTolerations(applycorev1.Toleration().WithOperator(corev1.TolerationOpExists)).
		WithAffinity(preferCPUNodeAffinityApply())
}

// WaitForCompletion watches the Job until it reaches a terminal state
// (Complete or Failed). Returns nil for both — the caller uses ExtractResult
// to determine pass/fail/skip from the exit code.
//
// Returns error only for infrastructure failures (watch error, timeout).
// Job failure (exit != 0) is NOT an error return — that decision lives here
// in the validator orchestrator, not in the shared pod.WaitForJobTerminal
// helper, which intentionally treats both Complete and Failed Jobs as
// legitimate completions and lets the caller classify them.
func (d *Deployer) WaitForCompletion(ctx context.Context, timeout time.Duration) error {
	waitTimeout := timeout + defaults.ValidatorWaitBuffer
	// pod.WaitForJobTerminal already returns structured errors with proper
	// codes (ErrCodeTimeout, ErrCodeUnavailable, ErrCodeInternal). Propagate
	// as-is so callers can distinguish retryable from terminal failures.
	if _, err := pod.WaitForJobTerminal(ctx, d.clientset, d.namespace, d.jobName, waitTimeout); err != nil {
		return err
	}
	return nil
}

// WaitForPodTermination watches the Job's pod until it reaches a terminal
// state. Prevents RBAC cleanup from racing with in-progress pod operations.
//
// Returns the underlying error from pod.WaitForTermination so callers can
// decide log severity. A nil error means the pod is gone or terminal; a
// non-nil error means the wait was abandoned (timeout, watch failure, or
// repeated watch closures) and the cleanup may race with an in-progress pod.
func (d *Deployer) WaitForPodTermination(ctx context.Context) error {
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		// Pod-not-found is the expected steady state once the Job's TTL
		// controller or foreground-propagation delete has already run.
		// Anything else (RBAC, transient API failure, timeout) must
		// propagate so the caller can decide whether to retry or escalate.
		var sErr *errors.StructuredError
		if stderrors.As(err, &sErr) && sErr.Code == errors.ErrCodeNotFound {
			slog.Debug("no pod found, skipping termination wait", "job", d.jobName)
			return nil
		}
		return err
	}

	if jobPod.Status.Phase == corev1.PodSucceeded || jobPod.Status.Phase == corev1.PodFailed {
		return nil
	}

	slog.Debug("waiting for pod termination", "pod", jobPod.Name)
	return pod.WaitForTermination(ctx, d.clientset, d.namespace, jobPod.Name)
}

func preferCPUNodeAffinityApply() *applycorev1.AffinityApplyConfiguration {
	return applycorev1.Affinity().WithNodeAffinity(
		applycorev1.NodeAffinity().WithPreferredDuringSchedulingIgnoredDuringExecution(
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
}
