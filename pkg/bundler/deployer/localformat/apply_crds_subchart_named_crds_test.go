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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// crdsSubchartNames are the metadata.name values of the CRDs in the fixture
// chart, which is shaped like NVCRE v0.12.0: the CRDs live in a subchart that
// is itself named "crds", so they sit in charts/crds/crds/.
var crdsSubchartNames = []string{"own.example.com", "a.example.com", "b.example.com"}

// chartWithCRDsSubchart builds an unpacked-subchart chart whose subchart is
// named "crds". Besides the real CRDs it holds the subchart's Chart.yaml and
// values.yaml, which a bare '*/crds/*' match collects and which carry no kind.
func chartWithCRDsSubchart(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := t.TempDir()
	crd := func(name string) string {
		return "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
			"metadata:\n  name: " + name + "\n"
	}
	files := map[string]string{
		"nvcre/Chart.yaml":                       "apiVersion: v2\nname: nvcre\nversion: 1.0.0\n",
		"nvcre/crds/own.yaml":                    crd("own.example.com"),
		"nvcre/charts/crds/Chart.yaml":           "apiVersion: v2\nname: crds\nversion: 1.0.0\n",
		"nvcre/charts/crds/values.yaml":          "replicas: 1\n",
		"nvcre/charts/crds/crds/a.yaml":          crd("a.example.com"),
		"nvcre/charts/crds/crds/b.yaml":          crd("b.example.com"),
		"nvcre/charts/crds/templates/notes.yaml": "note: not-a-crd\n",
	}
	for rel, body := range files {
		writeFile(t, filepath.Join(root, rel), body)
	}
	tgz := filepath.Join(t.TempDir(), "nvcre.tgz")
	if out, err := exec.Command("tar", "-czf", tgz, "-C", root, "nvcre").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	return tgz
}

// kindCheckingKubectlStub behaves like kubectl for the two verbs the CRD step
// uses: get and apply reject any file without a kind, exactly as kubectl does
// ("Object 'Kind' is missing" and "kind not set"), and apply appends each
// applied CRD's metadata.name to log. getOutput is what `get` reports, so a
// caller picks between the fresh-install and retained-CRD paths.
func kindCheckingKubectlStub(log, getOutput string) string {
	return "#!/usr/bin/env bash\n" +
		"verb=\"$1\"; f=\"\"; prev=\"\"\n" +
		"for a in \"$@\"; do [[ \"${prev}\" == -f ]] && f=\"${a}\"; prev=\"${a}\"; done\n" +
		"if [[ \"${verb}\" == get || \"${verb}\" == apply ]] && [[ -n \"${f}\" ]]; then\n" +
		"  while IFS= read -r file; do\n" +
		"    grep -q '[^[:space:]]' \"${file}\" || continue\n" +
		"    grep -q '^kind:' \"${file}\" || { echo \"error: Object 'Kind' is missing in '${file}'\" >&2; exit 1; }\n" +
		"  done < <(find \"${f}\" -type f)\n" +
		"fi\n" +
		"if [[ \"${verb}\" == apply ]]; then grep -h '^  name:' \"${f}\" >> " + log + "; fi\n" +
		"[[ \"${verb}\" == get ]] && echo '" + getOutput + "'\n" +
		"exit 0\n"
}

func assertCRDsApplied(t *testing.T, log string, out []byte) {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("no CRD was applied (%v)\n%s", err, out)
	}
	for _, name := range crdsSubchartNames {
		if !strings.Contains(string(b), name) {
			t.Errorf("CRD %s was not applied; applied:\n%s", name, b)
		}
	}
}

// TestApplyCRDsScript_SkipsFilesOfSubchartNamedCRDs pins the NVCRE v0.12.0
// regression. A subchart named "crds" puts charts/crds/Chart.yaml under a path
// that a bare '*/crds/*' match treats as a CRD. kubectl rejects it, and
// because it sorts ahead of the real CRDs a fresh install exited 1 at
// `kubectl get -f` and an upgrade exited 1 at the first apply.
func TestApplyCRDsScript_SkipsFilesOfSubchartNamedCRDs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))

	tests := []struct {
		name     string
		listBody string
		getOut   string
		wantCRDs bool
	}{
		{"fresh install, no retained CRDs", ":", "", false},
		{"fresh install over retained CRDs", ":", "customresourcedefinition/own.example.com", true},
		{"upgrade of an existing release", "echo k8s-aibom", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "applied")
			path := stubPATH(t, map[string]string{
				"helm":    helmStub(tt.listBody, chartWithCRDsSubchart(t)),
				"kubectl": kindCheckingKubectlStub(log, tt.getOut),
				"timeout": passthroughTimeoutStub,
			})
			cmd := exec.Command("bash", scriptPath)
			cmd.Env = append(os.Environ(), "PATH="+path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("script failed: %v\n%s", err, out)
			}
			if tt.wantCRDs {
				assertCRDsApplied(t, log, out)
			}
		})
	}
}

// TestCatalogCRDCommand_SkipsFilesOfSubchartNamedCRDs runs the catalog's
// manual CRD-apply block, as published, against the same chart. The block
// collects CRDs with its own find, separate from apply-crds.sh, and carried
// the same defect: helmfile, terraform and fleet deployers stopped at
// Chart.yaml before any CRD was updated.
func TestCatalogCRDCommand_SkipsFilesOfSubchartNamedCRDs(t *testing.T) {
	for _, tool := range []string{"bash", "tar", "find", "sort", "grep", "mktemp"} {
		requireTool(t, tool)
	}
	block := pinCatalogBlock(t, catalogCRDBlock(t), map[string]string{
		"CHART":   "oci://example.invalid/nvcre",
		"VERSION": "1.0.0",
	})
	log := filepath.Join(t.TempDir(), "applied")
	path := stubPATH(t, map[string]string{
		"helm":    helmStub(":", chartWithCRDsSubchart(t)),
		"kubectl": kindCheckingKubectlStub(log, ""),
	})
	cmd := exec.Command("bash", "-c", block)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("catalog block failed: %v\n%s", err, out)
	}
	assertCRDsApplied(t, log, out)
}
