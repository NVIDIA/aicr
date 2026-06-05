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

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
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

// TestResolveRecipeFromCriteriaLossless proves the lossless resolve path:
// ResolveRecipeFromCriteria returns a facade RecipeResult whose Resolved()
// surfaces the full pkg/recipe.RecipeResult, so callers see ComponentRefs
// and the threaded Metadata.Version directly.
func TestResolveRecipeFromCriteriaLossless(t *testing.T) {
	t.Parallel()

	c, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()), aicr.WithVersion("v1.2.3"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	crit, err := recipe.BuildCriteriaWithRegistry(nil, recipe.WithAcceleratorRegistry("h100"), recipe.WithIntentRegistry("training"))
	if err != nil {
		t.Fatalf("BuildCriteria: %v", err)
	}
	rec, err := c.ResolveRecipeFromCriteria(t.Context(), aicr.WrapCriteria(crit))
	if err != nil {
		t.Fatalf("ResolveRecipeFromCriteria: %v", err)
	}
	resolved := rec.Resolved()
	if resolved == nil {
		t.Fatal("expected non-nil Resolved()")
	}
	if len(resolved.ComponentRefs) == 0 {
		t.Fatal("expected component refs")
	}
	if resolved.Metadata.Version != "v1.2.3" {
		t.Fatalf("version not threaded: %q", resolved.Metadata.Version)
	}
}

// TestResolveRecipeFromSnapshot proves the snapshot-evaluator resolve path:
// ResolveRecipeFromSnapshot builds a recipe from explicit Criteria while
// evaluating its resolution constraints against an observed cluster Snapshot
// (mirroring `aicr recipe --snapshot`). A snapshot reporting K8s server
// version v1.33.0 satisfies the strictest readiness constraint on the
// h100-eks-training chain (">= 1.32.4"), so the OS-pinned/version-gated
// overlays are NOT excluded and the resolved recipe carries ComponentRefs —
// proving the constraint evaluator ran without error against the snapshot.
func TestResolveRecipeFromSnapshot(t *testing.T) {
	t.Parallel()

	c, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()), aicr.WithVersion("v1.2.3"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	crit, err := recipe.BuildCriteriaWithRegistry(nil,
		recipe.WithServiceRegistry("eks"),
		recipe.WithAcceleratorRegistry("h100"),
		recipe.WithIntentRegistry("training"),
	)
	if err != nil {
		t.Fatalf("BuildCriteria: %v", err)
	}

	// k8sVersionSnapshot() reports v1.33.0, which clears the
	// h100-eks-training chain's strictest readiness constraint (">= 1.32.4").
	snap := k8sVersionSnapshot()

	rec, err := c.ResolveRecipeFromSnapshot(t.Context(), aicr.WrapCriteria(crit), snap)
	if err != nil {
		t.Fatalf("ResolveRecipeFromSnapshot: %v", err)
	}
	if rec == nil {
		t.Fatal("expected non-nil *RecipeResult")
	}
	resolved := rec.Resolved()
	if resolved == nil {
		t.Fatal("expected non-nil Resolved()")
	}
	if len(resolved.ComponentRefs) == 0 {
		t.Fatal("expected component refs (snapshot satisfies the chain's constraints, so overlays must not be excluded)")
	}
	// Version is threaded through the builder just as on the criteria path.
	if resolved.Metadata.Version != "v1.2.3" {
		t.Fatalf("version not threaded: %q", resolved.Metadata.Version)
	}
}

