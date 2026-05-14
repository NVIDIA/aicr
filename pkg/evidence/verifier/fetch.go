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

package verifier

import (
	"context"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// MaterializedBundle is the verifier's view of a bundle on local disk.
// Only directory input is sourced today; OCI fetch and pointer-driven
// pull would populate additional fields here.
type MaterializedBundle struct {
	BundleDir string

	cleanup func()
}

// Cleanup releases temp resources. No-op for directory input.
func (m *MaterializedBundle) Cleanup() {
	if m == nil || m.cleanup == nil {
		return
	}
	m.cleanup()
	m.cleanup = nil
}

// MaterializeBundle dispatches on InputForm. Only InputFormDir is
// handled today; OCI fetch and pointer-driven pull land in follow-up
// slices. ctx is checked once up front so cancellation behaves the
// same as the rest of the pipeline, even though directory resolution
// itself is cheap.
func MaterializeBundle(ctx context.Context, opts VerifyOptions, form InputForm) (*MaterializedBundle, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "materialize canceled", err)
	}
	if form != InputFormDir {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "unsupported input form: "+string(form))
	}
	return materializeDir(opts.Input)
}

// materializeDir accepts either the summary-bundle root or a parent
// containing it. Bundles are recognized by recipe.yaml + manifest.json
// at the candidate root.
func materializeDir(input string) (*MaterializedBundle, error) {
	if hasBundleMarkers(input) {
		return &MaterializedBundle{BundleDir: filepath.Clean(input)}, nil
	}
	candidate := filepath.Join(input, attestation.SummaryBundleDirName)
	if hasBundleMarkers(candidate) {
		return &MaterializedBundle{BundleDir: filepath.Clean(candidate)}, nil
	}
	return nil, errors.New(errors.ErrCodeInvalidRequest,
		"directory "+input+" does not look like a summary bundle "+
			"(no recipe.yaml / manifest.json at root or under summary-bundle/)")
}

func hasBundleMarkers(dir string) bool {
	for _, f := range []string{attestation.RecipeFilename, attestation.ManifestFilename} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}
