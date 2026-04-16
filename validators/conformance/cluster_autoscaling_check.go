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

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const (
	clusterAutoTestPrefix  = "cluster-auto-test-"
	karpenterNodePoolLabel = "karpenter.sh/nodepool"
)

type clusterAutoscalingReport struct {
	NodePoolName       string
	Namespace          string
	DeploymentName     string
	HPAName            string
	HPADesiredReplicas int32
	HPACurrentReplicas int32
	BaselineNodeCount  int
	ObservedNodeCount  int
	ScheduledPodCount  int
	ObservedPodCount   int
}

// CheckClusterAutoscaling validates CNCF requirement #8a: Cluster Autoscaling.
// Checks three autoscaling mechanisms in order:
//  1. Karpenter — behavioral test with HPA + GPU NodePool
//  2. EKS node group — validates ASG-backed GPU node group via node labels
//  3. GKE cluster autoscaler — validates via cluster-autoscaler-status ConfigMap
//
// Skips gracefully when no autoscaling mechanism can be detected (e.g., Kind CI).
func CheckClusterAutoscaling(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. Karpenter controller deployment running.
	// Only fall back to platform autoscaling when Karpenter is truly absent.
	// If Karpenter exists but is unhealthy, report the failure rather than masking it.
	// Search across all namespaces by label since Karpenter can be deployed in any namespace.
	deploy, karpenterNS, deployErr := findKarpenterDeployment(ctx)
	if deployErr != nil {
		// Only fall back when Karpenter is genuinely absent (NotFound).
		// For API errors (RBAC, transient), report the failure rather than masking it.
		var structErr *errors.StructuredError
		if stderrors.As(deployErr, &structErr) && structErr.Code == errors.ErrCodeNotFound {
			return checkPlatformAutoscaling(ctx)
		}
		return deployErr
	}
	expected := ptr.Deref(deploy.Spec.Replicas, 1)
	if deploy.Status.AvailableReplicas < expected || expected == 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("Karpenter deployment exists but is unhealthy: %d/%d available",
				deploy.Status.AvailableReplicas, expected))
	}
	recordRawTextArtifact(ctx, "Karpenter Controller",
		fmt.Sprintf("kubectl get deploy -n %s", karpenterNS),
		fmt.Sprintf("Name:      %s/%s\nReplicas:  %d/%d available\nImage:     %s",
			deploy.Namespace, deploy.Name,
			deploy.Status.AvailableReplicas, expected,
			firstContainerImage(deploy.Spec.Template.Spec.Containers)))
	karpenterPods, podErr := ctx.Clientset.CoreV1().Pods(karpenterNS).List(ctx.Ctx, metav1.ListOptions{})
	if podErr != nil {
		recordRawTextArtifact(ctx, "Karpenter pods", fmt.Sprintf("kubectl get pods -n %s -o wide", karpenterNS),
			fmt.Sprintf("failed to list karpenter pods: %v", podErr))
	} else {
		var podSummary strings.Builder
		for _, pod := range karpenterPods.Items {
			fmt.Fprintf(&podSummary, "%-44s ready=%s phase=%s node=%s\n",
				pod.Name, podReadyCount(pod), pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName))
		}
		recordRawTextArtifact(ctx, "Karpenter pods", fmt.Sprintf("kubectl get pods -n %s -o wide", karpenterNS), podSummary.String())
	}

	// 2. GPU NodePool exists with nvidia.com/gpu limits
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	npGVR := schema.GroupVersionResource{
		Group: "karpenter.sh", Version: "v1", Resource: "nodepools",
	}
	nps, err := dynClient.Resource(npGVR).List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "failed to list NodePools", err)
	}

	var gpuNodePoolNames []string
	var poolSummary strings.Builder
	for _, np := range nps.Items {
		limits, found, _ := unstructured.NestedMap(np.Object, "spec", "limits")
		limitGPU := "none"
		if found {
			if raw, hasGPU := limits["nvidia.com/gpu"]; hasGPU {
				gpuNodePoolNames = append(gpuNodePoolNames, np.GetName())
				limitGPU = fmt.Sprintf("%v", raw)
			}
		}
		fmt.Fprintf(&poolSummary, "%-32s gpuLimit=%s\n", np.GetName(), limitGPU)
	}
	recordRawTextArtifact(ctx, "GPU NodePools",
		"kubectl get nodepools.karpenter.sh -o yaml", poolSummary.String())
	if len(gpuNodePoolNames) == 0 {
		return errors.New(errors.ErrCodeNotFound,
			"no NodePool with nvidia.com/gpu limits found")
	}

	recordRawTextArtifact(ctx, "GPU NodePools (filtered)",
		"kubectl get nodepools.karpenter.sh",
		fmt.Sprintf("Count: %d\nNames: %s", len(gpuNodePoolNames),
			strings.Join(gpuNodePoolNames, ", ")))
	slog.Info("discovered GPU NodePools", "pools", gpuNodePoolNames)

	gpuNodes, nodeErr := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: "nvidia.com/gpu.present=true",
	})
	if nodeErr != nil {
		recordRawTextArtifact(ctx, "GPU nodes",
			"kubectl get nodes -o custom-columns='NAME:.metadata.name,GPU:.status.capacity.nvidia.com/gpu'",
			fmt.Sprintf("failed to list GPU nodes: %v", nodeErr))
	} else {
		var nodeSummary strings.Builder
		for _, n := range gpuNodes.Items {
			gpuCap := n.Status.Capacity["nvidia.com/gpu"]
			instanceType := n.Labels["node.kubernetes.io/instance-type"]
			fmt.Fprintf(&nodeSummary, "%-44s gpu=%s instance=%s\n",
				n.Name, gpuCap.String(), valueOrUnknown(instanceType))
		}
		recordRawTextArtifact(ctx, "GPU nodes",
			"kubectl get nodes -o custom-columns='NAME:.metadata.name,GPU:.status.capacity.nvidia.com/gpu,INSTANCE-TYPE:.metadata.labels.node.kubernetes.io/instance-type'",
			nodeSummary.String())
	}

	// 3. Behavioral validation: try each discovered GPU NodePool until one succeeds.
	// Multiple pools may exist (e.g. different GPU types) and not all may be viable
	// for this test workload.
	var lastErr error
	for _, poolName := range gpuNodePoolNames {
		slog.Info("attempting behavioral validation with NodePool", "nodePool", poolName)
		report, validateErr := validateClusterAutoscaling(ctx.Ctx, ctx.Clientset, poolName)
		lastErr = validateErr
		if lastErr == nil {
			recordRawTextArtifact(ctx, "Apply test manifest",
				"kubectl apply -f docs/conformance/cncf/manifests/hpa-gpu-scale-test.yaml",
				fmt.Sprintf("Created namespace=%s deployment=%s hpa=%s for nodePool=%s",
					report.Namespace, report.DeploymentName, report.HPAName, report.NodePoolName))
			recordRawTextArtifact(ctx, "Cluster Autoscaling Behavioral Test",
				"kubectl get hpa && kubectl get nodes && kubectl get pods",
				fmt.Sprintf("NodePool:              %s\nNamespace:             %s\nHPA desired/current:   %d/%d\nKarpenter nodes:       baseline=%d observed=%d\nScheduled pods:        %d/%d",
					report.NodePoolName, report.Namespace, report.HPADesiredReplicas,
					report.HPACurrentReplicas, report.BaselineNodeCount, report.ObservedNodeCount,
					report.ScheduledPodCount, report.ObservedPodCount))
			recordRawTextArtifact(ctx, "Delete test namespace",
				"kubectl delete namespace cluster-auto-test-<id> --ignore-not-found",
				fmt.Sprintf("Deleted namespace %s after cluster autoscaling test.", report.Namespace))
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
func validateClusterAutoscaling(ctx context.Context, clientset kubernetes.Interface, nodePoolName string) (*clusterAutoscalingReport, error) {
	// Generate unique test resource names and namespace (prevents cross-run interference).
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	suffix := hex.EncodeToString(b)
	nsName := clusterAutoTestPrefix + suffix
	deployName := clusterAutoTestPrefix + suffix
	hpaName := clusterAutoTestPrefix + suffix
	report := &clusterAutoscalingReport{
		NodePoolName:   nodePoolName,
		Namespace:      nsName,
		DeploymentName: deployName,
		HPAName:        hpaName,
	}

	// Create unique test namespace.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: nsName},
	}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); k8s.IgnoreAlreadyExists(err) != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create cluster autoscaling test namespace", err)
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

	// Baseline: count existing Karpenter nodes for this pool before creating test resources.
	// This ensures we detect a NEW scale-up, not pre-existing nodes from prior runs.
	baselineNodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", karpenterNodePoolLabel, nodePoolName),
	})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to count baseline Karpenter nodes", err)
	}
	baselineNodeCount := len(baselineNodes.Items)
	report.BaselineNodeCount = baselineNodeCount
	slog.Info("baseline Karpenter node count", "pool", nodePoolName, "count", baselineNodeCount)

	// Create Deployment: GPU-requesting pods with Karpenter nodeSelector.
	deploy := buildClusterAutoTestDeployment(deployName, nsName, nodePoolName)
	if _, createErr := clientset.AppsV1().Deployments(nsName).Create(
		ctx, deploy, metav1.CreateOptions{}); createErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create cluster autoscaling test deployment", createErr)
	}

	// Create HPA targeting external metric dcgm_gpu_power_usage.
	hpa := buildClusterAutoTestHPA(hpaName, deployName, nsName)
	if _, hpaErr := clientset.AutoscalingV2().HorizontalPodAutoscalers(nsName).Create(
		ctx, hpa, metav1.CreateOptions{}); hpaErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create cluster autoscaling test HPA", hpaErr)
	}

	// Wait for HPA to report scaling intent.
	desired, current, err := waitForHPAScalingIntent(ctx, clientset, nsName, hpaName)
	if err != nil {
		return nil, err
	}
	report.HPADesiredReplicas = desired
	report.HPACurrentReplicas = current

	// Wait for Karpenter to provision KWOK nodes (above baseline count).
	observedNodes, err := waitForKarpenterNodes(ctx, clientset, nodePoolName, baselineNodeCount)
	if err != nil {
		return nil, err
	}
	report.ObservedNodeCount = observedNodes

	// Verify pods are scheduled (not Pending) with poll loop.
	scheduled, total, err := verifyPodsScheduled(ctx, clientset, nsName)
	if err != nil {
		return nil, err
	}
	report.ScheduledPodCount = scheduled
	report.ObservedPodCount = total
	return report, nil
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
								AllowPrivilegeEscalation: helper.BoolPtr(false),
								ReadOnlyRootFilesystem:   helper.BoolPtr(true),
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

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
			MinReplicas: helper.Int32Ptr(1),
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

