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
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

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
	clusterAutoTestImage   = "docker.io/library/ubuntu:22.04"
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

// CheckClusterAutoscaling records the cluster's GPU autoscaling arrangement
// for CNCF requirement cluster_autoscaling. It probes, in order:
//  1. Karpenter — drives a scale-up (HPA + GPU NodePool) and records it; the
//     scale-down is not observed
//  2. EKS node group — records the managed node group, any in-cluster
//     Cluster Autoscaler, its status ConfigMap and recent autoscaler events
//  3. GKE cluster autoscaler — records the node pool, the
//     cluster-autoscaler-status ConfigMap and recent autoscaler events
//  4. AKS agent pool — records the VMSS-backed agent pool, the status
//     ConfigMap and recent autoscaler events
//
// Only the Karpenter path observes a scale event. The platform paths record
// observations — autoscaler presence, pool membership, status, events — and
// drive no scale test, so a passing result there is not behavioral proof:
// the cluster_autoscaling claim rests on provider documentation (manual
// attestation) or a configured not-applicable justification, never on these
// observations alone (#3428).
//
// A providerID no probe recognizes (bare metal, Kind, KWOK) is a missing
// probe, recorded as a skip: it says nothing about whether the platform
// autoscales, so product nonapplicability must come from configuration.
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
			if raw, hasGPU := limits[resourceNVIDIAGPU]; hasGPU {
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
		LabelSelector: labelNVIDIAGPUPresent,
	})
	if nodeErr != nil {
		recordRawTextArtifact(ctx, "GPU nodes",
			"kubectl get nodes -o custom-columns='NAME:.metadata.name,GPU:.status.capacity.nvidia.com/gpu'",
			fmt.Sprintf("failed to list GPU nodes: %v", nodeErr))
	} else {
		var nodeSummary strings.Builder
		for _, n := range gpuNodes.Items {
			gpuCap := n.Status.Capacity[resourceNVIDIAGPU]
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
		// Stop once the check's budget is spent: a further attempt could not
		// start its Create, yet each one would still pay the namespace cleanup
		// budget, pushing the check past its deadline until a generic
		// orchestrator timeout replaced its verdict.
		if ctxErr := ctx.Ctx.Err(); ctxErr != nil {
			msg := "check budget exhausted before every GPU NodePool was tried"
			if lastErr != nil {
				msg = fmt.Sprintf("%s (last NodePool error: %v)", msg, lastErr)
			}
			return errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout, msg)
		}
		slog.Info("attempting behavioral validation with NodePool", "nodePool", poolName)
		report, validateErr := validateClusterAutoscaling(ctx, poolName)
		lastErr = validateErr
		if lastErr == nil {
			recordRawTextArtifact(ctx, "Apply test manifest",
				"# cluster-autoscaling test resources (HPA + GPU deployment + Karpenter NodePool) constructed via the Kubernetes API; no static manifest",
				fmt.Sprintf("Created namespace=%s deployment=%s hpa=%s for nodePool=%s",
					report.Namespace, report.DeploymentName, report.HPAName, report.NodePoolName))
			recordRawTextArtifact(ctx, "Cluster Autoscaling Behavioral Test",
				fmt.Sprintf("kubectl get hpa -n %s && kubectl get nodes && kubectl get pods -n %s",
					report.Namespace, report.Namespace),
				fmt.Sprintf("NodePool:              %s\nNamespace:             %s\nHPA desired/current:   %d/%d\nKarpenter nodes:       baseline=%d observed=%d\nScheduled pods:        %d/%d\n"+
					"Observation:           scale-up observed (%d new Karpenter node(s) provisioned for the pending GPU pods); "+
					"scale-down is NOT observed by this check — the test namespace is deleted and Karpenter consolidation runs afterwards, unverified",
					report.NodePoolName, report.Namespace, report.HPADesiredReplicas,
					report.HPACurrentReplicas, report.BaselineNodeCount, report.ObservedNodeCount,
					report.ScheduledPodCount, report.ObservedPodCount,
					report.ObservedNodeCount-report.BaselineNodeCount))
			// The behavioral validation's deferred cleanup has already run by the
			// time this executes, but a returning Delete call only means the
			// request was accepted — the namespace terminates asynchronously.
			// The artifact therefore records that deletion was requested, never
			// that it completed; a rejected request is reported by the warning
			// cleanup logs.
			recordRawTextArtifact(ctx, "Delete test namespace",
				fmt.Sprintf("kubectl delete namespace %s --ignore-not-found", report.Namespace),
				fmt.Sprintf("Deletion of namespace %s was requested after the cluster autoscaling test; "+
					"the namespace terminates asynchronously and this artifact does not confirm it. "+
					"If the request failed, the check logs a warning naming the namespace. "+
					"Find leftovers from earlier runs with: "+
					"kubectl get namespaces -o name | grep %s",
					report.Namespace, clusterAutoTestPrefix))
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
//
// The return is NAMED so the deferred cleanup can tell a failed run apart
// from a successful one: the caller records the "Delete test namespace"
// artifact only after success, so the failed path records it here, and a
// terminally failed cleanup fails an otherwise-passing run (a primary error
// is never overwritten).
func validateClusterAutoscaling(vctx *validators.Context, nodePoolName string) (report *clusterAutoscalingReport, err error) {
	ctx, clientset := vctx.Ctx, vctx.Clientset

	// Generate unique test resource names and namespace (prevents cross-run interference).
	b := make([]byte, 4)
	if _, randErr := rand.Read(b); randErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", randErr)
	}
	suffix := hex.EncodeToString(b)
	nsName := clusterAutoTestPrefix + suffix
	deployName := clusterAutoTestPrefix + suffix
	hpaName := clusterAutoTestPrefix + suffix
	report = &clusterAutoscalingReport{
		NodePoolName:   nodePoolName,
		Namespace:      nsName,
		DeploymentName: deployName,
		HPAName:        hpaName,
	}

	// Cleanup: delete namespace (cascades all resources, triggers Karpenter
	// consolidation) through the shared cleanupGPUTestNamespace, deferred
	// BEFORE the create. A Create that returns an error may still have
	// persisted the namespace, or may still be applying it when a single
	// Delete would see NotFound, so cleanup settles to confirmed absence
	// within its budget instead of trusting one Delete. The ownership label
	// is what lets it adopt a namespace that surfaces late; the rest of the
	// gpuTestRun model (128-bit token) is not ported here.
	run := &gpuTestRun{token: suffix, namespace: nsName}
	defer func() { //nolint:contextcheck // cleanup runs on its own bounded context
		status, cleanupErr := cleanupGPUTestNamespace(clientset, run)
		if cleanupErr != nil {
			// Surface a failed delete: silently discarding it leaves the
			// namespace (and anything still running in it) behind with no
			// operator-visible signal.
			slog.Warn("failed to delete cluster autoscaling test namespace; delete it manually",
				"namespace", nsName, "status", status, "error", cleanupErr)
			if err == nil {
				// A PASS that leaks the namespace, the workload, and its GPU
				// request is not a pass; a primary error always wins.
				err = cleanupErr
			}
		}
		if err != nil {
			// A namespace that may still surface after an ambiguous create
			// must be as visible on the failed path as on the successful one.
			recordRawTextArtifact(vctx, "Delete test namespace", cleanupInspectEquivalent(nsName), status)
			return
		}
		slog.Debug("cleaned up cluster autoscaling test namespace", "namespace", nsName, "status", status)
	}()

	// Nothing has been sent yet, so a context that is already done is a
	// definitive "never created": skip the Create instead of letting its
	// context error read as an ambiguous outcome that cleanup would settle
	// for the whole budget.
	if ctxErr := ctx.Err(); ctxErr != nil {
		run.nsNeverCreated = true
		return nil, errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout,
			"check budget exhausted before the cluster autoscaling test namespace was created")
	}

	// Create unique test namespace.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nsName,
			Labels: map[string]string{gpuTestRunLabel: run.token},
		},
	}
	created, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if k8s.IgnoreAlreadyExists(err) != nil {
		// A definitive rejection cannot have persisted the namespace; anything
		// else stays ambiguous and cleanup settles it.
		run.nsNeverCreated = createDefinitelyRejected(err)
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create cluster autoscaling test namespace", err)
	}
	if err == nil {
		run.nsObserved = true
		run.nsUID = created.UID
	}

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
				MatchLabels: map[string]string{labelApp: name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{labelApp: name},
				},
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{
						{
							Key:      resourceNVIDIAGPU,
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
							Image:   clusterAutoTestImage,
							Command: []string{containerNameSleep, "120"},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									resourceNVIDIAGPU: resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									resourceNVIDIAGPU: resource.MustParse("1"),
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

// detectPlatform returns "eks", "gke", "aks", or "" based on the first node's
// providerID, together with that providerID so the caller can name the
// scheme no probe recognizes. A genuinely empty node list (Kind CI, KWOK) or
// an unrecognized providerID yields ("", pid, nil): the caller records that
// as a missing probe and skips.
//
// A Nodes().List ERROR (RBAC denial, apiserver timeout, transport failure) is
// NOT evidence that the platform is unrecognized (#2122). Flattening it to ""
// would reach checkPlatformAutoscaling's default branch and masquerade as an
// inapplicable Skip — the exact #2122 false-PASS. Instead it must fail closed
// with a classified pkg/errors code. Classification is delegated to the shared
// capability classifier via Capability.RequireList, which — unlike Require —
// never treats a List error as Skip-eligible: even the (rare) NotFound shape on
// a collection endpoint is an apiserver/aggregation anomaly, so every List error
// blocks (Forbidden→Unauthorized, deadline→Timeout, transport→Unavailable, else
// Internal) and never a Skip. Only a genuinely empty result (below) is
// inapplicable.
func detectPlatform(ctx *validators.Context) (platform, providerID string, err error) {
	nodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{
		Limit: 1,
	})
	if err != nil {
		return "", "", validators.Capability{Subject: "cluster nodes"}.RequireList(err)
	}
	if len(nodes.Items) == 0 {
		return "", "", nil
	}
	pid := nodes.Items[0].Spec.ProviderID
	if strings.HasPrefix(pid, "aws://") {
		return "eks", pid, nil
	}
	if strings.HasPrefix(pid, "gce://") {
		return "gke", pid, nil
	}
	if strings.HasPrefix(pid, "azure://") {
		return "aks", pid, nil
	}
	return "", pid, nil
}

// providerScheme returns the scheme of a providerID ("nico" for
// "nico://..."), or "none" when the node carries no providerID at all.
func providerScheme(providerID string) string {
	scheme, _, found := strings.Cut(providerID, "://")
	if !found || scheme == "" {
		return valueNone
	}
	return scheme
}

// checkPlatformAutoscaling records the platform's autoscaling arrangement
// when Karpenter is absent: the EKS node group, GKE cluster autoscaler, or
// AKS agent-pool observations. A providerID none of them recognizes is a
// missing probe, not product nonapplicability: the skip says so, and the
// not-applicable justification for a platform without workload-driven
// autoscaling comes from spec.validate.evidence.cncf.notApplicable.
func checkPlatformAutoscaling(ctx *validators.Context) error {
	platform, providerID, err := detectPlatform(ctx)
	if err != nil {
		// A node-list infrastructure error (RBAC/timeout/transport) must block
		// the gate rather than fall through to the missing-probe Skip.
		return err
	}
	slog.Info("Karpenter not found, falling back to platform autoscaling", "platform", platform)

	switch platform {
	case "eks":
		return checkEKSAutoscaling(ctx)
	case "gke":
		return checkGKEAutoscaling(ctx)
	case "aks":
		return checkAKSAutoscaling(ctx)
	default:
		scheme := providerScheme(providerID)
		reason := fmt.Sprintf(
			"no autoscaler probe for this provider: Karpenter is not installed and the node providerID scheme %q is not one the check has a probe for (aws, gce, azure). "+
				"This is a missing probe, not product nonapplicability: if the platform has no workload-driven cluster autoscaler, declare cluster_autoscaling not applicable with a justification in spec.validate.evidence.cncf.notApplicable; "+
				"otherwise the cluster_autoscaling claim rests on provider documentation", scheme)
		recordRawTextArtifact(ctx, "Cluster Autoscaling Observation",
			"kubectl get nodes -o jsonpath='{.items[0].spec.providerID}'",
			fmt.Sprintf("Provider scheme: %s\nProbe:           none\n%s", scheme, reason))
		return validators.SkipWithReason(validators.SkipReasonUnrecognizedPlatform, reason)
	}
}

// recordClusterAutoscalerDeployment records an in-cluster Cluster Autoscaler
// Deployment when an available one is found in the namespaces it is commonly
// installed in, and returns whether it was. Absence is an observation, not a
// failure: EKS may use managed node-group scaling and AKS runs its
// autoscaler in the managed control plane, neither visible in-cluster. Only
// a NotFound establishes absence; a Deployment that exists but is not
// available, or a read that failed (RBAC, timeout, transport), is recorded
// as such so the artifact never claims an absence that was not observed;
// unread carries those notes for the summary line.
func recordClusterAutoscalerDeployment(ctx *validators.Context) (found bool, unread string) {
	caNamespaces := []string{defaults.KubeSystemNamespace, deploymentClusterAutoscaler, "system"}
	caDeployNames := []string{deploymentClusterAutoscaler, "cluster-autoscaler-aws-cluster-autoscaler", "cluster-autoscaler-azure-cluster-autoscaler"}
	var notes []string
	for _, caNS := range caNamespaces {
		for _, caName := range caDeployNames {
			caDeploy, caErr := getDeploymentIfAvailable(ctx, caNS, caName)
			switch {
			case caErr == nil:
				recordRawTextArtifact(ctx, "Cluster Autoscaler",
					fmt.Sprintf("kubectl get deploy -n %s %s", caNS, caName),
					fmt.Sprintf("Name:      %s/%s\nReplicas:  %d/%d available\nImage:     %s",
						caDeploy.Namespace, caDeploy.Name,
						caDeploy.Status.AvailableReplicas,
						ptr.Deref(caDeploy.Spec.Replicas, 1),
						firstContainerImage(caDeploy.Spec.Template.Spec.Containers)))
				return true, ""
			case caDeploy != nil:
				notes = append(notes, fmt.Sprintf("%s/%s exists but is not available (%d available replicas)",
					caNS, caName, caDeploy.Status.AvailableReplicas))
			case !stderrors.Is(caErr, errors.New(errors.ErrCodeNotFound, "")):
				notes = append(notes, fmt.Sprintf("%s/%s could not be read: %v", caNS, caName, caErr))
			}
		}
	}
	text := "No in-cluster Cluster Autoscaler Deployment found (a managed autoscaler running in the provider control plane is not visible here)"
	if len(notes) > 0 {
		unread = strings.Join(notes, "; ")
		text = "No available in-cluster Cluster Autoscaler Deployment; absence is NOT established for every candidate: " + unread
	}
	recordRawTextArtifact(ctx, "Cluster Autoscaler", "kubectl get deploy -A | grep cluster-autoscaler", text)
	return false, unread
}

// recordClusterAutoscalerStatus records the cluster-autoscaler-status
// ConfigMap the Cluster Autoscaler (self-managed, GKE's and AKS's managed
// instances alike) writes, and returns whether it was found. A read error
// other than NotFound is recorded with the absence so it is not mistaken for
// a disabled autoscaler.
func recordClusterAutoscalerStatus(ctx *validators.Context) (found bool, unread string) {
	var readErrs []string
	for _, ns := range []string{defaults.KubeSystemNamespace, deploymentClusterAutoscaler, "system"} {
		caStatus, caErr := ctx.Clientset.CoreV1().ConfigMaps(ns).Get(
			ctx.Ctx, "cluster-autoscaler-status", metav1.GetOptions{})
		if caErr != nil {
			if k8s.IgnoreNotFound(caErr) != nil {
				readErrs = append(readErrs, fmt.Sprintf("%s: %v", ns, caErr))
			}
			continue
		}
		statusData := caStatus.Data["status"]
		if len(statusData) > defaults.ConfigMapStatusTruncateLen {
			statusData = statusData[:defaults.ConfigMapStatusTruncateLen] + "\n... [truncated]"
		}
		recordRawTextArtifact(ctx, "Cluster Autoscaler Status",
			fmt.Sprintf("kubectl get configmap cluster-autoscaler-status -n %s -o jsonpath='{.data.status}'", ns),
			statusData)
		return true, ""
	}
	text := "ConfigMap cluster-autoscaler-status not found (the autoscaler may not be enabled, or does not publish status in-cluster)"
	if len(readErrs) > 0 {
		unread = strings.Join(readErrs, "; ")
		text = "ConfigMap cluster-autoscaler-status not found where readable; absence is NOT established for every namespace, some reads failed: " + unread
	}
	recordRawTextArtifact(ctx, "Cluster Autoscaler Status",
		"kubectl get configmap cluster-autoscaler-status -n kube-system", text)
	return false, unread
}

// statusObservation words the cluster-autoscaler-status ConfigMap finding
// for a summary line: present, definitively absent (with the platform's
// reading of absence), or unread — never absence after a failed read.
func statusObservation(found bool, unread, present, absent string) string {
	switch {
	case found:
		return present
	case unread != "":
		return "cluster-autoscaler-status ConfigMap could not be read (" + unread + ")"
	default:
		return absent
	}
}

// deploymentObservation words the in-cluster Cluster Autoscaler Deployment
// finding for a summary line the same way.
func deploymentObservation(found bool, unread, absent string) string {
	switch {
	case found:
		return "an in-cluster Cluster Autoscaler Deployment is available"
	case unread != "":
		return "an in-cluster Cluster Autoscaler Deployment could not be confirmed (" + unread + ")"
	default:
		return absent
	}
}

// recordAutoscalerEvents records the most recent cluster-autoscaler events
// (scale-up triggered or declined, scale-down) as an observation of past
// autoscaler activity; none found is recorded as such.
func recordAutoscalerEvents(ctx *validators.Context) {
	events, evErr := ctx.Clientset.CoreV1().Events("").List(ctx.Ctx, metav1.ListOptions{})
	if evErr != nil {
		recordRawTextArtifact(ctx, "Autoscaler Events", "kubectl get events -A",
			fmt.Sprintf("failed to list events: %v", evErr))
		return
	}
	// The list is not time-ordered (the apiserver keys it by namespace and
	// name), so select the newest matches by timestamp. Events recorded
	// through the events.k8s.io API carry eventTime (or a series) and leave
	// lastTimestamp zero.
	var matched []corev1.Event
	for i := range events.Items {
		switch events.Items[i].Reason {
		case "NotTriggerScaleUp", "ScaledUpGroup", "ScaleDown", "TriggeredScaleUp":
			matched = append(matched, events.Items[i])
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return autoscalerEventTime(&matched[i]).After(autoscalerEventTime(&matched[j]))
	})
	if len(matched) > defaults.AutoscalerMaxEvents {
		matched = matched[:defaults.AutoscalerMaxEvents]
	}
	var autoscalerEvents strings.Builder
	for i := range matched {
		ev := &matched[i]
		fmt.Fprintf(&autoscalerEvents, "%s  %s  %s  %s\n",
			autoscalerEventTime(ev).UTC().Format(time.RFC3339),
			ev.Reason, ev.InvolvedObject.Name, ev.Message)
	}
	if autoscalerEvents.Len() > 0 {
		recordRawTextArtifact(ctx, "Autoscaler Events",
			"kubectl get events -A | grep -E 'TriggeredScaleUp|ScaledUpGroup|NotTriggerScaleUp|ScaleDown'", autoscalerEvents.String())
		return
	}
	recordRawTextArtifact(ctx, "Autoscaler Events", "", "No recent autoscaler events found")
}

// autoscalerEventTime returns the latest occurrence an Event describes: the
// later of lastTimestamp (core API) and series.lastObservedTime (a repeated
// events.k8s.io event), else eventTime (its first observation), else the
// object's creation.
func autoscalerEventTime(ev *corev1.Event) time.Time {
	latest := ev.LastTimestamp.Time
	if ev.Series != nil && ev.Series.LastObservedTime.After(latest) {
		latest = ev.Series.LastObservedTime.Time
	}
	switch {
	case !latest.IsZero():
		return latest
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	default:
		return ev.CreationTimestamp.Time
	}
}

// platformObservationText renders the platform path's result artifact. It
// states what was observed and that no scale event was driven, so the
// evidence cannot be read as behavioral proof.
func platformObservationText(platform string, gpuNodes int, pool, autoscaler string) string {
	return fmt.Sprintf("OBSERVED — %s cluster with %d GPU node(s); %s; %s.\n"+
		"No scale event was driven or observed by this check. These observations do not by themselves demonstrate cluster_autoscaling: "+
		"the claim rests on provider documentation (manual attestation), or on a not-applicable justification configured in spec.validate.evidence.cncf.notApplicable.",
		platform, gpuNodes, pool, autoscaler)
}

// checkEKSAutoscaling validates EKS node group–based GPU autoscaling.
// Verifies GPU nodes exist and belong to a managed node group (ASG-backed).
func checkEKSAutoscaling(ctx *validators.Context) error {
	// List GPU nodes.
	gpuNodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: labelNVIDIAGPUPresent,
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
		gpuCount := n.Status.Capacity[resourceNVIDIAGPU]
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

	// Observations: an in-cluster Cluster Autoscaler is optional on EKS
	// (Karpenter or managed node-group scaling may be used instead), so its
	// absence is recorded, not failed.
	caFound, caUnread := recordClusterAutoscalerDeployment(ctx)
	caStatusFound, statusUnread := recordClusterAutoscalerStatus(ctx)
	recordAutoscalerEvents(ctx)

	pool := fmt.Sprintf("GPU nodes belong to managed node group %q (ASG-backed)", nodeGroupName)
	if nodeGroupName == "" {
		// Self-managed node groups carry no eks.amazonaws.com/nodegroup label.
		// With no in-cluster autoscaler either, this path has observed no
		// autoscaler at all; that is an observation like the other platforms'
		// absences, not a failure — the platform paths decide nothing (the
		// requirement-level reading is the aggregation's, #3612).
		pool = "GPU nodes carry no managed node group label (eks.amazonaws.com/nodegroup)"
		// "No autoscaler observed" only when every observation came back as a
		// definitive absence: a present status ConfigMap is an autoscaler
		// observation, and a failed read of either is unread, not absent.
		if !caFound && caUnread == "" && !caStatusFound && statusUnread == "" {
			pool = "no autoscaler observed: no managed node group label, no in-cluster Cluster Autoscaler, no Karpenter"
		}
	}
	autoscaler := deploymentObservation(caFound, caUnread, "no in-cluster Cluster Autoscaler Deployment")
	autoscaler += "; " + statusObservation(caStatusFound, statusUnread,
		"cluster-autoscaler-status ConfigMap present", "no cluster-autoscaler-status ConfigMap")
	recordRawTextArtifact(ctx, "EKS Cluster Autoscaling Observation", "",
		platformObservationText("EKS", len(gpuNodes.Items), pool, autoscaler))
	return nil
}

// checkAKSAutoscaling validates AKS VMSS-backed GPU agent-pool autoscaling capability.
// AKS labels every node with kubernetes.azure.com/agentpool; resolving that
// label on a GPU node proves the node belongs to an AKS agent pool that can be
// scaled by the managed cluster autoscaler when enabled.
func checkAKSAutoscaling(ctx *validators.Context) error {
	gpuNodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: labelNVIDIAGPUPresent,
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list GPU nodes", err)
	}
	if len(gpuNodes.Items) == 0 {
		return errors.New(errors.ErrCodeNotFound, "no GPU nodes found in AKS cluster")
	}

	var nodeSummary strings.Builder
	agentPools := make(map[string]struct{})
	var missingAgentPoolNodes []string
	for _, n := range gpuNodes.Items {
		gpuCount := n.Status.Capacity[resourceNVIDIAGPU]
		instanceType := n.Labels["node.kubernetes.io/instance-type"]
		agentPool := n.Labels["kubernetes.azure.com/agentpool"]
		zone := n.Labels["topology.kubernetes.io/zone"]
		fmt.Fprintf(&nodeSummary, "%-44s gpu=%s instance=%s agentPool=%s zone=%s\n",
			n.Name, gpuCount.String(), valueOrUnknown(instanceType),
			valueOrUnknown(agentPool), valueOrUnknown(zone))
		if agentPool == "" {
			missingAgentPoolNodes = append(missingAgentPoolNodes, n.Name)
		} else {
			agentPools[agentPool] = struct{}{}
		}
	}
	recordRawTextArtifact(ctx, "GPU Nodes",
		"kubectl get nodes -l nvidia.com/gpu.present=true -o wide", nodeSummary.String())

	if len(missingAgentPoolNodes) > 0 {
		sort.Strings(missingAgentPoolNodes)
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("AKS GPU nodes missing kubernetes.azure.com/agentpool label: %s — cannot verify autoscaling capability",
				strings.Join(missingAgentPoolNodes, ", ")))
	}

	agentPoolNames := make([]string, 0, len(agentPools))
	for name := range agentPools {
		agentPoolNames = append(agentPoolNames, name)
	}
	sort.Strings(agentPoolNames)
	recordRawTextArtifact(ctx, "AKS Cluster Details", "",
		fmt.Sprintf("GPU Agent Pools: %s\nGPU Node Count: %d",
			strings.Join(agentPoolNames, ", "), len(gpuNodes.Items)))

	// Observations: the AKS managed autoscaler runs in the control plane and
	// is not visible in-cluster; its status ConfigMap and events are, when
	// it is enabled.
	caFound, caUnread := recordClusterAutoscalerDeployment(ctx)
	caStatusFound, statusUnread := recordClusterAutoscalerStatus(ctx)
	recordAutoscalerEvents(ctx)

	autoscaler := deploymentObservation(caFound, caUnread,
		"the managed cluster autoscaler runs in the AKS control plane and is not visible in-cluster")
	autoscaler += "; " + statusObservation(caStatusFound, statusUnread,
		"cluster-autoscaler-status ConfigMap present (autoscaler enabled)",
		"no cluster-autoscaler-status ConfigMap (autoscaler enablement not observable)")
	recordRawTextArtifact(ctx, "AKS Cluster Autoscaling Observation", "",
		platformObservationText("AKS", len(gpuNodes.Items),
			fmt.Sprintf("GPU nodes belong to VMSS-backed agent pool(s) %q", strings.Join(agentPoolNames, ", ")),
			autoscaler))
	return nil
}

