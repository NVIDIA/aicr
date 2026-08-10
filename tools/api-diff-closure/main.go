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

// api-diff-closure inspects repository-local types used by the public SDK. It
// can print either the transparent aliases declared by one package or the
// named types reachable from a set of public API roots. It is an implementation
// detail of tools/api-diff.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const goListTimeout = 2 * time.Minute

type rootSpecs []string

func (r *rootSpecs) String() string {
	return strings.Join(*r, ",")
}

func (r *rootSpecs) Set(value string) error {
	if _, _, ok := strings.Cut(value, "="); !ok {
		return fmt.Errorf("root %q must have package=Type[,Type] form", value)
	}
	*r = append(*r, value)
	return nil
}

type rootSpec struct {
	packagePath string
	typeNames   []string
}

type listedPackage struct {
	ImportPath string
	Export     string
	Module     *struct {
		Path string
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "api-diff-closure: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("api-diff-closure", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	workingDir := flags.String("dir", ".", "module working directory")
	aliasPackage := flags.String("aliases", "", "package whose exported transparent aliases to print")
	var rawRoots rootSpecs
	flags.Var(&rawRoots, "root", "public API root in package=Type[,Type] form (repeatable)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *aliasPackage == "" && len(rawRoots) == 0 {
		return fmt.Errorf("either -aliases or at least one -root is required")
	}
	if *aliasPackage != "" && len(rawRoots) != 0 {
		return fmt.Errorf("-aliases and -root are mutually exclusive")
	}

	var (
		packagePaths []string
		roots        []rootSpec
		err          error
	)
	if *aliasPackage != "" {
		packagePaths = []string{*aliasPackage}
	} else {
		roots, err = parseRoots(rawRoots)
		if err != nil {
			return err
		}
		packagePaths = packagePathsForRoots(roots)
	}

	ctx, cancel := context.WithTimeout(context.Background(), goListTimeout)
	defer cancel()
	packages, modulePath, err := loadPackages(ctx, *workingDir, packagePaths)
	if err != nil {
		return err
	}

	if *aliasPackage != "" {
		aliases, mappingErr := transparentAliasMappings(packages[*aliasPackage], modulePath)
		if mappingErr != nil {
			return mappingErr
		}
		for _, alias := range aliases {
			if _, writeErr := fmt.Fprintf(stdout, "%s|%s|%s\n", alias.aliasName, alias.packagePath, alias.typeName); writeErr != nil {
				return fmt.Errorf("write alias mapping: %w", writeErr)
			}
		}
		return nil
	}

	closure, err := reachableTypes(packages, modulePath, roots)
	if err != nil {
		return err
	}
	for _, entry := range closure {
		if _, writeErr := fmt.Fprintf(stdout, "%s|%s\n", entry.packagePath, entry.typeName); writeErr != nil {
			return fmt.Errorf("write closure: %w", writeErr)
		}
	}
	return nil
}

func packagePathsForRoots(roots []rootSpec) []string {
	packagePaths := make([]string, 0, len(roots))
	for _, root := range roots {
		packagePaths = append(packagePaths, root.packagePath)
	}
	return packagePaths
}

func parseRoots(rawRoots []string) ([]rootSpec, error) {
	roots := make([]rootSpec, 0, len(rawRoots))
	seen := make(map[string]struct{})
	for _, raw := range rawRoots {
		packagePath, names, _ := strings.Cut(raw, "=")
		packagePath = strings.TrimSpace(packagePath)
		if packagePath == "" {
			return nil, fmt.Errorf("root %q has an empty package path", raw)
		}
		for _, name := range strings.Split(names, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, fmt.Errorf("root %q has an empty type name", raw)
			}
			key := packagePath + "=" + name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			roots = append(roots, rootSpec{packagePath: packagePath, typeNames: []string{name}})
		}
	}
	return roots, nil
}

