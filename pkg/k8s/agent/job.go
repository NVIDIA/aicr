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

package agent

import (
	"context"
	"strconv"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/pkg/recipe/oskind"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
)

// ensureJob deletes any existing Job and creates a fresh one.
func (d *Deployer) ensureJob(ctx context.Context) error {
	// Delete existing Job if present
	propagationPolicy := metav1.DeletePropagationForeground
	err := d.clientset.BatchV1().Jobs(d.config.Namespace).Delete(
		ctx,
		d.config.JobName,
		metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		},
	)
	if err != nil && !errors.IsNotFound(err) {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to delete existing Job", err)
	}

	// Wait for Job to be fully deleted
	jobExisted := err == nil // Job existed and was deleted
	if jobExisted {
		if waitErr := d.waitForJobDeletion(ctx); waitErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeTimeout, "timeout waiting for Job deletion", waitErr)
		}
	}

	// Create fresh Job
	job := d.buildJob()
	_, err = d.clientset.BatchV1().Jobs(d.config.Namespace).
		Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create Job", err)
	}

	return nil
}

// buildJob constructs the Job specification.
func (d *Deployer) buildJob() *batchv1.Job {
	// Build command arguments (directly invoke binary without shell)
	args := []string{"snapshot", "-o", d.config.Output}
	if d.config.Debug {
		args = []string{"--debug", "--log-json", "snapshot", "-o", d.config.Output}
	}

	// Build pod spec based on privileged mode
	podSpec := d.buildPodSpec(args)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.config.JobName,
			Namespace: d.config.Namespace,
			Labels: map[string]string{
				labelAppName: appName,
			},
		},
		Spec: batchv1.JobSpec{
			Completions:             ptr.To(int32(1)),
			Parallelism:             ptr.To(int32(1)),
			CompletionMode:          ptr.To(batchv1.NonIndexedCompletion),
			BackoffLimit:            ptr.To(int32(0)),
			TTLSecondsAfterFinished: ptr.To(int32(defaults.JobTTLAfterFinished.Seconds())),
			ActiveDeadlineSeconds:   ptr.To(int64(defaults.AgentJobActiveDeadline.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						labelAppName: appName,
					},
				},
				Spec: podSpec,
			},
		},
	}
}

// buildPodSpec constructs the pod specification.
// When Privileged=true: enables hostPID, hostNetwork, privileged container for full collector access.
// When Privileged=false: PSS-compliant restricted pod, only K8s collector works.
func (d *Deployer) buildPodSpec(args []string) corev1.PodSpec {
	spec := corev1.PodSpec{
		ServiceAccountName: d.config.ServiceAccountName,
		RestartPolicy:      corev1.RestartPolicyNever,
		NodeSelector:       d.config.NodeSelector,
		Tolerations:        d.config.Tolerations,
		ImagePullSecrets:   toLocalObjectReferences(d.config.ImagePullSecrets),
		Containers: []corev1.Container{
			{
				Name:    "aicr",
				Image:   d.config.Image,
				Command: []string{"aicr"},
				Args:    args,
				Env:     d.buildEnvVars(),
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "tmp",
						MountPath: "/tmp",
					},
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "tmp",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		},
	}

	if d.config.RuntimeClassName != "" {
		spec.RuntimeClassName = ptr.To(d.config.RuntimeClassName)
	}

	if d.config.Privileged {
		d.applyPrivilegedSettings(&spec)
	} else {
		d.applyRestrictedSettings(&spec)
	}

	return spec
}

