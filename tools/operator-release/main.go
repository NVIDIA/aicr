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
//	operator-release push    -dir dist/candidate
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
		return errors.New("usage: operator-release build|extract|push [flags]")
	}
	switch args[0] {
	case "build":
		return build(args[1:], out)
	case "extract":
		return extract(args[1:])
	case "push":
		return push(args[1:], out)
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
		return err
	}
	registry, err := readImage(filepath.Join(*dir, "registry-image"), *registryRepository)
	if err != nil {
		return err
	}
	aicr := AICR{Version: s.Operator.AICRVersion, Image: s.Operator.AICRImage, ValidatorImages: s.validators()}
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
	sboms, err := hashDir(filepath.Join(*dir, "sbom"))
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
		Plugin:    plugin, SBOMs: sboms,
		AICR:     aicr,
		Registry: registry,
		Inputs:   inputs,
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
