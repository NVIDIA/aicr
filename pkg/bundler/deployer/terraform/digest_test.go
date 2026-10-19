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

package terraform

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
)

// writeFolder lays out one bundled chart folder under root and returns the
// Folder record localformat would hand back for it.
func writeFolder(t *testing.T, root, dir string, files map[string]string) localformat.Folder {
	t.Helper()
	f := localformat.Folder{Dir: dir, Kind: localformat.KindLocalHelm, Name: dir, Parent: dir}
	for rel, body := range files {
		abs := filepath.Join(root, dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		f.Files = append(f.Files, filepath.ToSlash(filepath.Join(dir, rel)))
	}
	return f
}

// TestChartDigests pins the digest's contract: it covers a bundled chart's
// files and nothing else, so the two values files (already tracked through
// values_files) and the folder's number (already part of the chart path) do
// not move it, while any byte of the chart does. An upstream chart has none.
func TestChartDigests(t *testing.T) {
	chart := map[string]string{
		"Chart.yaml":          "apiVersion: v2\nname: x\nversion: 1.0.0\n",
		"templates/gate.yaml": "kind: Job\n",
		"values.yaml":         "a: 1\n",
		"cluster-values.yaml": "",
	}
	root := t.TempDir()
	base := writeFolder(t, root, "002-x", chart)

	edited := make(map[string]string, len(chart))
	for k, v := range chart {
		edited[k] = v
	}
	edited["templates/gate.yaml"] = "kind: Job\nspec: {}\n"
	changed := writeFolder(t, root, "003-x", edited)

	valuesOnly := make(map[string]string, len(chart))
	for k, v := range chart {
		valuesOnly[k] = v
	}
	valuesOnly["values.yaml"] = "a: 2\n"
	valuesOnly["cluster-values.yaml"] = "b: 1\n"
	renumbered := writeFolder(t, root, "007-x", valuesOnly)

	upstream := localformat.Folder{Dir: "001-nfd", Kind: localformat.KindUpstreamHelm, Upstream: &localformat.Upstream{Chart: "nfd"}}

	got, err := chartDigests(root, []localformat.Folder{upstream, base, changed, renumbered})
	if err != nil {
		t.Fatalf("chartDigests: %v", err)
	}
	if _, has := got[upstream.Dir]; has {
		t.Errorf("an upstream chart got a digest: %q", got[upstream.Dir])
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(got[base.Dir]) {
		t.Errorf("digest has the wrong shape: %q", got[base.Dir])
	}
	if got[base.Dir] != got[renumbered.Dir] {
		t.Errorf("values files or the folder number moved the digest:\n%s\n%s", got[base.Dir], got[renumbered.Dir])
	}
	if got[base.Dir] == got[changed.Dir] {
		t.Errorf("a changed template left the digest unchanged: %s", got[base.Dir])
	}
}
