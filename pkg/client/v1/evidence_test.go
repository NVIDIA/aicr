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

package aicr

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// requireCode asserts the outermost structured code, which is what callers
// branch on. errors.Is would also match a code buried deeper in the chain.
func requireCode(t *testing.T, err error, code aicrerrors.ErrorCode) {
	t.Helper()
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != code {
		t.Fatalf("error = %v, want outermost code %s", err, code)
	}
}

func TestCNCFEvidenceFeatures_ReturnsCopy(t *testing.T) {
	t.Parallel()
	got := CNCFEvidenceFeatures()
	if !slices.Contains(got, "dra-support") {
		t.Fatalf("CNCFEvidenceFeatures() = %v, want it to contain dra-support", got)
	}
	got[0] = "mutated"
	if CNCFEvidenceFeatures()[0] == "mutated" {
		t.Error("CNCFEvidenceFeatures() returned a shared slice")
	}
}

func TestRenderCNCFEvidence(t *testing.T) {
	t.Parallel()

	report := &ctrf.Report{}
	report.Results.Tests = []ctrf.TestResult{
		{Name: "dra-support", Status: ctrf.StatusPassed, Duration: 1000},
		{Name: "gang-scheduling", Status: ctrf.StatusSkipped},
	}

	t.Run("writes submission evidence", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := (&Client{}).RenderCNCFEvidence(context.Background(), report, dir); err != nil {
			t.Fatalf("RenderCNCFEvidence: %v", err)
		}
		for _, f := range []string{"index.md", "dra-support.md"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				t.Errorf("%s not written: %v", f, err)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "gang-scheduling.md")); !os.IsNotExist(err) {
			t.Errorf("gang-scheduling.md written for a skipped check (stat err = %v)", err)
		}
	})

	tests := []struct {
		name   string
		client *Client
		ctx    context.Context
		dir    string
	}{
		{"nil client", nil, context.Background(), "out"},
		{"nil context", &Client{}, nil, "out"},
		{"empty dir", &Client{}, context.Background(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requireCode(t, tt.client.RenderCNCFEvidence(tt.ctx, report, tt.dir), aicrerrors.ErrCodeInvalidRequest)
		})
	}
}

// TestCollectCNCFEvidence runs every case in NoCluster mode, so the collector
// short-circuits and nothing reaches a cluster. Policy resolution and input
// validation still run first.
func TestCollectCNCFEvidence(t *testing.T) {
	t.Parallel()

	client, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	recipePath := filepath.Join(t.TempDir(), "recipe.yaml")
	recipeYAML := "kind: RecipeMetadata\napiVersion: aicr.run/v1beta1\nmetadata:\n  name: test\nspec:\n  criteria:\n" +
		"    service: eks\n    accelerator: h100\n    intent: training\n    os: ubuntu\n"
	if err = os.WriteFile(recipePath, []byte(recipeYAML), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	rec, err := client.LoadRecipe(context.Background(), recipePath, "")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}

	badAdvertiser, err := client.LoadRecipe(context.Background(), recipePath, "")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}
	badAdvertiser.internal.Metadata.SelectedProfile = &recipe.SelectedProfile{Advertiser: "csp"}

	closed := newClientForBundleTest(t)
	if err = closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	expired, cancelExpired := context.WithTimeout(context.Background(), 0)
	t.Cleanup(cancelExpired)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	valid := CNCFCollectOptions{Dir: t.TempDir(), NoCluster: true}
	withFeatures := func(f ...string) CNCFCollectOptions {
		o := valid
		o.Features = f
		return o
	}
	withoutDir := valid
	withoutDir.Dir = ""

	tests := []struct {
		name     string
		client   *Client
		ctx      context.Context
		rec      *RecipeResult
		opts     CNCFCollectOptions
		wantCode aicrerrors.ErrorCode
	}{
		{name: "standalone run", client: client, ctx: context.Background(), opts: valid},
		{name: "recipe-backed run", client: client, ctx: context.Background(), rec: rec, opts: valid},
		{name: "feature aliases and all", client: client, ctx: context.Background(), opts: withFeatures("dra", "all")},
		{name: "unknown feature", client: client, ctx: context.Background(), opts: withFeatures("nonexistent"), wantCode: aicrerrors.ErrCodeInvalidRequest},
		{name: "unresolvable allocation policy fails closed", client: client, ctx: context.Background(), rec: badAdvertiser, opts: valid, wantCode: aicrerrors.ErrCodeInvalidRequest},
		{name: "policy resolution past deadline is a timeout", client: client, ctx: expired, rec: rec, opts: valid, wantCode: aicrerrors.ErrCodeTimeout},
		{name: "policy resolution after cancel is canceled", client: client, ctx: canceled, rec: rec, opts: valid, wantCode: aicrerrors.ErrCodeCanceled},
		{name: "recipe missing internal", client: client, ctx: context.Background(), rec: &RecipeResult{Name: "x"}, opts: valid, wantCode: aicrerrors.ErrCodeInvalidRequest},
		{name: "recipe from another client", client: newClientForBundleTest(t), ctx: context.Background(), rec: rec, opts: valid, wantCode: aicrerrors.ErrCodeInvalidRequest},
		{name: "empty dir", client: client, ctx: context.Background(), opts: withoutDir, wantCode: aicrerrors.ErrCodeInvalidRequest},
		{name: "nil client", ctx: context.Background(), opts: valid, wantCode: aicrerrors.ErrCodeInvalidRequest},
		{name: "nil context", client: client, opts: valid, wantCode: aicrerrors.ErrCodeInvalidRequest},
		{name: "closed client", client: closed, ctx: context.Background(), opts: valid, wantCode: aicrerrors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.client.CollectCNCFEvidence(tt.ctx, tt.rec, tt.opts)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("CollectCNCFEvidence: %v", err)
				}
				return
			}
			requireCode(t, err, tt.wantCode)
		})
	}
}

