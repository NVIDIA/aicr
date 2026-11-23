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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"
)

// pinRef is an image of a release in ghcr.io/nvidia with a digest of digit.
func pinRef(repo, version, digit string) string {
	return "ghcr.io/nvidia/" + repo + ":" + version + "@sha256:" + strings.Repeat(digit, 64)
}

// testPins pins release v1.3.0 and tests v1.0.0 and v1.3.0.
func testPins() *pins {
	validators := func(version, digit string) map[string]string {
		return map[string]string{
			"deployment":  pinRef("aicr-validators/deployment", version, digit),
			"conformance": pinRef("aicr-validators/conformance", version, digit),
			"performance": pinRef("aicr-validators/performance", version, digit),
		}
	}
	joined := func(m map[string]string) string {
		return m["deployment"] + "," + m["conformance"] + "," + m["performance"]
	}
	p := &pins{}
	p.Operator.AICRVersion = "v1.3.0"
	p.Operator.AICRImage = pinRef("aicr", "v1.3.0", "b")
	p.Operator.AICRValidatorImages = validators("v1.3.0", "b")
	p.Operator.AICRE2E = []e2eRelease{
		{Version: "v1.0.0", Image: pinRef("aicr", "v1.0.0", "a"), ValidatorImages: joined(validators("v1.0.0", "a"))},
		{Version: "v1.3.0", Image: pinRef("aicr", "v1.3.0", "b"), ValidatorImages: joined(validators("v1.3.0", "b"))},
	}
	return p
}

// Each way the pins can disagree is refused, and the error names the field.
// Releases may pin different validators.
func TestPinsCheck(t *testing.T) {
	t.Parallel()
	if err := testPins().check(); err != nil {
		t.Fatalf("check() of agreeing pins = %v", err)
	}
	fewer := testPins()
	e := &fewer.Operator.AICRE2E[0]
	e.ValidatorImages = e.ValidatorImages[:strings.LastIndexByte(e.ValidatorImages, ',')]
	if err := fewer.check(); err != nil {
		t.Errorf("check() of an older release with fewer validators = %v", err)
	}

	retag := func(ref, from, to string) string { return strings.Replace(ref, ":"+from+"@", ":"+to+"@", 1) }
	retagPinned := func(p *pins, from, to string) {
		p.Operator.AICRVersion = to
		p.Operator.AICRImage = retag(p.Operator.AICRImage, from, to)
		for k, v := range p.Operator.AICRValidatorImages {
			p.Operator.AICRValidatorImages[k] = retag(v, from, to)
		}
		e := &p.Operator.AICRE2E[1]
		e.Version, e.Image = to, retag(e.Image, from, to)
		e.ValidatorImages = strings.ReplaceAll(e.ValidatorImages, ":"+from+"@", ":"+to+"@")
	}
	redigest := func(ref string) string { return ref[:strings.LastIndexByte(ref, ':')+1] + strings.Repeat("c", 64) }
	tests := map[string]struct {
		edit func(*pins)
		want string
	}{
		"no pinned version":   {func(p *pins) { p.Operator.AICRVersion = "" }, "operator.aicr_version is empty"},
		"no pinned image":     {func(p *pins) { p.Operator.AICRImage = "" }, "operator.aicr_image is empty"},
		"no pinned validator": {func(p *pins) { p.Operator.AICRValidatorImages = nil }, "operator.aicr_validator_images is empty"},
		"image tagged with another release": {func(p *pins) { p.Operator.AICRImage = retag(p.Operator.AICRImage, "v1.3.0", "v1.2.0") },
			`operator.aicr_*: AICR image "ghcr.io/nvidia/aicr:v1.2.0`},
		"validator tagged with another release": {func(p *pins) {
			p.Operator.AICRValidatorImages["conformance"] = retag(p.Operator.AICRValidatorImages["conformance"], "v1.3.0", "v1.2.0")
		}, `operator.aicr_*: AICR image "ghcr.io/nvidia/aicr-validators/conformance:v1.2.0`},
		"a validator pinned as the aicr image": {func(p *pins) { p.Operator.AICRImage = p.Operator.AICRValidatorImages["deployment"] },
			"operator.aicr_*: " + pinRef("aicr-validators/deployment", "v1.3.0", "b") + " is not an aicr image"},
		"validator pinned under another name": {func(p *pins) {
			v := p.Operator.AICRValidatorImages
			v["deployment"], v["conformance"] = v["conformance"], v["deployment"]
		}, "operator.aicr_validator_images.conformance pins"},
		"validator pinned twice": {func(p *pins) {
			p.Operator.AICRValidatorImages["extra"] = redigest(p.Operator.AICRValidatorImages["deployment"])
		}, "operator.aicr_* pins the deployment validator twice"},
		"pinned version not vX.Y.Z": {func(p *pins) { p.Operator.AICRVersion = "1.3.0" }, `operator.aicr_*: version "1.3.0" is not vX.Y.Z`},
		"a prerelease pinned":       {func(p *pins) { retagPinned(p, "v1.3.0", "v1.3.0-rc.1") }, "operator.aicr_*: v1.3.0-rc.1 is a prerelease"},
		"a release the operator does not support": {func(p *pins) { retagPinned(p, "v1.3.0", "v2.0.0") },
			"AICR version v2.0.0 is outside"},
		"pinned release not tested":         {func(p *pins) { p.Operator.AICRE2E = p.Operator.AICRE2E[:1] }, "operator.aicr_e2e lacks v1.3.0"},
		"tested release without a version":  {func(p *pins) { p.Operator.AICRE2E[0].Version = "" }, "operator.aicr_e2e[0].version is empty"},
		"tested release without an image":   {func(p *pins) { p.Operator.AICRE2E[0].Image = "" }, "operator.aicr_e2e[0].image is empty"},
		"tested release without validators": {func(p *pins) { p.Operator.AICRE2E[0].ValidatorImages = "" }, "operator.aicr_e2e[0].validator_images is empty"},
		"tested image tagged with another release": {func(p *pins) {
			p.Operator.AICRE2E[0].Image = retag(p.Operator.AICRE2E[0].Image, "v1.0.0", "v1.0.1")
		}, `operator.aicr_e2e[0]: AICR image "ghcr.io/nvidia/aicr:v1.0.1`},
		"tested image differs": {func(p *pins) { p.Operator.AICRE2E[1].Image = redigest(p.Operator.AICRE2E[1].Image) },
			"operator.aicr_e2e[1] pins other v1.3.0 images"},
		"tested validator differs": {func(p *pins) { e := &p.Operator.AICRE2E[1]; e.ValidatorImages = redigest(e.ValidatorImages) },
			"operator.aicr_e2e[1] pins other v1.3.0 images"},
		"tested validator twice": {func(p *pins) {
			p.Operator.AICRE2E[0].ValidatorImages += "," + redigest(pinRef("aicr-validators/deployment", "v1.0.0", "a"))
		}, "operator.aicr_e2e[0] pins the deployment validator twice"},
		"a tested prerelease": {func(p *pins) {
			e := &p.Operator.AICRE2E[0]
			e.Version, e.Image = "v1.0.0-rc.1", retag(e.Image, "v1.0.0", "v1.0.0-rc.1")
			e.ValidatorImages = strings.ReplaceAll(e.ValidatorImages, ":v1.0.0@", ":v1.0.0-rc.1@")
		}, "operator.aicr_e2e[0]: v1.0.0-rc.1 is a prerelease"},
		"a release tested twice": {func(p *pins) { p.Operator.AICRE2E = append(p.Operator.AICRE2E, p.Operator.AICRE2E[0]) },
			"operator.aicr_e2e[2] repeats v1.0.0"},
	}
	for name, tt := range tests {
		p := testPins()
		tt.edit(p)
		if err := p.check(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: check() = %v, want an error containing %q", name, err, tt.want)
		}
	}
}

