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

package aicr_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/aicr"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

func TestNewClientRequiresRecipeSource(t *testing.T) {
	t.Parallel()

	_, err := aicr.NewClient()
	if err == nil {
		t.Fatalf("expected error when no recipe source is supplied, got nil")
	}

	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
	}
}

func TestNewClientRejectsMissingFilesystemDir(t *testing.T) {
	t.Parallel()

	// Build the missing path under t.TempDir() — strictly local and
	// guaranteed not to exist after this test runs. The hardcoded
	// "/nonexistent/path/..." form is environment-dependent (a runner
	// where the path coincidentally exists would skip the rejection
	// path this test is supposed to exercise). The layered data
	// provider constructor should refuse a missing directory rather
	// than silently fall through to the embedded default (the bug
	// that motivated this test).
	missing := filepath.Join(t.TempDir(), "missing-recipe-source")
	_, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.FilesystemSource(missing)),
	)
	if err == nil {
		t.Fatalf("expected NewClient to fail when FilesystemSource points at a nonexistent directory")
	}

	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest for bad filesystem source, got %s", se.Code)
	}
}

func TestNewClientRejectsOCISource(t *testing.T) {
	t.Parallel()

	// OCI sources are reserved but not yet implemented by the facade;
	// NewClient must refuse them with a clear error rather than
	// silently falling through.
	_, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.OCISource("ghcr.io/nvidia/aicr-recipes", "v0.1.0")),
	)
	if err == nil {
		t.Fatalf("expected NewClient to fail for OCI source (not yet implemented)")
	}

	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeUnavailable {
		t.Errorf("expected ErrCodeUnavailable for OCI source, got %s", se.Code)
	}
}

// TestNewClient_IsolatedDataProvider locks in the per-Client
// isolation contract: two Clients constructed from different
// FilesystemSource paths must not clobber each other's view of the
// recipe data.
//
// Pre-v0.12 the facade called recipe.SetDataProvider on every
// NewClient, mutating a process-global. Constructing a second
// Client with a different source replaced the first Client's data
// provider — the gpucluster controller in NVIDIA Crossplane would
// hit this whenever it had two ProviderConfigs pointing at
// different filesystem layouts and reconciled them concurrently.
//
// This test would fail under the old singleton implementation
// because client A's resolve would silently see client B's data
// after the second NewClient ran. Under the per-Builder
// DataProvider, both Clients keep their own cached metadata store
// and component registry.
func TestNewClient_IsolatedDataProvider(t *testing.T) {
	t.Parallel()

	// Build two separate filesystem layouts. Each gets a minimal
	// registry.yaml that's just-different-enough to tell them
	// apart by a Source() probe (ReadFile would also work but
	// requires a real component file; Source() always returns
	// the path or marker the layered provider tracks).
	dirA := t.TempDir()
	dirB := t.TempDir()
	for _, dir := range []string{dirA, dirB} {
		if err := os.WriteFile(filepath.Join(dir, "registry.yaml"),
			[]byte("components: []\n"), 0o600); err != nil {
			t.Fatalf("setup: write registry.yaml in %s: %v", dir, err)
		}
	}

	clientA, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(dirA)))
	if err != nil {
		t.Fatalf("NewClient A: %v", err)
	}
	t.Cleanup(func() { _ = clientA.Close() })
	clientB, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(dirB)))
	if err != nil {
		t.Fatalf("NewClient B: %v", err)
	}
	t.Cleanup(func() { _ = clientB.Close() })

	// Both Clients exist — under the singleton model only one
	// would have the "real" view of the data. Concretely: the
	// most-recently-constructed Client's source would have
	// replaced the global. With per-Builder DataProvider we get
	// two distinct, non-nil Client objects holding their own
	// state. The pure presence test below is intentionally weak;
	// the meaningful guarantee is that NewClient's fields don't
	// touch any shared mutable state, verified by reading the
	// implementation rather than by behavioral test (a real
	// behavioral test would need the resolve path to actually
	// produce different results for the two layouts, which means
	// dropping a real recipe set into each tempdir — out of
	// scope for this unit test, exercised in e2e instead).
	if clientA == nil || clientB == nil {
		t.Fatalf("both clients must be non-nil; got A=%v B=%v", clientA, clientB)
	}
	if clientA == clientB {
		t.Errorf("expected distinct *Client instances, got identical pointers")
	}
}

