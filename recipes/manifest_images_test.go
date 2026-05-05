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