func loadPackages(ctx context.Context, workingDir string, packagePaths []string) (map[string]*types.Package, string, error) {
	rootPackages := make([]string, 0, len(packagePaths))
	seen := make(map[string]struct{})
	for _, packagePath := range packagePaths {
		if _, ok := seen[packagePath]; ok {
			continue
		}
		seen[packagePath] = struct{}{}
		rootPackages = append(rootPackages, packagePath)
	}
	sort.Strings(rootPackages)

	listArgs := append([]string{"list", "-json", "-export", "-deps"}, rootPackages...)
	cmd := exec.CommandContext(ctx, "go", listArgs...)
	cmd.Dir = workingDir
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("load packages: %w", ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, "", fmt.Errorf("go list failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, "", fmt.Errorf("run go list: %w", err)
	}

	listed := make(map[string]listedPackage)
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			return nil, "", fmt.Errorf("decode go list output: %w", err)
		}
		listed[pkg.ImportPath] = pkg
	}

	modulePath := ""
	for _, packagePath := range rootPackages {
		pkg, ok := listed[packagePath]
		if !ok {
			return nil, "", fmt.Errorf("go list omitted root package %s", packagePath)
		}
		if pkg.Module == nil || pkg.Module.Path == "" {
			return nil, "", fmt.Errorf("root package %s is not in a Go module", packagePath)
		}
		if modulePath == "" {
			modulePath = pkg.Module.Path
		} else if modulePath != pkg.Module.Path {
			return nil, "", fmt.Errorf("root packages span modules %s and %s", modulePath, pkg.Module.Path)
		}
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
	packageImporter := importer.ForCompiler(token.NewFileSet(), "gc", lookup)
	loaded := make(map[string]*types.Package, len(rootPackages))
	for _, packagePath := range rootPackages {
		pkg, err := packageImporter.Import(packagePath)
		if err != nil {
			return nil, "", fmt.Errorf("import %s: %w", packagePath, err)
		}
		loaded[packagePath] = pkg
	}
	return loaded, modulePath, nil
}

type aliasMapping struct {
	aliasName   string
	packagePath string
	typeName    string
}

func transparentAliasMappings(pkg *types.Package, modulePath string) ([]aliasMapping, error) {
	if pkg == nil {
		return nil, fmt.Errorf("alias package was not loaded")
	}

	mappings := make([]aliasMapping, 0)
	for _, name := range pkg.Scope().Names() {
		object, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok || !object.Exported() || !object.IsAlias() {
			continue
		}

		target, err := normalizedAliasTarget(object.Type())
		if err != nil {
			return nil, fmt.Errorf("normalize exported alias %s.%s: %w", pkg.Path(), name, err)
		}
		targetObject := target.Obj()
		if targetObject.Pkg() == nil {
			return nil, fmt.Errorf("exported alias %s.%s targets non-package type %s", pkg.Path(), name, targetObject.Name())
		}
		if !isWithinModule(targetObject.Pkg().Path(), modulePath) {
			return nil, fmt.Errorf(
				"exported alias %s.%s targets type %s.%s outside module %s",
				pkg.Path(), name, targetObject.Pkg().Path(), targetObject.Name(), modulePath,
			)
		}
		mappings = append(mappings, aliasMapping{
			aliasName:   name,
			packagePath: targetObject.Pkg().Path(),
			typeName:    targetObject.Name(),
		})
	}

	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].aliasName == mappings[j].aliasName {
			if mappings[i].packagePath == mappings[j].packagePath {
				return mappings[i].typeName < mappings[j].typeName
			}
			return mappings[i].packagePath < mappings[j].packagePath
		}
		return mappings[i].aliasName < mappings[j].aliasName
	})
	return mappings, nil
}

func normalizedAliasTarget(typ types.Type) (*types.Named, error) {
	switch typ := typ.(type) {
	case *types.Alias:
		return normalizedAliasTarget(typ.Rhs())
	case *types.Pointer:
		return normalizedAliasTarget(typ.Elem())
	case *types.Named:
		return typ, nil
	default:
		return nil, fmt.Errorf("target %s is not a named type or pointer to a named type", typ)
	}
}

type closureEntry struct {
	packagePath string
	typeName    string
}

