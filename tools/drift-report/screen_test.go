// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"reflect"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

func versions(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.UpdateType+" "+c.Version)
	}
	return out
}

// The dates below are the registry's own, read from each tag's
// org.opencontainers.image.created annotation on 2026-10-12.
func TestScreen(t *testing.T) {
	tests := []struct {
		name          string
		lookup        Lookup
		wantOffered   []string
		wantPredating []string
		wantUndated   []string
		wantNoCurrent bool
	}{
		{
			// The issue's own case: max-semver offered the retracted v0.20.1, and
			// v0.18.x -- the line upstream marks Latest -- never appeared.
			name: "kai-scheduler: retracted higher line withheld, active line offered",
			lookup: Lookup{
				Current: "v0.16.9", CurrentPublished: mustTime(t, "2026-08-16T10:29:50Z"),
				Candidates: []Candidate{
					{Version: "v0.16.11", UpdateType: "patch", Published: mustTime(t, "2026-10-11T10:16:51Z")},
					{Version: "v0.17.2", UpdateType: "minor", Published: mustTime(t, "2026-09-16T12:00:00Z")},
					{Version: "v0.18.5", UpdateType: "minor", Published: mustTime(t, "2026-10-09T12:00:00Z")},
					{Version: "v0.20.1", UpdateType: "minor", Published: mustTime(t, "2025-11-30T13:27:49Z")},
				},
			},
			wantOffered:   []string{"patch v0.16.11", "minor v0.18.5"},
			wantPredating: []string{"minor v0.20.1"},
		},
		{
			name: "agentgateway: kgateway-lineage major withheld, minor offered",
			lookup: Lookup{
				Current: "v1.5.0", CurrentPublished: mustTime(t, "2026-08-27T17:26:27Z"),
				Candidates: []Candidate{
					{Version: "v1.6.0", UpdateType: "minor", Published: mustTime(t, "2026-10-02T15:42:02Z")},
					{Version: "v2.2.1", UpdateType: "major", Published: mustTime(t, "2026-02-17T22:12:11Z")},
				},
			},
			wantOffered:   []string{"minor v1.6.0"},
			wantPredating: []string{"major v2.2.1"},
		},
		{
			// agentgateway as the 2026-09-16 report saw it, before v1.6.0 existed.
			name: "every candidate predates the pin: nothing offered",
			lookup: Lookup{
				Current: "v1.5.0", CurrentPublished: mustTime(t, "2026-08-27T17:26:27Z"),
				Candidates: []Candidate{
					{Version: "v2.2.1", UpdateType: "major", Published: mustTime(t, "2026-02-17T22:12:11Z")},
				},
			},
			wantPredating: []string{"major v2.2.1"},
		},
		{
			// eks-charts regenerates its whole index in one pass, so v0.5.29 and
			// v0.5.33 are stamped microseconds apart; that order means nothing.
			name: "batch-regenerated index: sub-second order is not publish order",
			lookup: Lookup{
				Current: "v0.5.33", CurrentPublished: mustTime(t, "2026-10-05T21:16:09.295299385Z"),
				Candidates: []Candidate{
					{Version: "v0.5.34", UpdateType: "patch", Published: mustTime(t, "2026-10-05T21:16:09.291439978Z")},
				},
			},
			wantOffered: []string{"patch v0.5.34"},
		},
		{
			name: "one line per type: only the highest acceptable version is offered",
			lookup: Lookup{
				Current: "26.6.0", CurrentPublished: mustTime(t, "2026-06-06T10:14:56Z"),
				Candidates: []Candidate{
					{Version: "26.7.1", UpdateType: "minor", Published: mustTime(t, "2026-07-10T00:00:00Z")},
					{Version: "26.10.1", UpdateType: "minor", Published: mustTime(t, "2026-09-21T18:38:32Z")},
					{Version: "26.8.0", UpdateType: "minor", Published: mustTime(t, "2026-08-10T00:00:00Z")},
				},
			},
			wantOffered: []string{"minor 26.10.1"},
		},
		{
			// A predating line below the offer would never have been shown, so
			// listing it as withheld would only add noise.
			name: "predating version below the offer is dropped, not withheld",
			lookup: Lookup{
				Current: "v1.0.0", CurrentPublished: mustTime(t, "2026-08-01T00:00:00Z"),
				Candidates: []Candidate{
					{Version: "v1.1.0", UpdateType: "minor", Published: mustTime(t, "2025-01-01T00:00:00Z")},
					{Version: "v1.2.0", UpdateType: "minor", Published: mustTime(t, "2026-09-01T00:00:00Z")},
				},
			},
			wantOffered: []string{"minor v1.2.0"},
		},
		{
			name: "undated current: nothing can be withheld, the check is reported as not run",
			lookup: Lookup{
				Current: "v1.0.0",
				Candidates: []Candidate{
					{Version: "v1.1.0", UpdateType: "minor", Published: mustTime(t, "2025-01-01T00:00:00Z")},
					{Version: "v2.0.0", UpdateType: "major", Published: mustTime(t, "2025-01-01T00:00:00Z")},
				},
			},
			wantOffered:   []string{"minor v1.1.0", "major v2.0.0"},
			wantNoCurrent: true,
		},
		{
			// An undated candidate cannot be shown to predate the pin, so it is
			// offered -- the pre-screen behavior -- and named as unchecked.
			name: "undated candidate is offered and named",
			lookup: Lookup{
				Current: "v1.0.0", CurrentPublished: mustTime(t, "2026-08-01T00:00:00Z"),
				Candidates: []Candidate{
					{Version: "v1.0.1", UpdateType: "patch", Published: mustTime(t, "2026-09-01T00:00:00Z")},
					{Version: "v1.1.0", UpdateType: "minor"},
				},
			},
			wantOffered: []string{"patch v1.0.1", "minor v1.1.0"},
			wantUndated: []string{"v1.1.0"},
		},
		{
			name: "walk continues past a withheld version to an undated one",
			lookup: Lookup{
				Current: "v1.0.0", CurrentPublished: mustTime(t, "2026-08-01T00:00:00Z"),
				Candidates: []Candidate{
					{Version: "v1.2.0", UpdateType: "minor"},
					{Version: "v1.3.0", UpdateType: "minor", Published: mustTime(t, "2025-01-01T00:00:00Z")},
				},
			},
			wantOffered:   []string{"minor v1.2.0"},
			wantPredating: []string{"minor v1.3.0"},
			wantUndated:   []string{"v1.2.0"},
		},
		{
			name:   "no candidates",
			lookup: Lookup{Current: "v1.0.0", CurrentPublished: mustTime(t, "2026-08-01T00:00:00Z")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := screen(tt.lookup)
			if got := versions(s.offered); !reflect.DeepEqual(got, nonNil(tt.wantOffered)) {
				t.Errorf("offered = %v, want %v", got, tt.wantOffered)
			}
			if got := versions(s.predating); !reflect.DeepEqual(got, nonNil(tt.wantPredating)) {
				t.Errorf("predating = %v, want %v", got, tt.wantPredating)
			}
			if got := nonNil(s.undated); !reflect.DeepEqual(got, nonNil(tt.wantUndated)) {
				t.Errorf("undated = %v, want %v", got, tt.wantUndated)
			}
			if s.currentUndated != tt.wantNoCurrent {
				t.Errorf("currentUndated = %v, want %v", s.currentUndated, tt.wantNoCurrent)
			}
		})
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// screen groups by update type through a map, so its output order must come
// from the final sort and never from map iteration.
func TestScreenIsDeterministic(t *testing.T) {
	l := Lookup{
		Current: "v1.0.0", CurrentPublished: mustTime(t, "2026-08-01T00:00:00Z"),
		Candidates: []Candidate{
			{Version: "v1.0.1", UpdateType: "patch"},
			{Version: "v1.1.0", UpdateType: "minor"},
			{Version: "v2.0.0", UpdateType: "major"},
			{Version: "v3.0.0", UpdateType: "major", Published: mustTime(t, "2025-01-01T00:00:00Z")},
			{Version: "v1.2.0", UpdateType: "minor", Published: mustTime(t, "2025-01-01T00:00:00Z")},
		},
	}
	first := screen(l)
	for range 50 {
		if got := screen(l); !reflect.DeepEqual(got, first) {
			t.Fatalf("screen output varied between calls:\n  %+v\n  %+v", first, got)
		}
	}
	if got, want := first.undated, []string{"v1.0.1", "v1.1.0", "v2.0.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("undated = %v, want %v", got, want)
	}
}

func TestScreeningUnscreened(t *testing.T) {
	tests := []struct {
		name string
		s    screening
		want string
	}{
		{"fully screened", screening{}, ""},
		{"undated current", screening{currentUndated: true},
			"no publish date for current v1.0.0; no candidate was checked against it"},
		{"undated candidates", screening{undated: []string{"v1.0.1", "v1.1.0"}},
			"no publish date for v1.0.1, v1.1.0; offered without the check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.unscreened("v1.0.0"); got != tt.want {
				t.Errorf("unscreened = %q, want %q", got, tt.want)
			}
		})
	}
}
