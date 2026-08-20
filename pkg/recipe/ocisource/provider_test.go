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

package ocisource

import (
	"context"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

type fakeStagedArtifact struct {
	mu sync.Mutex

	authorize   func(context.Context) error
	materialize func(context.Context) (string, error)
	close       func() error
	closeCalls  int
}

func (a *fakeStagedArtifact) AuthorizeDigestMaterialization(ctx context.Context) error {
	if a.authorize != nil {
		return a.authorize(ctx)
	}
	return nil
}

func (a *fakeStagedArtifact) Materialize(ctx context.Context) (string, error) {
	if a.materialize != nil {
		return a.materialize(ctx)
	}
	return "materialized", nil
}

func (a *fakeStagedArtifact) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeCalls++
	if a.close != nil {
		return a.close()
	}
	return nil
}

func (a *fakeStagedArtifact) CloseCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closeCalls
}

type fakeDataProvider struct {
	read   func(context.Context, string) ([]byte, error)
	walk   func(context.Context, string, fs.WalkDirFunc) error
	source func(string) string
}

func (p *fakeDataProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if p.read != nil {
		return p.read(ctx, path)
	}
	return []byte(path), nil
}

func (p *fakeDataProvider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	if p.walk != nil {
		return p.walk(ctx, root, fn)
	}
	return nil
}

func (p *fakeDataProvider) Source(path string) string {
	if p.source != nil {
		return p.source(path)
	}
	return "fake:" + path
}

func testEmbeddedProvider() *recipe.EmbeddedDataProvider {
	return recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ".")
}

func successfulDependencies(
	artifact stagedRecipeArtifact,
	delegate recipe.DataProvider,
) dependencies {

	return dependencies{
		stage: func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error) {
			return artifact, nil
		},
		newLayered: func(
			*recipe.EmbeddedDataProvider,
			recipe.LayeredProviderConfig,
		) (recipe.DataProvider, error) {

			return delegate, nil
		},
		validateCatalog: func(context.Context, recipe.DataProvider) error { return nil },
		timeout:         time.Minute,
	}
}

func assertErrorCode(t *testing.T, err error, code apperrors.ErrorCode) {
	t.Helper()
	if !stderrors.Is(err, apperrors.New(code, "")) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestDefaultDependenciesUseConstructionTimeout(t *testing.T) {
	if got := defaultDependencies().timeout; got != defaults.OCIRecipeConstructionTimeout {
		t.Fatalf("default construction timeout = %v, want %v",
			got, defaults.OCIRecipeConstructionTimeout)
	}
}

func TestNewRejectsInvalidInputsAndDependencies(t *testing.T) {
	embedded := testEmbeddedProvider()
	artifact := &fakeStagedArtifact{}
	delegate := &fakeDataProvider{}
	base := successfulDependencies(artifact, delegate)

	tests := []struct {
		name     string
		ctx      context.Context
		embedded *recipe.EmbeddedDataProvider
		mutate   func(*dependencies)
		code     apperrors.ErrorCode
	}{
		{
			name:     "nil context",
			embedded: embedded,
			code:     apperrors.ErrCodeInvalidRequest,
		},
		{
			name: "nil embedded provider",
			ctx:  t.Context(),
			code: apperrors.ErrCodeInvalidRequest,
		},
		{
			name:     "missing stage dependency",
			ctx:      t.Context(),
			embedded: embedded,
			mutate:   func(deps *dependencies) { deps.stage = nil },
			code:     apperrors.ErrCodeInternal,
		},
		{
			name:     "missing layered dependency",
			ctx:      t.Context(),
			embedded: embedded,
			mutate:   func(deps *dependencies) { deps.newLayered = nil },
			code:     apperrors.ErrCodeInternal,
		},
		{
			name:     "missing validation dependency",
			ctx:      t.Context(),
			embedded: embedded,
			mutate:   func(deps *dependencies) { deps.validateCatalog = nil },
			code:     apperrors.ErrCodeInternal,
		},
		{
			name:     "zero timeout",
			ctx:      t.Context(),
			embedded: embedded,
			mutate:   func(deps *dependencies) { deps.timeout = 0 },
			code:     apperrors.ErrCodeInternal,
		},
		{
			name:     "negative timeout",
			ctx:      t.Context(),
			embedded: embedded,
			mutate:   func(deps *dependencies) { deps.timeout = -time.Second },
			code:     apperrors.ErrCodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := base
			if tt.mutate != nil {
				tt.mutate(&deps)
			}
			provider, err := newWithDependencies(
				tt.ctx, tt.embedded, Config{}, deps)
			if provider != nil {
				t.Fatal("provider returned for invalid construction")
			}
			assertErrorCode(t, err, tt.code)
		})
	}

	// Exercise the public boundary without performing registry I/O.
	//nolint:staticcheck // A nil context is the invalid public input under test.
	if provider, err := New(nil, embedded, Config{}); provider != nil || err == nil {
		t.Fatalf("New(nil, ...) = (%v, %v), want nil,error", provider, err)
	}
}

