package aicr_test

import (
	stderrors "errors"
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

// TestMirrorInventory_OptionsAreApplied asserts the options reach the lister.
//
// Both change the discovered set: overrides can disable a sub-component and
// remove its images, and the Kubernetes version changes how charts branch. An
// option that silently did nothing would produce a plausible-looking inventory
// that mirrors the wrong artifacts.
func TestMirrorInventory_OptionsAreApplied(t *testing.T) {
	client := newSnapshotCriteriaClient(t)

	// nil options must be tolerated rather than panic: callers build option
	// slices conditionally.
	_, err := client.MirrorInventory(t.Context(), nil, nil,
		aicr.WithMirrorKubeVersion("1.31.0"),
		aicr.WithMirrorValueOverrides(nil))
	if err == nil {
		t.Fatal("expected the nil-recipe error to still win over option handling")
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
