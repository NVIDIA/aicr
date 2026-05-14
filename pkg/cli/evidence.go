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

package cli

import (
	"github.com/urfave/cli/v3"
)

// evidenceCmd is the parent verb-group for offline operations on
// recipe-evidence bundles produced by `aicr validate --emit-attestation`.
func evidenceCmd() *cli.Command {
	return &cli.Command{
		Name:     "evidence",
		Category: functionalCategoryName,
		Usage:    "Inspect and verify recipe evidence bundles.",
		Description: `Offline operations on recipe-evidence v1 bundles.

Bundles are produced by ` + "`aicr validate --emit-attestation`" + ` and consumed
by maintainers and CI to verify a recipe contribution without re-running
the validators against hardware they may not have access to.

Subcommands:

  verify  Verify a bundle's integrity claims.

See docs/design/007-recipe-evidence.md for the trust model.`,
		Commands: []*cli.Command{
			evidenceVerifyCmd(),
		},
	}
}
