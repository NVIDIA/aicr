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
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	k8spod "github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
)

// grdmaPciTopoCheckOverridePattern is the full-line pattern matched both by
// the in-pod grep argument and by parseNVregFromParams(). Centralized so the
// two sites cannot drift.
const grdmaPciTopoCheckOverridePattern = `^GrdmaPciTopoCheckOverride: 1$`

// grdmaPciTopoCheckOverrideRe wraps the pattern with multiline anchoring for
// the pure-function parser; grep handles multiline matching natively.
var grdmaPciTopoCheckOverrideRe = regexp.MustCompile(`(?m)` + grdmaPciTopoCheckOverridePattern)

// parseNVregFromParams reports whether /proc/driver/nvidia/params content has
// NVreg_GrdmaPciTopoCheckOverride set to 1. Pure function for unit testing;
// the pod-based check uses grep with the same pattern.
func parseNVregFromParams(content string) bool {
	return grdmaPciTopoCheckOverrideRe.MatchString(content)
}

const (
	// preflightNodeConcurrency caps the number of in-flight per-node probe
	// Pods. Large enough to keep wall-clock low on the typical (<=64 node)
	// GB200 cluster while bounding apiserver and scheduler pressure on
	// larger clusters.
	preflightNodeConcurrency = 16

	// preflightPodNamePrefix is the generateName seed for the per-node probe
	// pods. Short so the full name (including node hash + rand suffix) fits
	// inside the 63-character DNS-1123 label limit on all realistic node
	// names.
	preflightPodNamePrefix = "nccl-nvreg-probe-"

	// nvregDocsHint is the cluster-operator-facing message the preflight
	// emits when the flag is missing. Keeps the fix one `kubectl` away.
	nvregDocsHint = `NVreg_GrdmaPciTopoCheckOverride=1 is required on p6e-gb200 EKS nodes so ` +
		`the NVIDIA driver allows EFA (a PCIe-attached NIC) to attach dma-buf ` +
		`handles for GPU HBM on the Grace CPU topology. Without it, the kernel ` +
		`rejects the attach with "NVRM: dma-buf attach failed: topology not ` +
		`supported for mapping type FORCE_PCIE" and NCCL silently falls back ` +
		`to the Socket transport. Set it via the GPU Operator ClusterPolicy: ` +
		`spec.driver.kernelModuleConfig.name → a ConfigMap in gpu-operator ` +
		`with data "nvidia.conf: options nvidia NVreg_GrdmaPciTopoCheckOverride=1", ` +
		`then delete the nvidia-driver DaemonSet pods to pick up the change.`
)