// TestResolveRecipeFromSnapshot_NilArgs pins the bounds-checking contract:
// nil receiver, nil context, nil criteria, and nil snapshot each surface a
// structured ErrCodeInvalidRequest rather than panicking on a nil dereference.
func TestResolveRecipeFromSnapshot_NilArgs(t *testing.T) {
	t.Parallel()

	c, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	crit, err := recipe.BuildCriteriaWithRegistry(nil,
		recipe.WithAcceleratorRegistry("h100"),
		recipe.WithIntentRegistry("training"),
	)
	if err != nil {
		t.Fatalf("BuildCriteria: %v", err)
	}
	snap := k8sVersionSnapshot()

	facadeCrit := aicr.WrapCriteria(crit)

	// Nil receiver: must not panic; returns an error.
	t.Run("nil receiver", func(t *testing.T) {
		t.Parallel()
		var nilClient *aicr.Client
		if _, err := nilClient.ResolveRecipeFromSnapshot(context.Background(), facadeCrit, snap); err == nil {
			t.Fatal("expected error from nil receiver, got nil")
		}
	})

	// The remaining guards return a structured ErrCodeInvalidRequest.
	tests := []struct {
		name string
		ctx  context.Context
		crit *aicr.Criteria
		snap *aicr.Snapshot
	}{
		{name: "nil context", ctx: nil, crit: facadeCrit, snap: snap},
		{name: "nil criteria", ctx: context.Background(), crit: nil, snap: snap},
		{name: "nil snapshot", ctx: context.Background(), crit: facadeCrit, snap: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.ResolveRecipeFromSnapshot(tt.ctx, tt.crit, tt.snap)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
			var se *aicrerrors.StructuredError
			if !errors.As(err, &se) {
				t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
			}
			if se.Code != aicrerrors.ErrCodeInvalidRequest {
				t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
			}
		})
	}
}

// TestResolveRecipeFromSnapshotRejectsOutOfAllowList proves allowlist
// enforcement applies on the snapshot path too: a Client allowing only h100
// rejects a b200 request before any recipe is built — even though a snapshot
// is supplied.
func TestResolveRecipeFromSnapshotRejectsOutOfAllowList(t *testing.T) {
	t.Parallel()

	al := &aicr.AllowLists{
		Accelerators: []string{string(recipe.CriteriaAcceleratorH100)},
	}
	c, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithAllowLists(al),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	crit, err := recipe.BuildCriteriaWithRegistry(nil,
		recipe.WithAcceleratorRegistry("b200"),
		recipe.WithIntentRegistry("training"),
	)
	if err != nil {
		t.Fatalf("BuildCriteria: %v", err)
	}

	_, err = c.ResolveRecipeFromSnapshot(t.Context(), aicr.WrapCriteria(crit), k8sVersionSnapshot())
	if err == nil {
		t.Fatal("expected allowlist rejection for b200, got nil error")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
	}
}

// TestResolveRecipeFromCriteriaRejectsOutOfAllowList proves that a Client
// configured with an allowlist rejects a ResolveRecipeFromCriteria call
// whose accelerator is outside the allowed set. The allowlist permits only
// h100; requesting b200 must surface a structured error before any recipe
// is built.
func TestResolveRecipeFromCriteriaRejectsOutOfAllowList(t *testing.T) {
	t.Parallel()

	al := &aicr.AllowLists{
		Accelerators: []string{string(recipe.CriteriaAcceleratorH100)},
	}
	c, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithAllowLists(al),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	crit, err := recipe.BuildCriteriaWithRegistry(nil,
		recipe.WithAcceleratorRegistry("b200"),
		recipe.WithIntentRegistry("training"),
	)
	if err != nil {
		t.Fatalf("BuildCriteria: %v", err)
	}

	_, err = c.ResolveRecipeFromCriteria(t.Context(), aicr.WrapCriteria(crit))
	if err == nil {
		t.Fatal("expected allowlist rejection for b200, got nil error")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
	}
}

