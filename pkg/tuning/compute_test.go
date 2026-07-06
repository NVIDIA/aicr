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

package tuning_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/tuning"
)

// wantRow is the version-agnostic shape of a row: the tuple that does NOT change
// on a pin bump. Exact versions are guarded by the committed doc (make
// tuning-check) and the BOM cross-check below.
type wantRow struct {
	service, accelerator, profile string
	setupName, tuningName         string
}

func TestCompute_Structure(t *testing.T) {
	report, err := tuning.Compute(context.Background(), tuning.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	want := []wantRow{
		{"bcm", "*", "h100", "nvidia-setup", ""},
		{"bcm", "h100", "-", "nvidia-setup", ""},
		{"eks", "a100", "h100", "nvidia-setup", "nvidia-tuned"},
		{"eks", "gb200", "-", "nvidia-setup", "nvidia-tuned"},
		{"eks", "h100", "-", "nvidia-setup", "nvidia-tuned"},
		{"eks", "h200", "h100", "nvidia-setup", "nvidia-tuned"},
		{"eks", "rtx-pro-6000", "generic", "", "nvidia-tuned"},
		{"gke", "a100", "h100", "", "nvidia-tuning-gke"},
		{"gke", "b200", "-", "", "nvidia-tuning-gke"},
		{"gke", "h100", "-", "", "nvidia-tuning-gke"},
	}
	if len(report.Rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(report.Rows), len(want), report.Rows)
	}
	for i, w := range want {
		r := report.Rows[i]
		mismatch := r.Service != w.service || r.Accelerator != w.accelerator || r.Profile != w.profile ||
			r.Setup.Name != w.setupName || r.Tuning.Name != w.tuningName

		if mismatch {
			t.Errorf("row %d = %+v, want %+v", i, r, w)
		}
	}
}

func TestCompute_VersionsMatchBOM(t *testing.T) {
	// Each extracted pin must appear as image:version in the committed BOM, so
	// the two generated docs cannot drift. CWD during `go test` is the package
	// dir, so the repo BOM is two levels up.
	bom, err := os.ReadFile("../../docs/user/container-images.md")
	if err != nil {
		t.Fatalf("read BOM: %v", err)
	}
	report, err := tuning.Compute(context.Background(), tuning.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	seen := map[string]struct{}{}
	for _, r := range report.Rows {
		for _, p := range []tuning.PackagePin{r.Setup, r.Tuning} {
			if p.Name == "" {
				continue
			}
			ref := p.Name + ":" + p.Version
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			if !strings.Contains(string(bom), ref) {
				t.Errorf("pin %q not found in container-images.md (BOM drift)", ref)
			}
		}
	}
}
