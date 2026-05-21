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

package mirror

import (
	"io"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// Zarf package config schema. Pinned to the v1alpha1 API defined in
// Zarf v0.76.0 (2026-05-14). The canonical Go type lives at
// github.com/zarf-dev/zarf/src/api/v1alpha1.ZarfPackageConfig.
//
// Upstream schema reference:
//   https://docs.zarf.dev/docs/create-a-zarf-package/zarf-schema
//   https://github.com/zarf-dev/zarf/blob/main/zarf.schema.json
//
// We define minimal local structs instead of importing the upstream Go
// module to avoid pulling Zarf's large transitive dependency tree.

// zarfAPIVersion is the upstream schema version (zarf.dev/v1alpha1).
const zarfAPIVersion = "zarf.dev/v1alpha1"

// zarfPackageConfig is the root ZarfPackageConfig document.
type zarfPackageConfig struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   zarfMeta        `yaml:"metadata"`
	Components []zarfComponent `yaml:"components"`
}

type zarfMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type zarfComponent struct {
	Name     string      `yaml:"name"`
	Required bool        `yaml:"required"`
	Images   []string    `yaml:"images,omitempty"`
	Charts   []zarfChart `yaml:"charts,omitempty"`
}

type zarfChart struct {
	Name      string `yaml:"name"`
	Version   string `yaml:"version"`
	URL       string `yaml:"url"`
	Namespace string `yaml:"namespace,omitempty"`
	// RepoName is required for non-OCI charts so Zarf can identify the
	// Helm repository name when the URL is an HTTPS repo (not a direct
	// chart reference).
	RepoName string `yaml:"repoName,omitempty"`
}

// renderZarf writes a ZarfPackageConfig YAML to w. The output can be
// saved as zarf.yaml and consumed by `zarf package create`.
func renderZarf(w io.Writer, list *MirrorList) error {
	if list == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "mirror list is nil")
	}

	charts := make([]zarfChart, 0, len(list.Charts))
	for _, ch := range list.Charts {
		// Strip any path prefix from the chart name (e.g., "nvidia/gpu-operator"
		// → "gpu-operator"). Recipe resolution normalizes this via
		// pkg/recipe/metadata.go, but pre-hydrated RecipeResult files loaded
		// via --recipe bypass that path and may carry the raw registry value.
		chartName := ch.Chart
		if idx := strings.LastIndex(chartName, "/"); idx >= 0 {
			chartName = chartName[idx+1:]
		}

		zc := zarfChart{
			Name:      chartName,
			Version:   ch.Version,
			Namespace: ch.Namespace,
		}

		// OCI charts use the full OCI URL; HTTPS charts use the repo URL
		// with a separate repoName field.
		if strings.HasPrefix(ch.Repository, "oci://") {
			zc.URL = strings.TrimRight(ch.Repository, "/") + "/" + chartName
		} else {
			zc.URL = ch.Repository
			zc.RepoName = chartName
		}

		charts = append(charts, zc)
	}

	pkg := zarfPackageConfig{
		APIVersion: zarfAPIVersion,
		Kind:       "ZarfPackageConfig",
		Metadata: zarfMeta{
			Name:        "aicr",
			Description: "NVIDIA AI Cluster Runtime container images and Helm charts for air-gapped deployment",
		},
		Components: []zarfComponent{
			{
				Name:     "aicr-images",
				Required: true,
				Images:   list.Images,
				Charts:   charts,
			},
		},
	}

	data, err := serializer.MarshalYAMLDeterministic(pkg)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal zarf package config", err)
	}
	if _, err := w.Write(data); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write zarf package config", err)
	}

	return nil
}
