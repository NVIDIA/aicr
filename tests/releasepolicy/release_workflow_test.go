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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var releaseImages = map[string]string{
	"aicr":         "ghcr.io/nvidia/aicr",
	"aicrd":        "ghcr.io/nvidia/aicrd",
	"aiperf-bench": "ghcr.io/nvidia/aicr-validators/aiperf-bench",
	"conformance":  "ghcr.io/nvidia/aicr-validators/conformance",
	"deployment":   "ghcr.io/nvidia/aicr-validators/deployment",
	"gate":         "ghcr.io/nvidia/aicr-gate",
	"performance":  "ghcr.io/nvidia/aicr-validators/performance",
}

func TestReleaseCandidateBuildPolicy(t *testing.T) {
	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	jobs := mapValue(t, doc, "jobs")
	detect := mapValue(t, jobs, "detect")
	outputs := mapValue(t, detect, "outputs")
	if stringValue(t, outputs, "candidate_tag") != "${{ steps.check.outputs.candidate_tag }}" {
		t.Error("detect must export the validated candidate_tag")
	}

	detectText := marshalYAML(t, detect)
	if !strings.Contains(detectText, `candidate-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}`) {
		t.Error("detect must derive a run-unique candidate tag")
	}

	for _, name := range []string{"build-ko", "build-docker", "build-gate", "docker-manifest"} {
		job := mapValue(t, jobs, name)
		if !containsString(stringSlice(job["needs"]), "detect") {
			t.Errorf("%s must depend on detect", name)
		}
		text := marshalYAML(t, job)
		if strings.Contains(text, ":${{ github.ref_name }}") || strings.Contains(text, ":latest") {
			t.Errorf("%s must not write release or latest aliases", name)
		}
		if !strings.Contains(text, "candidate_tag") {
			t.Errorf("%s must consume detect.outputs.candidate_tag", name)
		}
	}
}

func TestReleaseUsesSingleDigestMap(t *testing.T) {
	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	jobs := mapValue(t, doc, "jobs")
	resolve := mapValue(t, jobs, "resolve-candidates")
	for _, dependency := range []string{"build-ko", "build-docker", "build-gate", "docker-manifest"} {
		if !jobTransitivelyDependsOn(jobs, "resolve-candidates", dependency) {
			t.Errorf("resolve-candidates must transitively depend on %s", dependency)
		}
	}
	if stringValue(t, mapValue(t, resolve, "outputs"), "digest_map") == "" {
		t.Error("resolve-candidates must expose one digest_map output")
	}

	scanText := marshalYAML(t, mapValue(t, jobs, "image-vuln-scan"))
	if !strings.Contains(scanText, "fromJSON(needs.resolve-candidates.outputs.digest_map)[matrix.image.key]") {
		t.Error("scanner must bind every image to the authoritative digest map")
	}
	if !strings.Contains(scanText, "@${{") {
		t.Error("scanner must scan image@digest rather than a mutable tag")
	}

	attest := mapValue(t, jobs, "attest")
	attestText := marshalYAML(t, attest)
	usesDigestMap := strings.Contains(attestText, "needs.resolve-candidates.outputs.digest_map")
	usesCandidateTag := strings.Contains(attestText, "needs.detect.outputs.candidate_tag")
	if !usesDigestMap || !usesCandidateTag {
		t.Error("attester must receive the same digest map and candidate tag")
	}

	releaseCheckText := marshalYAML(t, mapValue(t, jobs, "release-check"))
	for _, gate := range []string{"resolve-candidates", "image-vuln-scan", "attest"} {
		if !strings.Contains(releaseCheckText, gate) {
			t.Errorf("release-check must fail closed on %s", gate)
		}
	}
}

func TestReleaseScansResolvedDigestsOnBothPlatforms(t *testing.T) {
	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	scan := mapValue(t, mapValue(t, doc, "jobs"), "image-vuln-scan")
	matrix := mapValue(t, mapValue(t, scan, "strategy"), "matrix")
	images := sliceValue(t, matrix, "image")
	platforms := sliceValue(t, matrix, "platform")
	if len(images) != len(releaseImages) || len(platforms) != 2 {
		t.Fatalf("scan matrix is %dx%d, want 7x2", len(images), len(platforms))
	}
	gotImages := make(map[string]string, len(images))
	for _, raw := range images {
		entry := raw.(map[string]any)
		gotImages[fmt.Sprint(entry["key"])] = fmt.Sprint(entry["ref"])
	}
	for key, image := range releaseImages {
		if gotImages[key] != image {
			t.Errorf("scan image %s = %q, want %q", key, gotImages[key], image)
		}
	}
	gotPlatforms := make([]string, 0, len(platforms))
	for _, raw := range platforms {
		gotPlatforms = append(gotPlatforms, fmt.Sprint(raw.(map[string]any)["name"]))
	}
	sort.Strings(gotPlatforms)
	if strings.Join(gotPlatforms, ",") != "linux/amd64,linux/arm64" {
		t.Errorf("scan platforms = %v, want both release platforms", gotPlatforms)
	}
	text := marshalYAML(t, scan)
	if !strings.Contains(text, "GRYPE_PLATFORM: ${{ matrix.platform.name }}") {
		t.Error("each scan must explicitly select its matrix platform")
	}
}

func TestReleaseAttestationMatrixIsExact(t *testing.T) {
	doc := loadYAML(t, ".github/workflows/attest-images.yaml")
	attest := mapValue(t, mapValue(t, doc, "jobs"), "attest")
	include := sliceValue(t, mapValue(t, mapValue(t, attest, "strategy"), "matrix"), "include")
	if len(include) != len(releaseImages) {
		t.Fatalf("attestation matrix has %d entries, want 7", len(include))
	}
	got := make(map[string]string, len(include))
	for _, raw := range include {
		entry := raw.(map[string]any)
		got[fmt.Sprint(entry["key"])] = fmt.Sprint(entry["image"])
	}
	for key, image := range releaseImages {
		if got[key] != image {
			t.Errorf("attestation image %s = %q, want %q", key, got[key], image)
		}
	}
}

func TestReleaseAttestsResolvedDigests(t *testing.T) {
	doc := loadYAML(t, ".github/workflows/attest-images.yaml")
	inputs := mapValue(t, mapValue(t, mapValue(t, doc, "on"), "workflow_call"), "inputs")
	for _, name := range []string{"candidate_tag", "digest_map"} {
		input := mapValue(t, inputs, name)
		if required, ok := input["required"].(bool); !ok || !required {
			t.Errorf("attest-images input %s must be required", name)
		}
	}
	jobs := mapValue(t, doc, "jobs")
	if _, ok := jobs["validate-inputs"]; !ok {
		t.Error("attest-images must validate the candidate tag and exact digest map before parsing or I/O")
	} else {
		assertPermissions(t, mapValue(t, jobs, "validate-inputs"), map[string]string{})
	}

	action := loadYAML(t, ".github/actions/attest-image-from-tag/action.yml")
	actionInputs := mapValue(t, action, "inputs")
	for _, name := range []string{"candidate_tag", "expected_digest"} {
		input := mapValue(t, actionInputs, name)
		if required, ok := input["required"].(bool); !ok || !required {
			t.Errorf("attestation action input %s must be required", name)
		}
	}
	text := marshalYAML(t, action)
	if !strings.Contains(text, "resolved digest does not match expected digest") {
		t.Error("attestation action must compare the candidate resolution to expected_digest")
	}
	if strings.Contains(text, "${{ inputs.") && containsDirectRunInput(action) {
		t.Error("attestation action run blocks must not interpolate inputs directly")
	}
}