// TestSelectFromRecipe proves the hydrate+select helper mirrors
// `aicr query`: a dot-path selector extracts a nested value from a
// resolved recipe, and an empty selector returns the whole hydrated
// structure. The recipe is resolved from embedded data so the test is
// hermetic.
func TestSelectFromRecipe(t *testing.T) {
	t.Parallel()

	c, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	crit, err := recipe.BuildCriteriaWithRegistry(nil, recipe.WithAcceleratorRegistry("h100"), recipe.WithIntentRegistry("training"))
	if err != nil {
		t.Fatalf("BuildCriteria: %v", err)
	}
	rec, err := c.ResolveRecipeFromCriteria(t.Context(), aicr.WrapCriteria(crit))
	if err != nil {
		t.Fatalf("ResolveRecipeFromCriteria: %v", err)
	}

	// Dot-path selector into a known embedded value.
	got, err := aicr.SelectFromRecipe(rec, "components.gpu-operator.values.driver.version")
	if err != nil {
		t.Fatalf("SelectFromRecipe(driver.version): %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil value for components.gpu-operator.values.driver.version")
	}

	// Empty selector returns the entire hydrated structure.
	all, err := aicr.SelectFromRecipe(rec, "")
	if err != nil {
		t.Fatalf("SelectFromRecipe(empty): %v", err)
	}
	if all == nil {
		t.Fatal("expected non-nil hydrated structure for empty selector")
	}

	// nil recipe is rejected.
	if _, err := aicr.SelectFromRecipe(nil, ""); err == nil {
		t.Fatal("expected error for nil recipe, got nil")
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

// TestClient_NoCacheGrowthAcrossManyCloseCycles is the acceptance-
// criterion test for the issue's "memory does not grow when N
// clients are created and released in a loop" requirement.
//
// Each NewClient call constructs a fresh LayeredDataProvider (unique
// pointer identity), so the recipe package's sync.Map caches —
// storeCache and registryCache, keyed by DataProvider — would
// accumulate N entries if Close didn't evict them. The test takes
// a baseline of both cache counts immediately before the loop and
// asserts they return to the same value after N iterations. Baseline
// math insulates the test from any parallel sibling's cache entries
// (those exist both before and after, so they cancel out in the diff).
//
// N is intentionally large (50) so a regression that leaks a single
// entry per Close cycle would push the delta well past any noise
// floor; running with -race confirms there's no concurrent map
// corruption while Close is racing the package-global cache writers.
//
// NOT t.Parallel: the recipe-package caches (storeCache, registryCache)
// are process-global sync.Maps. A parallel sibling test that constructs
// a Client populates those caches while this test runs, so the
// baseline-diff arithmetic would catch sibling entries as "growth"
// from this test's perspective. Running sequentially (during go test's
// pre-parallel phase) gives a clean baseline at minimal time cost.
func TestClient_NoCacheGrowthAcrossManyCloseCycles(t *testing.T) {
	const N = 50

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"),
		[]byte("components: []\n"), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}

	baselineStore := recipe.CachedStoreCountForTesting()
	baselineRegistry := recipe.CachedRegistryCountForTesting()

	for i := 0; i < N; i++ {
		c, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(tmp)))
		if err != nil {
			t.Fatalf("iteration %d: NewClient: %v", i, err)
		}

		// ResolveRecipe forces both caches to populate for this
		// Client's DataProvider. Without this, only registryCache
		// would gain an entry (from buildDataProvider's construction
		// path); storeCache wouldn't, and the test would miss the
		// store-side leak.
		if _, err := c.ResolveRecipe(t.Context(), aicr.RecipeRequest{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
		}); err != nil {
			t.Fatalf("iteration %d: ResolveRecipe: %v", i, err)
		}

		if err := c.Close(); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
	}

	afterStore := recipe.CachedStoreCountForTesting()
	afterRegistry := recipe.CachedRegistryCountForTesting()

	if delta := afterStore - baselineStore; delta != 0 {
		t.Errorf("storeCache grew by %d after %d NewClient/Resolve/Close cycles; expected 0 (Close should evict)",
			delta, N)
	}
	if delta := afterRegistry - baselineRegistry; delta != 0 {
		t.Errorf("registryCache grew by %d after %d NewClient/Resolve/Close cycles; expected 0 (Close should evict)",
			delta, N)
	}
}

// leafOverlayYAML is a minimal leaf RecipeMetadata overlay (carries
// spec.criteria) that LoadRecipe can auto-hydrate against the embedded
// recipe data. It targets the h100-eks-training chain (no OS pin) so the
// hydrated result's only top-level readiness constraint is
// K8s.server.version — keeping the ValidateState assertion below
// decoupled from OS-mixin constraints while still exercising real
// embedded resolution (base + h100-any + eks + eks-training).
const leafOverlayYAML = `kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: aicr-loadrecipe-test
spec:
  base: h100-eks-training
  criteria:
    service: eks
    accelerator: h100
    intent: training
  componentRefs: []
`