func TestNewUsesOneContextAndExactPhaseOrder(t *testing.T) {
	var (
		calls         []string
		firstCtx      context.Context
		firstDeadline time.Time
	)
	recordContext := func(label string, ctx context.Context) {
		t.Helper()
		calls = append(calls, label)
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("%s context has no deadline", label)
		}
		if firstCtx == nil {
			firstCtx = ctx
			firstDeadline = deadline
			return
		}
		if ctx != firstCtx {
			t.Fatalf("%s received a different operation context", label)
		}
		if !deadline.Equal(firstDeadline) {
			t.Fatalf("%s deadline = %v, want %v", label, deadline, firstDeadline)
		}
	}

	artifact := &fakeStagedArtifact{
		authorize: func(ctx context.Context) error {
			recordContext("authorize", ctx)
			return nil
		},
		materialize: func(ctx context.Context) (string, error) {
			recordContext("materialize", ctx)
			return "/owned/materialized", nil
		},
	}
	delegate := &fakeDataProvider{}
	pull := oci.RecipePullOptions{
		Repository: "registry.example/aicr/recipes",
		Selector:   "v1.0.0",
		TempDir:    "/owned/parent",
	}
	deps := dependencies{
		stage: func(ctx context.Context, got oci.RecipePullOptions) (stagedRecipeArtifact, error) {
			recordContext("stage", ctx)
			if !reflect.DeepEqual(got, pull) {
				t.Fatalf("pull options = %#v, want %#v", got, pull)
			}
			return artifact, nil
		},
		newLayered: func(
			embedded *recipe.EmbeddedDataProvider,
			got recipe.LayeredProviderConfig,
		) (recipe.DataProvider, error) {

			calls = append(calls, "layered")
			if embedded == nil {
				t.Fatal("layered constructor received nil embedded provider")
			}
			want := recipe.LayeredProviderConfig{
				ExternalDir:   "/owned/materialized",
				MaxFileSize:   defaults.MaxOCIRecipeFileBytes,
				AllowSymlinks: false,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("layered config = %#v, want %#v", got, want)
			}
			return delegate, nil
		},
		validateCatalog: func(ctx context.Context, got recipe.DataProvider) error {
			recordContext("validate", ctx)
			if _, ok := got.(*Provider); !ok {
				t.Fatalf("validation provider = %T, want *Provider", got)
			}
			return nil
		},
		timeout: 30 * time.Second,
	}

	callerCtx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	provider, err := newWithDependencies(callerCtx, testEmbeddedProvider(), Config{
		PullOptions: pull,
	}, deps)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	if want := []string{"stage", "authorize", "materialize", "layered", "validate"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	remaining := time.Until(firstDeadline)
	if remaining <= 0 || remaining > deps.timeout {
		t.Fatalf("operation deadline remaining = %v, want within (0,%v]", remaining, deps.timeout)
	}
}

func TestNewHonorsTighterCallerDeadline(t *testing.T) {
	artifact := &fakeStagedArtifact{}
	delegate := &fakeDataProvider{}
	deps := successfulDependencies(artifact, delegate)
	deps.timeout = time.Hour
	var gotDeadline time.Time
	deps.stage = func(ctx context.Context, _ oci.RecipePullOptions) (stagedRecipeArtifact, error) {
		gotDeadline, _ = ctx.Deadline()
		return artifact, nil
	}

	callerCtx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	wantDeadline, _ := callerCtx.Deadline()
	provider, err := newWithDependencies(callerCtx, testEmbeddedProvider(), Config{}, deps)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	if !gotDeadline.Equal(wantDeadline) {
		t.Fatalf("operation deadline = %v, want caller deadline %v", gotDeadline, wantDeadline)
	}
}

