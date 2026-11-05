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

package providertest_test

import (
	"context"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
)

var _ recipe.DataProvider = (*providertest.Provider)(nil)

func TestProvider(t *testing.T) {
	p := providertest.NewMap(map[string][]byte{
		"registry.yaml":      []byte("r"),
		"overlays/base.yaml": []byte("b"),
		"overlays/leaf.yaml": []byte("l"),
	})

	for _, name := range []string{"overlays/base.yaml", "./overlays/../overlays/base.yaml"} {
		got, err := p.ReadFile(t.Context(), name)
		if err != nil || string(got) != "b" {
			t.Errorf("ReadFile(%q) = %q, %v, want b", name, got, err)
		}
	}
	if _, err := p.ReadFile(t.Context(), "missing.yaml"); !stderrors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile(missing) error = %v, want fs.ErrNotExist", err)
	}

	var walked []string
	err := p.WalkDir(t.Context(), "overlays/", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			walked = append(walked, name)
		}
		return nil
	})
	if want := []string{"overlays/base.yaml", "overlays/leaf.yaml"}; err != nil || !slices.Equal(walked, want) {
		t.Errorf("WalkDir(overlays) = %v, %v, want %v", walked, err, want)
	}

	if got := p.Source("registry.yaml"); got != "providertest:registry.yaml" {
		t.Errorf("Source() = %q, want the default", got)
	}
	custom := providertest.New(nil, providertest.WithSource(func(string) string { return recipe.CatalogSourceExternal }))
	if got := custom.Source("registry.yaml"); got != recipe.CatalogSourceExternal {
		t.Errorf("Source() with WithSource = %q, want %q", got, recipe.CatalogSourceExternal)
	}
}

func TestWriteDir(t *testing.T) {
	files := map[string][]byte{
		"registry.yaml":                       []byte(providertest.EmptyRegistryYAML),
		"components/gpu-operator/values.yaml": []byte("v"),
	}
	dir := providertest.WriteDir(t, files)
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || string(got) != string(want) {
			t.Errorf("read %s = %q, %v, want %q", name, got, err, want)
		}
	}
	layered, err := recipe.NewLayeredDataProvider(recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ""),
		recipe.LayeredProviderConfig{ExternalDir: dir})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	t.Cleanup(func() { recipe.EvictCachedRegistry(layered) })
	if _, err := recipe.GetComponentRegistryFor(layered); err != nil {
		t.Fatalf("EmptyRegistryYAML does not load as a --data registry: %v", err)
	}
}

// fatalTB hands out dir as its TempDir, records Fatalf, and stops the calling
// goroutine as the real method does.
type fatalTB struct {
	testing.TB
	dir    string
	failed bool
}

func (f *fatalTB) Helper()         {}
func (f *fatalTB) TempDir() string { return f.dir }

func (f *fatalTB) Fatalf(string, ...any) {
	f.failed = true
	runtime.Goexit()
}

func TestWriteDirFails(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"parent is a file", "file/x"},
		{"target is a directory", "dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "dir"), 0o750); err != nil {
				t.Fatal(err)
			}

			rec := &fatalTB{TB: t, dir: dir}
			done := make(chan struct{})
			go func() {
				defer close(done)
				providertest.WriteDir(rec, map[string][]byte{tt.path: []byte("x")})
			}()
			<-done

			if !rec.failed {
				t.Errorf("WriteDir(%q) did not fail", tt.path)
			}
		})
	}
}

func TestProviderContextDone(t *testing.T) {
	p := providertest.NewMap(map[string][]byte{"a.yaml": []byte("a")})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := p.ReadFile(ctx, "a.yaml")
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	err = p.WalkDir(ctx, ".", func(string, fs.DirEntry, error) error { return nil })
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
}
