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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const reverifyWorkflow = ".github/workflows/release-reverify.yaml"

// reverifySBOMFloor mirrors the workflow's SBOM_SIGNING_FLOOR env value and is
// asserted against it, so raising the floor has to move this constant and with
// it the below/above-floor cases below.
const reverifySBOMFloor = "v0.18.0"

// TestReleaseReverifyWorkflowShape locks the structural contract of the daily
// re-verification workflow: how it is triggered, what it is allowed to do, that
// it reuses the hardened install-aicr-release composite rather than
// reimplementing binary verification, and that its two notification gates
// partition every failure with the security side as a strict allowlist.
func TestReleaseReverifyWorkflowShape(t *testing.T) {
	doc := loadYAML(t, reverifyWorkflow)

	triggers := mapValue(t, doc, "on")
	schedule := sliceValue(t, triggers, "schedule")
	if len(schedule) != 1 {
		t.Fatalf("re-verification must run on exactly one cron, got %d", len(schedule))
	}
	cron := fmt.Sprint(schedule[0].(map[string]any)["cron"])
	if fields := strings.Fields(cron); len(fields) != 5 || fields[2] != "*" || fields[4] != "*" {
		t.Errorf("cron %q must be a daily schedule", cron)
	}
	// GitHub disables scheduled workflows after 60 days of repository
	// inactivity; workflow_dispatch is how a maintainer re-arms it.
	if _, ok := triggers["workflow_dispatch"]; !ok {
		t.Error("re-verification must be dispatchable so the schedule can be re-armed on demand")
	}

	if permissions, ok := doc["permissions"].(map[string]any); !ok || len(permissions) != 0 {
		t.Errorf("workflow permissions = %v, want an empty fail-closed default", doc["permissions"])
	}
	assertConcurrency(t, doc, "release-reverify")

	job := mapValue(t, mapValue(t, doc, "jobs"), "reverify")
	assertPermissions(t, job, map[string]string{
		"contents": "read",
		"actions":  "read",
		"issues":   "write",
	})
	if _, ok := job["timeout-minutes"]; !ok {
		t.Error("the re-verification job must have an explicit timeout")
	}

	env := mapValue(t, doc, "env")
	if got := fmt.Sprint(env["SBOM_SIGNING_FLOOR"]); got != reverifySBOMFloor {
		t.Fatalf("SBOM_SIGNING_FLOOR = %q, want %q", got, reverifySBOMFloor)
	}

	steps := sliceValue(t, job, "steps")

	resolve := stepIndex(steps, "Resolve the latest release")
	if resolve < 0 {
		t.Fatal("re-verification must resolve the latest release rather than hardcode a tag")
	}
	resolveScript := stringValue(t, steps[resolve].(map[string]any), "run")
	for _, required := range []string{
		"gh-api-retry.sh",            // the shared bounded-retry helper, not a bare gh api
		"releases/latest",            // resolved, never hardcoded
		"GITHUB_STEP_SUMMARY",        // the resolved tag is visible in the job output
		"tag=${tag}",                 // and exported for the verification steps
		"^v[0-9]+\\.[0-9]+\\.[0-9]+", // validated before it reaches a regexp
	} {
		if !strings.Contains(resolveScript, required) {
			t.Errorf("release resolution missing %q", required)
		}
	}

	install := stepIndex(steps, "Verify the released aicr binary provenance")
	if install < 0 {
		t.Fatal("re-verification must verify the released binary's provenance")
	}
	installStep := steps[install].(map[string]any)
	if got := fmt.Sprint(installStep["uses"]); got != "./.github/actions/install-aicr-release" {
		t.Errorf("binary verification uses %q, want the hardened install-aicr-release composite", got)
	}
	if enabled, ok := installStep["continue-on-error"].(bool); !ok || !enabled {
		t.Error("the install step must hand its outcome to the classifier instead of ending the job unclassified")
	}
	with := mapValue(t, installStep, "with")
	if got := fmt.Sprint(with["aicr-version"]); got != "${{ steps.release.outputs.tag }}" {
		t.Errorf("install aicr-version = %q, want the resolved tag", got)
	}
	if got := fmt.Sprint(with["cosign_version"]); got != "${{ steps.versions.outputs.cosign }}" {
		t.Errorf("install cosign_version = %q, want the pinned .settings.yaml version", got)
	}

	verify := stepIndex(steps, "Re-verify release artifacts and classify")
	if verify < 0 {
		t.Fatal("re-verification must classify its outcome")
	}
	verifyScript := stringValue(t, steps[verify].(map[string]any), "run")
	for _, required := range []string{
		"CLASSIFICATION=${classification}", // same machine-readable line as tools/rekor-monitor
		"classification=${classification}", // and the step output the gates branch on
		"timeout --foreground",             // every network call is bounded
		"recipe verify-catalog",            // the shipped catalog verification command
		"cosign verify-blob-attestation",   // the shipped blob attestation command
		"sigstore_reachable",               // guard #1
		"infra_shaped",                     // guard #2
	} {
		if !strings.Contains(verifyScript, required) {
			t.Errorf("classifier missing %q", required)
		}
	}
	// Exit codes mirror tools/rekor-monitor: 0 clean, 1 security, 3 operational.
	for _, mapping := range []string{"clean) exit 0", "tamper) exit 1", "*) exit 3"} {
		if !strings.Contains(verifyScript, mapping) {
			t.Errorf("classifier missing the rekor-monitor exit mapping %q", mapping)
		}
	}

	alert := stepIndex(steps, "Open security alert issue")
	degraded := stepIndex(steps, "Open degraded issue if a non-security failure is persistent")
	closer := stepIndex(steps, "Close open issues on success")
	if alert < 0 || degraded < 0 || closer < 0 {
		t.Fatal("re-verification must open, de-duplicate, and auto-close both issue kinds")
	}
	alertGate := fmt.Sprint(steps[alert].(map[string]any)["if"])
	degradedGate := fmt.Sprint(steps[degraded].(map[string]any)["if"])
	// The security gate is a strict allowlist on one value and the operational
	// gate is its exact inverse, so an EMPTY classification (a step before the
	// classifier failed) can only ever land on the operational side.
	if !strings.Contains(alertGate, "steps.verify.outputs.classification == 'tamper'") {
		t.Errorf("security gate = %q, want an allowlist on the tamper classification", alertGate)
	}
	if !strings.Contains(degradedGate, "steps.verify.outputs.classification != 'tamper'") {
		t.Errorf("operational gate = %q, want the exact inverse of the security gate", degradedGate)
	}
	alertText := marshalYAML(t, steps[alert])
	if !strings.Contains(alertText, "select(.title == $t)") {
		t.Error("the security issue must de-duplicate on an exact title match")
	}
	degradedText := marshalYAML(t, steps[degraded])
	if !strings.Contains(degradedText, `index("success") // length`) {
		t.Error("the degraded issue must escalate on a consecutive-failure streak, not a failure count")
	}
	if !strings.Contains(degradedText, "workflows/release-reverify.yaml/runs") {
		t.Error("the streak query must read this workflow's own run history")
	}

	// Repo convention: no ${{ }} interpolation inside run: blocks. Every value a
	// script consumes arrives through env, so a tag or title can never be
	// spliced into shell source.
	for _, raw := range steps {
		step := raw.(map[string]any)
		script, ok := step["run"].(string)
		if !ok {
			continue
		}
		if strings.Contains(script, "${{") {
			t.Errorf("step %v interpolates an expression inside run:; use env indirection", step["name"])
		}
	}
}

