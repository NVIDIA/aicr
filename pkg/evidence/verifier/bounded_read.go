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

	"github.com/NVIDIA/aicr/pkg/evidence/internal/boundedio"
)

// readBoundedFile reads path into memory, capped at max bytes and bounded by
// the caller's context.
//
// The size cap guards against attacker-influenced bundle roots (extracted from
// an untrusted archive, symlink-rich tarball, /proc symlink, NFS mount) where
// os.ReadFile would allocate the whole file before any size check fires. The
// context bound is what keeps `aicr evidence verify` from hanging forever on a
// dead NFS/FUSE mount: a size cap bounds bytes, not time, and a bare os.Open
// against a wedged mount blocks in the syscall indefinitely. See
// pkg/evidence/internal/boundedio for why that requires a real cancellation
// boundary rather than a ctx.Err() check between chunks.
func readBoundedFile(ctx context.Context, path, label string, max int64) ([]byte, error) {
	return boundedio.ReadFile(ctx, path, label, max)
}
