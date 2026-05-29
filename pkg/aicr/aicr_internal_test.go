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

package aicr

import (
	"context"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// blockingDataProvider wraps an underlying DataProvider but holds
// WalkDir on a signal channel until the test releases it. Used by
// the Close-drains-inflight test to deterministically pin a resolve
// inside metadata-store loading while a second goroutine calls
// Client.Close, so the test can assert Close waits rather than
// racing the resolve's storeCache repopulation.
type blockingDataProvider struct {
	underlying  recipe.DataProvider
	walkStarted chan struct{}
	walkUnblock chan struct{}
}

func (b *blockingDataProvider) ReadFile(path string) ([]byte, error) {
	return b.underlying.ReadFile(path)
}

func (b *blockingDataProvider) WalkDir(root string, fn fs.WalkDirFunc) error {
	// Signal exactly once that WalkDir entered; tolerate multiple
	// calls so a retry inside the same test wouldn't deadlock.
	select {
	case <-b.walkStarted:
	default:
		close(b.walkStarted)
	}
	<-b.walkUnblock
	return b.underlying.WalkDir(root, fn)
}

func (b *blockingDataProvider) Source(path string) string {
	return b.underlying.Source(path)
}

// newRecipeResultForBundleTest builds a facade RecipeResult with its
// unexported internal field populated, side-stepping the requirement
// that callers obtain RecipeResults via ResolveRecipe. This is
// internal-only because the internal field is unexported on purpose;
// the production contract only allows the facade itself to set it.
//
// owner stamps the unexported owner pointer that BundleComponents /
// ValidateState check against. Pass the same *Client the test will
// invoke BundleComponents on so the cross-client guard accepts the
// result; pass a different (or nil) *Client to deliberately exercise
// the rejection path.
func newRecipeResultForBundleTest(owner *Client, refs []recipe.ComponentRef, facadeComponents []ComponentRef) *RecipeResult {
	internal := &recipe.RecipeResult{
		Kind:          "RecipeResult",
		APIVersion:    "v1",
		ComponentRefs: refs,
	}
	return &RecipeResult{
		Name:       "test",
		Components: facadeComponents,
		internal:   internal,
		owner:      owner,
	}
}

// newClientForBundleTest builds a Client whose builder is non-nil so
// BundleComponents passes the closed-Client guard. Only the closed-
// Client check looks at builder; the bundling path itself doesn't,
// so a placeholder Builder is enough.
//
// dp binds to the embedded recipe FS so the per-Client DataProvider
// snapshot in BundleComponents has a non-nil provider for values +
// manifest reads. Without an explicit dp, those reads fall back to
// recipe.GetDataProvider() — the package-global singleton — which
// races against any other test that touches it under -race.
func newClientForBundleTest(t *testing.T) *Client {
	t.Helper()
	return &Client{
		builder: recipe.NewBuilder(),
		dp:      recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "."),
	}
}

// TestBundleComponents_RejectsUnknownKind locks in the change from
// silent empty bundle (pre-fix) to a clear ErrCodeInvalidRequest
// (post-fix) when a recipe's component carries a Kind that doesn't
// normalise to "helm" or "kustomize" — typo bait at the recipe-emit
// boundary.
func TestBundleComponents_RejectsUnknownKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
	}{
		{"empty kind", ""},
		{"typo kind", "Hlem"},
		{"trailing space", "Helm "},
		{"truncation", "kustom"},
	}

	client := newClientForBundleTest(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRecipeResultForBundleTest(client,
				[]recipe.ComponentRef{{Name: "c1"}},
				[]ComponentRef{{Name: "c1", Kind: tt.kind}},
			)
			_, err := client.BundleComponents(context.Background(), r)
			if err == nil {
				t.Fatalf("expected error for unknown kind %q, got nil", tt.kind)
			}
			var se *aicrerrors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
			}
			if se.Code != aicrerrors.ErrCodeInvalidRequest {
				t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
			}
		})
	}
}

