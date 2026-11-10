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
	"bytes"
	stderrors "errors"
	"io"
	"io/fs"
	"testing"

	"gopkg.in/yaml.v3"
)

// gatewayCRDRetentionGuards are the annotations every vendored Gateway API and
// Inference Extension CRD must carry. Without them, uninstalling or pruning the
// release or Application that owns the CRD deletes it, with every resource of
// its kind. Helm reads keep from the release's stored manifest on uninstall, so
// they have to ship in the bundle that installs the CRD, not only on the live
// object.
var gatewayCRDRetentionGuards = map[string]string{
	"helm.sh/resource-policy":         "keep",
	"argocd.argoproj.io/sync-options": "Delete=false,Prune=false",
}

// isGatewayCRDGroup reports whether a CRD group is one of the Gateway API or
// Inference Extension groups, in their standard or experimental form.
func isGatewayCRDGroup(group string) bool {
	switch group {
	case "gateway.networking.k8s.io", "gateway.networking.x-k8s.io",
		"inference.networking.k8s.io", "inference.networking.x-k8s.io":
		return true
	}
	return false
}

// TestVendoredGatewayCRDsCarryRetentionGuards pins the retention guards on every
// Gateway API and Inference Extension CRD vendored under components/*/manifests,
// so re-vendoring from upstream, which ships neither annotation, cannot drop
// them unnoticed.
func TestVendoredGatewayCRDsCarryRetentionGuards(t *testing.T) {
	efs := GetEmbeddedFS()
	files, err := fs.Glob(efs, "components/*/manifests/*.yaml")
	if err != nil {
		t.Fatalf("glob vendored manifests: %v", err)
	}

	checked := 0
	for _, file := range files {
		data, readErr := efs.ReadFile(file)
		if readErr != nil {
			t.Fatalf("read %s: %v", file, readErr)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var doc struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name        string            `yaml:"name"`
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
				Spec struct {
					Group string `yaml:"group"`
				} `yaml:"spec"`
			}
			if decErr := dec.Decode(&doc); decErr != nil {
				if stderrors.Is(decErr, io.EOF) {
					break
				}
				// A Helm-templated manifest is not plain YAML and is not a
				// vendored upstream file; anything else must parse.
				if !bytes.Contains(data, []byte("{{")) {
					t.Fatalf("decode %s: %v", file, decErr)
				}
				break
			}
			if doc.Kind != "CustomResourceDefinition" || !isGatewayCRDGroup(doc.Spec.Group) {
				continue
			}
			checked++
			for key, want := range gatewayCRDRetentionGuards {
				if got := doc.Metadata.Annotations[key]; got != want {
					t.Errorf("%s: CRD %s has %s=%q, want %q", file, doc.Metadata.Name, key, got, want)
				}
			}
		}
	}

	// Five Gateway API standard-channel CRDs, five Inference Extension CRDs
	// and the TLSRoute CRD. A lower count means the walk missed the vendored
	// files and the assertions above checked nothing.
	const wantAtLeast = 11
	if checked < wantAtLeast {
		t.Fatalf("checked %d Gateway API and Inference Extension CRDs, want at least %d", checked, wantAtLeast)
	}
}