func TestNewChecksExpiredContextBetweenPhases(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	artifact := &fakeStagedArtifact{}
	delegate := &fakeDataProvider{}
	deps := successfulDependencies(artifact, delegate)
	deps.stage = func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error) {
		cancel()
		return artifact, nil
	}

	provider, err := newWithDependencies(ctx, testEmbeddedProvider(), Config{}, deps)
	if provider != nil {
		t.Fatal("provider returned after operation cancellation")
	}
	assertErrorCode(t, err, apperrors.ErrCodeCanceled)
	if artifact.CloseCalls() != 1 {
		t.Fatalf("artifact Close calls = %d, want 1", artifact.CloseCalls())
	}
}

func TestNewValidationFailureUsesContextErrorWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	validationErr := apperrors.Wrap(apperrors.ErrCodeTimeout,
		"layered validation timed out", context.Canceled)
	cleanupErr := stderrors.New("cleanup failure")
	artifact := &fakeStagedArtifact{close: func() error { return cleanupErr }}
	deps := successfulDependencies(artifact, &fakeDataProvider{})
	deps.validateCatalog = func(context.Context, recipe.DataProvider) error {
		cancel()
		return validationErr
	}

	provider, err := newWithDependencies(ctx, testEmbeddedProvider(), Config{}, deps)
	if provider != nil {
		t.Fatal("provider returned after catalog validation cancellation")
	}
	var primary *apperrors.StructuredError
	if !stderrors.As(err, &primary) {
		t.Fatalf("error = %v, want structured cancellation error", err)
	}
	if primary.Code != apperrors.ErrCodeCanceled {
		t.Fatalf("primary error code = %s, want %s", primary.Code, apperrors.ErrCodeCanceled)
	}
	if apperrors.IsTransient(err) {
		t.Fatalf("error = %v, want non-retryable operator cancellation", err)
	}
	if stderrors.Is(err, validationErr) {
		t.Fatalf("error = %v, want context cancellation to replace validation timeout", err)
	}
	if !stderrors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want cleanup error %v", err, cleanupErr)
	}
	if artifact.CloseCalls() != 1 {
		t.Fatalf("artifact Close calls = %d, want 1", artifact.CloseCalls())
	}
}

func TestNewValidationFailurePreservesErrorWhileContextActive(t *testing.T) {
	validationErr := apperrors.New(apperrors.ErrCodeUnauthorized, "catalog access denied")
	cleanupErr := stderrors.New("cleanup failure")
	artifact := &fakeStagedArtifact{close: func() error { return cleanupErr }}
	deps := successfulDependencies(artifact, &fakeDataProvider{})
	deps.validateCatalog = func(context.Context, recipe.DataProvider) error {
		return validationErr
	}

	provider, err := newWithDependencies(t.Context(), testEmbeddedProvider(), Config{}, deps)
	if provider != nil {
		t.Fatal("provider returned after catalog validation failure")
	}
	var primary *apperrors.StructuredError
	if !stderrors.As(err, &primary) {
		t.Fatalf("error = %v, want structured validation error", err)
	}
	if primary != validationErr {
		t.Fatalf("primary error = %v, want unchanged validation error %v", primary, validationErr)
	}
	if !stderrors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want cleanup error %v", err, cleanupErr)
	}
	if artifact.CloseCalls() != 1 {
		t.Fatalf("artifact Close calls = %d, want 1", artifact.CloseCalls())
	}
}