// TestBundleComponents_AcceptsLowercasedKind locks in the case-
// insensitive normalisation. Downstream deployment code typically
// accepts both forms; the AICR contract should match. A pure
// lowercase "helm" Kind with no values must produce a successful
// bundle (HelmValues nil, no error).
func TestBundleComponents_AcceptsLowercasedKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
	}{
		{"canonical Helm", "Helm"},
		{"lowercased helm", "helm"},
		{"mixed-case Helm", "HELM"},
	}

	client := newClientForBundleTest(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRecipeResultForBundleTest(client,
				// No ValuesFile, no Overrides → GetValuesForComponent
				// returns an empty map and never touches the global
				// DataProvider. Keeps the test hermetic.
				[]recipe.ComponentRef{{Name: "c1", Type: recipe.ComponentTypeHelm}},
				[]ComponentRef{{Name: "c1", Kind: tt.kind}},
			)
			bundles, err := client.BundleComponents(context.Background(), r)
			if err != nil {
				t.Fatalf("unexpected error for kind %q: %v", tt.kind, err)
			}
			if len(bundles) != 1 {
				t.Fatalf("expected 1 bundle, got %d", len(bundles))
			}
			if bundles[0].HelmValues != nil {
				t.Errorf("expected nil HelmValues for empty values map, got %q",
					bundles[0].HelmValues)
			}
		})
	}
}

// TestBundleComponents_HelmComponentLoadsManifestFiles locks in the
// "Helm components carry supplemental manifests too" contract. Recipes
// like h100-gke-cos-training and the base gpu-operator overlay attach
// extra raw manifests to a Helm component (gke-nccl-tcpxo installer +
// nri-device-injector; gpu-operator's dcgm-exporter overlay). Pre-fix
// the switch in BundleComponents only loaded ManifestFiles for the
// "Kustomize" branch, so these supplemental resources fell on the
// floor — bundle.Manifests was nil and the deployer had no way to
// know they should be applied.
//
// Post-fix: a Helm component with non-empty ManifestFiles produces a
// bundle whose Manifests is the multi-doc concatenation of those
// files (and HelmValues remains populated independently). A Helm
// component WITHOUT ManifestFiles still produces Manifests == nil so
// the existing one-Release-per-component path is unchanged.
//
// The test reads from the embedded recipe FS (the same
// components/gpu-operator/manifests/dcgm-exporter.yaml the existing
// TestGetManifestContent uses), keeping the hermetic-fixture style
// consistent with the rest of this file.
func TestBundleComponents_HelmComponentLoadsManifestFiles(t *testing.T) {
	t.Parallel()

	// Real embedded manifest on github/main. The specific manifest doesn't
	// matter for this test — we're verifying that BundleComponents reads
	// ANY supplemental manifest a recipe attaches to a Helm component. The
	// kernel-module-params overlay is small and stable.
	const manifestPath = "components/gpu-operator/manifests/kernel-module-params.yaml"

	tests := []struct {
		name           string
		manifestFiles  []string
		wantNilOutput  bool
		mustContainAll []string // substrings expected in the joined manifest blob
	}{
		{
			name:          "Helm component with no manifestFiles → Manifests nil",
			manifestFiles: nil,
			wantNilOutput: true,
		},
		{
			name:           "Helm component with one manifestFile → Manifests populated",
			manifestFiles:  []string{manifestPath},
			wantNilOutput:  false,
			mustContainAll: []string{"apiVersion", "kind"},
		},
		{
			name:           "Helm component with multiple manifestFiles → multi-doc joined with ---",
			manifestFiles:  []string{manifestPath, manifestPath},
			wantNilOutput:  false,
			mustContainAll: []string{"\n---\n"},
		},
	}

	client := newClientForBundleTest(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRecipeResultForBundleTest(client,
				[]recipe.ComponentRef{{
					Name:          "gpu-operator",
					Type:          recipe.ComponentTypeHelm,
					ManifestFiles: tt.manifestFiles,
				}},
				[]ComponentRef{{Name: "gpu-operator", Kind: "Helm"}},
			)
			bundles, err := client.BundleComponents(context.Background(), r)
			if err != nil {
				t.Fatalf("BundleComponents: %v", err)
			}
			if len(bundles) != 1 {
				t.Fatalf("expected 1 bundle, got %d", len(bundles))
			}
			got := bundles[0]
			if tt.wantNilOutput {
				if got.Manifests != nil {
					t.Errorf("expected nil Manifests for Helm component without manifestFiles, got %d bytes",
						len(got.Manifests))
				}
				return
			}
			if len(got.Manifests) == 0 {
				t.Fatalf("expected non-empty Manifests for Helm component with manifestFiles, got nil/empty")
			}
			for _, sub := range tt.mustContainAll {
				if !strings.Contains(string(got.Manifests), sub) {
					t.Errorf("Manifests missing expected substring %q; full content (%d bytes):\n%s",
						sub, len(got.Manifests), got.Manifests)
				}
			}
		})
	}
}

