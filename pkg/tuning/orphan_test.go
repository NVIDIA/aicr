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

package tuning

import (
	"context"
	"io/fs"
	"path"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// nwcManifestsDirNoSlash is the manifests directory relative to the embedded
// recipes FS root (nwcManifestsDir without its trailing slash), for fs.ReadDir.
const nwcManifestsDirNoSlash = "components/nodewright-customizations/manifests"

// TestNoOrphanedTuningManifests asserts every manifest under the
// nodewright-customizations manifests directory is referenced by at least one
// resolved leaf recipe, so dead tuning/setup manifests do not silently
// accumulate. Deliberate placeholders are allowlisted.
func TestNoOrphanedTuningManifests(t *testing.T) {
	// Manifests intentionally kept without a referencing leaf. Add an entry
	// here (with a reason) only when a manifest is a deliberate placeholder.
	allowedOrphans := map[string]struct{}{
		// Placeholder used until a full package suite can be tested for a
		// target; see the "## No-op" section of nodewright.md.
		"no-op.yaml": {},
	}

	leaves, err := recipe.ResolveLeaves(context.Background(), recipe.ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}

	referenced := map[string]struct{}{}
	for _, leaf := range leaves {
		if leaf.Result == nil {
			continue
		}
		ref, ok := findComponentRef(leaf.Result.ComponentRefs, nwcComponent)
		if !ok {
			continue
		}
		if m := selectTuningManifest(ref.ManifestFiles); m != "" {
			referenced[path.Base(m)] = struct{}{}
		}
	}

	entries, err := fs.ReadDir(recipe.GetEmbeddedFS(), nwcManifestsDirNoSlash)
	if err != nil {
		t.Fatalf("read %s: %v", nwcManifestsDirNoSlash, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if ext := path.Ext(name); ext != ".yaml" && ext != ".yml" {
			continue
		}
		if _, ok := referenced[name]; ok {
			continue
		}
		if _, ok := allowedOrphans[name]; ok {
			continue
		}
		t.Errorf("orphaned manifest %q: referenced by no resolved leaf and not in the intentional-placeholder allowlist — remove it, or add it to allowedOrphans (with a reason) if it is deliberate", name)
	}
}

func TestSelectTuningManifest(t *testing.T) {
	const (
		tuningManifest    = nwcManifestsDir + "tuning.yaml"
		okeTuningManifest = nwcManifestsDir + "tuning-oke-gb300.yaml"
		otherComponent    = "components/gpu-operator/manifests/dcgm-exporter.yaml"
		aksHardening      = nwcManifestsDir + "toolkit-hardening-aks.yaml"
		okeHardening      = nwcManifestsDir + "toolkit-hardening-oke.yaml"
	)
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{"none", nil, ""},
		{"other component only", []string{otherComponent}, ""},
		{"tuning only", []string{tuningManifest}, tuningManifest},
		{"toolkit hardening only", []string{aksHardening}, aksHardening},
		{"leaf manifest after hardening", []string{aksHardening, tuningManifest}, tuningManifest},
		{"leaf manifest before hardening", []string{tuningManifest, aksHardening}, tuningManifest},
		{"OKE toolkit hardening only", []string{okeHardening}, okeHardening},
		{"OKE leaf manifest after hardening", []string{okeHardening, okeTuningManifest}, okeTuningManifest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectTuningManifest(tt.files); got != tt.want {
				t.Errorf("selectTuningManifest(%v) = %q, want %q", tt.files, got, tt.want)
			}
		})
	}
}
