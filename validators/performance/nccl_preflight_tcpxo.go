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

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	k8spod "github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
)

const (
	// tcpxoHostNvidiaPath is the host directory the nccl-tcpxo-installer
	// DaemonSet populates on every GKE GPU node. The worker pod hostPath-mounts
	// it as /usr/local/nvidia (see testdata/h100/gke/runtime.yaml volumes) and
	// sources nccl-env-profile.sh from it before starting sshd. Preflight mounts
	// the same host path to assert the installer has finished.
	tcpxoHostNvidiaPath = "/home/kubernetes/bin/nvidia"

	// tcpxoEnvProfileRelPath is the nccl-env-profile.sh location relative to
	// tcpxoHostNvidiaPath. Its presence is the reliable "installer finished"
	// signal: the worker startup chain fails at `. .../nccl-env-profile.sh`
	// (a `&&` link) when it is absent, so sshd never starts and the launcher's
	// mpirun cannot reach the worker.
	tcpxoEnvProfileRelPath = "lib64/nccl-env-profile.sh"

	// tcpxoProbePodNamePrefix is the generateName seed for the per-node TCPXO
	// readiness probe pods. Kept short so the full name (node hash + rand
	// suffix) stays within the 63-char DNS-1123 label limit.
	tcpxoProbePodNamePrefix = "nccl-tcpxo-probe-"

	// tcpxoDocsHint is the operator-facing message emitted when a node is not
	// TCPXO-ready. Keeps the fix discoverable without leaving the failure text.
	tcpxoDocsHint = `The GKE GPUDirect-TCPXO plugin (nccl-env-profile.sh and the FastRak ` +
		`libraries under ` + tcpxoHostNvidiaPath + `) is installed by the ` +
		`nccl-tcpxo-installer DaemonSet from the gke-nccl-tcpxo component. On ` +
		`freshly provisioned nodes the DaemonSet may not have finished when this ` +
		`check runs; without it the NCCL worker pods fail to start sshd and the ` +
		`launcher mpirun cannot connect. Confirm the DaemonSet is Ready on every ` +
		`GPU node (kubectl get ds -A | grep tcpxo) and re-run. See ` +
		`docs/integrator/gke-tcpxo-networking.md.`
)

// hostPathDirOrCreate is the addressable HostPath type for the preflight probe
// volume (HostPathVolumeSource.Type is a *HostPathType).
var hostPathDirOrCreate = corev1.HostPathDirectoryOrCreate

// gkeTCPXOPreflightApplies reports whether the TCPXO readiness preflight should
// run for the given (variant, accelerator, service) tuple. Mirrors
// gb200NetPreflightApplies to keep the call site in validateNcclAllReduceBw
// uncluttered. Scoped to the GKE H100 default-variant path — the only
// combination that renders testdata/h100/gke/runtime.yaml with the TCPXO
// sidecar and host-artifact dependency.
func gkeTCPXOPreflightApplies(variant ncclVariant, accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType) bool {
	return variant == variantDefault &&
		service == recipe.CriteriaServiceGKE &&
		accelerator == recipe.CriteriaAcceleratorH100
}

// preflightGKETCPXOReady verifies that every target GPU node has the
// GPUDirect-TCPXO host artifacts (nccl-env-profile.sh under
// tcpxoHostNvidiaPath) that the worker pod depends on. Called only for the
// GKE H100 default variant. Turns an otherwise opaque, minutes-later launcher
// failure ("pod failed") into a fast, actionable error naming the unready
// nodes.
//
// The check runs one short-lived Pod per target node, pinned via NodeName,
// with tcpxoHostNvidiaPath hostPath-mounted read-only. The pod tests for the
// profile script and exits 0 if present, non-zero if absent. Per-node results
// are consolidated into a single error so operators see every unready node at
// once. Mirrors preflightGB200NetNVregFlag.
func preflightGKETCPXOReady(ctx *validators.Context, nodes []corev1.Node) error {
	if len(nodes) == 0 {
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			"preflight called with no target nodes")
	}

	slog.Info("GKE preflight: checking GPUDirect-TCPXO readiness on GPU nodes",
		"nodes", len(nodes))

	missing, err := runPerNodeProbe(ctx, nodes, "TCPXO", checkTCPXOOnNode)
	if err != nil {
		return err
	}

	if len(missing) > 0 {
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("GPUDirect-TCPXO not ready on GPU nodes: %s. %s",
				strings.Join(missing, ", "), tcpxoDocsHint))
	}

	slog.Info("GKE preflight passed: GPUDirect-TCPXO ready on all target nodes",
		"nodes", len(nodes))
	return nil
}

