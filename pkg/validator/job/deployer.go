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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
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
	entry            catalog.ValidatorEntry
	jobName          string // Unique name generated client-side (set by DeployJob)
	imagePullSecrets []string
	tolerations      []corev1.Toleration
	nodeSelector     map[string]string
}

// NewDeployer creates a Deployer for a single validator catalog entry.
// The factory must be a namespace-scoped SharedInformerFactory started by the caller.
func NewDeployer(
	clientset kubernetes.Interface,
	factory informers.SharedInformerFactory,
	namespace, runID string,
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

func (d *Deployer) buildApplyConfig() *applybatchv1.JobApplyConfiguration {
	timeout := d.entry.Timeout
	if timeout == 0 {
		timeout = defaults.ValidatorDefaultTimeout
	}

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

func (d *Deployer) buildEnvApply() []*applycorev1.EnvVarApplyConfiguration {
	orchestratorEnvCount := 6
	env := make([]*applycorev1.EnvVarApplyConfiguration, 0, orchestratorEnvCount+len(d.entry.Env))
	env = append(env,
		applycorev1.EnvVar().WithName("AICR_SNAPSHOT_PATH").WithValue("/data/snapshot/snapshot.yaml"),
		applycorev1.EnvVar().WithName("AICR_RECIPE_PATH").WithValue("/data/recipe/recipe.yaml"),
		applycorev1.EnvVar().WithName("AICR_VALIDATOR_NAME").WithValue(d.entry.Name),
		applycorev1.EnvVar().WithName("AICR_VALIDATOR_PHASE").WithValue(d.entry.Phase),
		applycorev1.EnvVar().WithName("AICR_RUN_ID").WithValue(d.runID),
		applycorev1.EnvVar().WithName("AICR_NAMESPACE").
			WithValueFrom(applycorev1.EnvVarSource().
				WithFieldRef(applycorev1.ObjectFieldSelector().WithFieldPath("metadata.namespace"))),
	)
	for _, e := range d.entry.Env {
		env = append(env, applycorev1.EnvVar().WithName(e.Name).WithValue(e.Value))
	}
	return env
}

// imagePullPolicy returns Always when the image uses :latest tag (dev builds),
// PullIfNotPresent otherwise. This ensures dev builds always pull fresh images
// and avoids exec format errors from stale cached images on cluster nodes.
func (d *Deployer) imagePullPolicy() corev1.PullPolicy {
	if strings.HasSuffix(d.entry.Image, ":latest") {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

func (d *Deployer) buildImagePullSecretsApply() []*applycorev1.LocalObjectReferenceApplyConfiguration {
	refs := make([]*applycorev1.LocalObjectReferenceApplyConfiguration, 0, len(d.imagePullSecrets))
	for _, name := range d.imagePullSecrets {
		refs = append(refs, applycorev1.LocalObjectReference().WithName(name))
	}
	return refs
}

func (d *Deployer) buildPodSpecApply() *applycorev1.PodSpecApplyConfiguration {
	spec := applycorev1.PodSpec().
		WithServiceAccountName(ServiceAccountName).
		WithRestartPolicy(corev1.RestartPolicyNever).
		WithTerminationGracePeriodSeconds(int64(defaults.ValidatorTerminationGracePeriod.Seconds())).
		WithImagePullSecrets(d.buildImagePullSecretsApply()...).
		WithTolerations(d.buildTolerationsApply()...).
		WithAffinity(preferCPUNodeAffinityApply())

	if len(d.nodeSelector) > 0 {
		spec = spec.WithNodeSelector(d.nodeSelector)
	}

	return spec
}

func (d *Deployer) buildTolerationsApply() []*applycorev1.TolerationApplyConfiguration {
	tols := make([]*applycorev1.TolerationApplyConfiguration, 0, len(d.tolerations))
	for i := range d.tolerations {
		t := &d.tolerations[i]
		tol := applycorev1.Toleration().WithOperator(t.Operator)
		if t.Key != "" {
			tol = tol.WithKey(t.Key)
		}
		if t.Value != "" {
			tol = tol.WithValue(t.Value)
		}
		if t.Effect != "" {
			tol = tol.WithEffect(t.Effect)
		}
		if t.TolerationSeconds != nil {
			tol = tol.WithTolerationSeconds(*t.TolerationSeconds)
		}
		tols = append(tols, tol)
	}
	return tols
}

// WaitForCompletion watches the Job until it reaches a terminal state
// (Complete or Failed). Returns nil for both — the caller uses ExtractResult
// to determine pass/fail/skip from the exit code.
//
// Returns error only for infrastructure failures (watch error, timeout).
// Job failure (exit != 0) is NOT an error return.
func (d *Deployer) WaitForCompletion(ctx context.Context, timeout time.Duration) error {
	waitTimeout := timeout + defaults.ValidatorWaitBuffer
	timeoutCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	// Fast path: Job may already be terminal.
	currentJob, err := d.clientset.BatchV1().Jobs(d.namespace).Get(timeoutCtx, d.jobName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get Job", err)
	}
	if terminal, _ := checkJobTerminal(currentJob); terminal {
		return nil
	}

	// Watch for state changes, starting from the current resourceVersion.
	watcher, err := d.clientset.BatchV1().Jobs(d.namespace).Watch(timeoutCtx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + d.jobName,
		ResourceVersion: currentJob.ResourceVersion,
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch Job", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "validator wait timeout", timeoutCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				// Watch channel closed — re-check Job status directly.
				recheck, recheckErr := d.clientset.BatchV1().Jobs(d.namespace).Get(timeoutCtx, d.jobName, metav1.GetOptions{})
				if recheckErr != nil {
					return errors.Wrap(errors.ErrCodeInternal, "watch closed and Job not found", recheckErr)
				}
				if terminal, _ := checkJobTerminal(recheck); terminal {
					return nil
				}
				return errors.New(errors.ErrCodeInternal, "watch channel closed, Job still running")
			}

			if event.Type == watch.Deleted {
				return errors.New(errors.ErrCodeInternal, "Job was deleted externally")
			}

			watchedJob, ok := event.Object.(*batchv1.Job)
			if !ok {
				continue
			}
			if terminal, _ := checkJobTerminal(watchedJob); terminal {
				return nil
			}
		}
	}
}

// WaitForPodTermination watches the Job's pod until it reaches a terminal
// state. Prevents RBAC cleanup from racing with in-progress pod operations.
func (d *Deployer) WaitForPodTermination(ctx context.Context) {
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		slog.Debug("no pod found, skipping termination wait", "job", d.jobName)
		return
	}

	if jobPod.Status.Phase == corev1.PodSucceeded || jobPod.Status.Phase == corev1.PodFailed {
		return
	}

	slog.Debug("waiting for pod termination", "pod", jobPod.Name)

	watcher, err := d.clientset.CoreV1().Pods(d.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + jobPod.Name,
		ResourceVersion: jobPod.ResourceVersion,
	})
	if err != nil {
		slog.Warn("failed to watch pod for termination", "error", err)
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Warn("timed out waiting for pod termination", "pod", jobPod.Name)
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}
			if event.Type == watch.Deleted {
				return
			}
			watchedPod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if watchedPod.Status.Phase == corev1.PodSucceeded || watchedPod.Status.Phase == corev1.PodFailed {
				return
			}
		}
	}
}

// checkJobTerminal returns true if the Job is in a terminal state (Complete or Failed).
func checkJobTerminal(job *batchv1.Job) (bool, string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return true, c.Reason
		}
	}
	return false, ""
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
