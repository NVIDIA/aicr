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
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	hpaTestPrefix = "hpa-test-"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "pod-autoscaling",
		Description: "Verify custom and external metrics APIs expose GPU metrics for HPA",
		Phase:       phaseConformance,
		Func:        CheckPodAutoscaling,
		TestName:    "TestPodAutoscaling",
	})
}

// CheckPodAutoscaling validates CNCF requirement #8b: Pod Autoscaling.
// Verifies that the custom metrics API is available, GPU custom metrics have data
// (with retries to account for prometheus-adapter relist delay), and the external
// metrics API exposes GPU metrics.
func CheckPodAutoscaling(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. Custom metrics API available
	restClient := ctx.Clientset.Discovery().RESTClient()
	if restClient == nil {
		return errors.New(errors.ErrCodeInternal, "discovery REST client is not available")
	}
	rawURL := "/apis/custom.metrics.k8s.io/v1beta1"
	result := restClient.Get().AbsPath(rawURL).Do(ctx.Context)
	if err := result.Error(); err != nil {
		return errors.Wrap(errors.ErrCodeNotFound,
			"custom metrics API not available (prometheus-adapter not ready)", err)
	}

	// 2. GPU custom metrics have data (poll with retries — adapter relist is 30s)
	metrics := []string{"gpu_utilization", "gpu_memory_used", "gpu_power_usage"}
	namespaces := []string{"gpu-operator", "dynamo-system"}

	var found bool
	maxAttempts := 12 // 2 minutes with 10s intervals
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		for _, metric := range metrics {
			for _, ns := range namespaces {
				path := fmt.Sprintf(
					"/apis/custom.metrics.k8s.io/v1beta1/namespaces/%s/pods/*/%s",
					ns, metric)
				raw, err := restClient.Get().AbsPath(path).DoRaw(ctx.Context)
				if err != nil {
					continue
				}

				var metricsResp struct {
					Items []json.RawMessage `json:"items"`
				}
				if json.Unmarshal(raw, &metricsResp) == nil && len(metricsResp.Items) > 0 {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			break
		}

		// Wait before retry (respect context cancellation)
		select {
		case <-ctx.Context.Done():
			return errors.Wrap(errors.ErrCodeTimeout,
				"timed out waiting for GPU custom metrics", ctx.Context.Err())
		case <-time.After(10 * time.Second):
		}
	}

	if !found {
		return errors.New(errors.ErrCodeNotFound,
			"no GPU custom metrics available (DCGM → Prometheus → adapter pipeline broken)")
	}

	// 3. External metrics API has GPU metrics
	extPath := "/apis/external.metrics.k8s.io/v1beta1/namespaces/default/dcgm_gpu_power_usage"
	raw, err := restClient.Get().AbsPath(extPath).DoRaw(ctx.Context)
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound,
			"external metric dcgm_gpu_power_usage not available", err)
	}
	var extResp struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(raw, &extResp) == nil && len(extResp.Items) == 0 {
		return errors.New(errors.ErrCodeNotFound,
			"external metric dcgm_gpu_power_usage has no data")
	}

	// 4. HPA behavioral validation: prove HPA reads external metrics and computes scale-up.
	return validateHPABehavior(ctx.Context, ctx.Clientset)
}

// validateHPABehavior creates a Deployment + HPA targeting a low external metric threshold,
// then verifies the HPA computes desiredReplicas > currentReplicas and the Deployment
// actually scales. This proves the full metrics pipeline (DCGM → Prometheus → adapter → HPA)
// is functional end-to-end.
func validateHPABehavior(ctx context.Context, clientset kubernetes.Interface) error {
	// Generate unique test resource names and namespace (prevents cross-run interference).
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	suffix := hex.EncodeToString(b)
	nsName := hpaTestPrefix + suffix
	deployName := hpaTestPrefix + suffix
	hpaName := hpaTestPrefix + suffix

	// Create unique test namespace.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: nsName},
	}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); k8s.IgnoreAlreadyExists(err) != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create HPA test namespace", err)
	}

	// Cleanup: delete namespace (cascades all resources).
	// Use background context with bounded timeout so cleanup runs even if the parent
	// context is already canceled (timeout/failure path). Without this, unique namespaces
	// would accumulate as leftovers across repeated runs.
	defer func() { //nolint:contextcheck // intentional: use background context so cleanup runs even if parent is canceled
		slog.Debug("cleaning up HPA test namespace", "namespace", nsName)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cleanupCancel()
		_ = k8s.IgnoreNotFound(clientset.CoreV1().Namespaces().Delete(
			cleanupCtx, nsName, metav1.DeleteOptions{}))
	}()

	// Create test Deployment (simple sleep pod, 1 replica, no GPU).
	deploy := buildHPATestDeployment(deployName, nsName)
	if _, err := clientset.AppsV1().Deployments(nsName).Create(
		ctx, deploy, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create HPA test deployment", err)
	}

	// Create HPA targeting external metric dcgm_gpu_power_usage with very low threshold.
	hpa := buildHPATestHPA(hpaName, deployName, nsName)
	if _, err := clientset.AutoscalingV2().HorizontalPodAutoscalers(nsName).Create(
		ctx, hpa, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create HPA test resource", err)
	}

	// Wait for HPA to report scaling intent: desiredReplicas > currentReplicas.
	if err := waitForHPAScalingIntent(ctx, clientset, nsName, hpaName); err != nil {
		return err
	}

	// Wait for Deployment to actually scale (proves HPA → Deployment controller chain).
	return waitForDeploymentScale(ctx, clientset, nsName, deployName)
}

