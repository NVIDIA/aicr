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
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	bundlercfg "github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
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

// resolveTypedOverrides parses the --set-json and --set-file flags into
// structured component-path overrides for list/object values that scalar
// --set cannot express (see #1161). --set-json decodes the value as JSON;
// --set-file reads the value from a file and decodes it as YAML (a JSON
// superset). Both reuse --set's "component:path" validation.
//
// The two flags are additive. When both target the same component:path,
// --set-file is collected first so an inline --set-json wins on that path,
// mirroring Helm's precedence of --set over -f value files. (urfave/cli does
// not expose the interleaved command-line order across two distinct flags, so
// this fixed precedence is the deterministic contract rather than raw argument
// order.) Within a single flag, the last entry for a given component:path
// wins. Returns nil when neither flag is set.
func resolveTypedOverrides(cmd *cli.Command) ([]bundlercfg.TypedComponentPath, error) {
	var out []bundlercfg.TypedComponentPath

	// Collect --set-file first so a later-applied --set-json on the same
	// component:path takes precedence (Helm-like: --set beats -f).
	//
	// ParseTypedOverrides / ParseValueOverridesJSON already return structured
	// ErrCodeInvalidRequest errors that name the offending spec (and, for value
	// decode/format failures, the flag), so propagate them as-is rather than
	// re-wrapping with the same code.
	if cmd.IsSet("set-file") {
		parsed, err := bundlercfg.ParseTypedOverrides(cmd.StringSlice("set-file"), "--set-file", decodeSetFileValue)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed...)
	}

	if cmd.IsSet("set-json") {
		parsed, err := bundlercfg.ParseValueOverridesJSON(cmd.StringSlice("set-json"))
		if err != nil {
			return nil, err
		}
		out = append(out, parsed...)
	}

	// The top-level "enabled" key is the bundler's component include/exclude
	// toggle, not a Helm chart value — it is honored only via scalar --set
	// (getSetEnabledOverride) and stripped before reaching chart values.
	// Routing it through the typed flags would neither toggle the component nor
	// strip the key: it would silently write a stray literal `enabled:` into the
	// component's values and ship a component the operator believed disabled.
	// Reject it loudly with a pointer to --set rather than emit a misconfigured
	// bundle. A nested chart value such as `gds.enabled` is unaffected — only
	// the exact toggle path is blocked.
	for _, tp := range out {
		if tp.Path == componentEnabledKey {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%q is the component enable/disable toggle and must be set with --set "+
					"(e.g. --set %s:%s=false), not --set-json/--set-file",
					componentEnabledKey, tp.Component, componentEnabledKey))
		}
	}

	return out, nil
}

// componentEnabledKey is the special value-override path that toggles whether a
// component is included in the bundle. It is consumed by the bundler's
// getSetEnabledOverride and is valid only on scalar --set (see #1161). Aliased
// to the bundler config's canonical constant so the CLI early-reject and the
// bundler's below-the-boundary guard never drift.
const componentEnabledKey = bundlercfg.ComponentEnabledKey

// decodeSetFileValue reads the value file at path (bounded to
// defaults.MaxSetFileBytes) and decodes it as YAML, a superset of JSON, for
// the --set-file flag. os.Open + io.LimitReader is used instead of
// os.ReadFile so an attacker-influenced path (e.g. a /proc symlink or a
// network mount) cannot OOM the process before the size check.
func decodeSetFileValue(path string) (any, error) {
	// Stat before Open so a non-regular path fails fast with a clear error
	// instead of hanging or being misreported. Opening a FIFO/named pipe blocks
	// in os.Open until a writer appears, which would hang the CLI/CI job; a
	// directory opens fine on Unix and only fails later in io.ReadAll with
	// EISDIR (misreported as an internal error). Rejecting every non-regular
	// mode (pipe, device, socket, directory) up front covers all of these.
	// os.Stat follows symlinks, so a symlink to a regular file is still
	// accepted.
	info, err := os.Stat(path)
	if err != nil {
		// Only a genuine "does not exist" is NOT_FOUND; permission denied or
		// transient I/O are bad operator input, not a missing resource — report
		// them as invalid request.
		if stderrors.Is(err, os.ErrNotExist) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, "cannot open --set-file value file: "+path, err)
		}
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "cannot open --set-file value file: "+path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"--set-file value path must be a regular file: "+path)
	}

	f, err := os.Open(path) //nolint:gosec // operator-supplied --set-file path, read bounded below
	if err != nil {
		// The file passed the regular-file check above; a failure here is a
		// race (removed/replaced) or permission/I/O issue. Keep the same
		// NOT_FOUND vs invalid-request split as the stat above.
		if stderrors.Is(err, os.ErrNotExist) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, "cannot open --set-file value file: "+path, err)
		}
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "cannot open --set-file value file: "+path, err)
	}
	defer func() { _ = f.Close() }()

	limited := io.LimitReader(f, defaults.MaxSetFileBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read --set-file value file: "+path, err)
	}
	if int64(len(data)) > defaults.MaxSetFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("--set-file value file %q exceeds %d bytes", path, defaults.MaxSetFileBytes))
	}

	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid YAML/JSON in --set-file value file: "+path, err)
	}
	return v, nil
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