func TestReleaseSBOMCoversBothPlatforms(t *testing.T) {
	doc := loadYAML(t, ".github/actions/sbom-and-attest/action.yml")
	text := marshalYAML(t, doc)
	steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")
	if stepIndex(steps, "Validate inputs") != 0 {
		t.Error("SBOM input validation must precede registry authentication")
	}
	for index, step := range steps[1:] {
		if strings.Contains(marshalYAML(t, step), "${{ inputs.") {
			t.Errorf("SBOM step %d consumes raw input after validation", index+1)
		}
	}
	if containsDirectRunInput(doc) {
		t.Error("SBOM run blocks must not interpolate inputs directly")
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		if !strings.Contains(text, "SYFT_PLATFORM: "+platform) {
			t.Errorf("SBOM generation must explicitly cover %s", platform)
		}
	}
	if strings.Count(text, "uses: anchore/sbom-action@") != 2 {
		t.Error("SBOM action must generate exactly one SBOM for each required platform")
	}
	for _, name := range []string{"Generate amd64 SBOM", "Generate arm64 SBOM"} {
		index := stepIndex(steps, name)
		if index < 0 {
			t.Fatalf("missing step %q", name)
		}
		with := mapValue(t, steps[index].(map[string]any), "with")
		if upload, ok := with["upload-release-assets"].(bool); !ok || upload {
			t.Errorf("%s must explicitly disable release-asset uploads", name)
		}
	}
}

// TestReleaseCosignAttestationsAreBounded pins the subject cardinality as well
// as the timeout. An SBOM describes one root filesystem, so each platform SBOM
// must be attested against that platform's own manifest digest; the OpenVEX
// document is platform independent and belongs on the index digest. Regressing
// an SBOM subject back to the index digest is the exact defect #1957 fixed, and
// nothing else in the suite would catch it.
func TestReleaseCosignAttestationsAreBounded(t *testing.T) {
	doc := loadYAML(t, ".github/actions/sbom-and-attest/action.yml")
	steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")
	for _, tc := range []struct {
		name    string
		subject string
	}{
		{name: "Cosign amd64 SBOM attestation", subject: "${{ steps.validate.outputs.amd64_digest }}"},
		{name: "Cosign arm64 SBOM attestation", subject: "${{ steps.validate.outputs.arm64_digest }}"},
		{name: "Cosign OpenVEX attestation", subject: "${{ steps.validate.outputs.image_digest }}"},
	} {
		index := stepIndex(steps, tc.name)
		if index < 0 {
			t.Fatalf("missing step %q", tc.name)
		}
		step := steps[index].(map[string]any)
		run := stringValue(t, step, "run")
		if !strings.Contains(run, "timeout --foreground 120s cosign attest") {
			t.Errorf("%s must bound cosign attest with the shared 120-second timeout", tc.name)
		}
		// The referrers publication path must be an explicit choice here, not
		// whatever the installed cosign happens to default to.
		if !strings.Contains(run, "--new-bundle-format=true") {
			t.Errorf("%s must set --new-bundle-format=true explicitly", tc.name)
		}
		variable := ""
		for key, value := range mapValue(t, step, "env") {
			if fmt.Sprint(value) == tc.subject {
				variable = key
			}
		}
		if variable == "" {
			t.Errorf("%s must bind %s into its environment", tc.name, tc.subject)
			continue
		}
		if reference := "\"${IMAGE_NAME}@${" + variable + "}\""; !strings.Contains(run, reference) {
			t.Errorf("%s must attest %s, want subject reference %s", tc.name, tc.subject, reference)
		}
	}
}

// TestReleasePlatformDigestResolution covers the attest-image-from-tag step
// that turns the pinned index digest into per-platform child manifest digests,
// plus its handoff to sbom-and-attest. Every failure mode has to be fail-closed:
// a release that lost an architecture, or a resolution that yielded the index
// digest, must stop the job rather than ship two SBOMs on one subject.
func TestReleasePlatformDigestResolution(t *testing.T) {
	t.Parallel()
	doc := loadYAML(t, ".github/actions/attest-image-from-tag/action.yml")
	steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")

	resolveIndex := stepIndex(steps, "Resolve per-platform manifest digests")
	if resolveIndex < 0 {
		t.Fatal("attest-image-from-tag must resolve per-platform manifest digests")
	}
	resolve := steps[resolveIndex].(map[string]any)
	if id := stringValue(t, resolve, "id"); id != "platforms" {
		t.Fatalf("platform resolution step id = %q, want platforms", id)
	}
	script := stringValue(t, resolve, "run")
	if !strings.Contains(script, "timeout --foreground 120s crane digest") {
		t.Error("platform digest lookups must use the bounded crane invocation")
	}

	attestIndex := stepIndex(steps, "Generate SBOM and attestations")
	if attestIndex < 0 {
		t.Fatal("attest-image-from-tag must call sbom-and-attest")
	}
	with := mapValue(t, steps[attestIndex].(map[string]any), "with")
	for input, want := range map[string]string{
		"amd64_digest": "${{ steps.platforms.outputs.amd64_digest }}",
		"arm64_digest": "${{ steps.platforms.outputs.arm64_digest }}",
	} {
		if got := fmt.Sprint(with[input]); got != want {
			t.Errorf("sbom-and-attest %s = %q, want %q", input, got, want)
		}
	}

	const indexDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const amd64Digest = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	const arm64Digest = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"

	tests := []struct {
		name    string
		amd64   string
		arm64   string
		wantErr bool
	}{
		{name: "resolves both child manifests", amd64: amd64Digest, arm64: arm64Digest},
		{name: "missing platform fails closed", amd64: amd64Digest, arm64: "MISSING", wantErr: true},
		{name: "amd64 equal to the index fails closed", amd64: indexDigest, arm64: arm64Digest, wantErr: true},
		{name: "arm64 equal to the index fails closed", amd64: amd64Digest, arm64: indexDigest, wantErr: true},
		{name: "malformed digest fails closed", amd64: "sha256:not-a-digest", arm64: arm64Digest, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatalf("create fake bin: %v", err)
			}
			writeExecutable(t, filepath.Join(bin, "crane"), fakePlatformCrane)
			writeExecutable(t, filepath.Join(bin, "timeout"), passthroughTimeout)
			platforms := filepath.Join(dir, "platforms.tsv")
			contents := fmt.Sprintf("linux/amd64\t%s\nlinux/arm64\t%s\n", tc.amd64, tc.arm64)
			if err := os.WriteFile(platforms, []byte(contents), 0o600); err != nil {
				t.Fatalf("write platform table: %v", err)
			}
			output := filepath.Join(dir, "outputs")

			command := exec.Command("bash", "-c", script)
			command.Env = append(os.Environ(),
				"PATH="+bin+":"+os.Getenv("PATH"),
				"FAKE_PLATFORM_DIGESTS="+platforms,
				"IMAGE_NAME=ghcr.io/nvidia/aicr",
				"INDEX_DIGEST="+indexDigest,
				"GITHUB_OUTPUT="+output,
			)
			result, err := command.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolution error = %v, wantErr %t\n%s", err, tc.wantErr, result)
			}
			if tc.wantErr {
				return
			}
			data := string(readFileAt(t, output))
			for _, want := range []string{"amd64_digest=" + tc.amd64, "arm64_digest=" + tc.arm64} {
				if !strings.Contains(data, want) {
					t.Errorf("resolved outputs = %q, want %q", data, want)
				}
			}
		})
	}
}

