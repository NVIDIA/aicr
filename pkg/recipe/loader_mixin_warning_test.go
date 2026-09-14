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
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestWarnUnappliedDirectOverlayMixins(t *testing.T) {
	tests := []struct {
		name            string
		overlayName     string
		mixins          []string
		appliedOverlays []string
		wantWarn        bool
	}{
		{
			name:            "no mixins declared",
			overlayName:     "leaf",
			mixins:          nil,
			appliedOverlays: nil,
			wantWarn:        false,
		},
		{
			name:            "overlay resolved from catalog",
			overlayName:     "leaf",
			mixins:          []string{"nvsentinel-observability"},
			appliedOverlays: []string{"base", "leaf"},
			wantWarn:        false,
		},
		{
			name:            "overlay outside catalog drops mixins",
			overlayName:     "leaf",
			mixins:          []string{"nvsentinel-observability"},
			appliedOverlays: []string{"base", "other-leaf"},
			wantWarn:        true,
		},
		{
			name:            "no overlays applied at all",
			overlayName:     "leaf",
			mixins:          []string{"nvsentinel-observability", "platform-inference"},
			appliedOverlays: nil,
			wantWarn:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
				Level: slog.LevelWarn,
			})))
			t.Cleanup(func() { slog.SetDefault(restore) })

			overlay := &RecipeMetadata{}
			overlay.Metadata.Name = tt.overlayName
			overlay.Spec.Mixins = tt.mixins

			rec := &RecipeResult{}
			rec.Metadata.AppliedOverlays = tt.appliedOverlays

			warnUnappliedDirectOverlayMixins("/tmp/overlay.yaml", overlay, rec)

			got := buf.String()
			if gotWarn := strings.Contains(got, "not applied"); gotWarn != tt.wantWarn {
				t.Fatalf("warned = %v, want %v (log: %q)", gotWarn, tt.wantWarn, got)
			}
			if !tt.wantWarn {
				return
			}
			// A warning that does not name the dropped mixins cannot be acted on.
			for _, m := range tt.mixins {
				if !strings.Contains(got, m) {
					t.Errorf("warning omits mixin %q; log: %q", m, got)
				}
			}
			if !strings.Contains(got, "/tmp/overlay.yaml") {
				t.Errorf("warning omits the overlay file path; log: %q", got)
			}
		})
	}
}
