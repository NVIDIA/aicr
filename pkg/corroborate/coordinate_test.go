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

package corroborate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestCriteriaFromCoordinate(t *testing.T) {
	tests := []struct {
		name    string
		coord   recipe.Coordinate
		want    recipe.Criteria
		wantErr bool
	}{
		{
			name:  "eks h100 ubuntu training kubeflow",
			coord: recipe.Coordinate{Group: "eks", Dashboard: "h100-ubuntu", Tab: "training-kubeflow"},
			want: recipe.Criteria{Service: "eks", Accelerator: "h100", OS: "ubuntu",
				Intent: "training", Platform: "kubeflow"},
		},
		{
			name:  "gke h100 cos inference dynamo",
			coord: recipe.Coordinate{Group: "gke", Dashboard: "h100-cos", Tab: "inference-dynamo"},
			want: recipe.Criteria{Service: "gke", Accelerator: "h100", OS: "cos",
				Intent: "inference", Platform: "dynamo"},
		},
		{
			name:  "bare intent yields empty platform",
			coord: recipe.Coordinate{Group: "gke", Dashboard: "h100-cos", Tab: "training"},
			want: recipe.Criteria{Service: "gke", Accelerator: "h100", OS: "cos",
				Intent: "training", Platform: ""},
		},
		{
			name:  "multi-token accelerator splits on the os suffix",
			coord: recipe.Coordinate{Group: "eks", Dashboard: "rtx-pro-6000-ubuntu", Tab: "inference-nim"},
			want: recipe.Criteria{Service: "eks", Accelerator: "rtx-pro-6000", OS: "ubuntu",
				Intent: "inference", Platform: "nim"},
		},
		{
			name:  "unknown but well-formed os inverts via the last-hyphen fallback (open taxonomy)",
			coord: recipe.Coordinate{Group: "eks", Dashboard: "h100-flatcar", Tab: "training"},
			want: recipe.Criteria{Service: "eks", Accelerator: "h100", OS: "flatcar",
				Intent: "training", Platform: ""},
		},
		{
			name:  "unknown but well-formed intent inverts via the first-hyphen fallback",
			coord: recipe.Coordinate{Group: "eks", Dashboard: "h100-ubuntu", Tab: "benchmark-suite"},
			want: recipe.Criteria{Service: "eks", Accelerator: "h100", OS: "ubuntu",
				Intent: "benchmark", Platform: "suite"},
		},
		{
			name:    "dashboard with no hyphen fails",
			coord:   recipe.Coordinate{Group: "eks", Dashboard: "h100", Tab: "training"},
			wantErr: true,
		},
		{
			name:    "empty tab fails",
			coord:   recipe.Coordinate{Group: "eks", Dashboard: "h100-ubuntu", Tab: ""},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := criteriaFromCoordinate(tt.coord)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("criteria = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestMappingGoldenReuse exercises the canonical recipe->coordinate golden
// (Contract 1) for both launch clouds: the shared recipe.CoordinateFor maps
// each criteria to its frozen path, and criteriaFromCoordinate round-trips it
// back. The authoritative golden lives in pkg/recipe; this proves GP4 imports
// (never forks) that mapping and inverts it consistently.
func TestMappingGoldenReuse(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden-coordinates.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden struct {
		Vectors []struct {
			Name     string            `json:"name"`
			Criteria map[string]string `json:"criteria"`
			Path     string            `json:"path"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(golden.Vectors) == 0 {
		t.Fatal("golden has no vectors")
	}

	for _, v := range golden.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			crit := recipe.Criteria{
				Service:     recipe.CriteriaServiceType(v.Criteria["service"]),
				Accelerator: recipe.CriteriaAcceleratorType(v.Criteria["accelerator"]),
				OS:          recipe.CriteriaOSType(v.Criteria["os"]),
				Intent:      recipe.CriteriaIntentType(v.Criteria["intent"]),
				Platform:    recipe.CriteriaPlatformType(v.Criteria["platform"]),
			}
			co, err := recipe.CoordinateFor(&crit)
			if err != nil {
				t.Fatalf("CoordinateFor: %v", err)
			}
			if co.Path() != v.Path {
				t.Fatalf("CoordinateFor path = %q, want %q", co.Path(), v.Path)
			}
			got, err := criteriaFromCoordinate(co)
			if err != nil {
				t.Fatalf("criteriaFromCoordinate: %v", err)
			}
			if got != crit {
				t.Errorf("round-trip criteria = %+v, want %+v", got, crit)
			}
		})
	}
}

func TestLabelFor(t *testing.T) {
	tests := []struct {
		name string
		meta RunMeta
		want string
	}{
		{
			name: "first-party fixed label",
			meta: RunMeta{Signer: RunMetaSigner{Class: string(ClassFirstParty),
				Identity: "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main"}},
			want: "NVIDIA UAT",
		},
		{
			name: "github org/repo",
			meta: RunMeta{Signer: RunMetaSigner{Class: string(ClassCommunity),
				Identity: "https://github.com/acme-gpu/aicr-attest/.github/workflows/attest.yaml@refs/heads/main"}},
			want: "acme-gpu/aicr-attest",
		},
		{
			name: "host only",
			meta: RunMeta{Signer: RunMetaSigner{Class: string(ClassPartner),
				Identity: "https://oidc.coreweave-lab.example/attest"}},
			want: "oidc.coreweave-lab.example",
		},
		{
			name: "non-code-host deep path is not mislabeled as org/repo",
			meta: RunMeta{Signer: RunMetaSigner{Class: string(ClassPartner),
				Identity: "https://oidc.partner.example/tenant/project/attest"}},
			want: "oidc.partner.example",
		},
		{
			name: "falls back to idHash when identity is empty",
			meta: RunMeta{Signer: RunMetaSigner{Class: string(ClassCommunity), IDHash: "deadbeef"}},
			want: "deadbeef",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := labelFor(tt.meta, Class(tt.meta.Signer.Class)); got != tt.want {
				t.Errorf("labelFor = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatWhen(t *testing.T) {
	if got := formatWhen("2026-06-20T03:14:07Z"); got != "2026-06-20 03:14 UTC" {
		t.Errorf("formatWhen = %q", got)
	}
	if got := formatWhenDate("2026-06-20T03:14:07Z"); got != "2026-06-20" {
		t.Errorf("formatWhenDate = %q", got)
	}
	// Unparseable input passes through verbatim (never panics, never the clock).
	if got := formatWhen("not-a-time"); got != "not-a-time" {
		t.Errorf("formatWhen passthrough = %q", got)
	}
	if got := formatWhenDate("not-a-time"); got != "not-a-time" {
		t.Errorf("formatWhenDate passthrough = %q", got)
	}
}