// TestReleaseSbomAttestInputValidation locks the digest contract sbom-and-attest
// enforces on its own inputs. attest-image-from-tag is the only in-repo caller,
// but the action is reusable, so a direct caller that passes the index digest
// through as a platform digest has to be rejected here rather than silently
// attaching a per-platform SBOM to the multi-platform index.
func TestReleaseSbomAttestInputValidation(t *testing.T) {
	t.Parallel()
	doc := loadYAML(t, ".github/actions/sbom-and-attest/action.yml")
	steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")
	index := stepIndex(steps, "Validate inputs")
	if index < 0 {
		t.Fatal("sbom-and-attest must validate its inputs")
	}
	script := stringValue(t, steps[index].(map[string]any), "run")
	actionPath := filepath.Join(repositoryRoot(t), ".github/actions/sbom-and-attest")

	const indexDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const amd64Digest = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	const arm64Digest = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"

	tests := []struct {
		name    string
		image   string
		digest  string
		amd64   string
		arm64   string
		wantErr bool
	}{
		{name: "distinct platform digests", image: "ghcr.io/nvidia/aicr", digest: indexDigest, amd64: amd64Digest, arm64: arm64Digest},
		{name: "amd64 equals the index digest", image: "ghcr.io/nvidia/aicr", digest: indexDigest, amd64: indexDigest, arm64: arm64Digest, wantErr: true},
		{name: "arm64 equals the index digest", image: "ghcr.io/nvidia/aicr", digest: indexDigest, amd64: amd64Digest, arm64: indexDigest, wantErr: true},
		{name: "platform digests are identical", image: "ghcr.io/nvidia/aicr", digest: indexDigest, amd64: amd64Digest, arm64: amd64Digest, wantErr: true},
		{name: "malformed platform digest", image: "ghcr.io/nvidia/aicr", digest: indexDigest, amd64: "sha256:short", arm64: arm64Digest, wantErr: true},
		{name: "image outside the release set", image: "ghcr.io/attacker/aicr", digest: indexDigest, amd64: amd64Digest, arm64: arm64Digest, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			output := filepath.Join(t.TempDir(), "outputs")
			command := exec.Command("bash", "-c", script)
			command.Env = append(os.Environ(),
				"GITHUB_ACTION_PATH="+actionPath,
				"GITHUB_OUTPUT="+output,
				"INPUT_IMAGE_NAME="+tc.image,
				"INPUT_IMAGE_DIGEST="+tc.digest,
				"INPUT_AMD64_DIGEST="+tc.amd64,
				"INPUT_ARM64_DIGEST="+tc.arm64,
			)
			result, err := command.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validation error = %v, wantErr %t\n%s", err, tc.wantErr, result)
			}
			if tc.wantErr {
				if data, readErr := os.ReadFile(output); readErr == nil && len(data) != 0 {
					t.Errorf("rejected input emitted outputs: %s", data)
				}
				return
			}
			data := string(readFileAt(t, output))
			for _, want := range []string{
				"image_digest=" + tc.digest,
				"amd64_digest=" + tc.amd64,
				"arm64_digest=" + tc.arm64,
			} {
				if !strings.Contains(data, want) {
					t.Errorf("validated outputs = %q, want %q", data, want)
				}
			}
		})
	}
}

// openVEXContext is the OpenVEX namespace the sbom-and-attest guard pins. It is
// asserted against the step's own env binding so a version bump has to move
// both the guard and this test, and with them the enum tables below.
const openVEXContext = "https://openvex.dev/ns/v0.2.0"

