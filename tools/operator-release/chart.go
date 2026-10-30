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
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The chart values sections a candidate sets.
const (
	aicrValues     = "aicr"
	operatorValues = "operator"
	registryValues = "registry"
)

var (
	chartName       = regexp.MustCompile(`(?m)^name:\s*(\S+)\s*$`)
	chartVersion    = regexp.MustCompile(`(?m)^version:.*$`)
	chartAppVersion = regexp.MustCompile(`(?m)^appVersion:.*$`)
)

// packageChart packages the chart in dir as a Helm archive stamped with the
// candidate version: Chart.yaml's version (without the "v") and
// appVersion. Unlike `helm package`, the archive is deterministic: sorted
// entries with fixed times, owners and modes, and a gzip header without name
// or time, so the same source always yields the same digest. .helmignore
// lines are a directory prefix ("dir/"), a base-name glob, or a path.
func packageChart(dir fs.FS, version string, aicr AICR, operatorImage, registryImage string) (name string, archive []byte, err error) {
	chartYAML, err := fs.ReadFile(dir, "Chart.yaml")
	if err != nil {
		return "", nil, fmt.Errorf("read Chart.yaml: %w", err)
	}
	m := chartName.FindSubmatch(chartYAML)
	if m == nil || len(chartVersion.FindAll(chartYAML, -1)) != 1 || len(chartAppVersion.FindAll(chartYAML, -1)) != 1 {
		return "", nil, fmt.Errorf("chart.yaml needs one name, version and appVersion")
	}
	name = string(m[1])
	chartYAML = chartVersion.ReplaceAll(chartYAML, []byte("version: "+strings.TrimPrefix(version, "v")))
	chartYAML = chartAppVersion.ReplaceAll(chartYAML, []byte("appVersion: "+version))
	values, err := fs.ReadFile(dir, "values.yaml")
	if err != nil {
		return "", nil, fmt.Errorf("read values.yaml: %w", err)
	}
	if values, err = stampValues(values, aicr, operatorImage, registryImage); err != nil {
		return "", nil, err
	}

	ignored, err := helmignore(dir)
	if err != nil {
		return "", nil, err
	}
	var files []string
	err = fs.WalkDir(dir, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == "." {
			return nil
		}
		if ignored(p, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		} else if !d.IsDir() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("walk the chart: %w", err)
	}
	sort.Strings(files)

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return "", nil, err
	}
	tw := tar.NewWriter(gz)
	for _, f := range files {
		var data []byte
		switch f {
		case "Chart.yaml":
			data = chartYAML
		case "values.yaml":
			data = values
		default:
			if data, err = fs.ReadFile(dir, f); err != nil {
				return "", nil, err
			}
		}
		h := &tar.Header{Name: name + "/" + f, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)),
			ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX}
		if err = tw.WriteHeader(h); err != nil {
			return "", nil, err
		}
		if _, err = tw.Write(data); err != nil {
			return "", nil, err
		}
	}
	if err = tw.Close(); err != nil {
		return "", nil, err
	}
	if err = gz.Close(); err != nil {
		return "", nil, err
	}
	return name, buf.Bytes(), nil
}

// stampValues sets the chart's default AICR release, AICR images, operator
// image and in-cluster registry image to the candidate's, so the published
// chart installs exactly what was qualified without other inputs. It edits
// values.yaml's text, which keeps its comments; each field must appear once
// in its section, the images empty.
func stampValues(values []byte, aicr AICR, operatorImage, registryImage string) ([]byte, error) {
	validators := "  validatorImages: []"
	if len(aicr.ValidatorImages) > 0 {
		validators = "  validatorImages:"
		for _, v := range aicr.ValidatorImages {
			validators += fmt.Sprintf("\n    - %q", v)
		}
	}
	stamps := []struct{ section, line, value string }{
		{aicrValues, "  version: ", "  version: " + aicr.Version},
		{aicrValues, `  image: ""`, fmt.Sprintf("  image: %q", aicr.Image)},
		{aicrValues, "  validatorImages: []", validators},
		{operatorValues, `  image: ""`, fmt.Sprintf("  image: %q", operatorImage)},
		{registryValues, `    image: ""`, fmt.Sprintf("    image: %q", registryImage)},
	}
	stamped := make([]int, len(stamps))
	lines := strings.Split(string(values), "\n")
	section := ""
	for i, line := range lines {
		if line != "" && line[0] != ' ' && line[0] != '#' {
			section, _, _ = strings.Cut(line, ":")
		}
		for j, st := range stamps {
			if section == st.section && (line == st.line || strings.HasSuffix(st.line, " ") && strings.HasPrefix(line, st.line)) {
				lines[i] = st.value
				stamped[j]++
			}
		}
	}
	for j, st := range stamps {
		if stamped[j] != 1 {
			return nil, fmt.Errorf("values.yaml has %d %q lines under %s:, want one", stamped[j], strings.TrimSpace(st.line), st.section)
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// helmignore returns a matcher for the chart's .helmignore, if any.
func helmignore(dir fs.FS) (func(p string, isDir bool) bool, error) {
	data, err := fs.ReadFile(dir, ".helmignore")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var patterns []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, line)
		}
	}
	return func(p string, isDir bool) bool {
		if p == ".helmignore" {
			return true
		}
		for _, pat := range patterns {
			if dirPat, ok := strings.CutSuffix(pat, "/"); ok {
				if isDir && (p == dirPat || path.Base(p) == dirPat) {
					return true
				}
				continue
			}
			if base, _ := path.Match(pat, path.Base(p)); base || p == pat {
				return true
			}
		}
		return false
	}, nil
}