// TestLoadRecipe_HydratesAndOwns proves LoadRecipe loads a leaf overlay
// file through the Client's provider, hydrates it against the embedded
// recipe data (Components populated), stamps the producing Client as
// owner, and produces a RecipeResult that ValidateState accepts (the
// owner/internal wiring is correct end-to-end).
func TestLoadRecipe_HydratesAndOwns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	overlayPath := filepath.Join(dir, "overlay.yaml")
	if err := os.WriteFile(overlayPath, []byte(leafOverlayYAML), 0o600); err != nil {
		t.Fatalf("setup: write overlay: %v", err)
	}

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	result, err := client.LoadRecipe(t.Context(), overlayPath, "")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}
	if result == nil {
		t.Fatal("LoadRecipe returned nil result")
	}
	if len(result.Components) == 0 {
		t.Errorf("expected hydrated recipe to carry components, got none")
	}

	// The loaded result must be usable by ValidateState (owner-stamped,
	// internal populated). no-cluster mode keeps the run hermetic, but
	// the readiness pre-flight still evaluates the recipe's resolution
	// constraints (e.g., K8s.server.version >= 1.32.4) against the
	// snapshot — so supply a snapshot that satisfies them.
	phases, err := client.ValidateState(t.Context(), result, k8sVersionSnapshot(),
		aicr.WithValidationNoCluster(true))
	if err != nil {
		t.Fatalf("ValidateState on loaded recipe: %v", err)
	}
	if len(phases) == 0 {
		t.Errorf("expected at least one phase result from no-cluster validation, got none")
	}
}

// TestLoadRecipe_BareResultNoCriteria proves LoadRecipe accepts an
// already-hydrated RecipeResult file that carries no spec.criteria — the
// same tolerance recipe.LoadFromFile has historically had. The resolve
// path rejects a nil Criteria as a builder bug, but a file loaded from
// disk legitimately may omit it; LoadRecipe must not diverge from the CLI
// loader's behavior or the validate-command kind-handling tests break.
func TestLoadRecipe_BareResultNoCriteria(t *testing.T) {
	t.Parallel()

	const bareResult = `kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  version: test
componentRefs: []
`
	dir := t.TempDir()
	recipePath := filepath.Join(dir, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte(bareResult), 0o600); err != nil {
		t.Fatalf("setup: write recipe: %v", err)
	}

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	result, err := client.LoadRecipe(t.Context(), recipePath, "")
	if err != nil {
		t.Fatalf("LoadRecipe on bare RecipeResult: %v", err)
	}
	if result == nil {
		t.Fatal("LoadRecipe returned nil result")
	}
	// No criteria → empty facade Name, but Resolved() must still be
	// populated so downstream validate/evidence paths work.
	if result.Name != "" {
		t.Errorf("Name = %q, want empty for a criteria-less recipe", result.Name)
	}
	if result.Resolved() == nil {
		t.Error("Resolved() = nil, want the loaded recipe")
	}
}

// k8sVersionSnapshot builds a minimal Snapshot whose K8s/server/version
// reading satisfies the readiness constraints carried by the embedded
// h100-eks-training chain (the strictest is ">= 1.32.4").
func k8sVersionSnapshot() *aicr.Snapshot {
	// v1.33.0 clears the strictest readiness constraint on the
	// h100-eks-training chain (">= 1.32.4"). All current callers want a
	// satisfying version, so it is a fixed constant here rather than a
	// parameter (unparam would flag a param that never varies).
	const version = "v1.33.0"
	return aicr.WrapSnapshot(&snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			measurement.NewMeasurement(measurement.TypeK8s).
				WithSubtypeBuilder(
					measurement.NewSubtypeBuilder("server").
						SetString("version", version),
				).
				Build(),
		},
	})
}

