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
	"io"
	"os"
	"strconv"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// readBoundedFile reads path into memory, capped at max bytes. The cap
// guards against attacker-influenced bundle roots (extracted from an
// untrusted archive, symlink-rich tarball, /proc symlink, NFS mount)
// where os.ReadFile would allocate the whole file before any size check
// fires. The +1 on the LimitReader lets us detect "at or over the cap"
// without reading the entire oversized payload.
func readBoundedFile(path, label string, max int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is bundle-local and validated by caller
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to read "+label, err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read "+label, err)
	}
	if int64(len(body)) > max {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			label+" exceeds maximum size of "+strconv.FormatInt(max, 10)+" bytes")
	}
	return body, nil
}