// TestReleaseOpenVEXValidation exercises the sbom-and-attest guard that stands
// between `.openvex.json` and a published VEX attestation. The step runs after
// image promotion, so it validates in jq rather than fetching a schema; these
// cases are what stands in for the schema. A document that reaches
// `cosign attest` malformed produces an attestation no scanner can apply, and
// the shapes that pass a naive presence check (an empty `statements` array, a
// `{}` statement, a `not_affected` statement with no reason) are exactly the
// ones that look healthy until a downstream consumer tries to use them.
func TestReleaseOpenVEXValidation(t *testing.T) {
	t.Parallel()
	doc := loadYAML(t, ".github/actions/sbom-and-attest/action.yml")
	steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")
	index := stepIndex(steps, "Verify OpenVEX document")
	if index < 0 {
		t.Fatal("sbom-and-attest must verify the OpenVEX document before attesting it")
	}
	step := steps[index].(map[string]any)
	script := stringValue(t, step, "run")
	if got := fmt.Sprint(mapValue(t, step, "env")["VEX_CONTEXT"]); got != openVEXContext {
		t.Fatalf("VEX_CONTEXT = %q, want %q", got, openVEXContext)
	}

	const validStatement = `{"vulnerability": {"name": "CVE-2026-0001"},
		"products": [{"@id": "pkg:oci/aicr", "identifiers": {"purl": "pkg:oci/aicr"}}],
		"status": "not_affected", "justification": "component_not_present"}`
	document := func(statements string) string {
		return `{"@context": "` + openVEXContext + `", "@id": "https://github.com/NVIDIA/aicr/.openvex.json",
			"author": "NVIDIA AICR maintainers", "timestamp": "2026-08-04T00:00:00Z", "version": 1,
			"statements": [` + statements + `]}`
	}

	tests := []struct {
		name     string
		document string
		wantErr  string
	}{
		{name: "a complete document is accepted", document: document(validStatement)},
		{
			name:     "missing document metadata",
			document: `{"statements": [` + validStatement + `]}`,
			wantErr:  "@id must be a non-empty string",
		},
		{
			name: "downgraded context",
			document: `{"@context": "https://openvex.dev/ns/v0.0.1", "@id": "urn:x", "author": "a",
				"timestamp": "2026-08-04T00:00:00Z", "version": 1, "statements": [` + validStatement + `]}`,
			wantErr: "@context must be " + openVEXContext,
		},
		{
			name:     "non-numeric version",
			document: strings.Replace(document(validStatement), `"version": 1`, `"version": "1"`, 1),
			wantErr:  "version must be a number",
		},
		{name: "empty statements", document: document(""), wantErr: "statements must not be empty"},
		{
			name:     "statement with no fields",
			document: document(`{}`),
			wantErr:  "statement 0 must set vulnerability.name to a non-empty string",
		},
		{
			name:     "statement without products",
			document: document(`{"vulnerability": {"name": "CVE-2026-0001"}, "status": "fixed"}`),
			wantErr:  "statement 0 must list at least one product",
		},
		{
			name: "product without an identifier",
			document: document(`{"vulnerability": {"name": "CVE-2026-0001"}, "products": [{}],
				"status": "fixed"}`),
			wantErr: "statement 0 has a product with neither @id nor identifiers.purl",
		},
		{
			name:     "status outside the enum",
			document: strings.Replace(document(validStatement), `"not_affected"`, `"probably_fine"`, 1),
			wantErr:  "statement 0 status \"probably_fine\" is not one of",
		},
		{
			name: "not_affected without a justification or an impact statement",
			document: document(`{"vulnerability": {"name": "CVE-2026-0001"},
				"products": [{"@id": "pkg:oci/aicr"}], "status": "not_affected"}`),
			wantErr: "statement 0 is not_affected and must carry a justification or an impact_statement",
		},
		{
			name: "not_affected with only an impact statement is accepted",
			document: document(`{"vulnerability": {"name": "CVE-2026-0001"},
				"products": [{"@id": "pkg:oci/aicr"}], "status": "not_affected",
				"impact_statement": "the vulnerable entry point is compiled out"}`),
		},
		{
			name:     "justification outside the enum",
			document: strings.Replace(document(validStatement), `"component_not_present"`, `"seems_unlikely"`, 1),
			wantErr:  "statement 0 justification \"seems_unlikely\" is not one of",
		},
		{
			name: "affected without an action statement",
			document: document(`{"vulnerability": {"name": "CVE-2026-0001"},
				"products": [{"@id": "pkg:oci/aicr"}], "status": "affected"}`),
			wantErr: "statement 0 is affected and must carry an action_statement",
		},
		{
			name:     "statements holding a scalar",
			document: document(`"CVE-2026-0001"`),
			wantErr:  "statement 0 must be a JSON object",
		},
		{name: "document that is not an object", document: `[]`, wantErr: "statements must be an array"},
		{name: "document that is not JSON", document: `{`, wantErr: "not valid JSON"},
		{name: "empty document", document: "", wantErr: "not found or empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			workspace := t.TempDir()
			vex := filepath.Join(workspace, ".openvex.json")
			if err := os.WriteFile(vex, []byte(tc.document), 0o600); err != nil {
				t.Fatalf("write OpenVEX fixture: %v", err)
			}
			output, result := runOpenVEXGuard(t, script, workspace)
			if (result != nil) != (tc.wantErr != "") {
				t.Fatalf("guard error = %v, wantErr %q\n%s", result, tc.wantErr, output)
			}
			if tc.wantErr != "" {
				if !strings.Contains(output, tc.wantErr) {
					t.Errorf("guard output = %q, want it to report %q", output, tc.wantErr)
				}
				return
			}
			if !strings.Contains(output, "file="+vex) {
				t.Errorf("accepted document did not publish the file output: %q", output)
			}
		})
	}

	// The shipped document has to survive the guard it is published through.
	// A release cannot be the first place this is discovered.
	t.Run("the committed .openvex.json is valid", func(t *testing.T) {
		t.Parallel()
		workspace := t.TempDir()
		committed := readFile(t, ".openvex.json")
		if err := os.WriteFile(filepath.Join(workspace, ".openvex.json"), committed, 0o600); err != nil {
			t.Fatalf("stage the committed OpenVEX document: %v", err)
		}
		output, err := runOpenVEXGuard(t, script, workspace)
		if err != nil {
			t.Fatalf(".openvex.json fails the release guard: %v\n%s", err, output)
		}
	})
}

// runOpenVEXGuard executes the extracted guard against a workspace holding a
// candidate `.openvex.json`, returning the combined step output (its ::error::
// annotations and the GITHUB_OUTPUT contents) alongside the exit status.
func runOpenVEXGuard(t *testing.T, script, workspace string) (string, error) {
	t.Helper()
	budget := scriptBudget(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	outputs := filepath.Join(t.TempDir(), "outputs")
	command := exec.CommandContext(ctx, "bash", "-c", script)
	command.Env = append(os.Environ(),
		"GITHUB_WORKSPACE="+workspace,
		"GITHUB_OUTPUT="+outputs,
		"VEX_CONTEXT="+openVEXContext,
	)
	combined, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("OpenVEX guard exceeded %v budget (derived from -timeout): %v\n%s", budget, ctx.Err(), combined)
	}
	written, readErr := os.ReadFile(outputs)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read step outputs: %v", readErr)
	}
	return string(combined) + string(written), err
}

// fakePlatformCrane answers `crane digest --platform <platform> <ref>` from a
// table, mirroring crane's own non-zero exit and "no child with platform"
// message when the index has no such child.
const fakePlatformCrane = `#!/usr/bin/env bash
set -euo pipefail
platform=""
previous=""
for argument in "$@"; do
  if [[ "${previous}" == "--platform" ]]; then platform="${argument}"; fi
  previous="${argument}"
done
while IFS=$'\t' read -r candidate digest; do
  if [[ "${candidate}" == "${platform}" && "${digest}" != "MISSING" ]]; then
    printf '%s\n' "${digest}"
    exit 0
  fi
done < "${FAKE_PLATFORM_DIGESTS}"
echo "Error: no child with platform ${platform} in index" >&2
exit 1
`

// passthroughTimeout runs the wrapped command directly. The bound itself is
// asserted against the step's source text; a real timer here would keep the
// command substitution's pipe open past the test deadline.
const passthroughTimeout = `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "--foreground" ]]; then shift; fi
shift
exec "$@"
`

