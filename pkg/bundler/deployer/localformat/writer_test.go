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
	"context"
	stderrors "errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
)

var update = flag.Bool("update", false, "update golden files")

func TestWrite_UpstreamHelmOnly(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:       "nfd",
			Namespace:  "node-feature-discovery",
			Repository: "https://kubernetes-sigs.github.io/node-feature-discovery/charts",
			ChartName:  "node-feature-discovery",
			Version:    "v0.16.1",
			Values:     map[string]any{"image": map[string]any{"tag": "v0.16.1"}},
		}},
	})
	folders := res.Folders
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if len(folders) != 1 {
		t.Fatalf("want 1 folder, got %d", len(folders))
	}
	if got, want := folders[0].Dir, "001-nfd"; got != want {
		t.Errorf("folders[0].Dir = %q, want %q", got, want)
	}
	if got, want := folders[0].Kind, localformat.KindUpstreamHelm; got != want {
		t.Errorf("folders[0].Kind = %v, want %v", got, want)
	}

	// Files written on disk
	for _, rel := range []string{"install.sh", "values.yaml", "cluster-values.yaml", "upstream.env"} {
		if _, err := os.Stat(filepath.Join(outDir, "001-nfd", rel)); err != nil {
			t.Errorf("missing file %s: %v", rel, err)
		}
	}
	// No Chart.yaml for upstream-helm
	if _, err := os.Stat(filepath.Join(outDir, "001-nfd", "Chart.yaml")); !os.IsNotExist(err) {
		t.Errorf("Chart.yaml must not exist for upstream-helm folder")
	}

	// Golden-file compare for install.sh + upstream.env
	assertGolden(t, outDir, "testdata/upstream_helm_only", "001-nfd/install.sh")
	assertGolden(t, outDir, "testdata/upstream_helm_only", "001-nfd/upstream.env")
}

func TestWrite_LocalHelmManifestOnly(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:       "skyhook-customizations",
			Namespace:  "skyhook",
			Repository: "", // empty: manifest-only
		}},
		ComponentPostManifests: map[string]map[string][]byte{
			"skyhook-customizations": {
				// Realistic input: project recipe manifests carry a license header
				// (see recipes/components/gpu-operator/manifests/dcgm-exporter.yaml).
				"components/skyhook-customizations/manifests/customization.yaml": []byte(`# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

apiVersion: v1
kind: ConfigMap
metadata:
  name: x
`),
			},
		},
	})
	folders := res.Folders
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(folders) != 1 || folders[0].Kind != localformat.KindLocalHelm {
		t.Fatalf("want 1 local-helm folder, got %d folders kind=%v", len(folders), folders[0].Kind)
	}

	for _, rel := range []string{"install.sh", "values.yaml", "cluster-values.yaml", "Chart.yaml", "templates/customization.yaml"} {
		if _, err := os.Stat(filepath.Join(outDir, "001-skyhook-customizations", rel)); err != nil {
			t.Errorf("missing file %s: %v", rel, err)
		}
	}
	// upstream.env MUST NOT exist for local-helm
	if _, err := os.Stat(filepath.Join(outDir, "001-skyhook-customizations", "upstream.env")); !os.IsNotExist(err) {
		t.Errorf("upstream.env must not exist for local-helm folder")
	}

	assertGolden(t, outDir, "testdata/local_helm_manifest_only", "001-skyhook-customizations/install.sh")
	assertGolden(t, outDir, "testdata/local_helm_manifest_only", "001-skyhook-customizations/Chart.yaml")
	assertGolden(t, outDir, "testdata/local_helm_manifest_only", "001-skyhook-customizations/templates/customization.yaml")
}

