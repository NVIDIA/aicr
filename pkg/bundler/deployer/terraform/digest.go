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

package terraform

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// digestPrefix names the algorithm in a rendered digest, as a registry does.
const digestPrefix = "sha256:"

// isBundledChart reports whether a folder is installed from its directory in
// the bundle rather than from an upstream repository: a -pre, -post or
// -readiness wrapper, a vendored chart, or a local chart from --data.
func isBundledChart(f localformat.Folder) bool {
	return f.Kind != localformat.KindUpstreamHelm || f.Upstream == nil
}

// chartDigests hashes the content of every bundled chart folder, keyed by
// folder dir. helm_release tracks a chart directory by path, version and
// values, not by its files, and every bundled chart's version is the AICR
// version, so a regenerated folder whose templates changed would otherwise
// plan No changes. The digest rides on the release as its description, which
// the provider updates in place, and the upgrade that applies it re-reads the
// directory (measured on the helm provider 3.x: a template-only change plans
// nothing; with the description changed it plans one update and the new
// template is live after apply).
//
// Hashed: each file in the folder other than values.yaml and
// cluster-values.yaml, which values_files already tracks, in path order, as
// its folder-relative path and its bytes, each NUL-terminated. Relative to
// the folder so that renumbering, which already changes the chart path, does
// not also move the digest.
func chartDigests(outputDir string, folders []localformat.Folder) (map[string]string, error) {
	out := make(map[string]string, len(folders))
	for _, f := range folders {
		if !isBundledChart(f) {
			continue
		}
		rels := make([]string, 0, len(f.Files))
		for _, rel := range f.Files {
			base := filepath.Base(rel)
			if base == fileValues || base == fileClusterValues {
				continue
			}
			rels = append(rels, filepath.ToSlash(rel))
		}
		sort.Strings(rels)
		h := sha256.New()
		for _, rel := range rels {
			abs, joinErr := deployer.SafeJoin(outputDir, rel)
			if joinErr != nil {
				return nil, errors.PropagateOrWrap(joinErr, errors.ErrCodeInvalidRequest,
					fmt.Sprintf("path from localformat escapes outputDir: %s", rel))
			}
			data, readErr := os.ReadFile(abs)
			if readErr != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("failed to read %s for its digest", rel), readErr)
			}
			h.Write([]byte(strings.TrimPrefix(rel, f.Dir+"/")))
			h.Write([]byte{0})
			h.Write(data)
			h.Write([]byte{0})
		}
		out[f.Dir] = digestPrefix + hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}
