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

package aicr_test

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// writeConfig writes an AICRConfig document to a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aicr-config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// The fixtures are per-section on purpose. spec.recipe.criteria and
// spec.recipe.input.snapshot are mutually exclusive, and a document must
// carry at least one section — so a single "everything" fixture is not a
// valid AICRConfig. Splitting them also exercises the case a team actually
// hits: a document that configures some sections and not others.

const recipeConfig = `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: test
spec:
  recipe:
    data: /etc/aicr/recipes
    profile: gpuStack=operator-managed
    criteriaStrict: true
    criteria:
      service: eks
      accelerator: h100
      intent: training
      os: ubuntu
      platform: kubeflow
      nodes: 8
`

const snapshotInputConfig = `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: test
spec:
  recipe:
    input:
      snapshot: ./snapshot.yaml
`

const verifyConfig = `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: test
spec:
  verify:
    policy:
      minTrustLevel: verified
      requireCreator: ci@example.com
      cliVersionConstraint: ">= 0.16.0"
    trust:
      certificateIdentityRegexp: ^https://github\.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.*
      key: awskms://alias/aicr
      trustRoot: ./trusted_root.json
`

// TestLoadConfig_DerivesVerifyOptions pins the one-to-one mapping between
// spec.verify and BundleVerifyOptions. That alignment is the whole reason this
// derivation is a copy rather than a translation table, so a drift on either
// side should fail here.
func TestLoadConfig_DerivesVerifyOptions(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, verifyConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	opts, err := cfg.BundleVerifyOptions()
	if err != nil {
		t.Fatalf("BundleVerifyOptions: %v", err)
	}

	tests := []struct {
		field string
		got   string
		want  string
	}{
		{"MinTrustLevel", opts.MinTrustLevel, "verified"},
		{"RequireCreator", opts.RequireCreator, "ci@example.com"},
		{"CLIVersionConstraint", opts.CLIVersionConstraint, ">= 0.16.0"},
		{"Key", opts.Key, "awskms://alias/aicr"},
		{"TrustRoot", opts.TrustRoot, "./trusted_root.json"},
		{"CertificateIdentityRegexp", opts.CertificateIdentityRegexp, aicr.TrustedIdentityPattern},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.field, tt.got, tt.want)
		}
	}

	// IgnoreTLog has no config counterpart by design: a checked-in file must
	// not be able to drop the transparency-log requirement.
	if opts.IgnoreTLog {
		t.Error("IgnoreTLog = true; it must not be settable from a config document")
	}
}

// TestLoadConfig_DerivesRecipeInputs covers the spec.recipe derivations.
func TestLoadConfig_DerivesRecipeInputs(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, recipeConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	source, ok := cfg.RecipeSource()
	if !ok {
		t.Error("RecipeSource reported unset, but spec.recipe.data is populated")
	}
	// The source is opaque by design; proving it is usable matters more than
	// inspecting it, since NewClient is its only consumer.
	if _, err = aicr.NewClient(aicr.WithRecipeSource(source)); err == nil {
		t.Error("expected NewClient to reject a nonexistent data directory")
	}

	if !cfg.IsCriteriaStrict() {
		t.Error("IsCriteriaStrict = false, want true")
	}

	criteria, err := cfg.RecipeCriteria(nil)
	if err != nil {
		t.Fatalf("RecipeCriteria: %v", err)
	}
	if criteria == nil {
		t.Fatal("RecipeCriteria returned nil on a nil error")
	}
	for _, tt := range []struct{ field, got, want string }{
		{"Service", criteria.Service, "eks"},
		{"Accelerator", criteria.Accelerator, "h100"},
		{"Intent", criteria.Intent, "training"},
		{"OS", criteria.OS, "ubuntu"},
		{"Platform", criteria.Platform, "kubeflow"},
	} {
		if tt.got != tt.want {
			t.Errorf("Criteria.%s = %q, want %q", tt.field, tt.got, tt.want)
		}
	}
	if criteria.Nodes != 8 {
		t.Errorf("Criteria.Nodes = %d, want 8", criteria.Nodes)
	}

	opts, err := cfg.RecipeResolveOptions()
	if err != nil {
		t.Fatalf("RecipeResolveOptions: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("RecipeResolveOptions = %d options, want 1 (profile only; no accounting mode set)", len(opts))
	}
}

// TestConfig_NilSafe is the contract the CLI depends on: --config is optional,
// so every derivation runs unconditionally and must return zero values rather
// than panicking when no document was supplied.
func TestConfig_NilSafe(t *testing.T) {
	t.Parallel()

	var cfg *aicr.Config

	if got := cfg.Unwrap(); got != nil {
		t.Errorf("Unwrap = %v, want nil", got)
	}
	if got := cfg.SnapshotPath(); got != "" {
		t.Errorf("SnapshotPath = %q, want empty", got)
	}
	if cfg.IsCriteriaStrict() {
		t.Error("IsCriteriaStrict = true, want false")
	}
	if _, ok := cfg.RecipeSource(); ok {
		t.Error("RecipeSource reported set on a nil Config")
	}

	verifyOpts, err := cfg.BundleVerifyOptions()
	if err != nil {
		t.Errorf("BundleVerifyOptions: %v", err)
	}
	if verifyOpts != (aicr.BundleVerifyOptions{}) {
		t.Errorf("BundleVerifyOptions = %+v, want zero value", verifyOpts)
	}

	resolveOpts, err := cfg.RecipeResolveOptions()
	if err != nil {
		t.Errorf("RecipeResolveOptions: %v", err)
	}
	if len(resolveOpts) != 0 {
		t.Errorf("RecipeResolveOptions = %d options, want 0", len(resolveOpts))
	}

	criteria, err := cfg.RecipeCriteria(nil)
	if err != nil {
		t.Errorf("RecipeCriteria: %v", err)
	}
	if criteria == nil {
		t.Error("RecipeCriteria returned nil; callers append to it unconditionally")
	}
}

// TestConfig_SectionAbsentDerivesZero covers the realistic case: a document
// that configures one section and not another. Deriving from the absent
// section must yield zero values rather than erroring, since the CLI derives
// both unconditionally regardless of what the document happens to set.
func TestConfig_SectionAbsentDerivesZero(t *testing.T) {
	t.Parallel()

	t.Run("recipe-only config derives empty verify options", func(t *testing.T) {
		t.Parallel()
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, recipeConfig))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		opts, err := cfg.BundleVerifyOptions()
		if err != nil {
			t.Fatalf("BundleVerifyOptions: %v", err)
		}
		if opts != (aicr.BundleVerifyOptions{}) {
			t.Errorf("BundleVerifyOptions = %+v, want zero value", opts)
		}
	})

	t.Run("verify-only config derives empty recipe inputs", func(t *testing.T) {
		t.Parallel()
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, verifyConfig))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if _, ok := cfg.RecipeSource(); ok {
			t.Error("RecipeSource reported set for a verify-only document")
		}
		if got := cfg.SnapshotPath(); got != "" {
			t.Errorf("SnapshotPath = %q, want empty", got)
		}
		opts, err := cfg.RecipeResolveOptions()
		if err != nil {
			t.Fatalf("RecipeResolveOptions: %v", err)
		}
		if len(opts) != 0 {
			t.Errorf("RecipeResolveOptions = %d options, want 0", len(opts))
		}
	})

	t.Run("snapshot input is read from spec.recipe.input", func(t *testing.T) {
		t.Parallel()
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, snapshotInputConfig))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got := cfg.SnapshotPath(); got != "./snapshot.yaml" {
			t.Errorf("SnapshotPath = %q, want ./snapshot.yaml", got)
		}
	})
}

