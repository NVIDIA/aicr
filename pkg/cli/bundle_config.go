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

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// boolFlagOrConfig returns the CLI flag value when explicitly set on the
// command (or via env-var Source binding), otherwise the fallback. Logs an
// INFO line when the CLI value differs from a non-default fallback.
func boolFlagOrConfig(cmd *cli.Command, flagName string, fallback bool) bool {
	if cmd.IsSet(flagName) {
		v := cmd.Bool(flagName)
		if v != fallback {
			slog.Info("CLI flag overriding config value", "flag", flagName, "config", fallback, "override", v)
		}
		return v
	}
	return fallback
}

// stringSliceFlagOrConfig returns the CLI slice value when explicitly set,
// otherwise the fallback slice. Per the agreed design, CLI replaces config
// rather than appending. Returns a defensive copy so callers cannot mutate
// the loaded config's backing slice.
//
// nil input yields nil; an explicitly empty slice (e.g. `set: []` in
// config) yields an empty (non-nil) slice — preserving the user's intent
// to clear a list.
func stringSliceFlagOrConfig(cmd *cli.Command, flagName string, fallback []string) []string {
	if cmd.IsSet(flagName) {
		v := cmd.StringSlice(flagName)
		if len(fallback) > 0 {
			slog.Info("CLI flag replacing config value", "flag", flagName, "configCount", len(fallback), "overrideCount", len(v))
		}
		return slices.Clone(v)
	}
	return slices.Clone(fallback)
}

// resolveNodeSelector returns the parsed map for a CLI selector flag,
// preferring CLI input over the supplied fallback map. Errors from
// parsing carry ErrCodeInvalidRequest. The fallback is defensively
// cloned even though spec accessors already clone — this is the
// canonical entry point and should not require the caller to remember
// who copies what.
func resolveNodeSelector(cmd *cli.Command, flagName string, fallback map[string]string) (map[string]string, error) {
	if cmd.IsSet(flagName) {
		parsed, err := snapshotter.ParseNodeSelectors(cmd.StringSlice(flagName))
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid --%s", flagName))
		}
		if len(fallback) > 0 {
			slog.Info("CLI flag replacing config selector", "flag", flagName)
		}
		return parsed, nil
	}
	return maps.Clone(fallback), nil
}
