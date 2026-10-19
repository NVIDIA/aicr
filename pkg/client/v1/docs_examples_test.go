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

package aicr_test

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	exampleGuideRel        = "docs/integrator/go-library.md"
	exampleTableHeaderText = "| Example | Covers | Runs |"
)

var (
	exampleTableHeader    = regexp.MustCompile(`^\|\s*Example\s*\|\s*Covers\s*\|\s*Runs\s*\|$`)
	exampleTableSeparator = regexp.MustCompile(`^\|[\s:|-]+\|$`)

	// A row may name more than one example (VerifyCatalog and SignCatalog
	// share a description), and the Runs value applies to all of them.
	exampleTableFunc = regexp.MustCompile("`(Example[A-Za-z0-9_]*)`")
)

// exampleRow is one parsed row of the guide's table.
type exampleRow struct {
	Funcs    []string // every Example* named in the first column
	Runnable bool     // the Runs column
	Line     int      // 1-based line in the guide, for the failure message
}

// parseExampleTable extracts the guide's examples table. It takes the text
// rather than a path so the drift cases below can feed it broken copies.
func parseExampleTable(guide string) ([]exampleRow, error) {
	lines := strings.Split(guide, "\n")

	start := -1
	for i, line := range lines {
		if exampleTableHeader.MatchString(strings.TrimSpace(line)) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("no %q header row in the guide; the table was removed or "+
			"reformatted past recognition, and this gate is inert either way",
			exampleTableHeaderText)
	}
	if start+1 >= len(lines) || !exampleTableSeparator.MatchString(strings.TrimSpace(lines[start+1])) {
		return nil, fmt.Errorf("%s:%d: the %q header is not followed by a separator row",
			exampleGuideRel, start+1, exampleTableHeaderText)
	}

	rows := make([]exampleRow, 0, len(lines)-start)
	for i := start + 2; i < len(lines); i++ {
		text := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(text, "|") {
			break
		}

		cells := splitTableRow(text)
		if len(cells) != 3 {
			return nil, fmt.Errorf("%s:%d: expected 3 columns, found %d: %q",
				exampleGuideRel, i+1, len(cells), text)
		}

		var funcs []string
		for _, m := range exampleTableFunc.FindAllStringSubmatch(cells[0], -1) {
			funcs = append(funcs, m[1])
		}
		if len(funcs) == 0 {
			return nil, fmt.Errorf("%s:%d: the first column names no `Example…` function: %q",
				exampleGuideRel, i+1, cells[0])
		}

		var runnable bool
		switch strings.ToLower(cells[2]) {
		case "yes":
			runnable = true
		case "no":
			runnable = false
		default:
			return nil, fmt.Errorf("%s:%d: the Runs column reads %q; it must be yes or no",
				exampleGuideRel, i+1, cells[2])
		}

		rows = append(rows, exampleRow{Funcs: funcs, Runnable: runnable, Line: i + 1})
	}

	if len(rows) == 0 {
		return nil, fmt.Errorf("%s:%d: the examples table has a header but no rows",
			exampleGuideRel, start+1)
	}
	return rows, nil
}

// splitTableRow returns a Markdown row's cells, trimmed, without the empty
// fields the leading and trailing pipes produce.
func splitTableRow(row string) []string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(row), "|"), "|")
	cells := make([]string, 0, len(parts))
	for _, part := range parts {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells
}

