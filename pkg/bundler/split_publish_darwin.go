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

//go:build darwin

package bundler

import "golang.org/x/sys/unix"

// renameNoReplace moves oldpath to newpath and fails if newpath exists, in
// one syscall: renamex_np with RENAME_EXCL. A plain rename(2) would replace
// an empty directory that appeared after the caller's check.
func renameNoReplace(oldpath, newpath string) error {
	return unix.RenamexNp(oldpath, newpath, unix.RENAME_EXCL)
}
