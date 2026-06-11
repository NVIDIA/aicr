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
	"errors"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/health"
)

// wantInvalidRequest fails the test unless err carries ErrCodeInvalidRequest.
func wantInvalidRequest(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}

// TestComputeHealth_EmbeddedCatalog locks in that the facade delegates to
// pkg/health against the Client's own embedded provider and returns a populated
// report whose combos all carry a rolled-up structural status.
func TestComputeHealth_EmbeddedCatalog(t *testing.T) {
	t.Parallel()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err = client.LoadCatalog(context.Background()); err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	report, err := client.ComputeHealth(context.Background(), nil)
	if err != nil {
		t.Fatalf("ComputeHealth: %v", err)
	}
	if report == nil {
		t.Fatal("ComputeHealth returned nil report")
	}
	if report.SchemaVersion != health.SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", report.SchemaVersion, health.SchemaVersion)
	}
	if len(report.Combos) == 0 {
		t.Fatal("expected at least one combo in the embedded catalog")
	}
	for _, combo := range report.Combos {
		switch combo.Structure.Status {
		case health.StatusPass, health.StatusWarn, health.StatusFail, health.StatusUnknown:
			// valid rolled-up status
		default:
			t.Errorf("combo %q: unexpected status %q", combo.LeafOverlay, combo.Structure.Status)
		}
	}
}

// TestComputeHealth_Filter narrows the report to a single dimension and
// confirms every returned combo matches the filter.
func TestComputeHealth_Filter(t *testing.T) {
	t.Parallel()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err = client.LoadCatalog(context.Background()); err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	report, err := client.ComputeHealth(context.Background(), &aicr.Criteria{Service: "eks"})
	if err != nil {
		t.Fatalf("ComputeHealth: %v", err)
	}
	for _, combo := range report.Combos {
		if combo.Criteria == nil || combo.Criteria.Service != "eks" {
			t.Errorf("combo %q: service = %v, want eks", combo.LeafOverlay, combo.Criteria)
		}
	}
}

// TestComputeHealth_NilClient confirms the lenient-guard contract shared by the
// other facade methods: a nil Client returns ErrCodeInvalidRequest, not a panic.
func TestComputeHealth_NilClient(t *testing.T) {
	t.Parallel()

	var client *aicr.Client
	_, err := client.ComputeHealth(context.Background(), nil)
	wantInvalidRequest(t, err)
}

// TestComputeHealth_NilContext rejects a nil context with ErrCodeInvalidRequest.
func TestComputeHealth_NilContext(t *testing.T) {
	t.Parallel()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	//nolint:staticcheck // SA1012: deliberately passing nil context to test the guard.
	_, err = client.ComputeHealth(nil, nil)
	wantInvalidRequest(t, err)
}