func TestNewFailureCleanupAndCodes(t *testing.T) {
	primary := apperrors.New(apperrors.ErrCodeUnauthorized, "primary failure")
	uncodedPrimary := stderrors.New("invalid catalog shape")
	cleanup := stderrors.New("cleanup failure")
	delegate := &fakeDataProvider{}

	tests := []struct {
		name        string
		configure   func(*dependencies, *fakeStagedArtifact)
		wantCode    apperrors.ErrorCode
		wantClose   int
		wantPrimary error
	}{
		{
			name: "stage failure with owned artifact",
			configure: func(deps *dependencies, artifact *fakeStagedArtifact) {
				deps.stage = func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error) {
					return artifact, primary
				}
			},
			wantCode: apperrors.ErrCodeUnauthorized, wantClose: 1, wantPrimary: primary,
		},
		{
			name: "stage failure without artifact",
			configure: func(deps *dependencies, _ *fakeStagedArtifact) {
				deps.stage = func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error) {
					return nil, primary
				}
			},
			wantCode: apperrors.ErrCodeUnauthorized, wantClose: 0, wantPrimary: primary,
		},
		{
			name: "stage returns nil artifact",
			configure: func(deps *dependencies, _ *fakeStagedArtifact) {
				deps.stage = func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error) {
					//nolint:nilnil // An invalid injected dependency result is the case under test.
					return nil, nil
				}
			},
			wantCode: apperrors.ErrCodeInternal,
		},
		{
			name: "authorization failure",
			configure: func(_ *dependencies, artifact *fakeStagedArtifact) {
				artifact.authorize = func(context.Context) error {
					return primary
				}
			},
			wantCode: apperrors.ErrCodeUnauthorized, wantClose: 1, wantPrimary: primary,
		},
		{
			name: "materialization failure",
			configure: func(_ *dependencies, artifact *fakeStagedArtifact) {
				artifact.materialize = func(context.Context) (string, error) { return "", primary }
			},
			wantCode: apperrors.ErrCodeUnauthorized, wantClose: 1, wantPrimary: primary,
		},
		{
			name: "empty materialization",
			configure: func(_ *dependencies, artifact *fakeStagedArtifact) {
				artifact.materialize = func(context.Context) (string, error) { return "", nil }
			},
			wantCode: apperrors.ErrCodeInternal, wantClose: 1,
		},
		{
			name: "layered construction failure",
			configure: func(deps *dependencies, _ *fakeStagedArtifact) {
				deps.newLayered = func(
					*recipe.EmbeddedDataProvider,
					recipe.LayeredProviderConfig,
				) (recipe.DataProvider, error) {

					return nil, primary
				}
			},
			wantCode: apperrors.ErrCodeUnauthorized, wantClose: 1, wantPrimary: primary,
		},
		{
			name: "layered returns nil provider",
			configure: func(deps *dependencies, _ *fakeStagedArtifact) {
				deps.newLayered = func(
					*recipe.EmbeddedDataProvider,
					recipe.LayeredProviderConfig,
				) (recipe.DataProvider, error) {

					//nolint:nilnil // An invalid injected dependency result is the case under test.
					return nil, nil
				}
			},
			wantCode: apperrors.ErrCodeInternal, wantClose: 1,
		},
		{
			name: "uncoded catalog validation failure",
			configure: func(deps *dependencies, _ *fakeStagedArtifact) {
				deps.validateCatalog = func(context.Context, recipe.DataProvider) error {
					return uncodedPrimary
				}
			},
			wantCode: apperrors.ErrCodeInvalidRequest, wantClose: 1, wantPrimary: uncodedPrimary,
		},
		{
			name: "coded catalog validation failure",
			configure: func(deps *dependencies, _ *fakeStagedArtifact) {
				deps.validateCatalog = func(context.Context, recipe.DataProvider) error { return primary }
			},
			wantCode: apperrors.ErrCodeUnauthorized, wantClose: 1, wantPrimary: primary,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifact := &fakeStagedArtifact{close: func() error { return cleanup }}
			deps := successfulDependencies(artifact, delegate)
			tt.configure(&deps, artifact)
			provider, err := newWithDependencies(t.Context(), testEmbeddedProvider(), Config{}, deps)
			if provider != nil {
				t.Fatal("provider returned for failed construction")
			}
			assertErrorCode(t, err, tt.wantCode)
			if artifact.CloseCalls() != tt.wantClose {
				t.Fatalf("artifact Close calls = %d, want %d", artifact.CloseCalls(), tt.wantClose)
			}
			if tt.wantPrimary != nil && !stderrors.Is(err, tt.wantPrimary) {
				t.Fatalf("error = %v, want primary %v", err, tt.wantPrimary)
			}
			if tt.wantClose != 0 && !stderrors.Is(err, cleanup) {
				t.Fatalf("error = %v, want cleanup %v", err, cleanup)
			}
		})
	}
}