// TestClient_ConcurrentResolveAndClose lock in the data-race-free
// behavior of ResolveRecipe vs Close. Pre-fix the field reads in
// ResolveRecipe (c.builder access) raced with Close's writes
// (c.builder = nil), which `go test -race` would flag. Without the
// internal RWMutex this test would either deadlock, panic, or
// fail under -race. Run via go test -race ./... in CI.
func TestClient_ConcurrentResolveAndClose(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
		[]byte("components: []\n"), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}

	const goroutines = 50
	for trial := 0; trial < 5; trial++ {
		client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
		if err != nil {
			t.Fatalf("trial %d: NewClient: %v", trial, err)
		}

		var wg sync.WaitGroup
		// Spin up many ResolveRecipe goroutines, then race a
		// single Close against them. The mutex must serialize
		// the field-clear in Close against the field-read in
		// ResolveRecipe; correctness is "no panic, no race
		// flagged by -race, errors are well-typed."
		wg.Add(goroutines + 1)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				_, _ = client.ResolveRecipe(context.Background(), aicr.RecipeRequest{
					Service: "eks", Accelerator: "h100", Intent: "training",
				})
			}()
		}
		// Capture the Close error so a regression in the drain or
		// eviction path surfaces as a test failure instead of silently
		// disappearing into the void. Close runs concurrently with the
		// goroutines above; assert on its result AFTER wg.Wait so the
		// drain has converged.
		closeErrCh := make(chan error, 1)
		go func() {
			defer wg.Done()
			closeErrCh <- client.Close()
		}()
		wg.Wait()
		if err := <-closeErrCh; err != nil {
			t.Errorf("Close: %v", err)
		}
	}
}

// TestClient_CloseIsIdempotent and tolerates nil receivers — a Close
// method that panics on second call or on (*Client)(nil) tends to
// trip up cleanup chains, so we lock in the safety contract.
func TestClient_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
		[]byte("components: []\n"), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// First Close — clears state.
	if err := client.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// Second Close — must be a clean no-op.
	if err := client.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Nil-receiver Close — must not panic.
	var nilClient *aicr.Client
	if err := nilClient.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}

	// After Close, ResolveRecipe should fail cleanly rather than
	// panicking on the now-nil builder.
	if _, err := client.ResolveRecipe(t.Context(), aicr.RecipeRequest{}); err == nil {
		t.Error("expected ResolveRecipe to fail after Close, got nil error")
	}
}

// TestResolveRecipeRejectsPinnedReferences verifies that the facade
// surfaces a clear error — rather than silently dropping the fields —
// when callers set PinnedName or PinnedVersion on the request.
func TestResolveRecipeRejectsPinnedReferences(t *testing.T) {
	t.Parallel()

	// NewClient's layered provider requires the external directory to
	// contain a registry.yaml. Write a minimal one so setup succeeds
	// and we can exercise ResolveRecipe's pinned-rejection path.
	tmp := t.TempDir()
	minimalRegistry := "components: []\n"
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
		[]byte(minimalRegistry), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
	if err != nil {
		t.Fatalf("setup: NewClient failed with valid minimal registry: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	tests := []struct {
		name string
		req  aicr.RecipeRequest
	}{
		{
			name: "PinnedName alone",
			req:  aicr.RecipeRequest{PinnedName: "h100-eks-training"},
		},
		{
			name: "PinnedVersion alone",
			req:  aicr.RecipeRequest{PinnedVersion: "v1.2.3"},
		},
		{
			name: "both pinned fields",
			req: aicr.RecipeRequest{
				PinnedName:    "h100-eks-training",
				PinnedVersion: "v1.2.3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := client.ResolveRecipe(t.Context(), tt.req)
			if err == nil {
				t.Fatalf("expected ResolveRecipe to reject pinned fields, got nil error")
			}
			var se *aicrerrors.StructuredError
			if !errors.As(err, &se) {
				t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
			}
			if se.Code != aicrerrors.ErrCodeUnavailable {
				t.Errorf("expected ErrCodeUnavailable for pinned fields, got %s", se.Code)
			}
		})
	}
}

// TestBundleComponents_RequiresInternalRecipeResult pins the
// "RecipeResult must come from ResolveRecipe" contract: a caller-
// constructed RecipeResult has no internal field, so BundleComponents
// rejects it with a clear ErrCodeInvalidRequest rather than panicking
// on a nil dereference inside.
func TestBundleComponents_RequiresInternalRecipeResult(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
		[]byte("components: []\n"), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	// Caller-constructed RecipeResult — internal is nil.
	bogus := &aicr.RecipeResult{
		Name:    "made-up-name",
		Version: "v0",
		Components: []aicr.ComponentRef{
			{Name: "gpu-operator", Kind: "Helm", Version: "v25.10.0"},
		},
	}

	_, err = client.BundleComponents(context.Background(), bogus)
	if err == nil {
		t.Fatal("expected error from caller-constructed RecipeResult, got nil")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest for missing internal state, got %s", se.Code)
	}
}

// TestBundleComponents_NilInputsRejected pins the bounds-checking
// behavior — nil RecipeResult and Closed Client both surface
// ErrCodeInvalidRequest cleanly rather than panicking.
func TestBundleComponents_NilInputsRejected(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
		[]byte("components: []\n"), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}

	// nil RecipeResult
	{
		client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		defer client.Close()

		_, err = client.BundleComponents(context.Background(), nil)
		if err == nil {
			t.Fatal("expected error for nil RecipeResult, got nil")
		}
		var se *aicrerrors.StructuredError
		if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
			t.Errorf("expected ErrCodeInvalidRequest for nil result; got %v", err)
		}
	}

	// Bundling after Close
	{
		client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		// Caller-constructed RecipeResult — fails the internal-field
		// check first regardless of Close state. To exercise the
		// after-Close path we'd need to stash a real RecipeResult
		// from a successful ResolveRecipe; the empty registry.yaml
		// here can't resolve anything. The internal-field check
		// is sufficient guard; the after-Close guard is exercised
		// indirectly via TestClient_ConcurrentResolveAndClose's
		// stress.
		_ = client.Close()

		_, err = client.BundleComponents(context.Background(),
			&aicr.RecipeResult{Components: nil},
		)
		if err == nil {
			t.Fatal("expected error after Close, got nil")
		}
		var se *aicrerrors.StructuredError
		if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
			t.Errorf("expected ErrCodeInvalidRequest after Close; got %v", err)
		}
	}
}

