// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

import "testing"

func TestTrustCmd_HasUpdateSubcommand(t *testing.T) {
	cmd := trustCmd()

	if cmd.Name != "trust" {
		t.Errorf("Name = %q, want %q", cmd.Name, "trust")
	}

	if len(cmd.Commands) == 0 {
		t.Fatal("trust command should have subcommands")
	}

	found := false
	for _, sub := range cmd.Commands {
		if sub.Name == "update" {
			found = true
			break
		}
	}
	if !found {
		t.Error("trust command missing 'update' subcommand")
	}
}
