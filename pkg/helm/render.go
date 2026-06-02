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

// Package helm provides shared Helm chart rendering utilities used by
// both the mirror image discovery pipeline and the BOM generator.
package helm

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

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
	// Values are inline values marshaled to a temp file before rendering.
	// When both ValuesPath and Values are set, both are passed via separate
	// --values flags (Helm merges them, with later files winning).
	Values map[string]any
	// KubeVersion is passed to --kube-version. When empty,
	// defaults.MirrorDefaultKubeVersion is used.
	KubeVersion string
	// APIVersions is passed as --api-versions entries. When nil,
	// defaults.MirrorExtraAPIVersions is used.
	APIVersions []string
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
	if _, err := exec.LookPath("helm"); err != nil {
		return nil, errors.New(errors.ErrCodeNotFound,
			"helm binary not found on PATH; install helm to discover images from chart templates")
	}
	return RenderChart(ctx, input)
}

// RenderChart shells out to `helm template` and returns rendered YAML.
// OCI charts (Repository starts with "oci://") use the full URL as the
// chart argument with no --repo flag; HTTP charts use the bare chart name
// plus --repo.
func RenderChart(ctx context.Context, input ChartInput) ([]byte, error) {
	if input.Chart == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "component "+input.Name+": no helm chart configured")
	}

	var (
		chart    string
		repoFlag string
	)

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

	// Inline values: marshal to temp file.
	if len(input.Values) > 0 {
		tmpPath, writeErr := writeValuesFile(input.Values)
		if writeErr != nil {
			return nil, writeErr
		}
		defer os.Remove(tmpPath)
		args = append(args, "--values", tmpPath)
	}

	// Skip CRD installation in render to avoid surfacing CRD-shipped images
	// twice (manifests are walked separately).
	args = append(args, "--include-crds=false")

	// Ensure ctx carries a deadline so the helm subprocess is always bounded.
	// If a parent already set a tighter deadline, keep it; otherwise cap at
	// the shared helm-template timeout.
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline || time.Until(deadline) > defaults.HelmTemplateTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaults.HelmTemplateTimeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "helm", args...)
	var stdoutBuf, stderr bytes.Buffer
	stdout := &limitedWriter{w: &stdoutBuf, limit: defaults.HelmTemplateOutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdoutBuf.Bytes(), errors.WrapWithContext(errors.ErrCodeInternal, "helm template failed", err,
			map[string]any{"component": input.Name, "stderr": strings.TrimSpace(stderr.String())})
	}

	return stdoutBuf.Bytes(), nil
}

// writeValuesFile marshals values to a temporary YAML file and returns
// the path. The caller is responsible for removing the file.
func writeValuesFile(values map[string]any) (string, error) {
	data, err := yaml.Marshal(values)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "marshal values", err)
	}

	f, err := os.CreateTemp("", "aicr-helm-values-*.yaml")
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "create temp values file", err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name()) //nolint:gosec // G703: path is from f.Name() created by CreateTemp above
		return "", errors.Wrap(errors.ErrCodeInternal, "write temp values file", err)
	}

	closeErr := f.Close()
	if closeErr != nil {
		_ = os.Remove(f.Name()) //nolint:gosec // G703: path is from f.Name() created by CreateTemp above
		return "", errors.Wrap(errors.ErrCodeInternal, "close temp values file", closeErr)
	}

	return f.Name(), nil
}

// limitedWriter wraps an io.Writer and enforces a byte cap. Once the cap
// is reached, Write returns an error instead of silently truncating —
// silent truncation would produce partial YAML and mask missing images.
type limitedWriter struct {
	w       *bytes.Buffer
	limit   int64
	written int64
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.written+int64(len(p)) > lw.limit {
		return 0, errors.New(errors.ErrCodeInternal,
			"helm template output exceeds size limit")
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