// TestRecipeResultFromInternal_PlumbsHelmFields locks in that the
// translation from pkg/recipe.ComponentRef into the facade's
// ComponentRef carries Source, Chart, and Namespace through. Without
// these, downstream consumers can't build a usable Helm Release —
// chart.repository, chart.name, and forProvider.namespace all come
// from this triplet.
func TestRecipeResultFromInternal_PlumbsHelmFields(t *testing.T) {
	t.Parallel()

	internal := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "v1",
		Criteria:   &recipe.Criteria{},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      "nfd",
				Type:      recipe.ComponentTypeHelm,
				Version:   "0.15.5",
				Source:    "https://kubernetes-sigs.github.io/node-feature-discovery/charts",
				Chart:     "node-feature-discovery",
				Namespace: "node-feature-discovery",
			},
			{
				Name:      "gpu-operator",
				Type:      recipe.ComponentTypeHelm,
				Version:   "v25.10.0",
				Source:    "https://helm.ngc.nvidia.com/nvidia",
				Chart:     "gpu-operator",
				Namespace: "gpu-operator",
			},
		},
	}

	out, err := recipeResultFromInternal(internal)
	if err != nil {
		t.Fatalf("recipeResultFromInternal: %v", err)
	}
	if len(out.Components) != 2 {
		t.Fatalf("expected 2 components, got %d", len(out.Components))
	}

	for i, want := range internal.ComponentRefs {
		got := out.Components[i]
		if got.Source != want.Source {
			t.Errorf("Components[%d].Source = %q, want %q", i, got.Source, want.Source)
		}
		if got.Chart != want.Chart {
			t.Errorf("Components[%d].Chart = %q, want %q", i, got.Chart, want.Chart)
		}
		if got.Namespace != want.Namespace {
			t.Errorf("Components[%d].Namespace = %q, want %q", i, got.Namespace, want.Namespace)
		}
	}
}

// TestCollectSnapshot_RejectsNilClient locks in the nil-receiver
// guard that mirrors ResolveRecipe / BundleComponents. Calling on
// a nil Client must return ErrCodeInvalidRequest, not panic.
func TestCollectSnapshot_RejectsNilClient(t *testing.T) {
	t.Parallel()

	var c *Client
	_, err := c.CollectSnapshot(context.Background(), &AgentConfig{Namespace: "x"})
	if err == nil {
		t.Fatalf("expected error from nil Client, got nil")
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
	}
}

// TestCollectSnapshot_RejectsNilConfig locks in that an explicit
// nil AgentConfig surfaces as ErrCodeInvalidRequest at the facade
// before any K8s deployment is attempted. The underlying snapshotter
// rejects nil too, but doing it at the facade keeps the error code
// consistent across paths.
func TestCollectSnapshot_RejectsNilConfig(t *testing.T) {
	t.Parallel()

	c := newClientForBundleTest(t)
	_, err := c.CollectSnapshot(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error from nil config, got nil")
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
	}
}

