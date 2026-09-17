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
	wr := localformat.WriteResult{
		Folders: []localformat.Folder{
			{Index: 1, Dir: "001-cert-manager", Name: "cert-manager",
				Namespace: "cert-manager", Parent: "cert-manager"},
			{Index: 2, Dir: "002-gpu-operator", Name: "gpu-operator",
				Namespace: "gpu-operator", Parent: "gpu-operator"},
			{Index: 3, Dir: "003-gpu-operator-post", Name: "gpu-operator-post",
				Namespace: "gpu-operator", Parent: "gpu-operator"},
		},
	}

	got := wr.Releases()
	if len(got) != 3 {
		t.Fatalf("releases = %d, want 3", len(got))
	}
	// Folder order is deployment order and must survive the mapping.
	if got[0].Name != "cert-manager" || got[2].Name != "gpu-operator-post" {
		t.Errorf("order not preserved: %v", got)
	}
	// An injected -post folder is its own release but names its parent,
	// which is what lets a consumer group the three gpu-operator entries.
	if got[2].Component != "gpu-operator" {
		t.Errorf("injected folder Component = %q, want gpu-operator", got[2].Component)
	}
	if got[2].Path != "003-gpu-operator-post" {
		t.Errorf("Path = %q, want 003-gpu-operator-post", got[2].Path)
	}
}