// buildHPATestDeployment creates a minimal Deployment for the HPA behavioral test.
// The pod does not need GPU resources — the HPA uses an external metric which is cluster-wide.
func buildHPATestDeployment(name, namespace string) *appsv1.Deployment {
	replicas := int32(1)
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
					Containers: []corev1.Container{
						{
							Name:    "sleep",
							Image:   "busybox:1.37",
							Command: []string{"sleep", "3600"},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("16Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
}

// buildHPATestHPA creates an HPA targeting external metric dcgm_gpu_power_usage.
// The target value is intentionally very low (10W) — an idle H100 draws ~46W,
// so the HPA always computes a scale-up. This works on any cluster with DCGM +
// prometheus-adapter, not just KWOK clusters.
func buildHPATestHPA(name, deployName, namespace string) *autoscalingv2.HorizontalPodAutoscaler {
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
			MaxReplicas: 3,
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

// resourceQuantityPtr returns a pointer to a parsed resource.Quantity.
func resourceQuantityPtr(val string) *resource.Quantity {
	q := resource.MustParse(val)
	return &q
}

// waitForHPAScalingIntent polls the HPA until desiredReplicas > currentReplicas.
// This is the strict criterion: it proves the HPA read metrics and computed a scale-up.
// We do NOT accept ScalingActive=True alone as that can be true even without scale intent.
func waitForHPAScalingIntent(ctx context.Context, clientset kubernetes.Interface, namespace, hpaName string) error {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.HPAScaleTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, defaults.HPAPollInterval, true,
		func(ctx context.Context) (bool, error) {
			hpa, getErr := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(
				ctx, hpaName, metav1.GetOptions{})
			if getErr != nil {
				slog.Debug("HPA not ready yet", "error", getErr)
				return false, nil // retry
			}

			desired := hpa.Status.DesiredReplicas
			current := hpa.Status.CurrentReplicas
			slog.Debug("HPA status", "desired", desired, "current", current)

			if desired > current {
				slog.Info("HPA scaling intent detected",
					"desiredReplicas", desired, "currentReplicas", current)
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return errors.Wrap(errors.ErrCodeTimeout,
				"HPA did not report scaling intent within timeout — metrics pipeline may be broken", err)
		}
		return errors.Wrap(errors.ErrCodeInternal, "HPA scaling intent polling failed", err)
	}

	return nil
}

// waitForDeploymentScale polls the Deployment until status.replicas > 1, proving
// that the Deployment controller acted on the HPA's scaling recommendation.
func waitForDeploymentScale(ctx context.Context, clientset kubernetes.Interface, namespace, deployName string) error {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.DeploymentScaleTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, defaults.HPAPollInterval, true,
		func(ctx context.Context) (bool, error) {
			deploy, getErr := clientset.AppsV1().Deployments(namespace).Get(
				ctx, deployName, metav1.GetOptions{})
			if getErr != nil {
				slog.Debug("failed to get deployment for scale check", "error", getErr)
				return false, nil
			}

			replicas := deploy.Status.Replicas
			slog.Debug("deployment replica status", "name", deployName, "replicas", replicas)

			if replicas > 1 {
				slog.Info("deployment scaled up", "name", deployName, "replicas", replicas)
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return errors.Wrap(errors.ErrCodeTimeout,
				"deployment did not scale up within timeout — HPA may not be effective", err)
		}
		return errors.Wrap(errors.ErrCodeInternal, "deployment scale verification failed", err)
	}

	return nil
}
