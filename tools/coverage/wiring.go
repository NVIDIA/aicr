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
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/uatbroker"
	"gopkg.in/yaml.v3"
)

// UAT coverage must reflect what the scheduled nightly batch actually executes,
// not which test assets merely exist on disk. Two inputs answer that, and each
// is read from the artifact that owns it:
//
//   - WHICH LANES, and for which intents — the checked-in reservation registry.
//     uat-nightly-batch.yaml (the only cron'd UAT entrypoint) enumerates the
//     registry's rows through uat-broker and dispatches one uat-run per
//     (reservation x version x intent), so a row's nightly-intents is exactly
//     what runs on real hardware every night, and an opted-out row
//     (`nightly-intents: []`) is exactly what does not.
//   - WHICH PHASES those lanes execute — the per-cloud uat-<cloud>.yaml
//     pipelines, decoded as YAML so a commented-out CUJ step reads as absent.
//
// Both replace the previous approach of regex-scraping the pipelines for a
// literal `tests/uat/<cloud>/tests/<name>.yaml` config path. Those pipelines now
// build TEST_CONFIG from workflow inputs resolved out of the registry, so no
// such literal survives to match: the path scanner saw zero wired lanes and
// silently downgraded every UAT row to uncovered (#1977). A raw-text scan is
// also blind in the other direction — it matches invocations inside comment
// blocks, reporting a disabled phase as executed coverage.

const (
	// registryRelPath is the UAT reservation registry the nightly batch reads.
	registryRelPath = "infra/uat/reservations.yaml"
	// nightlyBatchRelPath is the cron'd batch that drives every UAT lane.
	nightlyBatchRelPath = ".github/workflows/uat-nightly-batch.yaml"
	// previousNInput is the batch input setting how many stable releases below
	// main each enrolled reservation runs per night.
	previousNInput = "previous_n"
)

// previousNScheduleFallback matches the cron path's literal fallback in
// uat-nightly-batch.yaml: PREVIOUS_N: ${{ inputs.previous_n || 'N' }}. On a
// schedule event the inputs context is empty, so workflow_dispatch defaults are
// NOT applied — this || 'N' is the value the cron actually uses.
var previousNScheduleFallback = regexp.MustCompile(`inputs\.previous_n\s*\|\|\s*'([^']*)'`)

// uatLane is one nightly-enrolled cloud lane: the per-cloud runner the batch
// invokes, the recipe intents its reservations are enrolled for, and the runner
// phases its pipeline actually executes.
type uatLane struct {
	runner  string          // repo-relative `tests/uat/<cloud>/run`
	intents map[string]bool // intents enrolled on this cloud's reservations
	phases  map[string]bool // runner phases invoked by an ENABLED pipeline step
}

// wiredUAT is the execution surface the nightly UAT batch actually drives, one
// entry per enrolled cloud lane.
type wiredUAT struct {
	lanes []uatLane
}

// runners returns the enrolled per-cloud runner scripts, in stable order.
func (w wiredUAT) runners() []string {
	out := make([]string, 0, len(w.lanes))
	for _, lane := range w.lanes {
		out = append(out, lane.runner)
	}
	return out
}

// runsIntent reports whether any enrolled lane runs intent at all — i.e. that
// intent's stack is deployed and validated nightly, whether or not the journey's
// own workload step executes.
func (w wiredUAT) runsIntent(intent string) bool {
	for _, lane := range w.lanes {
		if lane.intents[intent] {
			return true
		}
	}
	return false
}

// runsJourney reports whether any enrolled lane both runs intent AND executes
// the journey's CUJ phase. A lane whose CUJ step is commented out stands the
// stack up but never exercises the journey, so it is not live journey coverage.
func (w wiredUAT) runsJourney(intent, phase string) bool {
	for _, lane := range w.lanes {
		if lane.intents[intent] && lane.phases[phase] {
			return true
		}
	}
	return false
}

