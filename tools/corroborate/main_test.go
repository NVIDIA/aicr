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
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// fixtureGCS is the package's evidence fixture tree, reached from tools/corroborate.
var fixtureGCS = filepath.Join("..", "..", "pkg", "corroborate", "testdata", "gcs")

func TestRunMissingInput(t *testing.T) {
	if err := run(context.Background(), "", t.TempDir(), ""); err == nil {
		t.Fatal("expected error when -in is empty")
	}
}

func TestRunBadInput(t *testing.T) {
	if err := run(context.Background(), filepath.Join(t.TempDir(), "nope"), t.TempDir(), ""); err == nil {
		t.Fatal("expected error for nonexistent input dir")
	}
}

func TestRunHappyPath(t *testing.T) {
	out := t.TempDir()
	if err := run(context.Background(), fixtureGCS, out, ""); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, rel := range []string{"index.html", filepath.Join("data", "index.json")} {
		if _, err := os.Stat(filepath.Join(out, rel)); err != nil {
			t.Errorf("expected %s: %v", rel, err)
		}
	}
	// The per-recipe drilldown series are part of the emit contract; a fixture
	// with recipes must produce at least one series/<recipe>.json.
	series, err := filepath.Glob(filepath.Join(out, "data", "series", "*.json"))
	if err != nil {
		t.Fatalf("glob series: %v", err)
	}
	if len(series) == 0 {
		t.Error("expected at least one data/series/*.json artifact, got none")
	}
}

func TestParseAndRun(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"success", []string{"-in", fixtureGCS, "-out", t.TempDir()}, 0},
		{"with allowlist", []string{
			"-in", fixtureGCS, "-out", t.TempDir(),
			"-allowlist", filepath.Join("..", "..", "pkg", "corroborate", "testdata", "allowlist.yaml"),
		}, 0},
		{"missing -in is an invalid-input run error", []string{"-out", t.TempDir()}, errors.ExitInvalidInput},
		{"unknown flag is a parse error", []string{"-nope"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseAndRun(context.Background(), tt.args, io.Discard); got != tt.want {
				t.Errorf("parseAndRun(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}
