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

//go:build ignore

// gen.go regenerates summary-bundle/ (beside this file) for the cli-evidence-publish
// chainsaw suite. It emits an unsigned, local-only recipe-evidence bundle
// from a minimal unprofiled recipe and an empty snapshot, exactly as
// `aicr validate --emit-attestation` would, so the suite can exercise the
// standalone `aicr evidence publish` verb without a cluster.
//
// Run from the repo root whenever the bundle schema changes:
//
//	go run ./tests/chainsaw/cli/evidence-publish/testdata/gen.go
//
// It lives under testdata/ so Go tooling and the license-header check skip it.
//
// Emit stamps the current time (attestedAt, BOM serial and timestamp) into the
// bundle, so every regeneration rewrites all five files even when nothing
// else changed. Regenerate only when the bundle schema actually changes.
// Only summary-bundle/ is kept; the pointer.yaml Emit writes beside it is
// discarded because publish produces the real one at test time.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	target := filepath.Join("tests", "chainsaw", "cli", "evidence-publish", "testdata", attestation.SummaryBundleDirName)
	if _, err := os.Stat(filepath.Dir(target)); err != nil {
		return fmt.Errorf("run from the repo root: %w", err)
	}

	tmp, err := os.MkdirTemp("", "aicr-evidence-fixture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.run/v1",
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			OS:          recipe.CriteriaOSUbuntu,
			Intent:      recipe.CriteriaIntentTraining,
		},
		ComponentRefs: []recipe.ComponentRef{{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Type:      "Helm",
		}},
		DeploymentOrder: []string{"gpu-operator"},
	}

	if _, err := attestation.Emit(context.Background(), attestation.EmitOptions{
		OutDir:      tmp,
		Recipe:      rec,
		Snapshot:    &snapshotter.Snapshot{},
		AICRVersion: "v0.0.0-fixture",
	}); err != nil {
		return err
	}

	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return os.CopyFS(target, os.DirFS(filepath.Join(tmp, attestation.SummaryBundleDirName)))
}
