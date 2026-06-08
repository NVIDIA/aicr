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

package recipe

import (
	"context"
	stderrors "errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestHydrateHealthCheckAsserts exercises the hydration step that loads
// registry-declared healthCheck.assertFile content onto each ComponentRef
// during recipe resolution. See issue #1219.
func TestHydrateHealthCheckAsserts(t *testing.T) {
	provider := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	registry, err := GetComponentRegistryFor(provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}

	// nfd is one of the components with a registry-declared assertFile.
	if cfg := registry.Get("nfd"); cfg == nil || cfg.HealthCheck.AssertFile == "" {
		t.Fatalf("test precondition: nfd must have healthCheck.assertFile in registry")
	}

	tests := []struct {
		name        string
		refs        []ComponentRef
		wantContent func(string) bool
		wantSkip    bool // expect HealthCheckAsserts to remain empty
	}{
		{
			name: "hydrates from registry assertFile",
			refs: []ComponentRef{{Name: "nfd"}},
			wantContent: func(s string) bool {
				return strings.Contains(s, "apiVersion:") && strings.Contains(s, "chainsaw")
			},
		},
		{
			name:     "skip sentinel suppresses hydration",
			refs:     []ComponentRef{{Name: "nfd", HealthCheckSkip: true}},
			wantSkip: true,
		},
		{
			name: "inline HealthCheckAsserts is preserved (overlay wins)",
			refs: []ComponentRef{{
				Name:               "nfd",
				HealthCheckAsserts: "inline-overlay-content",
			}},
			wantContent: func(s string) bool { return s == "inline-overlay-content" },
		},
		{
			name:     "unknown component is a no-op",
			refs:     []ComponentRef{{Name: "does-not-exist-in-registry"}},
			wantSkip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := tt.refs
			if err := hydrateHealthCheckAsserts(provider, registry, refs); err != nil {
				t.Fatalf("hydrateHealthCheckAsserts: %v", err)
			}
			got := refs[0].HealthCheckAsserts
			if tt.wantSkip {
				if got != "" {
					t.Fatalf("expected empty HealthCheckAsserts, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("expected hydrated HealthCheckAsserts, got empty")
			}
			if !tt.wantContent(got) {
				t.Fatalf("HealthCheckAsserts content did not satisfy predicate; got: %q", got)
			}
		})
	}
}

// TestHydrateHealthCheckAsserts_NilProvider verifies hydration falls back
// to the package-global embedded provider when nil is passed, matching the
// nil-fallback contract documented on applyRegistryDefaults.
func TestHydrateHealthCheckAsserts_NilProvider(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	refs := []ComponentRef{{Name: "nfd"}}
	if err := hydrateHealthCheckAsserts(nil, registry, refs); err != nil {
		t.Fatalf("hydrateHealthCheckAsserts(nil provider): %v", err)
	}
	if refs[0].HealthCheckAsserts == "" {
		t.Fatalf("expected hydrated HealthCheckAsserts with nil provider fallback")
	}
}

// TestMergeComponentRef_HealthCheckSkip verifies the suppression sentinel
// propagates through overlay merge with set-if-true semantics (mirrors
// Cleanup).
func TestMergeComponentRef_HealthCheckSkip(t *testing.T) {
	tests := []struct {
		name     string
		base     ComponentRef
		overlay  ComponentRef
		wantSkip bool
	}{
		{
			name:     "overlay sets skip",
			base:     ComponentRef{Name: "x"},
			overlay:  ComponentRef{Name: "x", HealthCheckSkip: true},
			wantSkip: true,
		},
		{
			name:     "base set, overlay unset preserves",
			base:     ComponentRef{Name: "x", HealthCheckSkip: true},
			overlay:  ComponentRef{Name: "x"},
			wantSkip: true,
		},
		{
			name:     "neither set",
			base:     ComponentRef{Name: "x"},
			overlay:  ComponentRef{Name: "x"},
			wantSkip: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeComponentRef(tt.base, tt.overlay)
			if got.HealthCheckSkip != tt.wantSkip {
				t.Fatalf("HealthCheckSkip = %v, want %v", got.HealthCheckSkip, tt.wantSkip)
			}
		})
	}
}

// fakeFailingProvider is a DataProvider stub whose ReadFile always fails.
// Used to exercise the wrapped-error path in hydrateHealthCheckAsserts.
type fakeFailingProvider struct {
	delegate DataProvider // registry reads still need to work
}

func (f *fakeFailingProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// Allow registry.yaml reads through the delegate so the test can build
	// the registry; only fail when the hydrator tries to read the
	// assertFile path.
	if path == "registry.yaml" {
		return f.delegate.ReadFile(ctx, path)
	}
	return nil, fs.ErrNotExist
}

func (f *fakeFailingProvider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	return f.delegate.WalkDir(ctx, root, fn)
}

func (f *fakeFailingProvider) Source(path string) string { return f.delegate.Source(path) }

// TestHydrateHealthCheckAsserts_ReadError verifies the function wraps
// provider read errors with structured context (component name + path) so
// operators can pinpoint the offending registry entry.
func TestHydrateHealthCheckAsserts_ReadError(t *testing.T) {
	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	registry, err := GetComponentRegistryFor(embedded)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}

	provider := &fakeFailingProvider{delegate: embedded}
	refs := []ComponentRef{{Name: "nfd"}}
	err = hydrateHealthCheckAsserts(provider, registry, refs)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	// fs.ErrNotExist is not structured, so PropagateOrWrap falls back to
	// the supplied ErrCodeInternal fallback. A structured ReadFile error
	// (e.g., ErrCodeTimeout from a bounded ctx) would surface its inner
	// code instead — that path is exercised in the pkg/errors tests.
	if se.Code != errors.ErrCodeInternal {
		t.Fatalf("error code = %v, want ErrCodeInternal", se.Code)
	}
	// Component name + path are embedded in the message string so they
	// survive both the propagate and wrap branches of PropagateOrWrap
	// (the structured Context map is only populated by WrapWithContext,
	// which we deliberately don't use here).
	msg := se.Error()
	if !strings.Contains(msg, "nfd") {
		t.Fatalf("error message %q missing component name", msg)
	}
	if !strings.Contains(msg, "checks/nfd/health-check.yaml") {
		t.Fatalf("error message %q missing assertFile path", msg)
	}
}

