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

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/urfave/cli/v3"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

func TestCatalogVersion(t *testing.T) {
	tests := []struct {
		name string
		cat  *catalog.ValidatorCatalog
		want string
	}{
		{"nil catalog", nil, ""},
		{"no metadata", &catalog.ValidatorCatalog{}, ""},
		{"metadata with version", &catalog.ValidatorCatalog{
			Metadata: &catalog.CatalogMetadata{Version: "v1.4.2"},
		}, "v1.4.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := catalogVersion(tt.cat); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDedupValidatorImages(t *testing.T) {
	tests := []struct {
		name string
		cat  *catalog.ValidatorCatalog
		want []string
	}{
		{"nil catalog", nil, nil},
		{"no validators", &catalog.ValidatorCatalog{}, nil},
		{
			name: "dedupes by image preserving order",
			cat: &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
				{Name: "a", Image: "ghcr.io/x/deployment:v1"},
				{Name: "b", Image: "ghcr.io/x/deployment:v1"},
				{Name: "c", Image: "ghcr.io/x/performance:v1"},
				{Name: "d", Image: "ghcr.io/x/conformance:v1"},
			}},
			want: []string{
				"ghcr.io/x/deployment:v1",
				"ghcr.io/x/performance:v1",
				"ghcr.io/x/conformance:v1",
			},
		},
		{
			name: "skips entries with empty image",
			cat: &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
				{Name: "a", Image: ""},
				{Name: "b", Image: "ghcr.io/x/deployment:v1"},
			}},
			want: []string{"ghcr.io/x/deployment:v1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupValidatorImages(tt.cat)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidatorImagesForPredicate(t *testing.T) {
	cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
		{Name: "a", Image: "ghcr.io/x/deployment:v1"},
		{Name: "b", Image: "ghcr.io/x/deployment:v1"},
		{Name: "c", Image: "ghcr.io/x/performance:v1"},
	}}
	got := validatorImagesForPredicate(cat)
	want := []attestation.ValidatorImage{
		{Image: "ghcr.io/x/deployment:v1"},
		{Image: "ghcr.io/x/performance:v1"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if validatorImagesForPredicate(nil) != nil {
		t.Errorf("nil catalog should produce nil slice")
	}
}

func TestBuildPointerInputs_UnsignedLeavesSignerNil(t *testing.T) {
	bundle := &attestation.Bundle{RecipeName: "x"}
	in := buildPointerInputs(bundle, signPushOutcome{})
	if in.Signer != nil {
		t.Errorf("unsigned outcome should leave Signer nil; got %+v", in.Signer)
	}
}

func TestBuildPointerInputs_SignedWithRekorIndex(t *testing.T) {
	bundle := &attestation.Bundle{RecipeName: "x"}
	in := buildPointerInputs(bundle, signPushOutcome{
		Sign: &attestation.SignResult{
			Identity:      "u@x",
			Issuer:        "iss",
			RekorLogIndex: 42,
		},
	})
	if in.Signer == nil {
		t.Fatalf("signed outcome should produce non-nil Signer")
	}
	if in.Signer.Identity != "u@x" || in.Signer.Issuer != "iss" {
		t.Errorf("Identity/Issuer mismatch: %+v", in.Signer)
	}
	if in.Signer.RekorLogIndex == nil || *in.Signer.RekorLogIndex != 42 {
		t.Errorf("RekorLogIndex = %v, want *int64(42)", in.Signer.RekorLogIndex)
	}
}

func TestBuildPointerInputs_SignedWithoutRekorLeavesIndexNil(t *testing.T) {
	bundle := &attestation.Bundle{RecipeName: "x"}
	in := buildPointerInputs(bundle, signPushOutcome{
		Sign: &attestation.SignResult{Identity: "u@x", Issuer: "iss", RekorLogIndex: 0},
	})
	if in.Signer == nil {
		t.Fatalf("signed outcome should produce non-nil Signer")
	}
	if in.Signer.RekorLogIndex != nil {
		t.Errorf("zero Rekor index should yield nil pointer; got *%d", *in.Signer.RekorLogIndex)
	}
}

// runEvidenceCmd builds a minimal cli.Command that exposes the
// flag-set buildRecipeEvidenceConfig reads, runs it with the supplied
// args, and returns the resolved *recipeEvidenceConfig (or nil) plus
// any action error.
func runEvidenceCmd(t *testing.T, args []string, resolved *config.ValidateResolved) (*recipeEvidenceConfig, error) {
	t.Helper()
	var got *recipeEvidenceConfig
	cmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "emit-attestation"},
			&cli.StringFlag{Name: "bom"},
			&cli.StringFlag{Name: "push"},
			&cli.BoolFlag{Name: "plain-http"},
			&cli.BoolFlag{Name: "insecure-tls"},
			&cli.StringFlag{Name: "identity-token"},
			&cli.BoolFlag{Name: "oidc-device-flow"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			got = buildRecipeEvidenceConfig(c, resolved)
			return nil
		},
	}
	if err := cmd.Run(context.Background(), append([]string{"test"}, args...)); err != nil {
		return nil, err
	}
	return got, nil
}

