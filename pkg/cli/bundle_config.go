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

	corev1 "k8s.io/api/core/v1"

	"github.com/urfave/cli/v3"

	bundlercfg "github.com/NVIDIA/aicr/pkg/bundler/config"
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

// resolveTolerations returns the final tolerations slice for a flag,
// preferring CLI input over the typed fallback (already parsed from
// config). When neither source supplies a value, returns
// snapshotter.DefaultTolerations() — matching the parser's
// nil-input → DefaultTolerations behavior so callers never see a nil
// toleration slice from this entry point.
func resolveTolerations(cmd *cli.Command, flagName string, fallback []corev1.Toleration) ([]corev1.Toleration, error) {
	if cmd.IsSet(flagName) {
		raw := cmd.StringSlice(flagName)
		if len(fallback) > 0 {
			slog.Info("CLI flag replacing config value", "flag", flagName,
				"configCount", len(fallback), "overrideCount", len(raw))
		}
		parsed, err := snapshotter.ParseTolerations(raw)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid --%s", flagName), err)
		}
		return parsed, nil
	}
	if fallback == nil {
		return snapshotter.DefaultTolerations(), nil
	}
	return fallback, nil
}

// resolveComponentPaths returns the final component-path slice for a
// flag, preferring CLI input (parsed via parser) over the typed
// fallback (already parsed from config in BundleSpec.Resolve).
func resolveComponentPaths(cmd *cli.Command, flagName string,
	fallback []bundlercfg.ComponentPath,
	parser func([]string) ([]bundlercfg.ComponentPath, error),
) ([]bundlercfg.ComponentPath, error) {

	if cmd.IsSet(flagName) {
		raw := cmd.StringSlice(flagName)
		if len(fallback) > 0 {
			slog.Info("CLI flag replacing config value", "flag", flagName,
				"configCount", len(fallback), "overrideCount", len(raw))
		}
		parsed, err := parser(raw)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid --%s", flagName), err)
		}
		return parsed, nil
	}
	return fallback, nil
}

// resolveTaint returns the final taint pointer for a flag, preferring
// CLI input (parsed via snapshotter.ParseTaint) over the typed fallback
// (already parsed from config in BundleSpec.Resolve). An explicitly
// empty CLI value masks the fallback and yields nil — matching the
// pre-refactor behavior where stringFlagOrConfig would surface "" to
// the caller, the caller would skip parsing, and the taint would
// remain unset.
func resolveTaint(cmd *cli.Command, flagName string, fallback *corev1.Taint) (*corev1.Taint, error) {
	if cmd.IsSet(flagName) {
		raw := cmd.String(flagName)
		if raw == "" {
			//nolint:nilnil // a nil taint is the documented "no gate" state.
			return nil, nil
		}
		t, err := snapshotter.ParseTaint(raw)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid --%s", flagName), err)
		}
		if fallback != nil {
			slog.Info("CLI flag overriding config value", "flag", flagName)
		}
		return t, nil
	}
	return fallback, nil
}

// resolveDeployer returns the final deployer for the --deployer flag,
// preferring CLI input over the typed fallback. When the CLI flag is
// set to an empty string OR neither source supplies a value, returns
// bundlercfg.DeployerHelm — matching the pre-refactor behavior where
// the empty deployer string fell through to the Helm default rather
// than being passed to ParseDeployerType (which rejects "").
func resolveDeployer(cmd *cli.Command, fallback bundlercfg.DeployerType) (bundlercfg.DeployerType, error) {
	const flagName = "deployer"
	if cmd.IsSet(flagName) {
		raw := cmd.String(flagName)
		if raw == "" {
			return bundlercfg.DeployerHelm, nil
		}
		if fallback != "" && string(fallback) != raw {
			slog.Info("CLI flag overriding config value", "flag", flagName,
				"config", string(fallback), "override", raw)
		}
		d, err := bundlercfg.ParseDeployerType(raw)
		if err != nil {
			return "", errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid --%s value", flagName), err)
		}
		return d, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return bundlercfg.DeployerHelm, nil
}
