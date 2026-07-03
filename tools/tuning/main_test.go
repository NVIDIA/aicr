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
	"os"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/tuning"
)

func TestRenderTable_Deterministic(t *testing.T) {
	report := &tuning.Report{
		SchemaVersion: tuning.SchemaVersion,
		Rows: []tuning.Row{
			{Service: "bcm", Accelerator: "*", Profile: "h100",
				Setup: tuning.PackagePin{Name: "nvidia-setup", Version: "0.3.0"}},
			{Service: "eks", Accelerator: "h100", Profile: "-",
				Setup:  tuning.PackagePin{Name: "nvidia-setup", Version: "0.2.2"},
				Tuning: tuning.PackagePin{Name: "nvidia-tuned", Version: "0.3.0"}},
			{Service: "gke", Accelerator: "a100", Profile: "h100",
				Tuning: tuning.PackagePin{Name: "nvidia-tuning-gke", Version: "0.1.2"}},
		},
	}
	var buf bytes.Buffer
	if err := renderTable(&buf, report, markdownOptions{Deterministic: true, NoTitle: true}); err != nil {
		t.Fatalf("renderTable: %v", err)
	}
	got := buf.String()
	want := "\n| Service | Accelerator | Profile | Setup              | Tuning                  |\n" +
		"|---------|-------------|---------|--------------------|-------------------------|\n" +
		"| bcm     | *           | h100    | nvidia-setup 0.3.0 | -                       |\n" +
		"| eks     | h100        | -       | nvidia-setup 0.2.2 | nvidia-tuned 0.3.0      |\n" +
		"| gke     | a100        | h100    | -                  | nvidia-tuning-gke 0.1.2 |\n\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
	// No timestamp/title in deterministic no-title mode.
	if strings.Contains(got, "Generated") || strings.Contains(got, "#") {
		t.Errorf("deterministic no-title output leaked metadata: %q", got)
	}
}

func TestDocMarkersPresent(t *testing.T) {
	// A missing marker makes `make tuning-docs` a silent no-op; guard the doc.
	doc, err := os.ReadFile("../../docs/integrator/components/nodewright.md")
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	for _, marker := range []string{"{/* BEGIN AICR-TUNING */}", "{/* END AICR-TUNING */}"} {
		if !strings.Contains(string(doc), marker) {
			t.Errorf("nodewright.md is missing marker %q", marker)
		}
	}
}
