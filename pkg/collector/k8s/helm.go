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

	"github.com/NVIDIA/aicr/pkg/measurement"

	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// collectHelmReleasesScoped collects Helm releases based on HelmNamespaces config.
// nil/empty = skip collection, ["*"] = all namespaces, ["ns1","ns2"] = scoped.
func (k *Collector) collectHelmReleasesScoped(ctx context.Context) map[string]measurement.Reading {
	if len(k.HelmNamespaces) == 0 {
		slog.Debug("helm collection skipped - no namespaces configured")
		return make(map[string]measurement.Reading)
	}

	if len(k.HelmNamespaces) == 1 && k.HelmNamespaces[0] == "*" {
		return k.collectHelmReleasesInNamespace(ctx, "")
	}

	data := make(map[string]measurement.Reading)
	for _, ns := range k.HelmNamespaces {
		if err := ctx.Err(); err != nil {
			slog.Debug("helm collector context cancelled", slog.String("error", err.Error()))
			return data
		}
		nsData := k.collectHelmReleasesInNamespace(ctx, ns)
		for key, val := range nsData {
			data[key] = val
		}
	}
	return data
}

// collectHelmReleasesInNamespace discovers deployed Helm releases in a single namespace
// (or all namespaces when namespace is "").
// On any error, it degrades gracefully by returning an empty map.
func (k *Collector) collectHelmReleasesInNamespace(ctx context.Context, namespace string) map[string]measurement.Reading {
	if err := ctx.Err(); err != nil {
		slog.Debug("helm collector context cancelled", slog.String("error", err.Error()))
		return make(map[string]measurement.Reading)
	}

	d := driver.NewSecrets(k.ClientSet.CoreV1().Secrets(namespace))
	store := storage.Init(d)

	releases, err := store.ListDeployed()
	if err != nil {
		slog.Warn("failed to list helm releases",
			slog.String("namespace", namespace),
			slog.String("error", err.Error()))
		return make(map[string]measurement.Reading)
	}

	releases = latestReleases(releases)

	data := make(map[string]measurement.Reading)
	for _, rel := range releases {
		if err := ctx.Err(); err != nil {
			slog.Debug("helm collector context cancelled during iteration",
				slog.String("error", err.Error()))
			return data
		}
		mapRelease(rel, data)
	}

	slog.Debug("collected helm releases",
		slog.String("namespace", namespace),
		slog.Int("count", len(releases)))

	return data
}

// mapRelease extracts metadata and flattened config values from a single
// Helm release into the provided readings map. Keys are prefixed with
// the release name (e.g., "gpu-operator.chart", "gpu-operator.values.driver.version").
func mapRelease(rel *release.Release, data map[string]measurement.Reading) {
	if rel == nil {
		return
	}

	prefix := rel.Name

	data[prefix+".namespace"] = measurement.Str(rel.Namespace)
	data[prefix+".revision"] = measurement.Str(fmt.Sprintf("%d", rel.Version))

	if rel.Info != nil {
		data[prefix+".status"] = measurement.Str(string(rel.Info.Status))
	}

	if rel.Chart != nil && rel.Chart.Metadata != nil {
		md := rel.Chart.Metadata
		if md.Name != "" {
			data[prefix+".chart"] = measurement.Str(md.Name)
		}
		if md.Version != "" {
			data[prefix+".version"] = measurement.Str(md.Version)
		}
		if md.AppVersion != "" {
			data[prefix+".appVersion"] = measurement.Str(md.AppVersion)
		}
	}

	if len(rel.Config) > 0 {
		flattenSpec(rel.Config, prefix+".values", data)
	}
}

// latestReleases deduplicates releases by keeping only the highest revision
// per release name+namespace pair.
func latestReleases(releases []*release.Release) []*release.Release {
	if len(releases) == 0 {
		return releases
	}

	type key struct {
		name      string
		namespace string
	}

	latest := make(map[key]*release.Release, len(releases))
	for _, rel := range releases {
		k := key{name: rel.Name, namespace: rel.Namespace}
		if existing, ok := latest[k]; !ok || rel.Version > existing.Version {
			latest[k] = rel
		}
	}

	result := make([]*release.Release, 0, len(latest))
	for _, rel := range latest {
		result = append(result, rel)
	}

	return result
}