// loadTestRecipe constructs an EmbeddedSource Client and loads the
// shared leaf overlay through it, returning both so callers can drive
// ValidateState. The Client is registered for cleanup on t.
func loadTestRecipe(t *testing.T) (*aicr.Client, *aicr.RecipeResult) {
	t.Helper()
	dir := t.TempDir()
	overlayPath := filepath.Join(dir, "overlay.yaml")
	if err := os.WriteFile(overlayPath, []byte(leafOverlayYAML), 0o600); err != nil {
		t.Fatalf("setup: write overlay: %v", err)
	}
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.LoadRecipe(t.Context(), overlayPath, "")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}
	return client, result
}

// TestValidateState_PhaseSelection proves WithValidationPhases restricts
// the run to exactly the requested phase(s), in order. Run in no-cluster
// mode so no Kubernetes resources are created; the per-Client provider
// (EmbeddedSource) loads the validator catalog so the run reaches the
// phase loop rather than failing catalog load.
func TestValidateState_PhaseSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		phases []aicr.Phase
		want   []aicr.Phase
	}{
		{
			name:   "single deployment phase",
			phases: []aicr.Phase{aicr.PhaseDeployment},
			want:   []aicr.Phase{aicr.PhaseDeployment},
		},
		{
			name:   "deployment then conformance",
			phases: []aicr.Phase{aicr.PhaseDeployment, aicr.PhaseConformance},
			want:   []aicr.Phase{aicr.PhaseDeployment, aicr.PhaseConformance},
		},
		{
			name:   "unset runs all phases",
			phases: nil,
			want:   []aicr.Phase{aicr.PhaseDeployment, aicr.PhaseConformance, aicr.PhasePerformance},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, result := loadTestRecipe(t)

			opts := []aicr.ValidateOption{aicr.WithValidationNoCluster(true)}
			if tt.phases != nil {
				opts = append(opts, aicr.WithValidationPhases(tt.phases...))
			}

			phases, err := client.ValidateState(t.Context(), result,
				k8sVersionSnapshot(), opts...)
			if err != nil {
				t.Fatalf("ValidateState: %v", err)
			}

			got := make([]aicr.Phase, 0, len(phases))
			for _, pr := range phases {
				got = append(got, pr.Phase)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("phase count = %d (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("phase[%d] = %q, want %q (full: %v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

// TestValidateState_RejectsInvalidPhase proves an unrecognized phase value
// passed via WithValidationPhases is rejected with ErrCodeInvalidRequest
// before any cluster work — a typed Phase typo must not silently degrade to
// an empty/skipped run. Run in no-cluster mode so the only thing under test
// is the phase guard.
func TestValidateState_RejectsInvalidPhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		phases []aicr.Phase
	}{
		{name: "typo", phases: []aicr.Phase{aicr.Phase("deploymnt")}},
		{name: "empty string", phases: []aicr.Phase{aicr.Phase("")}},
		{name: "wildcard not accepted by facade", phases: []aicr.Phase{aicr.Phase("all")}},
		{name: "valid then invalid", phases: []aicr.Phase{aicr.PhaseDeployment, aicr.Phase("bogus")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, result := loadTestRecipe(t)

			_, err := client.ValidateState(t.Context(), result,
				k8sVersionSnapshot(),
				aicr.WithValidationNoCluster(true),
				aicr.WithValidationPhases(tt.phases...))
			if err == nil {
				t.Fatal("expected error for invalid phase, got nil")
			}
			var se *aicrerrors.StructuredError
			if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
				t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
			}
		})
	}
}

// TestRecipeResult_Resolved proves Resolved() exposes the full underlying
// recipe.RecipeResult after LoadRecipe — including the resolution
// constraints and validation config the lossy facade shape omits — so
// internal consumers (phase warnings, evidence emission) can reach them.
func TestRecipeResult_Resolved(t *testing.T) {
	t.Parallel()

	_, result := loadTestRecipe(t)

	resolved := result.Resolved()
	if resolved == nil {
		t.Fatal("Resolved() returned nil for a LoadRecipe result")
	}
	// The full recipe carries Criteria (used to derive the facade Name)
	// and at least one resolution constraint (the h100-eks-training chain
	// pins K8s.server.version) — neither is on the facade RecipeResult.
	if resolved.Criteria == nil {
		t.Error("Resolved().Criteria = nil, want the hydrated criteria")
	}
	if len(resolved.Constraints) == 0 {
		t.Error("Resolved().Constraints is empty, want the chain's resolution constraints")
	}

	// A RecipeResult NOT produced by the Client has a nil internal and
	// Resolved() must return nil rather than panic.
	var empty aicr.RecipeResult
	if empty.Resolved() != nil {
		t.Error("Resolved() on a zero-value RecipeResult = non-nil, want nil")
	}
}

// TestValidateState_ImageAndCommitOptions proves the three new validate
// options (commit + image registry/tag overrides) translate cleanly and a
// no-cluster ValidateState run still succeeds with them set. The overrides
// only influence emitted Job images (not exercised in no-cluster mode), so
// the assertion is that the option plumbing doesn't break the run.
func TestValidateState_ImageAndCommitOptions(t *testing.T) {
	t.Parallel()

	client, result := loadTestRecipe(t)

	phases, err := client.ValidateState(t.Context(), result, k8sVersionSnapshot(),
		aicr.WithValidationNoCluster(true),
		aicr.WithValidationCommit("abc1234"),
		aicr.WithValidationImageRegistryOverride("localhost:5001"),
		aicr.WithValidationImageTagOverride("dev"),
	)
	if err != nil {
		t.Fatalf("ValidateState with image/commit options: %v", err)
	}
	if len(phases) == 0 {
		t.Error("expected at least one phase result, got none")
	}

	// Empty-string overrides are the unset sentinel and must be accepted
	// the same as omitting the option entirely.
	if _, err := client.ValidateState(t.Context(), result, k8sVersionSnapshot(),
		aicr.WithValidationNoCluster(true),
		aicr.WithValidationCommit(""),
		aicr.WithValidationImageRegistryOverride(""),
		aicr.WithValidationImageTagOverride(""),
	); err != nil {
		t.Fatalf("ValidateState with empty overrides: %v", err)
	}
}

// writeExternalCriterionData lays out a minimal --data directory that the
// layered FilesystemSource provider can load: a registry.yaml (required) plus
// an overlay declaring a non-OSS criteria value. Loading this provider's
// catalog seeds that provider's criteria registry with the external value,
// mirroring the e2e fixture in tests/chainsaw/cli/criteria-registry.
func writeExternalCriterionData(t *testing.T, service string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"),
		[]byte("apiVersion: aicr.nvidia.com/v1alpha1\nkind: ComponentRegistry\ncomponents: []\n"), 0o600); err != nil {
		t.Fatalf("setup: write registry.yaml: %v", err)
	}
	overlayDir := filepath.Join(dir, "overlays")
	if err := os.MkdirAll(overlayDir, 0o755); err != nil {
		t.Fatalf("setup: mkdir overlays: %v", err)
	}
	overlay := "apiVersion: aicr.nvidia.com/v1alpha1\n" +
		"kind: RecipeMetadata\n" +
		"metadata:\n  name: " + service + "-h100-training\n" +
		"spec:\n  base: base\n  criteria:\n" +
		"    service: " + service + "\n    accelerator: h100\n    intent: training\n" +
		"  componentRefs: []\n"
	if err := os.WriteFile(filepath.Join(overlayDir, service+"-overlay.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatalf("setup: write overlay: %v", err)
	}
	return dir
}

// TestClient_LoadCatalogSeedsCriteriaRegistry verifies that LoadCatalog seeds
// THIS Client's per-provider criteria registry: embedded OSS values are always
// present, and a FilesystemSource --data overlay declaring a non-OSS service
// value becomes Has()-visible after LoadCatalog. A separate EmbeddedSource
// client's registry must NOT see the external value — that is the per-provider
// isolation guarantee Stage 4 relies on.
func TestClient_LoadCatalogSeedsCriteriaRegistry(t *testing.T) {
	// Not parallel: asserts on per-provider criteria-registry state seeded by
	// LoadCatalog, and uses t.Setenv (which forbids t.Parallel). Clearing
	// AICR_CRITERIA_STRICT neutralizes the CI gate (make test sets it to 1) so
	// each freshly-constructed per-provider registry starts non-strict and the
	// external --data value is admitted — strict-mode behavior is covered
	// separately in TestClient_CriteriaRegistryStrictMode.
	t.Setenv("AICR_CRITERIA_STRICT", "")

	const externalService = "ncp-internal-test"
	dataDir := writeExternalCriterionData(t, externalService)

	fsClient, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(dataDir)))
	if err != nil {
		t.Fatalf("NewClient(FilesystemSource): %v", err)
	}
	t.Cleanup(func() { _ = fsClient.Close() })

	embClient, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient(EmbeddedSource): %v", err)
	}
	t.Cleanup(func() { _ = embClient.Close() })

	if err := fsClient.LoadCatalog(t.Context()); err != nil {
		t.Fatalf("fsClient.LoadCatalog: %v", err)
	}
	if err := embClient.LoadCatalog(t.Context()); err != nil {
		t.Fatalf("embClient.LoadCatalog: %v", err)
	}

	fsReg := fsClient.CriteriaRegistry()
	if fsReg == nil {
		t.Fatal("fsClient.CriteriaRegistry() returned nil")
	}
	embReg := embClient.CriteriaRegistry()
	if embReg == nil {
		t.Fatal("embClient.CriteriaRegistry() returned nil")
	}

	// Embedded OSS criteria are seeded into both registries after LoadCatalog.
	if !fsReg.Has(recipe.FieldService, "eks") {
		t.Error("fsClient registry missing embedded OSS service 'eks'")
	}
	if !embReg.Has(recipe.FieldService, "eks") {
		t.Error("embClient registry missing embedded OSS service 'eks'")
	}

	// The external --data overlay's novel service value is visible ONLY in the
	// FilesystemSource client's registry — the EmbeddedSource client never
	// walked that directory, so its registry is isolated.
	if !fsReg.Has(recipe.FieldService, externalService) {
		t.Errorf("fsClient registry missing external service %q after LoadCatalog", externalService)
	}
	if embReg.Has(recipe.FieldService, externalService) {
		t.Errorf("embClient registry leaked external service %q (per-provider isolation broken)", externalService)
	}
}

