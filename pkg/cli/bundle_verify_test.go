// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package cli

import (
	"testing"

	"github.com/urfave/cli/v3"
)

func TestBundleVerifyCmd_HasExpectedFlags(t *testing.T) {
	cmd := bundleVerifyCmd()

	if cmd.Name != "verify" {
		t.Errorf("Name = %q, want %q", cmd.Name, "verify")
	}

	expectedFlags := []string{"min-trust-level", "require-creator", "cli-version-constraint", "certificate-identity-regexp", "format"}
	for _, name := range expectedFlags {
		found := false
		for _, f := range cmd.Flags {
			if f.Names()[0] == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected flag: --%s", name)
		}
	}
}

func TestBundleVerifyCmd_MinTrustLevelDefault(t *testing.T) {
	cmd := bundleVerifyCmd()

	for _, f := range cmd.Flags {
		if f.Names()[0] == "min-trust-level" {
			// Check it's a StringFlag with default "max"
			sf, ok := f.(*cli.StringFlag)
			if !ok {
				t.Fatal("min-trust-level should be a StringFlag")
			}
			if sf.Value != "max" {
				t.Errorf("min-trust-level default = %q, want %q", sf.Value, "max")
			}
			return
		}
	}
	t.Error("min-trust-level flag not found")
}