func TestBuildRecipeEvidenceConfig(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		resolved *config.ValidateResolved
		wantNil  bool
		want     *recipeEvidenceConfig
	}{
		{
			name:     "neither flag nor config returns nil",
			args:     nil,
			resolved: &config.ValidateResolved{},
			wantNil:  true,
		},
		{
			name: "flag-only populates from flags",
			args: []string{
				"--emit-attestation", "/tmp/out",
				"--bom", "/tmp/bom.json",
				"--push", "ttl.sh/x:1h",
				"--plain-http",
				"--identity-token", "tok",
				"--oidc-device-flow",
			},
			resolved: &config.ValidateResolved{},
			want: &recipeEvidenceConfig{
				OutDir:    "/tmp/out",
				BOMPath:   "/tmp/bom.json",
				Push:      "ttl.sh/x:1h",
				PlainHTTP: true,
				OIDCResolve: bundleattest.ResolveOptions{
					IdentityToken: "tok",
					DeviceFlow:    true,
				},
			},
		},
		{
			name: "config-only fallback when flags unset",
			args: nil,
			resolved: &config.ValidateResolved{
				EvidenceAttestation: &config.EvidenceAttestationResolved{
					Out:         "/cfg/out",
					BOM:         "/cfg/bom.json",
					Push:        "ttl.sh/cfg:1h",
					PlainHTTP:   true,
					InsecureTLS: true,
				},
			},
			want: &recipeEvidenceConfig{
				OutDir:      "/cfg/out",
				BOMPath:     "/cfg/bom.json",
				Push:        "ttl.sh/cfg:1h",
				PlainHTTP:   true,
				InsecureTLS: true,
			},
		},
		{
			name: "flag overrides config when both set",
			args: []string{"--emit-attestation", "/flag/out", "--push", "ttl.sh/flag:1h"},
			resolved: &config.ValidateResolved{
				EvidenceAttestation: &config.EvidenceAttestationResolved{
					Out:  "/cfg/out",
					Push: "ttl.sh/cfg:1h",
					BOM:  "/cfg/bom.json",
				},
			},
			want: &recipeEvidenceConfig{
				OutDir:  "/flag/out",
				Push:    "ttl.sh/flag:1h",
				BOMPath: "/cfg/bom.json",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runEvidenceCmd(t, tt.args, tt.resolved)
			if err != nil {
				t.Fatalf("cmd.Run: %v", err)
			}
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil config")
			}
			if got.OutDir != tt.want.OutDir {
				t.Errorf("OutDir = %q, want %q", got.OutDir, tt.want.OutDir)
			}
			if got.BOMPath != tt.want.BOMPath {
				t.Errorf("BOMPath = %q, want %q", got.BOMPath, tt.want.BOMPath)
			}
			if got.Push != tt.want.Push {
				t.Errorf("Push = %q, want %q", got.Push, tt.want.Push)
			}
			if got.PlainHTTP != tt.want.PlainHTTP {
				t.Errorf("PlainHTTP = %v, want %v", got.PlainHTTP, tt.want.PlainHTTP)
			}
			if got.InsecureTLS != tt.want.InsecureTLS {
				t.Errorf("InsecureTLS = %v, want %v", got.InsecureTLS, tt.want.InsecureTLS)
			}
			if got.OIDCResolve.IdentityToken != tt.want.OIDCResolve.IdentityToken {
				t.Errorf("IdentityToken = %q, want %q",
					got.OIDCResolve.IdentityToken, tt.want.OIDCResolve.IdentityToken)
			}
			if got.OIDCResolve.DeviceFlow != tt.want.OIDCResolve.DeviceFlow {
				t.Errorf("DeviceFlow = %v, want %v",
					got.OIDCResolve.DeviceFlow, tt.want.OIDCResolve.DeviceFlow)
			}
		})
	}
}