// TestLoadConfig_Guards covers the pre-work rejections and confirms loader
// error codes survive rather than being flattened — the code is how a caller
// tells "no such file" from "this file is malformed".
func TestLoadConfig_Guards(t *testing.T) {
	t.Parallel()

	t.Run("nil context", func(t *testing.T) {
		t.Parallel()
		//nolint:staticcheck // SA1012: deliberately passing nil to test the guard.
		_, err := aicr.LoadConfig(nil, "aicr-config.yaml")
		wantInvalidRequest(t, err)
	})

	t.Run("empty source", func(t *testing.T) {
		t.Parallel()
		_, err := aicr.LoadConfig(context.Background(), "")
		wantInvalidRequest(t, err)
	})

	t.Run("structurally malformed document is rejected", func(t *testing.T) {
		t.Parallel()
		// Negative nodes, not a bogus criteria VALUE: membership is checked
		// at consumption against the provider registry, so an unknown value
		// loads cleanly by design (see
		// TestLoadConfig_ExternalCatalogCriteria). A negative count is
		// registry-independent and still fails here.
		_, err := aicr.LoadConfig(context.Background(), writeConfig(t,
			"apiVersion: aicr.run/v1alpha2\nkind: AICRConfig\nmetadata:\n  name: t\nspec:\n  recipe:\n    criteria:\n      nodes: -1\n"))
		if err == nil {
			t.Fatal("expected an error for a negative node count")
		}
		// Assert the CODE, not just failure: the code is how a caller tells a
		// malformed document from an unreachable one, and a regression that
		// flattened it to Internal would still satisfy a non-nil check.
		wantInvalidRequest(t, err)
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		_, err := aicr.LoadConfig(context.Background(), filepath.Join(t.TempDir(), "absent.yaml"))
		if err == nil {
			t.Fatal("expected an error for a missing config file")
		}
		var se *aicrerrors.StructuredError
		if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
			t.Errorf("error = %v, want code %v", err, aicrerrors.ErrCodeNotFound)
		}
	})
}

