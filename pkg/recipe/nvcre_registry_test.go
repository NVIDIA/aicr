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

package recipe

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/recipes"
	"gopkg.in/yaml.v3"
)

// TestNVCRERegisteredWithoutOverlay keeps the public CRE chart installable
// from the registry without making it part of any shipped overlay or mixin.
func TestNVCRERegisteredWithoutOverlay(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get("nvcre")
	if comp == nil {
		t.Fatal("nvcre is missing from recipes/registry.yaml")
	}
	if !comp.OwnsCRDs {
		t.Error("ownsCRDs must stay true: the chart ships nvcre.nvidia.com CRDs")
	}
	if !comp.HasSelfRefCRDs {
		t.Error("hasSelfRefCRDs must stay true: templates/ render LogProfile CRs of a CRD the chart also ships")
	}
	if comp.HealthCheck.AssertFile != "checks/nvcre/health-check.yaml" {
		t.Errorf("nvcre assertFile = %q, want checks/nvcre/health-check.yaml", comp.HealthCheck.AssertFile)
	}
	if got := comp.GetSystemNodeSelectorPaths(); len(got) != 0 {
		t.Errorf("system nodeSelectorPaths = %v, want empty (chart has no manager.nodeSelector)", got)
	}

	type overlaySpec struct {
		Spec struct {
			ComponentRefs []struct {
				Name string `yaml:"name"`
			} `yaml:"componentRefs"`
		} `yaml:"spec"`
	}

	for _, dir := range []string{"overlays", "mixins"} {
		err := fs.WalkDir(recipes.FS, dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
				return nil
			}
			raw, err := recipes.FS.ReadFile(path)
			if err != nil {
				return err
			}
			var doc overlaySpec
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Errorf("%s: unmarshal: %v", path, err)
				return nil
			}
			for _, ref := range doc.Spec.ComponentRefs {
				if ref.Name == "nvcre" {
					t.Errorf("%s declares componentRef nvcre; CRE must stay opt-in", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}