// TestCollectSnapshot_RejectsClosedClient locks in the closed-Client
// guard. After Close() clears the builder, CollectSnapshot must
// surface that as ErrCodeInvalidRequest rather than calling through
// to snapshotter.DeployAndGetSnapshot with stale state.
func TestCollectSnapshot_RejectsClosedClient(t *testing.T) {
	t.Parallel()

	c := newClientForBundleTest(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := c.CollectSnapshot(context.Background(), &AgentConfig{Namespace: "x"})
	if err == nil {
		t.Fatalf("expected error from closed Client, got nil")
	}
	if got != nil {
		t.Errorf("expected nil Snapshot on error, got %v", got)
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
	}
}

// TestValidateState_RejectsBadInput locks in every facade-side guard
// that runs before the validator is even constructed. Each row
// triggers a different path (nil client, nil recipe, recipe missing
// internal state, nil snapshot) and asserts the same outer error
// code so callers can rely on a uniform branch.
func TestValidateState_RejectsBadInput(t *testing.T) {
	t.Parallel()

	validClient := newClientForBundleTest(t)
	validRecipe := newRecipeResultForBundleTest(validClient,
		[]recipe.ComponentRef{{Name: "c1", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "c1", Kind: "Helm"}},
	)
	validSnap := &Snapshot{}

	tests := []struct {
		name   string
		client *Client
		recipe *RecipeResult
		snap   *Snapshot
	}{
		{"nil client", nil, validRecipe, validSnap},
		{"nil recipe", validClient, nil, validSnap},
		{"recipe missing internal", validClient, &RecipeResult{Name: "no-internal"}, validSnap},
		{"nil snapshot", validClient, validRecipe, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.client.ValidateState(context.Background(), tt.recipe, tt.snap)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var se *aicrerrors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
			}
			if se.Code != aicrerrors.ErrCodeInvalidRequest {
				t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
			}
		})
	}
}

// TestValidateState_RejectsClosedClient locks in the closed-Client
// guard. After Close() clears the builder, ValidateState must surface
// that as ErrCodeInvalidRequest rather than constructing a Validator
// from a half-torn-down Client.
func TestValidateState_RejectsClosedClient(t *testing.T) {
	t.Parallel()

	c := newClientForBundleTest(t)
	r := newRecipeResultForBundleTest(c,
		[]recipe.ComponentRef{{Name: "c1", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "c1", Kind: "Helm"}},
	)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := c.ValidateState(context.Background(), r, &Snapshot{})
	if err == nil {
		t.Fatalf("expected error from closed Client, got nil")
	}
	if got != nil {
		t.Errorf("expected nil PhaseResult slice on error, got %v", got)
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
	}
}

// TestClient_CloseDrainsInflightResolve is the deterministic race
// test for the inflight-drain pattern. Without the drain, this
// sequence races storeCache repopulation against eviction:
//
//  1. ResolveRecipe (goroutine A) RLock-snapshots builder, releases
//     mu, enters builder.BuildFromCriteria → LoadMetadataStoreFor →
//     blocked here inside provider.WalkDir.
//  2. Close (goroutine B) takes write-lock, nils builder/dp,
//     releases, evicts storeCache[dp] / registryCache[dp].
//  3. Goroutine A's WalkDir returns, buildMetadataStore finishes,
//     LoadMetadataStoreFor stores into storeCache[dp] AFTER Close
//     already evicted — leaking a stray entry.
//
// The drain in Close (c.inflight.Wait()) closes that window. This
// test pauses the resolve mid-walk via a blockingDataProvider, then
// confirms Close blocks until the resolve completes and that both
// caches return to their pre-test baseline.
//
// Constructed in-package because the dependency injection point
// (Client.dp/builder.dp) is unexported; the public NewClient API
// only takes a FilesystemSource and there's no way to thread a
// blocking provider through it.
func TestClient_CloseDrainsInflightResolve(t *testing.T) {
	t.Parallel()

	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ".")
	blockedDP := &blockingDataProvider{
		underlying:  embedded,
		walkStarted: make(chan struct{}),
		walkUnblock: make(chan struct{}),
	}
	builder := recipe.NewBuilder(recipe.WithDataProvider(blockedDP))
	c := &Client{
		builder: builder,
		dp:      blockedDP,
	}

	// Goroutine A: ResolveRecipe parks inside WalkDir on the unblock
	// channel. The criteria here are intentionally minimal; the
	// resolve may eventually error after unblock (the blocking
	// provider's underlying embedded data may not match every
	// overlay), but the assertions below don't care about the
	// resolve's correctness — only that it drains before Close
	// finishes and that it doesn't repopulate caches afterward.
	resolveDone := make(chan struct{})
	go func() {
		defer close(resolveDone)
		_, _ = c.ResolveRecipe(context.Background(), RecipeRequest{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
		})
	}()

	// Wait for the resolve to actually enter WalkDir, so the
	// subsequent Close races a known-in-flight operation.
	select {
	case <-blockedDP.walkStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("ResolveRecipe never entered WalkDir within 5s")
	}

	// Goroutine B: Close should block here on inflight.Wait.
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		_ = c.Close()
	}()

	// Give Close a deliberate window to (incorrectly) complete
	// early. If the drain is missing, Close returns immediately and
	// then the unblocked resolve repopulates the caches. 100ms is
	// large enough that a non-draining Close would have completed
	// many times over.
	select {
	case <-closeDone:
		t.Fatal("Close returned before in-flight ResolveRecipe completed; drain is missing")
	case <-time.After(100 * time.Millisecond):
		// Expected: Close is parked on inflight.Wait().
	}

	// Release WalkDir; resolve and Close both finish.
	close(blockedDP.walkUnblock)
	select {
	case <-resolveDone:
	case <-time.After(10 * time.Second):
		t.Fatal("ResolveRecipe did not complete within 10s after unblock")
	}
	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not complete within 10s after resolve drained")
	}

	// Cache invariant: the entry the resolve populated for OUR
	// blockedDP must have been evicted by Close. Scope-by-provider
	// (Contains, not Count) so concurrent tests in other packages
	// that touch their own DataProvider don't perturb the signal.
	// If the inflight-drain regresses, the resolve's
	// LoadMetadataStoreFor stores a fresh storeCache[blockedDP] entry
	// AFTER Close's eviction, and this assertion catches the leak.
	if recipe.CachedStoreContainsForTesting(blockedDP) {
		t.Error("storeCache leaked: blockedDP entry still present after Close (cache repopulated post-Close)")
	}
	if recipe.CachedRegistryContainsForTesting(blockedDP) {
		t.Error("registryCache leaked: blockedDP entry still present after Close (cache repopulated post-Close)")
	}
}

