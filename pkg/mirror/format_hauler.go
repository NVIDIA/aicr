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

// Hauler manifest schema version. Pinned to the stable v1 API introduced
// in Hauler v1.x (current upstream: v1.4.3, 2026-05-05).
//
// Upstream schema reference:
//
//	https://docs.hauler.dev/docs/guides-references/hauler-manifests
//	https://github.com/hauler-dev/hauler (content.hauler.cattle.io/v1)
const haulerAPIVersion = "content.hauler.cattle.io/v1"

// haulerImages is the Images document in a Hauler manifest.
type haulerImages struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   haulerMeta       `yaml:"metadata"`
	Spec       haulerImagesSpec `yaml:"spec"`
}

type haulerMeta struct {
	Name string `yaml:"name"`
}

type haulerImagesSpec struct {
	Images []haulerImage `yaml:"images"`
}

type haulerImage struct {
	Name string `yaml:"name"`
}

// haulerCharts is the Charts document in a Hauler manifest.
type haulerCharts struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   haulerMeta       `yaml:"metadata"`
	Spec       haulerChartsSpec `yaml:"spec"`
}

type haulerChartsSpec struct {
	Charts []haulerChart `yaml:"charts"`
}

type haulerChart struct {
	Name    string `yaml:"name"`
	RepoURL string `yaml:"repoURL"`
	Version string `yaml:"version"`
}

// renderHauler writes a multi-document Hauler manifest (Images + Charts)
// to w. The output can be piped directly into `hauler store sync -f`.
func renderHauler(w io.Writer, list *MirrorList) error {
	if list == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "mirror list is nil")
	}

	// Build Images document.
	images := haulerImages{
		APIVersion: haulerAPIVersion,
		Kind:       "Images",
		Metadata:   haulerMeta{Name: "aicr-images"},
		Spec:       haulerImagesSpec{Images: make([]haulerImage, 0, len(list.Images))},
	}
	for _, img := range list.Images {
		images.Spec.Images = append(images.Spec.Images, haulerImage{Name: img})
	}

	imgBytes, err := serializer.MarshalYAMLDeterministic(images)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal hauler images", err)
	}
	if _, err := w.Write(imgBytes); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write hauler images", err)
	}

	// Build Charts document (only if charts exist).
	if len(list.Charts) > 0 {
		if _, err := io.WriteString(w, "---\n"); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write YAML separator", err)
		}

		charts := haulerCharts{
			APIVersion: haulerAPIVersion,
			Kind:       "Charts",
			Metadata:   haulerMeta{Name: "aicr-charts"},
			Spec:       haulerChartsSpec{Charts: make([]haulerChart, 0, len(list.Charts))},
		}
		for _, ch := range list.Charts {
			// Strip any path prefix from the chart name (e.g., "nvidia/gpu-operator"
			// → "gpu-operator"). Recipe resolution normalizes this via
			// pkg/recipe/metadata.go, but pre-hydrated RecipeResult files loaded
			// via --recipe bypass that path and may carry the raw registry value.
			chartName := ch.Chart
			if idx := strings.LastIndex(chartName, "/"); idx >= 0 {
				chartName = chartName[idx+1:]
			}

			charts.Spec.Charts = append(charts.Spec.Charts, haulerChart{
				Name:    chartName,
				RepoURL: ch.Repository,
				Version: ch.Version,
			})
		}

		chartBytes, marshalErr := serializer.MarshalYAMLDeterministic(charts)
		if marshalErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to marshal hauler charts", marshalErr)
		}
		if _, err := w.Write(chartBytes); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write hauler charts", err)
		}
	}

	return nil
}