// checkGKEAutoscaling validates GKE built-in cluster autoscaler for GPU node pools.
func checkGKEAutoscaling(ctx *validators.Context) error {
	// List GPU nodes.
	gpuNodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: labelNVIDIAGPUPresent,
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
		gpuCap := n.Status.Capacity[resourceNVIDIAGPU]
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

	// Observation: GKE's cluster autoscaler writes its status ConfigMap to
	// kube-system when enabled for a node pool.
	caStatusFound, statusUnread := recordClusterAutoscalerStatus(ctx)

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

	recordAutoscalerEvents(ctx)

	autoscaler := statusObservation(caStatusFound, statusUnread,
		"the built-in cluster autoscaler publishes cluster-autoscaler-status (enabled)",
		"no cluster-autoscaler-status ConfigMap — the built-in cluster autoscaler may not be enabled for this node pool")
	recordRawTextArtifact(ctx, "GKE Cluster Autoscaling Observation", "",
		platformObservationText("GKE", len(gpuNodes.Items),
			fmt.Sprintf("GPU nodes belong to node pool(s) %s", strings.Join(gkeNodePools(gpuNodes.Items), ", ")),
			autoscaler))
	return nil
}

// gkeNodePools returns the sorted distinct cloud.google.com/gke-nodepool
// labels of the nodes, "unknown" standing in for an unlabeled node.
func gkeNodePools(nodes []corev1.Node) []string {
	set := make(map[string]struct{})
	for i := range nodes {
		set[valueOrUnknown(nodes[i].Labels["cloud.google.com/gke-nodepool"])] = struct{}{}
	}
	pools := make([]string, 0, len(set))
	for pool := range set {
		pools = append(pools, pool)
	}
	sort.Strings(pools)
	return pools
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
