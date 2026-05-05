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
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// renderChart shells out to `helm template` and returns rendered YAML.
// The chart-arg vs --repo split mirrors pkg/bundler/deployer/localformat:
// OCI charts are addressed as a full URL with no --repo flag; HTTP charts
// pass the bare chart name plus --repo.
func renderChart(ctx context.Context, c component, valuesPath string) ([]byte, error) {
	if c.Helm.DefaultChart == "" {
		return nil, fmt.Errorf("component %s: no helm chart configured", c.Name)
	}

	var (
		chart    string
		repoFlag string
	)
	if c.Helm.isOCI() {
		chart = strings.TrimRight(c.Helm.DefaultRepository, "/") + "/" + lastPath(c.Helm.DefaultChart)
	} else {
		repoFlag = c.Helm.DefaultRepository
		// Strip any virtual repo prefix (e.g., "nvidia/gpu-operator" → "gpu-operator").
		chart = lastPath(c.Helm.DefaultChart)
	}

	args := []string{"template", "release-" + c.Name, chart}
	if repoFlag != "" {
		args = append(args, "--repo", repoFlag)
	}
	if c.Helm.DefaultVersion != "" {
		args = append(args, "--version", c.Helm.DefaultVersion)
	}
	if c.Helm.DefaultNamespace != "" {
		args = append(args, "--namespace", c.Helm.DefaultNamespace)
	}
	if valuesPath != "" {
		args = append(args, "--values", valuesPath)
	}
	// Skip CRD installation in render to avoid surfacing CRD-shipped images
	// twice via a sidecar mechanism (we still walk manifests separately).
	args = append(args, "--include-crds=false")

	cmd := exec.CommandContext(ctx, "helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("helm template %s: %w: %s",
			c.Name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func lastPath(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
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
