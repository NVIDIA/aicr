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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// UAT coverage must reflect what the scheduled workflows actually execute, not
// which test assets merely exist on disk. The functions here read the real
// `.github/workflows/uat-*.yaml` entrypoints so a present-but-unwired UAT tree
// (e.g. the Azure stubs, or the cuj2-inference dirs no scheduled run invokes)
// is never reported as live nightly coverage.

var (
	// uatRunRef matches a scheduled workflow invoking a cloud's runner, e.g.
	// `./tests/uat/aws/run prep "${TEST_CONFIG}"`.
	uatRunRef = regexp.MustCompile(`tests/uat/([a-z0-9-]+)/run\b`)
	// uatConfigRef matches a referenced per-cloud test config, e.g.
	// `tests/uat/aws/tests/h100-training-config.yaml`.
	uatConfigRef = regexp.MustCompile(`tests/uat/([a-z0-9-]+)/tests/([a-z0-9-]+)\.ya?ml`)
)

// wiredUAT is the execution surface the scheduled UAT workflows actually drive:
// the per-cloud runner scripts and the set of intents their configs select.
type wiredUAT struct {
	runners []string        // repo-relative paths to scheduled `tests/uat/<cloud>/run` scripts
	intents map[string]bool // intents (e.g. "training") selected by wired TEST_CONFIGs
}

// scanWiredUAT reads .github/workflows/uat-*.yaml and reports the runners and
// config-derived intents that are actually scheduled. Intent is inferred from
// the config filename (e.g. h100-training-config -> "training").
func scanWiredUAT(repoRoot string) wiredUAT {
	w := wiredUAT{intents: map[string]bool{}}
	runnerSet := map[string]bool{}

	wfDir := filepath.Join(repoRoot, ".github", "workflows")
	entries, err := os.ReadDir(wfDir)
	if err != nil {
		return w
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "uat-") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(wfDir, name)) //nolint:gosec // bounded to workflows dir
		if rerr != nil {
			continue
		}
		content := string(data)
		for _, m := range uatRunRef.FindAllStringSubmatch(content, -1) {
			runnerSet[filepath.Join("tests", "uat", m[1], "run")] = true
		}
		for _, m := range uatConfigRef.FindAllStringSubmatch(content, -1) {
			if intent := intentFromConfig(m[2]); intent != "" {
				w.intents[intent] = true
			}
		}
	}
	for r := range runnerSet {
		w.runners = append(w.runners, r)
	}
	sort.Strings(w.runners)
	return w
}

// intentFromConfig maps a UAT config base name to the journey intent it selects.
// Returns "" when no known intent keyword is present.
func intentFromConfig(configBase string) string {
	for _, intent := range []string{"training", "inference"} {
		if strings.Contains(configBase, intent) {
			return intent
		}
	}
	return ""
}
