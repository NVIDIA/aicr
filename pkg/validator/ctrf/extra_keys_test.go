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

package ctrf

import "testing"

// TestExtraKeysPinned freezes the shared key names and values: they are the
// wire contract between every shipped validator image and the readers in
// pkg/evidence, so a rename here silently breaks the crediting of results an
// older or newer image produced.
func TestExtraKeysPinned(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"ExtraKeySkipReason", ExtraKeySkipReason, "skipReason"},
		{"ExtraKeyObservationOnly", ExtraKeyObservationOnly, "observationOnly"},
		{"ExtraKeyResultSchema", ExtraKeyResultSchema, "resultSchema"},
		{"ExtraKeyResultNonce", ExtraKeyResultNonce, "resultNonce"},
		{"ExtraValueTrue", ExtraValueTrue, "true"},
		{"ResultSchemaVersion", ResultSchemaVersion, "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestTrustsReportedImages(t *testing.T) {
	tests := []struct {
		schema string
		want   bool
	}{
		{"", false},
		{"1", false},
		{"2", true},
		{"3", true},
		{"2-dev", false},
		{"02", false},
		{"+2", false},
	}
	for _, tt := range tests {
		t.Run(tt.schema, func(t *testing.T) {
			if got := TrustsReportedImages(tt.schema); got != tt.want {
				t.Errorf("TrustsReportedImages(%q) = %v, want %v", tt.schema, got, tt.want)
			}
		})
	}
}

func TestResultSchemaVersions(t *testing.T) {
	got := ResultSchemaVersions()
	if len(got) != 2 || got[0] != "1" || got[len(got)-1] != ResultSchemaVersion {
		t.Errorf("ResultSchemaVersions() = %v, want [1 %s]", got, ResultSchemaVersion)
	}
}
