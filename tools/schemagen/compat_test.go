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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Breaking-change gate for the frozen artifact schemas (issue #2113, scope
// item 5's schema half, with item 6's additive allowance).
//
// The generated schemas track the Go types automatically, which is what makes a
// gate necessary: without one, deleting a field from a struct silently deletes
// it from the published contract and nothing objects. The baseline is the
// frozen shape; the generated files are the current one.
//
// The rules mirror tools/openapi-diff so the two frozen surfaces fail the same
// way. Additive change passes, and an acknowledgement that no longer matches a
// real break FAILS, because a stale entry pre-approves the break returning.

const (
	baselineDir       = schemaDir + "/baseline"
	schemaExcepts     = schemaDir + "/schema-diff-exceptions.yaml"
	breakRemovedField = "field-removed"
	breakNowRequired  = "field-became-required"
	breakTypeChanged  = "type-changed"
	breakEnumRemoved  = "enum-value-removed"
	breakEnumAdded    = "enum-restriction-added"
)

// schemaBreak is one detected incompatibility.
type schemaBreak struct {
	Rule string
	Kind string
	Path string
	Note string
}

func (b schemaBreak) String() string {
	return fmt.Sprintf("[%s] %s at %s: %s", b.Rule, b.Kind, b.Path, b.Note)
}

// acknowledgement is an intentional break recorded in the exceptions file.
type acknowledgement struct {
	Rule   string `yaml:"rule"`
	Kind   string `yaml:"kind"`
	Path   string `yaml:"path"`
	Reason string `yaml:"reason"`
}

// TestArtifactSchemasAreCompatible fails on unacknowledged breaking changes.
func TestArtifactSchemasAreCompatible(t *testing.T) {
	artifacts := Artifacts()
	if len(artifacts) == 0 {
		t.Fatal("no artifacts declared; this gate would inspect nothing")
	}

	var found []schemaBreak
	var compared int

	for _, artifact := range artifacts {
		basePath := filepath.Join(baselineDir, FileName(artifact.Kind))
		base, err := os.ReadFile(filepath.Clean(basePath))
		if err != nil {
			// A missing baseline is a new artifact kind, which is additive.
			// Silently treating a *deleted* baseline the same way would hide a
			// removal, so that is caught by the reverse check below.
			t.Logf("no baseline for %s; treating as a new artifact kind", artifact.Kind)
			continue
		}

		current, err := os.ReadFile(filepath.Clean(filepath.Join(schemaDir, FileName(artifact.Kind))))
		if err != nil {
			t.Fatalf("read generated schema for %s: %v (run `make schemas`)", artifact.Kind, err)
		}

		var baseNode, currentNode schemaNode
		if err := json.Unmarshal(base, &baseNode); err != nil {
			t.Fatalf("parse baseline %s: %v", basePath, err)
		}
		if err := json.Unmarshal(current, &currentNode); err != nil {
			t.Fatalf("parse generated %s: %v", artifact.Kind, err)
		}

		compared++
		found = append(found, compareNodes(artifact.Kind, "", &baseNode, &currentNode)...)
	}

	if compared == 0 {
		t.Fatal("compared no schemas against a baseline; the gate is inert. " +
			"Seed it with `make schema-baseline`")
	}

	acks := loadAcknowledgements(t)
	unacknowledged, stale := reconcile(found, acks)

	for _, b := range unacknowledged {
		t.Errorf("unacknowledged breaking change to the frozen artifact schema:\n  %s\n"+
			"  Resolve by making the change additive, recording it in %s, or "+
			"accepting it with `make schema-baseline`.", b, schemaExcepts)
	}
	for _, ack := range stale {
		t.Errorf("stale acknowledgement in %s: rule=%q kind=%q path=%q (%s) matches "+
			"no reported break. Remove it; left in place it pre-approves that break "+
			"returning.", schemaExcepts, ack.Rule, ack.Kind, ack.Path, ack.Reason)
	}
}

// compareNodes walks two schema nodes and reports incompatibilities.
func compareNodes(kind, path string, base, current *schemaNode) []schemaBreak {
	var breaks []schemaBreak

	if base.Type != current.Type {
		breaks = append(breaks, schemaBreak{
			Rule: breakTypeChanged, Kind: kind, Path: displayPath(path),
			Note: fmt.Sprintf("type %q became %q", base.Type, current.Type),
		})
	}

	// A value removed from an enum rejects documents that were valid before.
	for _, value := range base.Enum {
		if !contains(current.Enum, value) {
			breaks = append(breaks, schemaBreak{
				Rule: breakEnumRemoved, Kind: kind, Path: displayPath(path),
				Note: fmt.Sprintf("enum value %q removed", value),
			})
		}
	}

	// Constraining a previously free-form field is a narrowing, and the loop
	// above cannot see it: with no baseline values there is nothing to find
	// missing. Every document carrying a value outside the new set starts
	// failing, so it is a break even though nothing was removed.
	if len(base.Enum) == 0 && len(current.Enum) > 0 {
		breaks = append(breaks, schemaBreak{
			Rule: breakEnumAdded, Kind: kind, Path: displayPath(path),
			Note: fmt.Sprintf("field was unconstrained and now permits only %d value(s): %s",
				len(current.Enum), strings.Join(current.Enum, ", ")),
		})
	}

	// A newly required field rejects documents that omitted it.
	for _, name := range current.Required {
		if !contains(base.Required, name) {
			breaks = append(breaks, schemaBreak{
				Rule: breakNowRequired, Kind: kind, Path: displayPath(join(path, name)),
				Note: "became required",
			})
		}
	}

	names := make([]string, 0, len(base.Properties))
	for name := range base.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		child, still := current.Properties[name]
		if !still {
			breaks = append(breaks, schemaBreak{
				Rule: breakRemovedField, Kind: kind, Path: displayPath(join(path, name)),
				Note: "field removed from the published contract",
			})
			continue
		}
		breaks = append(breaks, compareNodes(kind, join(path, name), base.Properties[name], child)...)
	}

	if base.Items != nil && current.Items != nil {
		breaks = append(breaks, compareNodes(kind, join(path, "[]"), base.Items, current.Items)...)
	}
	return breaks
}

