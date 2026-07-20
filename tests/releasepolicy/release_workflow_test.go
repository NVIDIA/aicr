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

func TestReleaseCosignAttestationsAreBounded(t *testing.T) {
	doc := loadYAML(t, ".github/actions/sbom-and-attest/action.yml")
	steps := sliceValue(t, mapValue(t, doc, "runs"), "steps")
	for _, name := range []string{
		"Cosign amd64 SBOM attestation",
		"Cosign arm64 SBOM attestation",
	} {
		index := stepIndex(steps, name)
		if index < 0 {
			t.Fatalf("missing step %q", name)
		}
		run := stringValue(t, steps[index].(map[string]any), "run")
		if !strings.Contains(run, "timeout --foreground 120s cosign attest") {
			t.Errorf("%s must bound cosign attest with the shared 120-second timeout", name)
		}
	}
}

func TestReleaseAttestationInputValidationEmitsCanonicalDigestMap(t *testing.T) {
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
		"actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0",
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
