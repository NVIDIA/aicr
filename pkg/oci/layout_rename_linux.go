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

	"golang.org/x/sys/unix"
)

// renameNoReplace renames oldName to newName within root and fails with
// EEXIST if newName exists, in one syscall: renameat2 with RENAME_NOREPLACE.
// A plain rename(2) would replace an empty directory that appeared after a
// check. A filesystem that does not implement the flag (EINVAL, ENOSYS,
// ENOTSUP) gets the two-step fallback.
func renameNoReplace(root *os.Root, oldName, newName string) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	conn, err := dir.SyscallConn()
	if err != nil {
		return err
	}
	var renameErr error
	if err = conn.Control(func(fd uintptr) {
		dirfd := int(fd) //nolint:gosec // a file descriptor fits in int
		renameErr = unix.Renameat2(dirfd, oldName, dirfd, newName, unix.RENAME_NOREPLACE)
	}); err != nil {
		return err
	}
	if stderrors.Is(renameErr, unix.EINVAL) || stderrors.Is(renameErr, unix.ENOSYS) ||
		stderrors.Is(renameErr, unix.ENOTSUP) || stderrors.Is(renameErr, unix.EOPNOTSUPP) {

		return renameCheckThenMove(root, oldName, newName)
	}
	return renameErr
}
