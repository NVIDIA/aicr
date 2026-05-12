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

// provenance.yaml — bundle-time audit log emitted when --vendor-charts
// is set. One entry per vendored chart with the upstream identity
// (name, version, source URL) plus the SHA256 of the .tgz bytes that
// were copied into the bundle. Operators use this to:
//
//   - cross-reference vendored charts against CVE-yank lists (the
//     upstream-yank fail-loud signal is otherwise lost when bundles
//     freeze chart versions);
//   - reproduce an air-gapped bundle deterministically from the same
//     recipe + provenance entries; and
//   - audit which puller implementation produced the bundle (the
//     PullerVersion field carries that — useful while the project
//     transitions from CLIChartPuller to a future SDK-based puller).
//
// Wire shape: K8s-style apiVersion + kind, matching every other AICR
// persisted format (Recipe, Snapshot, RecipeCriteria, RecipeMixin).
//
// Lives in localformat (the package that owns the VendorRecord type and
// the vendoring branch in Write) so every deployer — helm, argocd,
// argocd-helm — can emit the same audit file without duplicating the
// writer or re-marshaling the record shape.

package localformat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// ProvenanceFileName is the on-disk filename at the bundle root. Exported
// so consumers (deployers, downstream tooling) reference the same name.
const ProvenanceFileName = "provenance.yaml"

// ProvenanceAPIVersion / ProvenanceKind identify the document shape using
// AICR's K8s-style convention. Bump the apiVersion when a downstream
// consumer would need to branch on shape (v1alpha1 → v1beta1 → v1).
// Additive fields do not require a bump.
const (
	ProvenanceAPIVersion = "aicr.nvidia.com/v1alpha1"
	ProvenanceKind       = "BundleProvenance"
)

// provenanceFile is the YAML shape emitted at the bundle root. JSON
// struct tags are used because sigs.k8s.io/yaml routes through encoding/
// json on serialization — same tags work for both formats.
type provenanceFile struct {
	APIVersion     string         `json:"apiVersion"`
	Kind           string         `json:"kind"`
	VendoredCharts []VendorRecord `json:"vendoredCharts"`
}

// WriteProvenance sorts records by component name and writes
// provenance.yaml at the root of outputDir. Returns the absolute file
// path, byte size, and any error. records MUST be non-empty; callers
// guard the call so an empty vendor set produces no file.
//
// ctx is checked for cancellation before the filesystem write so a
// caller-initiated abort short-circuits the I/O. The marshaling and
// path-safety checks above the write are pure CPU and run regardless.
//
// Mode 0o644: provenance is audit content meant to be read by operators
// and downstream tooling (yank-list scanners), not a secret.
func WriteProvenance(ctx context.Context, outputDir string, records []VendorRecord) (string, int64, error) {
	if len(records) == 0 {
		return "", 0, errors.New(errors.ErrCodeInternal,
			"WriteProvenance: empty records slice")
	}

	sorted := make([]VendorRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	body, err := yaml.Marshal(provenanceFile{
		APIVersion:     ProvenanceAPIVersion,
		Kind:           ProvenanceKind,
		VendoredCharts: sorted,
	})
	if err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			"marshal provenance.yaml", err)
	}

	abs, err := deployer.SafeJoin(outputDir, ProvenanceFileName)
	if err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInvalidRequest,
			"provenance.yaml path unsafe", err)
	}

	if err = ctx.Err(); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeTimeout,
			"context cancelled before provenance write", err)
	}
	if err = os.WriteFile(abs, body, 0o644); err != nil { //nolint:gosec // audit content meant to be read, not a secret
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("write %s", filepath.Base(abs)), err)
	}
	return abs, int64(len(body)), nil
}
