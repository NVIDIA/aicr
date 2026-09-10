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
	"bytes"
	"log/slog"
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
	// t.TempDir is unique per run, so the dedup subject (which embeds the path)
	// differs on every invocation and `go test -count=2` still sees a warning.
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return path
}

func captureWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestLoadFromFileWarnsOnAlphaAPIVersion binds the snapshot loader to the
// deprecation channel. Without a test at this level the call site is deletable
// green — the pkg/header unit tests prove the helper works, not that any loader
// calls it.
//
// It also pins the Release N+1 acceptance criterion that matters most: an
// archived alpha snapshot must still LOAD. Narrowing that gate is #2417.
func TestLoadFromFileWarnsOnAlphaAPIVersion(t *testing.T) {
	path := writeSnapshotWithAPIVersion(t, "legacy-snapshot.yaml", header.GroupVersion)
	buf := captureWarn(t)

	if _, err := LoadFromFile(t.Context(), path); err != nil {
		t.Fatalf("alpha snapshot must still load until %s: %v", header.AlphaRemovedIn, err)
	}

	got := buf.String()
	if !strings.Contains(got, "legacy-snapshot.yaml") {
		t.Errorf("warning does not name the file: %q", got)
	}
	if !strings.Contains(got, header.AlphaRemovedIn) {
		t.Errorf("warning does not name the removal release %q: %q", header.AlphaRemovedIn, got)
	}
	if !strings.Contains(got, header.GroupVersionV1) {
		t.Errorf("warning does not name the stable target %q: %q", header.GroupVersionV1, got)
	}
}

// TestLoadFromFileIsSilentOnTargetAPIVersion is the other half: a snapshot
// recaptured on v0.22 must not nag. A channel that fires on the value the user
// was just told to adopt teaches them to filter it out.
func TestLoadFromFileIsSilentOnTargetAPIVersion(t *testing.T) {
	path := writeSnapshotWithAPIVersion(t, "current-snapshot.yaml", header.GroupVersionV1)
	buf := captureWarn(t)

	if _, err := LoadFromFile(t.Context(), path); err != nil {
		t.Fatalf("target snapshot must load: %v", err)
	}
	if got := buf.String(); got != "" {
		t.Errorf("target apiVersion must not warn, got: %q", got)
	}
}
