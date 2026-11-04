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

package releasepolicy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// jobUses reports the steps of job that use the local action at path.
func jobUses(t *testing.T, job map[string]any, path string) []map[string]any {
	t.Helper()
	var out []map[string]any
	steps, _ := job["steps"].([]any)
	for _, s := range steps {
		step, ok := s.(map[string]any)
		if ok && fmt.Sprint(step["uses"]) == path {
			out = append(out, step)
		}
	}
	return out
}

// A release job that configures the DGXC Go proxy without a static token
// mints its credential through GitHub OIDC, so it needs id-token: write; a
// job-level permissions block withholds every scope it does not list.
func TestReleaseGoProxyJobsCanMintOIDC(t *testing.T) {
	t.Parallel()
	jobs := mapValue(t, loadYAML(t, ".github/workflows/on-tag.yaml"), "jobs")
	checked := 0
	for name, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, step := range jobUses(t, job, "./.github/actions/setup-dgxc-goproxy") {
			with, _ := step["with"].(map[string]any)
			if _, static := with["token"]; static {
				continue
			}
			checked++
			permissions, _ := job["permissions"].(map[string]any)
			if fmt.Sprint(permissions["id-token"]) != "write" {
				t.Errorf("on-tag.yaml job %s configures the Go proxy through OIDC but lacks id-token: write", name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no on-tag.yaml job configures the Go proxy; the policy checks nothing")
	}
}

// A release job that runs a kind cluster runs third-party images on
// privileged nodes, so it must not hold the release's OIDC identity or the
// Artifactory credential the Go proxy mints.
func TestReleaseKindJobsHoldNoReleaseIdentity(t *testing.T) {
	t.Parallel()
	jobs := mapValue(t, loadYAML(t, ".github/workflows/on-tag.yaml"), "jobs")
	checked := 0
	for name, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok || len(jobUses(t, job, "./.github/actions/operator-kind")) == 0 {
			continue
		}
		checked++
		permissions, _ := job["permissions"].(map[string]any)
		if fmt.Sprint(permissions["id-token"]) == "write" {
			t.Errorf("on-tag.yaml job %s runs kind with id-token: write", name)
		}
		if len(jobUses(t, job, "./.github/actions/setup-dgxc-goproxy")) > 0 {
			t.Errorf("on-tag.yaml job %s runs kind with the Go proxy's credential", name)
		}
	}
	if checked == 0 {
		t.Fatal("no on-tag.yaml job runs kind; the policy checks nothing")
	}
}

// operator-publish's digest record is exactly what attest-operator.yaml's
// input validation accepts, so the two cannot drift apart unnoticed until a
// release has already pushed its tags.
func TestOperatorPublishDigestsPassAttestValidation(t *testing.T) {
	t.Parallel()
	publish := mapValue(t, mapValue(t, loadYAML(t, ".github/workflows/on-tag.yaml"), "jobs"), "operator-publish")
	steps := sliceValue(t, publish, "steps")
	record := stepIndex(steps, "Record the published digests")
	if record < 0 {
		t.Fatal("operator-publish must record the published digests")
	}
	recordStep, ok := steps[record].(map[string]any)
	if !ok {
		t.Fatal("the digest record step is not a map")
	}
	validate := mapValue(t, mapValue(t, loadYAML(t, ".github/workflows/attest-operator.yaml"), "jobs"), "validate-inputs")
	validateStep, ok := sliceValue(t, validate, "steps")[0].(map[string]any)
	if !ok {
		t.Fatal("attest-operator.yaml's validation step is not a map")
	}

	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	work := t.TempDir()
	candidate := fmt.Sprintf(`{"operator":{"platforms":{"linux/amd64":%q,"linux/arm64/v8":%q}},`+
		`"registry":{"platforms":{"linux/amd64":%q,"linux/arm64/v8":%q}}}`, digest("1"), digest("2"), digest("4"), digest("5"))
	if err := os.MkdirAll(filepath.Join(work, "dist", "candidate"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "dist", "candidate", "candidate.json"), []byte(candidate), 0o600); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(work, "record-output")
	cmd := exec.Command("bash", "-c", stringValue(t, recordStep, "run"))
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+recorded,
		"IMAGE_DIGEST="+digest("0"), "REGISTRY_DIGEST="+digest("3"), "CHART_DIGEST="+digest("6"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("record the published digests: %v\n%s", err, out)
	}
	digests := strings.TrimSpace(strings.TrimPrefix(string(readFileAt(t, recorded)), "digests="))

	validated := filepath.Join(work, "validate-output")
	cmd = exec.Command("bash", "-c", stringValue(t, validateStep, "run"))
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+validated, "INPUT_DIGESTS="+digests,
		"INPUT_PLUGIN_ARTIFACT=aicr-operator-candidate-candidate-123-1-attempt-1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("attest-operator.yaml rejects operator-publish's digests %s: %v\n%s", digests, err, out)
	}
	if !strings.Contains(string(readFileAt(t, validated)), "digests="+digests+"\n") {
		t.Errorf("attest-operator.yaml did not pass the digests through: %s", readFileAt(t, validated))
	}
}