func TestNewValidationFailureEvictsWrapperCaches(t *testing.T) {
	artifact := &fakeStagedArtifact{}
	embedded := testEmbeddedProvider()
	primary := apperrors.New(apperrors.ErrCodeInvalidRequest, "reject catalog")
	deps := successfulDependencies(artifact, embedded)
	deps.validateCatalog = func(ctx context.Context, provider recipe.DataProvider) error {
		if _, err := recipe.LoadMetadataStoreFor(ctx, provider); err != nil {
			t.Fatalf("LoadMetadataStoreFor() error = %v", err)
		}
		if _, err := recipe.GetComponentRegistryFor(provider); err != nil {
			t.Fatalf("GetComponentRegistryFor() error = %v", err)
		}
		_ = recipe.GetCriteriaRegistryFor(provider)
		if !recipe.CachedStoreContainsForTesting(provider) ||
			!recipe.CachedRegistryContainsForTesting(provider) ||
			!recipe.CachedCriteriaRegistryContainsForTesting(provider) {

			t.Fatal("wrapper caches were not populated during validation")
		}
		return primary
	}

	provider, err := newWithDependencies(t.Context(), embedded, Config{}, deps)
	if provider != nil {
		t.Fatal("provider returned for failed validation")
	}
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
	if artifact.CloseCalls() != 1 {
		t.Fatalf("artifact Close calls = %d, want 1", artifact.CloseCalls())
	}

	// The failed wrapper is intentionally not returned, so validate through a
	// captured key rather than a result value.
	var captured recipe.DataProvider
	deps.validateCatalog = func(_ context.Context, provider recipe.DataProvider) error {
		captured = provider
		_ = recipe.GetCriteriaRegistryFor(provider)
		return primary
	}
	artifact = &fakeStagedArtifact{}
	deps.stage = func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error) {
		return artifact, nil
	}
	_, _ = newWithDependencies(t.Context(), embedded, Config{}, deps)
	if recipe.CachedStoreContainsForTesting(captured) ||
		recipe.CachedRegistryContainsForTesting(captured) ||
		recipe.CachedCriteriaRegistryContainsForTesting(captured) {

		t.Fatal("failed constructor left wrapper cache entries behind")
	}
}

func TestProviderRealLayeringValidationAndParentLifetime(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "owned-child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("Mkdir(child) error = %v", err)
	}
	sentinel := filepath.Join(parent, "caller-owned.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile(sentinel) error = %v", err)
	}

	embedded := testEmbeddedProvider()
	base, readErr := embedded.ReadFile(t.Context(), "overlays/base.yaml")
	if readErr != nil {
		t.Fatalf("read embedded base error = %v", readErr)
	}
	if mkdirErr := os.MkdirAll(filepath.Join(child, "overlays"), 0o700); mkdirErr != nil {
		t.Fatalf("MkdirAll(overlays) error = %v", mkdirErr)
	}
	if writeErr := os.WriteFile(filepath.Join(child, "overlays/base.yaml"),
		append([]byte("# OCI override\n"), base...), 0o600); writeErr != nil {
		t.Fatalf("WriteFile(base) error = %v", writeErr)
	}
	registry := `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components:
  - name: custom-oci-component
    displayName: Custom OCI Component
    helm:
      defaultRepository: https://example.invalid/charts
      defaultChart: example/custom
      defaultVersion: v1.0.0
`
	if writeErr := os.WriteFile(
		filepath.Join(child, recipe.RegistryFileName), []byte(registry), 0o600); writeErr != nil {
		t.Fatalf("WriteFile(registry) error = %v", writeErr)
	}

	artifact := &fakeStagedArtifact{
		materialize: func(context.Context) (string, error) { return child, nil },
		close:       func() error { return os.RemoveAll(child) },
	}
	deps := defaultDependencies()
	deps.stage = func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error) {
		return artifact, nil
	}
	provider, err := newWithDependencies(t.Context(), embedded, Config{}, deps)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}

	overridden, err := provider.ReadFile(t.Context(), "overlays/base.yaml")
	if err != nil {
		t.Fatalf("ReadFile(overridden base) error = %v", err)
	}
	if !strings.HasPrefix(string(overridden), "# OCI override") {
		t.Fatal("external OCI file did not take precedence")
	}
	if got := provider.Source("overlays/base.yaml"); got != recipe.CatalogSourceExternal {
		t.Fatalf("Source(overridden base) = %q, want %q", got, recipe.CatalogSourceExternal)
	}
	merged, err := provider.ReadFile(t.Context(), recipe.RegistryFileName)
	if err != nil {
		t.Fatalf("ReadFile(merged registry) error = %v", err)
	}
	if !strings.Contains(string(merged), "custom-oci-component") ||
		!strings.Contains(string(merged), "gpu-operator") {

		t.Fatal("merged registry does not contain embedded and OCI components")
	}
	if got := provider.Source(recipe.RegistryFileName); !strings.Contains(got, "merged") {
		t.Fatalf("Source(registry) = %q, want merged provenance", got)
	}
	if _, err := provider.ReadFile(t.Context(), "overlays/h100-eks-ubuntu-training.yaml"); err != nil {
		t.Fatalf("embedded fallback ReadFile() error = %v", err)
	}
	if _, err := recipe.GetComponentRegistryFor(provider); err != nil {
		t.Fatalf("GetComponentRegistryFor() error = %v", err)
	}
	_ = recipe.GetCriteriaRegistryFor(provider)
	if !recipe.CachedStoreContainsForTesting(provider) ||
		!recipe.CachedRegistryContainsForTesting(provider) ||
		!recipe.CachedCriteriaRegistryContainsForTesting(provider) {

		t.Fatal("expected provider caches to be populated")
	}

	if err := provider.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(child); !stderrors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned child still exists after Close: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("caller parent sentinel = (%q, %v), want keep,nil", data, err)
	}
	if recipe.CachedStoreContainsForTesting(provider) ||
		recipe.CachedRegistryContainsForTesting(provider) ||
		recipe.CachedCriteriaRegistryContainsForTesting(provider) {

		t.Fatal("Close left provider cache entries behind")
	}
}

