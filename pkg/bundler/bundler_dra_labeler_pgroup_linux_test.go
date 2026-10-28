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

package bundler

import (
	"bytes"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// groupHasLiveMember reports whether a process in group pgid is not a zombie.
// Zombies count as gone, because a container PID 1 that never reaps orphans
// keeps them in the group indefinitely.
func groupHasLiveMember(pgid int) (bool, error) {
	paths, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return false, err
	}
	want := strconv.Itoa(pgid)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if stderrors.Is(err, fs.ErrNotExist) || stderrors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return false, err
		}
		// The command name may hold spaces and parentheses, so fields are
		// counted from the last ')'. The first is the state, the third pgrp.
		i := bytes.LastIndexByte(data, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(data[i+1:]))
		if len(fields) < 3 || fields[2] != want {
			continue
		}
		if fields[0] != "Z" && fields[0] != "X" {
			return true, nil
		}
	}
	return false, nil
}
