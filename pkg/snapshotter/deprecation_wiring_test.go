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

package snapshotter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// writeSnapshotWithAPIVersion writes a loadable snapshot carrying v. A snapshot
// with no measurements is rejected before the header is considered, so this
// carries one — the header path is only reachable on an otherwise valid file.
func writeSnapshotWithAPIVersion(t *testing.T, name, v string) string {
	t.Helper()
	snap := NewSnapshot()
	snap.Kind = header.KindSnapshot
	snap.APIVersion = v
	snap.Measurements = []*measurement.Measurement{{
		Type: measurement.TypeK8s,
		Subtypes: []measurement.Subtype{{
			Name: "slinky-slurm",
			Data: map[string]measurement.Reading{
				"collection-state": measurement.Str("absent"),
			},
		}},
	}}
	body, err := serializer.MarshalYAMLDeterministic(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return path
}

// These were the Release N+1 wiring tests, asserting that an archived alpha or
// headerless snapshot still LOADED and merely warned. ADR-022 §3 N+2 (#2417)
// inverted that contract, and they are inverted with it rather than deleted:
// what they exist to catch is unchanged. The gate lives at the call site, so
// without a test at this level the narrowed check is deletable green -- the
// pkg/header unit tests prove the predicate, not that any loader consults it.

// TestLoadFromFileRejectsRetiredAPIVersion pins the acceptance criterion: the
// rejection names the observed value, the expected value, and why an artifact
// that used to load no longer does.
func TestLoadFromFileRejectsRetiredAPIVersion(t *testing.T) {
	path := writeSnapshotWithAPIVersion(t, "legacy-snapshot.yaml", header.RetiredGroupVersionV1Alpha2)

	_, err := LoadFromFile(t.Context(), path)
	if err == nil {
		t.Fatalf("a snapshot stamped %q must be rejected since %s",
			header.RetiredGroupVersionV1Alpha2, header.AlphaRemovedIn)
	}

	got := err.Error()
	// Naming the file is what made the retired warning actionable across a
	// catalog of many snapshots, and the rejection inherits that obligation.
	// Asserted here because nothing else would notice it going missing.
	if !strings.Contains(got, "legacy-snapshot.yaml") {
		t.Errorf("error does not name the file: %q", got)
	}
	// The observed value, not just the expected one: without this a loader that
	// reported the wrong version -- or an empty one -- still satisfies the rest.
	if !strings.Contains(got, header.RetiredGroupVersionV1Alpha2) {
		t.Errorf("error does not name the observed apiVersion %q: %q",
			header.RetiredGroupVersionV1Alpha2, got)
	}
	if !strings.Contains(got, header.GroupVersionV1) {
		t.Errorf("error does not name the stable target %q: %q", header.GroupVersionV1, got)
	}
	if !strings.Contains(got, header.AlphaRemovedIn) {
		t.Errorf("error does not name the removal release %q: %q", header.AlphaRemovedIn, got)
	}
}

// TestLoadFromFileRejectsAbsentAPIVersion covers the other retired shape.
// ADR-011 §3 granted the empty-value tolerance to the snapshot, recipe and
// criteria loaders; ADR-022 §3 retired it at v1.0.0 alongside the alpha values.
func TestLoadFromFileRejectsAbsentAPIVersion(t *testing.T) {
	path := writeSnapshotWithAPIVersion(t, "headerless-snapshot.yaml", "")

	_, err := LoadFromFile(t.Context(), path)
	if err == nil {
		t.Fatalf("a headerless snapshot must be rejected since %s", header.AlphaRemovedIn)
	}

	got := err.Error()
	if !strings.Contains(got, "headerless-snapshot.yaml") {
		t.Errorf("error does not name the file: %q", got)
	}
	// An absent header has no observed value to echo, so the file, the expected
	// value and the release that withdrew the tolerance are the whole
	// actionable payload.
	if !strings.Contains(got, header.GroupVersionV1) {
		t.Errorf("error does not name the stable target %q: %q", header.GroupVersionV1, got)
	}
	if !strings.Contains(got, header.AlphaRemovedIn) {
		t.Errorf("error does not name the removal release %q: %q", header.AlphaRemovedIn, got)
	}
}

// TestLoadFromFileAcceptsTargetAPIVersion is the other half, and the reason the
// two above cannot be satisfied by a loader that rejects everything.
func TestLoadFromFileAcceptsTargetAPIVersion(t *testing.T) {
	path := writeSnapshotWithAPIVersion(t, "current-snapshot.yaml", header.GroupVersionV1)

	if _, err := LoadFromFile(t.Context(), path); err != nil {
		t.Fatalf("target snapshot must load: %v", err)
	}
}
