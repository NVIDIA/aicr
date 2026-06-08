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

package catalog

import (
	"context"
	"os"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// SignOptions configures a Sign call.
type SignOptions struct {
	// Attester performs the actual signing. Use attestation.NewNoOpAttester()
	// in tests; use attestation.ResolveAttester in production.
	Attester attestation.Attester

	// Output is the file path to write the Sigstore bundle to.
	// If empty or Attester returns nil bytes, no file is written.
	Output string

	// ToolVersion is the aicr version string (e.g. "v1.2.3").
	ToolVersion string
}

// SignResult is returned by Sign.
type SignResult struct {
	// Digest is the hex-encoded SHA-256 of the combined catalog content.
	Digest string

	// BundleJSON is the serialized Sigstore bundle, or nil when NoOpAttester
	// is used.
	BundleJSON []byte
}

// Sign computes the catalog digest, signs it via opts.Attester, and writes
// the bundle to opts.Output (skipped when BundleJSON is nil).
func Sign(ctx context.Context, provider recipe.DataProvider, opts SignOptions) (*SignResult, error) {
	if opts.Attester == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "attester is required")
	}

	subject, err := ComputeDigest(ctx, provider)
	if err != nil {
		return nil, err
	}

	subject.Metadata = attestation.StatementMetadata{
		Recipe:        catalogSubjectName,
		RecipeSource:  provider.Source(recipe.RegistryFileName),
		BuildType:     attestation.CatalogBuildType,
		ToolVersion:   opts.ToolVersion,
		Deterministic: true,
	}

	bundleJSON, err := opts.Attester.Attest(ctx, subject)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "catalog attestation signing failed")
	}

	if bundleJSON != nil && opts.Output != "" {
		if writeErr := writeBundle(opts.Output, bundleJSON); writeErr != nil {
			return nil, writeErr
		}
	}

	return &SignResult{
		Digest:     subject.Digest[digestAlgoSHA256],
		BundleJSON: bundleJSON,
	}, nil
}

func writeBundle(path string, data []byte) error {
	f, err := os.Create(path) //nolint:gosec // caller-controlled output path
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create bundle output file", err)
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write bundle", writeErr)
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to close bundle file", closeErr)
	}
	return nil
}
