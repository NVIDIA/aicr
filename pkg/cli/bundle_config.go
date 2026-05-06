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

	"github.com/urfave/cli/v3"

	appcfg "github.com/NVIDIA/aicr/pkg/config"
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
// nil input yields a nil result; an explicitly empty slice (e.g. `set: []`
// in config) yields an empty (non-nil) slice — preserving the user's
// intent to clear a list.
func stringSliceFlagOrConfig(cmd *cli.Command, flagName string, fallback []string) []string {
	if cmd.IsSet(flagName) {
		v := cmd.StringSlice(flagName)
		if len(fallback) > 0 {
			slog.Info("CLI flag replacing config value", "flag", flagName, "configCount", len(fallback), "overrideCount", len(v))
		}
		return copyStrings(v)
	}
	return copyStrings(fallback)
}

// resolveNodeSelector returns the parsed map for a CLI selector flag,
// preferring CLI input over the supplied fallback map. Returns a defensive
// copy in either path so callers cannot mutate the loaded config's map.
//
// nil fallback yields nil; an explicitly empty config map yields an empty
// (non-nil) map — preserving the user's intent to clear a default selector.
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
	return copyStringMap(fallback), nil
}

// bundleSpec returns cfg.Spec.Bundle if cfg is non-nil, else nil.
func bundleSpec(cfg *appcfg.AICRConfig) *appcfg.BundleSpec {
	if cfg == nil {
		return nil
	}
	return cfg.Spec.Bundle
}

func bundleRecipeInput(b *appcfg.BundleSpec) string {
	if b == nil || b.Input == nil {
		return ""
	}
	return b.Input.Recipe
}

func bundleOutputTarget(b *appcfg.BundleSpec) string {
	if b == nil || b.Output == nil {
		return ""
	}
	return b.Output.Target
}

func bundleOutputImageRefs(b *appcfg.BundleSpec) string {
	if b == nil || b.Output == nil {
		return ""
	}
	return b.Output.ImageRefs
}

func bundleDeploymentDeployer(b *appcfg.BundleSpec) string {
	if b == nil || b.Deployment == nil {
		return ""
	}
	return b.Deployment.Deployer
}

func bundleDeploymentRepo(b *appcfg.BundleSpec) string {
	if b == nil || b.Deployment == nil {
		return ""
	}
	return b.Deployment.Repo
}

// copyStrings returns a defensive copy of s. nil input yields nil; an
// explicitly empty slice yields an empty (non-nil) slice — preserving the
// caller's distinction between "unset" and "explicitly cleared".
func copyStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// copyStringMap returns a defensive copy of m. nil input yields nil; an
// explicitly empty map yields an empty (non-nil) map.
func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func bundleDeploymentSet(b *appcfg.BundleSpec) []string {
	if b == nil || b.Deployment == nil {
		return nil
	}
	return copyStrings(b.Deployment.Set)
}

func bundleDeploymentDynamic(b *appcfg.BundleSpec) []string {
	if b == nil || b.Deployment == nil {
		return nil
	}
	return copyStrings(b.Deployment.Dynamic)
}

func bundleSystemNodeSelector(b *appcfg.BundleSpec) map[string]string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return copyStringMap(b.Scheduling.SystemNodeSelector)
}

func bundleSystemNodeTolerations(b *appcfg.BundleSpec) []string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return copyStrings(b.Scheduling.SystemNodeTolerations)
}

func bundleAcceleratedNodeSelector(b *appcfg.BundleSpec) map[string]string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return copyStringMap(b.Scheduling.AcceleratedNodeSelector)
}

func bundleAcceleratedNodeTolerations(b *appcfg.BundleSpec) []string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return copyStrings(b.Scheduling.AcceleratedNodeTolerations)
}

func bundleWorkloadGate(b *appcfg.BundleSpec) string {
	if b == nil || b.Scheduling == nil {
		return ""
	}
	return b.Scheduling.WorkloadGate
}

func bundleWorkloadSelector(b *appcfg.BundleSpec) map[string]string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return copyStringMap(b.Scheduling.WorkloadSelector)
}

func bundleSchedulingNodes(b *appcfg.BundleSpec) int {
	if b == nil || b.Scheduling == nil {
		return 0
	}
	return b.Scheduling.Nodes
}

func bundleSchedulingStorageClass(b *appcfg.BundleSpec) string {
	if b == nil || b.Scheduling == nil {
		return ""
	}
	return b.Scheduling.StorageClass
}

func bundleAttestEnabled(b *appcfg.BundleSpec) bool {
	if b == nil || b.Attestation == nil {
		return false
	}
	return b.Attestation.Enabled
}

func bundleCertIDRegexp(b *appcfg.BundleSpec) string {
	if b == nil || b.Attestation == nil {
		return ""
	}
	return b.Attestation.CertificateIdentityRegexp
}

func bundleOIDCDeviceFlow(b *appcfg.BundleSpec) bool {
	if b == nil || b.Attestation == nil {
		return false
	}
	return b.Attestation.OIDCDeviceFlow
}

func bundleRegistryInsecureTLS(b *appcfg.BundleSpec) bool {
	if b == nil || b.Registry == nil {
		return false
	}
	return b.Registry.InsecureTLS
}

func bundleRegistryPlainHTTP(b *appcfg.BundleSpec) bool {
	if b == nil || b.Registry == nil {
		return false
	}
	return b.Registry.PlainHTTP
}