// TestReleaseReverifyClassification is the behavioral half: it extracts the
// classifier and runs it under bash against fake gh/cosign/curl/aicr binaries.
//
// The property under test is asymmetric and is the whole point of the workflow.
// A *missing* artifact must be a security finding; Sigstore, GitHub-API and
// network trouble must be operational and must never look like a missing entry.
// Every case here is one side of that line.
func TestReleaseReverifyClassification(t *testing.T) {
	script := reverifyClassifierScript(t)

	aboveFloor := "v0.19.0"
	assetsFor := func(version string, extra ...string) []string {
		base := make([]string, 0, 3+len(extra))
		base = append(base,
			"aicr_"+version+"_linux_amd64.tar.gz",
			"aicr_checksums.txt",
			"recipe-catalog.sigstore.json",
		)
		return append(base, extra...)
	}
	signedSBOMAssets := func(version string) []string {
		return assetsFor(version,
			"aicr_"+version+"_linux_amd64.sbom.json",
			"aicr_"+version+"_linux_amd64.sbom.json.sigstore.json",
		)
	}

	tests := []struct {
		name string
		opts reverifyOptions
		want string
		code int
	}{
		{
			name: "a complete release verifies clean",
			opts: reverifyOptions{
				tag:            reverifySBOMFloor,
				assets:         assetsFor(strings.TrimPrefix(reverifySBOMFloor, "v")),
				installOutcome: "success",
			},
			want: "clean",
			code: 0,
		},
		{
			name: "a release above the floor with signed SBOMs verifies clean",
			opts: reverifyOptions{
				tag:            aboveFloor,
				assets:         signedSBOMAssets(strings.TrimPrefix(aboveFloor, "v")),
				installOutcome: "success",
			},
			want: "clean",
			code: 0,
		},

		// --- security: positive evidence that a published artifact is absent ---
		{
			name: "a missing catalog signature is a security finding",
			opts: reverifyOptions{
				tag: reverifySBOMFloor,
				assets: []string{
					"aicr_0.18.0_linux_amd64.tar.gz",
					"aicr_checksums.txt",
				},
				installOutcome: "success",
			},
			want: "tamper",
			code: 1,
		},
		{
			name: "a missing binary archive is a security finding",
			opts: reverifyOptions{
				tag:            reverifySBOMFloor,
				assets:         []string{"aicr_checksums.txt", "recipe-catalog.sigstore.json"},
				installOutcome: "failure",
				noArchiveFile:  true,
			},
			want: "tamper",
			code: 1,
		},
		{
			name: "an archive shipped without its attestation bundle is a security finding",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "failure",
				omitAttestation:   true,
				sigstoreReachable: true,
			},
			want: "tamper",
			code: 1,
		},
		{
			name: "an SBOM published without its attestation bundle is a security finding",
			opts: reverifyOptions{
				tag:            aboveFloor,
				assets:         assetsFor("0.19.0", "aicr_0.19.0_linux_amd64.sbom.json"),
				installOutcome: "success",
			},
			want: "tamper",
			code: 1,
		},
		{
			name: "a release above the floor with no SBOM assets at all is a security finding",
			opts: reverifyOptions{
				tag:            aboveFloor,
				assets:         assetsFor("0.19.0"),
				installOutcome: "success",
			},
			want: "tamper",
			code: 1,
		},
		{
			name: "a signature that fails against a reachable Sigstore is a security finding",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "failure",
				cosignRC:          1,
				cosignMessage:     "Error: no matching signatures found for the given identity",
				sigstoreReachable: true,
			},
			want: "tamper",
			code: 1,
		},
		{
			name: "a catalog digest mismatch is a security finding",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "success",
				aicrRC:            1,
				aicrMessage:       "Error: recomputed catalog digest does not match the signed subject",
				sigstoreReachable: true,
			},
			want: "tamper",
			code: 1,
		},

		// --- operational: infrastructure must never look like a missing entry ---
		{
			name: "an unreachable Sigstore demotes a failed verification to operational",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "failure",
				cosignRC:          1,
				cosignMessage:     "Error: no matching signatures found for the given identity",
				sigstoreReachable: false,
			},
			want: "operational",
			code: 3,
		},
		{
			name: "a Rekor outage in the failure output demotes to operational",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "failure",
				cosignRC:          1,
				cosignMessage:     "Error: uploading to rekor: POST https://rekor.sigstore.dev: status 503 service unavailable",
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			name: "a TUF trusted-root fetch failure demotes to operational",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "failure",
				cosignRC:          1,
				cosignMessage:     "Error: initializing trusted root: could not fetch metadata",
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			name: "a network timeout during catalog verification is operational",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "success",
				aicrRC:            1,
				aicrMessage:       "Error: verifying catalog: dial tcp 34.1.2.3:443: i/o timeout",
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			name: "a failed release-asset download is operational",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "success",
				ghRC:              1,
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			name: "a corrupt archive download is operational, not a missing attestation",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "failure",
				corruptArchive:    true,
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			name: "a transient install failure that re-verifies is operational",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            assetsFor("0.18.0"),
				installOutcome:    "failure",
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},

		// --- precedence: a real finding is never masked by a concurrent outage ---
		{
			name: "a missing asset still pages when the network is also down",
			opts: reverifyOptions{
				tag:               reverifySBOMFloor,
				assets:            []string{"aicr_0.18.0_linux_amd64.tar.gz", "aicr_checksums.txt"},
				installOutcome:    "failure",
				noArchiveFile:     true,
				ghRC:              1,
				sigstoreReachable: false,
			},
			want: "tamper",
			code: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, code, output := runReverifyClassifier(t, script, tc.opts)
			if got != tc.want || code != tc.code {
				t.Fatalf("classification = %q (exit %d), want %q (exit %d)\n%s", got, code, tc.want, tc.code, output)
			}
			if !strings.Contains(output, "CLASSIFICATION="+tc.want) {
				t.Errorf("classifier did not print the machine-readable CLASSIFICATION line\n%s", output)
			}
		})
	}
}

