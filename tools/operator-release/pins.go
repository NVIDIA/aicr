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
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.yaml.in/yaml/v4"
	"golang.org/x/mod/semver"

	"aicr.run/pkg/operator/aicrjob"
)

// AICR's release images are attested by attest-images.yaml, run from the
// release's tag in this repository.
const (
	aicrRepository     = "NVIDIA/aicr"
	aicrSignerWorkflow = aicrRepository + "/.github/workflows/attest-images.yaml"
)

// verifyTimeout bounds each network call verify makes.
const verifyTimeout = time.Minute

// pins are the operator's AICR pins in .settings.yaml: the release the
// operator is built and qualified against, and the releases Operator E2E
// runs.
type pins struct {
	Operator struct {
		AICRVersion         string            `yaml:"aicr_version"`
		AICRImage           string            `yaml:"aicr_image"`
		AICRValidatorImages map[string]string `yaml:"aicr_validator_images"`
		AICRE2E             []e2eRelease      `yaml:"aicr_e2e"`
	} `yaml:"operator"`
}

// e2eRelease is one AICR release Operator E2E runs.
type e2eRelease struct {
	Version         string `yaml:"version"`
	Image           string `yaml:"image"`
	ValidatorImages string `yaml:"validator_images"`
}

func readPins(file string) (*pins, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	p := &pins{}
	if err = yaml.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	return p, nil
}

// check reports the first way the pins disagree, naming the field: an empty
// field; a version that is not a supported vX.Y.Z release; an image not
// pinned by digest, not tagged with its release, or not an aicr image where
// one is pinned; a validator pinned twice or under another name; or an
// Operator E2E matrix that does not run the pinned release with exactly its
// images. Releases may pin different validators.
func (p *pins) check() error {
	o := &p.Operator
	switch {
	case o.AICRVersion == "":
		return errors.New("operator.aicr_version is empty")
	case o.AICRImage == "":
		return errors.New("operator.aicr_image is empty")
	case len(o.AICRValidatorImages) == 0:
		return errors.New("operator.aicr_validator_images is empty")
	}
	for _, key := range slices.Sorted(maps.Keys(o.AICRValidatorImages)) {
		if ref := o.AICRValidatorImages[key]; !pinnedRef.MatchString(ref) {
			return fmt.Errorf("operator.aicr_validator_images.%s %q is not one image, repository:tag@sha256:<digest>", key, ref)
		}
	}
	pinned, err := release("operator.aicr_*", o.AICRVersion, o.AICRImage, slices.Collect(maps.Values(o.AICRValidatorImages)))
	if err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(o.AICRValidatorImages)) {
		if got := validatorName(o.AICRValidatorImages[key]); got != key {
			return fmt.Errorf("operator.aicr_validator_images.%s pins %s, the %s validator", key, o.AICRValidatorImages[key], got)
		}
	}
	tested := map[string]bool{}
	for i, e := range o.AICRE2E {
		field := fmt.Sprintf("operator.aicr_e2e[%d]", i)
		switch {
		case e.Version == "":
			return fmt.Errorf("%s.version is empty", field)
		case e.Image == "":
			return fmt.Errorf("%s.image is empty", field)
		case e.ValidatorImages == "":
			return fmt.Errorf("%s.validator_images is empty", field)
		}
		rel, relErr := release(field, e.Version, e.Image, strings.Split(e.ValidatorImages, ","))
		if relErr != nil {
			return relErr
		}
		if tested[rel.Version] {
			return fmt.Errorf("%s repeats %s", field, rel.Version)
		}
		tested[rel.Version] = true
		if rel.Version == pinned.Version && (rel.Image != pinned.Image || !slices.Equal(rel.ValidatorImages, pinned.ValidatorImages)) {
			return fmt.Errorf("%s pins other %s images than operator.aicr_image and aicr_validator_images", field, rel.Version)
		}
	}
	if !tested[pinned.Version] {
		return fmt.Errorf("operator.aicr_e2e lacks %s, the release operator.aicr_version pins", pinned.Version)
	}
	return nil
}

// release checks one pinned AICR release; field names it in errors.
func release(field, version, image string, validators []string) (AICR, error) {
	if !semver.IsValid(version) || semver.Canonical(version) != version {
		return AICR{}, fmt.Errorf("%s: version %q is not vX.Y.Z", field, version)
	}
	if semver.Prerelease(version) != "" {
		return AICR{}, fmt.Errorf("%s: %s is a prerelease; the operator pins and tests releases", field, version)
	}
	if err := aicrjob.CheckSupportedVersion(version); err != nil {
		return AICR{}, fmt.Errorf("%s: %w", field, err)
	}
	rel, err := aicrFlags(version, image, strings.Join(validators, ","))
	if err != nil {
		return AICR{}, fmt.Errorf("%s: %w", field, err)
	}
	if path.Base(imageRepository(rel.Image)) != "aicr" {
		return AICR{}, fmt.Errorf("%s: %s is not an aicr image", field, rel.Image)
	}
	names := validatorNames(rel.ValidatorImages)
	for i := 1; i < len(names); i++ {
		if names[i] == names[i-1] {
			return AICR{}, fmt.Errorf("%s pins the %s validator twice", field, names[i])
		}
	}
	return rel, nil
}

