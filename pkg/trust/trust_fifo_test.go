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

//go:build unix

package trust

import (
	stderrors "errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestLoadTrustedMaterialFromFile_FIFO is the regression guard for the
// FIFO-blocking hazard: a blocking open of a named pipe with no writer would
// hang in the kernel open(), so LoadTrustedMaterialFromFile opens with
// O_NONBLOCK and rejects non-regular files by fstat-ing the descriptor. If that
// regresses to a blocking open, this test hangs (caught by the package test
// timeout) rather than failing fast. A passing run proves both that the FIFO is
// rejected as ErrCodeInvalidRequest and that the call returns without blocking.
// This file is //go:build unix, so a Mkfifo failure is a broken environment,
// not an unsupported platform: fail hard rather than silently skip the guard.
func TestLoadTrustedMaterialFromFile_FIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "trusted_root.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo failed: %v", err)
	}

	_, err := LoadTrustedMaterialFromFile(fifo)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("want ErrCodeInvalidRequest for a FIFO, got %v", err)
	}
}
