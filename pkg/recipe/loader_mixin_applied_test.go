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

package recipe

import (
	stderrors "errors"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// A nil DataProvider resolves against the embedded catalog, so the cases below
// compare against real overlay declarations rather than hand-built fixtures.
func TestEnsureDirectOverlayMixinsApplied(t *testing.T) {
	tests := []struct {
		name            string
		overlayName     string
		mixins          []string
		appliedOverlays []string
		wantErr         bool
		wantNamed       []string
	}{
		{
			name:            "no mixins declared",
			overlayName:     "oke-ol-inference",
			mixins:          nil,
			appliedOverlays: []string{"oke-ol-inference"},
			wantErr:         false,
		},
		{
			// The file matches the catalog entry of that name, so the catalog
			// applied this declaration and nothing was dropped.
			name:            "catalog overlay with identical mixins is accepted",
			overlayName:     "oke-ol-inference",
			mixins:          []string{"platform-inference"},
			appliedOverlays: []string{"oke-ol-inference"},
			wantErr:         false,
		},
		{
			// Regression guard. Copying an embedded overlay and adding a mixin
			// keeps the name, so the catalog twin resolves and the name appears
			// in AppliedOverlays -- but the added mixin was dropped. Matching on
			// name alone would let this through silently.
			name:            "catalog name reused with an extra mixin is rejected",
			overlayName:     "oke-ol-inference",
			mixins:          []string{"platform-inference", "platform-kubeflow"},
			appliedOverlays: []string{"oke-ol-inference"},
			wantErr:         true,
			wantNamed:       []string{"platform-kubeflow"},
		},
		{
			name:            "overlay outside the catalog is rejected",
			overlayName:     "my-custom-leaf",
			mixins:          []string{"platform-inference"},
			appliedOverlays: []string{"my-custom-leaf"},
			wantErr:         true,
			wantNamed:       []string{"platform-inference"},
		},
		{
			// A mixin another applied chain member already declares is not
			// missing, even though this overlay is not itself in the catalog.
			name:            "mixin supplied by another applied overlay is accepted",
			overlayName:     "my-custom-leaf",
			mixins:          []string{"platform-inference"},
			appliedOverlays: []string{"my-custom-leaf", "oke-ol-inference"},
			wantErr:         false,
		},
		{
			name:            "no overlays applied at all is rejected",
			overlayName:     "my-custom-leaf",
			mixins:          []string{"platform-inference"},
			appliedOverlays: nil,
			wantErr:         true,
			wantNamed:       []string{"platform-inference"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlay := &RecipeMetadata{}
			overlay.Metadata.Name = tt.overlayName
			overlay.Spec.Mixins = tt.mixins

			rec := &RecipeResult{}
			rec.Metadata.AppliedOverlays = tt.appliedOverlays

			err := ensureDirectOverlayMixinsApplied(t.Context(), "/tmp/overlay.yaml", overlay, rec, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ensureDirectOverlayMixinsApplied() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
			// An error that does not name the dropped mixins cannot be acted on.
			for _, mixin := range tt.wantNamed {
				if !strings.Contains(err.Error(), mixin) {
					t.Errorf("error omits dropped mixin %q; got: %v", mixin, err)
				}
			}
			if !strings.Contains(err.Error(), "/tmp/overlay.yaml") {
				t.Errorf("error omits the overlay file path; got: %v", err)
			}
		})
	}
}

// A mixin the overlay does not declare must not be reported as dropped.
func TestEnsureDirectOverlayMixinsAppliedReportsOnlyMissing(t *testing.T) {
	overlay := &RecipeMetadata{}
	overlay.Metadata.Name = "oke-ol-inference"
	overlay.Spec.Mixins = []string{"platform-inference", "platform-kubeflow"}

	rec := &RecipeResult{}
	rec.Metadata.AppliedOverlays = []string{"oke-ol-inference"}

	err := ensureDirectOverlayMixinsApplied(t.Context(), "/tmp/overlay.yaml", overlay, rec, nil)
	if err == nil {
		t.Fatal("ensureDirectOverlayMixinsApplied() = nil, want error")
	}
	if strings.Contains(err.Error(), "platform-inference") {
		t.Errorf("error names platform-inference, which the catalog did apply; got: %v", err)
	}
}