func TestSignAndPushBundle_NoPushReturnsZeroOutcome(t *testing.T) {
	bundle := &attestation.Bundle{SummaryDir: "/tmp/x"}
	cfg := &recipeEvidenceConfig{Push: ""}
	out, err := signAndPushBundle(context.Background(), bundle, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Sign != nil || out.Summary != nil {
		t.Errorf("expected zero outcome when --push absent; got %+v", out)
	}
}

func TestObservedImagesFromSnapshot(t *testing.T) {
	mkSubtype := func(name string, data map[string]string) measurement.Subtype {
		readings := make(map[string]measurement.Reading, len(data))
		for k, v := range data {
			readings[k] = measurement.Str(v)
		}
		return measurement.Subtype{Name: name, Data: readings}
	}

	tests := []struct {
		name string
		snap *snapshotter.Snapshot
		want int // expected count; order is map-iteration so we don't assert it
	}{
		{"nil snapshot", nil, 0},
		{"no measurements", &snapshotter.Snapshot{}, 0},
		{
			name: "non-K8s measurement ignored",
			snap: &snapshotter.Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeOS, Subtypes: []measurement.Subtype{
					mkSubtype("image", map[string]string{"alpine": "3.20"}),
				}},
			}},
			want: 0,
		},
		{
			name: "non-image subtype ignored",
			snap: &snapshotter.Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeK8s, Subtypes: []measurement.Subtype{
					mkSubtype("server", map[string]string{"version": "v1.34.0"}),
				}},
			}},
			want: 0,
		},
		{
			name: "K8s image subtype collects refs",
			snap: &snapshotter.Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeK8s, Subtypes: []measurement.Subtype{
					mkSubtype("image", map[string]string{
						"coredns":     "v1.11",
						"kube-proxy":  "v1.34.0",
						"aws-ebs-csi": "v1.59",
					}),
				}},
			}},
			want: 3,
		},
		{
			name: "duplicate refs across subtypes are deduped",
			snap: &snapshotter.Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeK8s, Subtypes: []measurement.Subtype{
					mkSubtype("image", map[string]string{"coredns": "v1.11"}),
					mkSubtype("image", map[string]string{"coredns": "v1.11"}),
				}},
			}},
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := observedImagesFromSnapshot(tt.snap)
			if len(got) != tt.want {
				t.Errorf("got %d images, want %d (got=%v)", len(got), tt.want, got)
			}
		})
	}
}

func TestLoadOrGenerateBOM_FromPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bom.cdx.json")
	body := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","version":1}`)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	got, err := loadOrGenerateBOM(p, nil, nil, nil, "v0")
	if err != nil {
		t.Fatalf("loadOrGenerateBOM: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body mismatch: got=%q want=%q", got, body)
	}
}

func TestLoadOrGenerateBOM_PathNotFound(t *testing.T) {
	_, err := loadOrGenerateBOM("/nonexistent/path/bom.json", nil, nil, nil, "v0")
	if err == nil {
		t.Fatalf("expected error reading missing path")
	}
}

