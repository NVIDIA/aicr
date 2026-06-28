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
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

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
