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

package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/measurement"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// collectArgocdApplications discovers ArgoCD Application CRDs and extracts
// source, destination, sync, and health metadata as measurement readings.
// Returns an empty map on any error (graceful degradation).
func (k *Collector) collectArgocdApplications(ctx context.Context) map[string]measurement.Reading {
	if err := ctx.Err(); err != nil {
		slog.Debug("argocd collector context cancelled", slog.String("error", err.Error()))
		return make(map[string]measurement.Reading)
	}

	dynamicClient, err := dynamic.NewForConfig(k.RestConfig)
	if err != nil {
		slog.Warn("failed to create dynamic client for argocd", slog.String("error", err.Error()))
		return make(map[string]measurement.Reading)
	}

	discoveryClient := k.ClientSet.Discovery()
	apiResourceLists, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		slog.Debug("error discovering API resources for argocd (continuing with partial results)",
			slog.String("error", err.Error()))
		if len(apiResourceLists) == 0 {
			slog.Debug("no API resources discovered for argocd")
			return make(map[string]measurement.Reading)
		}
	}

	data := make(map[string]measurement.Reading)

	for _, apiResourceList := range apiResourceLists {
		if err := ctx.Err(); err != nil {
			slog.Debug("argocd collector context cancelled during discovery",
				slog.String("error", err.Error()))
			return data
		}

		if apiResourceList == nil {
			continue
		}

		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			continue
		}

		for _, resource := range apiResourceList.APIResources {
			if resource.Kind != "Application" {
				continue
			}

			if len(resource.Name) == 0 || strings.Contains(resource.Name, "/") {
				continue
			}

			// Only match argoproj.io group
			if !strings.HasSuffix(gv.Group, "argoproj.io") {
				continue
			}

			gvr := schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: resource.Name,
			}

			slog.Debug("found argocd application resource",
				slog.String("group", gv.Group),
				slog.String("version", gv.Version),
				slog.String("resource", resource.Name))

			apps, err := dynamicClient.Resource(gvr).Namespace("").List(ctx, v1.ListOptions{})
			if err != nil {
				slog.Debug("failed to list argocd applications",
					slog.String("group", gv.Group),
					slog.String("error", err.Error()))
				continue
			}

			for _, app := range apps.Items {
				if err := ctx.Err(); err != nil {
					slog.Debug("argocd collector context cancelled during iteration",
						slog.String("error", err.Error()))
					return data
				}

				mapArgocdApplication(app.Object, data)
			}
		}
	}

	slog.Debug("collected argocd applications", slog.Int("count", len(data)))

	return data
}

// mapArgocdApplication extracts metadata from a single ArgoCD Application
// unstructured object into the provided readings map.
func mapArgocdApplication(obj map[string]any, data map[string]measurement.Reading) {
	if obj == nil {
		return
	}

	name, _, _ := unstructured.NestedString(obj, "metadata", "name")
	if name == "" {
		return
	}

	ns, _, _ := unstructured.NestedString(obj, "metadata", "namespace")
	if ns != "" {
		data[name+".namespace"] = measurement.Str(ns)
	}

	project, _, _ := unstructured.NestedString(obj, "spec", "project")
	if project != "" {
		data[name+".project"] = measurement.Str(project)
	}

	destNS, _, _ := unstructured.NestedString(obj, "spec", "destination", "namespace")
	if destNS != "" {
		data[name+".targetNamespace"] = measurement.Str(destNS)
	}

	destServer, _, _ := unstructured.NestedString(obj, "spec", "destination", "server")
	if destServer != "" {
		data[name+".destination.server"] = measurement.Str(destServer)
	}

	// Single source (spec.source)
	source, sourceFound, _ := unstructured.NestedMap(obj, "spec", "source")
	if sourceFound {
		mapArgocdSource(source, name+".source", data)
	}

	// Multi-source (spec.sources[])
	sources, sourcesFound, _ := unstructured.NestedSlice(obj, "spec", "sources")
	if sourcesFound {
		for i, s := range sources {
			src, ok := s.(map[string]any)
			if !ok {
				continue
			}
			mapArgocdSource(src, fmt.Sprintf("%s.sources.%d", name, i), data)
		}
	}

	// Sync status
	syncStatus, _, _ := unstructured.NestedString(obj, "status", "sync", "status")
	if syncStatus != "" {
		data[name+".syncStatus"] = measurement.Str(syncStatus)
	}

	// Health status
	healthStatus, _, _ := unstructured.NestedString(obj, "status", "health", "status")
	if healthStatus != "" {
		data[name+".healthStatus"] = measurement.Str(healthStatus)
	}
}

// mapArgocdSource extracts source fields (repoURL, chart, targetRevision, path)
// and flattens Helm values and parameters under the given prefix.
func mapArgocdSource(source map[string]any, prefix string, data map[string]measurement.Reading) {
	for _, field := range []string{"repoURL", "chart", "targetRevision", "path"} {
		if v, ok := source[field].(string); ok && v != "" {
			data[prefix+"."+field] = measurement.Str(v)
		}
	}

	// Helm values string — parse as YAML-like map and flatten
	if valuesStr, ok := source["values"].(string); ok && valuesStr != "" {
		data[prefix+".helm.values"] = measurement.Str(valuesStr)
	}

	// Helm parameters (name/value pairs)
	if helm, ok := source["helm"].(map[string]any); ok {
		if params, ok := helm["parameters"].([]any); ok {
			for _, p := range params {
				param, ok := p.(map[string]any)
				if !ok {
					continue
				}
				pName, _ := param["name"].(string)
				pValue, _ := param["value"].(string)
				if pName != "" {
					data[prefix+".helm.parameters."+pName] = measurement.Str(pValue)
				}
			}
		}

		// Helm values from helm.values (string)
		if hv, ok := helm["values"].(string); ok && hv != "" {
			data[prefix+".helm.values"] = measurement.Str(hv)
		}

		// Helm valueFiles
		if vf, ok := helm["valueFiles"].([]any); ok && len(vf) > 0 {
			files := make([]string, 0, len(vf))
			for _, f := range vf {
				if s, ok := f.(string); ok {
					files = append(files, s)
				}
			}
			if len(files) > 0 {
				data[prefix+".helm.valueFiles"] = measurement.Str(strings.Join(files, ","))
			}
		}
	}
}
