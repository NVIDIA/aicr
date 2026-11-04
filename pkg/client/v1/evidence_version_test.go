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

package aicr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func TestRenderCNCFEvidenceWithOptions(t *testing.T) {
	t.Parallel()

	report := &ctrf.Report{}
	report.Results.Tests = []ctrf.TestResult{
		{Name: "dra-support", Status: ctrf.StatusPassed, Duration: 1000},
		{Name: "gang-scheduling", Status: ctrf.StatusPassed, Duration: 1000},
	}

	tests := []struct {
		name          string
		opts          RenderCNCFEvidenceOptions
		wantChecklist string
		wantDRA       string // how index.md lists dra_support
	}{
		{"zero options render the latest", RenderCNCFEvidenceOptions{}, "**Checklist:** " + string(cncf.LatestVersion), "## Supporting Evidence"},
		{"explicit v1.35 keeps dra_support a requirement", RenderCNCFEvidenceOptions{ConformanceVersion: "v1.35"}, "**Checklist:** v1.35", "| `dra_support` | MUST |"},
		{"explicit version accepts the bare form", RenderCNCFEvidenceOptions{ConformanceVersion: "1.36"}, "**Checklist:** v1.36", "## Supporting Evidence"},
		{"kubernetes version selects its minor", RenderCNCFEvidenceOptions{KubernetesVersion: "1.35.4-eks-abc"}, "**Checklist:** v1.35", "| `dra_support` | MUST |"},
		{"explicit version wins over kubernetes version", RenderCNCFEvidenceOptions{ConformanceVersion: "v1.37", KubernetesVersion: "v1.35.0"}, "**Checklist:** v1.37", "## Supporting Evidence"},
		{"kubernetes minor newer than any carried checklist renders the latest", RenderCNCFEvidenceOptions{KubernetesVersion: "v1.40.0"}, "**Checklist:** " + string(cncf.LatestVersion), "## Supporting Evidence"},
		{"kubernetes minor older than any carried checklist renders the oldest, not the latest", RenderCNCFEvidenceOptions{KubernetesVersion: "v1.34.9"}, "**Checklist:** v1.35", "| `dra_support` | MUST |"},
		{"kubernetes minor at the recipe floor renders v1.35", RenderCNCFEvidenceOptions{KubernetesVersion: "v1.32.4"}, "**Checklist:** v1.35", "| `dra_support` | MUST |"},
		{"unparseable kubernetes version falls back to the latest", RenderCNCFEvidenceOptions{KubernetesVersion: "unknown"}, "**Checklist:** " + string(cncf.LatestVersion), "## Supporting Evidence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := (&Client{}).RenderCNCFEvidenceWithOptions(context.Background(), report, dir, tt.opts); err != nil {
				t.Fatalf("RenderCNCFEvidenceWithOptions: %v", err)
			}
			index, err := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{tt.wantChecklist, tt.wantDRA} {
				if !strings.Contains(string(index), want) {
					t.Errorf("index.md missing %q:\n%s", want, index)
				}
			}
		})
	}

	t.Run("unsupported explicit version is refused before writing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		err := (&Client{}).RenderCNCFEvidenceWithOptions(context.Background(), report, dir,
			RenderCNCFEvidenceOptions{ConformanceVersion: "v1.38"})
		requireCode(t, err, aicrerrors.ErrCodeInvalidRequest)
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("evidence dir not left empty: %v", entries)
		}
	})
}

func TestSnapshotKubernetesVersion(t *testing.T) {
	t.Parallel()

	withServer := func(version string) *Snapshot {
		m := measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("server").
				Set(measurement.KeyVersion, measurement.Str(version))).
			Build()
		return WrapSnapshot(&snapshotter.Snapshot{Measurements: []*measurement.Measurement{m}})
	}

	tests := []struct {
		name string
		snap *Snapshot
		want string
	}{
		{"nil receiver", nil, ""},
		{"no measurements", &Snapshot{}, ""},
		{"server version with v prefix", withServer("v1.36.3"), "1.36.3"},
		{"server version with build metadata", withServer("v1.35.2-eks-1f2a3b"), "1.35.2-eks-1f2a3b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.snap.KubernetesVersion(); got != tt.want {
				t.Errorf("KubernetesVersion() = %q, want %q", got, tt.want)
			}
		})
	}

	// The facade value feeds the renderer option end to end.
	if v, ok := cncf.VersionForKubernetes(withServer("v1.36.3").KubernetesVersion()); !ok || v != cncf.Version136 {
		t.Errorf("VersionForKubernetes(KubernetesVersion()) = (%q, %v)", v, ok)
	}
}