// TestReleaseReverifyGuardsAreLoadBearing proves the classification logic is
// what produces the operational verdicts above, rather than the fakes happening
// to agree with the expectations. Each mutation removes exactly one guard from
// the extracted script and asserts that an infrastructure failure then
// misclassifies as a security finding — which is the failure mode this workflow
// exists to prevent, so the tests must not pass without the guards.
func TestReleaseReverifyGuardsAreLoadBearing(t *testing.T) {
	script := reverifyClassifierScript(t)

	tests := []struct {
		name string
		from string
		to   string
		opts reverifyOptions
	}{
		{
			name: "removing the Sigstore reachability guard",
			from: "if ! sigstore_reachable; then",
			to:   "if false; then",
			opts: reverifyOptions{
				installOutcome: "failure",
				cosignRC:       1,
				cosignMessage:  "Error: no matching signatures found for the given identity",
				// Sigstore is DOWN, so the intact script must say operational.
				sigstoreReachable: false,
			},
		},
		{
			name: "removing the infrastructure-signature guard",
			from: `if infra_shaped "$2"; then`,
			to:   "if false; then",
			opts: reverifyOptions{
				installOutcome: "failure",
				cosignRC:       1,
				// A textbook upstream outage, so the intact script must say operational.
				cosignMessage:     "Error: fetching bundle: status 503 service unavailable",
				sigstoreReachable: true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.tag = reverifySBOMFloor
			opts.assets = []string{
				"aicr_0.18.0_linux_amd64.tar.gz",
				"aicr_checksums.txt",
				"recipe-catalog.sigstore.json",
			}

			intact, _, output := runReverifyClassifier(t, script, opts)
			if intact != "operational" {
				t.Fatalf("intact classifier = %q, want operational for an infrastructure failure\n%s", intact, output)
			}

			if !strings.Contains(script, tc.from) {
				t.Fatalf("mutation target %q is no longer in the classifier", tc.from)
			}
			mutated := strings.Replace(script, tc.from, tc.to, 1)
			broken, _, brokenOutput := runReverifyClassifier(t, mutated, opts)
			if broken != "tamper" {
				t.Fatalf("classifier without %s = %q, want tamper — the guard is not load-bearing and this test proves nothing\n%s",
					tc.name, broken, brokenOutput)
			}
		})
	}
}