func TestWrite_Mixed(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:       "gpu-operator",
			Namespace:  "gpu-operator",
			Repository: "https://nvidia.github.io/gpu-operator",
			ChartName:  "nvidia/gpu-operator",
			Version:    "v24.9.1",
		}},
		ComponentPostManifests: map[string]map[string][]byte{
			"gpu-operator": {
				// Realistic: real project manifests carry a license header.
				"components/gpu-operator/manifests/dcgm-exporter.yaml": []byte(`# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

apiVersion: v1
kind: Service
metadata:
  name: dcgm
`),
			},
		},
	})
	folders := res.Folders
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if len(folders) != 2 {
		t.Fatalf("want 2 folders, got %d", len(folders))
	}
	if folders[0].Dir != "001-gpu-operator" || folders[0].Kind != localformat.KindUpstreamHelm {
		t.Errorf("folders[0] = %+v, want 001-gpu-operator / upstream-helm", folders[0])
	}
	if folders[1].Dir != "002-gpu-operator-post" || folders[1].Kind != localformat.KindLocalHelm {
		t.Errorf("folders[1] = %+v, want 002-gpu-operator-post / local-helm", folders[1])
	}
	if folders[1].Parent != "gpu-operator" {
		t.Errorf("folders[1].Parent = %q, want gpu-operator", folders[1].Parent)
	}
	if folders[1].Name != "gpu-operator-post" {
		t.Errorf("folders[1].Name = %q, want gpu-operator-post", folders[1].Name)
	}

	// Primary has NO Chart.yaml (upstream-helm)
	if _, err := os.Stat(filepath.Join(outDir, "001-gpu-operator", "Chart.yaml")); !os.IsNotExist(err) {
		t.Errorf("primary must not have Chart.yaml")
	}
	// Post HAS Chart.yaml + templates/dcgm-exporter.yaml
	if _, err := os.Stat(filepath.Join(outDir, "002-gpu-operator-post", "Chart.yaml")); err != nil {
		t.Errorf("post must have Chart.yaml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "002-gpu-operator-post", "templates", "dcgm-exporter.yaml")); err != nil {
		t.Errorf("post must have templates/dcgm-exporter.yaml: %v", err)
	}

	// Post's upstream.env MUST NOT exist (wrapped chart, not upstream ref)
	if _, err := os.Stat(filepath.Join(outDir, "002-gpu-operator-post", "upstream.env")); !os.IsNotExist(err) {
		t.Errorf("post must not have upstream.env")
	}
}

// TestWrite_PreFolderInstallOmitsCreateNamespace asserts that a
// pre-injection folder's install.sh does NOT pass --create-namespace
// to helm, while the primary and post folders still do. Helm 3 refuses
// to import a pre-existing namespace lacking
// app.kubernetes.io/managed-by=Helm + meta.helm.sh/release-name
// annotations, so pre folders (which carry the Namespace manifest by
// design) must let the chart's own Namespace template create it. See
// pkg/bundler/deployer/localformat/local_helm.go writeLocalHelmFolder
// docstring for the full rationale.
func TestWrite_PreFolderInstallOmitsCreateNamespace(t *testing.T) {
	outDir := t.TempDir()
	const licenseHeader = `# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

`

	if _, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:       "foo",
			Namespace:  "privileged-foo",
			Repository: "https://example.invalid/charts",
			ChartName:  "example/foo",
			Version:    "v1.0.0",
		}},
		ComponentPreManifests: map[string]map[string][]byte{
			"foo": {
				"foo/manifests/talos-namespace.yaml": []byte(licenseHeader +
					"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: privileged-foo\n"),
			},
		},
		ComponentPostManifests: map[string]map[string][]byte{
			"foo": {
				"foo/manifests/cm.yaml": []byte(licenseHeader +
					"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n"),
			},
		},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	tests := []struct {
		name                 string
		installPath          string
		wantCreateNamespace  bool
		wantNamespaceLiteral string
	}{
		{
			name:                 "pre folder omits --create-namespace",
			installPath:          "001-foo-pre/install.sh",
			wantCreateNamespace:  false,
			wantNamespaceLiteral: "--namespace privileged-foo \\",
		},
		{
			name:                 "primary folder keeps --create-namespace",
			installPath:          "002-foo/install.sh",
			wantCreateNamespace:  true,
			wantNamespaceLiteral: "--namespace privileged-foo --create-namespace \\",
		},
		{
			name:                 "post folder keeps --create-namespace",
			installPath:          "003-foo-post/install.sh",
			wantCreateNamespace:  true,
			wantNamespaceLiteral: "--namespace privileged-foo --create-namespace \\",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(outDir, tc.installPath))
			if err != nil {
				t.Fatalf("read install.sh: %v", err)
			}
			s := string(b)
			if got := strings.Contains(s, "--create-namespace"); got != tc.wantCreateNamespace {
				t.Errorf("--create-namespace present = %v, want %v\n%s", got, tc.wantCreateNamespace, s)
			}
			if !strings.Contains(s, tc.wantNamespaceLiteral) {
				t.Errorf("install.sh missing literal %q; got:\n%s", tc.wantNamespaceLiteral, s)
			}
		})
	}
}

