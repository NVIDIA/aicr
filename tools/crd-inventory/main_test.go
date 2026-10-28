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

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	ociregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func crdDoc(name string) string {
	return "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: " + name + "\n"
}

func healthCheck(ops ...string) string {
	return "apiVersion: chainsaw.kyverno.io/v1alpha1\nkind: Test\nmetadata:\n  name: hc\nspec:\n  steps:\n" +
		"    - name: s\n      try:\n" + strings.Join(ops, "")
}

func assertOp(kind, name string) string {
	return "        - assert:\n            resource:\n              kind: " + kind +
		"\n              metadata:\n                name: " + name + "\n"
}

func errorOp(kind, name string) string {
	return strings.Replace(assertOp(kind, name), "assert:", "error:", 1)
}

func TestParseAssertedCRDs(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
		wantErr bool
	}{
		{"sorted and unique", healthCheck(assertOp(crdKind, "b.x.io"), assertOp(crdKind, "a.x.io"),
			assertOp(crdKind, "b.x.io")), []string{"a.x.io", "b.x.io"}, false},
		{"error op asserts absence", healthCheck(errorOp(crdKind, "a.x.io")), nil, false},
		{"other kinds ignored", healthCheck(assertOp("Deployment", "op")), nil, false},
		{"every document", healthCheck(assertOp(crdKind, "a.x.io")) + "---\n" +
			healthCheck(assertOp(crdKind, "b.x.io")), []string{"a.x.io", "b.x.io"}, false},
		{"no literal name", healthCheck("        - assert:\n            resource:\n              kind: " +
			crdKind + "\n"), nil, true},
		{"not yaml", "spec: [", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAssertedCRDs([]byte(tt.content))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseAssertedCRDs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("parseAssertedCRDs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCRDNames(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
		wantErr bool
	}{
		{"crds among other kinds", "---\n" + crdDoc("b.x.io") + "---\nkind: Deployment\n---\n" + crdDoc("a.x.io"),
			[]string{"a.x.io", "b.x.io"}, false},
		{"empty documents", "---\n---\n", nil, false},
		{"crd without name", "kind: CustomResourceDefinition\nmetadata: {}\n", nil, true},
		{"not yaml", "kind: [", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCRDNames([]byte(tt.content))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseCRDNames() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("parseCRDNames() = %v, want %v", got, tt.want)
			}
		})
	}
}

func newMapProvider(t *testing.T, files map[string]string) *providertest.Provider {
	t.Helper()
	data := make(map[string][]byte, len(files))
	for file, body := range files {
		data[file] = []byte(body)
	}
	dp := providertest.NewMap(data)
	t.Cleanup(func() { recipe.EvictCachedRegistry(dp) })
	return dp
}

const registryHeader = "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\n"

func TestManifestCRDs(t *testing.T) {
	dp := newMapProvider(t, map[string]string{
		"components/c/manifests/crds.yaml":          crdDoc("a.x.io") + "---\nkind: [{{ .Values.broken }}\n---\n" + crdDoc("b.x.io"),
		"components/c/manifests/README.md":          crdDoc("c.x.io"),
		"components/c/manifests/nested/extra.yaml":  crdDoc("d.x.io"),
		"components/other/manifests/elsewhere.yaml": crdDoc("e.x.io"),
	})

	got, err := manifestCRDs(t.Context(), dp, "c")
	if err != nil {
		t.Fatalf("manifestCRDs() error = %v", err)
	}
	if want := []string{"a.x.io", "b.x.io"}; !slices.Equal(got, want) {
		t.Errorf("manifestCRDs() = %v, want %v", got, want)
	}
	if got, err := manifestCRDs(t.Context(), dp, "missing"); err != nil || len(got) != 0 {
		t.Errorf("manifestCRDs(missing) = %v, %v, want none", got, err)
	}
}