// codeExamples maps every example in dir to whether `go test` runs it, and to
// the file declaring it. The whole package is scanned rather than
// example_test.go alone: an example added in a second file would otherwise
// never need a table row.
func codeExamples(dir string) (runnable map[string]bool, file map[string]string, err error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, nil, fmt.Errorf("glob test files in %s: %w", dir, err)
	}

	runnable = make(map[string]bool)
	file = make(map[string]string)
	fset := token.NewFileSet()

	for _, path := range paths {
		parsed, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, parseErr)
		}
		base := filepath.Base(path)

		// go/doc applies the same rules the testing framework does, so the Runs
		// column is checked against the authority rather than against a regexp
		// for "// Output:" that would disagree on a misplaced comment.
		for _, ex := range doc.Examples(parsed) {
			name := "Example" + ex.Name
			runnable[name] = ex.Output != "" || ex.EmptyOutput
			file[name] = base
		}

		// An example go/doc declines to recognize would vanish from the set and
		// silently stop needing a row, so name it instead of dropping it.
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Example") {
				continue
			}
			if _, found := runnable[fn.Name.Name]; !found {
				return nil, nil, fmt.Errorf("%s declares %s, but go/doc does not recognize it "+
					"as an example, so `go test` will not run it as one either: an example takes "+
					"no arguments, returns nothing, and has no lower-case letter directly after "+
					"the Example prefix", base, fn.Name.Name)
			}
		}
	}

	if len(runnable) == 0 {
		return nil, nil, fmt.Errorf("found no examples in %s; every assertion below would "+
			"pass vacuously", dir)
	}
	return runnable, file, nil
}

// exampleMismatches reports every disagreement between the table and the code.
// The gate and the drift cases both call it, so a pinned case cannot diverge
// from what runs over the real guide.
func exampleMismatches(rows []exampleRow, runnable map[string]bool, file map[string]string) []string {
	problems := make([]string, 0, len(rows))
	documented := make(map[string]bool, len(runnable))

	for _, row := range rows {
		for _, name := range row.Funcs {
			if documented[name] {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: %s is listed twice; one row will be missed when the example changes",
					exampleGuideRel, row.Line, name))
				continue
			}
			documented[name] = true

			runs, exists := runnable[name]
			if !exists {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: the table lists %s, but no such example exists; delete the row or "+
						"correct it to the function that does",
					exampleGuideRel, row.Line, name))
				continue
			}
			if runs == row.Runnable {
				continue
			}
			if row.Runnable {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: the table says %s runs, but it has no \"Output:\" block, so `go test` "+
						"only compiles it; set Runs to no, or give %s (%s) an Output block",
					exampleGuideRel, row.Line, name, name, file[name]))
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the table says %s does not run, but it has an \"Output:\" block, so "+
					"`go test` executes and asserts it; set Runs to yes",
				exampleGuideRel, row.Line, name))
		}
	}

	undocumented := make([]string, 0, len(runnable))
	for name := range runnable {
		if !documented[name] {
			undocumented = append(undocumented, name)
		}
	}
	sort.Strings(undocumented)

	for _, name := range undocumented {
		problems = append(problems, fmt.Sprintf(
			"%s defines %s, but no row in the %s table lists it; add one naming its coverage "+
				"and whether it runs", file[name], name, exampleGuideRel))
	}

	return problems
}

func TestGoLibraryGuideExamplesTableMatchesCode(t *testing.T) {
	t.Parallel()

	root := exampleModuleRoot(t)

	guide, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(exampleGuideRel)))
	if err != nil {
		t.Fatalf("read %s: %v", exampleGuideRel, err)
	}
	rows, err := parseExampleTable(string(guide))
	if err != nil {
		t.Fatalf("parse the examples table: %v", err)
	}

	runnable, file, err := codeExamples(filepath.Join(root, "pkg", "client", "v1"))
	if err != nil {
		t.Fatalf("collect examples: %v", err)
	}

	for _, problem := range exampleMismatches(rows, runnable, file) {
		t.Error(problem)
	}
	t.Logf("checked %d examples against %d rows in %s", len(runnable), len(rows), exampleGuideRel)
}