// TestWrite_FolderLimit_CountsEmissionsNotComponents pins the
// folder-prefix-exhaustion guard. The check budgets against the actual
// number of NNN-* directories the bundle will emit (pre + primary +
// post, per the same conditions the main loop applies) instead of a
// worst-case 3*len(Components) multiplier. Without this, a recipe of
// 400 pure upstream-Helm components — which produce 400 directories —
// would be rejected before any work happens. The deploy/undeploy
// templates only glob three-digit prefixes, so the cap remains 999.
func TestWrite_FolderLimit_CountsEmissionsNotComponents(t *testing.T) {
	// Helper: build n primary-only upstream-Helm components.
	makeComponents := func(n int) []localformat.Component {
		out := make([]localformat.Component, n)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("c%04d", i)
			out[i] = localformat.Component{
				Name:       name,
				Namespace:  name,
				Repository: "https://example.invalid/charts",
				ChartName:  name,
				Version:    "v1.0.0",
			}
		}
		return out
	}

	t.Run("400 primary-only components stay under the cap", func(t *testing.T) {
		// Under the old 3x multiplier, 400 components would be rejected
		// (3*400 = 1200 > 999). With per-emission counting, 400 primary
		// folders is well under the limit.
		_, err := localformat.Write(context.Background(), localformat.Options{
			OutputDir:  t.TempDir(),
			Components: makeComponents(400),
		})
		if err != nil {
			t.Fatalf("400 primary-only components should fit under cap, got: %v", err)
		}
	})

	t.Run("1000 primary-only components exceed the cap", func(t *testing.T) {
		_, err := localformat.Write(context.Background(), localformat.Options{
			OutputDir:  t.TempDir(),
			Components: makeComponents(1000),
		})
		if err == nil {
			t.Fatal("1000 primary folders must exceed the 999-prefix cap")
		}
		if !strings.Contains(err.Error(), "too many emitted folders") {
			t.Errorf("error should mention emitted folder count; got: %v", err)
		}
	})
}

// TestWrite_PreManifestNamespaceDrift asserts that the writer rejects
// a pre-manifest whose Namespace metadata.name disagrees with the
// component's release namespace. Without this guard, the bundle would
// pass type checks but blow up at `helm install` time with an opaque
// "namespace not found" error because the chart creates one namespace
// while the release targets another.
func TestWrite_PreManifestNamespaceDrift(t *testing.T) {
	_, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: t.TempDir(),
		Components: []localformat.Component{{
			Name:       "foo",
			Namespace:  "privileged-foo",
			Repository: "https://example.invalid/charts",
			ChartName:  "example/foo",
			Version:    "v1.0.0",
		}},
		ComponentPreManifests: map[string]map[string][]byte{
			"foo": {
				"foo/manifests/typo-namespace.yaml": []byte(
					"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: privileged-foobar\n",
				),
			},
		},
	})
	if err == nil {
		t.Fatal("Write must reject pre-manifest Namespace name that drifts from ComponentRef.Namespace")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("error is %T, want *errors.StructuredError: %v", err, err)
	}
	if se.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", se.Code)
	}
	for _, want := range []string{"privileged-foo", "privileged-foobar", "typo-namespace.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q; got: %v", want, err)
		}
	}
}