// preflightGB200NetNVregFlag verifies that every target GPU node has the
// NVIDIA kernel driver loaded with NVreg_GrdmaPciTopoCheckOverride=1. Called
// only for the NET variant on GB200/EKS — this is the knob that determines
// whether EFA GPUDirect RDMA works on the GB200 PCI topology. NVLS (MNNVL)
// traffic stays on NVLink-C2C and does not need it.
//
// The check runs one short-lived Pod per target node, pinned via NodeName,
// with /proc/driver/nvidia hostPath-mounted read-only. The pod greps for the
// parameter line and exits 0 if found, non-zero if missing. The validator
// consolidates per-node results into a single error so operators see every
// misconfigured node at once rather than one-at-a-time.
func preflightGB200NetNVregFlag(ctx *validators.Context, nodes []corev1.Node) error {
	if len(nodes) == 0 {
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			"preflight called with no target nodes")
	}

	slog.Info("NET preflight: checking NVreg_GrdmaPciTopoCheckOverride on GPU nodes",
		"nodes", len(nodes))

	var (
		mu      sync.Mutex
		missing []string
	)
	g, gctx := errgroup.WithContext(ctx.Ctx)
	g.SetLimit(preflightNodeConcurrency)
	for _, n := range nodes {
		nodeName := n.Name
		g.Go(func() error {
			ok, err := checkNVregOnNode(gctx, ctx.Clientset, ctx.Namespace, nodeName)
			if err != nil {
				return aicrErrors.WrapWithContext(aicrErrors.ErrCodeInternal,
					"NVreg preflight probe failed", err,
					map[string]interface{}{"node": nodeName})
			}
			if !ok {
				mu.Lock()
				missing = append(missing, nodeName)
				mu.Unlock()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	if len(missing) > 0 {
		slices.Sort(missing)
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("NVreg_GrdmaPciTopoCheckOverride=1 missing on GPU nodes: %s. %s",
				strings.Join(missing, ", "), nvregDocsHint))
	}

	slog.Info("NET preflight passed: NVreg_GrdmaPciTopoCheckOverride=1 on all target nodes",
		"nodes", len(nodes))
	return nil
}

// checkNVregOnNode creates and waits on a short-lived probe pod that reads
// /proc/driver/nvidia/params on a specific node. Returns (true, nil) if the
// flag is set, (false, nil) if the flag is absent or zero, or (_, err) on
// any other failure (pod schedule, image pull, log read).
func checkNVregOnNode(ctx context.Context, clientset kubernetes.Interface, namespace, nodeName string) (bool, error) {
	podsClient := clientset.CoreV1().Pods(namespace)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: preflightPodNamePrefix,
			Namespace:    namespace,
			Labels: map[string]string{
				"app.kubernetes.io/component":  "nccl-nvreg-preflight",
				"app.kubernetes.io/managed-by": "aicr-validator",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			// Tolerate whatever taints the GPU nodes carry. The preflight
			// is cheap (busybox + grep) so we accept wherever scheduler
			// places us on the target node.
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   defaults.ProbeImage,
				Command: []string{"/bin/sh", "-c"},
				Args: []string{
					"grep '" + grdmaPciTopoCheckOverridePattern + "' /host-proc-nvidia/params",
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "proc-nvidia",
					MountPath: "/host-proc-nvidia",
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "proc-nvidia",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/proc/driver/nvidia",
					},
				},
			}},
		},
	}

	created, err := podsClient.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"failed to create NVreg preflight pod", err)
	}
	// Cleanup runs on an independent context so it fires even when the
	// probe context has been canceled (timeout, parallel-sibling error).
	defer func() { //nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.PreflightCleanupTimeout)
		defer cancel()
		if delErr := podsClient.Delete(cleanupCtx, created.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			slog.Warn("failed to delete NVreg preflight pod", "pod", created.Name, "err", delErr)
		}
	}()

	phase, err := waitForPreflightPodPhase(ctx, clientset, namespace, created.Name, defaults.DiagnosticTimeout)
	if err != nil {
		return false, err
	}

	if phase == corev1.PodSucceeded {
		return true, nil
	}

	// Failed: distinguish flag-absent (grep exit 1, empty stdout) from
	// harder errors (image pull, hostPath denied) by inspecting logs.
	logs, logErr := k8spod.GetPodLogs(ctx, clientset, namespace, created.Name, "probe")
	if logErr != nil {
		return false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"NVreg preflight pod Failed and logs were unreadable", logErr)
	}
	if strings.TrimSpace(logs) != "" {
		slog.Warn("NVreg preflight pod emitted unexpected output",
			"node", nodeName, "output", strings.TrimSpace(logs))
	}
	return false, nil
}

// waitForPreflightPodPhase watches a pod until it reaches a terminal phase
// (Succeeded or Failed). Uses the watch API per CLAUDE.md "Kubernetes
// Patterns" rather than polling. Returns the terminal phase on success.
func waitForPreflightPodPhase(ctx context.Context, clientset kubernetes.Interface, namespace, name string, timeout time.Duration) (corev1.PodPhase, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	podsClient := clientset.CoreV1().Pods(namespace)

	// Fast path: pod may already be terminal.
	if current, err := podsClient.Get(waitCtx, name, metav1.GetOptions{}); err == nil {
		if p := current.Status.Phase; p == corev1.PodSucceeded || p == corev1.PodFailed {
			return p, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get preflight pod", err)
	}

	watcher, err := podsClient.Watch(waitCtx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch preflight pod", err)
	}
	defer watcher.Stop()

	// Re-check after the watch is established: the pod may have reached a
	// terminal phase between the first Get and the Watch call, in which case
	// the watch will not replay the transition.
	if current, err := podsClient.Get(waitCtx, name, metav1.GetOptions{}); err == nil {
		if p := current.Status.Phase; p == corev1.PodSucceeded || p == corev1.PodFailed {
			return p, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get preflight pod", err)
	}

	for {
		select {
		case <-waitCtx.Done():
			return "", aicrErrors.WrapWithContext(aicrErrors.ErrCodeTimeout,
				"NVreg preflight pod did not terminate in time", waitCtx.Err(),
				map[string]interface{}{"pod": name})
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return "", aicrErrors.New(aicrErrors.ErrCodeInternal,
					"preflight pod watch channel closed unexpectedly")
			}
			if event.Type == watch.Deleted {
				return "", aicrErrors.New(aicrErrors.ErrCodeInternal,
					"preflight pod deleted before completion")
			}
			p, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				return p.Status.Phase, nil
			}
		}
	}
}

// gb200NetPreflightApplies reports whether the preflight check should run for
// the given (variant, accelerator, service) tuple. Keeps the call site at the
// top of validateNcclAllReduceBw uncluttered.
func gb200NetPreflightApplies(variant ncclVariant, accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType) bool {
	return variant == variantNET &&
		accelerator == recipe.CriteriaAcceleratorGB200 &&
		service == recipe.CriteriaServiceEKS
}
