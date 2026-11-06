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

package upstreamvalidator

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// The vendored script writes its report to stdout and reads the checklist
// schemas from schemaDir; Run sets both for the duration of one call. They
// are package state because the upstream code is a command-line script, so
// one call runs at a time under mu.
var (
	mu        sync.Mutex
	stdout    io.Writer = os.Stdout
	schemaDir           = "docs"
)

// Lines of the vendored script's report that classify a failed run. The
// script loads the CNCF landscape before it examines any product and prints
// "Validating <path>..." once it has; an "Error: " line with no such line
// after it means the validator could not start, which Run reports as
// unavailable rather than as a rejected submission.
const (
	errorLinePrefix      = "Error: "
	validatingLinePrefix = "Validating "
)

// Run validates the product file at productPath, which must lie under a
// "vX.Y/<dir>/" path as upstream requires, against the checklist copies in
// schemas (the directory holding AIConformance-X.Y.yaml). It returns the
// validator's report verbatim, warnings included, whether or not it passed.
// The validator fetches the CNCF landscape and requests every http(s) URL
// the product cites, so it needs network access and takes as long as those
// requests do; a dead ctx returns while the validator finishes in the
// background, bounded by its own request timeouts.
//
// Errors:
//   - ErrCodeUnavailable when the validator could not start because the CNCF
//     landscape could not be fetched or parsed.
//   - ErrCodeInternal when the validator rejected the product, or when
//     productPath or schemas is empty.
//   - ErrCodeTimeout, or ErrCodeCanceled on cancellation, when ctx ends
//     before the validator does.
func Run(ctx context.Context, productPath, schemas string) (string, error) {
	if productPath == "" || schemas == "" {
		return "", errors.New(errors.ErrCodeInternal, "upstream validator: product path and schema directory are required")
	}
	if err := ctx.Err(); err != nil {
		return "", errors.WrapCtxErr(err, errors.ErrCodeTimeout, "upstream validator not started")
	}
	type outcome struct {
		code   int
		output string
	}
	done := make(chan outcome, 1)
	go func() {
		mu.Lock()
		defer mu.Unlock()
		var buf bytes.Buffer
		stdout, schemaDir = &buf, filepath.Clean(schemas)
		defer func() { stdout, schemaDir = os.Stdout, "docs" }()
		code := run([]string{productPath})
		done <- outcome{code: code, output: buf.String()}
	}()
	select {
	case <-ctx.Done():
		return "", errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "upstream validator did not finish")
	case out := <-done:
		if out.code == 0 {
			return out.output, nil
		}
		return out.output, classify(out.output)
	}
}

// classify maps a failed run to its error: the report's own lines are the
// message, since upstream states each rejection on one line.
func classify(output string) error {
	trimmed := strings.TrimSpace(output)
	if !strings.Contains(trimmed, validatingLinePrefix) {
		for _, line := range strings.Split(trimmed, "\n") {
			if strings.HasPrefix(line, errorLinePrefix) {
				return errors.New(errors.ErrCodeUnavailable,
					"upstream validator could not start: "+strings.TrimPrefix(line, errorLinePrefix))
			}
		}
	}
	return errors.New(errors.ErrCodeInternal, "upstream validator rejected the submission:\n"+trimmed)
}
