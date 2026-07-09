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

func TestComponentRefCoherenceProblem(t *testing.T) {
	tests := []struct {
		name    string
		ref     ComponentRef
		wantBad bool
	}{
		{
			name: "helm coherent",
			ref:  ComponentRef{Name: "a", Type: ComponentTypeHelm, Source: "https://charts", Chart: "a", Version: "v1"},
		},
		{
			name:    "helm carries kustomize tag",
			ref:     ComponentRef{Name: "a", Type: ComponentTypeHelm, Source: "https://charts", Chart: "a", Version: "v1", Tag: "v2"},
			wantBad: true,
		},
		{
			name:    "helm carries kustomize path",
			ref:     ComponentRef{Name: "a", Type: ComponentTypeHelm, Source: "https://charts", Chart: "a", Version: "v1", Path: "deploy"},
			wantBad: true,
		},
		{
			name: "kustomize coherent local path",
			ref:  ComponentRef{Name: "a", Type: ComponentTypeKustomize, Path: "deploy"},
		},
		{
			name: "kustomize coherent git tag+source+path",
			ref:  ComponentRef{Name: "a", Type: ComponentTypeKustomize, Source: "git://x", Tag: "v1", Path: "deploy"},
		},
		{
			name:    "kustomize missing path",
			ref:     ComponentRef{Name: "a", Type: ComponentTypeKustomize, Source: "git://x", Tag: "v1"},
			wantBad: true,
		},
		{
			name:    "kustomize tag without source",
			ref:     ComponentRef{Name: "a", Type: ComponentTypeKustomize, Tag: "v1", Path: "deploy"},
			wantBad: true,
		},
		{
			name:    "kustomize with post-manifests",
			ref:     ComponentRef{Name: "a", Type: ComponentTypeKustomize, Path: "deploy", ManifestFiles: []string{"extra.yaml"}},
			wantBad: true,
		},
		{
			name: "kustomize with pre-manifests is allowed",
			ref:  ComponentRef{Name: "a", Type: ComponentTypeKustomize, Path: "deploy", PreManifestFiles: []string{"ns.yaml"}},
		},
		{
			name:    "empty type",
			ref:     ComponentRef{Name: "a", Version: "v1"},
			wantBad: true,
		},
		{
			name:    "unknown type",
			ref:     ComponentRef{Name: "a", Type: ComponentType("flux")},
			wantBad: true,
		},
		{
			// Lowercase is accepted as backward-compatible input (hand-authored
			// recipes / older clients); the canonical form is Helm. It must be
			// accepted case-insensitively, not rejected as an unsupported type.
			name: "lowercase helm is accepted",
			ref:  ComponentRef{Name: "a", Type: ComponentType("helm"), Source: "https://charts", Chart: "a", Version: "v1"},
		},
		{
			name: "lowercase kustomize is accepted",
			ref:  ComponentRef{Name: "a", Type: ComponentType("kustomize"), Path: "deploy"},
		},
		{
			// Case-insensitivity does not weaken the rules: a lowercase Helm ref
			// still may not carry Kustomize fields.
			name:    "lowercase helm still rejects tag",
			ref:     ComponentRef{Name: "a", Type: ComponentType("helm"), Source: "https://charts", Chart: "a", Version: "v1", Tag: "v2"},
			wantBad: true,
		},
		{
			// Patches are carried through resolution but applied by no deployer;
			// a ref that declares them is rejected rather than silently dropped.
			name:    "helm with patches rejected",
			ref:     ComponentRef{Name: "a", Type: ComponentTypeHelm, Source: "https://charts", Chart: "a", Version: "v1", Patches: []string{"p.yaml"}},
			wantBad: true,
		},
		{
			name:    "kustomize with patches rejected",
			ref:     ComponentRef{Name: "a", Type: ComponentTypeKustomize, Path: "deploy", Patches: []string{"p.yaml"}},
			wantBad: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ref.coherenceProblem()
			if (got != "") != tt.wantBad {
				t.Fatalf("coherenceProblem() = %q, wantBad=%v", got, tt.wantBad)
			}
		})
	}
}

func TestRecipeResultValidateCoherence(t *testing.T) {
	// Coherent set → no error.
	ok := &RecipeResult{ComponentRefs: []ComponentRef{
		{Name: "h", Type: ComponentTypeHelm, Source: "https://charts", Chart: "h", Version: "v1"},
		{Name: "k", Type: ComponentTypeKustomize, Path: "deploy"},
	}}
	if err := ok.ValidateCoherence(); err != nil {
		t.Fatalf("coherent refs returned error: %v", err)
	}

	// Two incoherent refs → single ErrCodeInvalidRequest naming both.
	bad := &RecipeResult{ComponentRefs: []ComponentRef{
		{Name: "h", Type: ComponentTypeHelm, Source: "https://charts", Chart: "h", Version: "v1", Tag: "v2"},
		{Name: "k", Type: ComponentTypeKustomize, Source: "git://x", Tag: "v1"}, // no path
	}}
	err := bad.ValidateCoherence()
	if err == nil {
		t.Fatal("expected error for incoherent refs, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("want ErrCodeInvalidRequest, got %v", err)
	}
	for _, want := range []string{"\"h\"", "\"k\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name offending ref %s", err.Error(), want)
		}
	}

	// A DISABLED incoherent ref is skipped (excluded from the bundle).
	disabled := &RecipeResult{ComponentRefs: []ComponentRef{
		{Name: "h", Type: ComponentTypeHelm, Tag: "v2", Overrides: map[string]any{"enabled": false}},
	}}
	if err := disabled.ValidateCoherence(); err != nil {
		t.Errorf("disabled incoherent ref should be skipped, got: %v", err)
	}

	// nil receiver is safe.
	var nilResult *RecipeResult
	if err := nilResult.ValidateCoherence(); err != nil {
		t.Errorf("nil RecipeResult should validate clean, got: %v", err)
	}
}

