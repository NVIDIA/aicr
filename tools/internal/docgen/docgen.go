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

// Package docgen holds shared plumbing for the hermetic Markdown doc generators
// (tools/health, tools/tuning): writing a rendered body to a file with the
// repo's writable-Close() error contract.
package docgen

import (
	"io"
	"os"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// WriteRendered opens path with the given flags and invokes render against it,
// checking the writable Close() error per the repo's file-handle rule. A render
// error is returned as-is (callers' render funcs already return a structured
// error, so re-wrapping would double-code it).
func WriteRendered(path string, flag int, render func(io.Writer) error) error {
	f, err := os.OpenFile(path, flag, 0o644) //nolint:gosec // path is a trusted operator flag or runner-supplied (e.g. $GITHUB_STEP_SUMMARY), never attacker-controlled
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "open "+path, err)
	}
	renderErr := render(f)
	closeErr := f.Close()
	if renderErr != nil {
		return renderErr
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "close "+path, closeErr)
	}
	return nil
}
