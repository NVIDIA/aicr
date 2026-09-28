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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/bundleinfo"
)

// TestBundleInfoDescribesTheBundle asserts the index and the tree agree, in
// both directions, for every deployer.
//
// Both directions matter and for different reasons. A claimed path that does
// not exist sends automation to a missing file. A directory the index omits
// is the quieter failure: deployer.Output.Releases is additive with a zero
// value, so a deployer that never populates it still compiles and still
// produces a well-formed bundle -- with an index that confidently
// under-describes it. Only the emitted-directories direction catches that.
func TestBundleInfoDescribesTheBundle(t *testing.T) {
	binary := buildAICR(t)

	for _, d := range layoutDeployers {
		t.Run(d, func(t *testing.T) {
			outDir := bundleLayoutFixture(t, binary, d)

			info, err := bundleinfo.Read(context.Background(), outDir)
			if err != nil {
				t.Fatalf("read bundle info: %v", err)
			}
			if info.Build.Deployer != d {
				t.Errorf("deployer = %q, want %q", info.Build.Deployer, d)
			}
			if info.Layout.Entrypoint == "" {
				t.Fatal("no entrypoint reported")
			}
			if _, statErr := os.Stat(filepath.Join(outDir, info.Layout.Entrypoint)); statErr != nil {
				t.Errorf("entrypoint %q does not exist: %v", info.Layout.Entrypoint, statErr)
			}
			if len(info.Layout.Releases) == 0 {
				t.Fatal("no releases indexed; the index under-describes the bundle")
			}

			claimed := make(map[string]bool, len(info.Layout.Releases))
			for _, r := range info.Layout.Releases {
				claimed[r.Path] = true
				if _, statErr := os.Stat(filepath.Join(outDir, r.Path)); statErr != nil {
					t.Errorf("release %q claims path %q, which does not exist: %v", r.Name, r.Path, statErr)
				}
				if r.Manifest != "" {
					if _, statErr := os.Stat(filepath.Join(outDir, r.Manifest)); statErr != nil {
						t.Errorf("release %q claims manifest %q, which does not exist: %v",
							r.Name, r.Manifest, statErr)
					}
				}
				if r.Component == "" {
					t.Errorf("release %q names no component; an injected folder must name its parent", r.Name)
				}
			}

			for _, dir := range releaseDirs(t, outDir, d) {
				if !claimed[dir] {
					t.Errorf("bundle emits release directory %q that bundle-info.yaml does not "+
						"claim; the index silently under-describes the bundle", dir)
				}
			}
		})
	}
}

// TestBundleInfoIsDeterministic bundles the same fixture twice and compares
// bytes. The record feeds checksums.txt, which is the attestation subject, so
// any run-varying field makes every bundle irreproducible.
func TestBundleInfoIsDeterministic(t *testing.T) {
	binary := buildAICR(t)

	for _, d := range layoutDeployers {
		t.Run(d, func(t *testing.T) {
			first := bundleLayoutFixture(t, binary, d)
			second := bundleLayoutFixture(t, binary, d)

			a, err := os.ReadFile(filepath.Join(first, bundleinfo.FileName))
			if err != nil {
				t.Fatalf("read first: %v", err)
			}
			b, err := os.ReadFile(filepath.Join(second, bundleinfo.FileName))
			if err != nil {
				t.Fatalf("read second: %v", err)
			}
			if string(a) != string(b) {
				t.Errorf("%s differs between runs:\n--- first\n%s\n--- second\n%s",
					bundleinfo.FileName, a, b)
			}
		})
	}
}

// bundleLayoutFixture bundles the frozen fixture recipe for one deployer and
// returns the output directory.
func bundleLayoutFixture(t *testing.T, binary, deployer string) string {
	t.Helper()

	outDir := filepath.Join(t.TempDir(), deployer)
	cmd := exec.Command(binary, "bundle",
		"-r", layoutFixture, "--deployer", deployer, "-o", outDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bundle --deployer %s: %v\n%s", deployer, err, out)
	}
	return outDir
}

// nnnDir matches the zero-padded folder prefix the four non-flux deployers use.
var nnnDir = regexp.MustCompile(`^\d{3}-`)

// releaseDirs lists the top-level directories that hold a release, derived
// from the tree rather than from the index -- deriving it from the index would
// make the under-description check assert against itself and pass vacuously.
//
// Flux is the exception to the NNN- convention: it writes an unprefixed
// <component>/ directory plus a shared sources/ that holds no release.
func releaseDirs(t *testing.T, outDir, deployer string) []string {
	t.Helper()

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read %s: %v", outDir, err)
	}

	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if deployer == "flux" {
			if e.Name() != "sources" {
				dirs = append(dirs, e.Name())
			}
			continue
		}
		if nnnDir.MatchString(e.Name()) {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}
