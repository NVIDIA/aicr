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
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
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

	return nil
}