// TestReleaseReverifySBOMLoopIsolatesChildStdin is the regression test for the
// SBOM loop's `< /dev/null` redirects.
//
// The loop reads its subject list from a here-string, so every child it spawns
// inherits that same stdin. A child that consumed stdin would swallow the
// remaining SBOM names and end the loop early — and because the skipped
// subjects are never examined, the run would report `clean` with no finding and
// no warning. That is precisely the "silently verifies nothing" outcome this
// whole workflow exists to catch, so it is the one bug class the job must never
// have. Neither `gh` nor `cosign` reads stdin today; the redirects make the loop
// independent of that, and this test keeps them.
//
// Three subjects, not two: with two, an early-terminating loop and a loop that
// merely dropped the last entry are indistinguishable. Three makes the skipped
// set unambiguous, and the assertions name the exact subjects rather than
// counting them.
func TestReleaseReverifySBOMLoopIsolatesChildStdin(t *testing.T) {
	script := reverifyClassifierScript(t)

	// Pin the redirect count so a change in form (a different redirection, or a
	// third stdin-reading child added to the loop) has to revisit this test
	// rather than silently neutering it.
	const redirect = " < /dev/null"
	if got := strings.Count(script, redirect); got != 2 {
		t.Fatalf("classifier has %d %q redirects, want 2 (gh release download and cosign inside the SBOM loop)", got, redirect)
	}
	mutated := strings.ReplaceAll(script, redirect, "")

	// A release above the SBOM signing floor, so the loop actually runs.
	const version = "0.19.0"
	// aicr and aicrd are the two binaries GoReleaser builds today; the third is
	// a stand-in for any future binary, since the loop is generic over whatever
	// SBOM assets the release publishes.
	subjects := []string{
		"aicr_" + version + "_linux_amd64.sbom.json",
		"aicrd_" + version + "_linux_amd64.sbom.json",
		"aicr-gate_" + version + "_linux_amd64.sbom.json",
	}
	assets := make([]string, 0, 3+2*len(subjects))
	assets = append(assets,
		"aicr_"+version+"_linux_amd64.tar.gz",
		"aicr_checksums.txt",
		"recipe-catalog.sigstore.json",
	)
	for _, subject := range subjects {
		assets = append(assets, subject, subject+".sigstore.json")
	}

	tests := []struct {
		name             string
		drainGHStdin     bool
		drainCosignStdin bool
	}{
		{name: "a stdin-consuming gh release download", drainGHStdin: true},
		{name: "a stdin-consuming cosign verify-blob-attestation", drainCosignStdin: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := reverifyOptions{
				tag:               "v" + version,
				assets:            assets,
				installOutcome:    "success",
				sigstoreReachable: true,
				drainGHStdin:      tc.drainGHStdin,
				drainCosignStdin:  tc.drainCosignStdin,
			}

			class, code, output := runReverifyClassifier(t, script, opts)
			if class != "clean" || code != 0 {
				t.Fatalf("intact classifier = %q (exit %d), want clean (exit 0)\n%s", class, code, output)
			}
			if got := verifiedSBOMs(output); !slices.Equal(got, subjects) {
				t.Fatalf("intact classifier verified %v, want every subject %v\n%s", got, subjects, output)
			}

			// Strip the redirects and the stdin-consuming child eats the rest of
			// the loop's input.
			brokenClass, brokenCode, brokenOutput := runReverifyClassifier(t, mutated, opts)
			broken := verifiedSBOMs(brokenOutput)
			if slices.Equal(broken, subjects) {
				t.Fatalf("without the stdin redirects the loop still verified every subject; the redirects are not load-bearing and this test proves nothing\n%s", brokenOutput)
			}
			if !slices.Equal(broken, subjects[:1]) {
				t.Errorf("without the stdin redirects the loop verified %v, want only the first subject %v", broken, subjects[:1])
			}
			// The damning part: the skipped subjects produce no finding and no
			// warning, so the run still reports a clean verification.
			if brokenClass != "clean" || brokenCode != 0 {
				t.Errorf("mutated classifier = %q (exit %d); the skip is expected to be SILENT (clean/0), which is why it needs a test",
					brokenClass, brokenCode)
			}
			for _, skipped := range subjects[1:] {
				if strings.Contains(brokenOutput, "SBOM attestation verified: "+skipped) {
					t.Errorf("%s was expected to be silently skipped by the mutated loop", skipped)
				}
			}
		})
	}
}

