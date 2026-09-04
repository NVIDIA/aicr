// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package architecture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadForAnalysis(t *testing.T) {
	t.Parallel()

	loaded := loadForAnalysis(t, "github.com/NVIDIA/aicr/pkg/cli", "github.com/NVIDIA/aicr/pkg/server")

	for _, path := range []string{"github.com/NVIDIA/aicr/pkg/cli", "github.com/NVIDIA/aicr/pkg/server"} {
		lp, ok := loaded[path]
		if !ok {
			t.Fatalf("package %s missing from load result", path)
		}
		if lp.Info == nil || len(lp.Info.Uses) == 0 {
			t.Errorf("%s: no Uses recorded; type-checking did not run over source", path)
		}
		if len(lp.Info.Selections) == 0 {
			t.Errorf("%s: no Selections recorded; foreign-type method analysis would be blind", path)
		}
	}
}

// listedPackage mirrors the subset of `go list -json` this gate consumes.
type listedPackage struct {
	ImportPath string
	Dir        string
	Export     string
	GoFiles    []string
}

type loadedPackage struct {
	Path string
	Fset *token.FileSet
	Info *types.Info
	Pkg  *types.Package
}

// goListTimeout matches tools/api-diff-closure: building export data for a
// dependency closure can take minutes on a cold or contended CI runner.
const goListTimeout = 5 * time.Minute

// loadCache memoizes loadForAnalysis by its sorted, joined package-path set.
// Four call sites across this package request overlapping package sets while
// running in parallel (t.Parallel()), and each cache miss reruns
// `go list -json -export -deps` plus a from-source type-check of the whole
// dependency closure -- the single largest cost in this package's test run.
// A repeat request for the same set reuses the prior result instead of
// redoing that work; callers only read Info/Pkg/Fset, so sharing the
// returned map across tests is safe.
var loadCache sync.Map // key: cacheKey(packagePaths) -> *loadCacheEntry

// loadCacheEntry guards a single package-set's load with a sync.Once, plus an
// ok flag checked after Do returns. testing.T.Fatalf unwinds via
// runtime.Goexit, which still runs Once's internal "mark done" defer, so a
// populating call that fails leaves the entry permanently "done" but with ok
// still false. Every other goroutine waiting on that same key must notice ok
// is false and fail loudly on its own *testing.T rather than silently
// returning a zero-value result.
type loadCacheEntry struct {
	once   sync.Once
	result map[string]loadedPackage
	ok     bool
}

// cacheKey sorts and joins packagePaths so that request order never produces
// a spurious cache miss.
func cacheKey(packagePaths []string) string {
	sorted := append([]string(nil), packagePaths...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\x00")
}

// loadForAnalysis type-checks each requested package from source, resolving its
// dependencies from gc export data produced by `go list -export -deps`.
// Results are memoized per package set; see loadCache.
func loadForAnalysis(t *testing.T, packagePaths ...string) map[string]loadedPackage {
	t.Helper()

	entryAny, _ := loadCache.LoadOrStore(cacheKey(packagePaths), &loadCacheEntry{})
	entry := entryAny.(*loadCacheEntry)

	entry.once.Do(func() {
		entry.result = loadForAnalysisUncached(t, packagePaths...)
		entry.ok = true
	})
	if !entry.ok {
		t.Fatalf("loadForAnalysis(%v): a concurrent call for the same package set already failed", packagePaths)
	}
	return entry.result
}

// loadForAnalysisUncached does the actual `go list` + from-source type-check
// work for loadForAnalysis. Split out so loadForAnalysis can memoize it.
func loadForAnalysisUncached(t *testing.T, packagePaths ...string) map[string]loadedPackage {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), goListTimeout)
	defer cancel()

	root := repoRoot(t)
	args := append([]string{"list", "-json", "-export", "-deps"}, packagePaths...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("go list failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		t.Fatalf("run go list: %v", err)
	}

	listed := make(map[string]listedPackage)
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode go list output: %v", err)
		}
		listed[pkg.ImportPath] = pkg
	}

	lookup := func(path string) (io.ReadCloser, error) {
		pkg, ok := listed[path]
		if !ok {
			return nil, fmt.Errorf("package %s was not returned by go list", path)
		}
		if pkg.Export == "" {
			return nil, fmt.Errorf("package %s has no export data", path)
		}
		file, err := os.Open(pkg.Export) //nolint:gosec // path comes from trusted go list export metadata
		if err != nil {
			return nil, fmt.Errorf("open export data for %s: %w", path, err)
		}
		return file, nil
	}

	result := make(map[string]loadedPackage, len(packagePaths))
	for _, path := range packagePaths {
		pkg, ok := listed[path]
		if !ok {
			t.Fatalf("go list omitted %s", path)
		}
		fset := token.NewFileSet()
		files := make([]*ast.File, 0, len(pkg.GoFiles))
		for _, name := range pkg.GoFiles {
			full := filepath.Join(pkg.Dir, name)
			parsed, err := parser.ParseFile(fset, full, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", full, err)
			}
			files = append(files, parsed)
		}
		info := &types.Info{
			Uses:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
		}
		conf := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
		checked, err := conf.Check(path, fset, files, info)
		if err != nil {
			t.Fatalf("type-check %s: %v", path, err)
		}
		result[path] = loadedPackage{Path: path, Fset: fset, Info: info, Pkg: checked}
	}
	return result
}

// repoRoot walks up from the test's working directory to the directory holding
// go.mod. `go test` runs with the package directory as cwd, so this is two
// levels up in practice, but walking is robust to the package moving.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test working directory")
		}
		dir = parent
	}
}
