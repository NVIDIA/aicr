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
	"context"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// evidenceRequested reports whether the flag or the config field asks for the
// committed-pointer check. The environment is applied by the facade, and a
// false flag does not turn an enabled environment variable off.
func evidenceRequested(cmd *cli.Command, cfg *aicr.Config) bool {
	if cmd != nil && cmd.Bool(flagRequireEvidence) {
		return true
	}
	return cfg != nil && cfg.IsRequireEvidence()
}

// requirePublishedEvidence rejects a resolved recipe that has no committed
// evidence pointer. It is a no-op unless the flag, config, or environment
// opts in, so the default resolve path does not list the catalog again.
func requirePublishedEvidence(ctx context.Context, cmd *cli.Command, cfg *aicr.Config, client *aicr.Client, result *aicr.RecipeResult) error {
	required, err := aicr.PublishedEvidenceRequired("", evidenceRequested(cmd, cfg))
	if err != nil || !required {
		return err
	}
	return client.RequirePublishedEvidence(ctx, result)
}