func TestNewRejectsMaterializationWithoutRegistryAndPreservesParent(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "owned-child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("Mkdir(child) error = %v", err)
	}
	artifact := &fakeStagedArtifact{
		materialize: func(context.Context) (string, error) { return child, nil },
		close:       func() error { return os.RemoveAll(child) },
	}
	deps := defaultDependencies()
	deps.stage = func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error) {
		return artifact, nil
	}

	provider, err := newWithDependencies(t.Context(), testEmbeddedProvider(), Config{}, deps)
	if provider != nil {
		t.Fatal("provider returned for materialization without registry.yaml")
	}
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
	if artifact.CloseCalls() != 1 {
		t.Fatalf("artifact Close calls = %d, want 1", artifact.CloseCalls())
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("caller parent removed: %v", err)
	}
}

func TestNewRejectsMalformedMaterializedCatalog(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "owned-child")
	if err := os.MkdirAll(filepath.Join(child, "overlays"), 0o700); err != nil {
		t.Fatalf("MkdirAll(overlays) error = %v", err)
	}
	registry := `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components: []
`
	if err := os.WriteFile(
		filepath.Join(child, recipe.RegistryFileName), []byte(registry), 0o600); err != nil {
		t.Fatalf("WriteFile(registry) error = %v", err)
	}
	malformed := `apiVersion: aicr.run/v1alpha3
kind: RecipeMetadata
metadata:
  name: malformed
spec:
  unexpectedField: rejected
`
	if err := os.WriteFile(
		filepath.Join(child, "overlays/malformed.yaml"), []byte(malformed), 0o600); err != nil {
		t.Fatalf("WriteFile(malformed overlay) error = %v", err)
	}

	artifact := &fakeStagedArtifact{
		materialize: func(context.Context) (string, error) { return child, nil },
		close:       func() error { return os.RemoveAll(child) },
	}
	deps := defaultDependencies()
	deps.stage = func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error) {
		return artifact, nil
	}
	provider, err := newWithDependencies(t.Context(), testEmbeddedProvider(), Config{}, deps)
	if provider != nil {
		t.Fatal("provider returned for malformed catalog")
	}
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
	if artifact.CloseCalls() != 1 {
		t.Fatalf("artifact Close calls = %d, want 1", artifact.CloseCalls())
	}
	if _, err := os.Stat(child); !stderrors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned child still exists after failed validation: %v", err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("caller parent removed: %v", err)
	}
}

