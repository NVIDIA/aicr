// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func datastoreEvidenceSnapshot(subtypes map[string]map[string]measurement.Reading) *snapshotter.Snapshot {
	m := &measurement.Measurement{Type: measurement.TypeK8s}
	for name, data := range subtypes {
		m.Subtypes = append(m.Subtypes, measurement.Subtype{Name: name, Data: data})
	}
	return &snapshotter.Snapshot{Measurements: []*measurement.Measurement{m}}
}

func stateReading(state string) map[string]measurement.Reading {
	return map[string]measurement.Reading{datastoreEvidenceCollectionState: measurement.Str(state)}
}

func TestComputeSnapshotEvidenceState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snap     *snapshotter.Snapshot
		subtype  string
		unknown  string
		priority func(string) int
		want     string
	}{
		{name: "nil snapshot", subtype: defaultStorageClassSubtypeName,
			unknown: recipe.DefaultStorageClassStateUnknown, priority: defaultStorageClassStatePriority},
		{name: "older snapshot without subtype", snap: datastoreEvidenceSnapshot(nil),
			subtype: defaultStorageClassSubtypeName, unknown: recipe.DefaultStorageClassStateUnknown,
			priority: defaultStorageClassStatePriority},
		{name: "storage present",
			snap:    datastoreEvidenceSnapshot(map[string]map[string]measurement.Reading{defaultStorageClassSubtypeName: stateReading("present")}),
			subtype: defaultStorageClassSubtypeName, unknown: recipe.DefaultStorageClassStateUnknown,
			priority: defaultStorageClassStatePriority, want: recipe.DefaultStorageClassStatePresent},
		{name: "storage multiple",
			snap:    datastoreEvidenceSnapshot(map[string]map[string]measurement.Reading{defaultStorageClassSubtypeName: stateReading("multiple")}),
			subtype: defaultStorageClassSubtypeName, unknown: recipe.DefaultStorageClassStateUnknown,
			priority: defaultStorageClassStatePriority, want: recipe.DefaultStorageClassStateMultiple},
		{name: "storage missing reading is unknown",
			snap:    datastoreEvidenceSnapshot(map[string]map[string]measurement.Reading{defaultStorageClassSubtypeName: {}}),
			subtype: defaultStorageClassSubtypeName, unknown: recipe.DefaultStorageClassStateUnknown,
			priority: defaultStorageClassStatePriority, want: recipe.DefaultStorageClassStateUnknown},
		{name: "storage non-string reading is unknown",
			snap: datastoreEvidenceSnapshot(map[string]map[string]measurement.Reading{
				defaultStorageClassSubtypeName: {datastoreEvidenceCollectionState: measurement.Int(1)}}),
			subtype: defaultStorageClassSubtypeName, unknown: recipe.DefaultStorageClassStateUnknown,
			priority: defaultStorageClassStatePriority, want: recipe.DefaultStorageClassStateUnknown},
		{name: "percona unrecognized is unknown",
			snap:    datastoreEvidenceSnapshot(map[string]map[string]measurement.Reading{perconaServerMongoDBSubtypeName: stateReading("bogus")}),
			subtype: perconaServerMongoDBSubtypeName, unknown: recipe.PerconaOperatorStateUnknown,
			priority: perconaOperatorStatePriority, want: recipe.PerconaOperatorStateUnknown},
		{name: "percona aicr-owned",
			snap:    datastoreEvidenceSnapshot(map[string]map[string]measurement.Reading{perconaServerMongoDBSubtypeName: stateReading("aicr-owned")}),
			subtype: perconaServerMongoDBSubtypeName, unknown: recipe.PerconaOperatorStateUnknown,
			priority: perconaOperatorStatePriority, want: recipe.PerconaOperatorStateAICROwned},
		{name: "percona operator-detected",
			snap:    datastoreEvidenceSnapshot(map[string]map[string]measurement.Reading{perconaServerMongoDBSubtypeName: stateReading("operator-detected")}),
			subtype: perconaServerMongoDBSubtypeName, unknown: recipe.PerconaOperatorStateUnknown,
			priority: perconaOperatorStatePriority, want: recipe.PerconaOperatorStateOperatorDetected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := computeSnapshotEvidenceState(t.Context(), tt.snap, tt.subtype, tt.unknown, tt.priority)
			if err != nil {
				t.Fatalf("computeSnapshotEvidenceState() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("computeSnapshotEvidenceState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestComputeSnapshotEvidenceStateAggregatesWorstState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		subtype  string
		states   []string
		unknown  string
		priority func(string) int
		want     string
	}{
		{name: "storage absent beats present", subtype: defaultStorageClassSubtypeName,
			states: []string{"present", "absent", "multiple"}, unknown: recipe.DefaultStorageClassStateUnknown,
			priority: defaultStorageClassStatePriority, want: recipe.DefaultStorageClassStateAbsent},
		{name: "percona crs beat aicr-owned", subtype: perconaServerMongoDBSubtypeName,
			states: []string{"aicr-owned", "crs-detected", "unknown"}, unknown: recipe.PerconaOperatorStateUnknown,
			priority: perconaOperatorStatePriority, want: recipe.PerconaOperatorStateCRsDetected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snap := &snapshotter.Snapshot{}
			for _, state := range tt.states {
				snap.Measurements = append(snap.Measurements, datastoreEvidenceSnapshot(
					map[string]map[string]measurement.Reading{tt.subtype: stateReading(state)}).Measurements[0])
			}
			got, err := computeSnapshotEvidenceState(t.Context(), snap, tt.subtype, tt.unknown, tt.priority)
			if err != nil {
				t.Fatalf("computeSnapshotEvidenceState() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("computeSnapshotEvidenceState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestComputeSnapshotEvidenceStateContextErrors(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		snap     *snapshotter.Snapshot
		wantCode errors.ErrorCode
	}{
		{name: "nil context", wantCode: errors.ErrCodeInvalidRequest},
		{name: "canceled context", ctx: canceled, wantCode: errors.ErrCodeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := computeSnapshotEvidenceState(tt.ctx, tt.snap, defaultStorageClassSubtypeName,
				recipe.DefaultStorageClassStateUnknown, defaultStorageClassStatePriority)
			if errorstest.ReportedCode(err) != tt.wantCode {
				t.Fatalf("error = %v, want code %s", err, tt.wantCode)
			}
		})
	}
}

func TestApplyNVSentinelDatastoreState(t *testing.T) {
	t.Parallel()

	snap := datastoreEvidenceSnapshot(map[string]map[string]measurement.Reading{
		defaultStorageClassSubtypeName:  stateReading(recipe.DefaultStorageClassStateAbsent),
		perconaServerMongoDBSubtypeName: stateReading(recipe.PerconaOperatorStateAPIDetected),
	})

	tests := []struct {
		name        string
		refs        []recipe.ComponentRef
		snap        *snapshotter.Snapshot
		wantStorage string
		wantPercona string
	}{
		{name: "no nvsentinel-mongodb clears stale evidence", refs: []recipe.ComponentRef{{Name: "nvsentinel"}}, snap: snap},
		{name: "nvsentinel-mongodb records evidence", refs: []recipe.ComponentRef{{Name: nvsentinelMongoDBComponentName}},
			snap: snap, wantStorage: recipe.DefaultStorageClassStateAbsent, wantPercona: recipe.PerconaOperatorStateAPIDetected},
		{name: "older snapshot records nothing", refs: []recipe.ComponentRef{{Name: nvsentinelMongoDBComponentName}},
			snap: datastoreEvidenceSnapshot(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := &recipe.RecipeResult{ComponentRefs: tt.refs}
			result.Metadata.DefaultStorageClassState = recipe.DefaultStorageClassStateUnknown
			result.Metadata.PerconaOperatorState = recipe.PerconaOperatorStateCRsDetected
			if err := applyNVSentinelDatastoreState(t.Context(), result, tt.snap); err != nil {
				t.Fatalf("applyNVSentinelDatastoreState() error = %v", err)
			}
			if got := result.Metadata.DefaultStorageClassState; got != tt.wantStorage {
				t.Errorf("DefaultStorageClassState = %q, want %q", got, tt.wantStorage)
			}
			if got := result.Metadata.PerconaOperatorState; got != tt.wantPercona {
				t.Errorf("PerconaOperatorState = %q, want %q", got, tt.wantPercona)
			}
		})
	}

	if err := applyNVSentinelDatastoreState(t.Context(), nil, snap); err != nil {
		t.Errorf("nil result error = %v", err)
	}
}

func TestApplyNVSentinelDatastoreStatePropagatesCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: nvsentinelMongoDBComponentName}}}
	err := applyNVSentinelDatastoreState(ctx, result, datastoreEvidenceSnapshot(nil))
	if errorstest.ReportedCode(err) != errors.ErrCodeTimeout {
		t.Fatalf("error = %v, want timeout", err)
	}
}
