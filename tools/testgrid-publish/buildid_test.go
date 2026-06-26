// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package main

import (
	"testing"
	"time"
)

func TestBuildID(t *testing.T) {
	ts := time.Unix(1749600000, 0).UTC()

	tests := []struct {
		name       string
		attestedAt time.Time
		digest     string
		want       string
	}{
		{
			name:       "sha256 digest uses last 8 chars",
			attestedAt: ts,
			digest:     "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			want:       "1749600000-23456789",
		},
		{
			name:       "deterministic for same inputs",
			attestedAt: ts,
			digest:     "sha256:aabbccdd11223344aabbccdd11223344aabbccdd11223344aabbccdd11223344",
			want:       "1749600000-11223344",
		},
		{
			name:       "different timestamps produce different ids",
			attestedAt: time.Unix(1749600001, 0).UTC(),
			digest:     "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			want:       "1749600001-23456789",
		},
		{
			name:       "empty digest falls back to unknown0",
			attestedAt: ts,
			digest:     "",
			want:       "1749600000-unknown0",
		},
		{
			name:       "digest without prefix",
			attestedAt: ts,
			digest:     "abcdef01",
			want:       "1749600000-abcdef01",
		},
		{
			name:       "short digest under 8 chars returned as-is",
			attestedAt: ts,
			digest:     "sha256:abc",
			want:       "1749600000-abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildID(tt.attestedAt, tt.digest)
			if got != tt.want {
				t.Errorf("buildID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildIDDeterminism(t *testing.T) {
	ts := time.Unix(1749600000, 0).UTC()
	digest := "sha256:abc123def456789012345678901234567890123456789012345678901234567"

	first := buildID(ts, digest)
	second := buildID(ts, digest)
	if first != second {
		t.Errorf("buildID not deterministic: %q != %q", first, second)
	}
}