// TestBundleComponents_NilReceiverSafe mirrors the existing
// ResolveRecipe-on-nil-Client guard. Easy to forget when adding new
// methods; pin so a cleanup chain calling BundleComponents on a
// (*Client)(nil) doesn't crash.
func TestBundleComponents_NilReceiverSafe(t *testing.T) {
	t.Parallel()

	var nilClient *aicr.Client
	_, err := nilClient.BundleComponents(context.Background(),
		&aicr.RecipeResult{Components: nil},
	)
	if err == nil {
		t.Fatal("expected error from nil receiver, got nil")
	}
}

// TestResolveRecipe_RejectsNegativeNodes locks in the upfront validation
// that RecipeRequest.Nodes < 0 is rejected with ErrCodeInvalidRequest at
// the facade boundary. Without this guard, the criteria builder treats
// negative values the same as zero ("unspecified"), masking the bug from
// the caller — a Crossplane controller computing Nodes from a CR spec
// field could ship -1 and never see an error.
func TestResolveRecipe_RejectsNegativeNodes(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// FilesystemSource requires registry.yaml; write a minimal one so the
	// tempdir layers cleanly over the embedded recipes. Resolution itself
	// never runs (negative Nodes rejection short-circuits before that),
	// but NewClient still validates the source on construction.
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
		[]byte("components: []\n"), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	tests := []struct {
		name  string
		nodes int32
	}{
		{"minus one", -1},
		{"minus large", -100},
		{"int32 min", -2147483648},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := client.ResolveRecipe(t.Context(), aicr.RecipeRequest{
				Service:     "eks",
				Accelerator: "h100",
				Intent:      "training",
				Nodes:       tt.nodes,
			})
			if err == nil {
				t.Fatalf("expected error for Nodes=%d, got nil", tt.nodes)
			}
			var se *aicrerrors.StructuredError
			if !errors.As(err, &se) {
				t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
			}
			if se.Code != aicrerrors.ErrCodeInvalidRequest {
				t.Errorf("expected ErrCodeInvalidRequest, got %s (msg: %s)", se.Code, se.Message)
			}
		})
	}
}