// TestClient_NoCacheGrowthAcrossManyCloseCycles is the acceptance-
// criterion test for the "memory does not grow when N clients are
// created and released in a loop" requirement.
//
// Each NewClient call constructs a fresh LayeredDataProvider (unique
// pointer identity), so the recipe package's sync.Map caches —
// storeCache and registryCache, keyed by DataProvider — would
// accumulate N entries if Close didn't evict them. After each
// Close, the assertion is scoped to THAT iteration's DataProvider
// (Contains, not Count), so concurrent sibling tests touching their
// own DataProvider don't perturb the signal.
//
// The DataProvider is captured before Close because Close zeros
// Client.dp (see Close in aicr.go).
//
// N is intentionally large (50) so a regression that leaks a single
// entry per Close cycle fails on the very first iteration but
// still exercises the eviction path under sustained load when
// running with -race.
//
// Lives in the internal test package so it can read Client.dp; an
// external test cannot.
func TestClient_NoCacheGrowthAcrossManyCloseCycles(t *testing.T) {
	const N = 50

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
		[]byte("components: []\n"), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}

	for i := 0; i < N; i++ {
		c, err := NewClient(WithRecipeSource(FilesystemSource(tmp)))
		if err != nil {
			t.Fatalf("iteration %d: NewClient: %v", i, err)
		}

		// ResolveRecipe forces both caches to populate for this
		// Client's DataProvider. Without this, only registryCache
		// would gain an entry (from buildDataProvider's construction
		// path); storeCache wouldn't, and the test would miss the
		// store-side leak.
		result, err := c.ResolveRecipe(t.Context(), RecipeRequest{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
		})
		if err != nil {
			t.Fatalf("iteration %d: ResolveRecipe: %v", i, err)
		}
		if result == nil {
			t.Fatalf("iteration %d: ResolveRecipe returned nil result without error", i)
		}

		dp := c.dp // capture before Close zeros it
		if err := c.Close(); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}

		if recipe.CachedStoreContainsForTesting(dp) {
			t.Errorf("iteration %d: storeCache not evicted after Close", i)
		}
		if recipe.CachedRegistryContainsForTesting(dp) {
			t.Errorf("iteration %d: registryCache not evicted after Close", i)
		}
	}
}
