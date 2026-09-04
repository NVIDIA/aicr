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
// imageDigestExemption scopes an exemption to the manifest that carries
// the reference: the exemption applies only when the walked path contains
// Manifest, so another resource reusing the same tag elsewhere cannot ride
// an existing exemption past the digest check.
type imageDigestExemption struct {
	// Manifest is a path substring under components/ that must appear in
	// the manifest path for the exemption to apply.
	Manifest string
	Reason   string
}

var imageDigestExemptions = map[string]imageDigestExemption{
	// NicClusterPolicy (network-operator AKS): repository/image/version
	// triplet schema; no digest field.
	"nvcr.io/nvidia/mellanox/doca-driver:doca3.2.0-25.10-1.2.8.0-2":               {"network-operator/manifests/nic-cluster-policy-aks", "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555"},
	"nvcr.io/nvidia/mellanox/k8s-rdma-shared-dev-plugin:network-operator-v26.4.1": {"network-operator/manifests/nic-cluster-policy-", "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555"},
	"nvcr.io/nvidia/doca/doca_telemetry:1.22.5-doca3.1.0-host":                    {"network-operator/manifests/nic-cluster-policy-aks", "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555"},

	// Skyhook Package (nodewright-customizations no-op): shellscript package
	// is pinned by tag only — `containerSHA` is not surfaced for this
	// upstream image. nodewright-packages/* refs are digest-pinned via the
	// Skyhook Package `containerSHA` field (issue #1031), folded into the
	// extracted image ref as `@sha256:...` by pkg/bom.ExtractImagesFromYAML.
	"ghcr.io/nvidia/skyhook-packages/shellscript:1.1.1": {"nodewright-customizations/manifests/", "Skyhook Package CRD does not accept image digests; tracked via #745 and NVIDIA/nodewright#224"},

	// cos-gpu-installer (gcp-driver-installer): node-local image preloaded on
	// every COS node and referenced with imagePullPolicy: Never — there is no
	// registry to pin a digest against, the "digest" differs per COS build,
	// and the ref must never be mirrored or pulled. Issue #1716.
	"cos-nvidia-installer:fixed": {"gcp-driver-installer/manifests/", "COS-node-local preloaded image (imagePullPolicy: Never); no registry digest exists and it must not be mirrored; issue #1716"},

	// NicClusterPolicy (network-operator OKE): same repository/image/version
	// triplet schema as the AKS entries above — no digest field in the CRD.
	"ghcr.io/mellanox/nvidia-k8s-ipam:v0.2.0":                         {"network-operator/manifests/nic-cluster-policy-oke-", "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555"},
	"ghcr.io/k8snetworkplumbingwg/multus-cni:v4.2.1":                  {"network-operator/manifests/nic-cluster-policy-oke-", "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555"},
	"ghcr.io/k8snetworkplumbingwg/plugins:v1.6.2-update.1":            {"network-operator/manifests/nic-cluster-policy-oke-", "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555"},
	"ghcr.io/k8snetworkplumbingwg/sriov-network-device-plugin:v3.9.0": {"network-operator/manifests/nic-cluster-policy-oke-", "NicClusterPolicy CRD does not accept image digests; tracked via #745 and Mellanox/network-operator#2555"},
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
			if strings.HasPrefix(ref.Digest, "sha256:") {
				continue
			}
			if ref.Digest != "" {
				t.Errorf("%s: image %q uses non-sha256 digest %q; ADR-006 requires @sha256:<digest>", p, img, ref.Digest)
				continue
			}
			if exemption, ok := imageDigestExemptions[img]; ok && strings.Contains(p, exemption.Manifest) {
				t.Logf("exempted: %s (%s) — %s", img, exemption.Manifest, exemption.Reason)
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
