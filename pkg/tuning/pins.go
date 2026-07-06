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

package tuning

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// PackagePin identifies a pinned nodewright package (by image basename) and its
// version. The zero value renders as "-" (no package).
type PackagePin struct {
	Name    string
	Version string
}

// recognizedImages are the package image basenames the tuning table surfaces.
// A manifest carrying only unrecognized images (e.g. no-op's "shellscript")
// yields no Setup/Tuning pin — an empty row — but is not an error.
var recognizedImages = map[string]struct{}{
	"nvidia-setup":      {},
	"nvidia-tuned":      {},
	"nvidia-tuning-gke": {},
}

// extractPackagePins parses the static image/version pins from a nodewright
// manifest. The manifest is a Helm template (plain YAML parse fails), but every
// entry under `packages:` carries static `image:` and `version:` literals. The
// parser walks the direct children of `packages:` and reads each entry's own
// image/version, ignoring nested sub-blocks (dependsOn/configMap/env/resources)
// — essential because dependsOn repeats package names + versions at a deeper
// indent that a naive scan would mis-associate. Returns image basename → version.
//
// Fail-loud: a recognized image present without a readable version is an error
// (a refactor must not silently blank a populated cell); a second entry for the
// same image with a different version is an error.
func extractPackagePins(manifest []byte) (map[string]string, error) {
	lines := strings.Split(string(manifest), "\n")

	pkgIdx, pkgIndent := -1, -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "packages:" {
			pkgIdx, pkgIndent = i, indentOf(ln)
			break
		}
	}
	if pkgIdx < 0 {
		return nil, errors.New(errors.ErrCodeInternal, "manifest has no packages: block")
	}

	entryIndent := -1
	for i := pkgIdx + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		ind := indentOf(lines[i])
		if ind <= pkgIndent {
			break
		}
		if strings.HasPrefix(t, "{{") {
			continue
		}
		entryIndent = ind
		break
	}
	if entryIndent < 0 {
		return nil, errors.New(errors.ErrCodeInternal, "packages: block has no entries")
	}
	// fieldIndent — the entry's direct-child indent — is detected from the first
	// field line encountered rather than assuming a fixed step, so a manifest
	// using any indent width still parses instead of silently yielding no pins.
	fieldIndent := -1

	pins := map[string]string{}
	var curImage, curVersion string
	flush := func() error {
		if curImage == "" {
			return nil
		}
		base := curImage[strings.LastIndex(curImage, "/")+1:]
		if curVersion == "" {
			if _, ok := recognizedImages[base]; ok {
				return errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("recognized package image %q has no version pin", base))
			}
			return nil
		}
		if existing, ok := pins[base]; ok && existing != curVersion {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("conflicting versions for image %q: %q vs %q", base, existing, curVersion))
		}
		pins[base] = curVersion
		return nil
	}

	for i := pkgIdx + 1; i < len(lines); i++ {
		ln := lines[i]
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		ind := indentOf(ln)
		if ind <= pkgIndent {
			break // dedent out of the packages block (covers a top-level {{- end }})
		}
		if strings.HasPrefix(t, "{{") {
			continue // template line inside the block
		}
		if ind == entryIndent && strings.HasSuffix(t, ":") {
			if err := flush(); err != nil {
				return nil, err
			}
			curImage, curVersion = "", ""
			continue
		}
		if ind <= entryIndent {
			continue
		}
		// A line deeper than the entry key belongs to the current entry. The
		// first such line fixes the direct-child indent; image/version are read
		// only there, so nested sub-blocks (dependsOn/configMap/…), which are
		// deeper still, are ignored regardless of the manifest's indent width.
		if fieldIndent == -1 {
			fieldIndent = ind
		}
		if ind == fieldIndent {
			if v, ok := strings.CutPrefix(t, "image:"); ok {
				curImage = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(t, "version:"); ok {
				curVersion = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return pins, nil
}

// indentOf returns the count of leading spaces on a line.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// classifyPins maps extracted image→version pins into the Setup and Tuning
// columns. Setup is the nvidia-setup pin; Tuning is nvidia-tuned or
// nvidia-tuning-gke. A manifest carrying both tuning packages is ambiguous and
// errors (fail-loud, matching the rest of this package) rather than letting one
// silently win. Unrecognized images yield zero-value pins (rendered "-").
func classifyPins(pins map[string]string) (setup, tuning PackagePin, err error) {
	if v, ok := pins["nvidia-setup"]; ok {
		setup = PackagePin{Name: "nvidia-setup", Version: v}
	}
	tunedVer, hasTuned := pins["nvidia-tuned"]
	gkeVer, hasGKE := pins["nvidia-tuning-gke"]
	switch {
	case hasTuned && hasGKE:
		return PackagePin{}, PackagePin{}, errors.New(errors.ErrCodeInternal,
			"manifest carries both nvidia-tuned and nvidia-tuning-gke; expected at most one tuning package")
	case hasTuned:
		tuning = PackagePin{Name: "nvidia-tuned", Version: tunedVer}
	case hasGKE:
		tuning = PackagePin{Name: "nvidia-tuning-gke", Version: gkeVer}
	}
	return setup, tuning, nil
}
