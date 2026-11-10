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
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func rdmaModeSnapshot(readings map[string]measurement.Reading) *snapshotter.Snapshot {
	os := &measurement.Measurement{Type: measurement.TypeOS}
	if readings != nil {
		os.Subtypes = []measurement.Subtype{{Name: kmodParamSubtypeName, Data: readings}}
	}
	return &snapshotter.Snapshot{Measurements: []*measurement.Measurement{os}}
}

func TestApplyRDMANamespaceMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		snap *snapshotter.Snapshot
		want string
	}{
		{name: "nil snapshot is not recorded"},
		{name: "older snapshot without the subtype is not recorded", snap: rdmaModeSnapshot(nil)},
		{name: "module not loaded is not recorded", snap: rdmaModeSnapshot(map[string]measurement.Reading{})},
		{
			name: "N is exclusive",
			snap: rdmaModeSnapshot(map[string]measurement.Reading{ibCoreNetnsModeReading: measurement.Str("N")}),
			want: recipe.RDMANamespaceModeExclusive,
		},
		{
			name: "Y is shared",
			snap: rdmaModeSnapshot(map[string]measurement.Reading{ibCoreNetnsModeReading: measurement.Str("Y")}),
			want: recipe.RDMANamespaceModeShared,
		},
		{
			name: "anything else is unknown",
			snap: rdmaModeSnapshot(map[string]measurement.Reading{ibCoreNetnsModeReading: measurement.Str("1")}),
			want: recipe.RDMANamespaceModeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := &recipe.RecipeResult{}
			result.Metadata.RDMANamespaceMode = "stale"
			if err := applyRDMANamespaceMode(context.Background(), result, tt.snap); err != nil {
				t.Fatalf("applyRDMANamespaceMode() error = %v", err)
			}
			if got := result.Metadata.RDMANamespaceMode; got != tt.want {
				t.Errorf("RDMANamespaceMode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyRDMANamespaceModeCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := applyRDMANamespaceMode(ctx, &recipe.RecipeResult{}, rdmaModeSnapshot(nil)); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
}