func TestProviderDelegatesArguments(t *testing.T) {
	wantCtx := t.Context()
	walkCalled := false
	delegate := &fakeDataProvider{
		read: func(ctx context.Context, path string) ([]byte, error) {
			if ctx != wantCtx || path != "read-path" {
				t.Fatalf("ReadFile arguments = (%v,%q), want original context,read-path", ctx, path)
			}
			return []byte("delegated"), nil
		},
		walk: func(ctx context.Context, root string, fn fs.WalkDirFunc) error {
			if ctx != wantCtx || root != "walk-root" {
				t.Fatalf("WalkDir arguments = (%v,%q), want original context,walk-root", ctx, root)
			}
			walkCalled = true
			return fn("entry", nil, nil)
		},
		source: func(path string) string {
			if path != "source-path" {
				t.Fatalf("Source path = %q, want source-path", path)
			}
			return "delegated-source"
		},
	}
	artifact := &fakeStagedArtifact{}
	provider, err := newWithDependencies(
		t.Context(), testEmbeddedProvider(), Config{},
		successfulDependencies(artifact, delegate))
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })

	data, err := provider.ReadFile(wantCtx, "read-path")
	if err != nil || string(data) != "delegated" {
		t.Fatalf("ReadFile() = (%q,%v), want delegated,nil", data, err)
	}
	callbackCalled := false
	err = provider.WalkDir(wantCtx, "walk-root", func(path string, _ fs.DirEntry, err error) error {
		callbackCalled = true
		if path != "entry" || err != nil {
			t.Fatalf("walk callback = (%q,%v), want entry,nil", path, err)
		}
		return nil
	})
	if err != nil || !walkCalled || !callbackCalled {
		t.Fatalf("WalkDir() = %v, delegate=%t callback=%t", err, walkCalled, callbackCalled)
	}
	if got := provider.Source("source-path"); got != "delegated-source" {
		t.Fatalf("Source() = %q, want delegated-source", got)
	}
}

func TestProvidersAreComparableAndIsolated(t *testing.T) {
	newProvider := func(value string) (*Provider, *fakeStagedArtifact) {
		t.Helper()
		artifact := &fakeStagedArtifact{}
		delegate := &fakeDataProvider{
			read: func(context.Context, string) ([]byte, error) { return []byte(value), nil },
		}
		provider, err := newWithDependencies(
			t.Context(), testEmbeddedProvider(), Config{},
			successfulDependencies(artifact, delegate))
		if err != nil {
			t.Fatalf("newWithDependencies(%q) error = %v", value, err)
		}
		return provider, artifact
	}

	first, firstArtifact := newProvider("first")
	second, secondArtifact := newProvider("second")
	providers := map[recipe.DataProvider]string{first: "first", second: "second"}
	if len(providers) != 2 {
		t.Fatalf("comparable provider map length = %d, want 2", len(providers))
	}
	firstData, _ := first.ReadFile(t.Context(), "same")
	secondData, _ := second.ReadFile(t.Context(), "same")
	if string(firstData) != "first" || string(secondData) != "second" {
		t.Fatalf("isolated reads = (%q,%q), want (first,second)", firstData, secondData)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first.Close() error = %v", err)
	}
	if firstArtifact.CloseCalls() != 1 || secondArtifact.CloseCalls() != 0 {
		t.Fatalf("artifact Close calls = (%d,%d), want (1,0)",
			firstArtifact.CloseCalls(), secondArtifact.CloseCalls())
	}
	if data, err := second.ReadFile(t.Context(), "same"); err != nil || string(data) != "second" {
		t.Fatalf("second read after first close = (%q,%v), want second,nil", data, err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second.Close() error = %v", err)
	}
}

func TestProviderCloseDrainsReadAndWalk(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *Provider) error
	}{
		{
			name: "read",
			run: func(ctx context.Context, provider *Provider) error {
				_, err := provider.ReadFile(ctx, "file")
				return err
			},
		},
		{
			name: "walk",
			run: func(ctx context.Context, provider *Provider) error {
				return provider.WalkDir(ctx, "", func(string, fs.DirEntry, error) error { return nil })
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var startOnce sync.Once
			block := func(ctx context.Context) error {
				startOnce.Do(func() { close(started) })
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			delegate := &fakeDataProvider{
				read: func(ctx context.Context, _ string) ([]byte, error) {
					return nil, block(ctx)
				},
				walk: func(ctx context.Context, _ string, _ fs.WalkDirFunc) error {
					return block(ctx)
				},
			}
			artifact := &fakeStagedArtifact{}
			provider, err := newWithDependencies(
				t.Context(), testEmbeddedProvider(), Config{},
				successfulDependencies(artifact, delegate))
			if err != nil {
				t.Fatalf("newWithDependencies() error = %v", err)
			}

			ioDone := make(chan error, 1)
			go func() { ioDone <- tt.run(t.Context(), provider) }()
			<-started
			closeDone := make(chan error, 1)
			go func() { closeDone <- provider.Close() }()
			select {
			case err := <-closeDone:
				t.Fatalf("Close returned during in-flight %s: %v", tt.name, err)
			case <-time.After(25 * time.Millisecond):
			}
			if artifact.CloseCalls() != 0 {
				t.Fatal("artifact closed before in-flight I/O drained")
			}
			close(release)
			if err := <-ioDone; err != nil {
				t.Fatalf("in-flight %s error = %v", tt.name, err)
			}
			if err := <-closeDone; err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if artifact.CloseCalls() != 1 {
				t.Fatalf("artifact Close calls = %d, want 1", artifact.CloseCalls())
			}
		})
	}
}

