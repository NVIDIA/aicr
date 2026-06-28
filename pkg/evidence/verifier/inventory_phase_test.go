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

package verifier

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// TestCheckPhaseDigests_Match confirms a freshly built bundle's on-disk CTRF
// report digests match the predicate's recorded per-phase CTRFDigest.
func TestCheckPhaseDigests_Match(t *testing.T) {
	summary := summaryDirOf(t, buildTestBundle(t))
	mat := &MaterializedBundle{BundleDir: summary}

	pred, err := loadUnsignedPredicate(mat)
	if err != nil {
		t.Fatalf("loadUnsignedPredicate: %v", err)
	}

	rows, err := CheckPhaseDigests(mat, pred)
	if err != nil {
		t.Fatalf("CheckPhaseDigests = %v (rows %+v), want nil", err, rows)
	}
}

// TestCheckPhaseDigests_TamperedReportFails verifies that mutating the on-disk
// ctrf/<phase>.json after the predicate is captured is detected — the
// predicate's CTRFDigest no longer matches the report bytes.
func TestCheckPhaseDigests_TamperedReportFails(t *testing.T) {
	summary := summaryDirOf(t, buildTestBundle(t))
	mat := &MaterializedBundle{BundleDir: summary}

	pred, err := loadUnsignedPredicate(mat)
	if err != nil {
		t.Fatalf("loadUnsignedPredicate: %v", err)
	}

	ctrfPath := filepath.Join(summary, filepath.FromSlash(attestation.CTRFRelPath(attestation.PhaseDeployment)))
	if writeErr := os.WriteFile(ctrfPath, []byte(`{"tampered":true}`+"\n"), 0o600); writeErr != nil {
		t.Fatalf("tamper ctrf report: %v", writeErr)
	}

	rows, err := CheckPhaseDigests(mat, pred)
	if err == nil {
		t.Fatalf("CheckPhaseDigests = nil, want mismatch error")
	}
	found := false
	for _, r := range rows {
		if strings.Contains(r.Value, "ctrfDigest mismatch") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a ctrfDigest mismatch row; got %+v", rows)
	}
}

// TestCheckPhaseDigests_MissingReportFails verifies that a predicate naming a
// phase whose report file is absent fails closed rather than passing.
func TestCheckPhaseDigests_MissingReportFails(t *testing.T) {
	summary := summaryDirOf(t, buildTestBundle(t))
	mat := &MaterializedBundle{BundleDir: summary}

	pred, err := loadUnsignedPredicate(mat)
	if err != nil {
		t.Fatalf("loadUnsignedPredicate: %v", err)
	}

	ctrfPath := filepath.Join(summary, filepath.FromSlash(attestation.CTRFRelPath(attestation.PhaseDeployment)))
	if err := os.Remove(ctrfPath); err != nil {
		t.Fatalf("remove ctrf report: %v", err)
	}

	if _, err := CheckPhaseDigests(mat, pred); err == nil {
		t.Fatalf("CheckPhaseDigests = nil, want error for missing report")
	}
}

// TestCheckPhaseDigests_EmptyDigestFails verifies that a predicate phase
// summary carrying no CTRFDigest is rejected.
func TestCheckPhaseDigests_EmptyDigestFails(t *testing.T) {
	summary := summaryDirOf(t, buildTestBundle(t))
	mat := &MaterializedBundle{BundleDir: summary}

	pred, err := loadUnsignedPredicate(mat)
	if err != nil {
		t.Fatalf("loadUnsignedPredicate: %v", err)
	}
	ps := pred.Phases[attestation.PhaseDeployment]
	ps.CTRFDigest = ""
	pred.Phases[attestation.PhaseDeployment] = ps

	rows, err := CheckPhaseDigests(mat, pred)
	if err == nil {
		t.Fatalf("CheckPhaseDigests = nil, want error for empty digest")
	}
	found := false
	for _, r := range rows {
		if strings.Contains(r.Value, "no ctrfDigest") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a missing-digest row; got %+v", rows)
	}
}

// TestCheckPhaseDigests_InvalidArgs covers the nil-guard paths.
func TestCheckPhaseDigests_InvalidArgs(t *testing.T) {
	if _, err := CheckPhaseDigests(nil, &attestation.Predicate{}); err == nil {
		t.Errorf("CheckPhaseDigests(nil mat) = nil, want error")
	}
	if _, err := CheckPhaseDigests(&MaterializedBundle{BundleDir: t.TempDir()}, nil); err == nil {
		t.Errorf("CheckPhaseDigests(nil pred) = nil, want error")
	}
}