// verifiedSBOMs extracts, in order, the SBOM subjects the classifier reported as
// verified. The loop emits exactly one such line per subject it processes, so
// the returned set is the set it actually examined.
func verifiedSBOMs(output string) []string {
	const marker = "SBOM attestation verified: "
	var verified []string
	for _, line := range strings.Split(output, "\n") {
		if subject, found := strings.CutPrefix(strings.TrimSpace(line), marker); found {
			verified = append(verified, subject)
		}
	}
	return verified
}

// reverifyOptions describes one simulated release and the behavior of the
// binaries the classifier shells out to.
type reverifyOptions struct {
	tag    string
	assets []string
	// installOutcome is what the install-aicr-release composite reported.
	installOutcome string
	// noArchiveFile omits the downloaded archive entirely (a failed download).
	noArchiveFile bool
	// omitAttestation ships an archive with no aicr-attestation.sigstore.json.
	omitAttestation bool
	// corruptArchive stages bytes that are not a tarball (a truncated download).
	corruptArchive bool

	cosignRC          int
	cosignMessage     string
	aicrRC            int
	aicrMessage       string
	ghRC              int
	sigstoreReachable bool

	// drainGHStdin / drainCosignStdin make the fake drain its standard input,
	// modeling a child that reads stdin. Neither real binary does today, which
	// is exactly why the SBOM loop's `< /dev/null` redirects need a regression
	// test: a future version that did would silently eat the remaining SBOM
	// names off the loop's here-string.
	drainGHStdin     bool
	drainCosignStdin bool
}