// reconcile splits detected breaks into unacknowledged ones and reports
// acknowledgements that matched nothing.
func reconcile(found []schemaBreak, acks []acknowledgement) (unacknowledged []schemaBreak, stale []acknowledgement) {
	used := make([]bool, len(acks))

	for _, b := range found {
		var covered bool
		for i, ack := range acks {
			if ack.Rule != b.Rule {
				continue
			}
			// An empty kind or path widens the entry deliberately, but an
			// unset field must not silently match everything by accident, so
			// both are compared explicitly.
			if ack.Kind != "" && ack.Kind != b.Kind {
				continue
			}
			if ack.Path != "" && ack.Path != b.Path {
				continue
			}
			covered = true
			used[i] = true
		}
		if !covered {
			unacknowledged = append(unacknowledged, b)
		}
	}

	for i, ack := range acks {
		if !used[i] {
			stale = append(stale, ack)
		}
	}
	return unacknowledged, stale
}

// loadAcknowledgements reads the exceptions file, failing on a malformed one
// rather than reading it as "no exceptions".
func loadAcknowledgements(t *testing.T) []acknowledgement {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(schemaExcepts))
	if err != nil {
		t.Fatalf("read %s: %v", schemaExcepts, err)
	}

	var file struct {
		Acknowledgements *[]acknowledgement `yaml:"acknowledgements"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse %s: %v", schemaExcepts, err)
	}
	if file.Acknowledgements == nil {
		t.Fatalf("%s has no 'acknowledgements' key; a malformed file must not be "+
			"read as an empty exception list", schemaExcepts)
	}

	for i, ack := range *file.Acknowledgements {
		if ack.Rule == "" {
			t.Errorf("%s entry %d has no rule", schemaExcepts, i)
		}
		if ack.Reason == "" {
			t.Errorf("%s entry %d (rule %q) has no reason; an unexplained "+
				"exception cannot be reviewed", schemaExcepts, i, ack.Rule)
		}
	}
	return *file.Acknowledgements
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func displayPath(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestSchemaBaselineCoversEveryArtifact asserts no artifact is missing a
// baseline, which would make the gate skip it entirely.
func TestSchemaBaselineCoversEveryArtifact(t *testing.T) {
	for _, artifact := range Artifacts() {
		path := filepath.Join(baselineDir, FileName(artifact.Kind))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s has no committed baseline at %s, so no breaking change to "+
				"it can be detected; run `make schema-baseline`", artifact.Kind, path)
		}
	}

	entries, err := os.ReadDir(baselineDir)
	if err != nil {
		t.Fatalf("read %s: %v", baselineDir, err)
	}
	declared := map[string]bool{}
	for _, artifact := range Artifacts() {
		declared[FileName(artifact.Kind)] = true
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".schema.json") {
			continue
		}
		if !declared[entry.Name()] {
			t.Errorf("baseline %s has no corresponding artifact; the kind was "+
				"removed, which is itself a breaking change", entry.Name())
		}
	}
}
