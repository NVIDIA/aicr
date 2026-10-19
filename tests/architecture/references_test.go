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
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	// Source is type-checked standalone; it needs no aicr dependency, so the
	// classifier is tested in isolation from export data.
	src := `package sample

type Widget struct{ Name string }

const Limit = 10

var Registry = map[string]int{}

var Hook = func() string { return "" }

type Executor func()

var Run Executor

func Build() Widget { return Widget{} }

func (w Widget) Render() string { return w.Name }
`
	pkg, _, _ := checkSource(t, "sample", src)

	want := map[string]symbolClass{
		"Widget":   classType,
		"Limit":    classConst,
		"Registry": classVar,
		"Hook":     classBehavioral,
		"Run":      classBehavioral,
		"Build":    classBehavioral,
	}
	for name, expected := range want {
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("symbol %s not found in checked package", name)
		}
		if got := classify(obj); got != expected {
			t.Errorf("classify(%s) = %q, want %q", name, got, expected)
		}
	}

	// A method is a *types.Func like a package-level function, but it isn't
	// found via pkg.Scope().Lookup -- methods live on the receiver type's
	// method set, not the package scope. Resolve Widget.Render through
	// types.LookupFieldOrMethod so the receiver-method case of the *types.Func
	// branch is pinned explicitly, not merely implied by the free-function case.
	widgetObj := pkg.Scope().Lookup("Widget")
	if widgetObj == nil {
		t.Fatal("Widget type not found in checked package")
	}
	methodObj, _, _ := types.LookupFieldOrMethod(widgetObj.Type(), false, pkg, "Render")
	if methodObj == nil {
		t.Fatal("Widget.Render method not found via LookupFieldOrMethod")
	}
	if _, ok := methodObj.(*types.Func); !ok {
		t.Fatalf("Widget.Render resolved to %T, want *types.Func", methodObj)
	}
	if got := classify(methodObj); got != classBehavioral {
		t.Errorf("classify(Widget.Render) = %q, want %q", got, classBehavioral)
	}
}

