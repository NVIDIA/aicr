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

// imageDigestExemption scopes an exemption to the manifest that carries
// the reference: the exemption applies only when the walked path contains
// one of Manifests, so another resource reusing the same tag elsewhere cannot
// ride an existing exemption past the digest check.
type imageDigestExemption struct {
	// Manifests are path substrings under components/; the exemption applies
	// to a manifest whose path contains any of them.
	Manifests []string
	Reason    string
}

func (e imageDigestExemption) covers(manifestPath string) bool {
	for _, m := range e.Manifests {
		if strings.Contains(manifestPath, m) {
			return true
		}
	}
	return false
}

// docaDriverDigestReason explains why the DOCA driver stays on its tag.
// network-operator builds the driver image per node from this tag, picking a
// precompiled build for the node's kernel when one exists; a digest version
// skips that lookup and runs one build everywhere.
const docaDriverDigestReason = "a digest version turns off network-operator's per-kernel precompiled driver " +
	"selection, so pinning changes which driver build runs; how to pin it is decided in #3658"

// nebiusOFEDDigestReason covers the Nebius marketplace chart 25.7.0, whose
// operator (v25.1.0) predates digest support in the OFED driver version.
const nebiusOFEDDigestReason = "network-operator v25.1.0 (Nebius chart 25.7.0) accepts no digest in the OFED driver version; " +
	"Mellanox/network-operator#1726 added it in 25.10; tracked in #3661"

// imageDigestExemptions enumerates manifest-level image references that
// AICR knowingly ships without an `@sha256:` digest. Each entry carries
// the reason and the tracking issue. New refs MUST either include a digest
// or be added here with a reason; ADR-006 and #749 formalize this contract.
//
// A CRD that keeps the image in sibling fields takes its digest there:
// NicClusterPolicy as `version: sha256:<hex>` and a Skyhook Package as
// `containerSHA`. What remains is a reference a digest cannot pin without
// changing what runs, an OFED driver whose operator predates digest support
// for it, or a node-local image with no registry.
var imageDigestExemptions = map[string]imageDigestExemption{
	// vr200-nvke pins network-operator one patch ahead of the registry
	// default (26.7.1 vs 26.7.0) because 26.7.0's nic-configuration-daemon
	// has no firmware-discovery fallback and cannot discover ConnectX-9.
	// A crd-digest annotation would have to name tag=network-operator-v26.7.1,
	// which TestNetworkOperatorTagAnnotationsFollowChartPin rejects while the
	// registry default is 26.7.0, so these two stay on tags.
	"nvcr.io/nvidia/mellanox/nic-configuration-operator:network-operator-v26.7.1":        {[]string{"network-operator/manifests/nic-cluster-policy-nvke-vr200"}, "leaf pins network-operator 26.7.1 ahead of the registry default; a digest annotation would name a tag the chart-pin guard rejects"},
	"nvcr.io/nvidia/mellanox/nic-configuration-operator-daemon:network-operator-v26.7.1": {[]string{"network-operator/manifests/nic-cluster-policy-nvke-vr200"}, "leaf pins network-operator 26.7.1 ahead of the registry default; a digest annotation would name a tag the chart-pin guard rejects"},
	"nvcr.io/nvidia/mellanox/nvidia-k8s-ipam:network-operator-v26.7.1":                   {[]string{"network-operator/manifests/nic-cluster-policy-nvke-vr200"}, "leaf pins network-operator 26.7.1 ahead of the registry default; a digest annotation would name a tag the chart-pin guard rejects"},
	"nvcr.io/nvidia/mellanox/plugins:network-operator-v26.7.1":                           {[]string{"network-operator/manifests/nic-cluster-policy-nvke-vr200"}, "leaf pins network-operator 26.7.1 ahead of the registry default; a digest annotation would name a tag the chart-pin guard rejects"},
	"nvcr.io/nvidia/mellanox/multus-cni:network-operator-v26.7.1":                        {[]string{"network-operator/manifests/nic-cluster-policy-nvke-vr200"}, "leaf pins network-operator 26.7.1 ahead of the registry default; a digest annotation would name a tag the chart-pin guard rejects"},

	"nvcr.io/nvidia/mellanox/doca-driver:doca3.2.0-25.10-1.2.8.0-2": {[]string{"network-operator/manifests/nic-cluster-policy-aks"}, docaDriverDigestReason},
	"nvcr.io/nvidia/mellanox/doca-driver:doca3.4.0-26.04-0.8.6.0-0": {[]string{"network-operator/manifests/nic-cluster-policy-generic-gb300"}, docaDriverDigestReason},

	"cr.eu-north1.nebius.cloud/marketplace/nebius/nvidia-network-operator/image/doca-driver:doca3.1.0-25.07-0.9.7.0-0": {[]string{"network-operator/manifests/nic-cluster-policy-nebius-b300"}, nebiusOFEDDigestReason},

	// cos-gpu-installer (gcp-driver-installer): node-local image preloaded on
	// every COS node and referenced with imagePullPolicy: Never — there is no
	// registry to pin a digest against, the "digest" differs per COS build,
	// and the ref must never be mirrored or pulled. Issue #1716.
	"cos-nvidia-installer:fixed": {[]string{"gcp-driver-installer/manifests/"}, "COS-node-local preloaded image (imagePullPolicy: Never); no registry digest exists and it must not be mirrored; issue #1716"},
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
			if exemption, ok := imageDigestExemptions[img]; ok && exemption.covers(p) {
				t.Logf("exempted: %s (%v) — %s", img, exemption.Manifests, exemption.Reason)
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
