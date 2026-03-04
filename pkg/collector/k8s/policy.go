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

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// collectClusterPolicies retrieves ClusterPolicy custom resources from all API groups and namespaces.
// It dynamically discovers all ClusterPolicy CRDs regardless of their API group.
func (k *Collector) collectClusterPolicies(ctx context.Context) (map[string]measurement.Reading, error) {
	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(k.RestConfig)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create dynamic client", err)
	}

	// Discover all API resources
	discoveryClient := k.ClientSet.Discovery()
	apiResourceLists, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		// ServerPreferredResources can return a partial result with an error
		// We should continue if we got some resources
		slog.Debug("error discovering API resources (continuing with partial results)", slog.String("error", err.Error()))
		if len(apiResourceLists) == 0 {
			slog.Warn("no API resources discovered", slog.String("error", err.Error()))
			return make(map[string]measurement.Reading), nil
		}
	}

	policyData := make(map[string]measurement.Reading)

	// Find all ClusterPolicy resources across all API groups
	for _, apiResourceList := range apiResourceLists {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if apiResourceList == nil {
			continue
		}

		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			continue
		}

		for _, resource := range apiResourceList.APIResources {
			// Look for ClusterPolicy kind
			if resource.Kind != "ClusterPolicy" {
				continue
			}

			// Skip subresources (they contain a slash like "clusterpolicies/status")
			if len(resource.Name) == 0 || strings.Contains(resource.Name, "/") {
				continue
			}

			// Construct GVR for this ClusterPolicy resource
			gvr := schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: resource.Name,
			}

			slog.Debug("found clusterpolicy resource",
				slog.String("group", gv.Group),
				slog.String("version", gv.Version),
				slog.String("resource", resource.Name))

			// List all instances across all namespaces
			policies, err := dynamicClient.Resource(gvr).Namespace("").List(ctx, v1.ListOptions{})
			if err != nil {
				slog.Debug("failed to list clusterpolicy",
					slog.String("group", gv.Group),
					slog.String("error", err.Error()))
				continue
			}

			// Process each policy
			for _, policy := range policies.Items {
				// Check for context cancellation
				if err := ctx.Err(); err != nil {
					return nil, err
				}

				// Extract spec for detailed information
				spec, found, err := unstructured.NestedMap(policy.Object, "spec")
				if err != nil || !found {
					slog.Warn("failed to extract spec from clusterpolicy",
						slog.String("name", policy.GetName()),
						slog.String("error", fmt.Sprintf("%v", err)))
					continue
				}

				// Flatten the spec into individual key-value pairs
				flattenSpec(spec, "", policyData)
			}
		}
	}

	slog.Debug("collected cluster policies", slog.Int("count", len(policyData)))
	return policyData, nil
}

// flattenSpec recursively flattens a nested map into dot-notation keys.
// Example: {"driver": {"version": "580.82.07"}} becomes "driver.version": "580.82.07"
func flattenSpec(data map[string]any, prefix string, result map[string]measurement.Reading) {
	for key, value := range data {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}

		switch v := value.(type) {
		case map[string]any:
			// Recursively flatten nested maps
			flattenSpec(v, fullKey, result)
		case []any:
			// Convert arrays to JSON strings for readability
			if len(v) > 0 {
				jsonBytes, err := json.Marshal(v)
				if err == nil {
					result[fullKey] = measurement.Str(string(jsonBytes))
				}
			}
		case string:
			result[fullKey] = measurement.Str(v)
		case bool:
			result[fullKey] = measurement.Str(fmt.Sprintf("%t", v))
		case float64:
			result[fullKey] = measurement.Str(fmt.Sprintf("%v", v))
		case int, int64:
			result[fullKey] = measurement.Str(fmt.Sprintf("%d", v))
		default:
			// For any other type, convert to string
			result[fullKey] = measurement.Str(fmt.Sprintf("%v", v))
		}
	}
}