// TestClassifyDefaultBranch pins classify's fail-closed default: an object
// kind none of the explicit cases name still comes back classBehavioral
// (earning review) rather than silently classType or classVar. *types.PkgName
// -- the name bound by an import declaration -- is such a kind, obtained here
// from a real type-checked fixture rather than fabricated by hand.
func TestClassifyDefaultBranch(t *testing.T) {
	t.Parallel()

	const src = `package importfixture

import osalias "os"

var _ = osalias.Args
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "importfixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := &types.Info{
		Defs: make(map[*ast.Ident]types.Object),
	}
	conf := types.Config{Importer: importer.Default()}
	if _, err := conf.Check("importfixture", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("type-check: %v", err)
	}

	var pkgNameObj types.Object
	for ident, obj := range info.Defs {
		if ident.Name == "osalias" {
			pkgNameObj = obj
		}
	}
	if pkgNameObj == nil {
		t.Fatal("did not resolve a types.Object for the osalias import")
	}
	if _, ok := pkgNameObj.(*types.PkgName); !ok {
		t.Fatalf("osalias resolved to %T, want *types.PkgName", pkgNameObj)
	}
	if got := classify(pkgNameObj); got != classBehavioral {
		t.Errorf("classify(*types.PkgName) = %q, want %q (fail-closed default)", got, classBehavioral)
	}
}

// checkSource type-checks a single self-contained source file with no imports.
// Used by the classifier and fixture tests, which must not depend on export
// data for speed and hermeticity.
func checkSource(t *testing.T, name, src string) (*types.Package, *types.Info, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name+".go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := &types.Info{
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	conf := types.Config{Importer: importer.Default()}
	pkg, err := conf.Check(name, fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	return pkg, info, fset
}

// classify maps a resolved object to its policy class. Funcs are behavioral —
// calling one drives work in the owning package. Everything else is inert
// structure the caller merely holds, names, or reads. A package-level var
// whose type is a func signature is also behavioral: calling it drives work
// exactly like a func declaration does. This includes both direct signatures
// (e.g. `var Hook = func() {...}`) and vars declared with a defined function
// type (e.g. `type Executor func(); var Run Executor`). Struct fields never
// reach this branch as *types.Var — they are filtered earlier by the
// IsField() guard in packageQualifiedRefs — so this only affects
// package-level vars.
func classify(obj types.Object) symbolClass {
	switch o := obj.(type) {
	case *types.Func:
		return classBehavioral
	case *types.TypeName:
		return classType
	case *types.Const:
		return classConst
	case *types.Var:
		if _, ok := types.Unalias(o.Type()).Underlying().(*types.Signature); ok {
			return classBehavioral
		}
		return classVar
	default:
		return classBehavioral // fail closed: an unrecognized object earns review
	}
}

func TestPackageQualifiedRefs(t *testing.T) {
	t.Parallel()

	loaded := loadForAnalysis(t, "github.com/NVIDIA/aicr/pkg/server")
	refs := packageQualifiedRefs(loaded["github.com/NVIDIA/aicr/pkg/server"], "github.com/NVIDIA/aicr/")

	// pkg/server is known to reference pkg/errors behaviorally and to name the
	// facade. If either disappears the gate's own inputs have changed.
	var sawErrors, sawFacade bool
	for ref := range refs {
		if ref.Package == "pkg/errors" {
			sawErrors = true
		}
		if ref.Package == "pkg/client/v1" {
			sawFacade = true
		}
		if strings.HasPrefix(ref.Package, "github.com/") {
			t.Errorf("package %q was not trimmed to a module-relative path", ref.Package)
		}
	}
	if !sawErrors {
		t.Error("no pkg/errors reference found in pkg/server")
	}
	if !sawFacade {
		t.Error("no pkg/client/v1 reference found in pkg/server")
	}
}

type reference struct {
	Package string
	Symbol  string
	Class   symbolClass
}

// packageQualifiedRefs collects every `pkg.Symbol` reference into a package
// under modulePrefix. It walks types.Info.Uses rather than the AST so that
// aliased imports resolve correctly and non-package identifiers of the same
// spelling are never confused for one.
func packageQualifiedRefs(lp loadedPackage, modulePrefix string) map[reference]bool {
	refs := make(map[reference]bool)
	for ident, obj := range lp.Info.Uses {
		if obj == nil || obj.Pkg() == nil {
			continue
		}
		owner := obj.Pkg().Path()
		if !strings.HasPrefix(owner, modulePrefix) || owner == lp.Path {
			continue
		}
		// Method objects are attributed to their receiver's package and are
		// handled by the Selections pass; skip them here so a method never
		// lands as a bare package-level symbol.
		if fn, ok := obj.(*types.Func); ok && fn.Signature() != nil && fn.Signature().Recv() != nil {
			continue
		}
		// Struct fields are *types.Var objects whose Pkg() is the declaring
		// package, so a field read or composite-literal key would otherwise be
		// recorded as if it named a package-level symbol. It doesn't: reading a
		// field is type-only use of a permitted type, and the type itself is
		// what the policy already records.
		if v, ok := obj.(*types.Var); ok && v.IsField() {
			continue
		}
		refs[reference{
			Package: strings.TrimPrefix(owner, modulePrefix),
			Symbol:  ident.Name,
			Class:   classify(obj),
		}] = true
	}
	return refs
}

// TestPackageQualifiedRefsSkipsStructFields pins that reading a struct field
// through a permitted type does not fabricate a package-level symbol entry.
// A struct field's types.Object is a *types.Var whose Pkg() is the declaring
// package, so a naive Uses scan records the field identifier exactly like a
// package-level one — it isn't: the field read is type-only use of a
// permitted type, and the type itself is what the policy already records.
func TestPackageQualifiedRefsSkipsStructFields(t *testing.T) {
	t.Parallel()

	const businessSrc = `package business

type Recipe struct{ Name string }
`
	const consumerSrc = `package consumer
import "fixture/business"
func F(r *business.Recipe) string { return r.Name }
`
	lp := checkTwoPackages(t, businessSrc, consumerSrc)
	refs := packageQualifiedRefs(lp, "fixture/")

	wantType := reference{Package: "business", Symbol: "Recipe", Class: classType}
	if !refs[wantType] {
		t.Errorf("missing type reference %+v in %+v", wantType, refs)
	}
	for ref := range refs {
		if ref.Symbol == "Name" {
			t.Errorf("struct field leaked into package-level refs: %+v", ref)
		}
	}
}

// TestImportFormsResolveIdentically pins that an aliased import and a
// dot-import produce the same reference as a plain one. An AST-based scan
// would miss both; the Uses-based scan resolves each identifier to an object
// whose Pkg() still names the business package.
func TestImportFormsResolveIdentically(t *testing.T) {
	t.Parallel()

	const businessSrc = `package business

type Recipe struct{ Name string }

func Build() Recipe { return Recipe{} }
`
	forms := map[string]string{
		"plain":   "package c\nimport \"fixture/business\"\nfunc F() business.Recipe { return business.Build() }\n",
		"aliased": "package c\nimport b \"fixture/business\"\nfunc F() b.Recipe { return b.Build() }\n",
		"dot":     "package c\nimport . \"fixture/business\"\nfunc F() Recipe { return Build() }\n",
	}

	want := map[reference]bool{
		{Package: "business", Symbol: "Recipe", Class: classType}:      true,
		{Package: "business", Symbol: "Build", Class: classBehavioral}: true,
	}

	for name, consumerSrc := range forms {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lp := checkTwoPackages(t, businessSrc, consumerSrc)
			got := packageQualifiedRefs(lp, "fixture/")
			if len(got) != len(want) {
				t.Fatalf("got %d refs, want %d: %+v", len(got), len(want), got)
			}
			for ref := range want {
				if !got[ref] {
					t.Errorf("missing reference %+v", ref)
				}
			}
		})
	}
}

// checkTwoPackages type-checks a consumer that imports one in-memory business
// package, returning the consumer as a loadedPackage. Both live in memory, so
// callers depend on no export data and no repo state.
func checkTwoPackages(t *testing.T, businessSrc, consumerSrc string) loadedPackage {
	t.Helper()

	fset := token.NewFileSet()
	businessFile, err := parser.ParseFile(fset, "business.go", businessSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse business: %v", err)
	}
	businessPkg, err := (&types.Config{Importer: importer.Default()}).
		Check("fixture/business", fset, []*ast.File{businessFile}, &types.Info{})
	if err != nil {
		t.Fatalf("type-check business: %v", err)
	}

	consumerFile, err := parser.ParseFile(fset, "consumer.go", consumerSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse consumer: %v", err)
	}
	info := &types.Info{
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	conf := types.Config{Importer: fixtureImporter{"fixture/business": businessPkg}}
	consumerPkg, err := conf.Check("fixture/consumer", fset, []*ast.File{consumerFile}, info)
	if err != nil {
		t.Fatalf("type-check consumer: %v", err)
	}
	return loadedPackage{Path: "fixture/consumer", Fset: fset, Info: info, Pkg: consumerPkg}
}

// fixtureImporter resolves the in-memory fixture packages and nothing else.
type fixtureImporter map[string]*types.Package

func (f fixtureImporter) Import(path string) (*types.Package, error) {
	if pkg, ok := f[path]; ok {
		return pkg, nil
	}
	return importer.Default().Import(path)
}

func TestForeignMethodRefs(t *testing.T) {
	t.Parallel()

	loaded := loadForAnalysis(t, "github.com/NVIDIA/aicr/pkg/server")
	refs := foreignMethodRefs(loaded["github.com/NVIDIA/aicr/pkg/server"], "github.com/NVIDIA/aicr/")

	if len(refs) == 0 {
		t.Fatal("no foreign-type method calls found in pkg/server; the pass is not working")
	}
	for ref := range refs {
		if ref.Class != classBehavioral {
			t.Errorf("%s.%s classified %q, want behavioral", ref.Package, ref.Symbol, ref.Class)
		}
		if !strings.Contains(ref.Symbol, ".") {
			t.Errorf("symbol %q is not in Type.Method form", ref.Symbol)
		}
	}
}

// foreignMethodRefs records method calls on types owned by a package under
// modulePrefix, in Type.Method form. A method value or expression is always
// behavioral: it runs code in the owning package.
func foreignMethodRefs(lp loadedPackage, modulePrefix string) map[reference]bool {
	refs := make(map[reference]bool)
	for _, sel := range lp.Info.Selections {
		if sel.Kind() != types.MethodVal && sel.Kind() != types.MethodExpr {
			continue
		}
		obj := sel.Obj()
		if obj == nil || obj.Pkg() == nil {
			continue
		}
		owner := obj.Pkg().Path()
		if !strings.HasPrefix(owner, modulePrefix) || owner == lp.Path {
			continue
		}
		// Use the declared receiver type from the method's signature, not from
		// the selection's receiver expression. When a local type embeds a foreign
		// type and calls a promoted method, sel.Recv() returns the outer local
		// type; the declared receiver is the actual method owner.
		var recv string
		if fn, ok := obj.(*types.Func); ok && fn.Signature() != nil && fn.Signature().Recv() != nil {
			declaredRecv := fn.Signature().Recv().Type()
			recv = receiverTypeName(declaredRecv)
		}
		// Fall back to the selection's receiver expression if declared receiver is unavailable.
		if recv == "" {
			recv = receiverTypeName(sel.Recv())
		}
		if recv == "" {
			continue
		}
		refs[reference{
			Package: strings.TrimPrefix(owner, modulePrefix),
			Symbol:  recv + "." + obj.Name(),
			Class:   classBehavioral,
		}] = true
	}
	return refs
}

// receiverTypeName reduces a receiver type to its bare named-type name,
// unwrapping pointers and generic instantiations so that *recipe.Recipe and
// recipe.Recipe produce the same policy key.
func receiverTypeName(t types.Type) string {
	for {
		switch typ := t.(type) {
		case *types.Pointer:
			t = typ.Elem()
		case *types.Named:
			return typ.Obj().Name()
		case *types.Alias:
			t = types.Unalias(typ)
		default:
			return ""
		}
	}
}

// TestForeignMethodRefsFixtures validates that foreignMethodRefs uses declared
// receiver types, not selection receiver expressions, and correctly handles
// embedded methods, pointers, aliases, and generics.
func TestForeignMethodRefsFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		business   string
		consumer   string
		wantSymbol string
	}{
		{
			name: "embedded method",
			business: `package business
type Recipe struct{ Name string }
func (r Recipe) Resolve() string { return r.Name }
`,
			consumer: `package consumer
import "fixture/business"
type Wrapper struct { business.Recipe }
func (w Wrapper) Use() string { return w.Resolve() }
`,
			wantSymbol: "Recipe.Resolve",
		},
		{
			name: "pointer receiver",
			business: `package business
type Recipe struct{ Name string }
func (r *Recipe) Resolve() string { return r.Name }
`,
			consumer: `package consumer
import "fixture/business"
func F(r *business.Recipe) string { return r.Resolve() }
`,
			wantSymbol: "Recipe.Resolve",
		},
		{
			name: "alias receiver",
			business: `package business
type Recipe struct{ Name string }
type RecipeAlias = Recipe
func (r RecipeAlias) Resolve() string { return r.Name }
`,
			consumer: `package consumer
import "fixture/business"
func F(r business.RecipeAlias) string { return r.Resolve() }
`,
			wantSymbol: "Recipe.Resolve",
		},
		{
			name: "generic instantiation",
			business: `package business
type Box[T any] struct{ v T }
func (b *Box[T]) Get() T { return b.v }
`,
			consumer: `package consumer
import "fixture/business"
func F(b *business.Box[int]) int { return b.Get() }
`,
			wantSymbol: "Box.Get",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lp := checkTwoPackages(t, tt.business, tt.consumer)
			refs := foreignMethodRefs(lp, "fixture/")

			var found bool
			for ref := range refs {
				if ref.Symbol == tt.wantSymbol {
					found = true
					if ref.Package != "business" {
						t.Errorf("%s package = %q, want business", tt.wantSymbol, ref.Package)
					}
					if ref.Class != classBehavioral {
						t.Errorf("%s class = %q, want behavioral", tt.wantSymbol, ref.Class)
					}
				}
			}
			if !found {
				t.Errorf("did not find %s in foreign method refs: %+v", tt.wantSymbol, refs)
			}
		})
	}
}