// scanWiredUAT resolves the nightly-enrolled lanes: which clouds the batch
// dispatches, which intents they run, and which runner phases their pipelines
// execute.
//
// It fails closed: an unreadable registry, or an enrolled cloud with no pipeline
// on disk, is an error — never an empty result. An empty result is
// indistinguishable from "nothing is wired", which is precisely the silent
// downgrade this generator exists to prevent.
func scanWiredUAT(repoRoot string) (wiredUAT, error) {
	reg, err := uatbroker.LoadRegistryFile(filepath.Join(repoRoot, filepath.FromSlash(registryRelPath)))
	if err != nil {
		return wiredUAT{}, err
	}

	// Several reservations can share a cloud (e.g. aws-h100 and aws-gb200); they
	// ride the same pipeline and runner, so their intents union into one lane.
	intentsByCloud := map[string]map[string]bool{}
	for i := range reg.Reservations {
		res := &reg.Reservations[i]
		// An explicit `nightly-intents: []` opts the row out of the batch: it stays
		// manually dispatchable but nothing scheduled runs it, so it contributes no
		// live coverage. An absent field defaults to [training] (see the registry).
		intents := res.NightlyIntentsOrDefault()
		if len(intents) == 0 {
			continue
		}
		if intentsByCloud[res.Cloud] == nil {
			intentsByCloud[res.Cloud] = map[string]bool{}
		}
		for _, intent := range intents {
			intentsByCloud[res.Cloud][intent] = true
		}
	}

	clouds := make([]string, 0, len(intentsByCloud))
	for cloud := range intentsByCloud {
		clouds = append(clouds, cloud)
	}
	sort.Strings(clouds)

	var w wiredUAT
	for _, cloud := range clouds {
		phases, perr := scanLanePhases(repoRoot, cloud)
		if perr != nil {
			return wiredUAT{}, perr
		}
		w.lanes = append(w.lanes, uatLane{
			runner:  filepath.Join("tests", "uat", cloud, "run"),
			intents: intentsByCloud[cloud],
			phases:  phases,
		})
	}
	return w, nil
}

// cloudPipeline is the minimal shape of a per-cloud uat-<cloud>.yaml: the `run`
// script of every DECLARED step. Decoding the YAML (rather than grepping the raw
// text) is what makes a disabled phase readable: a commented-out step is absent
// from the parsed document, so it cannot be mistaken for executed coverage —
// which a text scan does, since the disabled `serve` steps sit in comment blocks
// that still contain their runner invocation verbatim.
type cloudPipeline struct {
	Jobs map[string]struct {
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// scanLanePhases reports the runner phases the cloud's UAT pipeline actually
// executes. Fails closed when the pipeline for an enrolled cloud is missing.
func scanLanePhases(repoRoot, cloud string) (map[string]bool, error) {
	relPath := filepath.Join(".github", "workflows", "uat-"+cloud+".yaml")
	if !filepath.IsLocal(relPath) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "invalid UAT cloud path: "+strconv.Quote(cloud))
	}
	rel := filepath.ToSlash(relPath)
	data, err := readBoundedFile(filepath.Join(repoRoot, relPath), rel)
	if err != nil {
		return nil, err
	}
	var wf cloudPipeline
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse "+rel, err)
	}

	// Pinned to THIS cloud's runner, so the lane reports only the phases its own
	// pipeline drives, e.g. `./tests/uat/aws/run train "${TEST_CONFIG}"` -> "train".
	// The token class spans the whole argument, digits and separators included: a
	// letters-only capture would read `run train-v2` as the phase "train" and
	// credit a journey the pipeline never ran.
	phaseRef := regexp.MustCompile(`tests/uat/` + regexp.QuoteMeta(cloud) + `/run[ \t]+([a-z][a-z0-9_-]*)`)
	phases := map[string]bool{}
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			// Line by line, cutting shell comments before matching. Decoding the
			// YAML drops a commented-out *step*, but a `run:` block is an opaque
			// scalar, so a full-line `# ...` or a trailing `... # runner ref`
			// inside one survives into step.Run and would otherwise read as an
			// executed phase.
			for line := range strings.SplitSeq(step.Run, "\n") {
				code := stripShellComment(line)
				if strings.TrimSpace(code) == "" {
					continue
				}
				if m := phaseRef.FindStringSubmatch(code); m != nil {
					phases[m[1]] = true
				}
			}
		}
	}
	if len(phases) == 0 {
		// coverage-check runs unconditionally from `make qualify`, so this error
		// blocks every local qualify until the enrollment and pipeline agree.
		// Opt the cloud out of the nightly batch with `nightly-intents: []` on
		// its reservation(s) in infra/uat/reservations.yaml.
		return nil, errors.New(errors.ErrCodeNotFound,
			rel+" declares no enabled UAT runner step, but its cloud is enrolled in the nightly batch; "+
				"opt the cloud out with nightly-intents: [] on its reservation(s) in "+registryRelPath)
	}
	return phases, nil
}

