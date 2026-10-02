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

package uatbroker

import (
	"reflect"
	"testing"
)

// namedReservations builds bare Reservation rows (name + both nightly intents)
// for the ordering tests, which assert version order only.
func namedReservations(names ...string) []Reservation {
	out := make([]Reservation, 0, len(names))
	for _, n := range names {
		out = append(out, Reservation{Name: n, NightlyIntents: []string{IntentTraining, IntentInference}})
	}
	return out
}

// versionsOf returns the ordered AICRVersion strings for a reservation's
// cells, with the tip-of-main cell rendered as "main" so order is readable.
func versionsOf(cells []Cell) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		if c.IsMain {
			out = append(out, "main")
			continue
		}
		out = append(out, c.AICRVersion)
	}
	return out
}

func TestExpandScheduleOrdering(t *testing.T) {
	// Deliberately unsorted, with a pre-release and a non-semver tag mixed in.
	rawTags := []string{"v1.5.0", "v2.0.0-rc1", "v1.2.0", "v2.0.0", "not-a-tag", "v1.10.0"}

	tests := []struct {
		name         string
		reservations []string
		includeMain  bool
		previousN    int
		// want maps reservation -> ordered version labels ("main" for the
		// tip-of-main cell).
		want map[string][]string
	}{
		{
			name:         "main first then 2 newest stable descending",
			reservations: []string{"aws-h100"},
			includeMain:  true,
			previousN:    2,
			// Stable, descending: v2.0.0, v1.10.0, v1.5.0, v1.2.0. The rc1
			// pre-release and "not-a-tag" are dropped. previousN=2 keeps the
			// two newest; v1.5.0/v1.2.0 (oldest) are dropped.
			want: map[string][]string{"aws-h100": {"main", "v2.0.0", "v1.10.0"}},
		},
		{
			name:         "drop oldest first when previousN tight",
			reservations: []string{"aws-h100"},
			includeMain:  false,
			previousN:    1,
			want:         map[string][]string{"aws-h100": {"v2.0.0"}},
		},
		{
			name:         "previousN zero is main only",
			reservations: []string{"aws-h100"},
			includeMain:  true,
			previousN:    0,
			want:         map[string][]string{"aws-h100": {"main"}},
		},
		{
			name:         "previousN larger than available keeps all stable",
			reservations: []string{"aws-h100"},
			includeMain:  true,
			previousN:    99,
			want:         map[string][]string{"aws-h100": {"main", "v2.0.0", "v1.10.0", "v1.5.0", "v1.2.0"}},
		},
		{
			name:         "multiple reservations get identical ordered cells",
			reservations: []string{"aws-h100", "gcp-h100"},
			includeMain:  true,
			previousN:    2,
			want: map[string][]string{
				"aws-h100": {"main", "v2.0.0", "v1.10.0"},
				"gcp-h100": {"main", "v2.0.0", "v1.10.0"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandSchedule(namedReservations(tt.reservations...), nil, rawTags, tt.includeMain, tt.previousN)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d reservations, want %d", len(got), len(tt.want))
			}
			for res, wantVers := range tt.want {
				gotVers := versionsOf(got[res])
				if !reflect.DeepEqual(gotVers, wantVers) {
					t.Errorf("reservation %s = %v, want %v", res, gotVers, wantVers)
				}
			}
			// The release cells must carry the tag, and only main is IsMain.
			for res, cells := range got {
				for i, c := range cells {
					if c.Reservation != res {
						t.Errorf("%s cell[%d].Reservation = %q, want %q", res, i, c.Reservation, res)
					}
					if c.IsMain && c.AICRVersion != "" {
						t.Errorf("%s main cell[%d] has non-empty AICRVersion %q", res, i, c.AICRVersion)
					}
					if !c.IsMain && c.AICRVersion == "" {
						t.Errorf("%s release cell[%d] has empty AICRVersion", res, i)
					}
				}
			}
		})
	}
}

func TestExpandScheduleEmptyTags(t *testing.T) {
	got := ExpandSchedule(namedReservations("aws-h100"), nil, nil, true, 2)
	if v := versionsOf(got["aws-h100"]); !reflect.DeepEqual(v, []string{"main"}) {
		t.Errorf("empty tags = %v, want [main]", v)
	}
}

func TestExpandScheduleNegativePreviousN(t *testing.T) {
	// A negative previousN is clamped to zero (main only).
	got := ExpandSchedule(namedReservations("aws-h100"), nil, []string{"v1.0.0"}, true, -3)
	if v := versionsOf(got["aws-h100"]); !reflect.DeepEqual(v, []string{"main"}) {
		t.Errorf("negative previousN = %v, want [main]", v)
	}
}

// azureInferenceFloor gates azure inference release cells below v0.18.0.
var azureInferenceFloor = &Compat{Floors: []Floor{{
	Lane: CloudAzure, Intents: []string{IntentInference}, MinRelease: "v0.18.0", Reason: "perf fix", Line: 3,
}}}