func reachableTypes(packages map[string]*types.Package, modulePath string, roots []rootSpec) ([]closureEntry, error) {
	collector := typeCollector{
		modulePath: modulePath,
		seenTypes:  make(map[types.Type]struct{}),
		entries:    make(map[closureEntry]struct{}),
	}
	for _, root := range roots {
		pkg, ok := packages[root.packagePath]
		if !ok {
			return nil, fmt.Errorf("root package %s was not loaded", root.packagePath)
		}
		for _, name := range root.typeNames {
			object := pkg.Scope().Lookup(name)
			typeName, ok := object.(*types.TypeName)
			if !ok {
				return nil, fmt.Errorf("root %s.%s is not a type", root.packagePath, name)
			}
			collector.visit(typeName.Type())
		}
	}

	entries := make([]closureEntry, 0, len(collector.entries))
	for entry := range collector.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].packagePath == entries[j].packagePath {
			return entries[i].typeName < entries[j].typeName
		}
		return entries[i].packagePath < entries[j].packagePath
	})
	return entries, nil
}

type typeCollector struct {
	modulePath string
	seenTypes  map[types.Type]struct{}
	entries    map[closureEntry]struct{}
}

func (c *typeCollector) visit(typ types.Type) {
	if typ == nil {
		return
	}
	if _, ok := c.seenTypes[typ]; ok {
		return
	}
	c.seenTypes[typ] = struct{}{}

	switch typ := typ.(type) {
	case *types.Alias:
		c.visit(types.Unalias(typ))
		c.visitTypeParams(typ.TypeParams())
		for i := range typ.TypeArgs().Len() {
			c.visit(typ.TypeArgs().At(i))
		}
	case *types.Named:
		for i := range typ.TypeArgs().Len() {
			c.visit(typ.TypeArgs().At(i))
		}
		if !c.addTypeName(typ.Obj()) {
			return
		}
		c.visitTypeParams(typ.TypeParams())
		c.visit(typ.Underlying())
		c.visitMethodSet(types.NewMethodSet(typ))
		c.visitMethodSet(types.NewMethodSet(types.NewPointer(typ)))
	case *types.Pointer:
		c.visit(typ.Elem())
	case *types.Array:
		c.visit(typ.Elem())
	case *types.Slice:
		c.visit(typ.Elem())
	case *types.Map:
		c.visit(typ.Key())
		c.visit(typ.Elem())
	case *types.Chan:
		c.visit(typ.Elem())
	case *types.Struct:
		for i := range typ.NumFields() {
			field := typ.Field(i)
			if field.Exported() || field.Embedded() {
				c.visit(field.Type())
			}
		}
	case *types.Signature:
		c.visitTypeParams(typ.TypeParams())
		c.visitTuple(typ.Params())
		c.visitTuple(typ.Results())
	case *types.Interface:
		typ.Complete()
		for i := range typ.NumMethods() {
			method := typ.Method(i)
			if method.Exported() {
				c.visit(method.Type())
			}
		}
		for i := range typ.NumEmbeddeds() {
			c.visit(typ.EmbeddedType(i))
		}
	case *types.TypeParam:
		c.visit(typ.Constraint())
	case *types.Union:
		for i := range typ.Len() {
			c.visit(typ.Term(i).Type())
		}
	}
}

func (c *typeCollector) addTypeName(object *types.TypeName) bool {
	if object == nil || object.Pkg() == nil || !isWithinModule(object.Pkg().Path(), c.modulePath) {
		return false
	}
	c.entries[closureEntry{packagePath: object.Pkg().Path(), typeName: object.Name()}] = struct{}{}
	return true
}

func (c *typeCollector) visitMethodSet(methods *types.MethodSet) {
	for i := range methods.Len() {
		method, ok := methods.At(i).Obj().(*types.Func)
		if ok && method.Exported() {
			c.visit(method.Type())
		}
	}
}

func (c *typeCollector) visitTypeParams(params *types.TypeParamList) {
	for i := range params.Len() {
		c.visit(params.At(i))
	}
}

func (c *typeCollector) visitTuple(tuple *types.Tuple) {
	for i := range tuple.Len() {
		c.visit(tuple.At(i).Type())
	}
}

func isWithinModule(packagePath, modulePath string) bool {
	return packagePath == modulePath || strings.HasPrefix(packagePath, modulePath+"/")
}