// stripShellComment returns the executable portion of a shell line, cutting at
// the first `#` that is not inside single or double quotes. Trailing comments
// like `echo done # ./tests/uat/aws/run serve ...` must not count as phases.
func stripShellComment(line string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		// Backslash escapes the next byte outside quotes and inside double
		// quotes; inside single quotes it is literal.
		case c == '\\' && i+1 < len(line) && !inSingle:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			return line[:i]
		}
	}
	return line
}

// nightlyBatchWorkflow is the minimal shape of uat-nightly-batch.yaml the
// version axis is read from. Only the dispatch inputs are decoded; the rest of
// the workflow is irrelevant here. (yaml.v3 keeps `on` as a plain string key, so
// the GitHub Actions trigger block decodes without a YAML 1.1 boolean alias.)
type nightlyBatchWorkflow struct {
	On struct {
		WorkflowDispatch struct {
			Inputs map[string]struct {
				Default string `yaml:"default"`
			} `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
	} `yaml:"on"`
}

// scanVersionAxis derives the AICR-version axis the nightly batch exercises.
// Every enrolled reservation runs tip-of-main plus the `previous_n` stable
// releases below it, one full provision/CUJ/teardown cell per version.
//
// On a schedule event the inputs context is empty and workflow_dispatch
// defaults are NOT applied — the cron uses the literal `|| 'N'` fallback on
// PREVIOUS_N. The dispatch input default is what a manual run gets when the
// operator leaves the field blank. Both literals must agree; either alone is
// not the scheduled axis.
//
// Fails closed for the same reason as scanWiredUAT: a renamed input, a moved
// workflow, or drifted literals must surface as an error, not as a silent
// collapse to "main only".
func scanVersionAxis(repoRoot string) (VersionAxis, error) {
	data, err := readBoundedFile(filepath.Join(repoRoot, filepath.FromSlash(nightlyBatchRelPath)), nightlyBatchRelPath)
	if err != nil {
		return VersionAxis{}, err
	}

	var wf nightlyBatchWorkflow
	if err = yaml.Unmarshal(data, &wf); err != nil {
		return VersionAxis{}, errors.Wrap(errors.ErrCodeInvalidRequest, "parse "+nightlyBatchRelPath, err)
	}
	input, ok := wf.On.WorkflowDispatch.Inputs[previousNInput]
	if !ok {
		return VersionAxis{}, errors.New(errors.ErrCodeNotFound,
			nightlyBatchRelPath+" declares no "+previousNInput+" input; the version axis cannot be derived")
	}
	m := previousNScheduleFallback.FindSubmatch(data)
	if m == nil {
		return VersionAxis{}, errors.New(errors.ErrCodeNotFound,
			nightlyBatchRelPath+" declares no inputs."+previousNInput+" || 'N' schedule fallback; the version axis cannot be derived")
	}
	fallback := string(m[1])
	if input.Default != fallback {
		return VersionAxis{}, errors.New(errors.ErrCodeInvalidRequest,
			nightlyBatchRelPath+" "+previousNInput+" default "+strconv.Quote(input.Default)+
				" disagrees with the schedule fallback "+strconv.Quote(fallback))
	}
	n, err := strconv.Atoi(fallback)
	if err != nil || n < 0 {
		return VersionAxis{}, errors.New(errors.ErrCodeInvalidRequest,
			nightlyBatchRelPath+" has a non-integer or negative "+previousNInput+" schedule fallback: "+strconv.Quote(fallback))
	}
	return VersionAxis{PreviousReleases: n}, nil
}

// readBoundedFile reads path under the maxScanFileBytes cap, reporting errors
// against the repo-relative name rel. Bounded rather than os.ReadFile so an
// unexpectedly huge (or symlinked) input cannot balloon the generator.
func readBoundedFile(path, rel string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // fixed repo-relative path under the repo root
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "open "+rel, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxScanFileBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read "+rel, err)
	}
	if int64(len(data)) > maxScanFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, rel+" exceeds the scan size limit")
	}
	return data, nil
}
