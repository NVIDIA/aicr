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

package localformat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/manifest"
)

// TestWriteLocalHelmFolder_AllManifestsEmpty pins the fix for the single-package
// tuning manifests (tuning-gke.yaml / tuning-generic.yaml) rendered with
// tuningEnabled=false: the whole Skyhook CR is gated off, so every manifest
// renders to nothing. The component must still produce a valid template-less
// Helm chart (Chart.yaml + values) with NO empty templates/ directory — an
// empty templates/ dir is rejected by the inventory verifier as an unexpected
// directory, which previously aborted bundling on a supported GKE-COS recipe.
func TestWriteLocalHelmFolder_AllManifestsEmpty(t *testing.T) {
	outDir := t.TempDir()
	dir := "004-nodewright-customizations"

	// A gated-off manifest renders to comment/whitespace only — no YAML
	// objects — so writeLocalHelmFolder skips it. With all manifests skipped,
	// no templates/ dir must remain.
	manifests := map[string][]byte{
		"tuning-gke.yaml": []byte("{{- if false }}\napiVersion: v1\nkind: ConfigMap\n{{- end }}\n"),
	}

	c := Component{Name: "nodewright-customizations", Namespace: "skyhook"}
	renderInput := manifest.RenderInput{
		ComponentName: "nodewright-customizations",
		Namespace:     "skyhook",
		ChartName:     "nodewright-customizations",
		ChartVersion:  "0.1.0",
		Values:        map[string]any{},
	}

	folder, err := writeLocalHelmFolder(outDir, dir, 4, c, manifests, renderInput,
		"nodewright-customizations", "nodewright-customizations", false, stampFor(c, "v1.2.3"))
	if err != nil {
		t.Fatalf("writeLocalHelmFolder returned error for all-empty render: %v", err)
	}

	templatesDir := filepath.Join(outDir, dir, "templates")
	if _, statErr := os.Stat(templatesDir); statErr == nil {
		t.Errorf("templates/ directory must not exist when all manifests render empty: %s", templatesDir)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected error stat-ing templates dir: %v", statErr)
	}

	// Chart.yaml and values files must still be present — a valid template-less chart.
	for _, f := range []string{"Chart.yaml", "values.yaml", "cluster-values.yaml", "install.sh"} {
		if _, statErr := os.Stat(filepath.Join(outDir, dir, f)); statErr != nil {
			t.Errorf("expected %s to exist in template-less chart: %v", f, statErr)
		}
	}

	// The returned Folder must not reference any templates/ file.
	for _, f := range folder.Files {
		if filepath.Base(filepath.Dir(f)) == "templates" {
			t.Errorf("Folder.Files must not include a templates entry when render is empty: %q", f)
		}
	}
}

// TestWriteLocalHelmFolder_AllMixedManifestsEmpty pins the lazy-templates/
// contract for the mixed-component post wrapper, which both the vendored and
// non-vendored paths now share (#1835): a mixed component whose recipe-side
// manifests all render empty must produce NO templates/ directory and a
// Folder that references no templates file. Without the lazy creation, the
// eager MkdirAll would leave an empty templates/ dir that the inventory
// verifier rejects.
func TestWriteLocalHelmFolder_AllMixedManifestsEmpty(t *testing.T) {
	outDir := t.TempDir()
	dir := "003-network-operator-post"

	// A gated-off manifest renders to nothing (no YAML objects).
	manifests := map[string][]byte{
		"gated.yaml": []byte("{{- if false }}\napiVersion: v1\nkind: ConfigMap\n{{- end }}\n"),
	}
	c := Component{Name: "network-operator", Namespace: "nvidia-network-operator"}

	folder, err := writeLocalHelmFolder(outDir, dir, 3, c, manifests, renderInputFor(c),
		"network-operator-post", "network-operator", true, stampFor(c, "v1.2.3"))
	if err != nil {
		t.Fatalf("writeLocalHelmFolder returned error for all-empty render: %v", err)
	}
	for _, f := range folder.Files {
		if filepath.Base(filepath.Dir(f)) == "templates" {
			t.Errorf("Folder.Files must not include a templates entry when render is empty: %q", f)
		}
	}
	templatesDir := filepath.Join(outDir, dir, "templates")
	if _, statErr := os.Stat(templatesDir); statErr == nil {
		t.Errorf("templates/ directory must not exist when all mixed manifests render empty: %s", templatesDir)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected error stat-ing templates dir: %v", statErr)
	}
}