// applyPrivilegedSettings configures the pod for privileged mode (GPU/SystemD/OS collectors).
func (d *Deployer) applyPrivilegedSettings(spec *corev1.PodSpec) {
	spec.HostPID = true
	spec.HostNetwork = true
	spec.HostIPC = true
	spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsUser:           ptr.To(int64(0)),
		RunAsGroup:          ptr.To(int64(0)),
		FSGroup:             ptr.To(int64(0)),
		FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
	}

	container := &spec.Containers[0]
	requests := mergeResourceList(corev1.ResourceList{
		corev1.ResourceCPU:              mustParseQuantity("1"),
		corev1.ResourceMemory:           mustParseQuantity("4Gi"),
		corev1.ResourceEphemeralStorage: mustParseQuantity("2Gi"),
	}, d.config.Requests)
	limits := mergeResourceList(corev1.ResourceList{
		corev1.ResourceCPU:              mustParseQuantity("2"),
		corev1.ResourceMemory:           mustParseQuantity("8Gi"),
		corev1.ResourceEphemeralStorage: mustParseQuantity("4Gi"),
	}, d.config.Limits)
	// RequireGPU defaults the nvidia.com/gpu limit to 1 only when the
	// caller has not supplied an explicit value via --limits. Caller
	// override wins so a user can request multiple GPUs (e.g.
	// --require-gpu --limits nvidia.com/gpu=4) without the default
	// silently truncating it back to 1.
	if d.config.RequireGPU {
		gpuKey := corev1.ResourceName("nvidia.com/gpu")
		if _, ok := limits[gpuKey]; !ok {
			limits[gpuKey] = mustParseQuantity("1")
		}
	}
	container.Resources = corev1.ResourceRequirements{
		Requests: requests,
		Limits:   limits,
	}
	container.SecurityContext = &corev1.SecurityContext{
		Privileged:               ptr.To(true),
		RunAsUser:                ptr.To(int64(0)),
		RunAsGroup:               ptr.To(int64(0)),
		AllowPrivilegeEscalation: ptr.To(true),
		Capabilities: &corev1.Capabilities{
			Add: []corev1.Capability{"SYS_ADMIN", "SYS_CHROOT"},
		},
	}

	// Talos has no systemd and no /etc/os-release on the host filesystem; the
	// Talos service collector reads service state from the Kubernetes API
	// instead. Skipping these hostPath mounts is what unblocks agent
	// deployment on Talos clusters (the systemd hostPath mount is the
	// documented blocker — see issue #565).
	if d.config.OS == oskind.Talos {
		return
	}

	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{
			Name:      "run-systemd",
			MountPath: "/run/systemd",
			ReadOnly:  true,
		},
		corev1.VolumeMount{
			Name:      "host-os-release",
			MountPath: "/etc/os-release",
			ReadOnly:  true,
		},
	)

	spec.Volumes = append(spec.Volumes,
		corev1.Volume{
			Name: "run-systemd",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/run/systemd",
					Type: ptr.To(corev1.HostPathDirectory),
				},
			},
		},
		corev1.Volume{
			Name: "host-os-release",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/os-release",
					Type: ptr.To(corev1.HostPathFile),
				},
			},
		},
	)
}

// applyRestrictedSettings configures the pod for PSS-restricted namespaces (K8s collector only).
func (d *Deployer) applyRestrictedSettings(spec *corev1.PodSpec) {
	spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		RunAsUser:    ptr.To(int64(65534)), // nobody
		RunAsGroup:   ptr.To(int64(65534)),
		FSGroup:      ptr.To(int64(65534)),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}

	container := &spec.Containers[0]
	container.Resources = corev1.ResourceRequirements{
		Requests: mergeResourceList(corev1.ResourceList{
			corev1.ResourceCPU:              mustParseQuantity("100m"),
			corev1.ResourceMemory:           mustParseQuantity("256Mi"),
			corev1.ResourceEphemeralStorage: mustParseQuantity("256Mi"),
		}, d.config.Requests),
		Limits: mergeResourceList(corev1.ResourceList{
			corev1.ResourceCPU:              mustParseQuantity("500m"),
			corev1.ResourceMemory:           mustParseQuantity("512Mi"),
			corev1.ResourceEphemeralStorage: mustParseQuantity("512Mi"),
		}, d.config.Limits),
	}
	container.SecurityContext = &corev1.SecurityContext{
		Privileged:               ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
		RunAsUser:                ptr.To(int64(65534)),
		RunAsGroup:               ptr.To(int64(65534)),
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// buildEnvVars constructs the environment variables for the agent container.
func (d *Deployer) buildEnvVars() []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		{
			Name:  "AICR_AGENT_MODE",
			Value: "true",
		},
		{
			Name:  "AICR_LOG_PREFIX",
			Value: "agent",
		},
		{
			Name: "NODE_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "spec.nodeName",
				},
			},
		},
	}

	if d.config.MaxNodesPerEntry > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AICR_MAX_NODES_PER_ENTRY",
			Value: strconv.Itoa(d.config.MaxNodesPerEntry),
		})
	}

	if d.config.OS != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AICR_OS",
			Value: d.config.OS,
		})
	}

	// NVIDIA_VISIBLE_DEVICES=all is set explicitly here because no GPU resource is
	// requested (we rely on runtimeClassName to get container-runtime access to the
	// driver). On CDI-enabled clusters the runtime would normally inject this via the
	// CDI spec; setting it explicitly is intentional for this non-allocation path.
	if d.config.RuntimeClassName != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "NVIDIA_VISIBLE_DEVICES",
			Value: "all",
		})
	}

	return envVars
}

