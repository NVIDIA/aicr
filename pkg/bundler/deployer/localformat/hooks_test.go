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
)

func TestStripHelmHooks(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		mustNotContain []string
		mustContain    []string
	}{
		{
			name: "strips all helm.sh/hook* annotations from a single document",
			input: `apiVersion: skyhook.nvidia.com/v1alpha1
kind: Skyhook
metadata:
  annotations:
    "helm.sh/hook": post-install,post-upgrade
    "helm.sh/hook-weight": "10"
    "helm.sh/hook-delete-policy": before-hook-creation
    app.kubernetes.io/managed-by: aicr
  name: tuning
  namespace: skyhook
spec:
  packages:
    - tuning
`,
			mustNotContain: []string{"helm.sh/hook", "post-install", "before-hook-creation"},
			mustContain:    []string{"app.kubernetes.io/managed-by: aicr", "name: tuning", "kind: Skyhook"},
		},
		{
			name: "preserves documents that have no hook annotations",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  annotations:
    app.kubernetes.io/managed-by: aicr
  name: foo
data:
  key: value
`,
			mustNotContain: []string{"helm.sh/hook"},
			mustContain:    []string{"name: foo", "key: value", "managed-by: aicr"},
		},
		{
			name: "handles documents with no annotations key at all",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  name: foo
data:
  key: value
`,
			mustNotContain: []string{"helm.sh/hook"},
			mustContain:    []string{"name: foo", "key: value"},
		},
		{
			name: "strips hooks from each document in a multi-doc stream",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  annotations:
    "helm.sh/hook": pre-install
  name: cm-a
data:
  k: v
---
apiVersion: v1
kind: ConfigMap
metadata:
  annotations:
    "helm.sh/hook-weight": "5"
    keep: this
  name: cm-b
`,
			mustNotContain: []string{"helm.sh/hook", "pre-install", `"5"`},
			mustContain:    []string{"name: cm-a", "name: cm-b", "keep: this"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := stripHelmHooks([]byte(tt.input))
			if err != nil {
				t.Fatalf("stripHelmHooks error: %v", err)
			}
			s := string(out)
			for _, want := range tt.mustContain {
				if !strings.Contains(s, want) {
					t.Errorf("output missing %q\n--- got ---\n%s", want, s)
				}
			}
			for _, banned := range tt.mustNotContain {
				if strings.Contains(s, banned) {
					t.Errorf("output should not contain %q\n--- got ---\n%s", banned, s)
				}
			}
		})
	}
}

// TestStripHelmHooks_EmptyInput verifies pass-through for input with no
// YAML documents (whitespace-only or empty). The wrapping pipeline checks
// hasYAMLObjects after strip, so empty pass-through is sufficient.
func TestStripHelmHooks_EmptyInput(t *testing.T) {
	for _, in := range []string{"", "   \n", "# only a comment\n"} {
		out, err := stripHelmHooks([]byte(in))
		if err != nil {
			t.Errorf("stripHelmHooks(%q) error: %v", in, err)
		}
		if string(out) != in {
			t.Errorf("expected pass-through for %q, got %q", in, out)
		}
	}
}

// TestStripHelmHooks_SeparatorOnly verifies that separator-only multi-doc
// streams (no resource content, just `---` and comments) do not re-emit as
// `null` documents, which would defeat the hasYAMLObjects skip in
// local_helm.go and cause empty/null templates to land on disk.
func TestStripHelmHooks_SeparatorOnly(t *testing.T) {
	in := "---\n# comment only\n---\n"
	out, err := stripHelmHooks([]byte(in))
	if err != nil {
		t.Fatalf("stripHelmHooks error: %v", err)
	}
	if strings.Contains(string(out), "null") {
		t.Errorf("output should not contain re-emitted null document\n--- got ---\n%s", string(out))
	}
}

// TestStripHelmHooks_MalformedYAML pins the parse-error path: structurally
// invalid YAML must surface as an error rather than silently passing through.
func TestStripHelmHooks_MalformedYAML(t *testing.T) {
	// Mapping value where indentation forces the parser to reject (a list
	// item starting at the same column as the parent mapping key).
	in := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: bad\ndata:\n key: : :\n - bare\n"
	if _, err := stripHelmHooks([]byte(in)); err == nil {
		t.Fatalf("expected error for malformed YAML, got nil")
	}
}

// TestStripHelmHooks_PreservesLifecycleHooks confirms that pre-delete and
// post-delete annotations are preserved (Argo CD maps them to PreDelete/
// PostDelete phases that fire on Application deletion — folder ordering
// does NOT replace them, so stripping them would silently lose lifecycle
// behavior). Mixed values like "post-install,pre-delete" must be rewritten
// to drop only the sync-phase entries.
func TestStripHelmHooks_PreservesLifecycleHooks(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		mustNotContain []string
		mustContain    []string
	}{
		{
			name: "pre-delete-only annotation is preserved",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  annotations:
    "helm.sh/hook": pre-delete
    "helm.sh/hook-weight": "5"
    "helm.sh/hook-delete-policy": before-hook-creation
  name: cleanup
`,
			mustContain:    []string{"helm.sh/hook", "pre-delete", "hook-weight", "hook-delete-policy", `"5"`},
			mustNotContain: []string{"post-install"},
		},
		{
			name: "post-delete-only annotation is preserved",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  annotations:
    "helm.sh/hook": post-delete
  name: finalize
`,
			mustContain:    []string{"helm.sh/hook", "post-delete"},
			mustNotContain: []string{},
		},
		{
			name: "mixed sync-phase + lifecycle: sync-phase entries dropped, lifecycle kept",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  annotations:
    "helm.sh/hook": post-install,pre-delete
    "helm.sh/hook-weight": "1"
  name: mixed
`,
			mustContain:    []string{"helm.sh/hook", "pre-delete", "hook-weight", `"1"`},
			mustNotContain: []string{"post-install"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := stripHelmHooks([]byte(tt.input))
			if err != nil {
				t.Fatalf("stripHelmHooks error: %v", err)
			}
			s := string(out)
			for _, want := range tt.mustContain {
				if !strings.Contains(s, want) {
					t.Errorf("output missing %q\n--- got ---\n%s", want, s)
				}
			}
			for _, banned := range tt.mustNotContain {
				if strings.Contains(s, banned) {
					t.Errorf("output should not contain %q\n--- got ---\n%s", banned, s)
				}
			}
		})
	}
}
