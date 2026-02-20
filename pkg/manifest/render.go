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

// Package manifest provides Helm-compatible template rendering for manifest files.
// Both the bundler and validator use this package to render Go-templated manifests
// that use .Values, .Release, .Chart, and Helm functions (toYaml, nindent, etc.).
package manifest

import (
	"fmt"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
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
// like toYaml, nindent, toString, and default.
func Render(content []byte, input RenderInput) ([]byte, error) {
	tmpl, err := template.New("manifest").Funcs(HelmFuncMap()).Parse(string(content))
	if err != nil {
		return nil, err
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
		return nil, err
	}
	return []byte(buf.String()), nil
}

// HelmFuncMap returns Helm-compatible template functions for manifest rendering.
func HelmFuncMap() template.FuncMap {
	return template.FuncMap{
		"toYaml": func(v any) string {
			out, err := yaml.Marshal(v)
			if err != nil {
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