// TestWrapConfig covers the bridge for callers already holding a parsed
// document from pkg/config.
func TestWrapConfig(t *testing.T) {
	t.Parallel()

	if got := aicr.WrapConfig(nil); got != nil {
		t.Errorf("WrapConfig(nil) = %v, want nil", got)
	}

	loaded, err := aicr.LoadConfig(context.Background(), writeConfig(t, snapshotInputConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	rewrapped := aicr.WrapConfig(loaded.Unwrap())
	if rewrapped == nil {
		t.Fatal("WrapConfig returned nil for a non-nil document")
	}
	if got := rewrapped.SnapshotPath(); got != "./snapshot.yaml" {
		t.Errorf("SnapshotPath after rewrap = %q, want ./snapshot.yaml", got)
	}
}

const accountingConfig = `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: test
spec:
  recipe:
    profile: gpuStack=operator-managed
    configuration:
      slurm:
        accounting:
          mode: disabled
`

// TestConfig_RawAccessors covers the raw spec.recipe reads that exist for
// callers applying their own precedence before building options — the CLI
// overlaying an explicitly-set flag being the motivating case.
//
// They are deliberately redundant with RecipeResolveOptions, so this also
// asserts the two agree: a value readable raw must be the value folded into
// the options form.
func TestConfig_RawAccessors(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, accountingConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := cfg.RecipeProfile(); got != "gpuStack=operator-managed" {
		t.Errorf("RecipeProfile = %q, want gpuStack=operator-managed", got)
	}

	mode, set, err := cfg.RecipeAccountingMode()
	if err != nil {
		t.Fatalf("RecipeAccountingMode: %v", err)
	}
	if !set {
		t.Error("RecipeAccountingMode reported unset, but the document configures one")
	}
	if mode != "disabled" {
		t.Errorf("RecipeAccountingMode = %q, want disabled", mode)
	}

	// Both values present means the options form must carry both. This
	// package cannot see WHAT they carry — RecipeResolveOption is an opaque
	// func, so a count is the most an external test can assert, and a count
	// passes while a value is swapped or emptied. The folded values are
	// pinned in TestConfig_RecipeResolveOptions_FoldsValuesNotJustCount,
	// which applies them against the internal capture struct.
	opts, err := cfg.RecipeResolveOptions()
	if err != nil {
		t.Fatalf("RecipeResolveOptions: %v", err)
	}
	if len(opts) != 2 {
		t.Errorf("RecipeResolveOptions = %d options, want 2 (profile + accounting mode)", len(opts))
	}
}

// TestConfig_RawAccessorsNilSafe extends the nil-Config contract to the raw
// accessors. The CLI calls them unconditionally, before it knows whether
// --config was supplied.
func TestConfig_RawAccessorsNilSafe(t *testing.T) {
	t.Parallel()

	var cfg *aicr.Config

	if got := cfg.RecipeProfile(); got != "" {
		t.Errorf("RecipeProfile = %q, want empty", got)
	}
	mode, set, err := cfg.RecipeAccountingMode()
	if err != nil {
		t.Errorf("RecipeAccountingMode: %v", err)
	}
	if set || mode != "" {
		t.Errorf("RecipeAccountingMode = (%q, %v), want (\"\", false)", mode, set)
	}
}

// TestConfig_UnsetAccountingModeIsNotAnError separates "not configured" from
// "configured badly": a document with no accounting section reports unset
// rather than erroring, so a caller can append the options unconditionally.
func TestConfig_UnsetAccountingModeIsNotAnError(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, recipeConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	mode, set, err := cfg.RecipeAccountingMode()
	if err != nil {
		t.Fatalf("RecipeAccountingMode: %v", err)
	}
	if set || mode != "" {
		t.Errorf("RecipeAccountingMode = (%q, %v), want (\"\", false)", mode, set)
	}
}

// TestToInternalCriteria covers the facade-to-internal bridge, the counterpart
// to WrapCriteria. A caller that derived criteria from a config but must hand
// them to a pkg/recipe API needs a supported way across rather than
// reconstructing the enum-typed fields by hand.
func TestToInternalCriteria(t *testing.T) {
	t.Parallel()

	if got := aicr.ToInternalCriteria(nil); got != nil {
		t.Errorf("ToInternalCriteria(nil) = %v, want nil", got)
	}

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, recipeConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	derived, err := cfg.RecipeCriteria(nil)
	if err != nil {
		t.Fatalf("RecipeCriteria: %v", err)
	}

	internal := aicr.ToInternalCriteria(derived)
	if internal == nil {
		t.Fatal("ToInternalCriteria returned nil for a populated Criteria")
	}
	// Round-tripping must preserve the values: the enum types are what the
	// conversion exists for, so a silent drop would be invisible to a caller
	// until resolution picked the wrong overlay.
	if string(internal.Service) != derived.Service {
		t.Errorf("Service = %q, want %q", internal.Service, derived.Service)
	}
	if string(internal.Accelerator) != derived.Accelerator {
		t.Errorf("Accelerator = %q, want %q", internal.Accelerator, derived.Accelerator)
	}
	if string(internal.Intent) != derived.Intent {
		t.Errorf("Intent = %q, want %q", internal.Intent, derived.Intent)
	}
	if string(internal.OS) != derived.OS {
		t.Errorf("OS = %q, want %q", internal.OS, derived.OS)
	}
	if string(internal.Platform) != derived.Platform {
		t.Errorf("Platform = %q, want %q", internal.Platform, derived.Platform)
	}
	if internal.Nodes != derived.Nodes {
		t.Errorf("Nodes = %d, want %d", internal.Nodes, derived.Nodes)
	}
}

// TestLoadConfig_ExternalCatalogCriteria is the regression test for the
// external-catalog blocker: a config naming a criteria value that exists only
// in an external overlay could not be loaded at all.
//
// config.Load validated criteria against a nil registry — the EMBEDDED catalog
// — before spec.recipe.data could construct the provider whose registry
// defines the value. LoadConfig failed with "invalid service type", which made
// the documented provider-aware RecipeCriteria(client.CriteriaRegistry()) path
// unreachable: you could never get past loading to build the Client.
//
// Exercises the whole chain rather than the fix in isolation, because each hop
// is where it previously broke:
//
//	LoadConfig -> RecipeSource -> NewClient -> LoadCatalog -> RecipeCriteria
func TestLoadConfig_ExternalCatalogCriteria(t *testing.T) {
	t.Parallel()

	dataDir, err := filepath.Abs("testdata/external-catalog")
	if err != nil {
		t.Fatalf("resolve fixture path: %v", err)
	}
	cfgPath := writeConfig(t, `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: external
spec:
  recipe:
    data: `+dataDir+`
    criteria:
      service: ncp-review
      accelerator: h100
      intent: training
      os: ubuntu
`)

	// 1. Load must not reject a value the embedded catalog does not know.
	cfg, err := aicr.LoadConfig(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig rejected an external-catalog value: %v", err)
	}

	// 2. The document decides the recipe source.
	source, ok := cfg.RecipeSource()
	if !ok {
		t.Fatal("RecipeSource reported unset, but spec.recipe.data is populated")
	}
	client, err := aicr.NewClient(aicr.WithRecipeSource(source))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// 3. Loading the catalog seeds this provider's registry with the
	//    overlay-contributed value.
	if err = client.LoadCatalog(context.Background()); err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	// 4. Now the value parses, because the registry finally knows it.
	//
	// Strict mode is explicitly disabled first, and that is the point rather
	// than a workaround: strict mode exists to hide registry entries
	// contributed by an external overlay, so an external value is legal only
	// when it is off. The suite runs with AICR_CRITERIA_STRICT=1, which seeds
	// every registry strict — without this the test would assert the opposite
	// of what strict mode is for. The strict half is asserted below.
	reg := client.CriteriaRegistry()
	reg.SetStrict(false)

	criteria, err := cfg.RecipeCriteria(reg)
	if err != nil {
		t.Fatalf("RecipeCriteria against the provider registry: %v", err)
	}
	if criteria.Service != "ncp-review" {
		t.Errorf("Service = %q, want ncp-review", criteria.Service)
	}

	// Strict mode must still reject it: the value is real but externally
	// contributed, which is exactly what strict mode fences off.
	reg.SetStrict(true)
	if _, err = cfg.RecipeCriteria(reg); err == nil {
		t.Error("strict mode accepted an externally-contributed criteria value")
	}
}

// TestLoadConfig_ExternalCatalogCriteriaStillFailsClosed is the other half:
// deferring membership must not mean accepting anything. A value in no
// catalog — embedded or external — still fails, just at consumption rather
// than at load.
func TestLoadConfig_ExternalCatalogCriteriaStillFailsClosed(t *testing.T) {
	t.Parallel()

	dataDir, err := filepath.Abs("testdata/external-catalog")
	if err != nil {
		t.Fatalf("resolve fixture path: %v", err)
	}
	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: external
spec:
  recipe:
    data: `+dataDir+`
    criteria:
      service: not-in-any-catalog
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err = client.LoadCatalog(context.Background()); err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	if _, err = cfg.RecipeCriteria(client.CriteriaRegistry()); err == nil {
		t.Error("RecipeCriteria accepted a value in no catalog; deferral must not mean silent acceptance")
	}
}

const runtimeInventoryConfig = `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: test
spec:
  recipe:
    configuration:
      runtimeInventory:
        mode: disabled
`

// TestConfig_RuntimeInventoryMode covers the raw accessor and, more
// importantly, that RecipeResolveOptions projects the selection.
//
// The projection is the defect this guards. RecipeResolveOptions is the
// canonical config-to-options conversion for SDK callers, so a selection
// readable through the raw accessor but missing from the options form is
// silently dropped for anyone who configures it in a document rather than
// passing an option — with no error to notice.
func TestConfig_RuntimeInventoryMode(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, runtimeInventoryConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	mode, set, err := cfg.RecipeRuntimeInventoryMode()
	if err != nil {
		t.Fatalf("RecipeRuntimeInventoryMode: %v", err)
	}
	if !set {
		t.Fatal("RecipeRuntimeInventoryMode reported unset, but the document configures one")
	}
	if mode != "disabled" {
		t.Errorf("RecipeRuntimeInventoryMode = %q, want disabled", mode)
	}

	opts, err := cfg.RecipeResolveOptions()
	if err != nil {
		t.Fatalf("RecipeResolveOptions: %v", err)
	}
	if len(opts) == 0 {
		t.Fatal("RecipeResolveOptions returned no options; the runtime inventory selection was dropped")
	}

	// A nil Config must stay quiet rather than panic, matching the sibling
	// accessors.
	var nilCfg *aicr.Config
	if _, set, err := nilCfg.RecipeRuntimeInventoryMode(); err != nil || set {
		t.Errorf("nil Config: set=%v err=%v, want false/nil", set, err)
	}
}

// bundleConfig exercises every spec.bundle field BundleOptions projects, so a
// dropped mapping shows up as a wrong value rather than a smaller option list.
const bundleConfig = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  bundle:
    deployment:
      deployer: argocd
      repo: https://git.example.com/fleet
      appName: fleet-gpu
      vendorCharts: true
      set:
        - gpu-operator:driver.version=570.86.16
      dynamic:
        - gpu-operator:driver.enabled
    scheduling:
      systemNodeSelector:
        role: system
      systemNodeTolerations:
        - "node-role.kubernetes.io/control-plane:NoSchedule"
      acceleratedNodeSelector:
        role: gpu
      acceleratedNodeTolerations:
        - "nvidia.com/gpu:NoSchedule"
      draEvictionNodeLabel: nvidia.com/drain=true
      workloadGate: aicr.run/gate=busy:NoSchedule
      workloadSelector:
        app: training
      nodes: 12
      storageClass: fast
      sharedStorageClass: shared
    attestation:
      enabled: true
      certificateIdentityRegexp: ^https://github\.com/NVIDIA/.*$
      oidcDeviceFlow: true
      fulcioURL: https://fulcio.example.com
      rekorURL: https://rekor.example.com
`

// TestConfig_BundleOptions asserts the FOLDED VALUES, not that some options
// were produced. A count-only assertion passes when two fields are swapped,
// which is the whole failure mode this derivation can have.
func TestConfig_BundleOptions(t *testing.T) {
	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, bundleConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	opts, err := cfg.BundleOptions()
	if err != nil {
		t.Fatalf("BundleOptions: %v", err)
	}
	// A caller-supplied Config always wins over the flat fields (see
	// BundleOptions' "Two ways to supply the bundler configuration" godoc);
	// the derivation itself must never populate it, or a config-driven
	// caller's own per-field flag overrides on the flat fields below would
	// be silently ignored by bundlerConfig.
	if opts.Config != nil {
		t.Fatal("BundleOptions must not populate Config; it derives the flat fields only")
	}

	if got, want := string(opts.Deployer), "argocd"; got != want {
		t.Errorf("Deployer = %q, want %q", got, want)
	}
	if got, want := opts.Repo, "https://git.example.com/fleet"; got != want {
		t.Errorf("Repo = %q, want %q", got, want)
	}
	if got, want := opts.Nodes, 12; got != want {
		t.Errorf("Nodes = %d, want %d", got, want)
	}
	// Distinct values: a swap between these two same-typed fields is
	// invisible to the drift guard and to any fixture that reuses one value.
	if got, want := opts.StorageClass, "fast"; got != want {
		t.Errorf("StorageClass = %q, want %q", got, want)
	}
	if got, want := opts.SharedStorageClass, "shared"; got != want {
		t.Errorf("SharedStorageClass = %q, want %q", got, want)
	}
	if !opts.VendorCharts {
		t.Error("VendorCharts = false, want true")
	}
	if !opts.Attest {
		t.Error("Attest = false, want true")
	}
	if got, want := opts.CertIDRegexp, `^https://github\.com/NVIDIA/.*$`; got != want {
		t.Errorf("CertIDRegexp = %q, want %q", got, want)
	}
	if got, want := opts.SystemNodeSelector["role"], "system"; got != want {
		t.Errorf("SystemNodeSelector[role] = %q, want %q", got, want)
	}
	if got, want := opts.AcceleratedNodeSelector["role"], "gpu"; got != want {
		t.Errorf("AcceleratedNodeSelector[role] = %q, want %q", got, want)
	}
	if got, want := opts.WorkloadSelector["app"], "training"; got != want {
		t.Errorf("WorkloadSelector[app] = %q, want %q", got, want)
	}
	if got, want := opts.AppName, "fleet-gpu"; got != want {
		t.Errorf("AppName = %q, want %q", got, want)
	}
	if opts.DRAEvictionNodeLabel == nil || opts.DRAEvictionNodeLabel.Key != "nvidia.com/drain" ||
		opts.DRAEvictionNodeLabel.Value != "true" {

		t.Errorf("DRAEvictionNodeLabel = %+v, want nvidia.com/drain=true", opts.DRAEvictionNodeLabel)
	}
	if opts.WorkloadGate == nil || opts.WorkloadGate.Key != "aicr.run/gate" {
		t.Errorf("WorkloadGate = %+v, want key aicr.run/gate", opts.WorkloadGate)
	}
	// Assert IDENTITY, not just length. Both lists hold exactly one entry, so a
	// length check passes even when the two fields are swapped — and the drift
	// guard cannot catch a same-typed misroute either. The keys differ, so
	// comparing them is the only thing that pins the mapping.
	if len(opts.SystemNodeTolerations) != 1 ||
		opts.SystemNodeTolerations[0].Key != "node-role.kubernetes.io/control-plane" ||
		opts.SystemNodeTolerations[0].Effect != corev1.TaintEffectNoSchedule {

		t.Errorf("SystemNodeTolerations = %+v, want one node-role.kubernetes.io/control-plane:NoSchedule",
			opts.SystemNodeTolerations)
	}
	if len(opts.AcceleratedNodeTolerations) != 1 ||
		opts.AcceleratedNodeTolerations[0].Key != "nvidia.com/gpu" ||
		opts.AcceleratedNodeTolerations[0].Effect != corev1.TaintEffectNoSchedule {

		t.Errorf("AcceleratedNodeTolerations = %+v, want one nvidia.com/gpu:NoSchedule",
			opts.AcceleratedNodeTolerations)
	}
	if len(opts.ValueOverrides) == 0 {
		t.Error("ValueOverrides is empty; spec.bundle.deployment.set was dropped")
	}
	if len(opts.DynamicValues) == 0 {
		t.Error("DynamicValues is empty; spec.bundle.deployment.dynamic was dropped")
	}

	// This fixture DOES set rekorURL, which is an explicit Rekor v1 choice, so
	// the TUF signing config must be off. The default direction is covered by
	// TestConfig_BundleOptions_SigningTargetDefault.
	if opts.OIDCResolve.UseTUFSigningConfig {
		t.Error("UseTUFSigningConfig = true despite an explicit rekorURL")
	}

	// The four signing settings reach the attester, not the bundler.
	if !opts.OIDCResolve.Attest {
		t.Error("OIDCResolve.Attest = false, want true")
	}
	if !opts.OIDCResolve.DeviceFlow {
		t.Error("OIDCResolve.DeviceFlow = false, want true")
	}
	if got, want := opts.OIDCResolve.FulcioURL, "https://fulcio.example.com"; got != want {
		t.Errorf("OIDCResolve.FulcioURL = %q, want %q", got, want)
	}
	if got, want := opts.OIDCResolve.RekorURL, "https://rekor.example.com"; got != want {
		t.Errorf("OIDCResolve.RekorURL = %q, want %q", got, want)
	}
	// Keyless fixture: no KMS key. Setting both is rejected outright, which
	// TestConfig_BundleOptions_SigningModeExclusive covers.
	if opts.OIDCResolve.SigningKey != "" {
		t.Errorf("OIDCResolve.SigningKey = %q, want empty for a keyless config", opts.OIDCResolve.SigningKey)
	}

	// Fields with no spec.bundle counterpart stay zero so the caller, not the
	// derivation, decides them.
	if opts.Attester != nil {
		t.Error("Attester should stay nil; OIDCResolve drives signing")
	}
	if opts.OutputDir != "" || opts.Timeout != 0 || opts.BinaryAttestation != nil {
		t.Error("OutputDir/Timeout/BinaryAttestation should stay at zero values")
	}
}

