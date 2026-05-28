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

package os

import (
	"runtime"
	"testing"
)

// skipIfNotLinux skips the calling test on non-Linux platforms. The os
// collectors (grub/sysctl/kmod/release) read /proc/* and /usr/lib/os-release
// and degrade via slog.Warn on missing files rather than returning errors,
// so the existing `errors.Is(err, os.ErrNotExist)` skip branches in each
// integration test never fire on macOS/Windows. Use this helper from any
// test that depends on those Linux-only paths.
func skipIfNotLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("skipping Linux-only integration test on %s", runtime.GOOS)
	}
}
