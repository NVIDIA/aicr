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

package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	clusterAutoTestPrefix  = "cluster-auto-test-"
	karpenterNodePoolLabel = "karpenter.sh/nodepool"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "cluster-autoscaling",
		Description: "Verify Karpenter controller is deployed and a GPU-aware NodePool exists",
		Phase:       phaseConformance,
		Func:        CheckClusterAutoscaling,
		TestName:    "TestClusterAutoscaling",
	})
}

// CheckClusterAutoscaling validates CNCF requirement #8a: Cluster Autoscaling.
// Verifies the Karpenter controller deployment is running and at least one
// NodePool has nvidia.com/gpu limits configured.
func CheckClusterAutoscaling(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. Karpenter controller deployment running
	if err := verifyDeploymentAvailable(ctx, "karpenter", "karpenter"); err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "Karpenter controller check failed", err)
	}

	// 2. GPU NodePool exists with nvidia.com/gpu limits
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	npGVR := schema.GroupVersionResource{
		Group: "karpenter.sh", Version: "v1", Resource: "nodepools",
	}
	nps, err := dynClient.Resource(npGVR).List(ctx.Context, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "failed to list NodePools", err)
	}

	var gpuNodePoolNames []string
	for _, np := range nps.Items {
		limits, found, _ := unstructured.NestedMap(np.Object, "spec", "limits")
		if found {
			if _, hasGPU := limits["nvidia.com/gpu"]; hasGPU {
				gpuNodePoolNames = append(gpuNodePoolNames, np.GetName())
			}
		}
	}
	if len(gpuNodePoolNames) == 0 {
		return errors.New(errors.ErrCodeNotFound,
			"no NodePool with nvidia.com/gpu limits found")
	}

	slog.Info("discovered GPU NodePools", "pools", gpuNodePoolNames)

	// 3. Behavioral validation: try each discovered GPU NodePool until one succeeds.
	// Multiple pools may exist (e.g. different GPU types) and not all may be viable
	// for this test workload.
	var lastErr error
	for _, poolName := range gpuNodePoolNames {
		slog.Info("attempting behavioral validation with NodePool", "nodePool", poolName)
		lastErr = validateClusterAutoscaling(ctx.Context, ctx.Clientset, poolName)
		if lastErr == nil {
			return nil
		}
		slog.Debug("behavioral validation failed for NodePool",
			"nodePool", poolName, "error", lastErr)
	}
	return lastErr
}

// validateClusterAutoscaling validates the full metrics-driven GPU autoscaling chain:
// Deployment + HPA (external metric) → HPA computes scale-up → Karpenter provisions
// KWOK nodes → pods are scheduled. This proves the chain works end-to-end.
// nodePoolName is the discovered GPU NodePool name from the precheck.
func validateClusterAutoscaling(ctx context.Context, clientset kubernetes.Interface, nodePoolName string) error {
	// Generate unique test resource names and namespace (prevents cross-run interference).
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	suffix := hex.EncodeToString(b)
	nsName := clusterAutoTestPrefix + suffix
	deployName := clusterAutoTestPrefix + suffix
	hpaName := clusterAutoTestPrefix + suffix

	// Create unique test namespace.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: nsName},
	}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); k8s.IgnoreAlreadyExists(err) != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create cluster autoscaling test namespace", err)
	}

	// Cleanup: delete namespace (cascades all resources, triggers Karpenter consolidation).
	// Use background context with bounded timeout so cleanup runs even if the parent
	// context is already canceled (timeout/failure path). Without this, unique namespaces
	// would accumulate as leftovers across repeated runs.
	defer func() { //nolint:contextcheck // intentional: use background context so cleanup runs even if parent is canceled
		slog.Debug("cleaning up cluster autoscaling test namespace", "namespace", nsName)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cleanupCancel()
		_ = k8s.IgnoreNotFound(clientset.CoreV1().Namespaces().Delete(
			cleanupCtx, nsName, metav1.DeleteOptions{}))
	}()

	// Create Deployment: GPU-requesting pods with Karpenter nodeSelector.
	deploy := buildClusterAutoTestDeployment(deployName, nsName, nodePoolName)
	if _, err := clientset.AppsV1().Deployments(nsName).Create(
		ctx, deploy, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create cluster autoscaling test deployment", err)
	}

	// Create HPA targeting external metric dcgm_gpu_power_usage.
	hpa := buildClusterAutoTestHPA(hpaName, deployName, nsName)
	if _, err := clientset.AutoscalingV2().HorizontalPodAutoscalers(nsName).Create(
		ctx, hpa, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create cluster autoscaling test HPA", err)
	}

	// Wait for HPA to report scaling intent.
	if err := waitForClusterAutoHPAScale(ctx, clientset, nsName, hpaName); err != nil {
		return err
	}

	// Wait for Karpenter to provision KWOK nodes.
	if err := waitForKarpenterNodes(ctx, clientset, nodePoolName); err != nil {
		return err
	}

	// Verify pods are scheduled (not Pending) with poll loop.
	return verifyPodsScheduled(ctx, clientset, nsName)
}

