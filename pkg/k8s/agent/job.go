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
	"time"

	eidoserrors "github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/k8s"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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
		return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to delete existing Job", err)
	}

	// Wait for Job to be fully deleted
	jobExisted := err == nil // Job existed and was deleted
	if jobExisted {
		if waitErr := d.waitForJobDeletion(ctx); waitErr != nil {
			return eidoserrors.Wrap(eidoserrors.ErrCodeTimeout, "timeout waiting for Job deletion", waitErr)
		}
	}

	// Create fresh Job
	job := d.buildJob()
	_, err = d.clientset.BatchV1().Jobs(d.config.Namespace).
		Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to create Job", err)
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
				"app.kubernetes.io/name": "eidos",
			},
		},
		Spec: batchv1.JobSpec{
			Completions:             ptr.To(int32(1)),
			Parallelism:             ptr.To(int32(1)),
			CompletionMode:          ptr.To(batchv1.NonIndexedCompletion),
			BackoffLimit:            ptr.To(int32(0)),
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			ActiveDeadlineSeconds:   ptr.To(int64(18000)), // 5 hours
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "eidos",
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
				Name:    "eidos",
				Image:   d.config.Image,
				Command: []string{"eidos"},
				Args:    args,
				Env: []corev1.EnvVar{
					{
						Name:  "Eidos_LOG_PREFIX",
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
				},
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
	limits := corev1.ResourceList{
		corev1.ResourceCPU:              mustParseQuantity("2"),
		corev1.ResourceMemory:           mustParseQuantity("8Gi"),
		corev1.ResourceEphemeralStorage: mustParseQuantity("4Gi"),
	}
	if d.config.RequireGPU {
		limits[corev1.ResourceName("nvidia.com/gpu")] = mustParseQuantity("1")
	}
	container.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              mustParseQuantity("1"),
			corev1.ResourceMemory:           mustParseQuantity("4Gi"),
			corev1.ResourceEphemeralStorage: mustParseQuantity("2Gi"),
		},
		Limits: limits,
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
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              mustParseQuantity("100m"),
			corev1.ResourceMemory:           mustParseQuantity("256Mi"),
			corev1.ResourceEphemeralStorage: mustParseQuantity("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              mustParseQuantity("500m"),
			corev1.ResourceMemory:           mustParseQuantity("512Mi"),
			corev1.ResourceEphemeralStorage: mustParseQuantity("512Mi"),
		},
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

// waitForJobDeletion waits for the Job to be fully deleted.
func (d *Deployer) waitForJobDeletion(ctx context.Context) error {
	timeout := 30 * time.Second
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, timeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := d.clientset.BatchV1().Jobs(d.config.Namespace).
				Get(ctx, d.config.JobName, metav1.GetOptions{})
			if k8s.IgnoreNotFound(err) == nil {
				return true, nil // Job deleted successfully
			}
			if err != nil {
				return false, err
			}
			return false, nil // Job still exists, keep waiting
		},
	)
}

// mustParseQuantity parses a resource quantity or panics.
func mustParseQuantity(s string) resource.Quantity {
	q := resource.MustParse(s)
	return q
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
