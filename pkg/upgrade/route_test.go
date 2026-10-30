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

package upgrade

import (
	"math"
	"testing"

	"github.com/Masterminds/semver/v3"
)

func TestStepStartsRejectOverflow(t *testing.T) {
	tests := []struct {
		name  string
		start func(*semver.Version, uint64) (*semver.Version, bool)
		v     *semver.Version
		want  string
	}{
		{"next minor", minorStart, semver.MustParse("1.2.3"), "1.3.0"},
		{"the last minor", minorStart, semver.New(1, math.MaxUint64-1, 0, "", ""), semver.New(1, math.MaxUint64, 0, "", "").String()},
		{"a minor past the last", minorStart, semver.New(1, math.MaxUint64, 0, "", ""), ""},
		{"next major", majorStart, semver.MustParse("1.2.3"), "2.0.0"},
		{"a major past the last", majorStart, semver.New(math.MaxUint64, 0, 0, "", ""), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, ok := tt.start(tt.v, 1)
			if tt.want == "" {
				if ok {
					t.Errorf("start(%s, 1) = %s, want overflow", tt.v, next)
				}
				return
			}
			if !ok || next.String() != tt.want {
				t.Errorf("start(%s, 1) = %v, %v, want %s", tt.v, next, ok, tt.want)
			}
		})
	}
}