// The weekly scan reads each operator image once per architecture, as the
// release's operator-scan does, and keeps the two results apart, so a finding
// in either platform's image reaches the run's artifacts and the summary.
func TestWeeklyScanCoversOperatorImagesPerArch(t *testing.T) {
	t.Parallel()
	scan := mapValue(t, mapValue(t, loadYAML(t, ".github/workflows/vuln-scan-images.yaml"), "jobs"), "scan")
	matrix := mapValue(t, mapValue(t, scan, "strategy"), "matrix")
	images := sliceValue(t, matrix, "image")
	include := sliceValue(t, matrix, "include")
	for _, image := range []string{"ghcr.io/nvidia/aicr-operator", "ghcr.io/nvidia/aicr-registry"} {
		for _, raw := range images {
			if fmt.Sprint(raw) == image {
				t.Errorf("%s has a leg with no architecture; list it per architecture under include", image)
			}
		}
		var arches []string
		for _, raw := range include {
			leg, ok := raw.(map[string]any)
			if ok && fmt.Sprint(leg["image"]) == image {
				arches = append(arches, fmt.Sprint(leg["arch"]))
			}
		}
		slices.Sort(arches)
		if !slices.Equal(arches, []string{"amd64", "arm64"}) {
			t.Errorf("%s is scanned on %v, want [amd64 arm64]", image, arches)
		}
	}

	steps := sliceValue(t, scan, "steps")
	scanStep, ok := steps[stepIndex(steps, "Scan container image")].(map[string]any)
	if !ok {
		t.Fatal("the scan step is not a map")
	}
	env, _ := scanStep["env"].(map[string]any)
	if !strings.Contains(fmt.Sprint(env["GRYPE_PLATFORM"]), "matrix.arch") {
		t.Errorf("the scan step must pass matrix.arch to grype as GRYPE_PLATFORM, got %q", env["GRYPE_PLATFORM"])
	}
	with, _ := scanStep["with"].(map[string]any)
	if !strings.Contains(fmt.Sprint(with["image"]), "matrix.arch") {
		t.Errorf("an architecture's leg must read the image from the registry, which selects the platform; image input is %q", with["image"])
	}

	extract, ok := steps[stepIndex(steps, "Extract severity counts and IDs")].(map[string]any)
	if !ok {
		t.Fatal("the extract step is not a map")
	}
	work := t.TempDir()
	grypeJSON := filepath.Join(work, "grype.json")
	if err := os.WriteFile(grypeJSON, []byte(`{"matches":[{"artifact":{"name":"stdlib"},"vulnerability":{"id":"CVE-0000-0001","severity":"High"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(stringValue(t, extract, "run"), "${{ steps.scan.outputs.json }}", grypeJSON)
	for _, arch := range []string{"amd64", "arm64"} {
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "IMAGE=ghcr.io/nvidia/aicr-operator", "ARCH="+arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("extract for %s: %v\n%s", arch, err, out)
		}
	}
	results, err := filepath.Glob(filepath.Join(work, "scan-results", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	headers := make([]string, 0, len(results))
	for _, f := range results {
		headers = append(headers, strings.SplitN(string(readFileAt(t, f)), ":", 2)[0])
	}
	slices.Sort(headers)
	if want := []string{"aicr-operator amd64", "aicr-operator arm64"}; !slices.Equal(headers, want) {
		t.Errorf("per-architecture results are %v, want %v", headers, want)
	}
}

// The weekly scan's cleanup deletes this run's scan tags from every GHCR
// package the workflow pushes. A package's name is its path under
// ghcr.io/nvidia, so a name with an "nvidia/" prefix matches no package, and
// the step's error guards would hide the miss while scan tags accumulate.
func TestWeeklyScanCleanupNamesPushedPackages(t *testing.T) {
	t.Parallel()
	jobs := mapValue(t, loadYAML(t, ".github/workflows/vuln-scan-images.yaml"), "jobs")
	matrix := mapValue(t, mapValue(t, mapValue(t, jobs, "scan"), "strategy"), "matrix")
	pinned := map[string]bool{}
	include, listed := sliceValue(t, matrix, "include"), sliceValue(t, matrix, "image")
	images := make([]string, 0, len(include)+len(listed))
	for _, raw := range include {
		leg, _ := raw.(map[string]any)
		if _, ok := leg["tag"]; ok {
			pinned[fmt.Sprint(leg["image"])] = true
		}
		images = append(images, fmt.Sprint(leg["image"]))
	}
	for _, raw := range listed {
		images = append(images, fmt.Sprint(raw))
	}
	var want []string
	for _, image := range images {
		name, ok := strings.CutPrefix(image, "ghcr.io/nvidia/")
		if pkg := strings.ReplaceAll(name, "/", "%2F"); ok && !pinned[image] && !slices.Contains(want, pkg) {
			want = append(want, pkg)
		}
	}
	slices.Sort(want)

	steps := sliceValue(t, mapValue(t, jobs, "cleanup"), "steps")
	cleanup, ok := steps[stepIndex(steps, "Delete scan images")].(map[string]any)
	if !ok {
		t.Fatal("the cleanup step is not a map")
	}
	work := t.TempDir()
	log := filepath.Join(work, "gh.log")
	if err := os.WriteFile(filepath.Join(work, "gh"), []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> \"${GH_LOG}\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", stringValue(t, cleanup, "run"))
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "PATH="+work+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_LOG="+log, "SCAN_TAG=scan-test", "SHA=test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup: %v\n%s", err, out)
	}
	matches := regexp.MustCompile(`orgs/NVIDIA/packages/container/([^/ ]+)/versions\?`).FindAllStringSubmatch(string(readFileAt(t, log)), -1)
	got := make([]string, 0, len(matches))
	for _, m := range matches {
		got = append(got, m[1])
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("cleanup lists versions of packages %v, want the packages the workflow pushes %v", got, want)
	}
}
