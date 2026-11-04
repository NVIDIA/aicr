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

// Command operator-release completes a qualification candidate that
// tools/operator-candidate built: it packages the chart
// deterministically and records the candidate's identity in
// candidate.json, with its deployment inputs in candidate-values.yaml.
//
//	operator-release build   -dir dist/candidate -version v0.1.0-rc.1 -repository R -commit C -commit-date D -go-version G
//	operator-release extract -dir dist/candidate -platform linux/amd64 -out bin
//	operator-release refuses -dir dist/candidate
//	operator-release push    -dir dist/candidate
//	operator-release pull    -dir dist/published -version latest -below <candidate version>
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/NVIDIA/aicr/pkg/operator/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "operator-release:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: operator-release build|extract|refuses|push|pull [flags]")
	}
	switch args[0] {
	case "build":
		return build(args[1:], out)
	case "extract":
		return extract(args[1:])
	case "refuses":
		return refuses(args[1:], out)
	case "push":
		return push(args[1:], out)
	case "pull":
		return pull(args[1:], out)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func build(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	dir := fs.String("dir", "dist/candidate", "Candidate directory: image/, registry-image/, sbom/ and plugin/ from the build.")
	ver := fs.String("version", "", "Candidate release version, such as v0.1.0-rc.1.")
	repository := fs.String("repository", "", "Operator image repository the candidate is published to.")
	registryRepository := fs.String("registry-repository", "ghcr.io/nvidia/aicr-registry", "In-cluster registry image repository the candidate is published to.")
	commit := fs.String("commit", "", "Source commit.")
	commitDate := fs.String("commit-date", "", "Source commit date, RFC 3339 UTC.")
	goVersion := fs.String("go-version", "", "Go toolchain version.")
	chartDir := fs.String("chart", "charts/aicr-operator", "Chart source directory.")
	settingsFile := fs.String("settings", ".settings.yaml", "Pins.")
	koFile := fs.String("ko", ".ko.yaml", "ko configuration.")
	goSum := fs.String("go-sum", "go.sum", "Module checksums.")
	aicrVersion := fs.String("aicr-version", "", "AICR release the chart defaults to; with -aicr-image and -aicr-validator-images. Default: .settings.yaml's pins.")
	aicrImage := fs.String("aicr-image", "", "AICR CLI image for -aicr-version, as repository:tag@sha256:<digest>.")
	aicrValidators := fs.String("aicr-validator-images", "", "Comma-separated AICR validator images for -aicr-version, each repository:tag@sha256:<digest>.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case !version.IsRelease(*ver):
		return fmt.Errorf("version %q is not a release version; a candidate is a release build", *ver)
	case *repository == "" || *commit == "" || *commitDate == "" || *goVersion == "":
		return errors.New("-repository, -commit, -commit-date and -go-version are required")
	}
	s, err := readSettings(*settingsFile)
	if err != nil {
		return err
	}
	base, err := baseImage(*koFile)
	if err != nil {
		return err
	}
	image, err := readImage(filepath.Join(*dir, "image"), *repository)
	if err != nil {
		return fmt.Errorf("read the operator image: %w", err)
	}
	registry, err := readImage(filepath.Join(*dir, "registry-image"), *registryRepository)
	if err != nil {
		return fmt.Errorf("read the registry image: %w", err)
	}
	aicr := AICR{Version: s.Operator.AICRVersion, Image: s.Operator.AICRImage, ValidatorImages: s.validators()}
	if *aicrVersion != "" || *aicrImage != "" || *aicrValidators != "" {
		if aicr, err = aicrFlags(*aicrVersion, *aicrImage, *aicrValidators); err != nil {
			return err
		}
	}
	name, archive, err := packageChart(os.DirFS(*chartDir), *ver, aicr, image.Ref(), registry.Ref())
	if err != nil {
		return err
	}
	chartFile := fmt.Sprintf("%s-%s.tgz", name, strings.TrimPrefix(*ver, "v"))
	if err = os.MkdirAll(filepath.Join(*dir, "chart"), 0o755); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(*dir, "chart", chartFile), archive, 0o600); err != nil {
		return err
	}
	plugin, err := hashDir(filepath.Join(*dir, "plugin"))
	if err != nil {
		return err
	}
	inputs := map[string]string{}
	for _, f := range []string{*settingsFile, *koFile, *goSum} {
		data, readErr := os.ReadFile(f)
		if readErr != nil {
			return readErr
		}
		inputs[filepath.Base(f)] = sha256Of(data)
	}
	m := &Manifest{
		SchemaVersion: 1, Version: *ver,
		Source:    Source{Commit: *commit, CommitDate: *commitDate},
		Toolchain: Toolchain{Go: *goVersion, Ko: s.BuildTools.Ko, BaseImage: base},
		Operator:  image,
		Chart:     File{Name: chartFile, Digest: sha256Of(archive)},
		Plugin:    plugin,
		AICR:      aicr,
		Registry:  registry,
		Inputs:    inputs,
	}
	if err = m.seal(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(*dir, "candidate.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	vals, err := values(m)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(*dir, "candidate-values.yaml"), vals, 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "candidate %s %s\noperator %s\nregistry %s\nchart %s %s\n",
		m.Version, m.Digest, m.Operator.Ref(), m.Registry.Ref(), m.Chart.Name, m.Chart.Digest)
	return err
}

func extract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	dir := fs.String("dir", "dist/candidate", "Candidate directory.")
	platform := fs.String("platform", "linux/amd64", "Image platform.")
	layout := fs.String("image", "image", "Image layout in the candidate directory: image (the operator) or registry-image.")
	binary := fs.String("binary", "ko-app/aicr-operator", "Binary path in the image.")
	outFile := fs.String("out", "", "Where to write the binary.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outFile == "" {
		return errors.New("-out is required")
	}
	image, err := readImage(filepath.Join(*dir, *layout), "candidate")
	if err != nil {
		return err
	}
	digest, ok := image.Platforms[*platform]
	if !ok {
		return fmt.Errorf("the candidate has no %s image", *platform)
	}
	return extractBinary(filepath.Join(*dir, *layout), digest, *binary, *outFile)
}

// pinnedRef is an image reference with a tag and a sha256 digest.
var pinnedRef = regexp.MustCompile(`^[^\s@]+/[^\s@/:]+:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}@sha256:[a-f0-9]{64}$`)

// aicrFlags is the AICR release given on the command line, such as the one a
// release tag built: all three flags, each image pinned by digest and tagged
// with the version.
func aicrFlags(ver, image, validators string) (AICR, error) {
	if ver == "" || image == "" || validators == "" {
		return AICR{}, errors.New("-aicr-version, -aicr-image and -aicr-validator-images are set together")
	}
	if !semver.IsValid(ver) || semver.Canonical(ver) != ver {
		return AICR{}, fmt.Errorf("-aicr-version %q is not vX.Y.Z with an optional prerelease", ver)
	}
	refs := strings.Split(validators, ",")
	sort.Strings(refs)
	for _, ref := range append([]string{image}, refs...) {
		if !pinnedRef.MatchString(ref) {
			return AICR{}, fmt.Errorf("AICR image %q is not repository:tag@sha256:<digest>", ref)
		}
		name := ref[:strings.IndexByte(ref, '@')]
		if tag := name[strings.LastIndexByte(name, ':')+1:]; tag != ver {
			return AICR{}, fmt.Errorf("AICR image %q has tag %q, want %q", ref, tag, ver)
		}
	}
	return AICR{Version: ver, Image: image, ValidatorImages: refs}, nil
}