// buildClusterAutoTestDeployment creates a Deployment that requests GPU resources
// and targets the discovered Karpenter GPU NodePool. This matches the KWOK autoscaling
// test manifest (kwok/manifests/karpenter/hpa-gpu-scale-test.yaml).
func buildClusterAutoTestDeployment(name, namespace, nodePoolName string) *appsv1.Deployment {
	replicas := int32(1)
	runAsNonRoot := true
	runAsUser := int64(65534)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{
						{
							Key:      "nvidia.com/gpu",
							Operator: corev1.TolerationOpEqual,
							Value:    "present",
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "kwok.x-k8s.io/node",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					NodeSelector: map[string]string{
						karpenterNodePoolLabel: nodePoolName,
					},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "gpu-workload",
							Image:   "ubuntu:22.04",
							Command: []string{"sleep", "120"},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									"nvidia.com/gpu": resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									"nvidia.com/gpu": resource.MustParse("1"),
								},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: boolPtr(false),
								ReadOnlyRootFilesystem:   boolPtr(true),
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

// boolPtr returns a pointer to the given bool value.
func boolPtr(b bool) *bool { return &b }

// buildClusterAutoTestHPA creates an HPA targeting external metric dcgm_gpu_power_usage
// with a very low threshold (10W). An idle H100 draws ~46W, so this reliably triggers
// scale-up on any cluster with DCGM + prometheus-adapter.
func buildClusterAutoTestHPA(name, deployName, namespace string) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deployName,
			},
			MinReplicas: int32Ptr(1),
			MaxReplicas: 4,
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ExternalMetricSourceType,
					External: &autoscalingv2.ExternalMetricSource{
						Metric: autoscalingv2.MetricIdentifier{
							Name: "dcgm_gpu_power_usage",
						},
						Target: autoscalingv2.MetricTarget{
							Type:         autoscalingv2.AverageValueMetricType,
							AverageValue: resourceQuantityPtr("10"),
						},
					},
				},
			},
		},
	}
}

// waitForClusterAutoHPAScale polls the HPA until desiredReplicas > currentReplicas.
func waitForClusterAutoHPAScale(ctx context.Context, clientset kubernetes.Interface, namespace, hpaName string) error {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.HPAScaleTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, defaults.HPAPollInterval, true,
		func(ctx context.Context) (bool, error) {
			hpa, getErr := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(
				ctx, hpaName, metav1.GetOptions{})
			if getErr != nil {
				slog.Debug("HPA not ready yet", "error", getErr)
				return false, nil
			}

			desired := hpa.Status.DesiredReplicas
			current := hpa.Status.CurrentReplicas
			slog.Debug("cluster autoscaling HPA status", "desired", desired, "current", current)

			if desired > current {
				slog.Info("cluster autoscaling HPA scaling intent detected",
					"desiredReplicas", desired, "currentReplicas", current)
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return errors.Wrap(errors.ErrCodeTimeout,
				"HPA did not report scaling intent — external metrics pipeline may be broken", err)
		}
		return errors.Wrap(errors.ErrCodeInternal, "HPA scaling intent polling failed", err)
	}
	return nil
}

// waitForKarpenterNodes polls until at least one node with the discovered NodePool label exists.
func waitForKarpenterNodes(ctx context.Context, clientset kubernetes.Interface, nodePoolName string) error {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.KarpenterNodeTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, defaults.KarpenterPollInterval, true,
		func(ctx context.Context) (bool, error) {
			nodes, listErr := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s", karpenterNodePoolLabel, nodePoolName),
			})
			if listErr != nil {
				slog.Debug("failed to list Karpenter nodes", "error", listErr)
				return false, nil
			}

			if len(nodes.Items) > 0 {
				slog.Info("Karpenter provisioned KWOK GPU node(s)",
					"count", len(nodes.Items))
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return errors.Wrap(errors.ErrCodeTimeout,
				"Karpenter did not provision GPU nodes within timeout", err)
		}
		return errors.Wrap(errors.ErrCodeInternal, "Karpenter node polling failed", err)
	}
	return nil
}

// verifyPodsScheduled polls until pods in the unique test namespace are scheduled (not Pending).
// This proves the full chain: HPA → scale → Karpenter → nodes → pods scheduled.
// The namespace is unique per run, so all pods belong to this test — no stale pod interference.
func verifyPodsScheduled(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.PodScheduleTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, defaults.KarpenterPollInterval, true,
		func(ctx context.Context) (bool, error) {
			pods, listErr := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
			if listErr != nil {
				slog.Debug("failed to list test pods", "error", listErr)
				return false, nil
			}

			if len(pods.Items) < 2 {
				slog.Debug("waiting for HPA-scaled pods", "count", len(pods.Items))
				return false, nil
			}

			var scheduled int
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodPending {
					scheduled++
				}
			}

			slog.Debug("cluster autoscaling pod status",
				"total", len(pods.Items), "scheduled", scheduled)

			if scheduled >= 2 {
				slog.Info("cluster autoscaling pods verified",
					"total", len(pods.Items), "scheduled", scheduled)
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return errors.Wrap(errors.ErrCodeTimeout,
				"test pods not scheduled within timeout — Karpenter nodes may not be ready", err)
		}
		return errors.Wrap(errors.ErrCodeInternal, "pod scheduling verification failed", err)
	}
	return nil
}