// reverifyClassifierScript extracts the classifying step's shell from the
// workflow so the test executes the shipped source, not a copy of it.
func reverifyClassifierScript(t *testing.T) string {
	t.Helper()
	doc := loadYAML(t, reverifyWorkflow)
	job := mapValue(t, mapValue(t, doc, "jobs"), "reverify")
	steps := sliceValue(t, job, "steps")
	index := stepIndex(steps, "Re-verify release artifacts and classify")
	if index < 0 {
		t.Fatal("re-verification must have a classifying step")
	}
	return stringValue(t, steps[index].(map[string]any), "run")
}

// runReverifyClassifier stages a fake release plus fake gh/cosign/curl/aicr
// binaries, runs the extracted classifier, and returns the classification it
// published, its exit status, and the combined output.
func runReverifyClassifier(t *testing.T, script string, opts reverifyOptions) (string, int, string) {
	t.Helper()

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	relDir := filepath.Join(root, "_rel")
	workDir := filepath.Join(root, "_reverify")
	for _, dir := range []string{bin, relDir, workDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	writeExecutable(t, filepath.Join(bin, "timeout"), passthroughTimeout)
	writeExecutable(t, filepath.Join(bin, "sleep"), reverifyFakeSleep)
	writeExecutable(t, filepath.Join(bin, "curl"), reverifyFakeCurl)
	writeExecutable(t, filepath.Join(bin, "cosign"), reverifyFakeCosign)
	writeExecutable(t, filepath.Join(bin, "gh"), reverifyFakeGH)
	aicrBin := filepath.Join(bin, "aicr")
	writeExecutable(t, aicrBin, reverifyFakeAicr)

	assetsFile := filepath.Join(root, "release-assets.txt")
	contents := ""
	if len(opts.assets) > 0 {
		contents = strings.Join(opts.assets, "\n") + "\n"
	}
	if err := os.WriteFile(assetsFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("write asset inventory: %v", err)
	}

	archiveName := "aicr_" + strings.TrimPrefix(opts.tag, "v") + "_linux_amd64.tar.gz"
	switch {
	case opts.noArchiveFile:
	case opts.corruptArchive:
		if err := os.WriteFile(filepath.Join(relDir, archiveName), []byte("not a tarball"), 0o600); err != nil {
			t.Fatalf("stage corrupt archive: %v", err)
		}
	default:
		stageReleaseArchive(t, relDir, archiveName, !opts.omitAttestation)
	}

	outputs := filepath.Join(root, "outputs")
	summary := filepath.Join(root, "summary.md")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "-c", script)
	command.Dir = root
	command.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"REPO=NVIDIA/aicr",
		"TAG="+opts.tag,
		"RELEASE_ASSETS="+assetsFile,
		"INSTALL_OUTCOME="+opts.installOutcome,
		"REL_DIR="+relDir,
		"WORK_DIR="+workDir,
		"AICR_BIN="+aicrBin,
		"SBOM_SIGNING_FLOOR="+reverifySBOMFloor,
		"SIGSTORE_PROBE_URL=https://tuf-repo-cdn.example.invalid/1.root.json",
		"GITHUB_OUTPUT="+outputs,
		"GITHUB_STEP_SUMMARY="+summary,
		"GH_TOKEN=fake",
		fmt.Sprintf("FAKE_SIGSTORE_REACHABLE=%t", opts.sigstoreReachable),
		fmt.Sprintf("FAKE_COSIGN_RC=%d", opts.cosignRC),
		"FAKE_COSIGN_MESSAGE="+opts.cosignMessage,
		fmt.Sprintf("FAKE_AICR_RC=%d", opts.aicrRC),
		"FAKE_AICR_MESSAGE="+opts.aicrMessage,
		fmt.Sprintf("FAKE_GH_RC=%d", opts.ghRC),
		fmt.Sprintf("FAKE_GH_DRAIN_STDIN=%t", opts.drainGHStdin),
		fmt.Sprintf("FAKE_COSIGN_DRAIN_STDIN=%t", opts.drainCosignStdin),
	)
	combined, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("classifier exceeded the test deadline: %v\n%s", ctx.Err(), combined)
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run classifier: %v\n%s", err, combined)
		}
		code = exit.ExitCode()
	}

	published := ""
	if data, readErr := os.ReadFile(outputs); readErr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if rest, found := strings.CutPrefix(line, "classification="); found {
				published = rest
			}
		}
	} else if !os.IsNotExist(readErr) {
		t.Fatalf("read step outputs: %v", readErr)
	}
	return published, code, string(combined)
}

