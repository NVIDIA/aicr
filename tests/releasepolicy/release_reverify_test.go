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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// The per-call `timeout --foreground` bounds stack, and a job that hits
	// timeout-minutes is CANCELED by GitHub, which skips every `if: failure()`
	// step: red
	// with no degraded issue. The budget is sized in a comment on the field; this
	// pins the floor so a casual reduction has to revisit that arithmetic.
	budget, isInt := job["timeout-minutes"].(int)
	if !isInt || budget < 45 {
		t.Errorf("timeout-minutes = %v, want an explicit budget of at least 45 to cover the stacked per-call bounds", job["timeout-minutes"])
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
		"CLASSIFICATION=${classification}",   // same machine-readable line as tools/rekor-monitor
		"classification=${classification}",   // and the step output the gates branch on
		"timeout --foreground",               // every network call is bounded
		"recipe verify-catalog",              // the shipped catalog verification command
		"cosign verify-blob-attestation",     // the shipped blob attestation command
		"set -uo pipefail\nset +e",           // errexit is INHERITED from `bash -e {0}`
		"sigstore_reachable",                 // liveness guard
		"infra_shaped",                       // pattern guard
		`[ -r "$1" ] || return 0`,            // an unreadable log is not evidence
		`if [ ! -s "${log}" ]; then`,         // nor is an empty one
		`-eq 124 ]`,                          // a timeout kill is infrastructure
		`-eq 137 ]`,                          // so is a SIGKILL or OOM
		`if [ ! -r "${RELEASE_ASSETS}" ];`,   // grep exits 2 on an unreadable inventory
		`^https://github\\.com/`,             // fully escaped identity regexp
		`/\\.github/workflows/on-tag\\.yaml`, // including the /.github/ segment
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
	alertScript := stringValue(t, steps[alert].(map[string]any), "run")
	if !strings.Contains(alertScript, "select(.title == $t)") {
		t.Error("the security issue must de-duplicate on an exact title match")
	}
	// Tag-scoped: the job only verifies the latest release, so an alert must name
	// the tag it was raised for or a later clean run on a different tag would
	// close it (see TestReleaseReverifyAlertCloseIsTagScoped).
	if !strings.Contains(alertScript, `title="${ALERT_TITLE} [${TAG}]"`) {
		t.Error("the alert title must carry the resolved tag")
	}
	// A transient 5xx or a renamed label must not cost the one notification this
	// workflow exists to deliver.
	if !strings.Contains(alertScript, "gh-api-retry.sh") {
		t.Error("the alert de-duplication read must use the shared bounded-retry helper")
	}
	if !strings.Contains(alertScript, `--title "${title}" --body-file alert-body.md`) {
		t.Error("the alert must fall back to an unlabelled issue rather than abort on a missing label")
	}

	slack := stepIndex(steps, "Post Slack alert on security finding")
	if slack < 0 {
		t.Fatal("a tamper finding must page Slack, as rekor-monitor does")
	}
	slackStep := steps[slack].(map[string]any)
	if got := fmt.Sprint(slackStep["if"]); !strings.Contains(got, "classification == 'tamper'") {
		t.Errorf("Slack gate = %q, want the security classification only, never degraded", got)
	}
	if got := fmt.Sprint(mapValue(t, slackStep, "env")["SLACK_SERVICE"]); got != "${{ secrets.SLACK_SERVICE }}" {
		t.Errorf("Slack secret wiring = %q, want the same SLACK_SERVICE rekor-monitor uses", got)
	}

	degradedText := marshalYAML(t, steps[degraded])
	// Only a "failure" conclusion counts. A run that was canceled, skipped or
	// timed out says nothing about upstream health and would inflate the streak.
	if !strings.Contains(degradedText, `map(. == "failure") | (index(false) // length)`) {
		t.Error("the streak must count only consecutive failure conclusions")
	}
	if !strings.Contains(degradedText, "workflows/release-reverify.yaml/runs") {
		t.Error("the streak query must read this workflow's own run history")
	}

	closeScript := stringValue(t, steps[closer].(map[string]any), "run")
	if !strings.Contains(closeScript, `"${ALERT_TITLE} [${TAG}]"`) {
		t.Error("the close loop must scope the alert to the tag this run verified")
	}
	// A list blip must not flip a clean verification red, which would also
	// inflate the next run's streak.
	if !strings.Contains(closeScript, `|| echo '[]'`) {
		t.Error("the close-on-success listing must tolerate a transient list failure")
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
	t.Parallel()
	script := reverifyClassifierScript(t)

	// Every fixture derives from the floor constant. Hardcoding a version here
	// would desynchronize the moment the floor is raised, and unevenly: clean and
	// operational cases would fail loudly on a now-missing archive, while
	// security cases would keep PASSING on that spurious missing-archive finding
	// (tamper is sticky) rather than on the condition they were written for.
	atFloor := reverifySBOMFloor
	atFloorVersion := strings.TrimPrefix(atFloor, "v")
	aboveFloor := "v0.19.0"
	aboveFloorVersion := strings.TrimPrefix(aboveFloor, "v")
	assetsFor := func(version string, extra ...string) []string {
		base := make([]string, 0, 3+len(extra))
		base = append(base,
			"aicr_"+version+"_linux_amd64.tar.gz",
			"aicr_checksums.txt",
			"recipe-catalog.sigstore.json",
		)
		return append(base, extra...)
	}
	// The linux/amd64 SBOM subjects every post-floor release must publish, read
	// from the workflow so the fixtures cannot drift from what it mandates.
	sbomSubjects := func(version string) []string {
		binaries := strings.Fields(reverifyWorkflowEnv(t, "EXPECTED_SBOM_BINARIES"))
		names := make([]string, 0, len(binaries))
		for _, binary := range binaries {
			names = append(names, binary+"_"+version+"_linux_amd64.sbom.json")
		}
		return names
	}
	unsignedSBOMAssets := func(version string) []string {
		return assetsFor(version, sbomSubjects(version)...)
	}
	signedSBOMAssets := func(version string) []string {
		assets := assetsFor(version)
		for _, subject := range sbomSubjects(version) {
			assets = append(assets, subject, subject+".sigstore.json")
		}
		return assets
	}

	tests := []struct {
		name string
		opts reverifyOptions
		want string
		code int
	}{
		{
			// The real at-floor release ships unsigned aicr and aicrd linux/amd64
			// SBOMs with no .sigstore.json siblings, which is precisely the shape
			// the floor exempts. Without those assets this case would prove the
			// version gate fires but not that the exemption works on a release
			// that actually ships unsigned SBOMs.
			name: "a release at the floor with unsigned SBOMs verifies clean",
			opts: reverifyOptions{
				tag:            atFloor,
				assets:         unsignedSBOMAssets(atFloorVersion),
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
				tag: atFloor,
				assets: []string{
					"aicr_" + atFloorVersion + "_linux_amd64.tar.gz",
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
				tag:            atFloor,
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
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
				installOutcome:    "failure",
				omitAttestation:   true,
				sigstoreReachable: true,
			},
			want: "tamper",
			code: 1,
		},
		{
			// The expected set is derived from the tag, so deleting one of the
			// two mandatory SBOMs is a finding. Derived from the RELEASE's own
			// inventory it would not be: the loop would verify the survivor and
			// report clean.
			name: "a mandatory SBOM missing from the release is a security finding",
			opts: reverifyOptions{
				tag: aboveFloor,
				assets: append(assetsFor(aboveFloorVersion),
					"aicr_"+aboveFloorVersion+"_linux_amd64.sbom.json",
					"aicr_"+aboveFloorVersion+"_linux_amd64.sbom.json.sigstore.json"),
				installOutcome:    "success",
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
				assets:         assetsFor(aboveFloorVersion),
				installOutcome: "success",
			},
			want: "tamper",
			code: 1,
		},
		{
			name: "a signature that fails against a reachable Sigstore is a security finding",
			opts: reverifyOptions{
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
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
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
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
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
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
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
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
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
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
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
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
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
				installOutcome:    "success",
				ghRC:              1,
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			// The fault mchmarny found: a full disk or a permissions fault on
			// WORK_DIR breaks the extract. Before the guard, control fell
			// through to cosign, which failed on a bundle that was never
			// written -- and "no such file or directory" matches no
			// infra_shaped pattern, so both guards passed it through to a
			// security page. Every sibling branch in this chain demotes.
			name: "a failed extract is operational, never a security finding",
			opts: reverifyOptions{
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
				installOutcome:    "failure",
				tarExtractFails:   true,
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			// A `timeout --foreground` kill exits 124. The message the fake still
			// writes is deliberately NOT infra-shaped, so only the exit-status
			// demotion can catch this.
			name: "a cosign killed by its timeout is operational",
			opts: reverifyOptions{
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
				installOutcome:    "failure",
				cosignRC:          124,
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			// A SIGKILL (OOM killer) surfaces as 137.
			name: "a cosign killed by the OOM killer is operational",
			opts: reverifyOptions{
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
				installOutcome:    "failure",
				cosignRC:          137,
				cosignSilent:      true,
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			// An empty log is not evidence: grep declines to match it, so the
			// pattern guard alone would pass this straight through to security.
			name: "a failure that produced no output is operational",
			opts: reverifyOptions{
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
				installOutcome:    "failure",
				cosignRC:          1,
				cosignSilent:      true,
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			// The `> <log>` redirect fails against a pre-existing unreadable
			// file, so the classifier is handed a non-empty log it cannot read.
			// grep exits 2 there, which is indistinguishable from "no match".
			name: "a log the classifier cannot read is operational",
			opts: reverifyOptions{
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
				installOutcome:    "success",
				seedUnreadableLog: "catalog-verify.log",
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			// grep exits 2 on an unreadable inventory, which have_asset cannot
			// tell from "not present", so every required asset would read as
			// missing and page.
			name: "an unreadable asset inventory is operational, not every asset missing",
			opts: reverifyOptions{
				tag:               atFloor,
				missingAssetsFile: true,
				installOutcome:    "success",
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			// A Fulcio-only outage must demote too: probing the TUF CDN alone
			// would stay green through it.
			name: "a Fulcio-only outage demotes a failed verification",
			opts: reverifyOptions{
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
				installOutcome:    "failure",
				cosignRC:          1,
				cosignMessage:     "Error: no matching signatures found for the given identity",
				sigstoreReachable: true,
				sigstoreFailURL:   "fulcio",
			},
			want: "operational",
			code: 3,
		},
		{
			// Failing to order the tag against the floor must not silently skip
			// every SBOM check.
			name: "an undecidable SBOM floor ordering is operational",
			opts: reverifyOptions{
				tag:               aboveFloor,
				assets:            assetsFor(aboveFloorVersion),
				installOutcome:    "success",
				sortFails:         true,
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			name: "a corrupt archive download is operational, not a missing attestation",
			opts: reverifyOptions{
				tag:               atFloor,
				assets:            assetsFor(atFloorVersion),
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
				tag:               atFloor,
				assets:            unsignedSBOMAssets(atFloorVersion),
				installOutcome:    "failure",
				checksumsManifest: "match",
				sigstoreReachable: true,
			},
			want: "operational",
			code: 3,
		},
		{
			// The attestation binds the BINARY, so cosign passes while the
			// published manifest no longer describes the published archive. The
			// install action fails at its own sha256sum long before cosign, so
			// without the checksum re-check this reports "transient" forever
			// while every consumer following the documented checksum flow fails.
			name: "a checksums manifest that stopped matching is a security finding",
			opts: reverifyOptions{
				tag:               atFloor,
				assets:            unsignedSBOMAssets(atFloorVersion),
				installOutcome:    "failure",
				checksumsManifest: "mismatch",
				sigstoreReachable: true,
			},
			want: "tamper",
			code: 1,
		},

		// --- precedence: a real finding is never masked by a concurrent outage ---
		{
			name: "a missing asset still pages when the network is also down",
			opts: reverifyOptions{
				tag:               atFloor,
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
			t.Parallel()
			got, code, output := runReverifyClassifier(t, script, tc.opts)
			if got != tc.want || code != tc.code {
				t.Fatalf("classification = %q (exit %d), want %q (exit %d)\n%s", got, code, tc.want, tc.code, output)
			}
			if !strings.Contains(output, "CLASSIFICATION="+tc.want) {
				t.Errorf("classifier did not print the machine-readable CLASSIFICATION line\n%s", output)
			}
			// security() is the only emitter of ::error:: in the step, so a run
			// that is not a finding must carry none. This pins "NOT tamper"
			// directly rather than inferring it from the classification, and it
			// holds for every non-finding row, not just the one being added.
			if tc.want != "tamper" && strings.Contains(output, "::error::") {
				t.Errorf("a %s run emitted a security annotation; an infrastructure fault must never page\n%s", tc.want, output)
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
	t.Parallel()
	script := reverifyClassifierScript(t)

	tests := []struct {
		name string
		from string
		to   string
		opts reverifyOptions
		// wantIntactTamper inverts the assertion for a guard whose REMOVAL hides
		// a finding rather than manufacturing one: intact must be tamper and the
		// mutation must demote it to operational.
		wantIntactTamper bool
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
			// Restores the pre-fix, unguarded extract. The step runs without
			// `set -e`, so the failure falls through to cosign and reaches
			// security -- the guards are demote-only and cannot save a path
			// that never reaches them.
			name: "restoring the unguarded tar extract",
			from: `  if ! tar -xzf "${REL_DIR}/${archive}" -C "${WORK_DIR}" \
      aicr aicr-attestation.sigstore.json 2>/dev/null; then
    operational "the archive listed but could not be extracted; treating as infrastructure"
  elif timeout --foreground 120s cosign verify-blob-attestation \`,
			to: `  tar -xzf "${REL_DIR}/${archive}" -C "${WORK_DIR}" \
      aicr aicr-attestation.sigstore.json 2>/dev/null
  if timeout --foreground 120s cosign verify-blob-attestation \`,
			opts: reverifyOptions{
				installOutcome: "failure",
				// The extract fails; Sigstore is up and the resulting cosign
				// message is not infra-shaped, so the intact script must still
				// say operational.
				tarExtractFails:   true,
				sigstoreReachable: true,
			},
		},
		{
			// Exit 124 is a `timeout --foreground` kill and 137 a SIGKILL/OOM.
			// The fake still writes a non-infra-shaped message, so nothing else
			// in the chain can demote this.
			name: "removing the killed-command demotion",
			from: `  if [ "${status}" -eq 124 ] || [ "${status}" -eq 137 ]; then`,
			to:   "  if false; then",
			opts: reverifyOptions{
				installOutcome:    "failure",
				cosignRC:          124,
				sigstoreReachable: true,
			},
		},
		{
			// grep declines to match an empty file, so the pattern guard reads
			// "no infrastructure signature" and reaches security.
			name: "removing the empty-log demotion",
			from: `  if [ ! -s "${log}" ]; then`,
			to:   "  if false; then",
			opts: reverifyOptions{
				installOutcome:    "failure",
				cosignRC:          1,
				cosignSilent:      true,
				sigstoreReachable: true,
			},
		},
		{
			// grep exits 2 on an unreadable file, indistinguishable from rc 1.
			name: "removing the unreadable-log demotion",
			from: `  [ -r "$1" ] || return 0`,
			to:   `  [ -r "$1" ] || true`,
			opts: reverifyOptions{
				installOutcome:    "success",
				seedUnreadableLog: "catalog-verify.log",
				sigstoreReachable: true,
			},
		},
		{
			// Same rc-2 conflation one level up: an unreadable inventory makes
			// have_asset report every required asset as missing.
			name: "removing the asset-inventory readability guard",
			from: `if [ ! -r "${RELEASE_ASSETS}" ]; then`,
			to:   "if false; then",
			opts: reverifyOptions{
				missingAssetsFile: true,
				installOutcome:    "success",
				sigstoreReachable: true,
			},
		},
		{
			// The pre-fix shape: a passing cosign was taken as proof the install
			// action's failure was transient. It is not: the attestation binds
			// the binary, the manifest covers the archive.
			name: "presuming a passing cosign clears the checksums manifest",
			from: `    if [ ! -r "${REL_DIR}/aicr_checksums.txt" ]; then
      operational "the checksums manifest is not on disk to re-check; treating as infrastructure"
    elif (cd "${REL_DIR}" && grep " ${archive}\$" aicr_checksums.txt | sha256sum -c -) \
        > "${WORK_DIR}/checksum-verify.log" 2>&1; then
      operational "binary provenance and checksum verified on re-run after the install action failed; treating as a transient failure"
    else
      status=$?
      cat "${WORK_DIR}/checksum-verify.log" >&2 || true
      classify_failure "checksum verification of ${archive} for ${TAG}" "${WORK_DIR}/checksum-verify.log" "${status}"
    fi`,
			to: `    operational "binary provenance verified on re-run after the install action failed; treating as a transient failure"`,
			opts: reverifyOptions{
				installOutcome:    "failure",
				checksumsManifest: "mismatch",
				sigstoreReachable: true,
			},
			wantIntactTamper: true,
		},
		{
			name: "removing the infrastructure-signature guard",
			from: `  if infra_shaped "${log}"; then`,
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
			t.Parallel()
			opts := tc.opts
			opts.tag = reverifySBOMFloor
			if !opts.missingAssetsFile {
				version := strings.TrimPrefix(reverifySBOMFloor, "v")
				opts.assets = []string{
					"aicr_" + version + "_linux_amd64.tar.gz",
					"aicr_checksums.txt",
					"recipe-catalog.sigstore.json",
				}
				for _, binary := range strings.Fields(reverifyWorkflowEnv(t, "EXPECTED_SBOM_BINARIES")) {
					opts.assets = append(opts.assets, binary+"_"+version+"_linux_amd64.sbom.json")
				}
			}

			wantIntact, wantBroken := "operational", "tamper"
			if tc.wantIntactTamper {
				wantIntact, wantBroken = "tamper", "operational"
			}

			intact, _, output := runReverifyClassifier(t, script, opts)
			if intact != wantIntact {
				t.Fatalf("intact classifier = %q, want %s\n%s", intact, wantIntact, output)
			}

			if !strings.Contains(script, tc.from) {
				t.Fatalf("mutation target %q is no longer in the classifier", tc.from)
			}
			mutated := strings.Replace(script, tc.from, tc.to, 1)
			broken, _, brokenOutput := runReverifyClassifier(t, mutated, opts)
			if broken != wantBroken {
				t.Fatalf("classifier without %s = %q, want %s: the guard is not load-bearing and this test proves nothing\n%s",
					tc.name, broken, wantBroken, brokenOutput)
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
	t.Parallel()
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
	// aicr and aicrd are the two binaries GoReleaser builds today; the third is a
	// stand-in for any future binary, injected through the same env var the
	// workflow derives its expected set from. Three subjects, not two: with two,
	// an early-terminating loop and a loop that merely dropped the last entry are
	// indistinguishable.
	const binaries = "aicr aicrd aicr-gate"
	names := strings.Fields(binaries)
	subjects := make([]string, 0, len(names))
	for _, binary := range names {
		subjects = append(subjects, binary+"_"+version+"_linux_amd64.sbom.json")
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
			t.Parallel()
			opts := reverifyOptions{
				tag:                  "v" + version,
				assets:               assets,
				installOutcome:       "success",
				sigstoreReachable:    true,
				drainGHStdin:         tc.drainGHStdin,
				drainCosignStdin:     tc.drainCosignStdin,
				expectedSBOMBinaries: binaries,
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

// TestReleaseReverifySBOMFloorOrderingIsGuarded covers the one fault in this
// step that fails OPEN rather than paging. If `sort -V` is unavailable or dies,
// the floor comparison is undecidable; unguarded, the ordering test then reads
// as "at or before the floor" and skips every SBOM check while logging that it
// did so deliberately. A release that is genuinely missing an SBOM bundle would
// come back clean.
func TestReleaseReverifySBOMFloorOrderingIsGuarded(t *testing.T) {
	t.Parallel()
	script := reverifyClassifierScript(t)

	const from = `newest=""
if ! newest="$(printf '%s\n%s\n' "${SBOM_SIGNING_FLOOR}" "${TAG}" | sort -V | tail -n1)" \
    || [ -z "${newest}" ]; then
  operational "could not order ${TAG} against the SBOM signing floor ${SBOM_SIGNING_FLOOR}; treating as infrastructure"
elif [ "${TAG}" = "${SBOM_SIGNING_FLOOR}" ] || [ "${newest}" != "${TAG}" ]; then`
	const to = `newest="$(printf '%s\n%s\n' "${SBOM_SIGNING_FLOOR}" "${TAG}" | sort -V | tail -n1)"
if [ "${TAG}" = "${SBOM_SIGNING_FLOOR}" ] || [ "${newest}" != "${TAG}" ]; then`
	if !strings.Contains(script, from) {
		t.Fatal("the SBOM floor ordering guard is no longer in the classifier")
	}
	mutated := strings.Replace(script, from, to, 1)

	// A release above the floor that ships an SBOM with no attestation bundle:
	// a finding the SBOM block is supposed to catch.
	opts := reverifyOptions{
		tag: "v0.19.0",
		assets: []string{
			"aicr_0.19.0_linux_amd64.tar.gz",
			"aicr_checksums.txt",
			"recipe-catalog.sigstore.json",
			"aicr_0.19.0_linux_amd64.sbom.json",
		},
		installOutcome:    "success",
		sigstoreReachable: true,
	}

	// Baseline: with a working sort the block runs and finds it.
	if class, code, output := runReverifyClassifier(t, script, opts); class != "tamper" || code != 1 {
		t.Fatalf("baseline classification = %q (exit %d), want tamper (exit 1)\n%s", class, code, output)
	}

	// Undecidable ordering: demote, do not silently skip.
	opts.sortFails = true
	class, code, output := runReverifyClassifier(t, script, opts)
	if class != "operational" || code != 3 {
		t.Fatalf("with an undecidable ordering, classification = %q (exit %d), want operational (exit 3)\n%s", class, code, output)
	}

	// Unguarded, the same fault reports a clean release and never mentions the
	// missing bundle. That is the fail-open direction this guard exists for.
	brokenClass, brokenCode, brokenOutput := runReverifyClassifier(t, mutated, opts)
	if brokenClass == "operational" {
		t.Fatalf("the unguarded ordering still demoted; the guard is not load-bearing and this test proves nothing\n%s", brokenOutput)
	}
	if brokenClass != "clean" || brokenCode != 0 {
		t.Errorf("unguarded ordering = %q (exit %d), want clean (exit 0): the skip is expected to be SILENT", brokenClass, brokenCode)
	}
	if !strings.Contains(brokenOutput, "at or before the SBOM signing floor") {
		t.Errorf("unguarded ordering did not take the misleading floor branch\n%s", brokenOutput)
	}
	if strings.Contains(brokenOutput, "with no attestation bundle") {
		t.Errorf("unguarded ordering unexpectedly still reported the missing bundle\n%s", brokenOutput)
	}
}

// TestReleaseReverifyMandatorySBOMSetIsDerivedFromTheTag pins the one thing that
// makes the SBOM check able to find a missing artifact at all.
//
// Derived from the RELEASE's own inventory, the loop only iterates what the
// release already published, so the only detectable fault is a release with zero
// SBOMs: delete one of the two mandatory subjects and the loop verifies the
// survivor and reports clean. Deriving the expected set from the TAG, as
// expected_release_asset_names() in .github/scripts/release-images.sh does, is
// what turns a silently-missing mandatory artifact into a finding. This is the
// same derive-from-the-artifact-under-test flaw that shipped the allowlist bug
// in NVIDIA/aicr#1982.
func TestReleaseReverifyMandatorySBOMSetIsDerivedFromTheTag(t *testing.T) {
	t.Parallel()
	script := reverifyClassifierScript(t)

	derivation := "  read -r -a sbom_binaries <<< \"${EXPECTED_SBOM_BINARIES}\"\n" +
		"  expected_sboms=\"\"\n" +
		"  for binary in \"${sbom_binaries[@]}\"; do\n" +
		"    expected_sboms+=\"${binary}_${version}_linux_amd64.sbom.json\"$'\\n'\n" +
		"  done\n" +
		"  expected_sboms=\"${expected_sboms%$'\\n'}\""
	presence := "    if ! have_asset \"${sbom}\"; then\n" +
		"      security \"release ${TAG} does not publish the mandatory SBOM ${sbom}\"\n" +
		"    elif ! have_asset \"${bundle}\"; then"
	for _, target := range []string{derivation, presence} {
		if !strings.Contains(script, target) {
			t.Fatalf("mutation target is no longer in the classifier:\n%s", target)
		}
	}
	// Revert to the pre-fix shape: enumerate whatever the release shipped, and
	// check only that each of those has a bundle.
	inventoryDerived := "  expected_sboms=\"$(grep -E '_linux_amd64\\.sbom\\.json$' \"${RELEASE_ASSETS}\" || true)\""
	mutated := strings.Replace(script, derivation, inventoryDerived, 1)
	mutated = strings.Replace(mutated, presence, "    if ! have_asset \"${bundle}\"; then", 1)

	const version = "0.19.0"
	binaries := strings.Fields(reverifyWorkflowEnv(t, "EXPECTED_SBOM_BINARIES"))
	if len(binaries) < 2 {
		t.Fatalf("EXPECTED_SBOM_BINARIES = %v, want at least two mandatory subjects", binaries)
	}
	// A release that dropped the last mandatory subject and its bundle, but
	// published every other one correctly.
	dropped := binaries[len(binaries)-1] + "_" + version + "_linux_amd64.sbom.json"
	assets := make([]string, 0, 3+2*(len(binaries)-1))
	assets = append(assets,
		"aicr_"+version+"_linux_amd64.tar.gz",
		"aicr_checksums.txt",
		"recipe-catalog.sigstore.json",
	)
	for _, binary := range binaries[:len(binaries)-1] {
		subject := binary + "_" + version + "_linux_amd64.sbom.json"
		assets = append(assets, subject, subject+".sigstore.json")
	}
	opts := reverifyOptions{
		tag:               "v" + version,
		assets:            assets,
		installOutcome:    "success",
		sigstoreReachable: true,
	}

	class, code, output := runReverifyClassifier(t, script, opts)
	if class != "tamper" || code != 1 {
		t.Fatalf("classification = %q (exit %d), want tamper (exit 1) for a missing mandatory SBOM\n%s", class, code, output)
	}
	if !strings.Contains(output, dropped) {
		t.Errorf("the finding did not name the missing subject %q\n%s", dropped, output)
	}

	brokenClass, brokenCode, brokenOutput := runReverifyClassifier(t, mutated, opts)
	if brokenClass == "tamper" {
		t.Fatalf("the inventory-derived form still found it; the tag derivation is not load-bearing\n%s", brokenOutput)
	}
	if brokenClass != "clean" || brokenCode != 0 {
		t.Errorf("inventory-derived form = %q (exit %d), want clean (exit 0): the miss is expected to be SILENT", brokenClass, brokenCode)
	}
	if strings.Contains(brokenOutput, dropped) {
		t.Errorf("the inventory-derived form unexpectedly mentioned %q\n%s", dropped, brokenOutput)
	}
}

// TestReleaseReverifyAlertCloseIsTagScoped covers the close-on-success step. The
// job only ever verifies /releases/latest, so a clean run on vX+1 proves nothing
// about vX and must not close vX's alert: once vX stops being latest it is never
// re-checked, and closing its issue would silently resolve a live finding. This
// is the deliberate divergence from rekor-monitor, whose alert tracks the
// release-agnostic log and so genuinely clears on any clean run.
func TestReleaseReverifyAlertCloseIsTagScoped(t *testing.T) {
	t.Parallel()
	doc := loadYAML(t, reverifyWorkflow)
	env := mapValue(t, doc, "env")
	alertTitle := fmt.Sprint(env["ALERT_TITLE"])
	degradedTitle := fmt.Sprint(env["DEGRADED_TITLE"])

	steps := sliceValue(t, mapValue(t, mapValue(t, doc, "jobs"), "reverify"), "steps")
	index := stepIndex(steps, "Close open issues on success")
	if index < 0 {
		t.Fatal("re-verification must auto-close its issues on a clean run")
	}
	script := stringValue(t, steps[index].(map[string]any), "run")

	scoped := func(tag string) string { return alertTitle + " [" + tag + "]" }

	tests := []struct {
		name   string
		tag    string
		issues []closableIssue
		want   []string
	}{
		{
			name: "a clean run on a newer release leaves the older tag's alert open",
			tag:  "v0.19.0",
			issues: []closableIssue{
				{Number: 11, Title: scoped("v0.18.0")},
				{Number: 12, Title: degradedTitle},
			},
			// Only the degraded issue, which tracks the checker rather than a
			// release, closes.
			want: []string{"12"},
		},
		{
			name: "a clean run closes the alert for the tag it verified",
			tag:  "v0.19.0",
			issues: []closableIssue{
				{Number: 21, Title: scoped("v0.19.0")},
				{Number: 22, Title: degradedTitle},
			},
			want: []string{"21", "22"},
		},
		{
			name: "a title that merely contains the alert text is not closed",
			tag:  "v0.19.0",
			issues: []closableIssue{
				{Number: 31, Title: scoped("v0.19.0") + " (follow-up)"},
			},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			closed, output, err := runReverifyCloseStep(t, script, tc.tag, tc.issues)
			if err != nil {
				t.Fatalf("close step failed: %v\n%s", err, output)
			}
			if !slices.Equal(closed, tc.want) {
				t.Errorf("closed %v, want %v\n%s", closed, tc.want, output)
			}
		})
	}

	t.Run("an unscoped close reopens the bug", func(t *testing.T) {
		t.Parallel()
		// The pre-fix shape: one global alert title, closed by any clean run.
		// Staged with the unscoped title on both sides, which is what the
		// workflow used to create.
		const from = `for title in "${ALERT_TITLE} [${TAG}]" "${DEGRADED_TITLE}"; do`
		if !strings.Contains(script, from) {
			t.Fatal("the close loop no longer scopes the alert title by tag")
		}
		mutated := strings.Replace(script, from, `for title in "${ALERT_TITLE}" "${DEGRADED_TITLE}"; do`, 1)
		issues := []closableIssue{{Number: 41, Title: alertTitle}}

		closed, output, err := runReverifyCloseStep(t, script, "v0.19.0", issues)
		if err != nil {
			t.Fatalf("close step failed: %v\n%s", err, output)
		}
		if len(closed) != 0 {
			t.Fatalf("the tag-scoped close matched an unscoped title: closed %v", closed)
		}

		brokenClosed, brokenOutput, err := runReverifyCloseStep(t, mutated, "v0.19.0", issues)
		if err != nil {
			t.Fatalf("mutated close step failed: %v\n%s", err, brokenOutput)
		}
		if !slices.Equal(brokenClosed, []string{"41"}) {
			t.Fatalf("without tag scoping the close did not reach the unscoped alert (closed %v); the scoping is not load-bearing", brokenClosed)
		}
	})
}

// closableIssue is one open issue the fake `gh issue list` returns.
type closableIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// runReverifyCloseStep executes the extracted close-on-success step against a
// fake gh, returning the issue numbers it closed in order.
func runReverifyCloseStep(t *testing.T, script, tag string, issues []closableIssue) ([]string, string, error) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	writeExecutable(t, filepath.Join(bin, "gh"), reverifyFakeGHIssues)

	// The fake answers every `issue list` with the whole fixture, mirroring
	// GitHub's phrase-based title search over-matching. The step's own exact
	// title filter is what has to do the work.
	listing, err := json.Marshal(issues)
	if err != nil {
		t.Fatalf("marshal issue fixture: %v", err)
	}
	listFile := filepath.Join(root, "issues.json")
	if err := os.WriteFile(listFile, listing, 0o600); err != nil {
		t.Fatalf("write issue fixture: %v", err)
	}
	closedFile := filepath.Join(root, "closed.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", reverifyStepShell(t, root, script)...)
	command.Dir = root
	command.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"GH_TOKEN=fake",
		"GITHUB_REPOSITORY=NVIDIA/aicr",
		"GITHUB_RUN_ID=1234",
		"TAG="+tag,
		"ALERT_TITLE="+reverifyWorkflowEnv(t, "ALERT_TITLE"),
		"DEGRADED_TITLE="+reverifyWorkflowEnv(t, "DEGRADED_TITLE"),
		"FAKE_ISSUE_LIST="+listFile,
		"FAKE_CLOSED_LOG="+closedFile,
	)
	combined, runErr := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("close step exceeded the test deadline: %v\n%s", ctx.Err(), combined)
	}
	var closed []string
	if data, readErr := os.ReadFile(closedFile); readErr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line != "" {
				closed = append(closed, line)
			}
		}
	} else if !os.IsNotExist(readErr) {
		t.Fatalf("read closed log: %v", readErr)
	}
	return closed, string(combined), runErr
}

// reverifyWorkflowEnv reads one workflow-level env value, so the tests bind to
// the shipped titles rather than a copy of them.
func reverifyWorkflowEnv(t *testing.T, key string) string {
	t.Helper()
	return fmt.Sprint(mapValue(t, loadYAML(t, reverifyWorkflow), "env")[key])
}

// reverifyFakeGHIssues answers `gh issue list` from a fixture and records every
// `gh issue close` so the test can assert on exactly which issues were touched.
const reverifyFakeGHIssues = `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}:${2:-}" in
  issue:list)
    cat "${FAKE_ISSUE_LIST}"
    ;;
  issue:close)
    printf '%s\n' "${3}" >> "${FAKE_CLOSED_LOG}"
    ;;
  *)
    echo "unexpected gh invocation: $*" >&2
    exit 64
    ;;
esac
`

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
	// tarExtractFails makes the fake tar list the archive fine but fail the
	// extract, modeling a full disk or a permissions fault on WORK_DIR.
	tarExtractFails bool
	// missingAssetsFile points RELEASE_ASSETS at a path that does not exist, so
	// every `grep` against it exits 2 rather than 1.
	missingAssetsFile bool
	// seedUnreadableLog pre-creates the named log under WORK_DIR with content and
	// mode 0000, so the step's `> <log>` redirect fails and the classifier is
	// handed a non-empty log it cannot read.
	seedUnreadableLog string
	// sortFails makes the fake sort exit non-zero, breaking the SBOM floor
	// ordering.
	sortFails bool
	// checksumsManifest stages aicr_checksums.txt next to the downloaded archive:
	// "match" for the archive's real digest, "mismatch" for a manifest that no
	// longer describes it. Empty stages none.
	checksumsManifest string

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

	// cosignSilent makes the fake fail without writing anything, so the captured
	// log is empty (a killed or OOMed process).
	cosignSilent bool
	// sigstoreFailURL fails only the probe whose URL contains this substring,
	// modeling a Fulcio-only or TUF-only outage.
	sigstoreFailURL string
	// expectedSBOMBinaries overrides EXPECTED_SBOM_BINARIES. Empty means the
	// shipped workflow value, so cases bind to what production actually
	// mandates unless they need a different subject count.
	expectedSBOMBinaries string
}

// expectedSBOMBinaries resolves the SBOM subjects a case runs against, defaulting
// to the shipped workflow value so a case exercises what production mandates.
func expectedSBOMBinaries(t *testing.T, opts reverifyOptions) string {
	t.Helper()
	if opts.expectedSBOMBinaries != "" {
		return opts.expectedSBOMBinaries
	}
	return reverifyWorkflowEnv(t, "EXPECTED_SBOM_BINARIES")
}

// reverifyStepShell stages an extracted `run:` block and returns the bash
// arguments GitHub itself would use to invoke it.
//
// A step with no `shell:` key runs as `bash -e {0}` on the Actions runner: a
// FILE, under errexit. Running it any other way (a plain `bash -c` with no -e,
// which is what these tests used to do) tests semantics production does not
// have, so an unguarded command added to the step would pass here and abort the
// job in production. The classifier turns errexit off itself with an explicit
// `set +e`; this harness is what proves the step does so rather than assuming
// it. Keep in lockstep with GitHub's documented default shell.
func reverifyStepShell(t *testing.T, dir, script string) []string {
	t.Helper()
	path := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("stage extracted step: %v", err)
	}
	return []string{"-e", path}
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
	writeExecutable(t, filepath.Join(bin, "tar"), reverifyFakeTar)
	writeExecutable(t, filepath.Join(bin, "sort"), reverifyFakeSort)
	writeExecutable(t, filepath.Join(bin, "sha256sum"), reverifyFakeSha256Sum)
	writeExecutable(t, filepath.Join(bin, "cosign"), reverifyFakeCosign)
	writeExecutable(t, filepath.Join(bin, "gh"), reverifyFakeGH)
	aicrBin := filepath.Join(bin, "aicr")
	writeExecutable(t, aicrBin, reverifyFakeAicr)

	assetsFile := filepath.Join(root, "release-assets.txt")
	if opts.missingAssetsFile {
		assetsFile = filepath.Join(root, "no-such-inventory.txt")
	}
	contents := ""
	if len(opts.assets) > 0 {
		contents = strings.Join(opts.assets, "\n") + "\n"
	}
	if !opts.missingAssetsFile {
		if err := os.WriteFile(assetsFile, []byte(contents), 0o600); err != nil {
			t.Fatalf("write asset inventory: %v", err)
		}
	}
	if opts.seedUnreadableLog != "" {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permissions; the unreadable-log path cannot be staged")
		}
		seeded := filepath.Join(workDir, opts.seedUnreadableLog)
		if err := os.WriteFile(seeded, []byte("stale output from a previous run\n"), 0o600); err != nil {
			t.Fatalf("seed unreadable log: %v", err)
		}
		if err := os.Chmod(seeded, 0o000); err != nil {
			t.Fatalf("chmod unreadable log: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(seeded, 0o600) })
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

	if opts.checksumsManifest != "" {
		stageChecksumsManifest(t, relDir, archiveName, opts.checksumsManifest)
	}

	outputs := filepath.Join(root, "outputs")
	summary := filepath.Join(root, "summary.md")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", reverifyStepShell(t, root, script)...)
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
		"EXPECTED_SBOM_BINARIES="+expectedSBOMBinaries(t, opts),
		"SIGSTORE_PROBE_URLS=https://tuf.example.invalid/1.root.json https://fulcio.example.invalid/api/v2/trustBundle",
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
		"FAKE_TAR_REAL="+systemBinary(t, "tar"),
		"FAKE_SORT_REAL="+systemBinary(t, "sort"),
		fmt.Sprintf("FAKE_TAR_EXTRACT_FAILS=%t", opts.tarExtractFails),
		fmt.Sprintf("FAKE_COSIGN_SILENT=%t", opts.cosignSilent),
		"FAKE_SIGSTORE_FAIL_URL="+opts.sigstoreFailURL,
		fmt.Sprintf("FAKE_SORT_FAILS=%t", opts.sortFails),
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

// stageChecksumsManifest writes the aicr_checksums.txt the install action leaves
// beside the archive, either describing it correctly or not.
func stageChecksumsManifest(t *testing.T, dir, archive, mode string) {
	t.Helper()
	digest := strings.Repeat("0", 64)
	if mode == "match" {
		data, err := os.ReadFile(filepath.Join(dir, archive))
		if err != nil {
			t.Fatalf("read archive for its digest: %v", err)
		}
		sum := sha256.Sum256(data)
		digest = hex.EncodeToString(sum[:])
	}
	line := fmt.Sprintf("%s  %s\n", digest, archive)
	if err := os.WriteFile(filepath.Join(dir, "aicr_checksums.txt"), []byte(line), 0o600); err != nil {
		t.Fatalf("stage checksums manifest: %v", err)
	}
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
url="${*: -1}"
# Fail one probe only, so a Fulcio-only (or TUF-only) outage can be modeled.
if [[ -n "${FAKE_SIGSTORE_FAIL_URL:-}" && "${url}" == *"${FAKE_SIGSTORE_FAIL_URL}"* ]]; then
  echo "curl: (7) Failed to connect to ${url}" >&2
  exit 7
fi
if [[ "${FAKE_SIGSTORE_REACHABLE:-true}" == "true" ]]; then
  exit 0
fi
echo "curl: (7) Failed to connect to ${url}" >&2
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
# Real cosign opens the bundle before it verifies anything. Modeling that is
# what makes a failed extract observable: the pre-fix, unguarded tar extract
# fell through to here with no bundle on disk.
bundle=""
previous=""
for argument in "$@"; do
  if [[ "${previous}" == "--bundle" ]]; then bundle="${argument}"; fi
  previous="${argument}"
done
if [[ -n "${bundle}" && ! -f "${bundle}" ]]; then
  echo "Error: opening bundle: open ${bundle}: no such file or directory" >&2
  exit 1
fi
if [[ "${FAKE_COSIGN_RC:-0}" == "0" ]]; then
  echo "Verified OK"
  exit 0
fi
# A killed or OOMed process writes nothing; the captured log is then empty.
if [[ "${FAKE_COSIGN_SILENT:-false}" != "true" ]]; then
  printf '%s\n' "${FAKE_COSIGN_MESSAGE:-Error: verification failed}" >&2
fi
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

// systemBinary resolves a real binary once, so a fake can delegate everything it
// is not deliberately failing. The archive fixtures are built with the same tar
// from the test process, which never sees the fake.
func systemBinary(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("locate %s: %v", name, err)
	}
	return path
}

// reverifyFakeTar delegates to the real tar except that, when
// FAKE_TAR_EXTRACT_FAILS is set, `-x` fails while `-t` still succeeds. That
// asymmetry is the only way to reach the extract branch: the listing has to
// succeed first, or the classifier stops one branch earlier.
const reverifyFakeTar = `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == -x* && "${FAKE_TAR_EXTRACT_FAILS:-false}" == "true" ]]; then
  echo "tar: aicr: Cannot open: No space left on device" >&2
  exit 2
fi
exec "${FAKE_TAR_REAL}" "$@"
`

// reverifyFakeSort delegates to the real sort unless FAKE_SORT_FAILS is set, in
// which case it models a coreutils without -V (or a sort that dies), which is
// what leaves the SBOM floor ordering undecidable.
const reverifyFakeSort = `#!/usr/bin/env bash
set -euo pipefail
if [[ "${FAKE_SORT_FAILS:-false}" == "true" ]]; then
  echo "sort: unrecognized option '--version-sort'" >&2
  exit 2
fi
exec "${FAKE_SORT_REAL}" "$@"
`

// reverifyFakeSha256Sum implements the `-c -` subset the step uses. GNU
// coreutils ships sha256sum on the runner, but macOS provides only
// `shasum -a 256`, so the fake keeps the harness host-independent rather than
// letting a missing binary read as a checksum failure.
const reverifyFakeSha256Sum = `#!/usr/bin/env bash
set -uo pipefail
if [[ "${1:-}" != "-c" ]]; then
  echo "fake sha256sum only implements -c" >&2
  exit 64
fi
digest_of() {
  if command -v shasum > /dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    openssl dgst -sha256 "$1" | awk '{print $NF}'
  fi
}
lines=0
status=0
while read -r expected name; do
  [[ -n "${expected}" ]] || continue
  lines=$((lines + 1))
  actual="$(digest_of "${name}")"
  if [[ "${actual}" == "${expected}" ]]; then
    echo "${name}: OK"
  else
    echo "${name}: FAILED"
    status=1
  fi
done
if [[ "${lines}" -eq 0 ]]; then
  echo "sha256sum: no properly formatted checksum lines found" >&2
  exit 1
fi
if [[ "${status}" -ne 0 ]]; then
  echo "sha256sum: WARNING: 1 computed checksum did NOT match" >&2
fi
exit "${status}"
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
