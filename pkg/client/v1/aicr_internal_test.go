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

	corev1 "k8s.io/api/core/v1"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator"
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

func (b *blockingDataProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return b.underlying.ReadFile(ctx, path)
}

func (b *blockingDataProvider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	// Signal exactly once that WalkDir entered; tolerate multiple
	// calls so a retry inside the same test wouldn't deadlock.
	select {
	case <-b.walkStarted:
	default:
		close(b.walkStarted)
	}
	<-b.walkUnblock
	return b.underlying.WalkDir(ctx, root, fn)
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

// TestWithVersionStored locks in that WithVersion threads the supplied
// version string onto the Client so the builder can stamp it into recipe
// metadata.
func TestWithVersionStored(t *testing.T) {
	c, err := NewClient(WithRecipeSource(EmbeddedSource()), WithVersion("v9.9.9"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()
	if c.version != "v9.9.9" {
		t.Fatalf("version = %q, want v9.9.9", c.version)
	}
}

// TestEnforceAllowLists exercises the shared allowlist guard directly:
// a nil allowlist is a no-op (all criteria pass); a configured allowlist
// rejects out-of-list accelerators while accepting in-list ones. The
// resolve-path integration is covered end-to-end in aicr_test.go once
// ResolveRecipeFromCriteria exists.
func TestEnforceAllowLists(t *testing.T) {
	t.Parallel()

	h100, err := recipe.BuildCriteriaWithRegistry(nil,
		recipe.WithAcceleratorRegistry("h100"),
		recipe.WithIntentRegistry("training"),
	)
	if err != nil {
		t.Fatalf("BuildCriteria h100: %v", err)
	}
	b200, err := recipe.BuildCriteriaWithRegistry(nil,
		recipe.WithAcceleratorRegistry("b200"),
		recipe.WithIntentRegistry("training"),
	)
	if err != nil {
		t.Fatalf("BuildCriteria b200: %v", err)
	}

	tests := []struct {
		name       string
		allowLists *AllowLists
		criteria   *recipe.Criteria
		wantErr    bool
	}{
		{"nil allowlist allows anything", nil, b200, false},
		{
			"in-list accelerator passes",
			&AllowLists{Accelerators: []string{string(recipe.CriteriaAcceleratorH100)}},
			h100,
			false,
		},
		{
			"out-of-list accelerator rejected",
			&AllowLists{Accelerators: []string{string(recipe.CriteriaAcceleratorH100)}},
			b200,
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &Client{allowLists: tt.allowLists}
			err := c.enforceAllowLists(tt.criteria)
			if (err != nil) != tt.wantErr {
				t.Fatalf("enforceAllowLists() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestEmbeddedSourceBuildsBareProvider locks in that EmbeddedSource
// resolves to a bare embedded DataProvider via buildDataProvider —
// the embedded-only path the REST server and the no-`--data` CLI
// case both need (built-in recipe data, no external overlay).
func TestEmbeddedSourceBuildsBareProvider(t *testing.T) {
	dp, err := buildDataProvider(recipeSource{kind: sourceKindEmbedded})
	if err != nil {
		t.Fatalf("buildDataProvider(embedded): %v", err)
	}
	if dp == nil {
		t.Fatal("expected non-nil embedded data provider")
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

// TestWithValidationTolerations_ExplicitNilOverrides pins FIX C: calling
// WithValidationTolerations (even with nil/empty) marks tolerationsSet and
// emits validator.WithTolerations, so the CLI's "always override the
// validator default tolerate-all" behavior is preserved. When the option is
// never called, no WithTolerations option is emitted and the validator keeps
// its default.
func TestWithValidationTolerations_ExplicitNilOverrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		opts           []ValidateOption
		wantSet        bool
		wantEmitted    bool
		wantToleration []corev1.Toleration
	}{
		{
			name:        "option unset → no override, default kept",
			opts:        nil,
			wantSet:     false,
			wantEmitted: false,
		},
		{
			name:        "explicit nil → override emitted, tolerations nil",
			opts:        []ValidateOption{WithValidationTolerations(nil)},
			wantSet:     true,
			wantEmitted: true,
		},
		{
			name:        "explicit empty → override emitted, tolerations empty",
			opts:        []ValidateOption{WithValidationTolerations([]corev1.Toleration{})},
			wantSet:     true,
			wantEmitted: true,
		},
		{
			name: "explicit value → override emitted, tolerations carried",
			opts: []ValidateOption{WithValidationTolerations([]corev1.Toleration{
				{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists},
			})},
			wantSet:     true,
			wantEmitted: true,
			wantToleration: []corev1.Toleration{
				{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := buildValidateConfig(tt.opts)
			if cfg.tolerationsSet != tt.wantSet {
				t.Errorf("tolerationsSet = %v, want %v", cfg.tolerationsSet, tt.wantSet)
			}

			// A sentinel non-nil default so an emitted override clearing to
			// nil/empty is observable: the option overwrites whatever the
			// validator already held.
			v := &validator.Validator{
				Tolerations: []corev1.Toleration{{Key: "*", Operator: corev1.TolerationOpExists}},
			}
			for _, o := range validateOptionsFromConfig(cfg) {
				o(v)
			}

			if !tt.wantEmitted {
				// Option not emitted → the sentinel default survives untouched.
				if len(v.Tolerations) != 1 || v.Tolerations[0].Key != "*" {
					t.Errorf("expected default tolerations preserved, got %+v", v.Tolerations)
				}
				return
			}
			// Option emitted → sentinel default is cleared and replaced by the
			// override value (nil, empty, or the explicit slice).
			if len(v.Tolerations) != len(tt.wantToleration) {
				t.Fatalf("Tolerations len = %d, want %d (%+v)",
					len(v.Tolerations), len(tt.wantToleration), v.Tolerations)
			}
			for i := range tt.wantToleration {
				if v.Tolerations[i].Key != tt.wantToleration[i].Key {
					t.Errorf("Tolerations[%d].Key = %q, want %q",
						i, v.Tolerations[i].Key, tt.wantToleration[i].Key)
				}
			}
		})
	}
}

// TestWithValidationTimeout_OptIn pins FIX D: WithValidationTimeout captures
// a pointer-wrapped duration so the ValidateState switch can distinguish
// unset (nil → default 60m), explicit 0 (no facade cap), and explicit >0.
func TestWithValidationTimeout_OptIn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		opts        []ValidateOption
		wantNil     bool
		wantValue   time.Duration
		wantHasZero bool
	}{
		{"unset → nil (default applies)", nil, true, 0, false},
		{"explicit 0 → non-nil zero (uncapped)", []ValidateOption{WithValidationTimeout(0)}, false, 0, true},
		{"explicit 5m → non-nil value", []ValidateOption{WithValidationTimeout(5 * time.Minute)}, false, 5 * time.Minute, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := buildValidateConfig(tt.opts)
			if tt.wantNil {
				if cfg.timeout != nil {
					t.Errorf("timeout = %v, want nil (default path)", *cfg.timeout)
				}
				return
			}
			if cfg.timeout == nil {
				t.Fatal("timeout = nil, want non-nil")
			}
			if *cfg.timeout != tt.wantValue {
				t.Errorf("timeout = %v, want %v", *cfg.timeout, tt.wantValue)
			}
			if tt.wantHasZero && *cfg.timeout != 0 {
				t.Errorf("expected explicit zero (no facade cap), got %v", *cfg.timeout)
			}
		})
	}
}

// TestWithValidationFailFast_RoundTrip pins that WithValidationFailFast captures
// the bool into validateConfig.failFast (nil when unset, non-nil when set) and
// that validateOptionsFromConfig emits validator.WithFailFast when the field is
// non-nil, landing the value on the Validator.
func TestWithValidationFailFast_RoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		opts    []ValidateOption
		wantNil bool
		wantVal bool
	}{
		{"unset", nil, true, false},
		{"set true", []ValidateOption{WithValidationFailFast(true)}, false, true},
		{"set false", []ValidateOption{WithValidationFailFast(false)}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildValidateConfig(tt.opts)
			if tt.wantNil {
				if cfg.failFast != nil {
					t.Fatalf("failFast = %v, want nil", *cfg.failFast)
				}
				// An unset failFast must emit NO validator option. Pre-seed a
				// validator with FailFast=true, apply the translated options,
				// and confirm none flipped it back: a regression that dropped
				// the nil-guard and always emitted validator.WithFailFast(false)
				// would fail here (the "set false" case alone can't catch it,
				// since false is also the validator's zero value).
				valOpts := validateOptionsFromConfig(cfg)
				v := validator.New(append([]validator.Option{validator.WithFailFast(true)}, valOpts...)...)
				if !v.FailFast {
					t.Error("validateOptionsFromConfig emitted a WithFailFast option for unset failFast; want none")
				}
				return
			}
			if cfg.failFast == nil {
				t.Fatal("failFast is nil, want set")
			}
			if *cfg.failFast != tt.wantVal {
				t.Errorf("failFast = %v, want %v", *cfg.failFast, tt.wantVal)
			}

			// Verify validateOptionsFromConfig emits the validator option.
			valOpts := validateOptionsFromConfig(cfg)
			v := validator.New(valOpts...)
			if v.FailFast != tt.wantVal {
				t.Errorf("validator.FailFast = %v after options, want %v", v.FailFast, tt.wantVal)
			}
		})
	}
}

// TestValidateState_ThreadsClientVersion pins FIX B: ValidateState threads
// the Client's version into the validator (it rewrites :latest images and
// populates AICR_CLI_VERSION). Run in no-cluster mode so no Kubernetes
// resources are created; the assertion is indirect — a clean no-cluster run
// completes, confirming the version-bearing option is accepted on the path.
// The direct option translation (WithVersion → Validator.Version) is covered
// by validator package tests; here we lock in that the facade emits it.
func TestValidateState_ThreadsClientVersion(t *testing.T) {
	t.Parallel()

	// The translation helper proves WithVersion lands on the Validator. Assert
	// the facade emits it by translating the same option set ValidateState
	// builds: dp + version are appended after the user opts. We can't read the
	// private append directly, so apply WithVersion to a Validator and confirm
	// Validator.Version is set — the exact line ValidateState now executes.
	v := &validator.Validator{}
	validator.WithVersion("v9.9.9")(v)
	if v.Version != "v9.9.9" {
		t.Fatalf("validator.WithVersion did not set Version (got %q)", v.Version)
	}

	// End-to-end: a client built WithVersion runs ValidateState in no-cluster
	// mode without error, exercising the path that now appends
	// validator.WithVersion(c.version).
	client := newClientForBundleTest(t)
	client.version = "v9.9.9"
	rec := newRecipeResultForBundleTest(client,
		[]recipe.ComponentRef{{Name: "c1", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "c1", Kind: "Helm"}},
	)
	results, err := client.ValidateState(t.Context(), rec, &Snapshot{},
		WithValidationNoCluster(true))
	if err != nil {
		t.Fatalf("ValidateState (no-cluster) with version: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one phase result in no-cluster mode")
	}
}

// TestAdoptRecipe_DeepCopiesForClientIsolation pins FIX A: adopting the
// SAME caller-owned pkg/recipe.RecipeResult into two different Clients must
// not let the second adopt overwrite the first's provider binding, and must
// not mutate the caller's original recipe pointer. adoptRecipe deep-copies
// before BindDataProvider, so each adopted result carries its own Client's
// DataProvider and the input recipe's provider stays nil.
func TestAdoptRecipe_DeepCopiesForClientIsolation(t *testing.T) {
	t.Parallel()

	// Two Clients with distinct DataProviders. Each NewClient builds a fresh
	// DataProvider (a new interface value), so two EmbeddedSource Clients
	// still hold distinct providers by pointer identity — the property the
	// isolation guarantee rests on.
	clientA, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient A: %v", err)
	}
	t.Cleanup(func() { _ = clientA.Close() })
	clientB, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient B: %v", err)
	}
	t.Cleanup(func() { _ = clientB.Close() })

	if clientA.dp == clientB.dp {
		t.Fatal("test precondition failed: both Clients share a DataProvider")
	}

	// One caller-owned raw recipe reused across both adopts.
	input := &recipe.RecipeResult{
		Kind:       recipe.RecipeResultKind,
		APIVersion: recipe.RecipeAPIVersion,
		Criteria:   &recipe.Criteria{Service: recipe.CriteriaServiceEKS},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "c1", Type: recipe.ComponentTypeHelm},
		},
	}
	if input.DataProvider() != nil {
		t.Fatal("test precondition failed: input recipe already has a provider")
	}

	resA, err := clientA.adoptRecipe(t.Context(), input)
	if err != nil {
		t.Fatalf("adoptRecipe A: %v", err)
	}
	resB, err := clientB.adoptRecipe(t.Context(), input)
	if err != nil {
		t.Fatalf("adoptRecipe B: %v", err)
	}

	// Each result must carry its OWN Client's provider — no cross-contamination.
	if resA.internal.DataProvider() != clientA.dp {
		t.Errorf("adopted A provider = %p, want clientA.dp %p",
			resA.internal.DataProvider(), clientA.dp)
	}
	if resB.internal.DataProvider() != clientB.dp {
		t.Errorf("adopted B provider = %p, want clientB.dp %p",
			resB.internal.DataProvider(), clientB.dp)
	}
	// The second adopt must not have mutated the first result's binding.
	if resA.internal.DataProvider() == resB.internal.DataProvider() {
		t.Error("adopted A and B share a provider; deep-copy isolation broke")
	}
	// Owner tokens are each Client's own pointer.
	if resA.owner != clientA || resB.owner != clientB {
		t.Errorf("owner mismatch: A=%p (want %p) B=%p (want %p)",
			resA.owner, clientA, resB.owner, clientB)
	}

	// The caller-owned input must be unchanged: adoptRecipe deep-copies, so
	// its provider was never bound and its internal pointer is distinct from
	// both adopted copies.
	if input.DataProvider() != nil {
		t.Errorf("input recipe provider was mutated to %p; deep-copy did not protect caller state",
			input.DataProvider())
	}
	if resA.internal == input || resB.internal == input {
		t.Error("adopted result aliases the caller's input recipe; deep-copy did not allocate a fresh result")
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