func TestProviderCloseIsCheckedIdempotentAndPostCloseFails(t *testing.T) {
	closeErr := stderrors.New("owned cleanup failed")
	artifact := &fakeStagedArtifact{close: func() error { return closeErr }}
	delegate := &fakeDataProvider{source: func(string) string { return "open" }}
	provider, err := newWithDependencies(
		t.Context(), testEmbeddedProvider(), Config{},
		successfulDependencies(artifact, delegate))
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	if got := provider.Source("path"); got != "open" {
		t.Fatalf("Source() before Close = %q, want open", got)
	}

	first := provider.Close()
	assertErrorCode(t, first, apperrors.ErrCodeInternal)
	if !stderrors.Is(first, closeErr) {
		t.Fatalf("Close() error = %v, want %v", first, closeErr)
	}
	second := provider.Close()
	if second == nil || second.Error() != first.Error() || !stderrors.Is(second, closeErr) {
		t.Fatalf("second Close result = %v, want cached equivalent %v", second, first)
	}
	if artifact.CloseCalls() != 1 {
		t.Fatalf("artifact Close calls = %d, want 1", artifact.CloseCalls())
	}
	if _, err := provider.ReadFile(t.Context(), "path"); err == nil {
		t.Fatal("ReadFile after Close unexpectedly succeeded")
	} else {
		assertErrorCode(t, err, apperrors.ErrCodeUnavailable)
	}
	if err := provider.WalkDir(t.Context(), "", func(string, fs.DirEntry, error) error { return nil }); err == nil {
		t.Fatal("WalkDir after Close unexpectedly succeeded")
	} else {
		assertErrorCode(t, err, apperrors.ErrCodeUnavailable)
	}
	if got := provider.Source("path"); got != closedSource {
		t.Fatalf("Source after Close = %q, want %q", got, closedSource)
	}

	var nilProvider *Provider
	if err := nilProvider.Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}
	if got := nilProvider.Source("path"); got != closedSource {
		t.Fatalf("nil Source() = %q, want %q", got, closedSource)
	}
	if _, err := nilProvider.ReadFile(t.Context(), "path"); err == nil {
		t.Fatal("nil ReadFile unexpectedly succeeded")
	} else {
		assertErrorCode(t, err, apperrors.ErrCodeUnavailable)
	}
}

func TestProviderConcurrentCloseIsIdempotent(t *testing.T) {
	closeErr := stderrors.New("checked close failure")
	artifact := &fakeStagedArtifact{close: func() error { return closeErr }}
	provider, err := newWithDependencies(
		t.Context(), testEmbeddedProvider(), Config{},
		successfulDependencies(artifact, &fakeDataProvider{}))
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}

	const callers = 8
	results := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			results <- provider.Close()
		}()
	}
	group.Wait()
	close(results)
	for result := range results {
		assertErrorCode(t, result, apperrors.ErrCodeInternal)
		if !stderrors.Is(result, closeErr) {
			t.Fatalf("Close() error = %v, want %v", result, closeErr)
		}
	}
	if artifact.CloseCalls() != 1 {
		t.Fatalf("artifact Close calls = %d, want 1", artifact.CloseCalls())
	}
}
