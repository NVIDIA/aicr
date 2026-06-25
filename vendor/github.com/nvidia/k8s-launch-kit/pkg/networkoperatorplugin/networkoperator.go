// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/plugin"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	PluginName    = "network-operator"
	PluginVersion = "1.0.0"
)

type NetworkOperatorPlugin struct {
	// Groups is the list of source-group identifiers passed via
	// `--groups <a,b,...>` (matched case-sensitively against
	// `cluster-config.yaml`'s `clusterConfig[].identifier`). Empty when
	// the user didn't supply the flag.
	Groups []string
	// GpuType is the value passed via `--gpu-type <X>` (matched
	// case-insensitively against `gpuType`). Empty when the user didn't
	// supply the flag. Mutually exclusive with Groups.
	GpuType      string
	NodeSelector map[string]string
	RESTConfig   *rest.Config

	// KeepNamespace, when true, suppresses teardown of the
	// nicconfigdaemon.Namespace at the end of DiscoverClusterConfig — useful
	// for debugging a failed discovery.
	KeepNamespace bool

	// CollapseNicRails (default true, set from --collapse-nic-rails) makes
	// discovery advertise one rail per NIC: multi-plane NICs collapse to their
	// master PF while genuinely dual-port NICs keep a rail per port. False
	// restores the legacy one-rail-per-PF behaviour (handy on dev setups).
	CollapseNicRails bool

	// NetworkOperator is the resolved NetworkOperator config used by
	// DeployProfile to drive the helm install in Phase 0. Populated by
	// the launcher after ApplyOptionsToConfig has merged the catalog with
	// the user's l8k-config.yaml.
	NetworkOperator *config.NetworkOperatorConfig
	// DOCAVersion is the catalog-resolved docaDriver.version. Passed
	// through DeployProfile into preflight so the NCP component-versions
	// check can compare against ofedDriver.version on the live CR.
	DOCAVersion string
	// OverwriteExisting forwards the `--overwrite-existing` flag through
	// DeployProfile so helm install can promote to `helm upgrade --install`
	// when a release already exists with different values.
	OverwriteExisting bool
	// DryRun forwards the `--dry-run` flag through DeployProfile so
	// helm install can run in server-side dry-run mode.
	DryRun bool
}

func (p *NetworkOperatorPlugin) GetName() string {
	return PluginName
}

func (p *NetworkOperatorPlugin) GetVersion() string {
	return PluginVersion
}

func (p *NetworkOperatorPlugin) ProfileConfiguredInCmd(options options.Options) bool {
	return options.Fabric != "" || options.DeploymentType != ""
}

func (p *NetworkOperatorPlugin) BuildProfileFromOptions(options options.Options, profile *config.Profile) error {
	profile.Fabric = options.Fabric
	profile.Deployment = options.DeploymentType
	profile.Multirail = options.Multirail


	// Build SpectrumX nested struct if enabled
	if options.SpectrumX {
		profile.SpectrumX = &config.ProfileSpectrumX{
			Enable:         true,
			SPCXVersion:    options.SPCXVersion,
			MultiplaneMode: options.MultiplaneMode,
			NumberOfPlanes: options.NumberOfPlanes,
		}
	}

	log.Log.V(1).Info("Built profile for plugin", "plugin", p.GetName(), "profile", profile)
	return nil
}

// ApplyNetworkOperatorRelease resolves the effective Network Operator
// release (CLI flag > networkOperator.selectedRelease in config) and writes
// the catalog-derived version, componentVersion, repository, and DOCA
// driver version onto fullConfig. Shared between the generate flow's
// ApplyOptionsToConfig and the discover flow that also needs to honour
// --network-operator-release when emitting cluster-config.yaml. CLI-side
// validation in pkg/cmd/root.go has already rejected unknown releases
// supplied via flag; config-file values are validated here.
func ApplyNetworkOperatorRelease(options options.Options, fullConfig *config.LaunchKitConfig) error {
	effectiveRelease := options.NetworkOperatorRelease
	if effectiveRelease == "" && fullConfig.NetworkOperator != nil {
		effectiveRelease = fullConfig.NetworkOperator.SelectedRelease
	}
	if effectiveRelease == "" {
		return nil
	}
	rel, ok := LookupRelease(effectiveRelease)
	if !ok {
		return fmt.Errorf("unsupported network operator release %q; supported: %v",
			effectiveRelease, SupportedReleases())
	}
	if fullConfig.NetworkOperator == nil {
		fullConfig.NetworkOperator = &config.NetworkOperatorConfig{}
	}
	fullConfig.NetworkOperator.SelectedRelease = effectiveRelease
	fullConfig.NetworkOperator.Version = rel.NetworkOperator.Version
	fullConfig.NetworkOperator.ComponentVersion = rel.NetworkOperator.ComponentVersion
	fullConfig.NetworkOperator.Repository = rel.NetworkOperator.Repository
	fullConfig.NetworkOperator.OperatorRepository = rel.NetworkOperator.OperatorRepository
	fullConfig.NetworkOperator.HelmRepoURL = rel.NetworkOperator.HelmRepoURL
	if fullConfig.DOCADriver == nil {
		fullConfig.DOCADriver = &config.DOCADriverConfig{}
	}
	fullConfig.DOCADriver.Version = rel.DOCADriver.Version
	return nil
}