func TestContainerImage(t *testing.T) {
	deployment := func(field, container, image string) string {
		return "kind: Deployment\nspec:\n  template:\n    spec:\n      " + field + ":\n        - name: " +
			container + "\n          image: " + image + "\n"
	}
	initC := func(container, image string) string { return deployment("initContainers", container, image) }
	tests := []struct {
		name     string
		rendered string
		want     string
		wantErr  bool
	}{
		{"one init container image", initC("op", "op:1") + "---\n" + initC("other", "x:1"), "op:1", false},
		{"one container image", deployment("containers", "op", "op:1") + "---\n" + initC("other", "x:1"), "op:1", false},
		{"same image twice", initC("op", "op:1") + "---\n" + deployment("containers", "op", "op:1"), "op:1", false},
		{"no container", initC("other", "x:1"), "", true},
		{"two images", initC("op", "op:1") + "---\n" + deployment("containers", "op", "op:2"), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := containerImage([]byte(tt.rendered), "op")
			if (err != nil) != tt.wantErr {
				t.Fatalf("containerImage() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("containerImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, file := range slices.Sorted(maps.Keys(files)) {
		body := files[file]
		if err := tw.WriteHeader(&tar.Header{Name: file, Mode: 0o600, Size: int64(len(body)),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCRDsInTar(t *testing.T) {
	oversized := strings.Repeat("#", maxCRDFileBytes+1)
	tests := []struct {
		name     string
		archive  []byte
		want     []string
		wantCode errors.ErrorCode
	}{
		{"yaml files directly under dir", tarOf(t, map[string]string{
			"./opt/op/crds/a.yaml":        crdDoc("a.x.io"),
			"opt/op/crds/b.yml":           crdDoc("b.x.io"),
			"opt/op/crds/nested/c.yaml":   crdDoc("c.x.io"),
			"opt/op/crds/README.md":       crdDoc("d.x.io"),
			"opt/other/e.yaml":            crdDoc("e.x.io"),
			"opt/op/crds/deployment.yaml": "kind: Deployment\n",
			"opt/op/crds/template.yaml":   "kind: [{{ .Values.broken }}\n---\n" + crdDoc("f.x.io"),
			"opt/op/crds/commented.yaml":  "--- # first\n" + crdDoc("g.x.io"),
			"opt/op/crds/separator.yaml":  "--- {{ .Values.header }}\n" + crdDoc("h.x.io"),
		}), []string{"a.x.io", "b.x.io", "f.x.io", "g.x.io", "h.x.io"}, ""},
		{"oversized file outside dir", tarOf(t, map[string]string{"opt/other/big.yaml": oversized}), nil, ""},
		{"oversized file under dir", tarOf(t, map[string]string{"opt/op/crds/big.yaml": oversized}),
			nil, errors.ErrCodeInvalidRequest},
		{"truncated tar stream", []byte("not a tar"), nil, errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := crdsInTar(bytes.NewReader(tt.archive), "/opt/op/crds/")
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("crdsInTar() error = %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("crdsInTar() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadImageCRDs(t *testing.T) {
	srv := httptest.NewServer(ociregistry.New(ociregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	push := func(tag string, files map[string]string) {
		t.Helper()
		filemap := map[string][]byte{}
		for file, body := range files {
			filemap[file] = []byte(body)
		}
		img, err := crane.Image(filemap)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := name.ParseReference(host + "/op:" + tag)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, img, remote.WithContext(t.Context())); err != nil {
			t.Fatal(err)
		}
	}
	push("1", map[string]string{"opt/op/crds/a.yaml": crdDoc("a.x.io"), "opt/other/b.yaml": crdDoc("b.x.io")})
	push("big", map[string]string{"opt/op/crds/big.yaml": strings.Repeat("#", maxCRDFileBytes+1)})

	tests := []struct {
		name     string
		image    string
		want     []string
		wantCode errors.ErrorCode
	}{
		{"crds under dir", host + "/op:1", []string{"a.x.io"}, ""},
		{"oversized crd file", host + "/op:big", nil, errors.ErrCodeInvalidRequest},
		{"unknown tag", host + "/op:9", nil, errors.ErrCodeUnavailable},
		{"malformed reference", "UPPER/Case:tag", nil, errors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readImageCRDs(t.Context(), tt.image, "opt/op/crds")
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("readImageCRDs() error = %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("readImageCRDs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeRenderer returns the YAML under "<name>@<version>", else under the
// release name.
type fakeRenderer map[string]string

func (f fakeRenderer) Render(_ context.Context, in helm.ChartInput) ([]byte, error) {
	if !in.IncludeCRDs {
		return nil, os.ErrInvalid
	}
	if out, ok := f[in.Name+"@"+in.Version]; ok {
		return []byte(out), nil
	}
	return []byte(f[in.Name]), nil
}

const exampleRepo = "https://charts.example.com"

func helmRef(name, version, asserts string) recipe.ComponentRef {
	return recipe.ComponentRef{Name: name, Type: recipe.ComponentTypeHelm, Source: exampleRepo, Version: version,
		HealthCheckAsserts: asserts}
}

func leafOf(name string, refs ...recipe.ComponentRef) recipe.ResolvedLeaf {
	return recipe.ResolvedLeaf{Entry: recipe.CatalogEntry{Name: name}, Result: &recipe.RecipeResult{ComponentRefs: refs}}
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	chartCheck := healthCheck(assertOp(crdKind, "a.x.io"), assertOp(crdKind, "vendored.x.io"))
	dp := newMapProvider(t, map[string]string{
		"registry.yaml": registryHeader + `components:
  - name: chart
    healthCheck: {assertFile: checks/chart/health-check.yaml}
    helm: {defaultRepository: https://charts.example.com, defaultChart: ex/chart, defaultVersion: "1.0.0"}
  - name: no-asserts
    healthCheck: {assertFile: checks/no-asserts/health-check.yaml}
    helm: {defaultRepository: https://charts.example.com, defaultVersion: "1.0.0"}
  - name: dynamo-platform
    healthCheck: {assertFile: checks/dynamo-platform/health-check.yaml}
    helm: {defaultRepository: https://charts.example.com, defaultVersion: "2.0.0"}
  - name: manifest-only
    healthCheck: {assertFile: checks/manifest-only/health-check.yaml}
`,
		"checks/chart/health-check.yaml":           chartCheck,
		"components/chart/manifests/a.yaml":        crdDoc("vendored.x.io"),
		"checks/no-asserts/health-check.yaml":      healthCheck(assertOp("Pod", "p")),
		"checks/dynamo-platform/health-check.yaml": healthCheck(assertOp(crdKind, "img.x.io")),
		"checks/manifest-only/health-check.yaml":   healthCheck(assertOp(crdKind, "a.x.io")),
	})
	disabled := helmRef("chart", "9.9.9", chartCheck)
	disabled.Overrides = map[string]any{"enabled": false}
	leaves := []recipe.ResolvedLeaf{
		leafOf("pinned", helmRef("chart", "1.1.0", chartCheck),
			helmRef("inline", "3.0.0", healthCheck(assertOp(crdKind, "i.x.io"))),
			helmRef("no-asserts", "1.1.0", healthCheck(assertOp("Pod", "p")))),
		leafOf("disabled", disabled),
	}
	readInv := func(name string) inventory {
		t.Helper()
		inv, err := serializer.FromFile[inventory](inventoryPath(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return *inv
	}

	renderer := fakeRenderer{
		"chart":       crdDoc("a.x.io") + "---\n" + crdDoc("b.x.io"),
		"chart@1.1.0": crdDoc("a.x.io") + "---\n" + crdDoc("c.x.io"),
		"inline":      crdDoc("i.x.io"),
		"dynamo-platform": "kind: Deployment\nspec:\n  template:\n    spec:\n      initContainers:\n" +
			"        - name: crd-apply\n          image: op:2.0.0\n",
	}
	readImage := func(_ context.Context, image, dir string) ([]string, error) {
		if image != "op:2.0.0" || dir != imageCRDSources["dynamo-platform"].Dir {
			t.Errorf("readImage(%q, %q), want the rendered container image and the source dir", image, dir)
		}
		return []string{"img.x.io"}, nil
	}
	orphan := inventoryPath(root, "no-asserts")
	writeFile(t, orphan, "component: no-asserts\n")

	err := run(t.Context(), root, dp, leaves, renderer, readImage, true)
	if err == nil || !strings.Contains(err.Error(), "belongs to no CRD assertion") {
		t.Fatalf("run(check) with no committed inventories and an orphan error = %v, want stale and orphan", err)
	}
	if err = run(t.Context(), root, dp, leaves, renderer, readImage, false); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	wantChart := inventory{Component: "chart", Charts: []chartCRDs{
		{Repository: exampleRepo, Chart: "chart", SourceVersion: "1.0.0", CRDs: []string{"a.x.io", "b.x.io"}},
		{Repository: exampleRepo, Chart: "chart", SourceVersion: "1.1.0", CRDs: []string{"a.x.io", "c.x.io"}},
	}}
	if got := readInv("chart"); !reflect.DeepEqual(got, wantChart) {
		t.Errorf("chart inventory = %+v, want %+v", got, wantChart)
	}
	wantInline := inventory{Component: "inline", Charts: []chartCRDs{
		{Repository: exampleRepo, Chart: "inline", SourceVersion: "3.0.0", CRDs: []string{"i.x.io"}},
	}}
	if got := readInv("inline"); !reflect.DeepEqual(got, wantInline) {
		t.Errorf("inline inventory = %+v, want %+v", got, wantInline)
	}
	wantImage := inventory{Component: "dynamo-platform", Charts: []chartCRDs{
		{Repository: exampleRepo, Chart: "dynamo-platform", SourceVersion: "2.0.0", Image: "op:2.0.0",
			CRDs: []string{"img.x.io"}},
	}}
	if got := readInv("dynamo-platform"); !reflect.DeepEqual(got, wantImage) {
		t.Errorf("image inventory = %+v, want %+v", got, wantImage)
	}
	for _, name := range []string{"no-asserts", "manifest-only"} {
		if _, statErr := os.Stat(inventoryPath(root, name)); !os.IsNotExist(statErr) {
			t.Errorf("inventory for %q exists (stat error %v), want none", name, statErr)
		}
	}
	if err = run(t.Context(), root, dp, leaves, renderer, readImage, true); err != nil {
		t.Errorf("run(check) after regeneration error = %v", err)
	}

	err = run(t.Context(), root, dp, leaves[1:], renderer, readImage, true)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Errorf("run(check) after the leaf pin went away error = %v, want stale inventory", err)
	}

	renderer["chart@1.1.0"] = crdDoc("c.x.io")
	err = run(t.Context(), root, dp, leaves, renderer, readImage, true)
	for _, want := range []string{`"a.x.io"`, "leaf recipe pinned", "stale"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("run(check) after the leaf's chart dropped a.x.io error = %v, want it to contain %s", err, want)
		}
	}

	renderer["chart"] = "kind: Deployment\n"
	err = run(t.Context(), root, dp, leaves, renderer, readImage, false)
	if err == nil || !strings.Contains(err.Error(), "installs none") {
		t.Errorf("run() for a chart with no CRDs error = %v, want installs none", err)
	}
}

type renderFunc func(context.Context, helm.ChartInput) ([]byte, error)

func (f renderFunc) Render(ctx context.Context, in helm.ChartInput) ([]byte, error) {
	return f(ctx, in)
}

func TestRunErrors(t *testing.T) {
	const reg = registryHeader + `components:
  - name: chart
    healthCheck: {assertFile: checks/chart/health-check.yaml}
    helm: {defaultRepository: https://charts.example.com, defaultVersion: "1.0.0"}
  - name: dynamo-platform
    healthCheck: {assertFile: checks/dynamo-platform/health-check.yaml}
    helm: {defaultRepository: https://charts.example.com, defaultVersion: "2.0.0"}
`
	valid := map[string]string{
		"registry.yaml":                            reg,
		"checks/chart/health-check.yaml":           healthCheck(assertOp(crdKind, "a.x.io")),
		"checks/dynamo-platform/health-check.yaml": healthCheck(assertOp(crdKind, "img.x.io")),
	}
	with := func(overrides map[string]string) map[string]string {
		files := maps.Clone(valid)
		maps.Copy(files, overrides)
		return files
	}
	crdApply := "kind: Deployment\nspec:\n  template:\n    spec:\n      initContainers:\n" +
		"        - name: crd-apply\n          image: op:2.0.0\n"
	render := func(byName map[string]string) renderFunc {
		rendered := map[string]string{"chart": crdDoc("a.x.io"), "dynamo-platform": crdApply}
		maps.Copy(rendered, byName)
		return func(_ context.Context, in helm.ChartInput) ([]byte, error) {
			return []byte(rendered[in.Name]), nil
		}
	}
	readImage := func(context.Context, string, string) ([]string, error) { return []string{"img.x.io"}, nil }

	tests := []struct {
		name      string
		files     map[string]string
		leaves    []recipe.ResolvedLeaf
		diskFiles []string
		diskDirs  []string
		render    renderFunc
		readImage imageReader
		canceled  bool
		wantCode  errors.ErrorCode
	}{
		{name: "leaf fails to resolve", files: valid,
			leaves:   []recipe.ResolvedLeaf{{Entry: recipe.CatalogEntry{Name: "l"}, Err: errors.New(errors.ErrCodeInvalidRequest, "bad overlay")}},
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "leaf health check not yaml", files: valid,
			leaves:   []recipe.ResolvedLeaf{leafOf("l", helmRef("chart", "1.0.0", "spec: ["))},
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "orphan inventory cannot be deleted", files: valid,
			diskFiles: []string{"recipes/components/other/crds.yaml/x"}, wantCode: errors.ErrCodeInternal},
		{name: "missing registry", wantCode: errors.ErrCodeInternal},
		{name: "registry without header", files: with(map[string]string{"registry.yaml": "components: []\n"}),
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "missing health check", files: map[string]string{"registry.yaml": reg},
			wantCode: errors.ErrCodeNotFound},
		{name: "health check not yaml", files: with(map[string]string{"checks/chart/health-check.yaml": "spec: ["}),
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "component directory is a file", files: valid, diskFiles: []string{"recipes/components/chart"},
			wantCode: errors.ErrCodeInternal},
		{name: "render fails", files: valid,
			render: func(context.Context, helm.ChartInput) ([]byte, error) {
				return nil, errors.New(errors.ErrCodeUnavailable, "chart repository unreachable")
			},
			wantCode: errors.ErrCodeUnavailable},
		{name: "rendered chart not yaml", files: valid, render: render(map[string]string{"chart": "kind: ["}),
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "inventory path is a directory", files: valid, diskDirs: []string{"recipes/components/chart/crds.yaml"},
			wantCode: errors.ErrCodeInternal},
		{name: "no CRD container", files: valid,
			render:   render(map[string]string{"dynamo-platform": crdDoc("b.x.io")}),
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "image read fails", files: valid,
			readImage: func(context.Context, string, string) ([]string, error) {
				return nil, errors.New(errors.ErrCodeUnavailable, "registry unreachable")
			},
			wantCode: errors.ErrCodeUnavailable},
		{name: "canceled", files: valid, canceled: true, wantCode: errors.ErrCodeCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, file := range tt.diskFiles {
				writeFile(t, filepath.Join(root, file), "")
			}
			for _, dir := range tt.diskDirs {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			dp := newMapProvider(t, tt.files)
			r := tt.render
			if r == nil {
				r = render(nil)
			}
			ri := tt.readImage
			if ri == nil {
				ri = readImage
			}
			ctx := t.Context()
			if tt.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			errorstest.WantReportedCode(t, run(ctx, root, dp, tt.leaves, r, ri, false), tt.wantCode)
		})
	}
}

func TestDecodeInventory(t *testing.T) {
	want := inventory{Component: "c", Charts: []chartCRDs{
		{Repository: exampleRepo, Chart: "c", SourceVersion: "1.0.0", Image: "op:1", CRDs: []string{"a.x.io"}},
	}}
	rendered, err := renderInventory(&want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeInventory(rendered)
	if err != nil {
		t.Fatalf("decodeInventory(rendered) error = %v", err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("decodeInventory(rendered) = %+v, want %+v", *got, want)
	}
	for _, data := range []string{"charts: [", "component: c\nversion: 1.0.0\n"} {
		_, err := decodeInventory([]byte(data))
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
