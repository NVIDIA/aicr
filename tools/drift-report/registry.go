// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Command drift-report converts a Renovate dry-run report into the weekly
// component drift report consumed by .github/workflows/registry-drift.yaml and
// the aicr-reviewing-component-drift skill.
package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Pin is one component's chart pin as declared in recipes/registry.yaml,
// together with the `# renovate:` annotation that makes it visible to Renovate.
// A manifest-only component (no upstream chart) has an empty Repository.
type Pin struct {
	Component   string
	Chart       string
	Repository  string
	Version     string
	Datasource  string
	DepName     string
	RegistryURL string
	Kustomize   bool
	Annotated   bool
	Line        int
}

var (
	componentRe  = regexp.MustCompile(`^  - name: (\S+)`)
	kustomizeRe  = regexp.MustCompile(`^    kustomize:\s*$`)
	annotationRe = regexp.MustCompile(`^\s*# renovate: datasource=(\S+) depName=(\S+)(?: registryUrl=(\S+))?\s*$`)
	fieldRe      = regexp.MustCompile(`^\s+(defaultRepository|defaultChart|defaultVersion): *(.*?)\s*$`)
)

// LoadPins parses recipes/registry.yaml as text rather than through the YAML
// decoder: the `# renovate:` annotations are comments, which any decoder drops.
func LoadPins(repoRoot string) ([]Pin, error) {
	path := filepath.Join(repoRoot, "recipes", "registry.yaml")
	f, err := os.Open(path) //nolint:gosec // fixed in-repo path
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "open registry.yaml", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	var (
		pins    []Pin
		cur     *Pin
		pending *Pin // annotation seen, waiting for its defaultVersion
		lineNo  int
	)
	flush := func() {
		if cur != nil {
			pins = append(pins, *cur)
		}
		cur, pending = nil, nil
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lineNo++
		line := sc.Text()

		if m := componentRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &Pin{Component: m[1], Line: lineNo}
			continue
		}
		if cur == nil {
			continue // header comment block
		}
		if kustomizeRe.MatchString(line) {
			cur.Kustomize = true
			continue
		}
		if m := annotationRe.FindStringSubmatch(line); m != nil {
			pending = &Pin{Datasource: m[1], DepName: m[2], RegistryURL: m[3]}
			continue
		}
		m := fieldRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		val := strings.Trim(strings.TrimSpace(m[2]), `"'`)
		switch m[1] {
		case "defaultRepository":
			cur.Repository = val
		case "defaultChart":
			cur.Chart = val
		case "defaultVersion":
			cur.Version = val
			cur.Line = lineNo
			if pending != nil {
				cur.Annotated = true
				cur.Datasource = pending.Datasource
				cur.DepName = pending.DepName
				cur.RegistryURL = pending.RegistryURL
				pending = nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "scan registry.yaml", err)
	}
	flush()
	return pins, nil
}

// expectedDep derives the Renovate coordinates a pin's annotation must declare.
// OCI chart repositories are looked up through the docker datasource, where the
// dep name is the full image path; HTTP chart repositories use the helm
// datasource, where the dep name is the bare chart name and the repository
// travels as registryUrl.
func expectedDep(repo, chart string) (datasource, depName, registryURL string) {
	if strings.HasPrefix(repo, "oci://") {
		return "docker", strings.TrimPrefix(repo, "oci://") + "/" + chartLeaf(chart), ""
	}
	return "helm", chartLeaf(chart), repo
}

// chartLeaf strips the repository-alias prefix some defaultChart values carry
// ("jetstack/cert-manager" -> "cert-manager"); the helm datasource wants the
// chart name alone.
func chartLeaf(chart string) string {
	if i := strings.LastIndex(chart, "/"); i >= 0 {
		return chart[i+1:]
	}
	return chart
}
