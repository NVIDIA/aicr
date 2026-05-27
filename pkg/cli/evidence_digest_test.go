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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestEvidenceCmd_RegistersDigestSubcommand(t *testing.T) {
	cmd := evidenceCmd()
	found := false
	for _, sub := range cmd.Commands {
		if sub.Name == "digest" {
			found = true
		}
	}
	if !found {
		t.Errorf("evidence digest subcommand not registered")
	}
}

func TestEvidenceDigestCmd_HasExpectedFlags(t *testing.T) {
	cmd := evidenceDigestCmd()
	wanted := []string{"recipe", "kubeconfig"}
	for _, name := range wanted {
		found := false
		for _, f := range cmd.Flags {
			if f.Names()[0] == name {
				found = true
			}
		}
		if !found {
			t.Errorf("missing expected flag: --%s", name)
		}
	}
}

func TestEvidenceDigestCmd_RejectsMissingRecipe(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &out
	if err := root.Run(context.Background(), []string{"aicr", "evidence", "digest"}); err == nil {
		t.Errorf("expected error when --recipe is omitted")
	}
}

func TestEvidenceDigestCmd_PrintsHexDigest(t *testing.T) {
	dir := t.TempDir()
	recipeFile := filepath.Join(dir, "recipe.yaml")
	body := "kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\n"
	if err := os.WriteFile(recipeFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	root := newRootCmd()
	var out bytes.Buffer
	root.Writer = &out

	if err := root.Run(context.Background(), []string{
		"aicr", "evidence", "digest", "-r", recipeFile,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := strings.TrimSpace(out.String())
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Errorf("output is not a hex sha256: %q", got)
	}
}

func TestEvidenceDigestCmd_DeterministicAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	recipeFile := filepath.Join(dir, "recipe.yaml")
	body := "kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\n"
	if err := os.WriteFile(recipeFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	run := func() string {
		root := newRootCmd()
		var out bytes.Buffer
		root.Writer = &out
		if err := root.Run(context.Background(), []string{
			"aicr", "evidence", "digest", "-r", recipeFile,
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return strings.TrimSpace(out.String())
	}

	first := run()
	second := run()
	if first != second {
		t.Errorf("digest is non-deterministic: %q != %q", first, second)
	}
}

func TestEvidenceDigestCmd_DiffersOnContentChange(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(a, []byte("kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\nmetadata:\n  version: v1.0.0\n"), 0o600); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(b, []byte("kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\nmetadata:\n  version: v2.0.0\n"), 0o600); err != nil {
		t.Fatalf("write b: %v", err)
	}

	run := func(path string) string {
		root := newRootCmd()
		var out bytes.Buffer
		root.Writer = &out
		if err := root.Run(context.Background(), []string{
			"aicr", "evidence", "digest", "-r", path,
		}); err != nil {
			t.Fatalf("Run %s: %v", path, err)
		}
		return strings.TrimSpace(out.String())
	}

	if run(a) == run(b) {
		t.Errorf("digest did not change when recipe content changed")
	}
}
