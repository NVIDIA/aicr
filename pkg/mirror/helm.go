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
	"context"
	"os/exec"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// HelmRenderer renders a Helm chart to YAML bytes for image extraction.
// The default implementation shells out to `helm template`; tests inject
// a mock that returns canned YAML.
type HelmRenderer interface {
	Render(ctx context.Context, ref *recipe.ComponentRef, values map[string]any) ([]byte, error)
}

// defaultHelmRenderer delegates to pkg/helm.RenderChart.
type defaultHelmRenderer struct {
	// kubeVersion is passed to `helm template --kube-version` so charts
	// with a kubeVersion constraint render correctly.
	kubeVersion string

	// apiVersions is passed as `helm template --api-versions` entries so
	// charts that gate templates on .Capabilities.APIVersions can render
	// in offline mode.
	apiVersions []string
}

// Render invokes `helm template` for the given component reference and
// returns the rendered YAML.
func (r *defaultHelmRenderer) Render(ctx context.Context, ref *recipe.ComponentRef, values map[string]any) ([]byte, error) {
	if ref == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "component reference must not be nil")
	}

	if _, err := exec.LookPath("helm"); err != nil {
		return nil, errors.New(errors.ErrCodeNotFound,
			"helm binary not found on PATH; install helm to discover images from chart templates")
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.MirrorHelmTemplateTimeout)
	defer cancel()

	out, err := helm.RenderChart(ctx, helm.ChartInput{
		Name:        ref.Name,
		Chart:       ref.Chart,
		Repository:  ref.Source,
		Version:     ref.Version,
		Namespace:   ref.Namespace,
		Values:      values,
		KubeVersion: r.kubeVersion,
		APIVersions: r.apiVersions,
	})
	if err != nil {
		return out, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "helm template rendering failed")
	}

	return out, nil
}
