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

package recipes

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bom"
)

// crdDigestLineRE finds every digest held in a CRD sibling field, the two
// shapes the crd-digest Renovate managers exist to rotate.
var crdDigestLineRE = regexp.MustCompile(`(?m)^\s*(?:version|containerSHA):\s*["']?(sha256:[a-f0-9]{64})`)

// crdDigestMatchStrings returns the crd-digest customManager matchStrings
// from .github/renovate.json5, keyed by the CRD field holding the digest.
// They are the only matchStrings that capture a sha256 currentDigest.
func crdDigestMatchStrings(t *testing.T) map[string]*regexp.Regexp {
	t.Helper()
	cfg, err := os.ReadFile("../.github/renovate.json5")
	if err != nil {
		t.Fatalf("read renovate.json5: %v", err)
	}
	out := map[string]*regexp.Regexp{}
	for _, line := range strings.Split(string(cfg), "\n") {
		if !strings.Contains(line, `(?<currentDigest>sha256:`) {
			continue
		}
		first, last := strings.Index(line, `"`), strings.LastIndex(line, `"`)
		if first < 0 || last <= first {
			t.Fatalf("matchString line is not a double-quoted literal: %s", line)
		}
		pattern, uerr := strconv.Unquote(line[first : last+1])
		if uerr != nil {
			t.Fatalf("unquote matchString %s: %v", line, uerr)
		}
		re, cerr := regexp.Compile(pattern)
		if cerr != nil {
			t.Fatalf("compile matchString %q: %v", pattern, cerr)
		}
		field := "version"
		if strings.Contains(pattern, "containerSHA") {
			field = "containerSHA"
		}
		if _, dup := out[field]; dup {
			t.Fatalf("more than one crd-digest matchString for %s", field)
		}
		out[field] = re
	}
	if len(out) != 2 {
		t.Fatalf("found crd-digest matchStrings for %d fields, want 2 (version, containerSHA)", len(out))
	}
	return out
}

// TestCRDDigestPinsAreRenovateTracked runs the crd-digest matchStrings from
// .github/renovate.json5 over every component manifest. Each digest held in a
// NicClusterPolicy `version` or a Skyhook Package `containerSHA` must be
// matched exactly once, and the match must name the image the neighboring
// fields name. A pin Renovate cannot see is never rotated; a depName that
// drifts from those fields rotates the pin to another image's digest.
func TestCRDDigestPinsAreRenovateTracked(t *testing.T) {
	matchers := crdDigestMatchStrings(t)
	var checked int
	err := fs.WalkDir(FS, "components", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.Contains(p, "/manifests/") {
			return nil
		}
		if ext := path.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, rerr := fs.ReadFile(FS, p)
		if rerr != nil {
			return rerr
		}
		pinned := map[string]int{}
		for _, m := range crdDigestLineRE.FindAllStringSubmatch(string(data), -1) {
			pinned[m[1]]++
		}
		if len(pinned) == 0 {
			return nil
		}
		images, perr := bom.ExtractImagesFromYAML(data)
		if perr != nil {
			t.Errorf("%s: parse: %v", p, perr)
			return nil
		}
		extracted := map[string]bool{}
		for _, img := range images {
			extracted[img] = true
		}

		matched := map[string]int{}
		for field, re := range matchers {
			dep, tag, digest := re.SubexpIndex("depName"), re.SubexpIndex("currentValue"), re.SubexpIndex("currentDigest")
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				matched[m[digest]]++
				ref := m[dep] + "@" + m[digest]
				if field == "containerSHA" {
					ref = m[dep] + ":" + m[tag] + "@" + m[digest]
				}
				if !extracted[ref] {
					t.Errorf("%s: Renovate would rotate %q, which the manifest's fields do not name", p, ref)
				}
			}
		}
		for digest, n := range pinned {
			checked += n
			if matched[digest] != n {
				t.Errorf("%s: %s appears in %d CRD digest field(s) but %d crd-digest Renovate match(es); "+
					"see the crd-digest customManagers in .github/renovate.json5 for the expected shape",
					p, digest, n, matched[digest])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk components: %v", err)
	}
	if checked == 0 {
		t.Fatal("no CRD digest pins were checked; embedded FS may be empty")
	}
}

// networkOperatorTagRE captures the release a crd-digest annotation names for
// an image tagged with the network-operator release it ships with.
var networkOperatorTagRE = regexp.MustCompile(`(?m)^\s*# renovate: datasource=docker depName=\S+ tag=network-operator-v(\S+)\s*$`)

// TestNetworkOperatorTagAnnotationsFollowChartPin requires every
// `tag=network-operator-v<X>` annotation to name the registry's
// network-operator chart pin. The annotation is the only record of the tag
// behind a digest, and Renovate resolves digest updates from it, so one left
// behind on a chart bump turns the next rotation into a move back to the
// previous release's images.
func TestNetworkOperatorTagAnnotationsFollowChartPin(t *testing.T) {
	data, err := fs.ReadFile(FS, "registry.yaml")
	if err != nil {
		t.Fatalf("read registry.yaml: %v", err)
	}
	var reg struct {
		Components []struct {
			Name string `yaml:"name"`
			Helm struct {
				DefaultVersion string `yaml:"defaultVersion"`
			} `yaml:"helm"`
		} `yaml:"components"`
	}
	if err = yaml.Unmarshal(data, &reg); err != nil {
		t.Fatalf("parse registry.yaml: %v", err)
	}
	var pin string
	for _, c := range reg.Components {
		if c.Name == "network-operator" {
			pin = c.Helm.DefaultVersion
		}
	}
	if pin == "" {
		t.Fatal("registry.yaml has no network-operator helm.defaultVersion")
	}

	var checked int
	err = fs.WalkDir(FS, "components", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.Contains(p, "/manifests/") {
			return nil
		}
		content, rerr := fs.ReadFile(FS, p)
		if rerr != nil {
			return rerr
		}
		for _, m := range networkOperatorTagRE.FindAllStringSubmatch(string(content), -1) {
			checked++
			if m[1] != pin {
				t.Errorf("%s: annotation names network-operator-v%s, but registry.yaml pins network-operator %s; "+
					"move the tag and its digest together with the chart pin", p, m[1], pin)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk components: %v", err)
	}
	if checked == 0 {
		t.Fatal("no network-operator-v tag annotations were checked; embedded FS may be empty")
	}
}
