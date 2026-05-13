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

// Package manifest provides Helm-compatible template rendering for manifest files.
// Both the bundler and validator use this package to render Go-templated manifests
// that use .Values, .Release, .Chart, and Helm functions (toYaml, nindent, etc.).
package manifest

import (
	"fmt"
	"log/slog"
	"strings"
	"text/template"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// RenderInput provides the data needed to render a manifest template.
type RenderInput struct {
	// ComponentName is the component identifier, used as the Values map key.
	ComponentName string
	// Namespace is the release namespace (.Release.Namespace).
	Namespace string
	// ChartName is the chart name (.Chart.Name).
	ChartName string
	// ChartVersion is the normalized chart version without 'v' prefix (.Chart.Version).
	ChartVersion string
	// Values is the component values map, accessible as .Values[ComponentName].
	Values map[string]any
}

// templateData provides Helm-compatible template data for rendering manifests.
type templateData struct {
	Values  map[string]any
	Release releaseData
	Chart   chartData
}

type releaseData struct {
	Namespace string
	Service   string
}

type chartData struct {
	Name    string
	Version string
}

// Render renders manifest content as a Go template with Helm-compatible data
// and functions. Templates can use .Values, .Release, .Chart, and functions
// like toYaml, nindent, toString, and default. Bounded to MaxSpecFileBytes
// as a defense-in-depth check; callers reading from disk/network should
// already enforce the same limit at the source.
func Render(content []byte, input RenderInput) ([]byte, error) {
	if int64(len(content)) > defaults.MaxSpecFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("manifest content exceeds limit of %d bytes", defaults.MaxSpecFileBytes))
	}

	tmpl, err := template.New("manifest").Funcs(helmFuncMap()).Parse(string(content))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse manifest template", err)
	}

	data := templateData{
		Values: map[string]any{input.ComponentName: input.Values},
		Release: releaseData{
			Namespace: input.Namespace,
			Service:   "Helm",
		},
		Chart: chartData{
			Name:    input.ChartName,
			Version: input.ChartVersion,
		},
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to execute manifest template", err)
	}
	return []byte(buf.String()), nil
}

// helmFuncMap returns Helm-compatible template functions for manifest rendering.
func helmFuncMap() template.FuncMap {
	return template.FuncMap{
		"toYaml": func(v any) string {
			out, err := serializer.MarshalYAMLDeterministic(v)
			if err != nil {
				// Surface marshal failures to operators; silently returning
				// "" produces a blank manifest field that is hard to debug.
				slog.Error("toYaml marshal failed in manifest template",
					"error", err)
				return ""
			}
			return strings.TrimSuffix(string(out), "\n")
		},
		"nindent": func(indent int, s string) string {
			pad := strings.Repeat(" ", indent)
			lines := strings.Split(s, "\n")
			for i, line := range lines {
				if line != "" {
					lines[i] = pad + line
				}
			}
			return "\n" + strings.Join(lines, "\n")
		},
		"toString": func(v any) string {
			return fmt.Sprintf("%v", v)
		},
		"default": func(def, val any) any {
			if val == nil {
				return def
			}
			if s, ok := val.(string); ok && s == "" {
				return def
			}
			return val
		},
	}
}
