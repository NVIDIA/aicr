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
	"fmt"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/measurement"

	"helm.sh/helm/v4/pkg/release"
	v1release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
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
func mapRelease(rel release.Releaser, data map[string]measurement.Reading) {
	r, ok := rel.(*v1release.Release)
	if !ok || r == nil {
		return
	}

	prefix := r.Name

	data[prefix+".namespace"] = measurement.Str(r.Namespace)
	data[prefix+".revision"] = measurement.Str(fmt.Sprintf("%d", r.Version))

	if r.Info != nil {
		data[prefix+".status"] = measurement.Str(string(r.Info.Status))
	}

	if r.Chart != nil && r.Chart.Metadata != nil {
		md := r.Chart.Metadata
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

	if len(r.Config) > 0 {
		flattenSpec(r.Config, prefix+".values", data)
	}
}

// latestReleases deduplicates releases by keeping only the highest revision
// per release name+namespace pair.
func latestReleases(releases []release.Releaser) []release.Releaser {
	if len(releases) == 0 {
		return releases
	}

	type key struct {
		name      string
		namespace string
	}

	type entry struct {
		rel     release.Releaser
		version int
	}

	latest := make(map[key]entry, len(releases))
	for _, rel := range releases {
		r, ok := rel.(*v1release.Release)
		if !ok {
			continue
		}
		k := key{name: r.Name, namespace: r.Namespace}
		if existing, ok := latest[k]; !ok || r.Version > existing.version {
			latest[k] = entry{rel: rel, version: r.Version}
		}
	}

	result := make([]release.Releaser, 0, len(latest))
	for _, e := range latest {
		result = append(result, e.rel)
	}

	return result
}
