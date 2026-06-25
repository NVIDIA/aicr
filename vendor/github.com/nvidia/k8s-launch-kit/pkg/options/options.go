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

package options

import "time"

// Options holds all the configuration parameters for the application
type Options struct {
	// Logging
	LogLevel string
	LogFile  string // Path to log file (optional)

	// Phase 1: Cluster Discovery
	UserConfig              string // Path to user-provided config (skips discovery)
	DiscoverClusterConfig   bool   // Whether to discover cluster config
	// DiscoverOnly skips Phase 2 (manifest generation) entirely. Set by
	// the standalone `l8k discover` subcommand so its run produces only
	// cluster-config.yaml and never errors on "no profile selected".
	DiscoverOnly      bool
	SaveClusterConfig string // Path to save discovered config
	NetworkOperatorNamespace string // Override namespace for Network Operator (optional; ignored by `discover`)
	// KeepNamespace, when true, suppresses teardown of the
	// nvidia-k8s-launch-kit bootstrap namespace at the end of `discover` —
	// useful for debugging a failed run.
	KeepNamespace bool
	// CollapseNicRails (default true; --collapse-nic-rails) makes discovery
	// advertise one rail per NIC — multi-plane NICs collapse to their master
	// PF, while genuinely dual-port NIC models keep a rail per port. False
	// restores the legacy one-rail-per-PF behaviour for dev setups.
	CollapseNicRails bool
	// NetworkOperatorRelease is a MAJOR.MINOR catalog key (e.g. "26.4"), not
	// a full semver. Selects component image tags + repository from the
	// embedded releases catalog and drives version-gated template sections.
	NetworkOperatorRelease  string
	ImagePullSecrets        []string // Image pull secret names for NicClusterPolicy

	// Phase 2: Deployment Generation
	Fabric         string // Fabric type to deploy
	DeploymentType string // Deployment type to deploy
	Multirail      bool   // Whether to deploy with multirail
	// MultirailSet is true when the user explicitly passed `--multirail`
	// (regardless of value). Without it, the bool zero value can't be
	// distinguished from "not passed", which matters once
	// `pkg/resolve.ApplyHardwareDefaults` defaults Multirail to true:
	// `ApplyOptionsToConfig` only overrides the HW default when
	// MultirailSet is true, so a user passing `--multirail=false`
	// correctly opts out.
	MultirailSet bool
	SpectrumX    bool   // True when --spectrum-x is set; derived from SPCXVersion != ""
	SPCXVersion         string // Spectrum-X RA version (the value of --spectrum-x; empty = disabled)
	MultiplaneMode      string // Spectrum-X multiplane mode (default: swplb)
	NumberOfPlanes      int    // Number of planes for Spectrum-X (default: 4)
	// Groups limits `l8k generate` to the named source groups (matched
	// case-sensitively against `clusterConfig[].identifier`). Comma-separated
	// on the CLI (`--groups a,b`). Mutually exclusive with GpuType.
	Groups []string
	// GpuType limits `l8k generate` to source groups whose `gpuType` matches.
	// Single value, case-insensitive (`--gpu-type NVIDIA-H200`). Mutually
	// exclusive with Groups.
	GpuType      string
	NodeSelector string // Filter nodes for discovery and manifests (e.g., "key1=val1,key2=val2")
	// ForPreset is the directory name of a topology preset under presets/. When
	// set, generate replaces fullConfig.ClusterConfig with a single group
	// synthesized from the preset (skipping cluster discovery). Requires
	// NodeSelector since the preset has no live worker-node list.
	ForPreset           string
	// NetworkNamespaces is the comma-separated list from --network-namespaces:
	// the namespaces the secondary-network CRs + example test DaemonSets are
	// rendered into (one copy per namespace). Empty defaults to "default".
	NetworkNamespaces   []string
	SaveDeploymentFiles string // Directory to save generated files

	EnabledPlugins []string // Enabled plugins

	// Workload
	WorkloadManifest string // Path to user-defined workload manifest

	// DOCA Driver
	EnableDocaDriver *bool // Override docaDriver.enable from config (nil = use config value)

	// Phase 3: Cluster Deployment
	Deploy     bool   // Whether to deploy to cluster
	Kubeconfig string // Path to kubeconfig for discovery and deployment
	// DeployTimeout caps the *entire* deploy phase end-to-end (apply +
	// readiness wait for every manifest). A zero value means no
	// deadline — appropriate for large SR-IOV clusters where a single
	// reconciliation can outlast any reasonable per-manifest budget.
	// Plumbed into ctx via context.WithTimeout before
	// `networkoperatorplugin.ApplyManifestsFromDir` is invoked, so
	// every poll inside the deploy state machine observes the same
	// deadline.
	DeployTimeout time.Duration

	// OverwriteExisting forwards the `--overwrite-existing` flag through
	// to Phase 0 of `networkoperatorplugin.ApplyManifestsFromDir`. When
	// true and a network-operator helm release already exists in the
	// target namespace with values that differ from the freshly rendered
	// `values.yaml`, the install is promoted to `helm upgrade --install`.
	// When false (default), a value-conflict surfaces as a deployment
	// error pointing at this flag.
	OverwriteExisting bool

	// Output control
	OutputFormat string // Output format: "text" (default) or "json"
	Yes          bool   // Auto-confirm all prompts (--yes)
	Quiet        bool   // Suppress informational output (--quiet)
	DryRun       bool   // Preview without applying changes (--dry-run)
}