// deleteJob deletes the Job.
func (d *Deployer) deleteJob(ctx context.Context) error {
	propagationPolicy := metav1.DeletePropagationForeground
	err := d.clientset.BatchV1().Jobs(d.config.Namespace).Delete(
		ctx,
		d.config.JobName,
		metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		},
	)
	return k8s.IgnoreNotFound(err)
}

// waitForJobDeletion waits for the Job to be fully deleted using the watch API.
// Returns nil when the Job is observed deleted (Get returns NotFound, or a
// watch.Deleted event is received). Returns ErrCodeTimeout if the cleanup
// deadline elapses before deletion is observed.
func (d *Deployer) waitForJobDeletion(ctx context.Context) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()

	// Fast path: already deleted. Note: IgnoreNotFound(nil) returns nil,
	// so check NotFound explicitly — otherwise a successful Get (Job still
	// exists) would incorrectly short-circuit as "deleted".
	current, err := d.clientset.BatchV1().Jobs(d.config.Namespace).
		Get(timeoutCtx, d.config.JobName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to get Job", err)
	}

	watcher, err := d.clientset.BatchV1().Jobs(d.config.Namespace).Watch(timeoutCtx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + d.config.JobName,
		ResourceVersion: current.ResourceVersion,
	})
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to watch Job", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return aicrerrors.Wrap(aicrerrors.ErrCodeTimeout, "Job deletion wait timeout", timeoutCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				// Channel closed; verify with a Get to handle missed events.
				// Use explicit NotFound check (IgnoreNotFound(nil) returns nil
				// and would falsely report success when the Job still exists).
				_, getErr := d.clientset.BatchV1().Jobs(d.config.Namespace).
					Get(timeoutCtx, d.config.JobName, metav1.GetOptions{})
				if errors.IsNotFound(getErr) {
					return nil
				}
				if getErr != nil {
					return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "Job watch channel closed", getErr)
				}
				return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable,
					"Job watch channel closed before deletion observed", nil)
			}
			if event.Type == watch.Deleted {
				return nil
			}
		}
	}
}

// mustParseQuantity parses a resource quantity or panics.
func mustParseQuantity(s string) resource.Quantity {
	q := resource.MustParse(s)
	return q
}

// mergeResourceList returns a fresh ResourceList where every key in
// override replaces the same key in defaults; keys present only in
// defaults are preserved unchanged. Used by applyPrivilegedSettings
// and applyRestrictedSettings to honor --requests / --limits CLI
// overrides without forcing the caller to specify every resource.
func mergeResourceList(defaults, override corev1.ResourceList) corev1.ResourceList {
	merged := make(corev1.ResourceList, len(defaults))
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// toLocalObjectReferences converts a slice of secret names to LocalObjectReferences.
func toLocalObjectReferences(names []string) []corev1.LocalObjectReference {
	if len(names) == 0 {
		return nil
	}
	refs := make([]corev1.LocalObjectReference, len(names))
	for i, name := range names {
		refs[i] = corev1.LocalObjectReference{Name: name}
	}
	return refs
}
