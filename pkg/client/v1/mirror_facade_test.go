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
	stderrors "errors"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Mirror inventory closes the last SDK parity gap (#2025). Rendering stays in
// the CLI on purpose, so these tests cover the data contract only.

func TestMirrorInventory_RejectsNilRecipe(t *testing.T) {
	client := newSnapshotCriteriaClient(t)

	got, err := client.MirrorInventory(t.Context(), nil)
	if err == nil {
		t.Fatalf("expected an error, got %+v", got)
	}
	if got != nil {
		t.Errorf("inventory = %+v, want nil on error", got)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
	}
}

// TestMirrorInventory_RejectsUnresolvedRecipe covers a facade RecipeResult that
// did not come from the Client.
//
// Resolved() returns nil for one built by hand, and discovery cannot run on
// nothing. Failing with a named cause beats passing nil into the lister and
// surfacing whatever it says.
func TestMirrorInventory_RejectsUnresolvedRecipe(t *testing.T) {
	client := newSnapshotCriteriaClient(t)

	got, err := client.MirrorInventory(t.Context(), &aicr.RecipeResult{})
	if err == nil {
		t.Fatalf("expected an error, got %+v", got)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
	}
}

// TestMirrorInventory_ToleratesNilOptions covers option-slice hygiene.
//
// Callers build option slices conditionally, so a nil entry must not panic.
// This deliberately does NOT claim to test that options are applied: with a nil
// recipe the method returns before evaluating them. Whether the options reach
// discovery is asserted in the internal test, where the resolved option struct
// is visible without rendering charts.
func TestMirrorInventory_ToleratesNilOptions(t *testing.T) {
	client := newSnapshotCriteriaClient(t)

	_, err := client.MirrorInventory(t.Context(), nil, nil,
		aicr.WithMirrorKubeVersion("1.31.0"),
		aicr.WithMirrorValueOverrides(nil))
	if err == nil {
		t.Fatal("expected the nil-recipe error")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
	}
}

// TestMirrorInventory_NilClient covers the defensive receiver check.
func TestMirrorInventory_NilClient(t *testing.T) {
	var client *aicr.Client

	if _, err := client.MirrorInventory(t.Context(), nil); err == nil {
		t.Fatal("expected an error from a nil Client")
	}
}

// The Client-boundary counterfactuals below are the cases a passing suite did
// not exercise, and each one failed before the guards were added: a nil or
// closed Client returned a normal result, and a nil context reached discovery.
//
// The criteria case is the sharpest. CriteriaRegistry() is deliberately lenient
// -- a nil or closed Client yields a fresh ephemeral registry so callers can use
// it defensively -- so without an explicit guard the method silently derived
// criteria against the DEFAULT registry and returned them with no error. An
// external --data catalog's registered values would simply be missing, which is
// the one failure this method exists to prevent.

func TestClientBoundary_NilClientIsRejected(t *testing.T) {
	var client *aicr.Client

	if _, err := client.CriteriaFromSnapshot(&aicr.Snapshot{}); err == nil {
		t.Error("CriteriaFromSnapshot on a nil Client returned no error; it would " +
			"have used the default registry instead of the Client's provider")
	}
	if _, err := client.MirrorInventory(t.Context(), &aicr.RecipeResult{}); err == nil {
		t.Error("MirrorInventory on a nil Client returned no error")
	}
}

func TestClientBoundary_NilContextIsRejected(t *testing.T) {
	client := newSnapshotCriteriaClient(t)

	//nolint:staticcheck // passing a nil context is the case under test
	_, err := client.MirrorInventory(nil, &aicr.RecipeResult{})
	if err == nil {
		t.Fatal("MirrorInventory accepted a nil context")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("error = %q, want it to name the context; a nil context must not "+
			"be reported as a recipe problem", err)
	}
}

func TestClientBoundary_ClosedClientIsRejected(t *testing.T) {
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if closeErr := client.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	if _, err := client.CriteriaFromSnapshot(&aicr.Snapshot{}); err == nil {
		t.Error("CriteriaFromSnapshot on a closed Client returned no error; the " +
			"provider it was scoped to is gone")
	}
	if _, err := client.MirrorInventory(t.Context(), &aicr.RecipeResult{}); err == nil {
		t.Error("MirrorInventory on a closed Client returned no error")
	}
}
