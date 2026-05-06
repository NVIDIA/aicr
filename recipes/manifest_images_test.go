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
	"path"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bom"
)

// TestComponentManifestImagesAreFullyQualified asserts that every image
// reference under components/*/manifests/*.yaml carries a :tag or
// @digest. Catches the class of bug where a contributor lands
// `image: ubuntu` with no tag, which silently resolves to :latest in
// production and breaks reproducibility.
//
// Uses pkg/bom.ExtractImagesFromYAML, which combines CRD-style
// repository/image/version triplets, so manifests like NicClusterPolicy
// and Skyhook Packages are evaluated correctly even though `image:` and
// `version:` are sibling fields rather than concatenated.
func TestComponentManifestImagesAreFullyQualified(t *testing.T) {
	var checked int
	err := fs.WalkDir(FS, "components", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.Contains(p, "/manifests/") {
			return nil
		}
		if ext := path.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, rerr := fs.ReadFile(FS, p)
		if rerr != nil {
			return rerr
		}
		images, perr := bom.ExtractImagesFromYAML(data)
		if perr != nil {
			t.Errorf("%s: parse: %v", p, perr)
			return nil
		}
		for _, img := range images {
			checked++
			ref := bom.ParseImageRef(img)
			if ref.Tag == "" && ref.Digest == "" {
				t.Errorf("%s: image %q is not fully qualified (missing :tag and @digest)", p, img)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk components: %v", err)
	}
	if checked == 0 {
		t.Fatal("no images were checked; embedded FS may be empty")
	}
	t.Logf("verified %d image refs across components/*/manifests/", checked)
}

// imageDigestExemptions enumerates manifest-level image references that
// AICR knowingly ships without an `@sha256:` digest. Each entry carries
// the reason and the upstream tracking issue. New refs MUST either
// include a digest or be added here with a reason; ADR-006 and #749
// formalize this contract.
//
// The dominant exemption pattern: CRD-style schemas (NicClusterPolicy,
// Skyhook Package) where `image:` and `version:` are sibling fields and
// the schema does not accept an `@sha256:` digest. Reproducibility for
// these refs is delivered by admission-time digest or signature
// verification at deploy time (#745) plus the upstream signing requests
// filed under the supply-chain epic (#739).
var imageDigestExemptions = map[string]string{
	// NicClusterPolicy (network-operator AKS): repository/image/version
	// triplet schema; no digest field.
	"nvcr.io/nvidia/mellanox/doca-driver:doca3.2.0-25.10-1.2.8.0-2":               "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555",
	"nvcr.io/nvidia/mellanox/k8s-rdma-shared-dev-plugin:network-operator-v26.1.0": "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555",
	"nvcr.io/nvidia/doca/doca_telemetry:1.22.5-doca3.1.0-host":                    "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555",

	// Skyhook Package (nodewright-customizations): image/version sibling
	// schema; no digest field.
	"ghcr.io/nvidia/skyhook-packages/shellscript:1.1.1":       "Skyhook Package CRD does not accept image digests; tracked via #745 and NVIDIA/nodewright#224",
	"ghcr.io/nvidia/skyhook-packages/nvidia-tuning-gke:0.1.1": "Skyhook Package CRD does not accept image digests; tracked via #745 and NVIDIA/nodewright#224",
	"ghcr.io/nvidia/nodewright-packages/nvidia-setup:0.2.2":   "Skyhook Package CRD does not accept image digests; tracked via #745 and NVIDIA/nodewright#224",
	"ghcr.io/nvidia/nodewright-packages/nvidia-tuned:0.3.0":   "Skyhook Package CRD does not accept image digests; tracked via #745 and NVIDIA/nodewright#224",
}

// TestComponentManifestImagesAreDigestPinned asserts that every image
// reference under components/*/manifests/*.yaml carries an `@sha256:`
// digest, except for the explicitly enumerated exemptions in
// `imageDigestExemptions` above. This is the in-tree enforcement of
// ADR-006 layer 2 (digest-pin every image AICR overrides explicitly).
//
// A bare-tag reference outside the exemption set fails the test. To add
// a new tag-only manifest reference, add it to the exemption map with a
// reason and a tracking issue; otherwise pin to a digest.
func TestComponentManifestImagesAreDigestPinned(t *testing.T) {
	var checked int
	err := fs.WalkDir(FS, "components", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.Contains(p, "/manifests/") {
			return nil
		}
		if ext := path.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, rerr := fs.ReadFile(FS, p)
		if rerr != nil {
			return rerr
		}
		images, perr := bom.ExtractImagesFromYAML(data)
		if perr != nil {
			t.Errorf("%s: parse: %v", p, perr)
			return nil
		}
		for _, img := range images {
			checked++
			ref := bom.ParseImageRef(img)
			if ref.Digest != "" {
				continue
			}
			if reason, ok := imageDigestExemptions[img]; ok {
				t.Logf("exempted: %s — %s", img, reason)
				continue
			}
			t.Errorf("%s: image %q is not digest-pinned and not in the documented exemption set (recipes/manifest_images_test.go::imageDigestExemptions); per ADR-006 layer 2, append an @sha256:<digest> or add an exemption with a reason", p, img)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk components: %v", err)
	}
	if checked == 0 {
		t.Fatal("no images were checked; embedded FS may be empty")
	}
}