func TestReleaseAttestationInputValidationEmitsCanonicalDigestMap(t *testing.T) {
	t.Parallel()
	doc := loadYAML(t, ".github/workflows/attest-images.yaml")
	job := mapValue(t, mapValue(t, doc, "jobs"), "validate-inputs")
	steps := sliceValue(t, job, "steps")
	validation := steps[0].(map[string]any)
	script := stringValue(t, validation, "run")
	digestMap := fmt.Sprintf(
		`{"aicr":"sha256:%s","aicrd":"sha256:%s","aiperf-bench":"sha256:%s","conformance":"sha256:%s","deployment":"sha256:%s","gate":"sha256:%s","performance":"sha256:%s"}`,
		strings.Repeat("0", 64),
		strings.Repeat("1", 64),
		strings.Repeat("2", 64),
		strings.Repeat("3", 64),
		strings.Repeat("4", 64),
		strings.Repeat("5", 64),
		strings.Repeat("6", 64),
	)
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "canonical", value: digestMap},
		{name: "non-canonical whitespace", value: strings.Replace(digestMap, `{"aicr"`, `{ "aicr"`, 1), wantErr: true},
		{name: "uppercase digest", value: strings.Replace(digestMap, strings.Repeat("0", 64), strings.Repeat("A", 64), 1), wantErr: true},
		{name: "digest with trailing newline", value: strings.Replace(digestMap, strings.Repeat("0", 64), strings.Repeat("0", 64)+`\n`, 1), wantErr: true},
		{name: "missing image", value: strings.Replace(digestMap, `,"gate":"sha256:`+strings.Repeat("5", 64)+`"`, "", 1), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			output := filepath.Join(t.TempDir(), "outputs")
			command := exec.Command("bash", "-c", script)
			command.Env = append(os.Environ(),
				"GITHUB_OUTPUT="+output,
				"INPUT_CANDIDATE_TAG=candidate-123-4",
				"INPUT_DIGEST_MAP="+tc.value,
			)
			result, err := command.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validation error = %v, wantErr %v\n%s", err, tc.wantErr, result)
			}
			if tc.wantErr {
				if data, readErr := os.ReadFile(output); readErr == nil && len(data) != 0 {
					t.Errorf("rejected digest map emitted outputs: %s", data)
				}
				return
			}
			data := string(readFileAt(t, output))
			if !strings.Contains(data, "digest_map="+digestMap+"\n") {
				t.Errorf("validated digest map output = %q", data)
			}
		})
	}
}

func TestReleaseCompositeValidationUsesSharedLibrary(t *testing.T) {
	helper := filepath.Join(repositoryRoot(t), ".github/actions/release-input-validation.sh")
	info, err := os.Stat(helper)
	if err != nil {
		t.Fatalf("stat shared release input validation: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("shared release input validation mode = %04o, want 0644", info.Mode().Perm())
	}

	for _, path := range []string{
		".github/actions/go-build-release/action.yml",
		".github/actions/attest-image-from-tag/action.yml",
		".github/actions/sbom-and-attest/action.yml",
	} {
		doc := loadYAML(t, path)
		steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")
		run := stringValue(t, steps[0].(map[string]any), "run")
		if !strings.Contains(run, `source "${GITHUB_ACTION_PATH}/../release-input-validation.sh"`) {
			t.Errorf("%s must source the shared release input validation library", path)
		}
		if strings.Contains(run, "reject_newline() {") {
			t.Errorf("%s still defines a local reject_newline helper", path)
		}
		if path != ".github/actions/go-build-release/action.yml" &&
			!strings.Contains(run, "require_release_image") {

			t.Errorf("%s must use the shared release-image allowlist", path)
		}
	}
}

func TestReleaseInputValidationLibrary(t *testing.T) {
	t.Parallel()
	helper := filepath.Join(repositoryRoot(t), ".github/actions/release-input-validation.sh")
	type validationTest struct {
		name    string
		args    []string
		wantErr bool
	}
	tests := make([]validationTest, 0, 3+len(releaseImages))
	tests = append(tests,
		validationTest{name: "single line", args: []string{"reject_newline", "candidate_tag", "candidate-123-4"}},
		validationTest{name: "newline", args: []string{"reject_newline", "candidate_tag", "candidate-123-4\nlatest"}, wantErr: true},
		validationTest{name: "unknown image", args: []string{"require_release_image", "ghcr.io/example/aicr"}, wantErr: true},
	)
	for key, image := range releaseImages {
		tests = append(tests, validationTest{
			name: "release image " + key,
			args: []string{"require_release_image", image},
		})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := make([]string, 0, 4+len(tc.args))
			args = append(args, "-c", `source "$1"; shift; "$@"`, "release-input-validation", helper)
			args = append(args, tc.args...)
			command := exec.Command("bash", args...)
			output, err := command.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("shared validation error = %v, wantErr %v\n%s", err, tc.wantErr, output)
			}
		})
	}
}

func TestReleasePromotionPolicy(t *testing.T) {
	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	jobs := mapValue(t, doc, "jobs")
	promote := mapValue(t, jobs, "promote-images")
	for _, dependency := range []string{"detect", "resolve-candidates", "release-check"} {
		if !jobTransitivelyDependsOn(jobs, "promote-images", dependency) {
			t.Errorf("promote-images must transitively depend on %s", dependency)
		}
	}
	assertConcurrency(t, promote, "aicr-release-image-alias-promotion")
	assertPermissions(t, promote, map[string]string{
		"attestations": "read",
		"contents":     "read",
		"packages":     "write",
	})

	steps := sliceValue(t, promote, "steps")
	preflightIndex := stepIndex(steps, "Preflight image promotion")
	promoteIndex := stepIndex(steps, "Promote image aliases")
	if preflightIndex < 0 || promoteIndex < 0 || preflightIndex >= promoteIndex {
		t.Error("promotion must run an explicit read-only preflight step before alias mutation")
	}
	if !strings.Contains(marshalYAML(t, promote), ".github/scripts/release-images.sh promote") {
		t.Error("promote-images must use the fail-closed promotion script")
	}

	publish := mapValue(t, jobs, "publish")
	if !containsString(stringSlice(publish["needs"]), "promote-images") {
		t.Error("GitHub release publication must wait for image promotion")
	}

	homebrew := mapValue(t, jobs, "publish-homebrew")
	for _, dependency := range []string{"detect", "build-ko", "publish"} {
		if !containsString(stringSlice(homebrew["needs"]), dependency) {
			t.Errorf("publish-homebrew must depend on %s", dependency)
		}
	}
	assertConcurrency(t, homebrew, "aicr-homebrew-publication")
	if !strings.Contains(stringValue(t, homebrew, "if"), "is_prerelease == 'false'") {
		t.Error("Homebrew publication must run only for stable releases")
	}

	wholeWorkflow := marshalYAML(t, doc)
	if strings.Count(wholeWorkflow, "HOMEBREW_DEPLOY_KEY") != 1 {
		t.Error("Homebrew deploy key must be scoped to exactly one post-publication step")
	}
}

