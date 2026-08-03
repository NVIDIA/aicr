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
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func TestComputeMariaDBOperatorState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		snap *snapshotter.Snapshot
		want string
	}{
		{name: "nil snapshot is not recorded", want: ""},
		{name: "empty snapshot is not recorded", snap: &snapshotter.Snapshot{}, want: ""},
		{
			name: "older snapshot without MariaDB subtype is not recorded",
			snap: mariaDBStateSnapshot(nil),
			want: "",
		},
		{
			name: "missing collection state",
			snap: mariaDBStateSnapshot(map[string]measurement.Reading{}),
			want: recipe.MariaDBOperatorStateUnknown,
		},
		{
			name: "non-string collection state",
			snap: mariaDBStateSnapshot(map[string]measurement.Reading{
				mariaDBCollectionStateKey: measurement.Int(1),
			}),
			want: recipe.MariaDBOperatorStateUnknown,
		},
		{
			name: "unrecognized collection state",
			snap: mariaDBStateSnapshot(map[string]measurement.Reading{
				mariaDBCollectionStateKey: measurement.Str("unexpected"),
			}),
			want: recipe.MariaDBOperatorStateUnknown,
		},
		{
			name: "absent",
			snap: mariaDBStateSnapshot(map[string]measurement.Reading{
				mariaDBCollectionStateKey: measurement.Str(recipe.MariaDBOperatorStateAbsent),
			}),
			want: recipe.MariaDBOperatorStateAbsent,
		},
		{
			name: "API detected",
			snap: mariaDBStateSnapshot(map[string]measurement.Reading{
				mariaDBCollectionStateKey: measurement.Str(recipe.MariaDBOperatorStateAPIDetected),
			}),
			want: recipe.MariaDBOperatorStateAPIDetected,
		},
		{
			name: "CRs detected",
			snap: mariaDBStateSnapshot(map[string]measurement.Reading{
				mariaDBCollectionStateKey: measurement.Str(recipe.MariaDBOperatorStateCRsDetected),
			}),
			want: recipe.MariaDBOperatorStateCRsDetected,
		},
		{
			name: "collector reported unknown",
			snap: mariaDBStateSnapshot(map[string]measurement.Reading{
				mariaDBCollectionStateKey: measurement.Str(recipe.MariaDBOperatorStateUnknown),
			}),
			want: recipe.MariaDBOperatorStateUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := computeMariaDBOperatorState(t.Context(), tt.snap)
			if err != nil {
				t.Fatalf("computeMariaDBOperatorState() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("computeMariaDBOperatorState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyMariaDBOperatorStateOnlyForAICRProvidedAccounting(t *testing.T) {
	t.Parallel()

	newResult := func(mode recipe.AccountingMode) *recipe.RecipeResult {
		return &recipe.RecipeResult{
			Configuration: &recipe.RecipeConfiguration{
				Slurm: &recipe.SlurmConfiguration{
					Accounting: &recipe.SlurmAccountingConfiguration{Mode: mode},
				},
			},
		}
	}
	absentSnapshot := mariaDBStateSnapshot(map[string]measurement.Reading{
		mariaDBCollectionStateKey: measurement.Str(recipe.MariaDBOperatorStateAbsent),
	})

	provided := newResult(recipe.AccountingModeAICRProvided)
	if err := applyMariaDBOperatorState(t.Context(), provided, absentSnapshot); err != nil {
		t.Fatalf("applyMariaDBOperatorState() error = %v", err)
	}
	if got := provided.Metadata.MariaDBOperatorState; got != recipe.MariaDBOperatorStateAbsent {
		t.Errorf("AICR-provided metadata state = %q, want %q",
			got, recipe.MariaDBOperatorStateAbsent)
	}

	customerManaged := newResult(recipe.AccountingModeCustomerManaged)
	customerManaged.Metadata.MariaDBOperatorState = recipe.MariaDBOperatorStateCRsDetected
	if err := applyMariaDBOperatorState(t.Context(), customerManaged, absentSnapshot); err != nil {
		t.Fatalf("applyMariaDBOperatorState() error = %v", err)
	}
	if got := customerManaged.Metadata.MariaDBOperatorState; got != "" {
		t.Errorf("customer-managed stale metadata state = %q, want empty", got)
	}
}

func TestComputeMariaDBOperatorStateAggregatesAllK8sMeasurements(t *testing.T) {
	t.Parallel()
	withoutSubtype := mariaDBStateSnapshot(nil).Measurements[0]
	apiDetected := mariaDBStateSnapshot(map[string]measurement.Reading{
		mariaDBCollectionStateKey: measurement.Str(recipe.MariaDBOperatorStateAPIDetected),
	}).Measurements[0]
	crsDetected := mariaDBStateSnapshot(map[string]measurement.Reading{
		mariaDBCollectionStateKey: measurement.Str(recipe.MariaDBOperatorStateCRsDetected),
	}).Measurements[0]

	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			withoutSubtype,
			apiDetected,
			crsDetected,
		},
	}
	got, err := computeMariaDBOperatorState(t.Context(), snap)
	if err != nil {
		t.Fatalf("computeMariaDBOperatorState() error = %v", err)
	}
	if got != recipe.MariaDBOperatorStateCRsDetected {
		t.Fatalf("computeMariaDBOperatorState() = %q, want %q",
			got, recipe.MariaDBOperatorStateCRsDetected)
	}
}

func TestComputeMariaDBOperatorStateHonorsCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		snap *snapshotter.Snapshot
	}{
		{name: "nil snapshot"},
		{name: "empty snapshot", snap: &snapshotter.Snapshot{}},
		{name: "snapshot with measurement", snap: mariaDBStateSnapshot(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			_, err := computeMariaDBOperatorState(ctx, tt.snap)
			if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
				t.Fatalf("computeMariaDBOperatorState() error = %v, want timeout error", err)
			}
		})
	}
}

func TestComputeMariaDBOperatorStateRejectsNilContext(t *testing.T) {
	t.Parallel()

	//nolint:staticcheck // SA1012: deliberately passing nil context to test the guard.
	_, err := computeMariaDBOperatorState(nil, nil)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("computeMariaDBOperatorState() error = %v, want invalid request error", err)
	}
}

func mariaDBStateSnapshot(data map[string]measurement.Reading) *snapshotter.Snapshot {
	subtypes := []measurement.Subtype{}
	if data != nil {
		subtypes = append(subtypes, measurement.Subtype{
			Name: mariaDBOperatorSubtypeName,
			Data: data,
		})
	}
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{{
			Type:     measurement.TypeK8s,
			Subtypes: subtypes,
		}},
	}
}