// imageRepository is an image reference's repository, without tag or digest.
// The reference must match pinnedRef.
func imageRepository(ref string) string {
	named := ref[:strings.IndexByte(ref, '@')]
	return named[:strings.LastIndexByte(named, ':')]
}

// validatorName is the validator an image reference names: the last segment
// of its repository.
func validatorName(ref string) string {
	return path.Base(imageRepository(ref))
}

func validatorNames(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, validatorName(ref))
	}
	slices.Sort(out)
	return out
}

// pinnedImage is a pinned image and the release whose tag built it.
type pinnedImage struct {
	ref, version string
}

// images lists each distinct pinned image, sorted by reference.
func (p *pins) images() []pinnedImage {
	o := &p.Operator
	byRef := map[string]string{o.AICRImage: o.AICRVersion}
	for _, ref := range o.AICRValidatorImages {
		byRef[ref] = o.AICRVersion
	}
	for _, e := range o.AICRE2E {
		byRef[e.Image] = e.Version
		for _, ref := range strings.Split(e.ValidatorImages, ",") {
			byRef[ref] = e.Version
		}
	}
	out := make([]pinnedImage, 0, len(byRef))
	for _, ref := range slices.Sorted(maps.Keys(byRef)) {
		out = append(out, pinnedImage{ref: ref, version: byRef[ref]})
	}
	return out
}

// verifier is what verify reaches the network through; tests replace it.
type verifier struct {
	// gh runs the gh CLI and returns its standard output.
	gh func(ctx context.Context, args ...string) ([]byte, error)
	// digest resolves an image reference to its digest.
	digest func(ctx context.Context, ref string) (string, error)
}

// network is the verifier that runs gh and asks the registry.
var network = verifier{gh: runGH, digest: registryDigest}

// verify checks that each pinned image is its release's: the release is
// published in NVIDIA/aicr, the image's version tag resolves to the pinned
// digest, and the digest has provenance from attest-images.yaml, run on a
// GitHub-hosted runner from the release's tag. Provenance alone does not
// suffice: a tag's run attests its images before the release gates that can
// still stop it. Call verify on pins that pass check.
func (p *pins) verify(ctx context.Context, v verifier, out io.Writer) error {
	images := p.images()
	versions := map[string]bool{}
	for _, img := range images {
		versions[img.version] = true
	}
	for _, version := range slices.Sorted(maps.Keys(versions)) {
		draft, err := bounded(ctx, func(ctx context.Context) ([]byte, error) {
			return v.gh(ctx, "release", "view", version, "--repo", aicrRepository, "--json", "isDraft", "--jq", ".isDraft")
		})
		if err != nil {
			return fmt.Errorf("look up AICR release %s: %w", version, err)
		}
		if strings.TrimSpace(string(draft)) != "false" {
			return fmt.Errorf("AICR %s is not a published release in %s", version, aicrRepository)
		}
	}
	for _, img := range images {
		repo, digest := imageRepository(img.ref), img.ref[strings.IndexByte(img.ref, '@')+1:]
		tagged := repo + ":" + img.version
		got, err := bounded(ctx, func(ctx context.Context) (string, error) { return v.digest(ctx, tagged) })
		if err != nil {
			return fmt.Errorf("resolve %s: %w", tagged, err)
		}
		if got != digest {
			return fmt.Errorf("%s is %s, not the pinned %s", tagged, got, digest)
		}
		if _, err = bounded(ctx, func(ctx context.Context) ([]byte, error) {
			return v.gh(ctx, "attestation", "verify", "oci://"+repo+"@"+digest, "--repo", aicrRepository,
				"--signer-workflow", aicrSignerWorkflow, "--source-ref", "refs/tags/"+img.version, "--deny-self-hosted-runners")
		}); err != nil {
			return fmt.Errorf("verify the provenance of %s: %w", img.ref, err)
		}
		if _, err = fmt.Fprintf(out, "verified %s\n", img.ref); err != nil {
			return err
		}
	}
	return nil
}

// bounded runs f under verifyTimeout, and says so when the time ran out.
func bounded[T any](ctx context.Context, f func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	out, err := f(ctx)
	if err != nil && ctx.Err() != nil {
		return out, fmt.Errorf("timed out: %w", err)
	}
	return out, err
}

// runGH runs the gh CLI, returning its standard output, or its standard
// error with the failure.
func runGH(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// registryDigest resolves a public image reference to its digest.
func registryDigest(ctx context.Context, ref string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(r, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous))
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// pinsCmd checks that the operator's AICR pins agree and, with
// -attestations, that each pinned image is its published release's.
func pinsCmd(args []string, out io.Writer, v verifier) error {
	fs := flag.NewFlagSet("pins", flag.ContinueOnError)
	file := fs.String("settings", ".settings.yaml", "Pins.")
	attestations := fs.Bool("attestations", false, "Verify each pinned image against its published release with gh and the registry.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := readPins(*file)
	if err != nil {
		return err
	}
	if err = p.check(); err != nil {
		return err
	}
	versions := make([]string, 0, len(p.Operator.AICRE2E))
	for _, e := range p.Operator.AICRE2E {
		versions = append(versions, e.Version)
	}
	if _, err = fmt.Fprintf(out, "AICR pins agree: %s pinned; %s tested\n", p.Operator.AICRVersion, strings.Join(versions, ", ")); err != nil {
		return err
	}
	if !*attestations {
		return nil
	}
	return p.verify(context.Background(), v, out)
}
