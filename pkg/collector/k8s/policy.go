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

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"

	"golang.org/x/sync/errgroup"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// policyListConcurrency caps fan-out of per-GVR ClusterPolicy List calls.
// Each call is a network RTT; small bound keeps the apiserver QPS reasonable
// while collapsing N×RTT to roughly N/policyListConcurrency rounds.
const policyListConcurrency = 4

// collectClusterPolicies retrieves ClusterPolicy custom resources from all API groups and namespaces.
// It dynamically discovers all ClusterPolicy CRDs regardless of their API group.
func (k *Collector) collectClusterPolicies(ctx context.Context) (map[string]measurement.Reading, error) {
	// Resolve the cached dynamic client (constructed lazily on first call).
	dynamicClient, err := k.getDynamicClient()
	if err != nil {
		return nil, err
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

	// Phase 1: enumerate every ClusterPolicy GVR. Discovery is already done;
	// this is in-memory filtering, no I/O.
	gvrs := make([]schema.GroupVersionResource, 0)
	for _, apiResourceList := range apiResourceLists {
		if apiResourceList == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			continue
		}
		for _, resource := range apiResourceList.APIResources {
			if resource.Kind != "ClusterPolicy" {
				continue
			}
			// Skip subresources (e.g. "clusterpolicies/status").
			if len(resource.Name) == 0 || strings.Contains(resource.Name, "/") {
				continue
			}
			gvrs = append(gvrs, schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: resource.Name,
			})
		}
	}

	// Phase 2: fan-out per-GVR Lists. Sequential N×RTT becomes ~⌈N/conc⌉
	// rounds. Each goroutine writes its result into a pre-sized indexed
	// slot so the post-Wait merge happens in stable GVR order — overlapping
	// keys are not last-writer-wins on goroutine completion order.
	results := make([]map[string]measurement.Reading, len(gvrs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(policyListConcurrency)

	for i, gvr := range gvrs {
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return errors.Wrap(errors.ErrCodeTimeout, "policy collection cancelled", err)
			}
			slog.Debug("found clusterpolicy resource",
				slog.String("group", gvr.Group),
				slog.String("version", gvr.Version),
				slog.String("resource", gvr.Resource))

			policies, err := dynamicClient.Resource(gvr).Namespace("").List(gctx, v1.ListOptions{})
			if err != nil {
				// CRDs can come and go between discovery and List; degrade gracefully.
				slog.Debug("failed to list clusterpolicy",
					slog.String("group", gvr.Group),
					slog.String("error", err.Error()))
				return nil
			}
			local := make(map[string]measurement.Reading)
			for _, policy := range policies.Items {
				if err := gctx.Err(); err != nil {
					return errors.Wrap(errors.ErrCodeTimeout, "policy collection cancelled", err)
				}
				spec, found, err := unstructured.NestedMap(policy.Object, "spec")
				if err != nil || !found {
					slog.Warn("failed to extract spec from clusterpolicy",
						slog.String("name", policy.GetName()),
						slog.String("error", fmt.Sprintf("%v", err)))
					continue
				}
				flattenSpec(spec, "", local)
			}
			results[i] = local
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Ordered merge so overlapping keys resolve deterministically by GVR order.
	policyData := make(map[string]measurement.Reading)
	for _, m := range results {
		for k, v := range m {
			policyData[k] = v
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
