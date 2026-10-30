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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	mergeGatePath = "../../.github/workflows/merge-gate.yaml"
	moduleRoot    = "../.."
)

// TestMergeGateWatchesCRDInventoryInputs checks that the merge gate's crdinv
// path filter matches every Go file and embedded file in this module that the
// tool builds from, so a change to any of them runs make crd-inventory-check.
func TestMergeGateWatchesCRDInventoryInputs(t *testing.T) {
	filter := crdinvFilter(t)
	patterns := make([]*regexp.Regexp, 0, len(filter))
	for _, p := range filter {
		patterns = append(patterns, globRegexp(t, p))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-deps", "-json=Dir,GoFiles,EmbedFiles,Module", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	root, err := filepath.Abs(moduleRoot)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var pkg struct {
			Dir        string
			GoFiles    []string
			EmbedFiles []string
			Module     *struct{ Main bool }
		}
		if err := dec.Decode(&pkg); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		if pkg.Module == nil || !pkg.Module.Main {
			continue
		}
		for _, f := range slices.Concat(pkg.GoFiles, pkg.EmbedFiles) {
			rel, err := filepath.Rel(root, filepath.Join(pkg.Dir, f))
			if err != nil {
				t.Fatal(err)
			}
			rel = filepath.ToSlash(rel)
			checked++
			if !slices.ContainsFunc(patterns, func(re *regexp.Regexp) bool { return re.MatchString(rel) }) {
				t.Errorf("crd-inventory builds from %s, which the crdinv filter in %s does not match", rel,
					filepath.Base(mergeGatePath))
			}
		}
	}
	if checked == 0 {
		t.Fatal("go list found no file in this module; this guard would be vacuous")
	}
}

// crdinvFilter returns the patterns of the crdinv path filter in the merge
// gate.
func crdinvFilter(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(mergeGatePath))
	if err != nil {
		t.Fatalf("read merge gate: %v", err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				With struct {
					Filters string `yaml:"filters"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse merge gate: %v", err)
	}
	var found [][]string
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if step.With.Filters == "" {
				continue
			}
			var filters map[string][]string
			if err := yaml.Unmarshal([]byte(step.With.Filters), &filters); err != nil {
				t.Fatalf("parse paths-filter filters: %v", err)
			}
			if p, ok := filters["crdinv"]; ok {
				found = append(found, p)
			}
		}
	}
	if len(found) != 1 || len(found[0]) == 0 {
		t.Fatalf("merge gate has %d crdinv filters, want one with patterns", len(found))
	}
	return found[0]
}

// globRegexp returns the regexp for a paths-filter glob, where ** matches any
// path and * any run of characters other than /. Other glob syntax fails the
// test.
func globRegexp(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	if strings.ContainsAny(pattern, "?[]{}!") {
		t.Fatalf("crdinv pattern %q uses glob syntax this test does not support", pattern)
	}
	quoted := regexp.QuoteMeta(pattern)
	quoted = strings.ReplaceAll(quoted, `\*\*`, ".*")
	quoted = strings.ReplaceAll(quoted, `\*`, "[^/]*")
	return regexp.MustCompile("^" + quoted + "$")
}
