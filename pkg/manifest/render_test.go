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
			name:    "replace and trunc in chart label",
			content: `helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}`,
			input:   RenderInput{ComponentName: "test", ChartName: "gpu-operator-pre", ChartVersion: "0.1.0+09d01b0b4d7d"},
			wantSub: "helm.sh/chart: gpu-operator-pre-0.1.0_09d01b0b4d7d",
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

func TestHelmFuncMapFunctions(t *testing.T) {
	funcs := helmFuncMap()

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

	t.Run("replace", func(t *testing.T) {
		fn := funcs["replace"].(func(string, string, string) string)

		if got := fn("+", "_", "1.0.0+build"); got != "1.0.0_build" {
			t.Errorf("replace('+', '_', '1.0.0+build') = %q, want %q", got, "1.0.0_build")
		}
		if got := fn("a", "b", "aaa"); got != "bbb" {
			t.Errorf("replace('a', 'b', 'aaa') = %q, want %q", got, "bbb")
		}
		if got := fn("x", "y", "no match"); got != "no match" {
			t.Errorf("replace('x', 'y', 'no match') = %q, want %q", got, "no match")
		}
	})

	t.Run("quote", func(t *testing.T) {
		fn := funcs["quote"].(func(...any) string)

		tests := []struct {
			name string
			in   []any
			want string
		}{
			{"single string", []any{"hello"}, `"hello"`},
			{"multiple values", []any{"a", "b"}, `"a" "b"`},
			{"integer", []any{42}, `"42"`},
			{"nil skipped", []any{"a", nil, "b"}, `"a" "b"`},
			{"empty input", []any{}, ""},
			{"bool", []any{true}, `"true"`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := fn(tt.in...); got != tt.want {
					t.Errorf("quote(%v) = %q, want %q", tt.in, got, tt.want)
				}
			})
		}
	})

	t.Run("trunc", func(t *testing.T) {
		fn := funcs["trunc"].(func(int, string) string)

		if got := fn(5, "hello world"); got != "hello" {
			t.Errorf("trunc(5, 'hello world') = %q, want %q", got, "hello")
		}
		if got := fn(63, "short"); got != "short" {
			t.Errorf("trunc(63, 'short') = %q, want %q", got, "short")
		}
		if got := fn(0, "test"); got != "" {
			t.Errorf("trunc(0, 'test') = %q, want %q", got, "")
		}
		// Sprig semantics: negative c returns last |c| chars.
		if got := fn(-1, "test"); got != "t" {
			t.Errorf("trunc(-1, 'test') = %q, want %q", got, "t")
		}
		if got := fn(-5, "hello world"); got != "world" {
			t.Errorf("trunc(-5, 'hello world') = %q, want %q", got, "world")
		}
		if got := fn(-20, "short"); got != "short" {
			t.Errorf("trunc(-20, 'short') = %q, want %q", got, "short")
		}
	})
}