// waitForKarpenterNodes polls until nodes with the discovered NodePool label exceed the
// baseline count. This proves Karpenter provisioned NEW nodes, not just pre-existing ones.
func waitForKarpenterNodes(ctx context.Context, clientset kubernetes.Interface, nodePoolName string, baselineNodeCount int) (int, error) {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.KarpenterNodeTimeout)
	defer cancel()
	var observedNodeCount int

	err := wait.PollUntilContextCancel(waitCtx, defaults.KarpenterPollInterval, true,
		func(ctx context.Context) (bool, error) {
			nodes, listErr := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s", karpenterNodePoolLabel, nodePoolName),
			})
			if listErr != nil {
				slog.Debug("failed to list Karpenter nodes", "error", listErr)
				return false, nil
			}

			if len(nodes.Items) > baselineNodeCount {
				slog.Info("Karpenter provisioned new KWOK GPU node(s)",
					"total", len(nodes.Items), "baseline", baselineNodeCount,
					"new", len(nodes.Items)-baselineNodeCount)
				observedNodeCount = len(nodes.Items)
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return 0, errors.Wrap(errors.ErrCodeTimeout,
				"Karpenter did not provision GPU nodes within timeout", err)
		}
		return 0, errors.Wrap(errors.ErrCodeInternal, "Karpenter node polling failed", err)
	}
	return observedNodeCount, nil
}

