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

//go:build unix && !linux

package bundler

import (
	stderrors "errors"
	"syscall"
)

// groupHasLiveMember reports whether group pgid still exists. macOS reports
// EPERM while the group's last members are exiting.
func groupHasLiveMember(pgid int) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	switch {
	case stderrors.Is(err, syscall.ESRCH):
		return false, nil
	case err == nil, stderrors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, err
	}
}
