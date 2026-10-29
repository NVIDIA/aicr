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

// Package helm runs the helm CLI and renders charts with helm template.
package helm

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// ChartInput carries the parameters for a single helm template invocation.
type ChartInput struct {
	// Name is the release name suffix (rendered as "release-<Name>").
	Name string
	// Chart is the chart name (e.g., "gpu-operator").
	Chart string
	// Repository is the chart repository URL (oci:// or https://).
	Repository string
	// Version is the pinned chart version.
	Version string
	// Namespace is the target Kubernetes namespace.
	Namespace string
	// ValuesPath is a path to a values.yaml file on disk. Empty means skip.
	ValuesPath string
	// Values are inline values piped to helm on stdin. When both ValuesPath
	// and Values are set, both are passed via separate --values flags (Helm
	// merges them, with Values winning).
	Values map[string]any
	// KubeVersion is passed to --kube-version. When empty,
	// defaults.MirrorDefaultKubeVersion is used.
	KubeVersion string
	// APIVersions is passed as --api-versions entries. When nil,
	// defaults.MirrorExtraAPIVersions is used.
	APIVersions []string
	// IncludeCRDs is passed to --include-crds.
	IncludeCRDs bool
}

// Renderer renders a Helm chart to YAML bytes. The default implementation
// (CLIRenderer) shells out to `helm template`; tests inject a mock that
// returns canned YAML without requiring the helm binary on PATH.
type Renderer interface {
	Render(ctx context.Context, input ChartInput) ([]byte, error)
}

// CLIRenderer implements Renderer by shelling out to the helm CLI binary.
type CLIRenderer struct{}

// Default returns a CLIRenderer suitable for production use.
func Default() Renderer { return &CLIRenderer{} }

// Render delegates to RenderChart after verifying the helm binary is
// available on PATH.
func (r *CLIRenderer) Render(ctx context.Context, input ChartInput) ([]byte, error) {
	if _, err := exec.LookPath(Binary); err != nil {
		return nil, errors.New(errors.ErrCodeNotFound,
			"helm binary not found on PATH; install helm to discover images from chart templates")
	}
	return RenderChart(ctx, input)
}

// RenderChart returns the YAML helm template renders for input. It fails as
// Run does, and with ErrCodeInvalidRequest when input.Chart is empty.
func RenderChart(ctx context.Context, input ChartInput) ([]byte, error) {
	return renderChart(ctx, exec.CommandContext, input)
}

func renderChart(ctx context.Context, command commandFunc, input ChartInput) ([]byte, error) {
	if input.Chart == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "component "+input.Name+": no helm chart configured")
	}

	var stdin io.Reader
	if len(input.Values) > 0 {
		data, err := yaml.Marshal(input.Values)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "marshal values", err)
		}
		stdin = bytes.NewReader(data)
	}
	return run(ctx, command, stdin, defaults.HelmTemplateTimeout, defaults.HelmTemplateOutputLimit,
		templateArgs(input, stdin != nil)...)
}

// templateArgs returns the helm template arguments for input. When
// valuesOnStdin is set, they read values from stdin.
func templateArgs(input ChartInput, valuesOnStdin bool) []string {
	var (
		chart    string
		repoFlag string
	)

	// OCI charts are addressed by full URL with no --repo. HTTP charts use
	// the bare chart name plus --repo.
	if strings.HasPrefix(input.Repository, "oci://") {
		chart = strings.TrimRight(input.Repository, "/") + "/" + lastPathSegment(input.Chart)
	} else {
		repoFlag = input.Repository
		chart = lastPathSegment(input.Chart)
	}

	args := []string{"template", "release-" + input.Name, chart}
	if repoFlag != "" {
		args = append(args, "--repo", repoFlag)
	}
	if input.Version != "" {
		args = append(args, "--version", input.Version)
	}
	if input.Namespace != "" {
		args = append(args, "--namespace", input.Namespace)
	}

	// Kube version: use input or fall back to project default.
	kubeVersion := input.KubeVersion
	if kubeVersion == "" {
		kubeVersion = defaults.MirrorDefaultKubeVersion
	}
	args = append(args, "--kube-version", kubeVersion)

	// API versions: use input or fall back to project default.
	apiVersions := input.APIVersions
	if apiVersions == nil {
		apiVersions = defaults.MirrorExtraAPIVersions
	}
	for _, api := range apiVersions {
		args = append(args, "--api-versions", api)
	}

	// On-disk values file.
	if input.ValuesPath != "" {
		args = append(args, "--values", input.ValuesPath)
	}

	// Later --values flags win, so inline values override ValuesPath.
	if valuesOnStdin {
		args = append(args, "--values", "-")
	}

	if input.IncludeCRDs {
		return append(args, "--include-crds=true")
	}
	// Skip CRD installation in render to avoid surfacing CRD-shipped images
	// twice (manifests are walked separately).
	return append(args, "--include-crds=false")
}

// summarizeHelmError returns the text of the first stderr line that starts
// with "Error:", plus any continuation lines up to the next blank line,
// joined by spaces. Warnings and other lines that merely mention "Error:" are
// ignored. It returns "" when stderr has no such line.
func summarizeHelmError(stderr string) string {
	var parts []string
	found := false
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case !found:
			if rest, ok := strings.CutPrefix(line, "Error:"); ok {
				found = true
				parts = append(parts, strings.TrimSpace(rest))
			}
		case line == "":
			return strings.Join(parts, " ")
		default:
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, " ")
}

// limitedWriter wraps an io.Writer and enforces a byte cap. Once the cap
// is reached, Write returns an error instead of silently truncating —
// silent truncation would produce partial YAML and mask missing images.
type limitedWriter struct {
	w        *bytes.Buffer
	limit    int64
	written  int64
	exceeded bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.written+int64(len(p)) > lw.limit {
		lw.exceeded = true
		return 0, errors.New(errors.ErrCodeInternal,
			"helm output exceeds size limit")
	}
	n, err := lw.w.Write(p)
	lw.written += int64(n)
	return n, err //nolint:wrapcheck // bytes.Buffer.Write never errors; propagation is safe
}

// lastPathSegment returns the last path segment of s (after the final "/").
// If s contains no slash, it is returned unchanged.
func lastPathSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}
