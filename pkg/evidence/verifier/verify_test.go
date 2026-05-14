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

package verifier

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stepByNumber(t *testing.T, r *VerifyResult, step int) *StepResult {
	t.Helper()
	for i := range r.Steps {
		if r.Steps[i].Step == step {
			return &r.Steps[i]
		}
	}
	t.Fatalf("step %d not recorded; got %+v", step, r.Steps)
	return nil
}

func TestVerify_DirectoryHappyPath(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Input != InputFormDir {
		t.Errorf("Input = %v, want dir", res.Input)
	}
	if got := stepByNumber(t, res, stepMaterialize).Status; got != StepPassed {
		t.Errorf("materialize = %v, want passed", got)
	}
	if got := stepByNumber(t, res, stepInventory).Status; got != StepPassed {
		t.Errorf("inventory = %v, want passed", got)
	}
	if res.Exit != ExitValidPassed {
		t.Errorf("Exit = %d, want %d", res.Exit, ExitValidPassed)
	}
	if res.Predicate == nil {
		t.Errorf("Predicate is nil; expected parsed predicate")
	}
}

func TestVerify_TamperedFileFails(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	recipePath := filepath.Join(summary, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("apiVersion: aicr.nvidia.com/v1alpha1\nkind: RecipeResult\nmaterialEdit: 1\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Exit != ExitInvalid {
		t.Errorf("Exit = %d, want %d", res.Exit, ExitInvalid)
	}
	if got := stepByNumber(t, res, stepInventory).Status; got != StepFailed {
		t.Errorf("inventory = %v, want failed", got)
	}
}

func TestVerify_StrayFileFails(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	if err := os.WriteFile(filepath.Join(summary, "stray.txt"), []byte("rogue"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Exit != ExitInvalid {
		t.Errorf("Exit = %d, want %d", res.Exit, ExitInvalid)
	}
	inv := stepByNumber(t, res, stepInventory)
	found := false
	for _, row := range inv.SubRows {
		if row.Key == "stray.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected stray.txt in inventory sub-rows; got %+v", inv.SubRows)
	}
}

func TestVerify_RendersMarkdownAndJSON(t *testing.T) {
	bundleDir := buildTestBundle(t)
	res, err := Verify(context.Background(), VerifyOptions{Input: summaryDirOf(t, bundleDir)})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	md := RenderMarkdown(res)
	if !strings.Contains(md, "Evidence verification") {
		t.Errorf("Markdown missing header; got %q", md)
	}
	if !strings.Contains(md, "Verification steps") {
		t.Errorf("Markdown missing steps section")
	}
	js, err := RenderJSON(res)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !strings.Contains(string(js), `"steps":`) {
		t.Errorf("JSON output missing steps array")
	}
}

func TestVerify_EmptyInputErrors(t *testing.T) {
	if _, err := Verify(context.Background(), VerifyOptions{}); err == nil {
		t.Errorf("expected error for empty Input")
	}
}

func TestCheckInventory_RespectsCancellation(t *testing.T) {
	bundleDir := buildTestBundle(t)
	mat := &MaterializedBundle{BundleDir: summaryDirOf(t, bundleDir)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CheckInventory(ctx, mat)
	if err == nil {
		t.Errorf("CheckInventory(canceled ctx) = nil, want error")
	}
}

func TestWriteMarkdown_WritesFile(t *testing.T) {
	bundleDir := buildTestBundle(t)
	res, err := Verify(context.Background(), VerifyOptions{Input: summaryDirOf(t, bundleDir)})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	p := filepath.Join(t.TempDir(), "summary.md")
	if writeErr := WriteMarkdown(p, res); writeErr != nil {
		t.Fatalf("WriteMarkdown: %v", writeErr)
	}
	body, readErr := os.ReadFile(p)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if !strings.Contains(string(body), "Evidence verification") {
		t.Errorf("markdown file missing header; got %q", body)
	}
}