// TestConfig_BundleOptions_Absent covers the paths the CLI hits when --config
// is absent or the document omits spec.bundle: derive unconditionally, get
// "nothing configured" rather than an error.
func TestConfig_BundleOptions_Absent(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		var cfg *aicr.Config
		opts, err := cfg.BundleOptions()
		if err != nil {
			t.Fatalf("BundleOptions on nil Config: %v", err)
		}
		if opts.Config != nil || opts.Deployer != "" || opts.OIDCResolve.Attest {
			t.Error("nil Config should derive an empty BundleOptions")
		}
	})

	t.Run("no spec.bundle", func(t *testing.T) {
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, verifyConfig))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		opts, err := cfg.BundleOptions()
		if err != nil {
			t.Fatalf("BundleOptions: %v", err)
		}
		// An unset draEvictionNodeLabel must stay nil so bundlerConfig leaves
		// the bundler's documented default in place rather than overwriting
		// it with a zero label.
		if opts.DRAEvictionNodeLabel != nil {
			t.Error("absent spec.bundle should not set a DRA eviction label")
		}
	})
}

// bundleValueConfig gives every BundleOptions/BundleInputOptions field a
// distinct sentinel, so a swap between two same-typed neighbors (e.g.
// StorageClass/SharedStorageClass, SystemNodeSelector/AcceleratedNodeSelector)
// shows up as a wrong value rather than passing on a fixture that reuses one
// value for both.
const bundleValueConfig = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
metadata:
  name: test
spec:
  bundle:
    input:
      recipe: ./bundle-recipe.yaml
    output:
      imageRefs: ./image-refs.txt
    deployment:
      deployer: argocd
      repo: https://git.example.com/gitops
      appName: bundle-app
      vendorCharts: true
    scheduling:
      systemNodeSelector:
        tier: system
      acceleratedNodeSelector:
        tier: accel
      storageClass: sc-main
      sharedStorageClass: sc-shared
      nodes: 12