func TestReleasePublicAliasWritesExistOnlyInPromotion(t *testing.T) {
	script := string(readFile(t, ".github/scripts/release-images.sh"))
	promoteStart := strings.Index(script, "promote_command() {")
	promoteEnd := strings.Index(script, "\nusage() {")
	if promoteStart < 0 || promoteEnd <= promoteStart {
		t.Fatal("could not isolate promote_command")
	}
	outsidePromotion := script[:promoteStart] + script[promoteEnd:]
	if strings.Contains(outsidePromotion, "crane tag") {
		t.Error("crane tag may appear only inside promote_command")
	}
	if strings.Count(script[promoteStart:promoteEnd], "crane tag") != 2 {
		t.Error("promotion must have exactly the version and latest mutation sites")
	}

	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	jobs := mapValue(t, doc, "jobs")
	for name, raw := range jobs {
		text := marshalYAML(t, raw)
		if name != "promote-images" && strings.Contains(text, "release-images.sh promote") {
			t.Errorf("job %s invokes public-alias promotion", name)
		}
	}
	if !jobTransitivelyDependsOn(jobs, "promote-images", "release-check") {
		t.Error("public-alias mutation must be a descendant of release-check")
	}
}

func TestReleaseHomebrewWorkflowPolicy(t *testing.T) {
	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	jobs := mapValue(t, doc, "jobs")
	homebrew := mapValue(t, jobs, "publish-homebrew")
	assertPermissions(t, homebrew, map[string]string{"actions": "read", "contents": "read"})
	text := marshalYAML(t, homebrew)
	for _, required := range []string{
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		"actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c",
		"name: ${{ needs.build-ko.outputs.homebrew_artifact_name }}",
		"path: /tmp/aicr-homebrew-formula",
		"timeout 30s ssh-keyscan -t rsa github.com",
		"timeout 2m git clone",
		"timeout 2m git -C",
		"push origin HEAD:main",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("Homebrew workflow missing bounded or pinned invariant %q", required)
		}
	}
	if !strings.Contains(text, "exactly aicr.rb") {
		t.Error("Homebrew workflow must reject artifacts other than the exact formula")
	}
	for name, raw := range jobs {
		if name == "publish-homebrew" {
			continue
		}
		if strings.Contains(marshalYAML(t, raw), "HOMEBREW_DEPLOY_KEY") {
			t.Errorf("job %s receives the Homebrew deploy key", name)
		}
	}
}

func TestReleasePublishReverifiesSource(t *testing.T) {
	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	publish := mapValue(t, mapValue(t, doc, "jobs"), "publish")
	steps := sliceValue(t, publish, "steps")
	public := stepIndex(steps, "Publish validated GitHub release")
	if public < 0 {
		t.Fatal("publish must use the validated exact-ID publication step")
	}
	text := marshalYAML(t, steps[public])
	usesPolicyScript := strings.Contains(text, "release-images.sh publish-release")
	hasSourceRevision := strings.Contains(text, "GITHUB_SHA")
	hasReleaseKind := strings.Contains(text, "IS_PRERELEASE")
	if !usesPolicyScript || !hasSourceRevision || !hasReleaseKind {
		t.Error("publication must reverify source, release kind, assets, and exact release ID in the bounded policy script")
	}
	if strings.Contains(marshalYAML(t, publish), "gh release edit") {
		t.Error("publication must PATCH the validated release ID rather than resolve a mutable tag again")
	}
}

func TestReleaseGoReleaserCandidatePolicy(t *testing.T) {
	doc := loadYAML(t, ".goreleaser.yaml")
	release := mapValue(t, doc, "release")
	for _, name := range []string{"draft", "use_existing_draft", "replace_existing_artifacts"} {
		if enabled, ok := release[name].(bool); !ok || !enabled {
			t.Errorf("GoReleaser release.%s must be literal true", name)
		}
	}
	if _, ok := release["replace_existing_draft"]; ok {
		t.Error("GoReleaser must reuse the exact draft instead of deleting and recreating it")
	}
	if stringValue(t, release, "mode") != "replace" {
		t.Error("reused drafts must replace stale release notes with the current generated notes")
	}
	koDefs := sliceValue(t, doc, "kos")
	if len(koDefs) != 2 {
		t.Fatalf("got %d ko definitions, want 2", len(koDefs))
	}
	for index, raw := range koDefs {
		ko := raw.(map[string]any)
		tags := stringSlice(ko["tags"])
		if len(tags) != 1 || tags[0] != "{{ .Env.AICR_CANDIDATE_TAG }}" {
			t.Errorf("ko definition %d tags = %v, want only candidate tag", index, tags)
		}
		labels := mapValue(t, ko, "labels")
		if stringValue(t, labels, "org.opencontainers.image.version") != "{{ .Tag }}" {
			t.Errorf("ko definition %d must set the exact release version label", index)
		}
		if stringValue(t, labels, "org.opencontainers.image.revision") != "{{ .FullCommit }}" {
			t.Errorf("ko definition %d must set the exact source revision label", index)
		}
	}
	for index, raw := range sliceValue(t, doc, "brews") {
		brew := raw.(map[string]any)
		if skip, ok := brew["skip_upload"].(bool); !ok || !skip {
			t.Errorf("brew definition %d must use literal skip_upload: true", index)
		}
	}
	if strings.Contains(marshalYAML(t, doc), "private_key") {
		t.Error("GoReleaser must never receive a Homebrew private key")
	}
}

func TestReleaseBuildCompositeInputs(t *testing.T) {
	doc := loadYAML(t, ".github/actions/go-build-release/action.yml")
	inputs := mapValue(t, doc, "inputs")
	input := mapValue(t, inputs, "candidate_tag")
	if required, ok := input["required"].(bool); !ok || !required {
		t.Error("go-build-release candidate_tag input must be required")
	}
	if _, ok := inputs["homebrew_deploy_key"]; ok {
		t.Error("go-build-release must not accept a Homebrew key")
	}
	steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")
	if stepIndex(steps, "Validate inputs") != 0 {
		t.Error("input validation must be the first composite step")
	}
	target := stepIndex(steps, "Verify release target")
	build := stepIndex(steps, "Build and Release")
	if target < 0 || build < 0 || target+1 != build {
		t.Error("exact-tag release target verification must run immediately before GoReleaser")
	} else if !strings.Contains(marshalYAML(t, steps[target]), "release-images.sh release-target") {
		t.Error("release target verification must use the bounded release policy script")
	}
	if containsDirectRunInput(doc) {
		t.Error("go-build-release run blocks must not interpolate inputs directly")
	}
}

