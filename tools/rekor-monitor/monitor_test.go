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

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sigstore/rekor-monitor/pkg/identity"
	tlog "github.com/transparency-dev/formats/log"
)

func TestBuildMonitoredValues(t *testing.T) {
	tests := []struct {
		name        string
		certSubject string
		certIssuer  string
		wantLen     int
		wantErr     bool
	}{
		{name: "empty is consistency-only", certSubject: "", wantLen: 0},
		{name: "issuer without subject errors", certSubject: "", certIssuer: "^iss$", wantErr: true},
		{name: "subject only", certSubject: "^sub$", wantLen: 1},
		{name: "subject and issuer", certSubject: "^sub$", certIssuer: "^iss$", wantLen: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mv, err := buildMonitoredValues(tt.certSubject, tt.certIssuer)
			if (err != nil) != tt.wantErr {
				t.Fatalf("buildMonitoredValues() error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(mv) != tt.wantLen {
				t.Fatalf("len(mv) = %d, want %d", len(mv), tt.wantLen)
			}
			if tt.wantLen == 1 {
				cid, ok := mv[0].(identity.CertIdentityValue)
				if !ok {
					t.Fatalf("mv[0] type = %T, want CertIdentityValue", mv[0])
				}
				if cid.CertSubject != tt.certSubject {
					t.Errorf("CertSubject = %q, want %q", cid.CertSubject, tt.certSubject)
				}
				if tt.certIssuer == "" && len(cid.Issuers) != 0 {
					t.Errorf("Issuers = %v, want empty", cid.Issuers)
				}
				if tt.certIssuer != "" && (len(cid.Issuers) != 1 || cid.Issuers[0] != tt.certIssuer) {
					t.Errorf("Issuers = %v, want [%q]", cid.Issuers, tt.certIssuer)
				}
			}
		})
	}
}

