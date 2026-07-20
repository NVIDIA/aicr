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

package testgrid

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/corroborate"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestLinkFor(t *testing.T) {
	co := recipe.Coordinate{Group: "eks", Dashboard: "h100-ubuntu", Tab: "training-kubeflow"}
	want := "https://validation.aicr.run/#/eks/h100-ubuntu/training-kubeflow"
	if got := LinkFor(co); got != want {
		t.Errorf("LinkFor() = %q, want %q", got, want)
	}
	// Construction is pure — no map/clock/registry — so identical input yields
	// identical output by the compiler's guarantees; the golden assertion above
	// is the determinism check.
}

func TestLoadPresence(t *testing.T) {
	p, err := LoadPresence()
	if err != nil {
		t.Fatalf("LoadPresence() error = %v", err)
	}
	paths := p.Paths()
	if len(paths) == 0 {
		t.Fatal("LoadPresence() returned no coordinates")
	}
	// Paths must come back sorted (deterministic bot iteration).
	if !sort.StringsAreSorted(paths) {
		t.Errorf("Paths() not sorted: %v", paths)
	}
	// Every committed path must be well-formed and Has() must agree with Paths().
	for _, path := range paths {
		segs := strings.Split(path, "/")
		if len(segs) != 3 {
			t.Errorf("committed path %q is not 3 segments", path)
			continue
		}
		co := recipe.Coordinate{Group: segs[0], Dashboard: segs[1], Tab: segs[2]}
		if !p.Has(co) {
			t.Errorf("Has(%q) = false, but path is in the manifest", path)
		}
	}
}

func TestParsePresenceRejectsMalformed(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		wantErr  bool
	}{
		{"valid", "coordinates:\n  - eks/h100-ubuntu/training\n", false},
		{"unknown key (typo)", "coordinate:\n  - eks/h100-ubuntu/training\n", true},
		{"absent coordinates list", "other: 1\n", true},
		{"empty coordinates list", "coordinates: []\n", true},
		{"malformed coordinate path", "coordinates:\n  - eks/h100-ubuntu\n", true},
		{"duplicate coordinate", "coordinates:\n  - eks/h100-ubuntu/training\n  - eks/h100-ubuntu/training\n", true},
		{"trailing second document", "coordinates:\n  - eks/h100-ubuntu/training\n---\ngarbage: [\n", true},
		{"not yaml", "\t- : :\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePresence([]byte(tt.manifest))
			if (err != nil) != tt.wantErr {
				t.Errorf("parsePresence() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPresenceHas(t *testing.T) {
	p, err := LoadPresence()
	if err != nil {
		t.Fatalf("LoadPresence() error = %v", err)
	}
	tests := []struct {
		name string
		co   recipe.Coordinate
		want bool
	}{
		{"present (UAT-covered)", recipe.Coordinate{Group: "eks", Dashboard: "h100-ubuntu", Tab: "training-kubeflow"}, true},
		{"absent (no dashboard presence)", recipe.Coordinate{Group: "eks", Dashboard: "gb200-ubuntu", Tab: "training-slurm"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Has(tt.co); got != tt.want {
				t.Errorf("Has(%v) = %v, want %v", tt.co, got, tt.want)
			}
		})
	}
}

func TestLivePaths(t *testing.T) {
	idx := &corroborate.Index{
		Groups: []corroborate.Group{
			{
				Service: "eks",
				Dashboards: []corroborate.Dashboard{
					{
						Accelerator: "h100",
						OS:          "ubuntu",
						Tabs: []corroborate.Tab{
							{Coord: map[string]string{
								"service": "eks", "accelerator": "h100", "os": "ubuntu",
								"intent": "training", "platform": "kubeflow",
							}},
							// A bare-intent tab (no platform).
							{Coord: map[string]string{
								"service": "eks", "accelerator": "h100", "os": "ubuntu",
								"intent": "inference",
							}},
							// A malformed tab (missing os) is skipped, not mis-placed.
							{Coord: map[string]string{
								"service": "eks", "accelerator": "h100", "intent": "training",
							}},
						},
					},
				},
			},
		},
	}
	got := LivePaths(idx)
	want := map[string]struct{}{
		"eks/h100-ubuntu/training-kubeflow": {},
		"eks/h100-ubuntu/inference":         {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LivePaths() = %v, want %v", keys(got), keys(want))
	}
}

func TestLivePathsNil(t *testing.T) {
	if got := LivePaths(nil); len(got) != 0 {
		t.Errorf("LivePaths(nil) = %v, want empty", got)
	}
}

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"well-formed", "eks/h100-ubuntu/training", false},
		{"too few segments", "eks/h100-ubuntu", true},
		{"too many segments", "eks/h100/ubuntu/training", true},
		{"empty interior segment", "eks//training", true},
		{"trailing empty segment", "eks/h100-ubuntu/", true},
		{"leading empty segment", "/h100-ubuntu/training", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
