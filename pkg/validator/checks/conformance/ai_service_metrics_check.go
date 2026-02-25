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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

const prometheusBaseURL = "http://kube-prometheus-prometheus.monitoring.svc:9090"

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
// Verifies that GPU metric time series exist in Prometheus and that the
// custom metrics API is available.
func CheckAIServiceMetrics(ctx *checks.ValidationContext) error {
	return checkAIServiceMetricsWithURL(ctx, prometheusBaseURL)
}

// checkAIServiceMetricsWithURL is the testable implementation that accepts a configurable URL.
func checkAIServiceMetricsWithURL(ctx *checks.ValidationContext, promBaseURL string) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. Query Prometheus for GPU metric time series
	queryURL := fmt.Sprintf("%s/api/v1/query?query=DCGM_FI_DEV_GPU_UTIL", promBaseURL)
	body, err := httpGet(ctx.Context, queryURL)
	if err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable, "Prometheus unreachable", err)
	}

	var promResp struct {
		Status string `json:"status"`
		Data   struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &promResp); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to parse Prometheus response", err)
	}

	recordRawTextArtifact(ctx, "Prometheus Query: DCGM_FI_DEV_GPU_UTIL",
		fmt.Sprintf("curl -sf '%s'", queryURL),
		fmt.Sprintf("Status:            %s\nTime series count: %d", valueOrUnknown(promResp.Status), len(promResp.Data.Result)))
	recordChunkedTextArtifact(ctx, "Prometheus query response (GPU util)",
		fmt.Sprintf("curl -sf '%s'", queryURL), string(body))

	if len(promResp.Data.Result) == 0 {
		return errors.New(errors.ErrCodeNotFound,
			"no DCGM_FI_DEV_GPU_UTIL time series in Prometheus")
	}

	// 2. Custom metrics API available
	rawURL := "/apis/custom.metrics.k8s.io/v1beta1"
	restClient := ctx.Clientset.Discovery().RESTClient()
	if restClient == nil {
		return errors.New(errors.ErrCodeInternal, "discovery REST client is not available")
	}
	result := restClient.Get().AbsPath(rawURL).Do(ctx.Context)
	if cmErr := result.Error(); cmErr != nil {
		recordRawTextArtifact(ctx, "Custom Metrics API",
			"kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1",
			fmt.Sprintf("Status: unavailable\nError: %v", cmErr))
		return errors.Wrap(errors.ErrCodeNotFound,
			"custom metrics API not available", cmErr)
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