func TestWrite_Ordering(t *testing.T) {
	outDir := t.TempDir()
	mk := func(name, repo string) localformat.Component {
		return localformat.Component{
			Name:       name,
			Namespace:  name,
			Repository: repo,
			ChartName:  name,
			Version:    "v1.0.0",
		}
	}

	// b is mixed: helm repo set + manifests → emits b primary + b-post injected
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{
			mk("a", "https://a.example"),
			mk("b", "https://b.example"),
			mk("c", "https://c.example"),
		},
		ComponentPostManifests: map[string]map[string][]byte{
			"b": {
				"b/manifests/x.yaml": []byte(`# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

apiVersion: v1
kind: ConfigMap
metadata:
  name: x
`),
			},
		},
	})
	folders := res.Folders
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := make([]string, 0, len(folders))
	for _, f := range folders {
		got = append(got, f.Dir)
	}
	want := []string{"001-a", "002-b", "003-b-post", "004-c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("folder order = %v, want %v", got, want)
	}

	// Verify the primary/post relationship on b
	if folders[1].Kind != localformat.KindUpstreamHelm {
		t.Errorf("folders[1] (b) = %v, want KindUpstreamHelm", folders[1].Kind)
	}
	if folders[2].Kind != localformat.KindLocalHelm || folders[2].Parent != "b" || folders[2].Name != "b-post" {
		t.Errorf("folders[2] (b-post) = %+v, want KindLocalHelm parent=b name=b-post", folders[2])
	}

	// Verify subsequent indices are correct on the Folder struct itself (not just the Dir)
	wantIndices := []int{1, 2, 3, 4}
	for i, f := range folders {
		if f.Index != wantIndices[i] {
			t.Errorf("folders[%d].Index = %d, want %d (dir=%s)", i, f.Index, wantIndices[i], f.Dir)
		}
	}
}

func TestWrite_Kustomize(t *testing.T) {
	outDir := t.TempDir()

	// Absolute path to the kustomize fixture. `filepath.Abs` resolves the
	// test-relative "testdata/kustomize_input" to something buildKustomize
	// can feed to kustomize's on-disk filesystem.
	kustomizePath, err := filepath.Abs(filepath.Join("testdata", "kustomize_input"))
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:      "my-kustomize",
			Namespace: "mk",
			// Local kustomize: Path only. Tag/Repository are only meaningful
			// for git-sourced kustomizations and are validated as a pair by
			// Write — a Tag without Repository would (correctly) be rejected.
			Path: kustomizePath,
		}},
	})
	folders := res.Folders
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(folders) != 1 || folders[0].Kind != localformat.KindLocalHelm {
		t.Fatalf("want 1 local-helm folder (kustomize wrapped), got %d folders kind=%v", len(folders), folders[0].Kind)
	}

	// manifest.yaml is the single flattened output of kustomize build
	manifestPath := filepath.Join(outDir, "001-my-kustomize", "templates", "manifest.yaml")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("missing templates/manifest.yaml: %v", err)
	}
	// Chart.yaml should still exist (wrapped chart)
	if _, err := os.Stat(filepath.Join(outDir, "001-my-kustomize", "Chart.yaml")); err != nil {
		t.Errorf("missing Chart.yaml: %v", err)
	}
}

func TestWrite_Deterministic(t *testing.T) {
	kustomizePath, err := filepath.Abs(filepath.Join("testdata", "kustomize_input"))
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	opts := func(dir string) localformat.Options {
		return localformat.Options{
			OutputDir: dir,
			Components: []localformat.Component{
				{
					Name:       "a",
					Namespace:  "a",
					Repository: "https://a.example",
					ChartName:  "a",
					Version:    "v1",
					Values:     map[string]any{"image": map[string]any{"tag": "v1"}},
				},
				{
					Name:       "b",
					Namespace:  "b",
					Repository: "https://b.example",
					ChartName:  "b",
					Version:    "v1",
				},
				{
					// Kustomize component to lock determinism on the
					// kustomize build path (manifest.yaml ordering, etc.).
					Name:      "k",
					Namespace: "k",
					Path:      kustomizePath,
				},
			},
			// b is mixed — exercise the -post injection path in the determinism check
			ComponentPostManifests: map[string]map[string][]byte{
				"b": {
					// Two manifests with distinct basenames to exercise sorted iteration
					"b/manifests/m1.yaml": []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: m1\n"),
					"b/manifests/m2.yaml": []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: m2\n"),
				},
			},
		}
	}
	d1, d2 := t.TempDir(), t.TempDir()
	if _, err := localformat.Write(context.Background(), opts(d1)); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if _, err := localformat.Write(context.Background(), opts(d2)); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	assertDirsEqual(t, d1, d2)
}