func TestLoadOrGenerateBOM_AutoFromRecipe(t *testing.T) {
	rec := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			Intent:      recipe.CriteriaIntentTraining,
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Chart: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Version: "v25.10.1"},
		},
	}
	cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
		{Name: "operator-health", Image: "ghcr.io/x/deployment:latest"},
	}}

	body, err := loadOrGenerateBOM("", rec, nil, cat, "v0.1.0")
	if err != nil {
		t.Fatalf("loadOrGenerateBOM auto: %v", err)
	}
	if len(body) == 0 {
		t.Fatalf("auto-gen BOM is empty")
	}
	// Decoded BOM should declare CycloneDX.
	doc := &cdx.BOM{}
	if err := json.Unmarshal(body, doc); err != nil {
		t.Fatalf("auto-gen BOM is not valid JSON: %v", err)
	}
	if doc.BOMFormat != "CycloneDX" {
		t.Errorf("BOMFormat = %q, want CycloneDX", doc.BOMFormat)
	}
}

func TestBuildAutoBOM_IncludesRecipeAndValidatorComponents(t *testing.T) {
	rec := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Chart: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Version: "v25.10.1"},
			{Name: "disabled-comp", Type: recipe.ComponentTypeHelm, Overrides: map[string]any{"enabled": false}},
		},
	}
	cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
		{Name: "a", Image: "ghcr.io/x/deployment:latest"},
		{Name: "b", Image: "ghcr.io/x/deployment:latest"}, // dedup
		{Name: "c", Image: "ghcr.io/x/performance:latest"},
	}}

	body, err := buildAutoBOM(rec, nil, cat, "v0.1.0")
	if err != nil {
		t.Fatalf("buildAutoBOM: %v", err)
	}

	doc := &cdx.BOM{}
	if err := json.Unmarshal(body, doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Walk components for the names we expect; disabled-comp must be absent.
	var (
		sawGPUOperator bool
		sawValidators  bool
		sawDisabled    bool
	)
	if doc.Components != nil {
		for _, c := range *doc.Components {
			switch c.Name {
			case "gpu-operator":
				sawGPUOperator = true
			case "validators":
				sawValidators = true
			case "disabled-comp":
				sawDisabled = true
			}
		}
	}
	if !sawGPUOperator {
		t.Errorf("expected gpu-operator component in auto BOM")
	}
	if !sawValidators {
		t.Errorf("expected validators meta-component in auto BOM")
	}
	if sawDisabled {
		t.Errorf("disabled component must not appear in auto BOM")
	}
}

func TestEmitRecipeEvidence_HappyPathNoPush(t *testing.T) {
	dir := t.TempDir()
	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			Intent:      recipe.CriteriaIntentTraining,
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Chart: "gpu-operator", Version: "v25.10.1"},
		},
	}
	snap := &snapshotter.Snapshot{}

	cfg := &recipeEvidenceConfig{
		OutDir: dir,
		// no Push → unsigned path; no BOMPath → auto-gen
	}

	if err := emitRecipeEvidence(context.Background(), rec, snap, nil, cfg); err != nil {
		t.Fatalf("emitRecipeEvidence: %v", err)
	}

	// Pointer is always written.
	if _, err := os.Stat(filepath.Join(dir, "pointer.yaml")); err != nil {
		t.Errorf("pointer.yaml missing: %v", err)
	}
	// Summary-bundle content the unsigned path must produce.
	for _, name := range []string{"manifest.json", "statement.intoto.json", "recipe.yaml", "snapshot.yaml", "bom.cdx.json"} {
		p := filepath.Join(dir, "summary-bundle", name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s missing under summary-bundle: %v", name, err)
		}
	}
}

func TestEmitRecipeEvidence_InvalidPushReference(t *testing.T) {
	dir := t.TempDir()
	rec := &recipe.RecipeResult{Criteria: &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorH100}}
	snap := &snapshotter.Snapshot{}
	cfg := &recipeEvidenceConfig{
		OutDir: dir,
		Push:   "oci://not a valid ref",
	}
	err := emitRecipeEvidence(context.Background(), rec, snap, nil, cfg)
	if err == nil {
		t.Fatalf("expected error for malformed --push reference")
	}
}
