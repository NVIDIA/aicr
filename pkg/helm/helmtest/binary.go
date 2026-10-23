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

package helmtest

import (
	"os"
	"os/exec"
	"testing"
)

// RequireBinary skips t when no helm binary is on PATH. When the CI
// environment variable is set it fails t instead, because CI installs the
// helm version pinned in .settings.yaml.
func RequireBinary(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI but not on PATH; the go-test action must install the pinned version from .settings.yaml (testing_tools.helm)")
		}
		t.Skip("helm not available; skipping live-render test")
	}
}
