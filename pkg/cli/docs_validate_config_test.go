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
	"os"
	"path/filepath"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// docsAICRConfigBlocks returns every fenced YAML block in path whose body
// declares an AICRConfig. The fence language is not required to be "yaml": the
// docs use both "yaml" and "shell" fences, and a block is identified by its
// content so a re-tagged fence cannot silently drop it from the corpus.
func docsAICRConfigBlocks(t *testing.T, path string) []string {
	t.Helper()

	body, err := os.ReadFile(path) //nolint:gosec // fixed, in-repo docs path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var blocks []string
	var current []string
	inFence := false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "```") {
			if inFence {
				block := strings.Join(current, "\n")
				if strings.Contains(block, "kind: AICRConfig") {
					blocks = append(blocks, block)
				}
				current = nil
			}
			inFence = !inFence
			continue
		}
		if inFence {
			current = append(current, line)
		}
	}
	return blocks
}

// TestDocsValidateConfigExamplesPassOurOwnGuards loads every documented
// AICRConfig from the CLI reference and puts its spec.validate settings through
// validateFlagCombinations, the same guard `aicr validate --config` runs.
//
// The bug this catches is a documented example our own code refuses. The CLI
// reference used to set execution.skipChecks and evidence.cncf.dir in one
// config, which validateFlagCombinations rejects outright, so a reader copying
// the block got an error out of a document that promised a working config. The
// guard reads flag-or-config, so the YAML-only form is refused exactly as the
// two flags are.
//
// The gate is kept honest by the floor below: a config that carries no
// spec.validate section passes trivially, so a restructure that stopped the
// extractor from finding the validate block would leave every case vacuous.
// Counting the blocks that actually reached the guard with settings in hand,
// and failing when that count is zero, is what keeps the pass meaningful.
func TestDocsValidateConfigExamplesPassOurOwnGuards(t *testing.T) {
	reference := filepath.Join(docsRepoRoot(t), "docs", "user", "cli-reference.md")
	blocks := docsAICRConfigBlocks(t, reference)
	if len(blocks) == 0 {
		t.Fatalf("no AICRConfig blocks found in %s; the extractor is broken and this gate is inert", reference)
	}

	withValidateSettings := 0
	for i, block := range blocks {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(block), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}

		// Loaded through aicr.LoadConfig, the same entry point
		// loadFacadeConfig uses, so the corpus goes through the reader a real
		// `--config` run would use rather than a second parser.
		cfg, err := aicr.LoadConfig(t.Context(), path)
		if err != nil {
			t.Errorf("AICRConfig block %d does not load: %v\n%s", i, err, block)
			continue
		}

		opts, _, err := cfg.ValidateSettings()
		if err != nil {
			t.Errorf("AICRConfig block %d: spec.validate does not resolve: %v", i, err)
			continue
		}
		cncfOpts, err := cfg.CNCFEvidenceOptions()
		if err != nil {
			t.Errorf("AICRConfig block %d: spec.validate.evidence.cncf does not resolve: %v", i, err)
			continue
		}
		if len(opts.SkipChecks) > 0 || cncfOpts.Dir != "" || len(cncfOpts.Features) > 0 {
			withValidateSettings++
		}

		// explicitAttest is false: a config-only run sets no flag, and
		// --emit-attestation/--push are read from cmd.IsSet, never from YAML.
		if err := validateFlagCombinations(
			cncfOpts.CNCFSubmission, cncfOpts.Dir, cncfOpts.Features,
			opts.NoCluster, false, opts.SkipChecks,
		); err != nil {
			t.Errorf("AICRConfig block %d is documented but `aicr validate --config` refuses it: %v\n%s",
				i, err, block)
		}
	}

	if withValidateSettings == 0 {
		t.Fatalf("no documented AICRConfig reached the guard with any spec.validate setting "+
			"(skipChecks, evidence.cncf.dir or features); every case above passed vacuously, "+
			"so this gate proves nothing about %s", reference)
	}
}