// failingRegistryProvider errors on the registry.yaml read, to verify that a
// registry load failure during type back-fill is propagated (as ErrCodeInternal)
// rather than swallowed into a misleading "unsupported type" INVALID_REQUEST.
type failingRegistryProvider struct{}

func (failingRegistryProvider) ReadFile(_ context.Context, path string) ([]byte, error) {
	return nil, errors.New(errors.ErrCodeInternal, "boom: cannot read "+path)
}
func (failingRegistryProvider) WalkDir(_ context.Context, _ string, _ fs.WalkDirFunc) error {
	return errors.New(errors.ErrCodeInternal, "boom: walk")
}
func (failingRegistryProvider) Source(path string) string { return path }

func TestPrepareAndValidate_PropagatesRegistryError(t *testing.T) {
	// A type-less ref needs the registry; if the registry read fails, the
	// caller must see the underlying (retryable) internal error, not a
	// non-retryable "unsupported type" rejection.
	r := &RecipeResult{
		provider:      failingRegistryProvider{},
		ComponentRefs: []ComponentRef{{Name: "gpu-operator", Source: "https://charts", Chart: "gpu-operator", Version: "v1"}}, // type-less
	}
	err := r.PrepareAndValidate()
	if err == nil {
		t.Fatal("expected an error when the registry read fails during back-fill")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != errors.ErrCodeInternal {
		t.Errorf("expected ErrCodeInternal (registry failure), got %s: %v", se.Code, err)
	}

	// When no ref needs back-fill, the registry is never touched, so a failing
	// provider does not cause an error.
	ok := &RecipeResult{
		provider:      failingRegistryProvider{},
		ComponentRefs: []ComponentRef{{Name: "gpu-operator", Type: ComponentTypeHelm, Source: "https://charts", Chart: "gpu-operator", Version: "v1"}},
	}
	if err := ok.PrepareAndValidate(); err != nil {
		t.Errorf("no back-fill needed, but got error (registry should not be read): %v", err)
	}

	// A type-less DISABLED stub must not force a registry load: ValidateCoherence
	// skips disabled refs, so a registry failure caused solely by an irrelevant
	// disabled ref must not fail an otherwise-usable recipe.
	disabledStub := &RecipeResult{
		provider: failingRegistryProvider{},
		ComponentRefs: []ComponentRef{
			{Name: "enabled-helm", Type: ComponentTypeHelm, Source: "https://charts", Chart: "enabled-helm", Version: "v1"},
			{Name: "legacy-stub", Overrides: map[string]any{"enabled": false}}, // type-less + disabled
		},
	}
	if err := disabledStub.PrepareAndValidate(); err != nil {
		t.Errorf("disabled type-less stub should not trigger registry back-fill, got: %v", err)
	}
}

// TestPrepareAndValidate_BackfillLoopSkipsDisabled exercises the back-fill LOOP
// (not just the pre-scan): with a working registry, an enabled type-less ref is
// back-filled while a disabled type-less ref in the same result is left
// untouched and does not cause a rejection.
func TestPrepareAndValidate_BackfillLoopSkipsDisabled(t *testing.T) {
	dp := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	r := &RecipeResult{
		provider: dp,
		ComponentRefs: []ComponentRef{
			{Name: "gpu-operator", Source: "https://charts", Chart: "gpu-operator", Version: "v1"}, // enabled + type-less -> back-filled
			{Name: "network-operator", Overrides: map[string]any{"enabled": false}},                // disabled + type-less registry component -> loop must skip
		},
	}
	if err := r.PrepareAndValidate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := r.ComponentRefs[0].Type; got != ComponentTypeHelm {
		t.Errorf("enabled type-less ref not back-filled: got %q, want %q", got, ComponentTypeHelm)
	}
	if got := r.ComponentRefs[1].Type; got != "" {
		t.Errorf("disabled type-less ref should be left untouched, got type %q", got)
	}
}

// TestPrepareAndValidate_RejectsReservedDeployerName verifies the common
// validation boundary fails closed on a hand-authored componentRef named
// with the reserved deployer override key. Registry loads are guarded
// separately, but a recipe passed to `aicr bundle -r` or POST /v1/bundle
// bypasses the registry guard — and a component named "deployer" would
// make `--set deployer:*` ambiguous between component Helm values and
// deployer-level Argo options. Disabled refs are rejected too. See #1625.
func TestPrepareAndValidate_RejectsReservedDeployerName(t *testing.T) {
	tests := []struct {
		name    string
		refs    []ComponentRef
		wantErr bool
	}{
		{
			name: "enabled deployer ref rejected",
			refs: []ComponentRef{
				{Name: ReservedDeployerKey, Type: ComponentTypeHelm, Version: "v1",
					Source: "https://charts.example.com", Chart: "deployer"},
			},
			wantErr: true,
		},
		{
			name: "disabled deployer ref rejected",
			refs: []ComponentRef{
				{Name: "gpu-operator", Type: ComponentTypeHelm, Version: "v1"},
				{Name: ReservedDeployerKey, Overrides: map[string]any{"enabled": false}},
			},
			wantErr: true,
		},
		{
			name: "non-reserved names pass",
			refs: []ComponentRef{
				{Name: "gpu-operator", Type: ComponentTypeHelm, Version: "v1",
					Source: "https://charts", Chart: "gpu-operator"},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &RecipeResult{ComponentRefs: tt.refs}
			err := r.PrepareAndValidate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("PrepareAndValidate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("expected ErrCodeInvalidRequest, got: %v", err)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("error should mention the reservation, got: %v", err)
			}
		})
	}
}
