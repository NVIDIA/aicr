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

package localformat_test

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
)

func TestWriteResultReleases(t *testing.T) {
	// Declaration order deliberately disagrees with both alphabetical Name
	// order and numeric Index/Dir order (gpu-operator, gpu-operator-post,
	// cert-manager), so a Releases() that sorted by either field instead of
	// preserving input order would fail the order assertion below.
	wr := localformat.WriteResult{
		Folders: []localformat.Folder{
			{Index: 2, Dir: "002-gpu-operator", Name: "gpu-operator",
				Namespace: "gpu-operator", Parent: "gpu-operator"},
			{Index: 3, Dir: "003-gpu-operator-post", Name: "gpu-operator-post",
				Namespace: "gpu-operator", Parent: "gpu-operator"},
			{Index: 1, Dir: "001-cert-manager", Name: "cert-manager",
				Namespace: "cert-manager", Parent: "cert-manager"},
		},
	}

	got := wr.Releases()
	if len(got) != 3 {
		t.Fatalf("releases = %d, want 3", len(got))
	}
	// Folder order is deployment order and must survive the mapping: the
	// consuming artifact carries no ordinal field, so a consumer reads
	// sequence from list position alone.
	wantOrder := []string{"gpu-operator", "gpu-operator-post", "cert-manager"}
	for i, want := range wantOrder {
		if got[i].Name != want {
			t.Errorf("order not preserved: got[%d].Name = %q, want %q (full: %v)", i, got[i].Name, want, got)
		}
	}
	// An injected -post folder is its own release but names its parent,
	// which is what lets a consumer group the three gpu-operator entries.
	if got[1].Component != "gpu-operator" {
		t.Errorf("injected folder Component = %q, want gpu-operator", got[1].Component)
	}
	if got[1].Path != "003-gpu-operator-post" {
		t.Errorf("Path = %q, want 003-gpu-operator-post", got[1].Path)
	}
	// Manifest stays empty in this mapping: deployers that declare a release
	// through a per-folder file (rather than an orchestration script) set it
	// afterwards, which this helper must not pre-empt.
	for _, r := range got {
		if r.Manifest != "" {
			t.Errorf("Manifest = %q, want empty for %q", r.Manifest, r.Name)
		}
	}
}
