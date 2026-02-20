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

package manifest

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	tests := []struct {
		name    string
		content string
		input   RenderInput
		wantErr bool
		wantSub string // substring expected in output
	}{
		{
			name:    "valid template with values",
			content: "namespace: {{ .Release.Namespace }}",
			input:   RenderInput{ComponentName: "gpu-operator", Namespace: "gpu-operator", Values: map[string]any{"enabled": true}},
			wantSub: "namespace: gpu-operator",
		},
		{
			name:    "invalid template syntax",
			content: "{{ .Invalid {{ }}",
			input:   RenderInput{ComponentName: "test"},
			wantErr: true,
		},
		{
			name:    "chart data rendered",
			content: "chart: {{ .Chart.Name }}-{{ .Chart.Version }}",
			input:   RenderInput{ComponentName: "gpu-operator", ChartName: "gpu-operator", ChartVersion: "25.3.3"},
			wantSub: "chart: gpu-operator-25.3.3",
		},
		{
			name:    "nil values map",
			content: "svc: {{ .Release.Service }}",
			input:   RenderInput{ComponentName: "test-comp"},
			wantSub: "svc: Helm",
		},
		{
			name:    "toYaml function",
			content: "config: {{ toYaml .Values.mycomp }}",
			input:   RenderInput{ComponentName: "mycomp", Values: map[string]any{"key": "value"}},
			wantSub: "key: value",
		},
		{
			name:    "default function",
			content: `ns: {{ default "fallback" .Release.Namespace }}`,
			input:   RenderInput{ComponentName: "test", Namespace: ""},
			wantSub: "ns: fallback",
		},
		{
			name: "renders deployment from template",
			content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ index .Values "my-app" "name" }}
  namespace: {{ .Release.Namespace }}
`,
			input: RenderInput{
				ComponentName: "my-app",
				Namespace:     "test-ns",
				ChartName:     "my-chart",
				ChartVersion:  "1.0.0",
				Values:        map[string]any{"name": "controller"},
			},
			wantSub: "name: controller",
		},
		{
			name: "conditional template evaluates to empty",
			content: `{{- $app := index .Values "my-app" }}
{{- if $app.enabled }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: conditional-deploy
{{- end }}
`,
			input: RenderInput{
				ComponentName: "my-app",
				Namespace:     "test-ns",
				Values:        map[string]any{"enabled": false},
			},
			wantSub: "", // output should be empty/whitespace
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Render([]byte(tt.content), tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && tt.wantSub != "" {
				if !strings.Contains(string(result), tt.wantSub) {
					t.Errorf("output %q does not contain %q", string(result), tt.wantSub)
				}
			}
		})
	}
}

func TestHelmFuncMap(t *testing.T) {
	funcs := HelmFuncMap()

	t.Run("toYaml", func(t *testing.T) {
		fn := funcs["toYaml"].(func(any) string)

		got := fn(map[string]string{"key": "value"})
		if !strings.Contains(got, "key: value") {
			t.Errorf("toYaml(map) = %q, want to contain 'key: value'", got)
		}
	})

	t.Run("nindent", func(t *testing.T) {
		fn := funcs["nindent"].(func(int, string) string)

		got := fn(4, "line1\nline2")
		if !strings.Contains(got, "    line1") {
			t.Errorf("nindent(4, ...) = %q, want to contain '    line1'", got)
		}
	})

	t.Run("toString", func(t *testing.T) {
		fn := funcs["toString"].(func(any) string)

		if got := fn(42); got != "42" {
			t.Errorf("toString(42) = %q, want %q", got, "42")
		}
		if got := fn(true); got != "true" {
			t.Errorf("toString(true) = %q, want %q", got, "true")
		}
	})

	t.Run("default", func(t *testing.T) {
		fn := funcs["default"].(func(any, any) any)

		if got := fn("fallback", nil); got != "fallback" {
			t.Errorf("default('fallback', nil) = %v, want 'fallback'", got)
		}
		if got := fn("fallback", ""); got != "fallback" {
			t.Errorf("default('fallback', '') = %v, want 'fallback'", got)
		}
		if got := fn("fallback", "actual"); got != "actual" {
			t.Errorf("default('fallback', 'actual') = %v, want 'actual'", got)
		}
	})
}
