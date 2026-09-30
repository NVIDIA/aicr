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
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestPreflightSkipChecks_RejectsBadInput locks in the same facade-side guards
// ValidateState has, on the method a caller reaches FIRST. The CLI calls this
// before it captures a snapshot, so a caller whose Client is unusable has to
// learn it here rather than several cluster operations later.
func TestPreflightSkipChecks_RejectsBadInput(t *testing.T) {
	t.Parallel()

	validClient := newClientForBundleTest(t)
	validRecipe := newRecipeResultForBundleTest(validClient,
		[]recipe.ComponentRef{{Name: "c1", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "c1", Kind: "Helm"}},
	)

	tests := []struct {
		name   string
		client *Client
		recipe *RecipeResult
	}{
		{"nil client", nil, validRecipe},
		{"nil recipe", validClient, nil},
		{"recipe missing internal", validClient, &RecipeResult{Name: "no-internal"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.client.PreflightSkipChecks(context.Background(), tt.recipe,
				WithValidationSkipChecks("dra-support"))
			if err == nil {
				t.Fatal("expected error, got nil")
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

// TestPreflightSkipChecks_RejectsClosedClient pins that the Client guard runs
// BEFORE the empty-list short-circuit. Without the ordering, a caller that
// passed no skip list would get nil back from a Client it had already closed,
// and would find out only on the next call.
func TestPreflightSkipChecks_RejectsClosedClient(t *testing.T) {
	t.Parallel()

	c := newClientForBundleTest(t)
	r := newRecipeResultForBundleTest(c,
		[]recipe.ComponentRef{{Name: "c1", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "c1", Kind: "Helm"}},
	)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, name := range []string{"with a skip list", "with no skip list"} {
		opts := []ValidateOption{WithValidationSkipChecks("dra-support")}
		if name == "with no skip list" {
			opts = nil
		}
		err := c.PreflightSkipChecks(context.Background(), r, opts...)
		if err == nil {
			t.Errorf("%s: expected an error from a closed Client, got nil", name)
			continue
		}
		if !strings.Contains(err.Error(), "already closed") {
			t.Errorf("%s: error = %v, want the closed-Client message", name, err)
		}
	}
}

// TestPreflightSkipChecks_JudgesNamesAgainstTheCatalog is the behavioral half:
// an unknown name is refused and a real one is not. Both run through the
// Client's own DataProvider, which is what makes this worth having on the
// facade rather than only on pkg/validator: a Client built from a different
// recipe source must judge the names against THAT source's catalog.
func TestPreflightSkipChecks_JudgesNamesAgainstTheCatalog(t *testing.T) {
	t.Parallel()

	c := newClientForBundleTest(t)
	r := newRecipeResultForBundleTest(c,
		[]recipe.ComponentRef{{Name: "gpu-operator", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "gpu-operator", Kind: "Helm"}},
	)

	if err := c.PreflightSkipChecks(context.Background(), r,
		WithValidationSkipChecks("dra-support")); err != nil {
		t.Errorf("dra-support is in the catalog and must be accepted, got %v", err)
	}

	err := c.PreflightSkipChecks(context.Background(), r,
		WithValidationSkipChecks("dra-suport"))
	if err == nil {
		t.Fatal("a name matching no catalog entry must be refused, got nil")
	}
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), `skipChecks entry "dra-suport" matches no validator in the catalog`) {
		t.Errorf("error = %v, want it to name the offending entry", err)
	}
}

// TestPreflightSkipChecks_EmptyListIsANoOp pins the default path: a caller that
// passed no skip list gets nil, so `aicr validate` without --skip-check does no
// extra work.
func TestPreflightSkipChecks_EmptyListIsANoOp(t *testing.T) {
	t.Parallel()

	c := newClientForBundleTest(t)
	r := newRecipeResultForBundleTest(c,
		[]recipe.ComponentRef{{Name: "gpu-operator", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "gpu-operator", Kind: "Helm"}},
	)

	if err := c.PreflightSkipChecks(context.Background(), r); err != nil {
		t.Errorf("no options must be a no-op, got %v", err)
	}
	if err := c.PreflightSkipChecks(context.Background(), r,
		WithValidationSkipChecks()); err != nil {
		t.Errorf("an explicitly empty skip list must be a no-op, got %v", err)
	}
}