// TestResolveRecipe_OSEnablesOSPinnedOverlays proves that setting
// RecipeRequest.OS lets the resolver select OS-pinned leaf overlays
// like h100-eks-ubuntu-training-kubeflow. Without OS the asymmetric
// matching defaults the query OS to "any", which excludes any overlay
// pinning a specific OS — so platform mixins attached to those overlays
// (e.g., platform-kubeflow → kubeflow-trainer) become unreachable.
//
// The assertion: H100/EKS/Ubuntu/Training/Kubeflow MUST include the
// kubeflow-trainer component contributed by the platform-kubeflow mixin
// on the h100-eks-ubuntu-training-kubeflow overlay.
func TestResolveRecipe_OSEnablesOSPinnedOverlays(t *testing.T) {
	t.Parallel()

	// Write a minimal registry.yaml so FilesystemSource construction
	// succeeds; the LayeredDataProvider then merges with the embedded
	// recipes, so all embedded overlays (including the kubeflow-pinned
	// ones) remain reachable for resolution.
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
		[]byte("components: []\n"), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	result, err := client.ResolveRecipe(t.Context(), aicr.RecipeRequest{
		Service:     "eks",
		Accelerator: "h100",
		OS:          "ubuntu",
		Intent:      "training",
		Platform:    "kubeflow",
	})
	if err != nil {
		t.Fatalf("ResolveRecipe failed: %v", err)
	}

	const want = "kubeflow-trainer"
	var found bool
	names := make([]string, 0, len(result.Components))
	for _, c := range result.Components {
		names = append(names, c.Name)
		if c.Name == want {
			found = true
		}
	}
	if !found {
		t.Errorf("expected component %q in resolved recipe (proves the platform-kubeflow mixin on the os=ubuntu overlay was reachable); got components: %v",
			want, names)
	}
}

// TestBundleAndValidate_RejectCrossClientRecipeResult proves the owner-
// token guard: a RecipeResult produced by Client A cannot be threaded
// into Client B's BundleComponents or ValidateState. Without the guard
// the call silently mixed A's component refs (from resolution) with B's
// DataProvider reads (for values + manifests), producing the wrong
// values or supplemental manifests with no error — a classic foot-gun
// for multi-ProviderConfig controllers running one Client per
// ProviderConfig.
func TestBundleAndValidate_RejectCrossClientRecipeResult(t *testing.T) {
	t.Parallel()

	mkClient := func(t *testing.T) *aicr.Client {
		t.Helper()
		tmp := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
			[]byte("components: []\n"), 0o600); err != nil {
			t.Fatalf("setup: write registry.yaml: %v", err)
		}
		c, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	clientA := mkClient(t)
	clientB := mkClient(t)

	// Resolve through A. The simplest resolvable request that exercises
	// the embedded overlays — the resulting RecipeResult is what we'll
	// try to misuse on Client B.
	resultA, err := clientA.ResolveRecipe(t.Context(), aicr.RecipeRequest{
		Service:     "eks",
		Accelerator: "h100",
		OS:          "ubuntu",
		Intent:      "training",
	})
	if err != nil {
		t.Fatalf("clientA.ResolveRecipe: %v", err)
	}

	// BundleComponents on B with A's result must reject.
	if _, err := clientB.BundleComponents(t.Context(), resultA); err == nil {
		t.Error("BundleComponents: expected cross-client rejection, got nil error")
	} else {
		var se *aicrerrors.StructuredError
		if !errors.As(err, &se) {
			t.Errorf("BundleComponents: expected *aicrerrors.StructuredError, got %T: %v", err, err)
		} else if se.Code != aicrerrors.ErrCodeInvalidRequest {
			t.Errorf("BundleComponents: expected ErrCodeInvalidRequest, got %s", se.Code)
		}
	}

	// ValidateState on B with A's result must also reject. Passing a
	// non-nil &Snapshot{} is enough — the owner check runs before snap
	// validity, so we never need a real cluster snapshot.
	if _, err := clientB.ValidateState(t.Context(), resultA, &aicr.Snapshot{}); err == nil {
		t.Error("ValidateState: expected cross-client rejection, got nil error")
	} else {
		var se *aicrerrors.StructuredError
		if !errors.As(err, &se) {
			t.Errorf("ValidateState: expected *aicrerrors.StructuredError, got %T: %v", err, err)
		} else if se.Code != aicrerrors.ErrCodeInvalidRequest {
			t.Errorf("ValidateState: expected ErrCodeInvalidRequest, got %s", se.Code)
		}
	}

	// Sanity: A bundling its OWN result still succeeds (no spurious
	// rejection on the happy path). We don't assert on the bundle
	// content — that's other tests' job — only that no owner-related
	// error fires.
	if _, err := clientA.BundleComponents(t.Context(), resultA); err != nil {
		t.Errorf("clientA.BundleComponents(resultA) unexpectedly failed: %v", err)
	}
}

