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

package main

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestLoadRecipeSourcesRejectsSymlinkToNonRegularFile(t *testing.T) {
	// Opening a FIFO for read blocks until a writer appears, so a symlinked
	// FIFO must fail closed instead of hanging BOM generation: sources are
	// opened O_NONBLOCK and the regular-file requirement is checked by fstat
	// on the opened descriptor, so any non-regular resolved target is
	// rejected with no stat-to-open race window.
	root := writeRecipeTree(t, nil)
	// The FIFO lives INSIDE the recipes root (non-.yaml, so the walk never
	// collects the FIFO itself): resolution stays confined and the escape
	// rejection cannot be what saves us — only the non-regular check can.
	fifo := filepath.Join(root, "recipes", "overlays", "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if err := os.Symlink("fifo", filepath.Join(root, "recipes", "overlays", "hang.yaml")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := loadRecipeSources(context.Background(), root)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("loadRecipeSources read a symlinked FIFO as a recipe source")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("error code = %v, want %v", err, errors.ErrCodeInvalidRequest)
		}
		if !strings.Contains(err.Error(), "non-regular") {
			t.Errorf("error %q does not name the non-regular target", err.Error())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("loadRecipeSources hung on a symlinked FIFO (blocked open)")
	}
}
