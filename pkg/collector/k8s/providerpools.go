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

package k8s

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Provider node-pool projections (ADR-015 DD3).
//
// Each cloud expresses GPU-driver ownership in a different control-plane
// object with different semantics, so the projection layer is deliberately
// per-provider: a provider gets its own projector reading that provider's
// documented output format and emitting its own namespaced measurement
// subtype (aks-gpu-pools today; a GKE analog would add gke-gpu-pools beside
// it, projecting gpuDriverInstallationConfig from
// `gcloud container node-pools describe -o json`). Do not widen an existing
// provider's subtype or invent a cross-provider pools schema — unified
// schemas blur the fail-closed constraint semantics profile declarations
// depend on.
//
// The projection does NOT run inside a collector: it is pure file
// processing, so pkg/snapshotter projects the operator-supplied file up
// front (fail-loud, before any deploy or collection) and attaches the
// subtype to the K8s measurement — controller-side in agent Job mode,
// in-process in local mode. What IS shared across providers:
//   - this bounded, fail-loud file reader;
//   - the orchestration-layer project-then-attach flow in pkg/snapshotter
//     (attachProviderProjection / the per-provider merge helpers), which keeps explicit operator
//     input out of the snapshotter's degrade-to-warning collector policy;
//   - additive sibling CLI flags per provider (--aks-gpu-pools, ...), never
//     a generic flag with a provider discriminator.

// poolsFileReadBufferSize sizes the chunked-read buffer so cancellation is
// checked between chunks instead of once around a single blocking ReadAll.
const poolsFileReadBufferSize = 64 * 1024

// readBoundedPoolsFile loads an operator-supplied provider dump with the
// project's bounded-read discipline, descriptor-first so the checks and the
// read cannot race a path swap (the verifier's readBoundedFileContext
// pattern): open with O_NOFOLLOW|O_NONBLOCK (a symlink is rejected so we
// read what the operator actually named; a FIFO substituted for the file
// cannot block the open), validate the OPENED descriptor is a regular file
// within maxBytes, then stream with cancellation checks between chunks —
// io.LimitReader alone bounds bytes, not time. The read is additionally
// bounded by defaults.FileReadTimeout under the caller's context. desc
// names the input in errors (e.g. "AKS GPU pools").
func readBoundedPoolsFile(ctx context.Context, path, desc string, maxBytes int64) ([]byte, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout,
			fmt.Sprintf("%s file read canceled", desc), ctxErr)
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()

	file, err := os.OpenFile( //nolint:gosec // G703 -- path from an explicit CLI flag
		filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if stderrors.Is(err, syscall.ELOOP) {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s path %q is a symlink, not a regular file", desc, path), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to open %s file %q", desc, path), err)
	}
	defer func() {
		_ = file.Close() // read-only handle
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to inspect %s file %q", desc, path), err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s path %q is not a regular file", desc, path))
	}
	if info.Size() > maxBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s file %q exceeds %d bytes", desc, path, maxBytes))
	}

	raw := make([]byte, 0, info.Size())
	buffer := make([]byte, poolsFileReadBufferSize)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				fmt.Sprintf("%s file read canceled", desc), ctxErr)
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			if int64(len(raw))+int64(n) > maxBytes {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("%s file %q exceeds %d bytes", desc, path, maxBytes))
			}
			raw = append(raw, buffer[:n]...)
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				break
			}
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to read %s file %q", desc, path), readErr)
		}
	}
	return raw, nil
}