// The corpus is all-valid by construction, since it is the committed guide and
// it passes, so a run over it cannot show that drift would be caught. These
// cases mutate the real guide in memory instead of using a fixture that could
// go stale.
func TestExamplesTableGateDetectsDrift(t *testing.T) {
	t.Parallel()

	root := exampleModuleRoot(t)

	guide, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(exampleGuideRel)))
	if err != nil {
		t.Fatalf("read %s: %v", exampleGuideRel, err)
	}
	runnable, file, err := codeExamples(filepath.Join(root, "pkg", "client", "v1"))
	if err != nil {
		t.Fatalf("collect examples: %v", err)
	}

	tests := []struct {
		name string
		// mutate rewrites the guide; nil leaves it untouched.
		mutate func(t *testing.T, guide string) string
		// wantNamed is the identifier the failure must name. Empty expects no
		// failure at all.
		wantNamed string
	}{
		{
			name:   "the committed guide agrees with the code",
			mutate: nil,
		},
		{
			name: "a row is deleted",
			mutate: func(t *testing.T, guide string) string {
				return editExampleRow(t, guide, "ExampleClient_LoadRecipe", func(string) string { return "" })
			},
			wantNamed: "ExampleClient_LoadRecipe",
		},
		{
			name: "a row names a function that does not exist",
			mutate: func(t *testing.T, guide string) string {
				return editExampleRow(t, guide, "ExampleClient_LoadRecipe", func(row string) string {
					return strings.ReplaceAll(row, "ExampleClient_LoadRecipe", "ExampleClient_NoSuchFlow")
				})
			},
			wantNamed: "ExampleClient_NoSuchFlow",
		},
		{
			name: "a runnable example is marked as not running",
			mutate: func(t *testing.T, guide string) string {
				return editExampleRow(t, guide, "Example_errorCodes", func(row string) string {
					return strings.TrimSuffix(row, "| yes |") + "| no |"
				})
			},
			wantNamed: "Example_errorCodes",
		},
		{
			name: "a compile-only example is marked as running",
			mutate: func(t *testing.T, guide string) string {
				return editExampleRow(t, guide, "ExampleClient_ValidateState", func(row string) string {
					return strings.TrimSuffix(row, "| no |") + "| yes |"
				})
			},
			wantNamed: "ExampleClient_ValidateState",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			text := string(guide)
			if tt.mutate != nil {
				text = tt.mutate(t, text)
			}

			rows, parseErr := parseExampleTable(text)
			if parseErr != nil {
				t.Fatalf("parse the mutated table: %v", parseErr)
			}
			problems := exampleMismatches(rows, runnable, file)

			if tt.wantNamed == "" {
				if len(problems) > 0 {
					t.Errorf("expected no findings, got:\n%s", strings.Join(problems, "\n"))
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("the gate reported nothing; it does not detect %s", tt.name)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantNamed) {
				t.Errorf("no finding names %s; got:\n%s", tt.wantNamed, strings.Join(problems, "\n"))
			}
		})
	}
}

// editExampleRow rewrites the single table row naming fn, dropping the line
// when edit returns "". It locates the row by re-parsing rather than by
// searching for the identifier, which the guide also names in prose, and fails
// when the edit changes nothing. Either way the case would otherwise assert
// against an unmodified guide.
func editExampleRow(t *testing.T, guide, fn string, edit func(row string) string) string {
	t.Helper()

	rows, err := parseExampleTable(guide)
	if err != nil {
		t.Fatalf("parse the examples table: %v", err)
	}

	target := 0
	for _, row := range rows {
		for _, name := range row.Funcs {
			if name != fn {
				continue
			}
			if target != 0 {
				t.Fatalf("%s is named by rows %d and %d; the mutation is ambiguous",
					fn, target, row.Line)
			}
			target = row.Line
		}
	}
	if target == 0 {
		t.Fatalf("no table row names %s", fn)
	}

	lines := strings.Split(guide, "\n")
	edited := edit(lines[target-1])
	if edited == lines[target-1] {
		t.Fatalf("the edit left row %d unchanged: %q", target, lines[target-1])
	}
	if edited == "" {
		return strings.Join(append(lines[:target-1:target-1], lines[target:]...), "\n")
	}
	lines[target-1] = edited
	return strings.Join(lines, "\n")
}

// exampleModuleRoot walks up to the module root, so the guide is found
// regardless of where `go test` was invoked.
func exampleModuleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod walking up from the test working directory")
		}
		dir = parent
	}
}