`

// TestConfig_BundleOptions_FoldsValues asserts the FOLDED VALUES on the flat
// fields, including the two same-typed-neighbor pairs a swap could hide in.
func TestConfig_BundleOptions_FoldsValues(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, bundleValueConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got, err := cfg.BundleOptions()
	if err != nil {
		t.Fatalf("BundleOptions: %v", err)
	}

	if got.Repo != "https://git.example.com/gitops" {
		t.Errorf("Repo = %q, want the gitops URL", got.Repo)
	}
	if got.AppName != "bundle-app" {
		t.Errorf("AppName = %q, want bundle-app", got.AppName)
	}
	// Distinct values: a swap between these two same-typed fields is invisible
	// to the drift guard and to any fixture that reuses one value.
	if got.StorageClass != "sc-main" {
		t.Errorf("StorageClass = %q, want sc-main", got.StorageClass)
	}
	if got.SharedStorageClass != "sc-shared" {
		t.Errorf("SharedStorageClass = %q, want sc-shared", got.SharedStorageClass)
	}
	if got.SystemNodeSelector["tier"] != "system" {
		t.Errorf("SystemNodeSelector = %v, want tier=system", got.SystemNodeSelector)
	}
	if got.AcceleratedNodeSelector["tier"] != "accel" {
		t.Errorf("AcceleratedNodeSelector = %v, want tier=accel", got.AcceleratedNodeSelector)
	}
	if got.Nodes != 12 {
		t.Errorf("Nodes = %d, want 12", got.Nodes)
	}
	if !got.VendorCharts {
		t.Error("VendorCharts = false, want true")
	}
}

// TestConfig_BundleInputOptions_FoldsValues asserts the FOLDED VALUES on the
// caller-side fields, mirroring TestConfig_BundleOptions_FoldsValues for the
// half of spec.bundle BundleOptions does not carry.
func TestConfig_BundleInputOptions_FoldsValues(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, bundleValueConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got, err := cfg.BundleInputOptions()
	if err != nil {
		t.Fatalf("BundleInputOptions: %v", err)
	}
	if got.RecipePath != "./bundle-recipe.yaml" {
		t.Errorf("RecipePath = %q, want ./bundle-recipe.yaml", got.RecipePath)
	}
	if got.ImageRefsPath != "./image-refs.txt" {
		t.Errorf("ImageRefsPath = %q, want ./image-refs.txt", got.ImageRefsPath)
	}
}

// TestConfig_BundleInputOptions_NilConfig mirrors the nil-safety contract
// every Config derivation makes: a nil *Config yields the zero value, never
// an error.
func TestConfig_BundleInputOptions_NilConfig(t *testing.T) {
	t.Parallel()

	var cfg *aicr.Config
	got, err := cfg.BundleInputOptions()
	if err != nil {
		t.Fatalf("BundleInputOptions on nil Config: %v", err)
	}
	if got.RecipePath != "" || got.ImageRefsPath != "" || got.OutputTarget != nil ||
		got.OutputTargetRaw != "" || got.InsecureTLS || got.PlainHTTP {

		t.Errorf("got %+v, want zero value", got)
	}
}

// TestConfig_BundleOptions_SigningModeExclusive pins the rule the CLI enforces
// in validateSigningKeyExclusivity. ResolveAttesterLazy takes the KMS branch
// whenever SigningKey is non-empty, so accepting both would sign with the key
// while the document's keyless settings silently did nothing — the caller
// believing they signed against a named Fulcio.
func TestConfig_BundleOptions_SigningModeExclusive(t *testing.T) {
	const head = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  bundle:
    attestation:
      enabled: true
`
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"kms alone is fine", head + "      signingKey: gcpkms://projects/p/k\n", false},
		{"keyless alone is fine", head + "      oidcDeviceFlow: true\n      fulcioURL: https://fulcio.example.com\n", false},
		// oidcDeviceFlow is deliberately NOT checked eagerly here (see the
		// BundleOptions godoc): --oidc-device-flow=false must be able to
		// correct a config that sets both, and this layer runs BEFORE the
		// CLI's flag-over-config merge. validateSigningKeyExclusivity on the
		// CLI's merged opts still rejects the config-only combination — see
		// TestBundleCmd_SigningKeyFromConfig's rejectTests in pkg/cli.
		{"kms plus device flow is allowed at this layer", head + "      signingKey: gcpkms://projects/p/k\n      oidcDeviceFlow: true\n", false},
		{"kms plus fulcio is rejected", head + "      signingKey: gcpkms://projects/p/k\n      fulcioURL: https://fulcio.example.com\n", true},
		// rekorURL is NOT a conflict; it has its own rule against signingConfig.
		{"kms plus rekor is allowed", head + "      signingKey: gcpkms://projects/p/k\n      rekorURL: https://rekor.example.com\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, tt.body))
			if err != nil {
				if tt.wantErr {
					return // rejected even earlier, which is also fail-closed
				}
				t.Fatalf("LoadConfig: %v", err)
			}
			_, err = cfg.BundleOptions()
			if tt.wantErr && err == nil {
				t.Fatal("BundleOptions accepted mixed KMS and keyless signing settings")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("BundleOptions rejected a valid single-mode config: %v", err)
			}
		})
	}
}

// TestConfig_BundleOptions_SigningKeyTrimmed covers the YAML block-scalar case
// the CLI trims for: untrimmed, the key fails late in the KMS URI parser.
func TestConfig_BundleOptions_SigningKeyTrimmed(t *testing.T) {
	body := `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  bundle:
    attestation:
      enabled: true
      signingKey: "  gcpkms://projects/p/k  "
`
	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	opts, err := cfg.BundleOptions()
	if err != nil {
		t.Fatalf("BundleOptions: %v", err)
	}
	if got := opts.OIDCResolve.SigningKey; got != "gcpkms://projects/p/k" {
		t.Errorf("SigningKey = %q, want it trimmed", got)
	}
}

// TestConfig_BundleOptions_SigningTargetDefault pins the transparency target,
// which is invisible in the derived value and only shows up in where a
// signature lands.
//
// transparencyForOptions falls through to NewRekorPolicy("") — public Rekor
// v1 — when SigningConfig, SigningConfigPath, UseTUFSigningConfig and RekorURL
// are all unset. The CLI never hits that: signingTargetFromFlags defaults
// useTUF to true with no --rekor-url. Without the same default here, a
// config-driven SDK sign records to the legacy log while the identical CLI
// invocation records to v2 (#1650), silently.
func TestConfig_BundleOptions_SigningTargetDefault(t *testing.T) {
	const head = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  bundle:
    attestation:
      enabled: true
`
	tests := []struct {
		name    string
		body    string
		wantTUF bool
	}{
		{"no rekorURL takes the TUF signing config (v2)", head, true},
		{"explicit rekorURL is a deliberate v1 choice", head + "      rekorURL: https://rekor.example.com\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, tt.body))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			opts, err := cfg.BundleOptions()
			if err != nil {
				t.Fatalf("BundleOptions: %v", err)
			}
			if opts.OIDCResolve.UseTUFSigningConfig != tt.wantTUF {
				t.Errorf("UseTUFSigningConfig = %v, want %v", opts.OIDCResolve.UseTUFSigningConfig, tt.wantTUF)
			}
		})
	}
}

// snapshotAgentConfig exercises every spec.snapshot field SnapshotAgentConfig
// projects, with a value per field so a dropped or swapped mapping shows up as
// a wrong value rather than a shorter struct.
const snapshotAgentConfig = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  snapshot:
    agent:
      namespace: aicr-system
      image: nvcr.io/nvidia/aicr:v1.2.3
      imagePullSecrets:
        - regcred
      jobName: snap-job
      serviceAccountName: snap-sa
      nodeSelector:
        role: gpu
      tolerations:
        - "nvidia.com/gpu:NoSchedule"
      requireGpu: true
      runtimeClassName: nvidia
      os: ubuntu
      requests: cpu=100m,memory=256Mi
      limits: cpu=2,memory=1Gi
    execution:
      timeout: 12m
      maxNodesPerEntry: 7
    output:
      path: ./snap.yaml
      template: ./tmpl.tmpl
`

func TestConfig_SnapshotAgentConfig(t *testing.T) {
	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, snapshotAgentConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	ac, present, err := cfg.SnapshotAgentConfig()
	if err != nil {
		t.Fatalf("SnapshotAgentConfig: %v", err)
	}
	if ac == nil {
		t.Fatal("SnapshotAgentConfig returned nil for a populated spec.snapshot")
	}
	if !present {
		t.Error("present = false, want true for a document with spec.snapshot")
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Namespace", ac.Namespace, "aicr-system"},
		{"Image", ac.Image, "nvcr.io/nvidia/aicr:v1.2.3"},
		{"JobName", ac.JobName, "snap-job"},
		{"ServiceAccountName", ac.ServiceAccountName, "snap-sa"},
		{"RequireGPU", ac.RequireGPU, true},
		{"RuntimeClassName", ac.RuntimeClassName, "nvidia"},
		{"OS", ac.OS, "ubuntu"},
		{"MaxNodesPerEntry", ac.MaxNodesPerEntry, 7},
		{"Timeout", ac.Timeout, 12 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(ac.ImagePullSecrets) != 1 || ac.ImagePullSecrets[0] != "regcred" {
		t.Errorf("ImagePullSecrets = %v, want [regcred]", ac.ImagePullSecrets)
	}
	if ac.NodeSelector["role"] != "gpu" {
		t.Errorf("NodeSelector[role] = %q, want gpu", ac.NodeSelector["role"])
	}
	if len(ac.Tolerations) != 1 {
		t.Errorf("Tolerations = %v, want 1 entry", ac.Tolerations)
	}
	// Raw "name=quantity,..." strings; Resolve does not parse them, so a
	// dropped parse would surface as an empty ResourceList, not an error.
	if ac.Requests.Cpu().String() != "100m" {
		t.Errorf("Requests.cpu = %v, want 100m", ac.Requests.Cpu())
	}
	if ac.Limits.Memory().String() != "1Gi" {
		t.Errorf("Limits.memory = %v, want 1Gi", ac.Limits.Memory())
	}
}

// TestConfig_SnapshotAgentConfig_CleanupIsInverted covers the same trap
// spec.validate has: config says "noCleanup", AgentConfig says "Cleanup".
func TestConfig_SnapshotAgentConfig_CleanupIsInverted(t *testing.T) {
	for _, tt := range []struct {
		noCleanup   string
		wantCleanup bool
	}{{"true", false}, {"false", true}} {
		t.Run("noCleanup="+tt.noCleanup, func(t *testing.T) {
			body := "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nspec:\n  snapshot:\n    execution:\n      noCleanup: " + tt.noCleanup + "\n"
			cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, body))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			ac, _, err := cfg.SnapshotAgentConfig()
			if err != nil {
				t.Fatalf("SnapshotAgentConfig: %v", err)
			}
			if ac.Cleanup != tt.wantCleanup {
				t.Errorf("Cleanup = %v, want %v (noCleanup: %s)", ac.Cleanup, tt.wantCleanup, tt.noCleanup)
			}
		})
	}
}

// TestConfig_SnapshotAgentConfig_PrivilegedDefaultsTrue is the subtler of the
// two traps. The resolved field is a pointer so unset stays distinct from an
// explicit false, and the collector's default is privileged — dereferencing
// nil to false would silently drop privileges and surface as missing data
// rather than an error.
func TestConfig_SnapshotAgentConfig_PrivilegedDefaultsTrue(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{"unset defaults to privileged", "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nspec:\n  snapshot:\n    agent:\n      namespace: n\n", true},
		{"explicit false is preserved", "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nspec:\n  snapshot:\n    execution:\n      privileged: false\n", false},
		{"explicit true is preserved", "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nspec:\n  snapshot:\n    execution:\n      privileged: true\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, tt.body))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			ac, _, err := cfg.SnapshotAgentConfig()
			if err != nil {
				t.Fatalf("SnapshotAgentConfig: %v", err)
			}
			if ac.Privileged != tt.want {
				t.Errorf("Privileged = %v, want %v", ac.Privileged, tt.want)
			}
		})
	}
}