// stageReleaseArchive builds a real gzipped tarball shaped like a released aicr
// archive, so the classifier's `tar -tzf` presence check runs against real tar
// output rather than a stub.
func stageReleaseArchive(t *testing.T, dir, name string, withAttestation bool) {
	t.Helper()
	staging := t.TempDir()
	members := []string{"aicr"}
	if err := os.WriteFile(filepath.Join(staging, "aicr"), []byte("#!/bin/true\n"), 0o700); err != nil {
		t.Fatalf("stage binary: %v", err)
	}
	if withAttestation {
		members = append(members, "aicr-attestation.sigstore.json")
		if err := os.WriteFile(filepath.Join(staging, "aicr-attestation.sigstore.json"), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("stage attestation: %v", err)
		}
	}
	arguments := append([]string{"-czf", filepath.Join(dir, name), "-C", staging}, members...)
	if output, err := exec.Command("tar", arguments...).CombinedOutput(); err != nil {
		t.Fatalf("build release archive: %v\n%s", err, output)
	}
}

// reverifyFakeSleep keeps the reachability retry loop's backoff off the test clock. The
// bounds themselves are asserted against the step's source text.
const reverifyFakeSleep = `#!/usr/bin/env bash
exit 0
`

// reverifyFakeCurl stands in for the curl liveness probe against Sigstore's
// TUF CDN, reproducing curl's exit 7 when the host does not answer.
const reverifyFakeCurl = `#!/usr/bin/env bash
set -euo pipefail
if [[ "${FAKE_SIGSTORE_REACHABLE:-true}" == "true" ]]; then
  exit 0
fi
echo "curl: (7) Failed to connect to tuf-repo-cdn.sigstore.dev port 443" >&2
exit 7
`

// reverifyFakeCosign answers verify-blob-attestation from FAKE_COSIGN_RC, emitting the
// caller-supplied failure text so the infrastructure-signature guard has real
// cosign-shaped output to read.
const reverifyFakeCosign = `#!/usr/bin/env bash
set -euo pipefail
# Model a child that reads stdin. Real cosign does not, but the SBOM loop must
# not depend on that: without its ` + "`< /dev/null`" + ` redirect this drain would consume
# the loop's remaining here-string and silently skip every later SBOM.
if [[ "${FAKE_COSIGN_DRAIN_STDIN:-false}" == "true" ]]; then
  cat > /dev/null
fi
if [[ "${FAKE_COSIGN_RC:-0}" == "0" ]]; then
  echo "Verified OK"
  exit 0
fi
printf '%s\n' "${FAKE_COSIGN_MESSAGE:-Error: verification failed}" >&2
exit "${FAKE_COSIGN_RC}"
`

// reverifyFakeAicr answers `aicr recipe verify-catalog` from FAKE_AICR_RC.
const reverifyFakeAicr = `#!/usr/bin/env bash
set -euo pipefail
if [[ "${FAKE_AICR_RC:-0}" == "0" ]]; then
  echo "catalog verified"
  exit 0
fi
printf '%s\n' "${FAKE_AICR_MESSAGE:-Error: catalog verification failed}" >&2
exit "${FAKE_AICR_RC}"
`

// reverifyFakeGH materializes every --pattern into --dir, or fails the
// whole download when FAKE_GH_RC is set (a GitHub API / transport failure).
const reverifyFakeGH = `#!/usr/bin/env bash
set -euo pipefail
# Same stdin-consuming child model as the cosign fake above.
if [[ "${FAKE_GH_DRAIN_STDIN:-false}" == "true" ]]; then
  cat > /dev/null
fi
if [[ "${FAKE_GH_RC:-0}" != "0" ]]; then
  echo "gh: HTTP 503 (https://api.github.com)" >&2
  exit "${FAKE_GH_RC}"
fi
dir="."
patterns=()
previous=""
for argument in "$@"; do
  case "${previous}" in
    --dir) dir="${argument}" ;;
    --pattern) patterns+=("${argument}") ;;
  esac
  previous="${argument}"
done
mkdir -p "${dir}"
for pattern in "${patterns[@]}"; do
  printf 'fake release asset: %s\n' "${pattern}" > "${dir}/${pattern}"
done
`
