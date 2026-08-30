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
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// CriteriaFromSnapshot exists so the snapshot-to-recipe workflow needs only
// pkg/client/v1 (#2437). Before it, the CLI reached into pkg/fingerprint and
// pkg/recipe, and the integrator guide documented that as a supported escape
// hatch — which contradicts this package's promise to be the whole integration
// surface.

// newSnapshotCriteriaClient builds an embedded-source Client for these tests.
func newSnapshotCriteriaClient(t *testing.T) *aicr.Client {
	t.Helper()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("client close: %v", closeErr)
		}
	})
	return client
}

func TestCriteriaFromSnapshot_RejectsNilSnapshot(t *testing.T) {
	client := newSnapshotCriteriaClient(t)

	got, err := client.CriteriaFromSnapshot(nil)
	if err == nil {
		t.Fatalf("expected an error, got criteria %+v", got)
	}
	if got != nil {
		t.Errorf("criteria = %+v, want nil on error", got)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want ErrCodeInvalidRequest; asking for criteria "+
			"from no snapshot is a caller bug, not an empty result", err)
	}
}

// TestCriteriaFromSnapshot_EmptyMeasurementsYieldWildcards pins the deliberate
// non-error case.
//
// A cluster the collectors could not characterize is a legitimate outcome, not
// a failure: every dimension stays "any" and the caller's own
// minimum-specificity check decides whether that is usable. Returning an error
// here would make an unreadable cluster indistinguishable from a broken call.
func TestCriteriaFromSnapshot_EmptyMeasurementsYieldWildcards(t *testing.T) {
	client := newSnapshotCriteriaClient(t)

	got, err := client.CriteriaFromSnapshot(&aicr.Snapshot{})
	if err != nil {
		t.Fatalf("CriteriaFromSnapshot: %v", err)
	}
	if got == nil {
		t.Fatal("criteria = nil, want an all-wildcard result")
	}

	for name, value := range map[string]string{
		"Service":     got.Service,
		"Accelerator": got.Accelerator,
		"Intent":      got.Intent,
		"OS":          got.OS,
		"Platform":    got.Platform,
	} {
		if value != "any" {
			t.Errorf("%s = %q, want \"any\"; an undetermined dimension must stay "+
				"a wildcard rather than be guessed", name, value)
		}
	}
}

// TestCriteriaFromSnapshot_MatchesFacadeShape asserts the result is the
// facade's own Criteria, carrying plain strings.
//
// Returning pkg/recipe's enum-typed Criteria would leak that package across the
// semver boundary and defeat the point of adding the method.
func TestCriteriaFromSnapshot_MatchesFacadeShape(t *testing.T) {
	client := newSnapshotCriteriaClient(t)

	got, err := client.CriteriaFromSnapshot(&aicr.Snapshot{})
	if err != nil {
		t.Fatalf("CriteriaFromSnapshot: %v", err)
	}

	// The returned value must be usable everywhere the facade takes criteria,
	// with no conversion by the caller. Passing it straight to a resolve call
	// is the assertion: if CriteriaFromSnapshot returned pkg/recipe's
	// enum-typed shape instead, this would not compile.
	if _, resolveErr := client.ResolveRecipeFromCriteria(t.Context(), got); resolveErr == nil {
		t.Log("all-wildcard criteria resolved; nothing to assert about the error")
	}
}