// TestConfig_SnapshotAgentConfig_Absent covers BOTH routes to "no snapshot
// configuration", which fail differently, plus the presence bool that lets a
// caller tell them apart from "the document decided every field, silently."
//
// A nil Config is caught by the receiver check. A document that simply omits
// spec.snapshot is NOT: Resolve() returns a non-nil SnapshotResolved for an
// absent section, so without the section-presence check the derivation falls
// through and applies the in-section defaults — Cleanup and Privileged both
// true — to a document that never opted into snapshot configuration. Both
// routes must report present=false, and a document that DOES carry the
// section — even one that sets nothing inside it — must report present=true,
// so a caller (like the snapshot CLI) can apply its own defaults only in the
// former case rather than trusting the zero AgentConfig as configured intent.
func TestConfig_SnapshotAgentConfig_Absent(t *testing.T) {
	t.Run("document omits spec.snapshot", func(t *testing.T) {
		body := `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  verify:
    policy:
      minTrustLevel: max
`
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, body))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		ac, present, err := cfg.SnapshotAgentConfig()
		if err != nil {
			t.Fatalf("SnapshotAgentConfig: %v", err)
		}
		if ac == nil {
			t.Fatal("got nil; an absent section must still derive a zero value")
		}
		if present {
			t.Error("present = true, want false for a document with no spec.snapshot")
		}
		if ac.Cleanup || ac.Privileged {
			t.Errorf("Cleanup=%v Privileged=%v; an absent section must not apply in-section defaults",
				ac.Cleanup, ac.Privileged)
		}
	})

	t.Run("document has an empty spec.snapshot", func(t *testing.T) {
		body := "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nspec:\n  snapshot: {}\n"
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, body))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		ac, present, err := cfg.SnapshotAgentConfig()
		if err != nil {
			t.Fatalf("SnapshotAgentConfig: %v", err)
		}
		if !present {
			t.Error("present = false, want true: spec.snapshot is present, even though it sets nothing")
		}
		// A present-but-silent section DOES apply in-section defaults — the
		// opposite of the fully-absent case above.
		if !ac.Cleanup || !ac.Privileged {
			t.Errorf("Cleanup=%v Privileged=%v; a present-but-silent section must apply in-section defaults",
				ac.Cleanup, ac.Privileged)
		}
	})

	var cfg *aicr.Config
	ac, present, err := cfg.SnapshotAgentConfig()
	if err != nil {
		t.Fatalf("SnapshotAgentConfig on nil Config: %v", err)
	}
	if ac == nil {
		t.Fatal("got nil; a nil Config must still derive a zero-value AgentConfig")
	}
	if present {
		t.Error("present = true, want false for a nil Config")
	}
	// AgentConfig contains slices/maps, so compare the fields a derivation
	// would have populated rather than the struct as a whole.
	if ac.Namespace != "" || ac.Image != "" || ac.Cleanup || ac.Privileged ||
		ac.Timeout != 0 || len(ac.ImagePullSecrets) != 0 || len(ac.NodeSelector) != 0 {

		t.Errorf("got %+v, want a zero value", ac)
	}
}

// TestConfig_SnapshotAgentConfig_OSIsParsed pins that OS goes through the
// criteria registry rather than being copied. An unparsed "Talos" misses the
// agent's exact "talos" match and selects incompatible host mounts, and an
// undocumented value would travel instead of erroring.
func TestConfig_SnapshotAgentConfig_OSIsParsed(t *testing.T) {
	head := "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nspec:\n  snapshot:\n    agent:\n      os: "
	t.Run("mixed case is normalized", func(t *testing.T) {
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, head+"Talos\n"))
		if err != nil {
			t.Skipf("loader rejected the value before the derivation: %v", err)
		}
		ac, _, err := cfg.SnapshotAgentConfig()
		if err != nil {
			t.Fatalf("SnapshotAgentConfig: %v", err)
		}
		if ac.OS != "talos" {
			t.Errorf("OS = %q, want %q", ac.OS, "talos")
		}
	})
	t.Run("undocumented value is rejected", func(t *testing.T) {
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, head+"plan9\n"))
		if err != nil {
			return // rejected earlier, also fail-closed
		}
		if _, _, err := cfg.SnapshotAgentConfig(); err == nil {
			t.Fatal("SnapshotAgentConfig accepted an undocumented OS value")
		}
	})
}

const snapshotOutputConfig = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
metadata:
  name: test
spec:
  snapshot:
    output:
      path: ./snap-out.yaml
      format: json
      template: ./tmpl.tmpl
`

func TestConfig_SnapshotOutputOptions(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, snapshotOutputConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got, err := cfg.SnapshotOutputOptions()
	if err != nil {
		t.Fatalf("SnapshotOutputOptions: %v", err)
	}
	// Distinct values on purpose: a swap between two same-typed fields is
	// invisible when the fixture reuses one value.
	if got.Path != "./snap-out.yaml" {
		t.Errorf("Path = %q, want ./snap-out.yaml", got.Path)
	}
	if got.Format != "json" {
		t.Errorf("Format = %q, want json", got.Format)
	}
	if got.Template != "./tmpl.tmpl" {
		t.Errorf("Template = %q, want ./tmpl.tmpl", got.Template)
	}
}

func TestConfig_SnapshotOutputOptions_Absent(t *testing.T) {
	t.Parallel()

	var cfg *aicr.Config
	got, err := cfg.SnapshotOutputOptions()
	if err != nil {
		t.Fatalf("SnapshotOutputOptions on nil Config: %v", err)
	}
	if got != (aicr.SnapshotOutputOptions{}) {
		t.Errorf("got %+v, want zero value", got)
	}
}

const cncfEvidenceConfig = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
metadata:
  name: test
spec:
  validate:
    evidence:
      cncf:
        dir: ./cncf-evidence
        cncfSubmission: true
        features:
          - dra-support
          - secure-access
`