func TestMergeReports(t *testing.T) {
	t.Parallel()

	c := &Client{version: "v1.2.3"}

	r1 := &ctrf.Report{}
	r1.Results.Summary = ctrf.Summary{Tests: 2, Passed: 2}
	r2 := &ctrf.Report{}
	r2.Results.Summary = ctrf.Summary{Tests: 3, Passed: 1, Failed: 2}

	results := []*PhaseResult{
		{Phase: "deployment", Report: r1},
		nil,                                 // nil result skipped
		{Phase: "performance", Report: r2},  // counts merged
		{Phase: "conformance", Report: nil}, // nil report contributes nothing
	}

	merged := c.MergeReports(results)
	if merged == nil {
		t.Fatal("MergeReports returned nil")
	}
	if merged.Results.Tool.Name != "aicr" {
		t.Errorf("tool name = %q, want aicr", merged.Results.Tool.Name)
	}
	if merged.Results.Tool.Version != "v1.2.3" {
		t.Errorf("tool version = %q, want v1.2.3", merged.Results.Tool.Version)
	}
	if got := merged.Results.Summary.Tests; got != 5 {
		t.Errorf("merged tests = %d, want 5", got)
	}
	if got := merged.Results.Summary.Passed; got != 3 {
		t.Errorf("merged passed = %d, want 3", got)
	}
	if got := merged.Results.Summary.Failed; got != 2 {
		t.Errorf("merged failed = %d, want 2", got)
	}
}

// TestMergeReports_NilReceiver locks in that MergeReports tolerates a nil
// Client (empty version), so a caller cannot panic on it.
func TestMergeReports_NilReceiver(t *testing.T) {
	t.Parallel()
	var c *Client
	merged := c.MergeReports(nil)
	if merged == nil {
		t.Fatal("MergeReports(nil) returned nil")
	}
	if merged.Results.Tool.Version != "" {
		t.Errorf("version = %q, want empty for nil client", merged.Results.Tool.Version)
	}
}

func TestEmitRecipeEvidence_RejectsBadInput(t *testing.T) {
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
		opts   EvidenceOptions
	}{
		{"nil client", nil, validRecipe, validSnap, EvidenceOptions{OutDir: "out"}},
		{"nil recipe", validClient, nil, validSnap, EvidenceOptions{OutDir: "out"}},
		{"recipe missing internal", validClient, &RecipeResult{Name: "no-internal"}, validSnap, EvidenceOptions{OutDir: "out"}},
		{"nil snapshot", validClient, validRecipe, nil, EvidenceOptions{OutDir: "out"}},
		{"empty outdir", validClient, validRecipe, validSnap, EvidenceOptions{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.client.EmitRecipeEvidence(context.Background(), tt.recipe, tt.snap, nil, tt.opts)
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

// TestEmitRecipeEvidence_RejectsClosedClient locks in the closed-Client guard:
// after Close() clears the builder, evidence emission must fail closed rather
// than loading a catalog from a half-torn-down Client.
func TestEmitRecipeEvidence_RejectsClosedClient(t *testing.T) {
	t.Parallel()

	c := newClientForBundleTest(t)
	r := newRecipeResultForBundleTest(c,
		[]recipe.ComponentRef{{Name: "c1", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "c1", Kind: "Helm"}},
	)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := c.EmitRecipeEvidence(context.Background(), r, &Snapshot{}, nil, EvidenceOptions{OutDir: "out"})
	if err == nil {
		t.Fatalf("expected error from closed Client, got nil")
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}