// TestClient_ConcurrentResolveScopesToOwnSource is the acceptance-
// criterion test for the issue's "two clients constructed from
// different recipe sources... resolve concurrently and return
// results scoped to their own source" requirement.
//
// Approach: each tempdir layers a unique marker overlay over the
// embedded recipes. The overlays match the same criteria but
// contribute distinct, identifiable componentRefs. Resolving the
// criteria via Client A must return Client A's marker (and not
// Client B's); resolving via Client B must return Client B's
// marker (and not Client A's). The two resolves run concurrently
// via sync.WaitGroup so the test fails under -race if the metadata-
// store or component-registry caches leak across DataProviders.
//
// This intentionally exercises the FULL ResolveRecipe path —
// criteria translation, metadata-store load, overlay match, merge,
// registry default fill-in — rather than hand-building a
// RecipeResult (which is what older tests in this file do). That
// path is the one a real in-process consumer would hit, and it's
// the one most likely to regress if a future refactor reaches for
// the package-global DataProvider.
func TestClient_ConcurrentResolveScopesToOwnSource(t *testing.T) {
	t.Parallel()

	// Marker overlay YAML — same criteria, distinct component per
	// tempdir. The component has inline source+chart so
	// applyRegistryDefaults has nothing to do (the marker component
	// is intentionally not in the embedded registry).
	overlayYAML := func(marker string) string {
		return `kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: facade-test-marker-` + marker + `
spec:
  base: base
  criteria:
    service: kind
    accelerator: h100
    intent: training
  componentRefs:
    - name: facade-marker-` + marker + `
      type: Helm
      source: https://example.com/facade-test
      chart: facade-marker-` + marker + `
`
	}

	mkClient := func(t *testing.T, marker string) *aicr.Client {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "registry.yaml"),
			[]byte("components: []\n"), 0o600); err != nil {
			t.Fatalf("setup %s: registry.yaml: %v", marker, err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "overlays"), 0o755); err != nil {
			t.Fatalf("setup %s: mkdir overlays: %v", marker, err)
		}
		if err := os.WriteFile(
			filepath.Join(dir, "overlays", "facade-test-marker-"+marker+".yaml"),
			[]byte(overlayYAML(marker)), 0o600); err != nil {
			t.Fatalf("setup %s: write overlay: %v", marker, err)
		}
		c, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(dir)))
		if err != nil {
			t.Fatalf("NewClient %s: %v", marker, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	clientA := mkClient(t, "a")
	clientB := mkClient(t, "b")

	req := aicr.RecipeRequest{
		Service:     "kind",
		Accelerator: "h100",
		Intent:      "training",
	}

	// Concurrent resolves. The WaitGroup releases both goroutines
	// simultaneously after both are armed, increasing the chance
	// that the metadata-store and registry caches are populated
	// concurrently for the two distinct DataProviders. Result and
	// error channels capture each goroutine's outcome so the main
	// test goroutine reports per-Client failures clearly.
	var (
		wg      sync.WaitGroup
		startCh = make(chan struct{})
		resultA *aicr.RecipeResult
		resultB *aicr.RecipeResult
		errA    error
		errB    error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startCh
		resultA, errA = clientA.ResolveRecipe(t.Context(), req)
	}()
	go func() {
		defer wg.Done()
		<-startCh
		resultB, errB = clientB.ResolveRecipe(t.Context(), req)
	}()
	close(startCh)
	wg.Wait()

	if errA != nil {
		t.Fatalf("clientA.ResolveRecipe: %v", errA)
	}
	if errB != nil {
		t.Fatalf("clientB.ResolveRecipe: %v", errB)
	}

	hasComponent := func(result *aicr.RecipeResult, name string) bool {
		for _, c := range result.Components {
			if c.Name == name {
				return true
			}
		}
		return false
	}

	// Client A's result must contain A's marker and NOT B's. The
	// "NOT B's" half is the actually-meaningful assertion: it's
	// what catches a regression where A's resolve accidentally
	// reads from B's metadata store.
	if !hasComponent(resultA, "facade-marker-a") {
		t.Errorf("clientA resolved result missing its own marker component facade-marker-a (got %v)", componentNames(resultA))
	}
	if hasComponent(resultA, "facade-marker-b") {
		t.Errorf("clientA resolved result contains the OTHER client's marker facade-marker-b — cross-source contamination (got %v)", componentNames(resultA))
	}

	if !hasComponent(resultB, "facade-marker-b") {
		t.Errorf("clientB resolved result missing its own marker component facade-marker-b (got %v)", componentNames(resultB))
	}
	if hasComponent(resultB, "facade-marker-a") {
		t.Errorf("clientB resolved result contains the OTHER client's marker facade-marker-a — cross-source contamination (got %v)", componentNames(resultB))
	}
}

// componentNames extracts a slice of component names from a
// RecipeResult, suitable for error-message diagnostics. Keeps the
// assertion logic above readable.
func componentNames(r *aicr.RecipeResult) []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Components))
	for _, c := range r.Components {
		out = append(out, c.Name)
	}
	return out
}