// checkTCPXOOnNode creates and waits on a short-lived probe pod that tests for
// the nccl-env-profile.sh host artifact on a specific node. Returns (true, nil)
// if present, (false, nil) if absent (installer not finished), or (_, err) on
// any other failure (pod schedule, image pull, hostPath denied).
func checkTCPXOOnNode(ctx context.Context, clientset kubernetes.Interface, namespace, nodeName string) (bool, error) {
	podsClient := clientset.CoreV1().Pods(namespace)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: tcpxoProbePodNamePrefix,
			Namespace:    namespace,
			Labels: map[string]string{
				"app.kubernetes.io/component":  "nccl-tcpxo-preflight",
				"app.kubernetes.io/managed-by": "aicr-validator",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			// The probe only reads a hostPath and never calls the Kubernetes API,
			// so don't mount an API token onto it (defense-in-depth with the hostPath).
			AutomountServiceAccountToken: ptr.To(false),
			// Tolerate whatever taints the GPU nodes carry. The probe is cheap
			// (busybox + test) so we accept wherever scheduler places us on the
			// target node.
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   defaults.ProbeImage,
				Command: []string{shellBin, "-c"},
				Args: []string{
					// Print a marker on the miss path so checkTCPXOOnNode can
					// distinguish "artifact absent" (expected, exit 1 with the
					// marker) from a harder failure (image pull, hostPath denied)
					// that leaves empty output.
					"test -f /host-nvidia/" + tcpxoEnvProfileRelPath + " || { echo tcpxo-artifact-missing; exit 1; }",
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "host-nvidia",
					MountPath: "/host-nvidia",
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "host-nvidia",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: tcpxoHostNvidiaPath,
						// DirectoryOrCreate (what containerd does implicitly) so the
						// exact case this preflight exists to catch — the installer
						// DaemonSet never ran, so the dir is absent — deterministically
						// yields an empty dir the probe reports as not-ready, rather
						// than a mount failure that hangs the pod in ContainerCreating
						// until DiagnosticTimeout and aborts the fan-out as an internal
						// error. The empty dir is harmless: the installer owns its
						// contents and populates it when it runs.
						Type: &hostPathDirOrCreate,
					},
				},
			}},
		},
	}

	created, err := podsClient.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"failed to create TCPXO preflight pod", err)
	}
	// Cleanup runs on an independent context so it fires even when the probe
	// context has been canceled (timeout, parallel-sibling error).
	defer func() { //nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.PreflightCleanupTimeout)
		defer cancel()
		if delErr := podsClient.Delete(cleanupCtx, created.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			slog.Warn("failed to delete TCPXO preflight pod", "pod", created.Name, "err", delErr)
		}
	}()

	phase, err := waitForPreflightPodPhase(ctx, clientset, namespace, created.Name, defaults.DiagnosticTimeout)
	if err != nil {
		return false, err
	}

	if phase == corev1.PodSucceeded {
		return true, nil
	}

	// Failed: distinguish artifact-absent (our marker on stdout) from harder
	// errors (image pull, hostPath denied) that leave empty output.
	logs, logErr := k8spod.GetPodLogs(ctx, clientset, namespace, created.Name, "probe")
	if logErr != nil {
		return false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"TCPXO preflight pod Failed and logs were unreadable", logErr)
	}
	if !strings.Contains(logs, "tcpxo-artifact-missing") {
		return false, aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("TCPXO preflight pod on node %q Failed for an unexpected reason (output: %q)",
				nodeName, strings.TrimSpace(logs)))
	}
	return false, nil
}
