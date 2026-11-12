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

package argocdtest

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Helper()               {}
func (r *recordingTB) Errorf(string, ...any) { r.failed = true }

// t.Run silently suffixes a duplicate subtest name, so a copied case would
// pass unnoticed.
func TestChildNameCases(t *testing.T) {
	seen := make(map[string]bool)
	for _, tc := range ChildNameCases() {
		if tc.Name == "" || tc.ErrSubstr == "" {
			t.Errorf("case %+v needs a Name and an ErrSubstr", tc)
		}
		if seen[tc.Name] {
			t.Errorf("duplicate case name %q", tc.Name)
		}
		seen[tc.Name] = true
	}
}

func TestWantRejected(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantFail bool
	}{
		{"matching rejection", errors.New(errors.ErrCodeInvalidRequest, "name collides"), false},
		{"nil error", nil, true},
		{"wrong code", errors.New(errors.ErrCodeInternal, "name collides"), true},
		{"missing substring", errors.New(errors.ErrCodeInvalidRequest, "other"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingTB{TB: t}
			WantRejected(rec, "Generate", tt.err, "collides")
			if rec.failed != tt.wantFail {
				t.Errorf("WantRejected failed = %v, want %v", rec.failed, tt.wantFail)
			}
		})
	}
}
