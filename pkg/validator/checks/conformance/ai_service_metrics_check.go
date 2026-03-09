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

package conformance

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

// requiredCustomMetrics maps CNCF-required GPU metric categories to their
// custom metrics API resource names (as exposed by prometheus-adapter).
// If these exist in the custom metrics API, the full pipeline is proven:
// DCGM exporter → Prometheus scraping → prometheus-adapter → custom metrics API.
var requiredCustomMetrics = []string{
	"gpu_utilization",
	"gpu_memory_used",
	"gpu_power_usage",
}

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:                  "ai-service-metrics",
		Description:           "Verify GPU metrics flow through Prometheus and custom metrics API is available",
		Phase:                 phaseConformance,
		Func:                  CheckAIServiceMetrics,
		TestName:              "TestAIServiceMetrics",
		RequirementID:         "accelerator_metrics",
		EvidenceTitle:         "Accelerator & AI Service Metrics",
		EvidenceDescription:   "Demonstrates that GPU metrics flow through Prometheus and are available via the Kubernetes custom metrics API for HPA scaling.",
		EvidenceFile:          "accelerator-metrics.md",
		SubmissionRequirement: true,
	})
}

// CheckAIServiceMetrics validates CNCF requirement #5: AI Service Metrics.
// Verifies that GPU metrics are available via the Kubernetes custom metrics API,
// proving the full observability pipeline: DCGM exporter → Prometheus → prometheus-adapter → custom metrics API.
// All queries route through the K8s API server, avoiding pod-to-pod network
// issues (security groups, network policies) that can block direct HTTP.
func CheckAIServiceMetrics(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	restClient := ctx.Clientset.Discovery().RESTClient()
	if restClient == nil {
		return errors.New(errors.ErrCodeInternal, "discovery REST client is not available")
	}

	// 1. Custom metrics API is available (proves prometheus-adapter is deployed).
	rawURL := "/apis/custom.metrics.k8s.io/v1beta1"
	result := restClient.Get().AbsPath(rawURL).Do(ctx.Context)
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

	// Build a set of available metric resource names for lookup.
	availableMetrics := make(map[string]bool, len(customMetricsResp.Resources))
	var resourceList strings.Builder
	for i, r := range customMetricsResp.Resources {
		availableMetrics[r.Name] = true
		if i < 20 {
			fmt.Fprintf(&resourceList, "- %s (namespaced=%t)\n", r.Name, r.Namespaced)
		}
	}
	recordRawTextArtifact(ctx, "Custom Metrics API",
		"kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1",
		fmt.Sprintf("HTTP Status:    %d\nGroupVersion:   %s\nResource count: %d\n\nResources:\n%s",
			statusCode, valueOrUnknown(customMetricsResp.GroupVersion),
			len(customMetricsResp.Resources), resourceList.String()))

	// 2. Verify required GPU metrics have actual data (not just registered).
	// Query each metric via the custom metrics API data path to prove samples
	// are flowing through the pipeline. Resource registration alone can be
	// present even when DCGM exporter is not scraping.
	var missingMetrics []string
	var emptyMetrics []string
	var metricStatus strings.Builder

	// Build candidate namespaces for metric queries. GPU metrics may exist in
	// different namespaces depending on cluster configuration (gpu-operator,
	// workload namespaces, etc.). Try recipe component namespaces first, then
	// well-known defaults.
	candidateNS := buildMetricNamespaces(ctx)

	for _, metric := range requiredCustomMetrics {
		podScoped := "pods/" + metric
		nsScoped := "namespaces/" + metric
		registered := availableMetrics[podScoped] || availableMetrics[nsScoped]
		if !registered {
			missingMetrics = append(missingMetrics, metric)
			fmt.Fprintf(&metricStatus, "  %-30s NOT REGISTERED\n", metric)
			continue
		}

		// Query actual metric values to verify data is flowing.
		// Try multiple namespaces and both pod-scoped and namespace-scoped paths.
		// Prometheus-adapter may expose metrics under different scopes depending
		// on configuration. Accept whichever returns data first.
		var itemCount int
		var queryBody string
		var queryPath string
		for _, ns := range candidateNS {
			paths := []string{
				fmt.Sprintf("/apis/custom.metrics.k8s.io/v1beta1/namespaces/%s/pods/*/%s", ns, metric),
				fmt.Sprintf("/apis/custom.metrics.k8s.io/v1beta1/namespaces/%s/metrics/%s", ns, metric),
			}
			for _, path := range paths {
				result := restClient.Get().AbsPath(path).Do(ctx.Context)
				if result.Error() != nil {
					continue
				}
				body, err := result.Raw()
				if err != nil {
					continue
				}
				var resp struct {
					Items []json.RawMessage `json:"items"`
				}
				if err := json.Unmarshal(body, &resp); err != nil {
					continue
				}
				if len(resp.Items) > 0 {
					itemCount = len(resp.Items)
					queryBody = string(body)
					queryPath = path
					break
				}
			}
			if itemCount > 0 {
				break
			}
		}

		if itemCount == 0 {
			emptyMetrics = append(emptyMetrics, metric)
			fmt.Fprintf(&metricStatus, "  %-30s REGISTERED (0 samples)\n", metric)
		} else {
			fmt.Fprintf(&metricStatus, "  %-30s OK (%d samples)\n", metric, itemCount)
			recordChunkedTextArtifact(ctx, fmt.Sprintf("Custom Metrics: %s", metric),
				fmt.Sprintf("kubectl get --raw '%s'", queryPath), queryBody)
		}
	}
	recordRawTextArtifact(ctx, "Required GPU Metrics (Custom Metrics API)",
		fmt.Sprintf("kubectl get --raw '/apis/custom.metrics.k8s.io/v1beta1/namespaces/{%s}/pods/*/<metric>'",
			strings.Join(candidateNS, ",")),
		metricStatus.String())

	if len(missingMetrics) > 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("GPU metrics not registered in custom metrics API: %s — verify prometheus-adapter rules are configured",
				strings.Join(missingMetrics, ", ")))
	}
	if len(emptyMetrics) > 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("GPU metrics registered but no samples flowing: %s — verify DCGM exporter is running and Prometheus is scraping",
				strings.Join(emptyMetrics, ", ")))
	}

	return nil
}

// buildMetricNamespaces returns candidate namespaces for GPU metric queries.
// Starts with recipe component namespaces (gpu-operator, dcgm-exporter), then
// falls back to well-known defaults. Deduplicates and preserves order.
func buildMetricNamespaces(ctx *checks.ValidationContext) []string {
	seen := make(map[string]bool)
	var namespaces []string

	add := func(ns string) {
		if ns != "" && !seen[ns] {
			seen[ns] = true
			namespaces = append(namespaces, ns)
		}
	}

	// Recipe component namespaces first (most specific).
	if ctx.Recipe != nil {
		for _, ref := range ctx.Recipe.ComponentRefs {
			switch ref.Name {
			case "gpu-operator", "dcgm-exporter", "nvidia-dcgm-exporter":
				add(ref.Namespace)
			}
		}
	}

	// Well-known defaults.
	add("gpu-operator")
	add("dynamo-system")

	return namespaces
}
