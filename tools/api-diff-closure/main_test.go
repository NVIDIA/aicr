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
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTransparentAliasMappingsIncludesGenericAndPointerAliases(t *testing.T) {
	const modulePath = "example.com/project"
	target := checkPackage(t, modulePath+"/target", `
package target

type Generic[T any] struct {
	Value T
}

type Concrete struct{}
`, nil)
	facade := checkPackage(t, modulePath+"/facade", `
package facade

import "example.com/project/target"

type GenericAlias[T any] = target.Generic[T]
type PointerAlias = *target.Concrete
type Defined target.Concrete
type hiddenAlias = target.Concrete
`, packageImporter{target.Path(): target})

	got, err := transparentAliasMappings(facade, projectMembership(facade, target))
	if err != nil {
		t.Fatalf("transparentAliasMappings() error = %v", err)
	}
	want := []aliasMapping{
		{aliasName: "GenericAlias", packagePath: target.Path(), typeName: "Generic"},
		{aliasName: "PointerAlias", packagePath: target.Path(), typeName: "Concrete"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transparentAliasMappings() = %#v, want %#v", got, want)
	}
}

func TestTransparentAliasMappingsIncludesNonMaterializedAliases(t *testing.T) {
	t.Setenv("GODEBUG", "gotypesalias=0")
	const modulePath = "example.com/project"
	target := checkPackage(t, modulePath+"/target", `
package target

type Concrete struct{}
`, nil)
	facade := checkPackage(t, modulePath+"/facade", `
package facade

import "example.com/project/target"

type PlainAlias = target.Concrete
type PointerAlias = *target.Concrete
`, packageImporter{target.Path(): target})

	got, err := transparentAliasMappings(facade, projectMembership(facade, target))
	if err != nil {
		t.Fatalf("transparentAliasMappings() error = %v", err)
	}
	want := []aliasMapping{
		{aliasName: "PlainAlias", packagePath: target.Path(), typeName: "Concrete"},
		{aliasName: "PointerAlias", packagePath: target.Path(), typeName: "Concrete"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transparentAliasMappings() = %#v, want %#v", got, want)
	}
}

func TestTransparentAliasMappingsRejectsNonMaterializedGenericSpecialization(t *testing.T) {
	t.Setenv("GODEBUG", "gotypesalias=0")
	const modulePath = "example.com/project"
	target := checkPackage(t, modulePath+"/target", `
package target

type Generic[T any] struct {
	Value T
}
`, nil)
	facade := checkPackage(t, modulePath+"/facade", `
package facade

import "example.com/project/target"

type Public = target.Generic[int]
`, packageImporter{target.Path(): target})

	_, err := transparentAliasMappings(facade, projectMembership(facade, target))
	if err == nil {
		t.Fatal("transparentAliasMappings() error = nil, want non-materialized generic specialization error")
	}
	const wantDiagnostic = "exported alias example.com/project/facade.Public"
	if !strings.Contains(err.Error(), wantDiagnostic) {
		t.Fatalf("transparentAliasMappings() error = %q, want fragment %q", err, wantDiagnostic)
	}
}

func TestTransparentAliasMappingsRejectsUnsafeGenericSpecializations(t *testing.T) {
	const modulePath = "example.com/project"
	target := checkPackage(t, modulePath+"/target", `
package target

type Generic[T any] struct {
	Value T
}
`, nil)
	tests := []struct {
		name        string
		declaration string
		wantTarget  string
	}{
		{
			name:        "concrete argument",
			declaration: "type Public = target.Generic[int]",
			wantTarget:  modulePath + "/target.Generic[int]",
		},
		{
			name:        "transformed parameter",
			declaration: "type Public[T any] = target.Generic[[]T]",
			wantTarget:  modulePath + "/target.Generic[[]T]",
		},
		{
			name:        "narrowed parameter",
			declaration: "type Public[T ~int] = target.Generic[T]",
			wantTarget:  modulePath + "/target.Generic[T]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facade := checkPackage(t, modulePath+"/facade", `
package facade

import "example.com/project/target"

`+tt.declaration, packageImporter{target.Path(): target})

			_, err := transparentAliasMappings(facade, projectMembership(facade, target))
			if err == nil {
				t.Fatal("transparentAliasMappings() error = nil, want unsupported generic specialization error")
			}
			for _, fragment := range []string{
				"inspect exported alias example.com/project/facade.Public",
				"generic target alias instantiated as " + tt.wantTarget + " cannot be scoped safely",
				"concrete, transformed, and narrowed instantiations are unsupported",
			} {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("transparentAliasMappings() error = %q, want fragment %q", err, fragment)
				}
			}
		})
	}
}

func TestNormalizedAliasTargetPreservesConcreteTypeArguments(t *testing.T) {
	const modulePath = "example.com/project"
	payload := checkPackage(t, modulePath+"/payload", `
package payload

type Contract struct{}
`, nil)
	target := checkPackage(t, modulePath+"/target", `
package target

type Box[T any] struct {
	Value T
}
`, nil)
	facade := checkPackage(t, modulePath+"/facade", `
package facade

import (
	"example.com/project/payload"
	"example.com/project/target"
)

type Public = *target.Box[map[string][]*payload.Contract]
`, packageImporter{payload.Path(): payload, target.Path(): target})

	alias := facade.Scope().Lookup("Public").(*types.TypeName)
	got, err := normalizedAliasTarget(alias.Type())
	if err != nil {
		t.Fatalf("normalizedAliasTarget() error = %v", err)
	}
	if got.Obj().Pkg().Path() != target.Path() || got.Obj().Name() != "Box" {
		t.Fatalf("normalizedAliasTarget() target = %s, want %s.Box", got, target.Path())
	}
	if got.TypeArgs().Len() != 1 {
		t.Fatalf("normalizedAliasTarget() type argument count = %d, want 1", got.TypeArgs().Len())
	}
	if got.TypeArgs().At(0).String() != "map[string][]*example.com/project/payload.Contract" {
		t.Fatalf("normalizedAliasTarget() type argument = %s, want concrete payload container", got.TypeArgs().At(0))
	}
}

func TestTransparentAliasMappingsRejectsNonProjectTarget(t *testing.T) {
	const modulePath = "example.com/project"
	external := checkPackage(t, "example.com/external", `
package external

type Type struct{}
`, nil)
	facade := checkPackage(t, modulePath+"/facade", `
package facade

import "example.com/external"

type ExternalAlias = external.Type
`, packageImporter{external.Path(): external})

	membership := projectMembership(facade)
	membership.packageModulePaths[external.Path()] = "example.com/external"
	_, err := transparentAliasMappings(facade, membership)
	if err == nil {
		t.Fatal("transparentAliasMappings() error = nil, want non-project-target error")
	}
	const wantDiagnostic = "from module example.com/external, not root module example.com/project"
	if !strings.Contains(err.Error(), wantDiagnostic) {
		t.Fatalf("transparentAliasMappings() error = %q, want fragment %q", err, wantDiagnostic)
	}
}

func TestModuleMembershipUsesExactModuleIdentity(t *testing.T) {
	const modulePath = "example.com/project"
	membership := newModuleMembership(modulePath, map[string]string{
		modulePath:                        modulePath,
		modulePath + "/internal/contract": modulePath,
		modulePath + "/extension":         modulePath + "/extension",
		modulePath + "-tools":             modulePath + "-tools",
	})
	tests := []struct {
		name        string
		packagePath string
		want        bool
	}{
		{name: "module root", packagePath: modulePath, want: true},
		{name: "same-module dependency", packagePath: modulePath + "/internal/contract", want: true},
		{name: "nested dependency module", packagePath: modulePath + "/extension", want: false},
		{name: "similar external module", packagePath: modulePath + "-tools", want: false},
		{name: "standard library without module", packagePath: "time", want: false},
		{name: "unlisted package without module", packagePath: "command-line-arguments", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := membership.contains(tt.packagePath); got != tt.want {
				t.Fatalf("contains(%q) = %v, want %v", tt.packagePath, got, tt.want)
			}
		})
	}
}

func TestRunReportsCurrentAliasMappings(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}

	var stdout bytes.Buffer
	err = run([]string{
		"-dir", repositoryRoot,
		"-aliases", "github.com/NVIDIA/aicr/pkg/client/v1",
	}, &stdout)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	const want = "BundleArtifact|github.com/NVIDIA/aicr/pkg/bundler/result|Output\n" +
		"BundleAttester|github.com/NVIDIA/aicr/pkg/bundler/attestation|Attester\n" +
		"BundleConfig|github.com/NVIDIA/aicr/pkg/bundler/config|Config\n" +
		"BundleVerifyReport|github.com/NVIDIA/aicr/pkg/bundler/verifier|VerifyResult\n" +
		"CriteriaRegistry|github.com/NVIDIA/aicr/pkg/recipe|CriteriaRegistry\n" +
		"EvidenceVerification|github.com/NVIDIA/aicr/pkg/evidence/verifier|VerifyResult\n" +
		"OIDCResolveOptions|github.com/NVIDIA/aicr/pkg/bundler/attestation|ResolveOptions\n"
	if stdout.String() != want {
		t.Fatalf("run() output = %q, want %q", stdout.String(), want)
	}
}

func TestLoadPackagesKeepsNestedModuleOutsideClosure(t *testing.T) {
	const (
		modulePath       = "example.com/project"
		rootPackagePath  = modulePath + "/root"
		localPackagePath = modulePath + "/extensions"
	)
	workingDir := t.TempDir()
	writeFixtureFile(t, workingDir, "go.mod", `module example.com/project

go 1.26

require example.com/project/extension v0.0.0

replace example.com/project/extension => ./nested-extension
`)
	writeFixtureFile(t, workingDir, "nested-extension/go.mod", `module example.com/project/extension

go 1.26
`)
	writeFixtureFile(t, workingDir, "nested-extension/extension.go", `package extension

type Contract struct{}
`)
	writeFixtureFile(t, workingDir, "extensions/contract.go", `package extensions

type Contract struct{}
`)
	writeFixtureFile(t, workingDir, "root/root.go", `package root

import (
	"example.com/project/extension"
	"example.com/project/extensions"
)

type Root struct {
	Dependency extension.Contract
	Local      extensions.Contract
}
`)

	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOWORK", "off")
	ctx, cancel := context.WithTimeout(context.Background(), goListTimeout)
	defer cancel()
	packages, membership, err := loadPackages(ctx, workingDir, []string{rootPackagePath})
	if err != nil {
		t.Fatalf("loadPackages() error = %v", err)
	}
	const dependencyModulePath = modulePath + "/extension"
	if membership.contains(dependencyModulePath) {
		t.Fatal("nested dependency module package was classified as repository-local")
	}
	if gotModulePath, ok := membership.moduleForPackage(dependencyModulePath); !ok || gotModulePath != dependencyModulePath {
		t.Fatalf("nested dependency module identity = %q, %v, want %q, true", gotModulePath, ok, dependencyModulePath)
	}
	if !membership.contains(localPackagePath) {
		t.Fatal("same-module package with similar import path was not classified as repository-local")
	}

	got, err := reachableTypes(
		packages,
		membership,
		[]rootSpec{{packagePath: rootPackagePath, typeNames: []string{"Root"}}},
	)
	if err != nil {
		t.Fatalf("reachableTypes() error = %v", err)
	}
	want := []closureEntry{
		{packagePath: localPackagePath, typeName: "Contract"},
		{packagePath: rootPackagePath, typeName: "Root"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reachableTypes() = %#v, want %#v", got, want)
	}
}

func TestReachableTypes(t *testing.T) {
	const modulePath = "example.com/project"
	rootPackage := types.NewPackage(modulePath+"/root", "root")
	nestedPackage := types.NewPackage(modulePath+"/nested", "nested")
	externalPackage := types.NewPackage("example.com/external", "external")

	nested := newNamed(nestedPackage, "Nested", types.NewStruct(nil, nil))
	methodResult := newNamed(nestedPackage, "MethodResult", types.NewStruct(nil, nil))
	hidden := newNamed(rootPackage, "hidden", types.NewStruct(nil, nil))
	external := newNamed(externalPackage, "External", types.NewStruct(nil, nil))

	rootFields := []*types.Var{
		types.NewField(token.NoPos, rootPackage, "Nested", types.NewPointer(nested), false),
		types.NewField(token.NoPos, rootPackage, "External", external, false),
		types.NewField(token.NoPos, rootPackage, "private", hidden, false),
	}
	root := newNamed(rootPackage, "Root", types.NewStruct(rootFields, nil))
	methodSignature := types.NewSignatureType(
		types.NewVar(token.NoPos, rootPackage, "", types.NewPointer(root)),
		nil,
		nil,
		types.NewTuple(types.NewVar(token.NoPos, rootPackage, "input", nested)),
		types.NewTuple(types.NewVar(token.NoPos, rootPackage, "", methodResult)),
		false,
	)
	root.AddMethod(types.NewFunc(token.NoPos, rootPackage, "Transform", methodSignature))
	rootPackage.Scope().Insert(root.Obj())

	got, err := reachableTypes(
		map[string]*types.Package{rootPackage.Path(): rootPackage},
		projectMembership(rootPackage, nestedPackage),
		[]rootSpec{{packagePath: rootPackage.Path(), typeNames: []string{"Root"}}},
	)
	if err != nil {
		t.Fatalf("reachableTypes() error = %v", err)
	}
	want := []closureEntry{
		{packagePath: nestedPackage.Path(), typeName: "MethodResult"},
		{packagePath: nestedPackage.Path(), typeName: "Nested"},
		{packagePath: rootPackage.Path(), typeName: "Root"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reachableTypes() = %#v, want %#v", got, want)
	}
}

func TestReachableTypesTraversesUnexportedEmbeddedFields(t *testing.T) {
	const modulePath = "example.com/project"
	nested := checkPackage(t, modulePath+"/nested", `
package nested

type PromotedField struct{}
`, nil)
	root := checkPackage(t, modulePath+"/root", `
package root

import "example.com/project/nested"

type embedded struct {
	Promoted nested.PromotedField
}

type Root struct {
	embedded
}
`, packageImporter{nested.Path(): nested})

	got, err := reachableTypes(
		map[string]*types.Package{root.Path(): root},
		projectMembership(root, nested),
		[]rootSpec{{packagePath: root.Path(), typeNames: []string{"Root"}}},
	)
	if err != nil {
		t.Fatalf("reachableTypes() error = %v", err)
	}
	want := []closureEntry{
		{packagePath: nested.Path(), typeName: "PromotedField"},
		{packagePath: root.Path(), typeName: "Root"},
		{packagePath: root.Path(), typeName: "embedded"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reachableTypes() = %#v, want %#v", got, want)
	}
}

func TestReachableTypesTraversesNamedTypeParameters(t *testing.T) {
	const modulePath = "example.com/project"
	constraint := checkPackage(t, modulePath+"/constraint", `
package constraint

type Contract interface {
	Validate()
}
`, nil)
	root := checkPackage(t, modulePath+"/root", `
package root

import "example.com/project/constraint"

type Box[T constraint.Contract] struct{}
`, packageImporter{constraint.Path(): constraint})

	got, err := reachableTypes(
		map[string]*types.Package{root.Path(): root},
		projectMembership(root, constraint),
		[]rootSpec{{packagePath: root.Path(), typeNames: []string{"Box"}}},
	)
	if err != nil {
		t.Fatalf("reachableTypes() error = %v", err)
	}
	want := []closureEntry{
		{packagePath: constraint.Path(), typeName: "Contract"},
		{packagePath: root.Path(), typeName: "Box"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reachableTypes() = %#v, want %#v", got, want)
	}
}

func TestReachableTypesTraversesAliasTypeParameters(t *testing.T) {
	const modulePath = "example.com/project"
	constraint := checkPackage(t, modulePath+"/constraint", `
package constraint

type Contract interface {
	Validate()
}
`, nil)
	target := checkPackage(t, modulePath+"/target", `
package target

type Concrete struct{}
`, nil)
	facade := checkPackage(t, modulePath+"/facade", `
package facade

import (
	"example.com/project/constraint"
	"example.com/project/target"
)

type Public[T constraint.Contract] = target.Concrete
`, packageImporter{constraint.Path(): constraint, target.Path(): target})

	got, err := reachableTypes(
		map[string]*types.Package{facade.Path(): facade},
		projectMembership(facade, constraint, target),
		[]rootSpec{{packagePath: facade.Path(), typeNames: []string{"Public"}}},
	)
	if err != nil {
		t.Fatalf("reachableTypes() error = %v", err)
	}
	want := []closureEntry{
		{packagePath: constraint.Path(), typeName: "Contract"},
		{packagePath: target.Path(), typeName: "Concrete"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reachableTypes() = %#v, want %#v", got, want)
	}
}

func TestReachableTypesTraversesConcreteAliasArguments(t *testing.T) {
	const modulePath = "example.com/project"
	payload := checkPackage(t, modulePath+"/payload", `
package payload

type Contract struct {
	Stable string
}

type Unrelated struct {
	Legacy string
}
`, nil)
	external := checkPackage(t, "example.com/external", `
package external

import "example.com/project/payload"

type Wrapper[T any] struct {
	Value T
	Unrelated payload.Unrelated
}
`, packageImporter{payload.Path(): payload})
	target := checkPackage(t, modulePath+"/target", `
package target

type Box[T any] struct {
	Value T
}
`, nil)
	facade := checkPackage(t, modulePath+"/facade", `
package facade

import (
	"example.com/external"
	"example.com/project/payload"
	"example.com/project/target"
)

type PointerContainers = *target.Box[map[string][]*payload.Contract]
type ArrayChannel = target.Box[chan [2]*payload.Contract]
type ExternalContainer = target.Box[external.Wrapper[payload.Contract]]
`, packageImporter{external.Path(): external, payload.Path(): payload, target.Path(): target})

	want := []closureEntry{
		{packagePath: payload.Path(), typeName: "Contract"},
		{packagePath: target.Path(), typeName: "Box"},
	}
	for _, rootName := range []string{"PointerContainers", "ArrayChannel", "ExternalContainer"} {
		t.Run(rootName, func(t *testing.T) {
			got, err := reachableTypes(
				map[string]*types.Package{facade.Path(): facade},
				projectMembership(facade, payload, target),
				[]rootSpec{{packagePath: facade.Path(), typeNames: []string{rootName}}},
			)
			if err != nil {
				t.Fatalf("reachableTypes() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("reachableTypes() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestReachableTypesRejectsMissingRoot(t *testing.T) {
	pkg := types.NewPackage("example.com/project/root", "root")
	_, err := reachableTypes(
		map[string]*types.Package{pkg.Path(): pkg},
		projectMembership(pkg),
		[]rootSpec{{packagePath: pkg.Path(), typeNames: []string{"Missing"}}},
	)
	if err == nil {
		t.Fatal("reachableTypes() error = nil, want missing-root error")
	}
}

func TestParseRoots(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []rootSpec
		wantErr bool
	}{
		{
			name:  "splits and deduplicates types",
			input: []string{"example.com/project/pkg=Root,Nested", "example.com/project/pkg=Root"},
			want: []rootSpec{
				{packagePath: "example.com/project/pkg", typeNames: []string{"Root"}},
				{packagePath: "example.com/project/pkg", typeNames: []string{"Nested"}},
			},
		},
		{name: "missing separator", input: []string{"example.com/project/pkg"}, wantErr: true},
		{name: "empty package", input: []string{"=Root"}, wantErr: true},
		{name: "empty type", input: []string{"example.com/project/pkg="}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRoots(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRoots() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseRoots() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func newNamed(pkg *types.Package, name string, underlying types.Type) *types.Named {
	return types.NewNamed(types.NewTypeName(token.NoPos, pkg, name, nil), underlying, nil)
}

func projectMembership(packages ...*types.Package) moduleMembership {
	packageModulePaths := make(map[string]string, len(packages))
	for _, pkg := range packages {
		packageModulePaths[pkg.Path()] = "example.com/project"
	}
	return newModuleMembership("example.com/project", packageModulePaths)
}

func writeFixtureFile(t *testing.T, root, relativePath, content string) {
	t.Helper()
	path := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create fixture directory for %s: %v", relativePath, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture file %s: %v", relativePath, err)
	}
}

type packageImporter map[string]*types.Package

func (i packageImporter) Import(path string) (*types.Package, error) {
	pkg, ok := i[path]
	if !ok {
		return nil, fmt.Errorf("package %s not found", path)
	}
	return pkg, nil
}

func checkPackage(t *testing.T, packagePath, source string, importer types.Importer) *types.Package {
	t.Helper()

	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, packagePath+".go", source, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", packagePath, err)
	}
	config := types.Config{
		GoVersion: "go1.26",
		Importer:  importer,
	}
	pkg, err := config.Check(packagePath, fileSet, []*ast.File{file}, nil)
	if err != nil {
		t.Fatalf("type-check %s: %v", packagePath, err)
	}
	return pkg
}