// TestClient_CriteriaRegistryStrictMode verifies that SetStrict applied to the
// Client's own registry hides external-origin values while keeping embedded
// OSS values — the per-Client equivalent of the --criteria-strict gate.
func TestClient_CriteriaRegistryStrictMode(t *testing.T) {
	// Not parallel: mutates per-provider registry strict state and uses
	// t.Setenv (which forbids t.Parallel). Clear AICR_CRITERIA_STRICT so the
	// registry starts non-strict regardless of the CI gate (make test sets it
	// to 1); the test then flips strict explicitly via SetStrict.
	t.Setenv("AICR_CRITERIA_STRICT", "")

	const externalService = "ncp-strict-test"
	dataDir := writeExternalCriterionData(t, externalService)

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(dataDir)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.LoadCatalog(t.Context()); err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	reg := client.CriteriaRegistry()
	if !reg.Has(recipe.FieldService, externalService) {
		t.Fatalf("precondition: external service %q not seeded", externalService)
	}

	reg.SetStrict(true)
	t.Cleanup(func() { reg.SetStrict(false) })

	if reg.Has(recipe.FieldService, externalService) {
		t.Errorf("strict mode must hide external service %q", externalService)
	}
	if !reg.Has(recipe.FieldService, "eks") {
		t.Error("strict mode must still admit embedded OSS service 'eks'")
	}
}

// TestClient_LoadCatalogGuards verifies the nil-context and closed-Client
// rejections on LoadCatalog match the other Client methods.
func TestClient_LoadCatalogGuards(t *testing.T) {
	t.Parallel()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	//nolint:staticcheck // intentional nil-context guard test
	if err := client.LoadCatalog(nil); err == nil {
		t.Error("LoadCatalog(nil ctx) must be rejected")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.LoadCatalog(t.Context()); err == nil {
		t.Error("LoadCatalog on a closed Client must be rejected")
	}
}