func TestReleaseCompositeValidationRejectsUnsafeInputsBeforeIO(t *testing.T) {
	t.Parallel()
	goBuildValid := map[string]string{
		"INPUT_REGISTRY":            "ghcr.io",
		"INPUT_KO_VERSION":          "v0.19.1",
		"INPUT_GORELEASER_VERSION":  "v2.17.0",
		"INPUT_GO_LICENSES_VERSION": "v2.0.1",
		"INPUT_CANDIDATE_TAG":       "candidate-123-4",
	}
	attestValid := map[string]string{
		"INPUT_IMAGE_NAME":      "ghcr.io/nvidia/aicr",
		"INPUT_CANDIDATE_TAG":   "candidate-123-4",
		"INPUT_EXPECTED_DIGEST": "sha256:" + strings.Repeat("a", 64),
		"INPUT_CRANE_VERSION":   "v0.21.7",
	}
	sbomValid := map[string]string{
		"INPUT_IMAGE_NAME":   "ghcr.io/nvidia/aicr",
		"INPUT_IMAGE_DIGEST": "sha256:" + strings.Repeat("a", 64),
	}
	tests := []struct {
		name      string
		path      string
		base      map[string]string
		overrides map[string]string
	}{
		{name: "empty registry", path: ".github/actions/go-build-release/action.yml", base: goBuildValid, overrides: map[string]string{"INPUT_REGISTRY": ""}},
		{name: "wrong registry", path: ".github/actions/go-build-release/action.yml", base: goBuildValid, overrides: map[string]string{"INPUT_REGISTRY": "registry.example.com"}},
		{name: "newline tool version", path: ".github/actions/go-build-release/action.yml", base: goBuildValid, overrides: map[string]string{"INPUT_KO_VERSION": "v0.19.1\nmalicious"}},
		{name: "floating tool version", path: ".github/actions/go-build-release/action.yml", base: goBuildValid, overrides: map[string]string{"INPUT_GORELEASER_VERSION": "latest"}},
		{name: "public candidate alias", path: ".github/actions/go-build-release/action.yml", base: goBuildValid, overrides: map[string]string{"INPUT_CANDIDATE_TAG": "latest"}},
		{name: "wrong image repository", path: ".github/actions/attest-image-from-tag/action.yml", base: attestValid, overrides: map[string]string{"INPUT_IMAGE_NAME": "ghcr.io/example/aicr"}},
		{name: "newline image", path: ".github/actions/attest-image-from-tag/action.yml", base: attestValid, overrides: map[string]string{"INPUT_IMAGE_NAME": "ghcr.io/nvidia/aicr\nother"}},
		{name: "release tag as candidate", path: ".github/actions/attest-image-from-tag/action.yml", base: attestValid, overrides: map[string]string{"INPUT_CANDIDATE_TAG": "v1.2.3"}},
		{name: "short digest", path: ".github/actions/attest-image-from-tag/action.yml", base: attestValid, overrides: map[string]string{"INPUT_EXPECTED_DIGEST": "sha256:abc"}},
		{name: "unpinned crane", path: ".github/actions/attest-image-from-tag/action.yml", base: attestValid, overrides: map[string]string{"INPUT_CRANE_VERSION": "main"}},
		{name: "SBOM wrong image repository", path: ".github/actions/sbom-and-attest/action.yml", base: sbomValid, overrides: map[string]string{"INPUT_IMAGE_NAME": "ghcr.io/example/aicr"}},
		{name: "SBOM malformed digest", path: ".github/actions/sbom-and-attest/action.yml", base: sbomValid, overrides: map[string]string{"INPUT_IMAGE_DIGEST": "sha256:abc"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := loadYAML(t, tc.path)
			steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")
			validation := steps[0].(map[string]any)
			script := stringValue(t, validation, "run")
			output := filepath.Join(t.TempDir(), "outputs")
			values := make(map[string]string, len(tc.base))
			for key, value := range tc.base {
				values[key] = value
			}
			for key, value := range tc.overrides {
				values[key] = value
			}
			command := exec.Command("bash", "-c", script)
			command.Env = append(os.Environ(),
				"GITHUB_ACTION_PATH="+filepath.Join(repositoryRoot(t), filepath.Dir(tc.path)),
				"GITHUB_OUTPUT="+output,
			)
			for key, value := range values {
				command.Env = append(command.Env, key+"="+value)
			}
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatalf("unsafe input passed validation\n%s", output)
			}
			if data, err := os.ReadFile(output); err == nil && len(data) != 0 {
				t.Errorf("rejected input emitted validated outputs: %s", data)
			}
		})
	}
}

func TestReleaseCompositeLaterStepsUseOnlyValidatedInputs(t *testing.T) {
	for _, path := range []string{
		".github/actions/go-build-release/action.yml",
		".github/actions/attest-image-from-tag/action.yml",
		".github/actions/sbom-and-attest/action.yml",
	} {
		doc := loadYAML(t, path)
		steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")
		if len(steps) < 2 {
			t.Fatalf("%s has no post-validation steps", path)
		}
		for index, step := range steps[1:] {
			if strings.Contains(marshalYAML(t, step), "${{ inputs.") {
				t.Errorf("%s step %d consumes raw input after validation", path, index+1)
			}
		}
	}
}

func TestReleasePackagingConfig(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{path: ".github/workflows/build-attested.yaml", want: "candidate-attested-${{ github.run_id }}-${{ github.run_attempt }}"},
		{path: ".github/workflows/packaging.yaml", want: "candidate-packaging-${{ github.run_id }}-${{ github.run_attempt }}"},
	}
	for _, tc := range cases {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			text := string(readFile(t, tc.path))
			if !strings.Contains(text, "AICR_CANDIDATE_TAG: "+tc.want) {
				t.Errorf("%s must provide its safe run-unique candidate tag", tc.path)
			}
			if strings.Contains(text, "HOMEBREW_DEPLOY_KEY") {
				t.Errorf("%s must not carry an obsolete Homebrew placeholder", tc.path)
			}
		})
	}
}

