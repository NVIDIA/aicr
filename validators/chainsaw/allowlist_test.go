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

package chainsaw

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kyverno/chainsaw/pkg/apis/v1alpha1"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestValidateTestReadOnly(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantErr     bool
		wantContain string // substring of error message
	}{
		{
			name: "assert-only passes",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: ok
spec:
  steps:
    - try:
        - assert:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                name: foo
`,
			wantErr: false,
		},
		{
			name: "error-only passes",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: ok
spec:
  steps:
    - try:
        - error:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                phase: Failed
`,
			wantErr: false,
		},
		{
			name: "assert + error mix passes",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: ok
spec:
  steps:
    - try:
        - assert:
            resource: {apiVersion: v1, kind: Pod, metadata: {name: foo}}
        - error:
            resource: {apiVersion: v1, kind: Pod, metadata: {phase: Failed}}
`,
			wantErr: false,
		},
		{
			name: "script rejected",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: bad
spec:
  steps:
    - name: pwn
      try:
        - script:
            content: "rm -rf /"
`,
			wantErr:     true,
			wantContain: `"script"`,
		},
		{
			name: "apply rejected",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: bad
spec:
  steps:
    - try:
        - apply:
            file: foo.yaml
`,
			wantErr:     true,
			wantContain: `"apply"`,
		},
		{
			name: "delete rejected",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: bad
spec:
  steps:
    - try:
        - delete:
            ref: {apiVersion: v1, kind: Pod, name: foo}
`,
			wantErr:     true,
			wantContain: `"delete"`,
		},
		{
			name: "proxy rejected",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: bad
spec:
  steps:
    - try:
        - proxy:
            apiVersion: v1
            kind: Pod
            name: foo
            namespace: kube-system
            targetPath: /healthz
`,
			wantErr:     true,
			wantContain: `"proxy"`,
		},
		{
			name: "wait rejected",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: bad
spec:
  steps:
    - try:
        - wait:
            apiVersion: v1
            kind: Pod
            for:
              condition:
                name: Ready
`,
			wantErr:     true,
			wantContain: `"wait"`,
		},
		{
			name: "top-level spec.catch rejected",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: bad
spec:
  catch:
    - delete:
        ref: {apiVersion: v1, kind: Pod, name: foo}
  steps:
    - try:
        - assert:
            resource: {apiVersion: v1, kind: Pod, metadata: {name: foo}}
`,
			wantErr:     true,
			wantContain: `spec.catch[0]`,
		},
		{
			name: "second document in multi-doc YAML is rejected",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: ok-first
spec:
  steps:
    - try:
        - assert:
            resource: {apiVersion: v1, kind: Pod, metadata: {name: foo}}
---
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: bad-second
spec:
  steps:
    - try:
        - script:
            content: "rm -rf /"
`,
			wantErr:     true,
			wantContain: `doc[1]`,
		},
		{
			name: "catch with only metadata still rejected (fail-closed)",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: bad
spec:
  steps:
    - try:
        - assert:
            resource: {apiVersion: v1, kind: Pod, metadata: {name: foo}}
      catch:
        - description: "diagnostic block with no op"
`,
			wantErr:     true,
			wantContain: `<catch-finally-block>`,
		},
		{
			name: "catch block rejected",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: bad
spec:
  steps:
    - try:
        - assert:
            resource: {apiVersion: v1, kind: Pod, metadata: {name: foo}}
      catch:
        - podLogs:
            namespace: kube-system
`,
			wantErr:     true,
			wantContain: `"podLogs"`,
		},
		{
			name: "malformed YAML returns ErrCodeInvalidRequest",
			yaml: `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
spec:
  steps:
    - try: [not-a-mapping]
`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTestReadOnly("test-component", tt.yaml)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				var se *errors.StructuredError
				if !stderrors.As(err, &se) {
					t.Fatalf("expected StructuredError, got %T", err)
				}
				if se.Code != errors.ErrCodeInvalidRequest {
					t.Fatalf("Code = %v, want ErrCodeInvalidRequest", se.Code)
				}
				if tt.wantContain != "" && !strings.Contains(se.Error(), tt.wantContain) {
					t.Fatalf("error %q missing substring %q", se.Error(), tt.wantContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}

// TestDisallowedOperation_AllowlistFailsClosed verifies the true-allowlist
// shape: an Operation with neither Assert nor Error set is rejected even
// when none of the currently-enumerated known-bad ops are set either. This
// is the future-proofing guarantee — a future chainsaw op upstream adds
// won't slip through this code path silently. Per PR #1235 review.
func TestDisallowedOperation_AllowlistFailsClosed(t *testing.T) {
	// All-zero Operation: nothing set. The allowlist must still reject
	// (Assert == nil && Error == nil falls through to "<unrecognized>").
	var empty v1alpha1.Operation
	name, bad := disallowedOperation(empty)
	if !bad {
		t.Fatalf("disallowedOperation should reject empty Operation; got allowed")
	}
	if name != "<unrecognized>" {
		t.Fatalf("expected name '<unrecognized>', got %q", name)
	}

	// An Operation with only Assert is allowed.
	withAssert := v1alpha1.Operation{Assert: &v1alpha1.Assert{}}
	if _, bad := disallowedOperation(withAssert); bad {
		t.Fatal("Operation with Assert set must be allowed")
	}

	// An Operation with only Error is allowed.
	withError := v1alpha1.Operation{Error: &v1alpha1.Error{}}
	if _, bad := disallowedOperation(withError); bad {
		t.Fatal("Operation with Error set must be allowed")
	}
}

// TestValidateTestReadOnly_RegistryContent walks every in-tree
// healthCheck.assertFile under recipes/checks/*/health-check.yaml and
// verifies it passes the allowlist. If this regresses, a registry-declared
// check has crept in that violates the read-only contract — block at PR
// time (PR #1223 will add the same check at lint time, with structured
// reporting that names every offender, not just the first).
func TestValidateTestReadOnly_RegistryContent(t *testing.T) {
	// Walk repo-relative recipes/checks. The test runs from the package
	// directory (validators/chainsaw), so walk up two levels to the repo
	// root. If the path changes, the t.Fatalf below makes the cause
	// obvious instead of letting the test silently pass.
	root := filepath.Join("..", "..", "recipes", "checks")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("recipes/checks not found at %s: %v", root, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read recipes/checks: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "health-check.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			// Component dir without a health-check.yaml is allowed (e.g.,
			// the 3 backfill components #1221 will add).
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", path, err)
		}
		if !IsChainsawTest(string(data)) {
			// The allowlist only applies to Chainsaw Test format.
			continue
		}
		if err := ValidateTestReadOnly(e.Name(), string(data)); err != nil {
			t.Errorf("%s violates read-only allowlist: %v", path, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no health checks were validated — registry walker is broken")
	}
}