// TestConfig_CNCFEvidenceOptions pins the three fields CNCFEvidenceOptions
// projects, mirroring TestConfig_SnapshotOutputOptions for the sibling
// caller-consumed (not Client-consumed) derivation.
func TestConfig_CNCFEvidenceOptions(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, cncfEvidenceConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got, err := cfg.CNCFEvidenceOptions()
	if err != nil {
		t.Fatalf("CNCFEvidenceOptions: %v", err)
	}
	if got.Dir != "./cncf-evidence" {
		t.Errorf("Dir = %q, want ./cncf-evidence", got.Dir)
	}
	if !got.CNCFSubmission {
		t.Error("CNCFSubmission = false, want true")
	}
	if want := []string{"dra-support", "secure-access"}; !slices.Equal(got.Features, want) {
		t.Errorf("Features = %v, want %v", got.Features, want)
	}
}

func TestConfig_CNCFEvidenceOptions_Absent(t *testing.T) {
	t.Parallel()

	var cfg *aicr.Config
	got, err := cfg.CNCFEvidenceOptions()
	if err != nil {
		t.Fatalf("CNCFEvidenceOptions on nil Config: %v", err)
	}
	if got.Dir != "" || got.CNCFSubmission || got.Features != nil {
		t.Errorf("got %+v, want zero value", got)
	}
}

const recipeOutputConfig = `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: test
spec:
  recipe:
    output:
      path: ./out-recipe.yaml
      format: json
`

func TestConfig_RecipeOutputOptions(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, recipeOutputConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got := cfg.RecipeOutputOptions()
	// Distinct values on purpose: a swap between two same-typed fields is
	// invisible when the fixture reuses one value.
	if got.Path != "./out-recipe.yaml" {
		t.Errorf("Path = %q, want ./out-recipe.yaml", got.Path)
	}
	if got.Format != "json" {
		t.Errorf("Format = %q, want json", got.Format)
	}
}

func TestConfig_RecipeOutputOptions_Absent(t *testing.T) {
	t.Parallel()

	var cfg *aicr.Config
	got := cfg.RecipeOutputOptions()
	if got != (aicr.RecipeOutputOptions{}) {
		t.Errorf("got %+v, want zero value", got)
	}
}

const evidenceAttestationConfig = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
metadata:
  name: test
spec:
  validate:
    evidence:
      attestation:
        out: ./evidence
        bom: ./sbom.cdx.json
        push: ghcr.io/example/evidence:run-1
        plainHTTP: true
        insecureTLS: true
`

// TestConfig_EvidenceAttestationOptions covers the five fields that project
// and, just as importantly, the four that must NOT: Commit and OIDCResolve are
// per-invocation, and NoSign/Full are command-line-only because both weaken a
// run and a checked-in file must not be able to do that silently.
func TestConfig_EvidenceAttestationOptions(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, evidenceAttestationConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	opts, ok, err := cfg.EvidenceAttestationOptions()
	if err != nil {
		t.Fatalf("EvidenceAttestationOptions: %v", err)
	}
	if !ok {
		t.Fatal("reported not configured, but the document sets attestation.out")
	}

	if opts.OutDir != "./evidence" {
		t.Errorf("OutDir = %q, want ./evidence", opts.OutDir)
	}
	if opts.BOMPath != "./sbom.cdx.json" {
		t.Errorf("BOMPath = %q, want ./sbom.cdx.json", opts.BOMPath)
	}
	if opts.Push != "ghcr.io/example/evidence:run-1" {
		t.Errorf("Push = %q, want ghcr.io/example/evidence:run-1", opts.Push)
	}
	if !opts.PlainHTTP {
		t.Error("PlainHTTP = false, want true")
	}
	if !opts.InsecureTLS {
		t.Error("InsecureTLS = false, want true")
	}

	// The un-projected half. NoSign and Full staying false is a control, not
	// an oversight: a document that could flip either would weaken signing or
	// redaction without showing up as a flag in the run's command line.
	if opts.NoSign {
		t.Error("NoSign = true, but it has no spec counterpart and must stay command-line-only")
	}
	if opts.Full {
		t.Error("Full = true, but it has no spec counterpart and must stay command-line-only")
	}
	if opts.Commit != "" {
		t.Errorf("Commit = %q, want empty; it names the running binary, not the document", opts.Commit)
	}
	if opts.OIDCResolve.IdentityToken != "" {
		t.Error("OIDCResolve.IdentityToken populated; a signing token is a secret " +
			"and must never come from a version-controlled document")
	}
}

// TestConfig_EvidenceAttestationOptions_OutIsTheGate pins the spec's own rule:
// out enables the path, and an out-less section stays off no matter how much
// else is filled in. Without the gate a document that set only `push` would
// derive ok=true and hand EmitRecipeEvidence an empty OutDir, turning a
// half-written section into an ErrCodeInvalidRequest at emit time instead of a
// clean "not configured".
func TestConfig_EvidenceAttestationOptions_OutIsTheGate(t *testing.T) {
	t.Parallel()

	const outless = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
metadata:
  name: test
spec:
  validate:
    evidence:
      attestation:
        push: ghcr.io/example/evidence:run-1
        plainHTTP: true
`
	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, outless))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	opts, ok, err := cfg.EvidenceAttestationOptions()
	if err != nil {
		t.Fatalf("EvidenceAttestationOptions: %v", err)
	}
	if ok {
		t.Error("reported configured with an empty out; the spec says out is the enable gate")
	}
	// out is the ENABLE gate, not a zeroing gate: OutDir stays empty, but the
	// rest of the section that was set still populates. See
	// TestConfig_EvidenceAttestationOptions_PopulatesWithoutOut for why —
	// zeroing the whole struct here would drop a caller-supplied out (e.g.
	// the CLI's --emit-attestation flag) from ever seeing push/plainHTTP.
	if opts.OutDir != "" {
		t.Errorf("OutDir = %q, want empty when out is unset", opts.OutDir)
	}
	if opts.Push != "ghcr.io/example/evidence:run-1" {
		t.Errorf("Push = %q, want ghcr.io/example/evidence:run-1 even though out is unset", opts.Push)
	}
	if !opts.PlainHTTP {
		t.Error("PlainHTTP = false, want true even though out is unset")
	}
	if opts.BOMPath != "" {
		t.Errorf("BOMPath = %q, want empty; the document never set bom", opts.BOMPath)
	}
	if opts.InsecureTLS {
		t.Error("InsecureTLS = true, want false; the document never set insecureTLS")
	}
}

// TestConfig_EvidenceAttestationOptions_PopulatesWithoutOut is the regression
// test for a config that enables bom/push but leaves out unset. out can
// arrive from somewhere other than this document:
// pkg/cli/validate_evidence.go's buildRecipeEvidenceConfig resolves it with
// stringFlagOrConfig(cmd, "emit-attestation", att.OutDir), so the
// --emit-attestation flag can supply out while bom/push come from config.
// Before this fix, an empty out returned a fully zeroed EvidenceOptions,
// which silently dropped bom/push in exactly that case. ok must still be
// false — THIS document did not enable the path — but BOMPath and Push must
// survive so a flag-supplied out does not lose them.
func TestConfig_EvidenceAttestationOptions_PopulatesWithoutOut(t *testing.T) {
	t.Parallel()

	const noOut = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
metadata:
  name: test
spec:
  validate:
    evidence:
      attestation:
        bom: ./sbom.cdx.json
        push: ghcr.io/example/evidence:run-1
`
	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, noOut))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	opts, ok, err := cfg.EvidenceAttestationOptions()
	if err != nil {
		t.Fatalf("EvidenceAttestationOptions: %v", err)
	}
	if ok {
		t.Error("reported configured with an empty out; the spec says out is the enable gate")
	}
	if opts.OutDir != "" {
		t.Errorf("OutDir = %q, want empty when out is unset", opts.OutDir)
	}
	if opts.BOMPath != "./sbom.cdx.json" {
		t.Errorf("BOMPath = %q, want ./sbom.cdx.json to survive an empty out", opts.BOMPath)
	}
	if opts.Push != "ghcr.io/example/evidence:run-1" {
		t.Errorf("Push = %q, want ghcr.io/example/evidence:run-1 to survive an empty out", opts.Push)
	}
}

// TestConfig_EvidenceAttestationOptions_Absent covers every route to "no
// attestation configured": no document, no spec.validate, and a spec.validate
// with no evidence section. All three must be a quiet false, not an error —
// the CLI derives unconditionally, before it knows whether --config was given.
func TestConfig_EvidenceAttestationOptions_Absent(t *testing.T) {
	t.Parallel()

	t.Run("nil config", func(t *testing.T) {
		t.Parallel()
		var cfg *aicr.Config
		_, ok, err := cfg.EvidenceAttestationOptions()
		if err != nil {
			t.Fatalf("EvidenceAttestationOptions on nil Config: %v", err)
		}
		if ok {
			t.Error("reported configured for a nil Config")
		}
	})

	t.Run("no spec.validate", func(t *testing.T) {
		t.Parallel()
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, recipeConfig))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		_, ok, err := cfg.EvidenceAttestationOptions()
		if err != nil {
			t.Fatalf("EvidenceAttestationOptions: %v", err)
		}
		if ok {
			t.Error("reported configured for a document with no spec.validate")
		}
	})

	t.Run("spec.validate without evidence", func(t *testing.T) {
		t.Parallel()
		const noEvidence = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
metadata:
  name: test
spec:
  validate:
    execution:
      timeout: 5m
`
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, noEvidence))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		_, ok, err := cfg.EvidenceAttestationOptions()
		if err != nil {
			t.Fatalf("EvidenceAttestationOptions: %v", err)
		}
		if ok {
			t.Error("reported configured for a spec.validate with no evidence section")
		}
	})
}

