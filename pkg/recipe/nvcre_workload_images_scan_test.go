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

package recipe

import (
	"os"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	nvcreWorkloadImagesFile = "../../recipes/components/nvcre/workload-images.yaml"
	nvcreScanWorkflowFile   = "../../.github/workflows/vuln-scan-images.yaml"
	nvcreScanClosureMarker  = "nvcre"
)

// TestNVCREWorkloadImagesScanned keeps the weekly vulnerability scan on the
// images NVCRE's certification workloads pull (#3511). Those images come from a
// catalog compiled into the manager binary, so neither chart rendering nor the
// scan's other legs reach them.
//
// Both directions fail: an image in workload-images.yaml with no scan leg, or
// pinned at a different digest, would be reported clean while the cluster runs
// something else, and a leg whose image left the file keeps scanning bytes
// nothing ships. The legs are matched by structure, not by substring, because
// a stale tag on one leg must not be satisfied by another leg's.
func TestNVCREWorkloadImagesScanned(t *testing.T) {
	closureData, err := os.ReadFile(nvcreWorkloadImagesFile)
	if err != nil {
		t.Fatalf("reading %s: %v", nvcreWorkloadImagesFile, err)
	}
	var closure struct {
		Images []struct {
			Image  string `yaml:"image"`
			Digest string `yaml:"digest"`
		} `yaml:"images"`
	}
	if err = yaml.Unmarshal(closureData, &closure); err != nil {
		t.Fatalf("parsing %s: %v", nvcreWorkloadImagesFile, err)
	}
	if len(closure.Images) == 0 {
		t.Fatalf("%s lists no images; the scan has nothing to cover", nvcreWorkloadImagesFile)
	}

	workflowData, err := os.ReadFile(nvcreScanWorkflowFile)
	if err != nil {
		t.Fatalf("reading %s: %v", nvcreScanWorkflowFile, err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []map[string]any `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err = yaml.Unmarshal(workflowData, &workflow); err != nil {
		t.Fatalf("parsing %s: %v", nvcreScanWorkflowFile, err)
	}

	// Scan legs by "<repository>:<tag@digest>", and every leg name in use.
	legs := make(map[string]int)
	names := make(map[string]int)
	for _, job := range workflow.Jobs {
		for _, entry := range job.Strategy.Matrix.Include {
			name, _ := entry["name"].(string)
			if name != "" {
				names[name]++
			}
			if marker, _ := entry["closure"].(string); marker != nvcreScanClosureMarker {
				continue
			}
			image, _ := entry["image"].(string)
			tag, _ := entry["tag"].(string)
			legs[image+":"+tag]++
			if name == "" {
				t.Errorf("scan leg %s:%s has no name; two legs of one image would write the same result file", image, tag)
			}
		}
	}

	want := make(map[string]bool, len(closure.Images))
	for _, img := range closure.Images {
		repository, tag, ok := splitImageRef(img.Image)
		if !ok || img.Digest == "" {
			t.Errorf("%s: image %q needs a tag and a digest to be pinned in the scan", nvcreWorkloadImagesFile, img.Image)
			continue
		}
		key := repository + ":" + tag + "@" + img.Digest
		want[key] = true
		switch legs[key] {
		case 1:
		case 0:
			t.Errorf("%s has no leg for %s; #3511 requires the scan to cover every NVCRE workload image at the digest in %s",
				nvcreScanWorkflowFile, key, nvcreWorkloadImagesFile)
		default:
			t.Errorf("%s has %d legs for %s, want 1", nvcreScanWorkflowFile, legs[key], key)
		}
	}

	var stale []string
	for key := range legs {
		if !want[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("%s scans %s, which is not in %s; drop the leg or regenerate the file with `make nvcre-closure`",
			nvcreScanWorkflowFile, key, nvcreWorkloadImagesFile)
	}

	for name, count := range names {
		if count > 1 {
			t.Errorf("%s uses leg name %q %d times; result files are keyed by name", nvcreScanWorkflowFile, name, count)
		}
	}
}
