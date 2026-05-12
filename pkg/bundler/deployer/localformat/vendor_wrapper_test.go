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

package localformat

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRenderWrapperChartYAML(t *testing.T) {
	got, err := renderWrapperChartYAML("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.0")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"name: gpu-operator",
		"- name: gpu-operator",
		"version: v25.3.0",
		`repository: ""`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("Chart.yaml missing %q\n--- got:\n%s", want, got)
		}
	}
}

func TestNestUnderSubchart(t *testing.T) {
	tests := []struct {
		name      string
		values    map[string]any
		subchart  string
		wantNil   bool
		wantOuter string
	}{
		{"happy", map[string]any{"a": 1}, "foo", false, "foo"},
		{"nil values", nil, "foo", true, ""},
		{"empty values", map[string]any{}, "foo", true, ""},
		{"empty subchart", map[string]any{"a": 1}, "", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nestUnderSubchart(tt.values, tt.subchart)
			if tt.wantNil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if _, ok := got[tt.wantOuter]; !ok {
				t.Errorf("expected outer key %q in %v", tt.wantOuter, got)
			}
		})
	}
}

func TestNestUnderSubchart_InnerMapDeepCopy(t *testing.T) {
	// nestUnderSubchart deep-copies the inner map so the caller's source
	// map is unaffected by downstream mutation of the returned value.
	// Without this, a later writer mutating the result would silently
	// mutate the caller's values map and produce non-deterministic
	// bundle content.
	in := map[string]any{"a": 1}
	out := nestUnderSubchart(in, "foo")
	out["foo"].(map[string]any)["b"] = 2

	if _, leaked := in["b"]; leaked {
		t.Errorf("mutation of nested map leaked back to caller; deep copy not effective")
	}
	if got := in["a"]; got != 1 {
		t.Errorf("source map mutated: a = %v, want 1", got)
	}
}

func TestInjectPostInstallHooks_AddsAnnotations(t *testing.T) {
	src := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: foo
data:
  k: v
`)
	got, err := injectPostInstallHooks(src)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	var node map[string]any
	if err := yaml.Unmarshal(got, &node); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, got)
	}
	annos := node["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annos["helm.sh/hook"] != "post-install" {
		t.Errorf("hook = %v, want post-install", annos["helm.sh/hook"])
	}
	if annos["helm.sh/hook-weight"] != "100" {
		t.Errorf("hook-weight = %v, want 100", annos["helm.sh/hook-weight"])
	}
	if annos["helm.sh/hook-delete-policy"] != "before-hook-creation" {
		t.Errorf("hook-delete-policy = %v, want before-hook-creation", annos["helm.sh/hook-delete-policy"])
	}
}

func TestInjectPostInstallHooks_PreservesExistingAnnotations(t *testing.T) {
	// A recipe-author annotation must survive hook injection: the
	// previous map[string]any-only assertion would silently drop it
	// when the YAML decoder produced a different inner map type.
	src := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: foo
  annotations:
    author/owned: keep-me
`)
	got, err := injectPostInstallHooks(src)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !strings.Contains(string(got), "author/owned: keep-me") {
		t.Errorf("existing annotation dropped:\n%s", got)
	}
	if !strings.Contains(string(got), "helm.sh/hook: post-install") {
		t.Errorf("post-install hook not added:\n%s", got)
	}
}

func TestInjectPostInstallHooks_HonorsAuthorDeletePolicy(t *testing.T) {
	// helm.sh/hook-delete-policy is per-resource state; if the author
	// set it explicitly (e.g., "before-hook-creation,hook-succeeded"
	// or just "hook-succeeded") respect that rather than clobbering.
	src := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: foo
  annotations:
    helm.sh/hook-delete-policy: hook-succeeded
`)
	got, err := injectPostInstallHooks(src)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !strings.Contains(string(got), "hook-delete-policy: hook-succeeded") {
		t.Errorf("author-declared hook-delete-policy was overwritten:\n%s", got)
	}
	if strings.Contains(string(got), "hook-delete-policy: before-hook-creation") {
		t.Errorf("default delete-policy clobbered author's:\n%s", got)
	}
}

func TestInjectPostInstallHooks_PreservesAuthorHook(t *testing.T) {
	src := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: foo
  annotations:
    helm.sh/hook: pre-install
data:
  k: v
`)
	got, err := injectPostInstallHooks(src)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !strings.Contains(string(got), "pre-install") {
		t.Errorf("author-declared hook lost: %s", got)
	}
	if strings.Contains(string(got), "post-install") {
		t.Errorf("post-install hook overrode author hook: %s", got)
	}
}

func TestInjectPostInstallHooks_StableWeights(t *testing.T) {
	src := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: a
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: b
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: c
`)
	got, err := injectPostInstallHooks(src)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	for i, want := range []string{`"100"`, `"101"`, `"102"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("doc %d: weight %s not found in:\n%s", i, want, got)
		}
	}
}

func TestInjectPostInstallHooks_CommentOnlyDocDropped(t *testing.T) {
	// Comment-only / empty docs are dropped from the output rather than
	// preserved — Helm doesn't render them, so they contribute nothing
	// to the rendered bundle. Pinned so a contract change is intentional.
	src := []byte("# only a comment\n")
	got, err := injectPostInstallHooks(src)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("comment-only doc not dropped: got %q want \"\"", got)
	}
}

func TestInjectPostInstallHooks_LeadingSeparator(t *testing.T) {
	// A leading `---` is YAML's doc-start indicator, not a separator
	// before an empty first doc. One input doc → one output doc, no
	// stray separator prefix.
	src := []byte(`---
apiVersion: v1
kind: ConfigMap
metadata:
  name: foo
`)
	got, err := injectPostInstallHooks(src)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	// Hook annotation must be present exactly once.
	hookCount := strings.Count(string(got), "helm.sh/hook: post-install")
	if hookCount != 1 {
		t.Errorf("hook count = %d, want 1\n%s", hookCount, got)
	}
	// No stray "---\n" prefix from the parser confusion.
	if strings.HasPrefix(string(got), "---\n") {
		t.Errorf("output begins with stray separator: %q", got)
	}
}

func TestInjectPostInstallHooks_SeparatorInsideString(t *testing.T) {
	// A ConfigMap with a multi-line string containing `---` on its own
	// line: the embedded separator inside a literal-block scalar must
	// not be misread as a YAML document boundary.
	src := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: foo
data:
  README.md: |
    Title
    ---
    Body line
`)
	got, err := injectPostInstallHooks(src)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !strings.Contains(string(got), "Body line") {
		t.Errorf("embedded multi-line string lost; output:\n%s", got)
	}
	if strings.Count(string(got), "helm.sh/hook: post-install") != 1 {
		t.Errorf("expected exactly one hook annotation:\n%s", got)
	}
}