// TestEligibleNightlyIntents covers the harness-compat gate directly: main
// runs everything, a release below an intent's floor skips that intent (and
// records why), a release at or above it keeps it, and ungated intents always
// run.
func TestEligibleNightlyIntents(t *testing.T) {
	azure := Reservation{Name: "azure-h100", Cloud: CloudAzure, NightlyIntents: []string{IntentTraining, IntentInference}}
	skippedInference := []SkippedIntent{{Intent: IntentInference, Floor: "v0.18.0", Lane: CloudAzure, Reason: "perf fix"}}
	both := []string{IntentTraining, IntentInference}
	tests := []struct {
		name        string
		res         Reservation
		compat      *Compat
		version     string
		isMain      bool
		want        []string
		wantSkipped []SkippedIntent
	}{
		{"main runs every intent despite the floor", azure, azureInferenceFloor, "", true, both, nil},
		{"release below the floor skips inference", azure, azureInferenceFloor, "v0.17.0", false, []string{IntentTraining}, skippedInference},
		{"release at the floor keeps inference", azure, azureInferenceFloor, "v0.18.0", false, both, nil},
		{"release above the floor keeps inference", azure, azureInferenceFloor, "v0.19.0", false, both, nil},
		{"nil compat runs every intent", azure, nil, "v0.1.0", false, both, nil},
		{"empty compat runs every intent", azure, &Compat{Floors: []Floor{}}, "v0.1.0", false, both, nil},
		{
			"floor on another lane does not apply",
			Reservation{Name: "aws-h100", Cloud: CloudAWS, NightlyIntents: both},
			azureInferenceFloor, "v0.1.0", false, both, nil,
		},
		{
			"absent nightly-intents defaults to training and is ungated",
			Reservation{Name: "x", Cloud: CloudAzure}, azureInferenceFloor, "v0.1.0", false, []string{IntentTraining}, nil,
		},
		{"unparseable release version fails open", azure, azureInferenceFloor, "not-a-semver", false, both, nil},
		{
			"unparseable floor fails open",
			azure,
			&Compat{Floors: []Floor{{Lane: CloudAzure, Intents: []string{IntentInference}, MinRelease: "bogus"}}},
			"v0.1.0", false, both, nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, skipped := tt.res.EligibleNightlyIntents(tt.compat, tt.version, tt.isMain)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("intents(%q, main=%v) = %v, want %v", tt.version, tt.isMain, got, tt.want)
			}
			if !reflect.DeepEqual(skipped, tt.wantSkipped) {
				t.Errorf("skipped(%q, main=%v) = %+v, want %+v", tt.version, tt.isMain, skipped, tt.wantSkipped)
			}
		})
	}
}

// TestExpandScheduleAppliesCompatFloor verifies the floor flows through to
// per-cell Intents and Skipped: main and the at/above release carry both
// intents; the below-floor release carries only training and names the skip.
func TestExpandScheduleAppliesCompatFloor(t *testing.T) {
	res := Reservation{Name: "azure-h100", Cloud: CloudAzure, NightlyIntents: []string{IntentTraining, IntentInference}}
	compat := &Compat{Floors: []Floor{{Lane: CloudAzure, Intents: []string{IntentInference}, MinRelease: "v2.0.0", Reason: "r"}}}
	got := ExpandSchedule([]Reservation{res}, compat, []string{"v1.0.0", "v2.0.0"}, true, 2)
	byVersion := map[string][]string{}
	skippedBy := map[string][]SkippedIntent{}
	for _, c := range got["azure-h100"] {
		key := c.AICRVersion
		if c.IsMain {
			key = "main"
		}
		byVersion[key] = c.Intents
		if c.Skipped != nil {
			skippedBy[key] = c.Skipped
		}
	}
	want := map[string][]string{
		"main":   {IntentTraining, IntentInference},
		"v2.0.0": {IntentTraining, IntentInference},
		"v1.0.0": {IntentTraining},
	}
	if !reflect.DeepEqual(byVersion, want) {
		t.Errorf("per-cell intents = %v, want %v", byVersion, want)
	}
	wantSkipped := map[string][]SkippedIntent{
		"v1.0.0": {{Intent: IntentInference, Floor: "v2.0.0", Lane: CloudAzure, Reason: "r"}},
	}
	if !reflect.DeepEqual(skippedBy, wantSkipped) {
		t.Errorf("per-cell skipped = %+v, want %+v", skippedBy, wantSkipped)
	}
}

func TestSortedStableDescending(t *testing.T) {
	// "v1.0" normalizes to 1.0.0 (a duplicate of "v1.0.0") and must be dropped,
	// not consume a second slot.
	got := sortedStableDescending([]string{"v1.0.0", "v0.9.0", "v1.0.0-beta", "garbage", "v2.3.4", "  v1.10.0  ", "v1.0"})
	want := []string{"v2.3.4", "v1.10.0", "v1.0.0", "v0.9.0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortedStableDescending = %v, want %v", got, want)
	}
}