func TestMonitorWatchesIdentity(t *testing.T) {
	tests := []struct {
		name       string
		identities identity.MonitoredValues
		want       bool
	}{
		{name: "no identities", want: false},
		{name: "one identity", identities: identity.MonitoredValues{identity.CertIdentityValue{CertSubject: "^x$"}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&monitor{identities: tt.identities}).watchesIdentity(); got != tt.want {
				t.Errorf("watchesIdentity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScanWindow(t *testing.T) {
	cp := func(size uint64) *tlog.Checkpoint { return &tlog.Checkpoint{Size: size} }
	tests := []struct {
		name      string
		prev, cur *tlog.Checkpoint
		wantStart int64
		wantEnd   int64
		wantOK    bool
	}{
		// GetEntriesByIndexRange is exclusive-start, so start = prev.Size-1 makes
		// the effective scanned range (start, end] = [prev.Size, cur.Size-1].
		{name: "first run (nil prev)", prev: nil, cur: cp(100), wantOK: false},
		{name: "nil cur", prev: cp(100), cur: nil, wantOK: false},
		{name: "zero prev size", prev: cp(0), cur: cp(100), wantOK: false},
		{name: "multiple new entries", prev: cp(100), cur: cp(150), wantStart: 99, wantEnd: 149, wantOK: true},
		{name: "single new entry covers index prev.Size", prev: cp(100), cur: cp(101), wantStart: 99, wantEnd: 100, wantOK: true},
		{name: "no growth", prev: cp(100), cur: cp(100), wantOK: false},
		{name: "shrunk", prev: cp(150), cur: cp(100), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, ok := scanWindow(tt.prev, tt.cur)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("window = [%d, %d], want [%d, %d]", start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestFilterKnownReleases(t *testing.T) {
	san := func(tag string) string {
		return "https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/" + tag
	}
	mk := func(subjects ...string) []identity.MonitoredIdentity {
		entries := make([]identity.LogEntry, 0, len(subjects))
		for _, s := range subjects {
			entries = append(entries, identity.LogEntry{CertSubject: s})
		}
		return []identity.MonitoredIdentity{{FoundIdentityEntries: entries}}
	}
	known := map[string]bool{"v0.18.0-rc1": true}

	t.Run("known release suppressed -> no residual", func(t *testing.T) {
		got := filterKnownReleases(mk(san("v0.18.0-rc1"), san("v0.18.0-rc1")), known)
		if len(got) != 0 {
			t.Errorf("residual = %v, want empty", got)
		}
	})
	t.Run("unknown tag kept", func(t *testing.T) {
		got := filterKnownReleases(mk(san("v9.9.9-attacker")), known)
		if len(got) != 1 || len(got[0].FoundIdentityEntries) != 1 {
			t.Errorf("residual = %v, want 1 entry", got)
		}
	})
	t.Run("malformed SAN kept (fail closed)", func(t *testing.T) {
		got := filterKnownReleases(mk("no-tag-ref-here"), known)
		if len(got) != 1 {
			t.Errorf("residual = %v, want kept", got)
		}
	})
	t.Run("mixed: only unknown survives", func(t *testing.T) {
		got := filterKnownReleases(mk(san("v0.18.0-rc1"), san("v9.9.9-attacker")), known)
		ok := len(got) == 1 && len(got[0].FoundIdentityEntries) == 1 &&
			got[0].FoundIdentityEntries[0].CertSubject == san("v9.9.9-attacker")
		if !ok {
			t.Errorf("residual = %v, want only the attacker entry", got)
		}
	})
	t.Run("empty tag ref kept (fail closed)", func(t *testing.T) {
		got := filterKnownReleases(mk(san("")), map[string]bool{"": true, "v0.18.0-rc1": true})
		if len(got) != 1 {
			t.Errorf("residual = %v, want kept (empty tag must not be suppressed even if \"\" is in the set)", got)
		}
	})
	t.Run("empty known set -> passthrough", func(t *testing.T) {
		in := mk(san("v0.18.0-rc1"))
		got := filterKnownReleases(in, map[string]bool{})
		if len(got) != 1 {
			t.Errorf("residual = %v, want passthrough", got)
		}
	})
}

func TestOutcomeHasFindings(t *testing.T) {
	tests := []struct {
		name   string
		found  []identity.MonitoredIdentity
		failed []identity.FailedLogEntry
		want   bool
	}{
		{name: "clean", want: false},
		{name: "match", found: []identity.MonitoredIdentity{{}}, want: true},
		{name: "failed entry", failed: []identity.FailedLogEntry{{}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := outcome{found: tt.found, failed: tt.failed}
			if got := o.hasFindings(); got != tt.want {
				t.Errorf("hasFindings() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOutcomeReport(t *testing.T) {
	cp := func(size uint64) *tlog.Checkpoint { return &tlog.Checkpoint{Size: size} }
	tests := []struct {
		name string
		out  outcome
		want []string // substrings expected in the report
	}{
		{
			name: "baseline (first run)",
			out:  outcome{prev: nil, cur: cp(100)},
			want: []string{"baseline established at tree size 100"},
		},
		{
			name: "consistency only, no scan",
			out:  outcome{prev: cp(100), cur: cp(150)},
			want: []string{"consistency verified: 100 -> 150"},
		},
		{
			name: "clean identity scan",
			out:  outcome{prev: cp(100), cur: cp(150), scanned: true, from: 99, to: 149},
			want: []string{"consistency verified: 100 -> 150", "identity scan [99, 149]: no matching entries"},
		},
		{
			name: "finding with match and failed entry",
			out: outcome{prev: cp(100), cur: cp(150), scanned: true, from: 100, to: 149,
				found: []identity.MonitoredIdentity{{}}, failed: []identity.FailedLogEntry{{}}},
			want: []string{"ALERT", "MATCH:", "FAILED:"},
		},
		{
			name: "shard rotation",
			out: outcome{prev: &tlog.Checkpoint{Origin: "log2025-1", Size: 100},
				cur: &tlog.Checkpoint{Origin: "log2026-1", Size: 5}, rotated: true},
			want: []string{"shard rotation detected", "log2025-1", "log2026-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tt.out.report(&buf)
			got := buf.String()
			for _, sub := range tt.want {
				if !strings.Contains(got, sub) {
					t.Errorf("report missing %q\ngot: %s", sub, got)
				}
			}
		})
	}
}