func TestWrite_KustomizeWithManifestsRejected(t *testing.T) {
	// Point Path at the existing kustomize fixture so Tag/Path are set
	// realistically, but attach raw manifests alongside — bundle must refuse.
	kustomizePath, err := filepath.Abs(filepath.Join("testdata", "kustomize_input"))
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	_, err = localformat.Write(context.Background(), localformat.Options{
		OutputDir: t.TempDir(),
		Components: []localformat.Component{{
			Name:      "busted-component",
			Namespace: "ns",
			Tag:       "v1.0.0",
			Path:      kustomizePath,
		}},
		ComponentPostManifests: map[string]map[string][]byte{
			"busted-component": {
				"extra/m.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"),
			},
		},
	})
	if err == nil {
		t.Fatalf("want error rejecting kustomize + raw manifests, got nil")
	}
	// Must be a structured error with ErrCodeInvalidRequest
	var structErr *errors.StructuredError
	if !stderrors.As(err, &structErr) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if structErr.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("error code = %s, want %s (full error: %v)", structErr.Code, errors.ErrCodeInvalidRequest, err)
	}
	// Message should name the component and reference the conflict
	msg := err.Error()
	if !strings.Contains(msg, "busted-component") || !strings.Contains(msg, "kustomize") || !strings.Contains(msg, "manifests") {
		t.Errorf("error message should mention component name + conflict; got: %s", msg)
	}
}

func TestWrite_PathContainment(t *testing.T) {
	_, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: t.TempDir(),
		Components: []localformat.Component{{
			Name:       "../escape",
			Repository: "https://example.com",
		}},
	})
	if err == nil {
		t.Fatalf("want error rejecting unsafe component name, got nil")
	}
	var structErr *errors.StructuredError
	if !stderrors.As(err, &structErr) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if structErr.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("code = %v, want ErrCodeInvalidRequest", structErr.Code)
	}
	if !strings.Contains(err.Error(), "../escape") {
		t.Errorf("error should name the offending component; got: %v", err)
	}
}

func TestWrite_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Write

	_, err := localformat.Write(ctx, localformat.Options{
		OutputDir: t.TempDir(),
		Components: []localformat.Component{{
			Name:       "a",
			Repository: "https://a.example",
			ChartName:  "a",
			Version:    "v1",
		}},
	})
	if err == nil {
		t.Fatalf("want error on cancelled context, got nil")
	}
	var structErr *errors.StructuredError
	if !stderrors.As(err, &structErr) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if structErr.Code != errors.ErrCodeTimeout {
		t.Errorf("code = %v, want ErrCodeTimeout", structErr.Code)
	}
}

// assertDirsEqual walks d1 and compares each file to the corresponding file
// in d2 (same relative path). Fails on missing files, extra files, or content
// mismatch. Path-relative compare — absolute TempDir prefix is stripped.
func assertDirsEqual(t *testing.T, d1, d2 string) {
	t.Helper()
	files1 := listFiles(t, d1)
	files2 := listFiles(t, d2)
	if !reflect.DeepEqual(files1, files2) {
		t.Fatalf("file trees differ:\n  d1=%v\n  d2=%v", files1, files2)
	}
	for _, rel := range files1 {
		b1, err := os.ReadFile(filepath.Join(d1, rel))
		if err != nil {
			t.Fatalf("read %s from d1: %v", rel, err)
		}
		b2, err := os.ReadFile(filepath.Join(d2, rel))
		if err != nil {
			t.Fatalf("read %s from d2: %v", rel, err)
		}
		if string(b1) != string(b2) {
			t.Errorf("content differs at %s:\n--- d1 ---\n%s\n--- d2 ---\n%s", rel, b1, b2)
		}
	}
}

// listFiles returns sorted relative paths of all regular files under dir.
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode().IsRegular() {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(files)
	return files
}

// assertGolden reads outDir/relPath and diffs it against goldenDir/relPath.
// With -update, writes the actual content to the golden path.
func assertGolden(t *testing.T, outDir, goldenDir, relPath string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(outDir, relPath))
	if err != nil {
		t.Fatalf("read actual %s: %v", relPath, err)
	}
	goldenPath := filepath.Join(goldenDir, relPath)
	if *update {
		if err = os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err = os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", relPath, got, want)
	}
}
