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

package chainsaw

import (
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestIsNotFoundErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not found", errors.New(errors.ErrCodeNotFound, "no such resource"), true},
		{"wrapped not found", errors.Wrap(errors.ErrCodeNotFound, "outer", errors.New(errors.ErrCodeNotFound, "inner")), true},
		{"shape mismatch (internal)", errors.New(errors.ErrCodeInternal, "field mismatch"), false},
		{"transient unavailable", errors.New(errors.ErrCodeUnavailable, "api 503"), false},
		{"invalid request", errors.New(errors.ErrCodeInvalidRequest, "bad assert"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFoundErr(tt.err); got != tt.want {
				t.Errorf("isNotFoundErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestResourceObservedErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// Shape mismatch = resource fetched but did not match => observed/exists.
		{"shape mismatch (internal)", errors.New(errors.ErrCodeInternal, "field mismatch"), true},
		{"wrapped internal", errors.Wrap(errors.ErrCodeInternal, "outer", errors.New(errors.ErrCodeInternal, "inner")), true},
		// Absent: not observed.
		{"not found", errors.New(errors.ErrCodeNotFound, "absent"), false},
		// Transient API blip (rate-limiter / 5xx) proves nothing about existence —
		// must NOT latch the grace off.
		{"transient unavailable", errors.New(errors.ErrCodeUnavailable, "api 503"), false},
		{"invalid request", errors.New(errors.ErrCodeInvalidRequest, "bad assert"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resourceObservedErr(tt.err); got != tt.want {
				t.Errorf("resourceObservedErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNotFoundGraceDeadline(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	absent := base.Add(30 * time.Second)
	deadline := base.Add(6 * time.Minute)

	notFound := errors.New(errors.ErrCodeNotFound, "absent")
	notReady := errors.New(errors.ErrCodeInternal, "not ready")

	tests := []struct {
		name           string
		err            error
		sawResource    bool
		absentDeadline time.Time
		deadline       time.Time
		want           time.Time
	}{
		// Absent from the start (never observed): bounded to the short grace.
		{"absent, never seen -> grace", notFound, false, absent, deadline, absent},
		// Not-ready (exists, wrong shape): keeps the full deadline.
		{"not ready -> full deadline", notReady, false, absent, deadline, deadline},
		// Absent but grace already past the deadline: never exceed deadline.
		{"absent grace beyond deadline -> deadline", notFound, false, deadline.Add(time.Minute), deadline, deadline},
		// Transient/unavailable is not NotFound: keeps full deadline.
		{"unavailable -> full deadline", errors.New(errors.ErrCodeUnavailable, "503"), false, absent, deadline, deadline},
		// Recreate flow: resource was observed earlier (sawResource), so a later
		// NotFound keeps the FULL deadline — must not fast-fail on the stale grace.
		{"notfound after seen (recreate) -> full deadline", notFound, true, absent, deadline, deadline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notFoundGraceDeadline(tt.err, tt.sawResource, tt.absentDeadline, tt.deadline); !got.Equal(tt.want) {
				t.Errorf("notFoundGraceDeadline() = %v, want %v", got, tt.want)
			}
		})
	}
}
