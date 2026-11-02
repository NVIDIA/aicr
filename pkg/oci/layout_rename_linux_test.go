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

//go:build linux

package oci

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestRenameNoReplaceUsesRenameat2 tells the single-syscall path from the
// check-then-rename fallback: only renameat2 reports EEXIST, while the
// fallback returns a bare fs.ErrExist.
func TestRenameNoReplaceUsesRenameat2(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"stage", "target"} {
		if err := os.Mkdir(filepath.Join(parent, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	probe := unix.Renameat2(unix.AT_FDCWD, filepath.Join(parent, "stage"),
		unix.AT_FDCWD, filepath.Join(parent, "target"), unix.RENAME_NOREPLACE)
	if stderrors.Is(probe, unix.EINVAL) || stderrors.Is(probe, unix.ENOSYS) ||
		stderrors.Is(probe, unix.ENOTSUP) || stderrors.Is(probe, unix.EOPNOTSUPP) {

		t.Skipf("renameat2 with RENAME_NOREPLACE is unavailable here: %v", probe)
	}
	if err = renameNoReplace(root, "stage", "target"); !stderrors.Is(err, unix.EEXIST) {
		t.Fatalf("renameNoReplace() error = %v, want EEXIST from renameat2", err)
	}
	if got := dirNames(t, parent); len(got) != 2 {
		t.Fatalf("parent entries = %v, want stage and target untouched", got)
	}
}