// findKarpenterDeployment searches for a Karpenter deployment across all namespaces
// using the app.kubernetes.io/name=karpenter label (with app=karpenter fallback).
// Returns the deployment and its namespace.
// Karpenter can be deployed in any namespace (karpenter, system, kube-system, etc.).
func findKarpenterDeployment(ctx *validators.Context) (*appsv1.Deployment, string, error) {
	// Search all namespaces for deployments with the app=karpenter label.
	deploys, err := ctx.Clientset.AppsV1().Deployments("").List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=karpenter",
	})
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInternal, "failed to search for Karpenter deployment", err)
	}
	if len(deploys.Items) == 0 {
		// Try legacy label as fallback.
		deploys, err = ctx.Clientset.AppsV1().Deployments("").List(ctx.Ctx, metav1.ListOptions{
			LabelSelector: "app=karpenter",
		})
		if err != nil {
			return nil, "", errors.Wrap(errors.ErrCodeInternal, "failed to search for Karpenter deployment", err)
		}
	}
	if len(deploys.Items) == 0 {
		return nil, "", errors.New(errors.ErrCodeNotFound, "Karpenter deployment not found in any namespace")
	}
	deploy := &deploys.Items[0]
	return deploy, deploy.Namespace, nil
}

// detectPlatform returns "eks", "gke", or "" based on the first node's providerID.
func detectPlatform(ctx *validators.Context) string {
	nodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{
		Limit: 1,
	})
	if err != nil || len(nodes.Items) == 0 {
		return ""
	}
	pid := nodes.Items[0].Spec.ProviderID
	if strings.HasPrefix(pid, "aws://") {
		return "eks"
	}
	if strings.HasPrefix(pid, "gce://") {
		return "gke"
	}
	return ""
}