const validateValueConfig = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
metadata:
  name: test
spec:
  validate:
    agent:
      namespace: val-ns
      image: example.com/val:1
      jobName: val-job
      serviceAccountName: val-sa
      requireGpu: true
    execution:
      phases: [deployment]
      failFast: true
      noCluster: true
      noCleanup: true
      timeout: 7m
`

func TestConfig_ValidateSettings_FoldsValues(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, validateValueConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got, present, err := cfg.ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	if !present {
		t.Error("present = false, want true: spec.validate is present in this document")
	}

	for _, tt := range []struct{ field, got, want string }{
		{"Namespace", got.Namespace, "val-ns"},
		{"Image", got.Image, "example.com/val:1"},
		{"JobName", got.JobName, "val-job"},
		{"ServiceAccountName", got.ServiceAccountName, "val-sa"},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.field, tt.got, tt.want)
		}
	}
	if !got.RequireGPU {
		t.Error("RequireGPU = false, want true")
	}
	if !got.NoCluster {
		t.Error("NoCluster = false, want true")
	}
	// INVERTED: config says noCleanup: true, so Cleanup must be false.
	if got.Cleanup {
		t.Error("Cleanup = true, want false (noCleanup: true inverts)")
	}
	if got.FailFast == nil || !*got.FailFast {
		t.Error("FailFast not folded as true")
	}
	if got.Timeout == nil || *got.Timeout != 7*time.Minute {
		t.Errorf("Timeout = %v, want 7m", got.Timeout)
	}
	if len(got.Phases) != 1 || got.Phases[0] != aicr.PhaseDeployment {
		t.Errorf("Phases = %v, want [deployment]", got.Phases)
	}
}

// TestConfig_ValidateSettings_PresenceBool covers BOTH routes to "no validate
// configuration", which fail differently, plus the presence bool that lets a
// caller tell them apart from "the document decided every field, silently."
// Mirrors TestConfig_SnapshotAgentConfig_Absent exactly, for the sibling
// derivation.
//
// A nil Config is caught by the receiver check. A document that simply omits
// spec.validate is NOT: Resolve() returns a non-nil ValidateResolved for an
// absent section, so without the section-presence check the derivation falls
// through and applies the in-section default — Cleanup: true (NoCleanup's
// zero value inverted) — to a document that never opted into validate
// configuration. Both routes must report present=false, and a document that
// DOES carry the section — even one that sets nothing inside it — must
// report present=true, so a caller (like the validate CLI) applies its own
// default only in the former case rather than trusting the zero
// ValidateSettings as configured intent.
//
// This is also the direct regression guard for the security defect fix round
// 2 found: pre-fix, a nil Config yielded Cleanup=false while a present-but-
// silent section yielded Cleanup=true — two spellings of "no opinion"
// disagreeing on whether the CLI should leave cluster-admin RBAC and Jobs
// behind. Post-fix, a correct caller reads present rather than trusting
// Cleanup directly, so both routes agree on the EFFECTIVE decision (clean
// up): present=false tells the caller to substitute its own true default
// (asserted below), and present=true here already carries Cleanup=true from
// the config's own zero-value default.
func TestConfig_ValidateSettings_PresenceBool(t *testing.T) {
	t.Parallel()

	t.Run("document omits spec.validate", func(t *testing.T) {
		t.Parallel()
		body := `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  verify:
    policy:
      minTrustLevel: max
`
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, body))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		got, present, err := cfg.ValidateSettings()
		if err != nil {
			t.Fatalf("ValidateSettings: %v", err)
		}
		if present {
			t.Error("present = true, want false for a document with no spec.validate")
		}
		if got.Cleanup {
			t.Error("Cleanup = true; an absent section must not apply in-section defaults")
		}
	})

	t.Run("document has an empty spec.validate", func(t *testing.T) {
		t.Parallel()
		body := "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nspec:\n  validate: {}\n"
		cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, body))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		got, present, err := cfg.ValidateSettings()
		if err != nil {
			t.Fatalf("ValidateSettings: %v", err)
		}
		if !present {
			t.Error("present = false, want true: spec.validate is present, even though it sets nothing")
		}
		// A present-but-silent section DOES apply the in-section default — the
		// opposite of the fully-absent case above, and the exact disagreement the
		// fix closes.
		if !got.Cleanup {
			t.Error("Cleanup = false; a present-but-silent section must apply the in-section default (clean up)")
		}
	})

	t.Run("nil Config", func(t *testing.T) {
		t.Parallel()
		var cfg *aicr.Config
		got, present, err := cfg.ValidateSettings()
		if err != nil {
			t.Fatalf("ValidateSettings on nil Config: %v", err)
		}
		if present {
			t.Error("present = true, want false for a nil Config")
		}
		if got.Namespace != "" || got.Cleanup || got.NoCluster || len(got.Phases) != 0 {
			t.Errorf("got %+v, want a zero value", got)
		}
	})
}

const validateInputConfig = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
metadata:
  name: test
spec:
  validate:
    input:
      recipe: ./r.yaml
      snapshot: ./s.yaml
    execution:
      failOnError: false
`

func TestConfig_ValidateInputOptions_FoldsValues(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, validateInputConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got, err := cfg.ValidateInputOptions()
	if err != nil {
		t.Fatalf("ValidateInputOptions: %v", err)
	}

	if got.RecipePath != "./r.yaml" {
		t.Errorf("RecipePath = %q, want ./r.yaml", got.RecipePath)
	}
	if got.SnapshotPath != "./s.yaml" {
		t.Errorf("SnapshotPath = %q, want ./s.yaml", got.SnapshotPath)
	}
	// The pointer itself is the assertion: FailOnError must be a non-nil
	// pointer to false, distinguishing "explicitly false" from "config said
	// nothing" (nil) — the whole reason the field is a pointer.
	if got.FailOnError == nil {
		t.Fatal("FailOnError = nil, want a non-nil pointer to false")
	}
	if *got.FailOnError {
		t.Error("FailOnError = true, want false")
	}
}

func TestConfig_ValidateInputOptions_NilConfig(t *testing.T) {
	t.Parallel()

	var cfg *aicr.Config
	got, err := cfg.ValidateInputOptions()
	if err != nil {
		t.Fatalf("ValidateInputOptions on nil Config: %v", err)
	}
	if got.RecipePath != "" || got.SnapshotPath != "" || got.FailOnError != nil {
		t.Errorf("got %+v, want zero value", got)
	}
}
