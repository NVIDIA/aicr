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
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/kyverno/chainsaw/pkg/apis/v1alpha1"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
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

// TestValidateTestReadOnly_RegistryContent walks every registry-declared
// healthCheck.assertFile (via the embedded recipe data provider) and
// verifies it passes the read-only allowlist. PR #1223 changed this from
// a filesystem walk to a registry walk so a registry entry pointing at a
// non-conventional path (anything other than
// recipes/checks/<name>/health-check.yaml) is still validated — the
// runtime hydration path resolves through the same registry +
// DataProvider, so any path the runtime would honor must also be
// allowlist-compliant.
//
// Paired with pkg/recipe.TestComponentRegistry_RequiresHealthCheck which
// enforces that every component HAS an assertFile, this gives the full
// PR-time contract: every registry entry has a readable, allowlist-
// compliant chainsaw check.
//
// CROSS-TEST DEPENDENCY: empty-assertFile and unreadable-path failure
// modes are deliberately skipped here and owned by
// TestComponentRegistry_RequiresHealthCheck in pkg/recipe — that test
// fails first with a clearer message, so we avoid double-reporting on
// the same root cause. If that test is ever removed or weakened,
// strengthen this one to fail on empty/unreadable entries rather than
// silently skipping them; otherwise a broken registry could green-
// light here as long as ≥1 valid entry keeps `checked > 0`. Post-merge
// review on PR #1244.
func TestValidateTestReadOnly_RegistryContent(t *testing.T) {
	// Empty prefix matches the canonical defaultEmbeddedProvider used at
	// runtime (pkg/recipe/components.go); "." also works because
	// filepath.Join cleans it, but matches the runtime path exactly
	// without the head-scratch.
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}
	checked := 0
	for _, comp := range registry.Components {
		assertFile := comp.HealthCheck.AssertFile
		if assertFile == "" {
			// Lint guard in pkg/recipe (TestComponentRegistry_RequiresHealthCheck)
			// fails first with a clearer message; skip here so this test
			// doesn't double-report on the same root cause.
			continue
		}
		data, err := provider.ReadFile(context.Background(), assertFile)
		if err != nil {
			// Path-readability is also covered by
			// TestComponentRegistry_RequiresHealthCheck. Skip here for the
			// same reason — single source of truth on that failure mode.
			continue
		}
		if !IsChainsawTest(string(data)) {
			// The allowlist only applies to Chainsaw Test format. Raw K8s
			// YAML asserts are evaluated by the chainsaw Go library and
			// have no operations to gate.
			continue
		}
		if err := ValidateTestReadOnly(comp.Name, string(data)); err != nil {
			t.Errorf("component %q assertFile %q violates read-only allowlist: %v",
				comp.Name, assertFile, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no health checks were validated — registry walker is broken")
	}
}