// checkPlatformAutoscaling validates cluster autoscaling when Karpenter is absent.
// Falls back to EKS node group or GKE cluster autoscaler validation.
func checkPlatformAutoscaling(ctx *validators.Context) error {
	platform := detectPlatform(ctx)
	slog.Info("Karpenter not found, falling back to platform autoscaling", "platform", platform)

	switch platform {
	case "eks":
		return checkEKSAutoscaling(ctx)
	case "gke":
		return checkGKEAutoscaling(ctx)
	default:
		return validators.Skip("Karpenter not found and cluster platform not recognized (not EKS or GKE)")
	}
}

// checkEKSAutoscaling validates EKS node group–based GPU autoscaling.
// Verifies GPU nodes exist and belong to a managed node group (ASG-backed).
func checkEKSAutoscaling(ctx *validators.Context) error {
	// List GPU nodes.
	gpuNodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: "nvidia.com/gpu.present=true",
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list GPU nodes", err)
	}
	if len(gpuNodes.Items) == 0 {
		return errors.New(errors.ErrCodeNotFound, "no GPU nodes found in EKS cluster")
	}

	// Record GPU node summary.
	var nodeSummary strings.Builder
	for _, n := range gpuNodes.Items {
		gpuCount := n.Status.Capacity["nvidia.com/gpu"]
		instanceType := n.Labels["node.kubernetes.io/instance-type"]
		nodeGroup := n.Labels["eks.amazonaws.com/nodegroup"]
		if nodeGroup == "" {
			nodeGroup = n.Labels["nodeGroup"]
		}
		zone := n.Labels["topology.kubernetes.io/zone"]
		fmt.Fprintf(&nodeSummary, "%-44s gpu=%s instance=%s nodeGroup=%s zone=%s\n",
			n.Name, gpuCount.String(), valueOrUnknown(instanceType),
			valueOrUnknown(nodeGroup), valueOrUnknown(zone))
	}
	recordRawTextArtifact(ctx, "GPU Nodes",
		"kubectl get nodes -l nvidia.com/gpu.present=true -o wide", nodeSummary.String())

	// Extract node group name from any GPU node (not just the first — mixed clusters
	// may have some nodes without the label).
	var nodeGroupName, region string
	for _, n := range gpuNodes.Items {
		if ng := n.Labels["eks.amazonaws.com/nodegroup"]; ng != "" {
			nodeGroupName = ng
			break
		}
		if ng := n.Labels["nodeGroup"]; ng != "" {
			nodeGroupName = ng
			break
		}
	}

	// Extract region from topology label (any node).
	for _, n := range gpuNodes.Items {
		if r := n.Labels["topology.kubernetes.io/region"]; r != "" {
			region = r
			break
		}
	}

	recordRawTextArtifact(ctx, "EKS Cluster Details", "",
		fmt.Sprintf("GPU Node Group: %s\nRegion:         %s\nGPU Node Count: %d",
			valueOrUnknown(nodeGroupName), valueOrUnknown(region), len(gpuNodes.Items)))

	// Check for Cluster Autoscaler deployment (optional — EKS may use Karpenter or managed scaling).
	// Search common namespaces since Cluster Autoscaler can be deployed anywhere.
	caNamespaces := []string{defaults.KubeSystemNamespace, "cluster-autoscaler", "system"}
	caDeployNames := []string{"cluster-autoscaler", "cluster-autoscaler-aws-cluster-autoscaler"}
	var caFound bool
	for _, caNS := range caNamespaces {
		for _, caName := range caDeployNames {
			if caDeploy, caErr := getDeploymentIfAvailable(ctx, caNS, caName); caErr == nil {
				caFound = true
				recordRawTextArtifact(ctx, "Cluster Autoscaler",
					fmt.Sprintf("kubectl get deploy -n %s %s", caNS, caName),
					fmt.Sprintf("Name:      %s/%s\nReplicas:  %d/%d available\nImage:     %s",
						caDeploy.Namespace, caDeploy.Name,
						caDeploy.Status.AvailableReplicas,
						ptr.Deref(caDeploy.Spec.Replicas, 1),
						firstContainerImage(caDeploy.Spec.Template.Spec.Containers)))
				break
			}
		}
		if caFound {
			break
		}
	}

	if nodeGroupName == "" && !caFound {
		return errors.New(errors.ErrCodeNotFound,
			"EKS GPU nodes found but no node group label or Cluster Autoscaler detected — cannot verify autoscaling capability")
	}

	recordRawTextArtifact(ctx, "EKS Cluster Autoscaling Result", "",
		fmt.Sprintf("PASS — EKS cluster with %d GPU nodes in node group %q. "+
			"ASG-backed node group provides autoscaling capability.",
			len(gpuNodes.Items), valueOrUnknown(nodeGroupName)))
	return nil
}