func TestReleaseArtifactNamesAreRerunSafe(t *testing.T) {
	t.Parallel()
	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	jobs := mapValue(t, doc, "jobs")
	build := mapValue(t, jobs, "build-ko")
	buildText := marshalYAML(t, build)
	if !strings.Contains(buildText, "refusing Homebrew formula outside dist") {
		t.Error("build-ko must confine the staged formula to its dist directory")
	}
	homebrewOutput := stringValue(t, mapValue(t, build, "outputs"), "homebrew_artifact_name")
	expectedHomebrewOutput := "${{ steps.homebrew-artifact.outputs.name }}"
	if homebrewOutput != expectedHomebrewOutput {
		t.Error("build-ko must persist the complete producer-selected Homebrew artifact name")
	}
	steps := sliceValue(t, build, "steps")
	uploadIndex := stepIndex(steps, "Upload Homebrew formula")
	if uploadIndex < 0 {
		t.Fatal("build-ko has no Homebrew formula upload step")
	}
	upload := steps[uploadIndex].(map[string]any)
	if fmt.Sprint(mapValue(t, upload, "with")["retention-days"]) != "30" {
		t.Error("Homebrew formula must remain available for GitHub's full 30-day rerun window")
	}
	nameIndex := stepIndex(steps, "Name Homebrew artifact")
	if nameIndex < 0 {
		t.Fatal("build-ko has no Homebrew artifact naming step")
	}
	nameStep := steps[nameIndex].(map[string]any)
	script := stringValue(t, nameStep, "run")
	render := func(candidate, attempt string) string {
		t.Helper()
		output := filepath.Join(t.TempDir(), "output")
		command := exec.Command("bash", "-c", script)
		command.Env = append(os.Environ(),
			"CANDIDATE_TAG="+candidate,
			"GITHUB_RUN_ATTEMPT="+attempt,
			"GITHUB_OUTPUT="+output,
		)
		if result, err := command.CombinedOutput(); err != nil {
			t.Fatalf("render Homebrew artifact name: %v\n%s", err, result)
		}
		return strings.TrimPrefix(strings.TrimSpace(string(readFileAt(t, output))), "name=")
	}
	attempt1 := render("candidate-123-1", "1")
	attempt2 := render("candidate-123-2", "2")
	if attempt1 != "aicr-homebrew-formula-candidate-123-1-attempt-1" {
		t.Errorf("attempt-1 artifact = %q", attempt1)
	}
	if attempt2 != "aicr-homebrew-formula-candidate-123-2-attempt-2" || attempt1 == attempt2 {
		t.Errorf("full attempt-2 artifact = %q, must differ from attempt 1", attempt2)
	}

	homebrew := mapValue(t, jobs, "publish-homebrew")
	homebrewText := marshalYAML(t, homebrew)
	usesProducerName := strings.Contains(homebrewText, "needs.build-ko.outputs.homebrew_artifact_name")
	reconstructsName := strings.Contains(homebrewText, "name: aicr-homebrew-formula-${{")
	if !usesProducerName || reconstructsName {
		t.Error("attempt-2 downstream must download the persisted attempt-1 producer name")
	}

	workflow := string(readFile(t, ".github/workflows/on-tag.yaml"))
	expectedDigestArtifact := "release-candidate-digests-${{ needs.detect.outputs.candidate_tag }}-" +
		"attempt-${{ github.run_attempt }}"
	if !strings.Contains(workflow, expectedDigestArtifact) {
		t.Error("digest artifact must be unique by candidate and producer run attempt")
	}
	sbom := string(readFile(t, ".github/actions/sbom-and-attest/action.yml"))
	if strings.Count(sbom, "attempt-${{ github.run_attempt }}") != 2 {
		t.Error("each platform SBOM artifact must be unique across rerun attempts")
	}
}

func TestReleaseDocumentationCoversRecoveryLimits(t *testing.T) {
	releasing := string(readFile(t, "RELEASING.md"))
	releasing = strings.Join(strings.Fields(releasing), " ")
	for _, required := range []string{
		"candidate-<run-id>-<run-attempt>",
		"Re-run failed jobs",
		"Re-run all jobs",
		"not transactional",
		"at most one pending run",
		"intentionally retained",
	} {
		if !strings.Contains(releasing, required) {
			t.Errorf("RELEASING.md missing recovery or operational limit %q", required)
		}
	}
	validator := string(readFile(t, "docs/contributor/validator.md"))
	validator = strings.Join(strings.Fields(validator), " ")
	for _, required := range []string{"one authoritative digest map", "cannot be atomic", "read-only preflight"} {
		if !strings.Contains(validator, required) {
			t.Errorf("validator docs missing release invariant %q", required)
		}
	}
}

func TestReleaseRunnableJobsHaveTimeouts(t *testing.T) {
	for _, path := range []string{
		".github/workflows/on-tag.yaml",
		".github/workflows/attest-images.yaml",
		".github/workflows/build-attested.yaml",
		".github/workflows/packaging.yaml",
		".github/workflows/release-reverify.yaml",
	} {
		doc := loadYAML(t, path)
		for name, raw := range mapValue(t, doc, "jobs") {
			job := raw.(map[string]any)
			if _, reusable := job["uses"]; reusable {
				continue
			}
			if _, ok := job["timeout-minutes"]; !ok {
				t.Errorf("%s job %s must have an explicit timeout", path, name)
			}
		}
	}
}

func loadYAML(t *testing.T, relative string) map[string]any {
	t.Helper()
	data := readFile(t, relative)
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse %s: %v", relative, err)
	}
	return document
}

func readFile(t *testing.T, relative string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return data
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
}

func readFileAt(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func mapValue(t *testing.T, value map[string]any, key string) map[string]any {
	t.Helper()
	result, ok := value[key].(map[string]any)
	if !ok {
		t.Fatalf("%s must be a map, got %T", key, value[key])
	}
	return result
}

func sliceValue(t *testing.T, value map[string]any, key string) []any {
	t.Helper()
	result, ok := value[key].([]any)
	if !ok {
		t.Fatalf("%s must be a slice, got %T", key, value[key])
	}
	return result
}

func stringValue(t *testing.T, value map[string]any, key string) string {
	t.Helper()
	result, ok := value[key].(string)
	if !ok {
		t.Fatalf("%s must be a string, got %T", key, value[key])
	}
	return result
}

func stringSlice(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			result = append(result, fmt.Sprint(item))
		}
		return result
	default:
		return nil
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func jobTransitivelyDependsOn(jobs map[string]any, jobName, dependency string) bool {
	visited := make(map[string]bool)
	var visit func(string) bool
	visit = func(current string) bool {
		if visited[current] {
			return false
		}
		visited[current] = true
		raw, ok := jobs[current].(map[string]any)
		if !ok {
			return false
		}
		for _, needed := range stringSlice(raw["needs"]) {
			if needed == dependency || visit(needed) {
				return true
			}
		}
		return false
	}
	return visit(jobName)
}

func marshalYAML(t *testing.T, value any) string {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatalf("marshal YAML projection: %v", err)
	}
	return string(data)
}

func containsDirectRunInput(document map[string]any) bool {
	var visit func(any) bool
	visit = func(value any) bool {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "run" && strings.Contains(fmt.Sprint(child), "${{ inputs.") {
					return true
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(document)
}

func stepIndex(steps []any, name string) int {
	for index, raw := range steps {
		step, ok := raw.(map[string]any)
		if ok && fmt.Sprint(step["name"]) == name {
			return index
		}
	}
	return -1
}

func assertConcurrency(t *testing.T, job map[string]any, group string) {
	t.Helper()
	concurrency := mapValue(t, job, "concurrency")
	if stringValue(t, concurrency, "group") != group {
		t.Errorf("concurrency group must be %s", group)
	}
	if cancel, ok := concurrency["cancel-in-progress"].(bool); !ok || cancel {
		t.Error("concurrency must use cancel-in-progress: false")
	}
}

func assertPermissions(t *testing.T, job map[string]any, want map[string]string) {
	t.Helper()
	permissions := mapValue(t, job, "permissions")
	got := make([]string, 0, len(permissions))
	for key := range permissions {
		got = append(got, key)
	}
	sort.Strings(got)
	wantKeys := make([]string, 0, len(want))
	for key := range want {
		wantKeys = append(wantKeys, key)
	}
	sort.Strings(wantKeys)
	if strings.Join(got, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("permissions keys = %v, want %v", got, wantKeys)
	}
	for key, expected := range want {
		if fmt.Sprint(permissions[key]) != expected {
			t.Errorf("permission %s = %v, want %s", key, permissions[key], expected)
		}
	}
}