// TestHydrateHealthCheckAsserts_ExpectedResourcesWins verifies hydration is
// skipped when the overlay also declared ExpectedResources. The deployment
// validator's current mutex
// (validators/deployment/expected_resources.go:86) only runs the chainsaw
// path when ExpectedResources is empty, so hydrating in this case would
// write inert content onto the resolved recipe. k8s-nim-operator is the
// canonical example flagged in PR #1231 review (registry assertFile +
// overlay expectedResources). PR #1220 will drop both the validator-side
// mutex and this skip simultaneously.
func TestHydrateHealthCheckAsserts_ExpectedResourcesWins(t *testing.T) {
	provider := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	registry, err := GetComponentRegistryFor(provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	refs := []ComponentRef{{
		Name: "nfd",
		ExpectedResources: []ExpectedResource{
			{Kind: "Deployment", Namespace: "node-feature-discovery", Name: "nfd-master"},
		},
	}}
	if err := hydrateHealthCheckAsserts(provider, registry, refs); err != nil {
		t.Fatalf("hydrateHealthCheckAsserts: %v", err)
	}
	if refs[0].HealthCheckAsserts != "" {
		t.Fatalf("HealthCheckAsserts should remain empty when ExpectedResources is set, got %q",
			refs[0].HealthCheckAsserts)
	}
}

// TestHydrateHealthCheckAsserts_NoAssertFile verifies the no-op branch when
// a component is in the registry but has no healthCheck.assertFile declared.
// Uses a synthetic ComponentRegistry to isolate the branch from in-tree
// registry contents (every embedded component currently has an assertFile,
// so this branch is otherwise unreachable from the embedded fixture).
func TestHydrateHealthCheckAsserts_NoAssertFile(t *testing.T) {
	registry := &ComponentRegistry{
		Components: []ComponentConfig{{Name: "no-check-component"}},
		byName: map[string]*ComponentConfig{
			"no-check-component": {Name: "no-check-component"},
		},
	}
	provider := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	refs := []ComponentRef{{Name: "no-check-component"}}
	if err := hydrateHealthCheckAsserts(provider, registry, refs); err != nil {
		t.Fatalf("hydrateHealthCheckAsserts: %v", err)
	}
	if refs[0].HealthCheckAsserts != "" {
		t.Fatalf("HealthCheckAsserts should remain empty when registry has no assertFile, got %q",
			refs[0].HealthCheckAsserts)
	}
}

// TestMixinComponentRefSafeForMerge_RejectsHealthCheckSkip ensures a mixin
// cannot silently suppress hydration of an inherited check. The merge
// resolver must reject the offending field by name.
func TestMixinComponentRefSafeForMerge_RejectsHealthCheckSkip(t *testing.T) {
	ref := ComponentRef{Name: "x", HealthCheckSkip: true}
	field, ok := mixinComponentRefSafeForMerge(ref)
	if ok {
		t.Fatalf("expected mixin with HealthCheckSkip to be rejected")
	}
	if field != "healthCheckSkip" {
		t.Fatalf("expected offending field name 'healthCheckSkip', got %q", field)
	}
}
