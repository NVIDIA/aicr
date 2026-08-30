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

package bundler_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Bundle layout is the last of the four surfaces ROADMAP section 1 freezes at
// v1 (issue #2113, scope items 1, 2 and 5's layout half).
//
// An integrator's automation reads paths out of a bundle: a GitOps pipeline
// commits argocd/app-of-apps.yaml, an operator runs helm/deploy.sh, a script
// walks NNN-<component>/values.yaml. None of that was gated. The per-deployer
// golden trees under pkg/bundler/deployer/*/testdata cover a single deployer's
// rendering; nothing asserted the shape of a whole bundle, so a renamed root
// file or a changed directory convention would ship silently.
//
// # Why a fixture recipe rather than the live catalog
//
// Some emitted names are recipe-derived, not layout: flux writes one
// helmrepo-<host>.yaml per chart repository, and helmfile writes level-N.yaml
// per dependency depth. A baseline taken against the live catalog would churn
// whenever a recipe changed, and a baseline that churns is one people
// regenerate without reading -- which is the failure mode this gate exists to
// prevent.
//
// testdata/layout/recipe.yaml is a frozen two-component recipe, trimmed to the
// fields that can influence the emitted tree. It changes only when someone
// changes it.

const (
	layoutFixture   = "testdata/layout/recipe.yaml"
	layoutManifests = "testdata/layout/manifests"
)

// layoutDeployers is every deployer the bundle CLI accepts. It is checked
// against the OpenAPI enum, so a new deployer cannot ship unfrozen.
var layoutDeployers = []string{"helm", "argocd", "argocd-helm", "flux", "helmfile"}

// TestBundleLayoutMatchesManifest asserts each deployer emits the frozen tree.
//
// Removals and renames fail: they break an integrator reading that path.
// Additions do not, because a new file breaks nobody -- the manifest may lag
// until someone runs `make bundle-layout-baseline`, the same way the OpenAPI
// baseline may lag on additive change.
func TestBundleLayoutMatchesManifest(t *testing.T) {
	binary := buildAICR(t)

	for _, deployer := range layoutDeployers {
		t.Run(deployer, func(t *testing.T) {
			got := generateLayout(t, binary, deployer)
			want := readManifest(t, deployer)

			if len(want) == 0 {
				t.Fatalf("manifest for %s is empty; the gate would accept any tree",
					deployer)
			}
			if len(got) == 0 {
				t.Fatalf("bundling produced no files for %s", deployer)
			}

			wantSet := make(map[string]bool, len(want))
			for _, path := range want {
				wantSet[path] = true
			}
			gotSet := make(map[string]bool, len(got))
			for _, path := range got {
				gotSet[path] = true
			}

			var removed []string
			for _, path := range want {
				if !gotSet[path] {
					removed = append(removed, path)
				}
			}
			sort.Strings(removed)

			for _, path := range removed {
				t.Errorf("%s no longer emits %q, which the frozen layout promises; "+
					"integrator automation reading that path breaks. Make the change "+
					"additive, or accept it with `make bundle-layout-baseline` and "+
					"say why in the PR.", deployer, path)
			}

			// Additive drift is reported without failing: it is allowed, but a
			// silently stale manifest stops describing the bundle.
			var added []string
			for _, path := range got {
				if !wantSet[path] {
					added = append(added, path)
				}
			}
			sort.Strings(added)
			if len(added) > 0 {
				t.Logf("%s emits %d new path(s) not in the manifest (additive, "+
					"allowed): %s. Refresh with `make bundle-layout-baseline`.",
					deployer, len(added), strings.Join(added, ", "))
			}
		})
	}
}

// TestBundleLayoutCoversEveryDeployer asserts the frozen set matches the
// deployers the API actually accepts.
//
// Without this, adding a deployer would ship an entirely ungated layout while
// every existing check stayed green -- the gate would look complete and cover
// less than it claims.
func TestBundleLayoutCoversEveryDeployer(t *testing.T) {
	spec := filepath.Join("..", "..", "api", "aicr", "v1", "server.yaml")
	data, err := os.ReadFile(filepath.Clean(spec))
	if err != nil {
		t.Fatalf("read %s: %v", spec, err)
	}

	var parsed struct {
		Components struct {
			Parameters struct {
				BundleDeployer struct {
					Schema struct {
						Enum []string `yaml:"enum"`
					} `yaml:"schema"`
				} `yaml:"BundleDeployer"`
			} `yaml:"parameters"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse %s: %v", spec, err)
	}

	declared := parsed.Components.Parameters.BundleDeployer.Schema.Enum
	if len(declared) == 0 {
		t.Fatal("no deployer enum found in the spec; this assertion would pass " +
			"vacuously")
	}

	frozen := make(map[string]bool, len(layoutDeployers))
	for _, name := range layoutDeployers {
		frozen[name] = true
	}
	for _, name := range declared {
		if !frozen[name] {
			t.Errorf("the API accepts deployer %q but its bundle layout is not "+
				"frozen; add it to layoutDeployers and commit a manifest", name)
		}
	}

	for _, name := range layoutDeployers {
		var accepted bool
		for _, candidate := range declared {
			if candidate == name {
				accepted = true
				break
			}
		}
		if !accepted {
			t.Errorf("layout is frozen for deployer %q, which the API no longer "+
				"accepts; remove it and its manifest", name)
		}
	}

	// Every frozen deployer needs a manifest on disk, or its subtest above
	// fails for a confusing reason.
	for _, name := range layoutDeployers {
		path := filepath.Join(layoutManifests, name+".txt")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("no manifest at %s; run `make bundle-layout-baseline`", path)
		}
	}
}

// buildAICR compiles the CLI once so each deployer subtest does not pay for it.
//
// The bundle path is exercised through the real binary rather than the library:
// the layout an integrator sees is what the CLI writes, including the root
// files the command adds around the deployer's own output.
func buildAICR(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "aicr")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/aicr")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aicr: %v\n%s", err, out)
	}
	return binary
}

// generateLayout bundles the fixture and returns the sorted relative paths.
func generateLayout(t *testing.T, binary, deployer string) []string {
	t.Helper()

	outDir := filepath.Join(t.TempDir(), deployer)
	cmd := exec.Command(binary, "bundle",
		"-r", layoutFixture, "--deployer", deployer, "-o", outDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bundle --deployer %s: %v\n%s", deployer, err, out)
	}

	var paths []string
	walkErr := filepath.Walk(outDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(outDir, path)
		if relErr != nil {
			return relErr
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", outDir, walkErr)
	}

	sort.Strings(paths)
	return paths
}

// readManifest loads a committed layout manifest.
func readManifest(t *testing.T, deployer string) []string {
	t.Helper()

	path := filepath.Join(layoutManifests, deployer+".txt")
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read manifest %s: %v (run `make bundle-layout-baseline`)", path, err)
	}

	var paths []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			paths = append(paths, line)
		}
	}
	sort.Strings(paths)
	return paths
}