// checkGKEAutoscaling validates GKE built-in cluster autoscaler for GPU node pools.
func checkGKEAutoscaling(ctx *validators.Context) error {
	// List GPU nodes.
	gpuNodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: "nvidia.com/gpu.present=true",
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list GPU nodes", err)
	}
	if len(gpuNodes.Items) == 0 {
		return errors.New(errors.ErrCodeNotFound, "no GPU nodes found in GKE cluster")
	}

	// Record GPU node summary.
	var nodeSummary strings.Builder
	for _, n := range gpuNodes.Items {
		gpuCap := n.Status.Capacity["nvidia.com/gpu"]
		instanceType := n.Labels["node.kubernetes.io/instance-type"]
		accelerator := n.Labels["cloud.google.com/gke-accelerator"]
		nodePool := n.Labels["cloud.google.com/gke-nodepool"]
		fmt.Fprintf(&nodeSummary, "%-44s gpu=%s instance=%s accelerator=%s nodePool=%s\n",
			n.Name, gpuCap.String(), valueOrUnknown(instanceType),
			valueOrUnknown(accelerator), valueOrUnknown(nodePool))
	}
	recordRawTextArtifact(ctx, "GPU Nodes",
		"kubectl get nodes -l nvidia.com/gpu.present=true -o wide", nodeSummary.String())

	// Extract GKE project and zone from providerID (gce://project/zone/instance).
	providerID := gpuNodes.Items[0].Spec.ProviderID
	parts := strings.Split(strings.TrimPrefix(providerID, "gce://"), "/")
	var project, zone string
	if len(parts) >= 2 {
		project = parts[0]
		zone = parts[1]
	}
	recordRawTextArtifact(ctx, "GKE Cluster Details", "",
		fmt.Sprintf("Project:        %s\nZone:           %s\nGPU Node Count: %d",
			valueOrUnknown(project), valueOrUnknown(zone), len(gpuNodes.Items)))

	// Check cluster-autoscaler-status ConfigMap (GKE writes autoscaler status here).
	// GKE always writes this to kube-system, but check common namespaces as a safeguard.
	var caStatus *corev1.ConfigMap
	var caErr error
	for _, ns := range []string{"kube-system", "cluster-autoscaler", "system"} {
		caStatus, caErr = ctx.Clientset.CoreV1().ConfigMaps(ns).Get(
			ctx.Ctx, "cluster-autoscaler-status", metav1.GetOptions{})
		if caErr == nil {
			break
		}
	}
	var caStatusFound bool
	if caErr == nil && caStatus != nil {
		caStatusFound = true
		statusData := caStatus.Data["status"]
		if len(statusData) > defaults.ConfigMapStatusTruncateLen {
			statusData = statusData[:defaults.ConfigMapStatusTruncateLen] + "\n... [truncated]"
		}
		recordRawTextArtifact(ctx, "Cluster Autoscaler Status",
			"kubectl get configmap cluster-autoscaler-status -n kube-system -o jsonpath='{.data.status}'",
			statusData)
	} else {
		recordRawTextArtifact(ctx, "Cluster Autoscaler Status",
			"kubectl get configmap cluster-autoscaler-status -n kube-system",
			"ConfigMap cluster-autoscaler-status not found")
	}

	// Node pool annotations for autoscaling config.
	var annotSummary strings.Builder
	for _, n := range gpuNodes.Items {
		scaleDown := n.Annotations["cluster-autoscaler.kubernetes.io/scale-down-disabled"]
		nodePool := n.Labels["cloud.google.com/gke-nodepool"]
		fmt.Fprintf(&annotSummary, "%-44s nodePool=%s scaleDownDisabled=%s\n",
			n.Name, valueOrUnknown(nodePool), valueOrUnknown(scaleDown))
	}
	recordRawTextArtifact(ctx, "GPU Node Pool Annotations",
		"kubectl get nodes -l nvidia.com/gpu.present=true annotations", annotSummary.String())

	// Check for recent autoscaler events.
	events, evErr := ctx.Clientset.CoreV1().Events("").List(ctx.Ctx, metav1.ListOptions{})
	if evErr == nil {
		var autoscalerEvents strings.Builder
		count := 0
		for i := len(events.Items) - 1; i >= 0 && count < defaults.AutoscalerMaxEvents; i-- {
			ev := events.Items[i]
			if ev.Reason == "NotTriggerScaleUp" || ev.Reason == "ScaledUpGroup" ||
				ev.Reason == "ScaleDown" || ev.Reason == "TriggeredScaleUp" {

				fmt.Fprintf(&autoscalerEvents, "%s  %s  %s  %s\n",
					ev.LastTimestamp.Format("2006-01-02T15:04:05Z"),
					ev.Reason, ev.InvolvedObject.Name, ev.Message)
				count++
			}
		}
		if autoscalerEvents.Len() > 0 {
			recordRawTextArtifact(ctx, "Autoscaler Events",
				"kubectl get events -A | grep autoscaler", autoscalerEvents.String())
		} else {
			recordRawTextArtifact(ctx, "Autoscaler Events", "", "No recent autoscaler events found")
		}
	}

	if caStatusFound {
		recordRawTextArtifact(ctx, "GKE Cluster Autoscaling Result", "",
			fmt.Sprintf("PASS — GKE cluster with %d GPU nodes and built-in cluster autoscaler active.",
				len(gpuNodes.Items)))
	} else {
		recordRawTextArtifact(ctx, "GKE Cluster Autoscaling Result", "",
			fmt.Sprintf("PASS (partial) — GKE cluster with %d GPU nodes. "+
				"Cluster autoscaler status ConfigMap not found — autoscaler may not be enabled for this node pool.",
				len(gpuNodes.Items)))
	}
	return nil
}

// verifyPodsScheduled polls until pods in the unique test namespace are scheduled (not Pending).
// This proves the full chain: HPA → scale → Karpenter → nodes → pods scheduled.
// The namespace is unique per run, so all pods belong to this test — no stale pod interference.
func verifyPodsScheduled(ctx context.Context, clientset kubernetes.Interface, namespace string) (int, int, error) {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.PodScheduleTimeout)
	defer cancel()
	var scheduledOut int
	var totalOut int

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
			totalOut = len(pods.Items)

			var scheduled int
			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodSucceeded {
					scheduled++
				}
			}
			scheduledOut = scheduled

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
			return 0, 0, errors.Wrap(errors.ErrCodeTimeout,
				"test pods not scheduled within timeout — Karpenter nodes may not be ready", err)
		}
		return 0, 0, errors.Wrap(errors.ErrCodeInternal, "pod scheduling verification failed", err)
	}
	return scheduledOut, totalOut, nil
}