// The repository's own pins agree.
func TestRepositoryPinsAgree(t *testing.T) {
	t.Parallel()
	p, err := readPins(filepath.Join("..", "..", ".settings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.check(); err != nil {
		t.Fatalf("the operator's AICR pins in .settings.yaml disagree: %v", err)
	}
}

// fakeNetwork answers verify as GitHub and the registry would for testPins:
// both releases published, every version tag at its pinned digest, every
// image attested. Its fields change one answer.
type fakeNetwork struct {
	mu    sync.Mutex
	calls []string
	// draft is a release that is a draft.
	draft string
	// moved is a version tag that resolves to another digest.
	moved string
	// unattested is an image repository with no attestation.
	unattested string
	// hang makes every call wait for its deadline.
	hang bool
}

func (f *fakeNetwork) record(t *testing.T, ctx context.Context, call string) error {
	t.Helper()
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > verifyTimeout {
		t.Errorf("%s ran without a deadline within %s", call, verifyTimeout)
	}
	if f.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeNetwork) verifier(t *testing.T) verifier {
	return verifier{
		gh: func(ctx context.Context, args ...string) ([]byte, error) {
			if err := f.record(t, ctx, "gh "+strings.Join(args, " ")); err != nil {
				return nil, err
			}
			switch {
			case args[0] == "release" && args[2] == f.draft:
				return []byte("true\n"), nil
			case args[0] == "release":
				return []byte("false\n"), nil
			case f.unattested != "" && strings.Contains(args[2], f.unattested):
				return nil, errors.New("no matching attestations")
			}
			return nil, nil
		},
		digest: func(ctx context.Context, ref string) (string, error) {
			if err := f.record(t, ctx, "digest "+ref); err != nil {
				return "", err
			}
			digit := "b"
			if strings.HasSuffix(ref, ":v1.0.0") {
				digit = "a"
			}
			if ref == f.moved {
				digit = "c"
			}
			return "sha256:" + strings.Repeat(digit, 64), nil
		},
	}
}

// Each pinned release must be published, and each distinct pinned image is
// resolved by its version tag and verified once, as built by
// attest-images.yaml from its release's tag on a GitHub-hosted runner.
func TestPinsVerify(t *testing.T) {
	t.Parallel()
	f := &fakeNetwork{}
	var out bytes.Buffer
	if err := testPins().verify(context.Background(), f.verifier(t), &out); err != nil {
		t.Fatalf("verify() = %v", err)
	}
	want := make([]string, 0, 18)
	want = append(want,
		"gh release view v1.0.0 --repo NVIDIA/aicr --json isDraft --jq .isDraft",
		"gh release view v1.3.0 --repo NVIDIA/aicr --json isDraft --jq .isDraft")
	for version, digit := range map[string]string{"v1.0.0": "a", "v1.3.0": "b"} {
		for _, repo := range []string{"aicr", "aicr-validators/deployment", "aicr-validators/conformance", "aicr-validators/performance"} {
			want = append(want, "digest ghcr.io/nvidia/"+repo+":"+version,
				"gh attestation verify oci://ghcr.io/nvidia/"+repo+"@sha256:"+strings.Repeat(digit, 64)+
					" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml"+
					" --source-ref refs/tags/"+version+" --deny-self-hosted-runners")
		}
	}
	slices.Sort(want)
	if got := slices.Sorted(slices.Values(f.calls)); !slices.Equal(got, want) {
		t.Errorf("verify() calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := strings.Count(out.String(), "verified "); n != 8 {
		t.Errorf("verify() reported %d images, want 8:\n%s", n, out.String())
	}

	for name, tt := range map[string]struct {
		f    *fakeNetwork
		want string
	}{
		"a draft release": {&fakeNetwork{draft: "v1.3.0"}, "AICR v1.3.0 is not a published release"},
		"a tag at another digest": {&fakeNetwork{moved: "ghcr.io/nvidia/aicr-validators/performance:v1.0.0"},
			"ghcr.io/nvidia/aicr-validators/performance:v1.0.0 is sha256:cccc"},
		"no attestation": {&fakeNetwork{unattested: "aicr-validators/conformance"},
			"verify the provenance of ghcr.io/nvidia/aicr-validators/conformance:v1.0.0"},
	} {
		if err := testPins().verify(context.Background(), tt.f.verifier(t), &out); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: verify() = %v, want an error containing %q", name, err, tt.want)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := testPins().verify(ctx, (&fakeNetwork{hang: true}).verifier(t), &out); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("verify() of a call that ran out of time = %v, want it reported as timed out", err)
	}
}

// runGH returns gh's standard output, or its standard error with the
// failure.
func TestRunGH(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\nif [ \"$1\" = fail ]; then echo 'no matching attestations' >&2; exit 1; fi\necho \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := runGH(context.Background(), "release", "view"); err != nil || string(out) != "release view\n" {
		t.Errorf("runGH() = %q, %v", out, err)
	}
	if _, err := runGH(context.Background(), "fail", "now"); err == nil || !strings.Contains(err.Error(), "no matching attestations") {
		t.Errorf("runGH() of a failing gh = %v, want gh's message", err)
	}
}

// The pins command checks a settings file and, with -attestations, verifies
// what it pins.
func TestRunPins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, raw []byte) string {
		t.Helper()
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}
	marshal := func(p *pins) []byte {
		t.Helper()
		raw, err := yaml.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	file := write("settings.yaml", marshal(testPins()))
	var out bytes.Buffer
	if err := run([]string{"pins", "-settings", file}, &out); err != nil || !strings.Contains(out.String(), "v1.3.0 pinned; v1.0.0, v1.3.0 tested") {
		t.Errorf("run(pins) = %v, printed %q", err, out.String())
	}
	out.Reset()
	if err := pinsCmd([]string{"-settings", file, "-attestations"}, &out, (&fakeNetwork{}).verifier(t)); err != nil || strings.Count(out.String(), "verified ") != 8 {
		t.Errorf("pins -attestations = %v, printed %q", err, out.String())
	}
	if err := pinsCmd([]string{"-settings", file, "-attestations"}, &out, (&fakeNetwork{draft: "v1.0.0"}).verifier(t)); err == nil {
		t.Error("pins -attestations accepted a draft release")
	}
	bad := testPins()
	bad.Operator.AICRE2E = bad.Operator.AICRE2E[:1]
	for name, args := range map[string][]string{
		"pins that disagree": {"pins", "-settings", write("bad.yaml", marshal(bad))},
		"an unknown flag":    {"pins", "-settings", file, "-attestation"},
		"no settings file":   {"pins", "-settings", filepath.Join(dir, "missing.yaml")},
		"malformed settings": {"pins", "-settings", write("malformed.yaml", []byte("operator: ["))},
	} {
		if err := run(args, &out); err == nil {
			t.Errorf("run(%s) succeeded", name)
		}
	}
}
