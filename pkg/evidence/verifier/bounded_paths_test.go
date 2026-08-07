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
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// TestVerifierFilesystemPathsAreBounded is the load-bearing guarantee of
// issue #2083: every filesystem entry point on the verify path must sit
// behind a cancellation boundary, so a dead NFS/FUSE mount surfaces as an
// error instead of hanging the process forever.
//
// It runs each entry point against a real, valid bundle — so an unbounded
// implementation would succeed instantly — with an already-canceled context.
// Anything that returns success has performed I/O the caller could not abort,
// which is exactly the defect. Having a ctx parameter is not enough: a
// ctx.Err() check between chunks or between walk entries never runs when the
// syscall itself is wedged in the kernel.
func TestVerifierFilesystemPathsAreBounded(t *testing.T) {
	dir := summaryDirOf(t, buildTestBundle(t))
	mat := &MaterializedBundle{BundleDir: dir}

	// A real predicate, so the phase-digest pass actually reads ctrf/ files.
	// An empty one would short-circuit before touching the filesystem and the
	// case would vacuously pass.
	pred, err := loadUnsignedPredicate(context.Background(), mat)
	if err != nil {
		t.Fatalf("loadUnsignedPredicate: %v", err)
	}
	if len(pred.Phases) == 0 {
		t.Fatal("test bundle has no phases — the phase-digest case would not exercise any I/O")
	}

	tests := []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"readBoundedFile", func(ctx context.Context) error {
			_, err := readBoundedFile(ctx, dir+"/"+attestation.ManifestFilename,
				"manifest.json", defaults.MaxManifestFileBytes)
			return err
		}},
		{"loadUnsignedPredicate", func(ctx context.Context) error {
			_, err := loadUnsignedPredicate(ctx, mat)
			return err
		}},
		{"CheckPhaseDigestsContext", func(ctx context.Context) error {
			_, err := CheckPhaseDigestsContext(ctx, mat, pred)
			return err
		}},
		{"hashFile", func(ctx context.Context) error {
			_, err := hashFile(ctx, dir, attestation.RecipeFilename, 0)
			return err
		}},
		{"hashAndCaptureFile", func(ctx context.Context) error {
			_, _, err := hashAndCaptureFile(ctx, dir, attestation.RecipeFilename, 0)
			return err
		}},
		{"findExtras", func(ctx context.Context) error {
			_, err := findExtras(ctx, dir, nil)
			return err
		}},
		{"checkInventoryCaptureRecipe", func(ctx context.Context) error {
			_, _, err := checkInventoryCaptureRecipe(ctx, mat, "sha256:deadbeef")
			return err
		}},
		{"VerifySignature", func(ctx context.Context) error {
			_, err := VerifySignature(ctx, mat, VerifyOptions{})
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := tt.run(ctx)
			if err == nil {
				t.Fatal("completed against a canceled context — the I/O is not behind a cancellation boundary")
			}
			// An explicitly canceled caller must produce exactly
			// ErrCodeCanceled. Accepting ErrCodeTimeout here would let an
			// operator abort keep being reported as a retryable fault, which
			// is the defect this bounding work exists to fix — a looser
			// assertion would pass while the bug was still present.
			if !stderrors.Is(err, errors.New(errors.ErrCodeCanceled, "")) {
				t.Errorf("abort misclassified (want ErrCodeCanceled): %v", err)
			}
		})
	}
}
