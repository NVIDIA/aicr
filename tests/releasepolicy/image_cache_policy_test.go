// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package releasepolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// aiperfBenchDockerfile marks a build as the aiperf-bench image. Discovery
// keys off this path rather than a fixed workflow list so a workflow that
// starts building the image is covered the day it lands.
const aiperfBenchDockerfile = "validators/performance/aiperf-bench.Dockerfile"

// buildPushAction is the action whose cache-from/cache-to inputs wire a build
// to the shared GitHub Actions cache. Plain `docker build` steps (the UAT
// workflows, .github/actions/e2e, and the Makefile) import no cache at all and
// are therefore not subject to this gate.
const buildPushAction = "docker/build-push-action"

// aiperfBenchCacheWorkflows are the workflows that build the image through
// buildPushAction today. The discovery walk must find every one of them: a
// rename or a dropped build would otherwise let this gate pass vacuously.
var aiperfBenchCacheWorkflows = []string{
	"on-push.yaml",
	"on-tag.yaml",
	"vuln-scan-images.yaml",
}

// aiperfBenchCacheGate matches the conditional that yields an empty cache
// setting on the aiperf-bench matrix leg while leaving every other phase on
// type=gha. The negated condition is load-bearing: see aiperfBenchFalsyGate.
// Submatch 1 is the value the other phases still receive.
var aiperfBenchCacheGate = regexp.MustCompile(
	`^\$\{\{\s*matrix\.phase\s*!=\s*'aiperf-bench'\s*&&\s*'([^']+)'\s*\|\|\s*''\s*\}\}$`)

// aiperfBenchCacheValues constrains that retained value per key, so a gate that
// excludes aiperf-bench correctly cannot also degrade the phases it keeps.
// cache-to without mode=max is the live trap: it silently stops exporting
// intermediate layers, and nothing in the build reports the difference.
// Trailing options are allowed so a per-leg scope= can be added without
// touching this test.
//
// An absent or empty input stays legal on purpose. That means no cache at all,
// which satisfies #2086 strictly more than the gate does, and pinning it would
// fail this test on a deliberate move off the GHA cache backend.
var aiperfBenchCacheValues = map[string]*regexp.Regexp{
	"cache-from": regexp.MustCompile(`^type=gha(,|$)`),
	"cache-to":   regexp.MustCompile(`^type=gha,(.+,)?mode=max(,|$)`),
}

// aiperfBenchFalsyGate matches the reading-order spelling of the same intent,
// which is a silent no-op. GitHub's `a && b || c` yields c whenever b is
// falsy, and the empty string is falsy, so an empty true branch hands the
// cache value back to every phase including aiperf-bench. Detected separately
// from an outright missing gate because the failure is invisible: the workflow
// reads correctly and the build emits no warning.
var aiperfBenchFalsyGate = regexp.MustCompile(
	`^\$\{\{\s*matrix\.phase\s*==\s*'aiperf-bench'\s*&&\s*''\s*\|\|`)

// TestAIPerfBenchBuildsWithoutLayerCache guards the fix for #2086.
//
// The image installs an intentionally unpinned requirements.txt so each
// rebuild resolves patched transitive versions. BuildKit cannot observe that
// `pip install` reached the network, and neither the base image nor the
// COPY'd requirements.txt changes between releases, so a cache hit replays the
// previous dependency closure verbatim and the image stops picking up CVE
// fixes. Restoring cache-from/cache-to on this leg reintroduces that silently,
// with no build-time signal, which is why it is asserted rather than reviewed.
func TestAIPerfBenchBuildsWithoutLayerCache(t *testing.T) {
	workflowDir := filepath.Join(".github", "workflows")
	entries, err := os.ReadDir(filepath.Join(repositoryRoot(t), workflowDir))
	if err != nil {
		t.Fatalf("read %s: %v", workflowDir, err)
	}

	covered := make(map[string]bool, len(aiperfBenchCacheWorkflows))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		jobs, ok := loadYAML(t, filepath.Join(workflowDir, name))["jobs"].(map[string]any)
		if !ok {
			continue
		}
		for jobName, raw := range jobs {
			job, ok := raw.(map[string]any)
			if !ok || !strings.Contains(marshalYAML(t, job), aiperfBenchDockerfile) {
				continue
			}
			if assertAIPerfBenchCacheDisabled(t, name, jobName, job) {
				covered[name] = true
			}
		}
	}

	for _, want := range aiperfBenchCacheWorkflows {
		if !covered[want] {
			t.Errorf("%s has no %s job building %s, so the #2086 cache gate is unenforced there; "+
				"drop it from aiperfBenchCacheWorkflows if the build moved on purpose",
				want, buildPushAction, aiperfBenchDockerfile)
		}
	}
}

// assertAIPerfBenchCacheDisabled checks every buildPushAction step in job and
// reports whether any were found, so the caller can detect a workflow that no
// longer builds the image through the cached path.
func assertAIPerfBenchCacheDisabled(t *testing.T, workflow, jobName string, job map[string]any) bool {
	t.Helper()

	steps, ok := job["steps"].([]any)
	if !ok {
		return false
	}
	found := false
	for _, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if uses, _ := step["uses"].(string); !strings.Contains(uses, buildPushAction) {
			continue
		}
		with, ok := step["with"].(map[string]any)
		if !ok {
			continue
		}
		found = true
		for _, key := range []string{"cache-from", "cache-to"} {
			raw, present := with[key]
			if !present {
				// No cache input at all: nothing is imported, so the
				// invariant holds by construction.
				continue
			}
			// A sequence-valued input unmarshals to []any, and a bare type
			// assertion would silently yield "" and take the OK branch below,
			// letting a re-enabled cache ship green. Fail closed instead.
			text, ok := raw.(string)
			if !ok {
				t.Errorf("%s job %s: %s is %T, not a scalar this gate can read; rewrite it as a "+
					"string or teach the gate that shape before relying on it (#2086)",
					workflow, jobName, key, raw)
				continue
			}
			value := strings.TrimSpace(text)
			gated := aiperfBenchCacheGate.FindStringSubmatch(value)
			switch {
			case value == "":
			case gated != nil:
				if want := aiperfBenchCacheValues[key]; !want.MatchString(gated[1]) {
					t.Errorf("%s job %s: %s excludes aiperf-bench correctly but hands the other "+
						"phases %q, which does not match %s; a degraded cache value here is "+
						"invisible at build time (#2086)",
						workflow, jobName, key, gated[1], want)
				}
			case aiperfBenchFalsyGate.MatchString(value):
				t.Errorf("%s job %s: %s=%q looks like a gate but is a no-op; GitHub returns the "+
					"false branch whenever the true branch is falsy, and '' is falsy. Negate it: "+
					"${{ matrix.phase != 'aiperf-bench' && '<value>' || '' }} (#2086)",
					workflow, jobName, key, value)
			default:
				t.Errorf("%s job %s: %s=%q reuses the BuildKit layer cache on the aiperf-bench leg; "+
					"its pip resolution would replay a stale dependency closure (#2086)",
					workflow, jobName, key, value)
			}
		}
	}
	return found
}