// ApplyOptionsToConfig applies CLI options to the configuration, overriding file values.
// CLI flags take precedence over config file values for any explicitly set option.
func (p *NetworkOperatorPlugin) ApplyOptionsToConfig(options options.Options, fullConfig *config.LaunchKitConfig) error {
	if err := ApplyNetworkOperatorRelease(options, fullConfig); err != nil {
		return err
	}

	// Apply network operator namespace override from CLI
	if options.NetworkOperatorNamespace != "" {
		if fullConfig.NetworkOperator == nil {
			fullConfig.NetworkOperator = &config.NetworkOperatorConfig{}
		}
		fullConfig.NetworkOperator.Namespace = options.NetworkOperatorNamespace
	}

	// Apply image pull secrets override from CLI
	if len(options.ImagePullSecrets) > 0 {
		if fullConfig.NetworkOperator == nil {
			fullConfig.NetworkOperator = &config.NetworkOperatorConfig{}
		}
		fullConfig.NetworkOperator.ImagePullSecrets = options.ImagePullSecrets
	}

	// Apply network namespaces override from CLI.
	if len(options.NetworkNamespaces) > 0 {
		fullConfig.NetworkNamespaces = options.NetworkNamespaces
	}
	// Back-compat: a legacy scalar podNamespace becomes the sole network
	// namespace when the list isn't set.
	if len(fullConfig.NetworkNamespaces) == 0 && fullConfig.PodNamespace != "" {
		fullConfig.NetworkNamespaces = []string{fullConfig.PodNamespace}
	}
	// Default to a single "default" namespace if neither config nor CLI set it.
	if len(fullConfig.NetworkNamespaces) == 0 {
		fullConfig.NetworkNamespaces = []string{"default"}
	}
	// PodNamespace is the "current" namespace scalar the templates read; the
	// renderer overrides it per network namespace while fanning out. Seed it
	// with the first entry so non-fanned-out templates render deterministically.
	fullConfig.PodNamespace = fullConfig.NetworkNamespaces[0]

	// Default NicConfigurationOperator if not set
	if fullConfig.NicConfigurationOperator == nil {
		fullConfig.NicConfigurationOperator = &config.NicConfigurationOperatorConfig{
			DeployNicInterfaceNameTemplate: true,
			RdmaPrefix:                     "rdma_r%rail_id%",
			NetdevPrefix:                   "eth_r%rail_id%",
		}
	}

	if fullConfig.Profile == nil {
		return nil
	}

	// Apply base profile overrides from CLI (non-empty strings and true bools override config)
	if options.Fabric != "" {
		fullConfig.Profile.Fabric = options.Fabric
	}
	if options.DeploymentType != "" {
		fullConfig.Profile.Deployment = options.DeploymentType
	}
	// `MultirailSet` is true only when the user explicitly passed
	// `--multirail` (regardless of value). Without it, the bool zero
	// value can't be distinguished from "not passed", so a user who
	// did NOT pass the flag would clobber the hardware default of
	// true with `false`. See `pkg/options.Options.MultirailSet`.
	if options.MultirailSet {
		fullConfig.Profile.Multirail = options.Multirail
	}

	// Apply workload manifest override from CLI
	if options.WorkloadManifest != "" {
		if fullConfig.Workload == nil {
			fullConfig.Workload = &config.WorkloadConfig{}
		}
		fullConfig.Workload.Manifest = options.WorkloadManifest
	}

	// Apply DOCA driver enable override from CLI
	if options.EnableDocaDriver != nil {
		if fullConfig.DOCADriver == nil {
			fullConfig.DOCADriver = &config.DOCADriverConfig{}
		}
		fullConfig.DOCADriver.Enable = *options.EnableDocaDriver
	}

	// Apply Spectrum-X CLI options
	if options.SpectrumX {
		if fullConfig.Profile.SpectrumX == nil {
			fullConfig.Profile.SpectrumX = &config.ProfileSpectrumX{}
		}
		fullConfig.Profile.SpectrumX.Enable = true
		if options.SPCXVersion != "" {
			fullConfig.Profile.SpectrumX.SPCXVersion = options.SPCXVersion
		}
		if options.MultiplaneMode != "" {
			fullConfig.Profile.SpectrumX.MultiplaneMode = options.MultiplaneMode
		}
		if options.NumberOfPlanes != 0 {
			fullConfig.Profile.SpectrumX.NumberOfPlanes = options.NumberOfPlanes
		}
	}

	log.Log.V(1).Info("Applied options to config", "plugin", p.GetName())
	return nil
}

func (p *NetworkOperatorPlugin) SelectProfile(config *config.LaunchKitConfig) (*profiles.Profile, error) {
	return nil, nil
}

var _ plugin.Plugin = &NetworkOperatorPlugin{}
