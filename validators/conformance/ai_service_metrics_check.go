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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	prometheusComponentName = "kube-prometheus-stack"
	prometheusDefaultPort   = 9090
)

// CheckAIServiceMetrics validates CNCF requirement #5: AI Service Metrics.
// Discovers the Prometheus service URL from the recipe's kube-prometheus-stack
// component, then verifies GPU metric time series exist and that the custom
// metrics API is available.
func CheckAIServiceMetrics(ctx *validators.Context) error {
	promURL, err := discoverPrometheusURL(ctx)
	if err != nil {
		return err
	}
	return checkAIServiceMetricsWithURL(ctx, promURL)
}

// discoverPrometheusURL finds the Prometheus service URL by looking up the
// kube-prometheus-stack component namespace in the recipe and discovering
// the Prometheus service via label selector. No hardcoded service names.
func discoverPrometheusURL(ctx *validators.Context) (string, error) {
	if ctx.Recipe == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, "recipe is not available")
	}

	var namespace string
	for _, ref := range ctx.Recipe.ComponentRefs {
		if ref.Name == prometheusComponentName {
			namespace = ref.Namespace
			break
		}
	}
	if namespace == "" {
		return "", errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("component %q not found in recipe or has no namespace", prometheusComponentName))
	}

	// Try multiple label selectors to handle different kube-prometheus-stack versions.
	// Older versions (<=81.x) use app.kubernetes.io/name=prometheus;
	// newer versions (>=82.x) dropped that label but retain self-monitor=true.
	selectors := []string{
		"app.kubernetes.io/name=prometheus",
		"self-monitor=true",
	}
	for _, selector := range selectors {
		services, err := ctx.Clientset.CoreV1().Services(namespace).List(ctx.Ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return "", errors.Wrap(errors.ErrCodeInternal, "failed to list Prometheus services", err)
		}
		for i := range services.Items {
			svc := &services.Items[i]
			for _, port := range svc.Spec.Ports {
				if port.Port == int32(prometheusDefaultPort) {
					return fmt.Sprintf("http://%s.%s.svc:%d", svc.Name, namespace, prometheusDefaultPort), nil
				}
			}
		}
	}

	return "", errors.New(errors.ErrCodeNotFound,
		fmt.Sprintf("no Prometheus service with port %d found in namespace %q", prometheusDefaultPort, namespace))
}

// checkAIServiceMetricsWithURL is the testable implementation that accepts a configurable URL.
func checkAIServiceMetricsWithURL(ctx *validators.Context, promBaseURL string) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. Query Prometheus for GPU metric time series, retrying until DCGM
	//    exporter metrics appear or the timeout expires.
	queryURL := fmt.Sprintf("%s/api/v1/query?query=DCGM_FI_DEV_GPU_UTIL", promBaseURL)
	deadline := time.Now().Add(defaults.AIServiceMetricsWaitTimeout)

	var promResp struct {
		Status string `json:"status"`
		Data   struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}

	for {
		body, err := httpGet(ctx.Ctx, queryURL)
		if err != nil {
			return errors.Wrap(errors.ErrCodeUnavailable,
				fmt.Sprintf("Prometheus unreachable at %s — verify network connectivity "+
					"(security groups, network policies) between validator pod and Prometheus service",
					promBaseURL), err)
		}

		if err := json.Unmarshal(body, &promResp); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to parse Prometheus response", err)
		}

		recordRawTextArtifact(ctx, "Prometheus Query: DCGM_FI_DEV_GPU_UTIL",
			fmt.Sprintf("curl -sf '%s'", queryURL),
			fmt.Sprintf("Status:            %s\nTime series count: %d", valueOrUnknown(promResp.Status), len(promResp.Data.Result)))
		recordRawTextArtifact(ctx, "Prometheus query response (GPU util)",
			fmt.Sprintf("curl -sf '%s'", queryURL), string(body))

		if len(promResp.Data.Result) > 0 {
			break
		}

		if time.Now().After(deadline) {
			return errors.New(errors.ErrCodeNotFound,
				"no DCGM_FI_DEV_GPU_UTIL time series in Prometheus — verify DCGM exporter is running and scraping GPU metrics")
		}

		slog.Info("DCGM_FI_DEV_GPU_UTIL not yet available, retrying",
			"poll_interval", defaults.AIServiceMetricsPollInterval,
			"deadline", deadline.Format(time.RFC3339))

		select {
		case <-ctx.Ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "context canceled while waiting for GPU metrics", ctx.Ctx.Err())
		case <-time.After(defaults.AIServiceMetricsPollInterval):
		}
	}

	// 2. Custom metrics API available
	rawURL := "/apis/custom.metrics.k8s.io/v1beta1"
	restClient := ctx.Clientset.Discovery().RESTClient()
	if restClient == nil {
		return errors.New(errors.ErrCodeInternal, "discovery REST client is not available")
	}
	result := restClient.Get().AbsPath(rawURL).Do(ctx.Ctx)
	if cmErr := result.Error(); cmErr != nil {
		recordRawTextArtifact(ctx, "Custom Metrics API",
			"kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1",
			fmt.Sprintf("Status: unavailable\nError: %v", cmErr))
		return errors.Wrap(errors.ErrCodeNotFound,
			"custom metrics API not available — verify prometheus-adapter is deployed and healthy", cmErr)
	}
	var statusCode int
	result.StatusCode(&statusCode)
	rawBody, rawErr := result.Raw()
	if rawErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to read custom metrics API response", rawErr)
	}
	var customMetricsResp struct {
		GroupVersion string `json:"groupVersion"`
		Resources    []struct {
			Name       string `json:"name"`
			Namespaced bool   `json:"namespaced"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(rawBody, &customMetricsResp); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to parse custom metrics API response", err)
	}
	var resources strings.Builder
	limit := len(customMetricsResp.Resources)
	if limit > 20 {
		limit = 20
	}
	for i := 0; i < limit; i++ {
		r := customMetricsResp.Resources[i]
		fmt.Fprintf(&resources, "- %s (namespaced=%t)\n", r.Name, r.Namespaced)
	}
	recordRawTextArtifact(ctx, "Custom Metrics API",
		"kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1",
		fmt.Sprintf("HTTP Status:    %d\nGroupVersion:   %s\nResource count: %d\n\nResources:\n%s",
			statusCode, valueOrUnknown(customMetricsResp.GroupVersion), len(customMetricsResp.Resources), resources.String()))

	return nil
}
