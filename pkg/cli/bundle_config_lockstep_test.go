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

package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strings"
	"testing"
)

// configOptionCalls returns the sorted, de-duplicated set of config.With*
// function names invoked anywhere in the body of the function or method
// named fnName, declared in the Go source file at path. path is resolved
// relative to this package's directory (pkg/cli), matching how `go test`
// sets the working directory.
//
// Both call sites this guards only ever call config.With* inside the target
// function — runBundleCmdWithDependencies' single config.NewConfig(...) call,
// and bundlerConfig's []config.Option build plus its two conditional
// appends — so a blanket walk of the whole function body is precise enough;
// neither function needs sub-scoping to exclude an unrelated config.With*
// call.
func configOptionCalls(t *testing.T, path, fnName string) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var fn ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if fd, ok := n.(*ast.FuncDecl); ok && fd.Name.Name == fnName {
			fn = fd
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatalf("could not find func/method %s in %s; this guard cannot enforce its invariant", fnName, path)
	}

	seen := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok || pkgIdent.Name != "config" || !strings.HasPrefix(sel.Sel.Name, "With") {
			return true
		}
		seen[sel.Sel.Name] = true
		return true
	})

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestBundleCmd_NewConfigMatchesBundlerConfigFields guards the lockstep
// bundlerConfig's godoc calls out (pkg/client/v1/bundle.go): a bundler
// setting with a spec.bundle counterpart must be wired into three places —
// the CLI's config.NewConfig call (runBundleCmdWithDependencies, this
// package), the flat BundleOptions field (Config.BundleOptions,
// pkg/client/v1/config.go), and bundlerConfig's translation back to
// config.Option (pkg/client/v1/bundle.go) — with no single guard binding all
// three. This test pins the first and third directly: the set of
// config.With* calls the CLI issues, minus the settings that are
// deliberately CLI-only (no spec.bundle counterpart, so Config.BundleOptions
// never derives them and bundlerConfig never reads them from a flat field —
// see BundleOptions' "Two ways to supply the bundler configuration" godoc),
// must equal the set bundlerConfig issues when Config is unset.
//
// A future bundler setting wired into the CLI's NewConfig call but forgotten
// in bundlerConfig (or the reverse) shows up here as an asymmetric diff
// instead of silently diverging between a CLI-generated bundle and a
// config-driven SDK bundle that goes through the flat BundleOptions fields.
func TestBundleCmd_NewConfigMatchesBundlerConfigFields(t *testing.T) {
	t.Parallel()

	cliCalls := configOptionCalls(t, "bundle.go", "runBundleCmdWithDependencies")
	facadeCalls := configOptionCalls(t, "../client/v1/bundle.go", "bundlerConfig")

	// CLI-only settings: no spec.bundle counterpart. Named explicitly, rather
	// than diffed away silently, so adding one here is a reviewable decision
	// documented alongside BundleOptions' own "Two ways to supply the bundler
	// configuration" godoc, not a change that quietly widens what this test
	// accepts.
	cliOnly := map[string]bool{
		"WithVersion":                  true,
		"WithTargetRevision":           true,
		"WithValueOverridesTypedPaths": true,
		"WithReadinessHooks":           true,
		"WithSerial":                   true,
		"WithOCISourceName":            true,
		"WithFluxNamespace":            true,
		"WithBundleChartName":          true,
		"WithBundleChartVersion":       true,
		"WithOCIParentNamespace":       true,
	}

	var cliBundlerFields []string
	for _, name := range cliCalls {
		if !cliOnly[name] {
			cliBundlerFields = append(cliBundlerFields, name)
		}
	}
	sort.Strings(cliBundlerFields)

	if !slices.Equal(cliBundlerFields, facadeCalls) {
		t.Errorf("CLI's config.NewConfig call issues %v (bundler-setting subset)\n"+
			"bundlerConfig() issues                    %v\n"+
			"a bundler setting was wired into only one side of the three-way "+
			"lockstep (CLI NewConfig, BundleOptions flat field, bundlerConfig)",
			cliBundlerFields, facadeCalls)
	}
}
