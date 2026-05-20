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

package main

import (
	"context"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/helm"
)

// renderChart delegates to pkg/helm.RenderChart after mapping the
// component struct into a ChartInput.
func renderChart(ctx context.Context, c component, valuesPath string) ([]byte, error) {
	return helm.RenderChart(ctx, helm.ChartInput{
		Name:       c.Name,
		Chart:      c.Helm.DefaultChart,
		Repository: c.Helm.DefaultRepository,
		Version:    c.Helm.DefaultVersion,
		Namespace:  c.Helm.DefaultNamespace,
		ValuesPath: valuesPath,
	})
}

// componentValuesPath returns the canonical values.yaml path for a component.
// The caller is responsible for stat-checking; this is purely a path joiner.
func componentValuesPath(repoRoot, name string) string {
	return filepath.Join(repoRoot, "recipes", "components", name, "values.yaml")
}

// componentManifestsDir returns the embedded-manifests directory path for a
// component. The caller is responsible for stat-checking.
func componentManifestsDir(repoRoot, name string) string {
	return filepath.Join(repoRoot, "recipes", "components", name, "manifests")
}
