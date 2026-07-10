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

package config

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"net"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"gopkg.in/yaml.v2"
)

// defaultConfigYAML is the canonical baseline l8k configuration baked into the
// binary. Library callers (and CLI invocations without a checked-out repo or
// installed share dir) start from these defaults; an explicit configPath
// overlays / replaces them. See DefaultLaunchKitConfig and LoadFullConfig.
//
//go:embed default-config.yaml
var defaultConfigYAML []byte

// DefaultLaunchKitConfig returns a freshly-parsed copy of the binary's
// embedded default configuration. Returned value is safe to mutate; each call
// re-parses to avoid sharing pointers between callers.
//
// This is the library-mode entry point: a Go caller that does not want to
// rely on any filesystem layout can construct a LaunchKitConfig with sensible
// defaults via this function alone, then optionally override fields in code.
func DefaultLaunchKitConfig() (*LaunchKitConfig, error) {
	var cfg LaunchKitConfig
	if err := yaml.Unmarshal(defaultConfigYAML, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse embedded default l8k-config: %w", err)
	}
	if err := NormalizeMaintenance(&cfg); err != nil {
		return nil, fmt.Errorf("invalid embedded maintenance config: %w", err)
	}
	return &cfg, nil
}

// MachineLabelKey is the node label `l8k discover` writes onto every
// node whose group has both machineType and gpuType resolved. The value
// is the literal `<machineType>-<gpuType>` (e.g. `DGX-B200-NVIDIA-H100-NVL`)
// — upstream discovery already trims whitespace and converts spaces to
// hyphens to match GPU operator label format. Per-source-group
// `NodeSelector` keys on this label.
const MachineLabelKey = "nvidia.kubernetes-launch-kit.machine"

// GPULabelKey is the node label `l8k discover` writes onto every node
// whose group has gpuType resolved. The value is the literal `<gpuType>`
// (e.g. `NVIDIA-H100-NVL`) — same shape as `nvidia.com/gpu.product`. Used
// as the auto-merged-group `NodeSelector` when source groups span
// machineTypes but share a GPU type.
const GPULabelKey = "nvidia.kubernetes-launch-kit.gpu"

// MaxLabelValueLength is the Kubernetes hard limit for label values.
const MaxLabelValueLength = 63

// MachineLabelValue returns the per-source-group machine label value:
// `<machineType>-<gpuType>` literal when it fits Kubernetes' 63-char
// label-value limit, or a deterministic shortened form for long names
// (truncated prefix + 8-hex FNV-32a suffix). Returns the empty string
// only when either input is empty.
//
// The shortened form looks like
// `HPE-ProLiant-Compute-DL380a-Gen12-NVIDIA-RTX-PRO-6000-B-7c4d8e91`
// and is reproducible: identical inputs always produce the same
// value, so the label discover writes onto nodes always matches what
// `MachineLabelValue` returns at filter time.
func MachineLabelValue(machineType, gpuType string) string {
	if machineType == "" || gpuType == "" {
		return ""
	}
	raw := machineType + "-" + gpuType
	if len(raw) <= MaxLabelValueLength {
		return raw
	}
	// 63-char budget = prefix + "-" + 8-hex hash. 8 hex digits + dash = 9.
	h := fnv.New32a()
	_, _ = h.Write([]byte(raw))
	suffix := fmt.Sprintf("-%08x", h.Sum32())
	prefixBudget := MaxLabelValueLength - len(suffix)
	prefix := strings.TrimRight(raw[:prefixBudget], "-_.")
	return prefix + suffix
}

// GPULabelValue returns the gpu label value, applying the same
// truncate-with-hash rule as `MachineLabelValue` for long gpuType
// strings. Returns "" only when input is empty.
func GPULabelValue(gpuType string) string {
	if gpuType == "" {
		return ""
	}
	if len(gpuType) <= MaxLabelValueLength {
		return gpuType
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(gpuType))
	suffix := fmt.Sprintf("-%08x", h.Sum32())
	prefixBudget := MaxLabelValueLength - len(suffix)
	prefix := strings.TrimRight(gpuType[:prefixBudget], "-_.")
	return prefix + suffix
}

// SanitizeIdentifier converts a product-type or label-value string into a
// valid K8s name component: lowercases the input and replaces spaces with
// hyphens. Used by discovery to derive `ClusterConfig.Identifier` from the
// machine label, and by the renderer when an identifier needs to land in a
// resource name. Both call sites must agree on the rule so a config produced
// by discovery renders the same names downstream — single function here
// guarantees that.
func SanitizeIdentifier(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// LaunchKitConfig represents the l8k-config.yaml structure
type LaunchKitConfig struct {
	NetworkOperator *NetworkOperatorConfig `yaml:"networkOperator,omitempty"`
	DOCADriver      *DOCADriverConfig      `yaml:"docaDriver,omitempty"`
	Maintenance     *MaintenanceConfig     `yaml:"maintenance,omitempty"`
	NvIpam          *NvIpamConfig          `yaml:"nvIpam,omitempty"`
	Sriov           *SriovConfig           `yaml:"sriov,omitempty"`
	Hostdev         *HostdevConfig         `yaml:"hostdev,omitempty"`
	RdmaShared      *RdmaSharedConfig      `yaml:"rdmaShared,omitempty"`
	Ipoib           *IpoibConfig           `yaml:"ipoib,omitempty"`
	Macvlan         *MacvlanConfig         `yaml:"macvlan,omitempty"`
	SpectrumX                *SpectrumXConfig                `yaml:"spectrumX,omitempty"`
	NicConfigurationOperator *NicConfigurationOperatorConfig `yaml:"nicConfigurationOperator,omitempty"`
	PodNamespace             string                          `yaml:"podNamespace,omitempty"`
	// NetworkNamespaces is the set of namespaces the secondary-network CRs
	// (SriovNetwork, SriovIBNetwork, HostDeviceNetwork, MacvlanNetwork,
	// IPoIBNetwork) and their example test DaemonSets are rendered into —
	// one independent copy per namespace. Shared resources (IPPool,
	// NicNodePolicy, SriovNetworkNodePolicy, NicClusterPolicy, …) are NOT
	// duplicated. Empty defaults to a single "default" namespace; a legacy
	// scalar podNamespace is folded in as the sole entry. PodNamespace stays
	// the "current" namespace scalar the templates read — the renderer
	// overrides it per entry while fanning out.
	NetworkNamespaces        []string                        `yaml:"networkNamespaces,omitempty"`
	Workload                 *WorkloadConfig                 `yaml:"workload,omitempty"`
	Profile         *Profile               `yaml:"profile,omitempty"`
	ClusterConfig   []ClusterConfig        `yaml:"clusterConfig,omitempty"`
}

type NetworkOperatorConfig struct {
	Version          string   `yaml:"version"`
	ComponentVersion string   `yaml:"componentVersion"`
	// SelectedRelease is the catalog key (MAJOR.MINOR, e.g. "26.4") chosen via
	// --network-operator-release. Empty means "no release pinned"; templates
	// treat that as "latest" so existing configs render the newest gates by
	// default. When non-empty, ApplyOptionsToConfig has already populated
	// Version/ComponentVersion/Repository and DOCADriver.Version from the
	// embedded catalog.
	SelectedRelease  string   `yaml:"selectedRelease,omitempty"`
	Repository       string   `yaml:"repository"`
	// OperatorRepository is the registry path for the network-operator
	// BINARY image itself, distinct from Repository (which is the
	// COMPONENT-images registry rendered into NicClusterPolicy /
	// NicNodePolicy). Populated by ApplyNetworkOperatorRelease from
	// the embedded catalog. Stable releases publish the operator
	// under `nvcr.io/nvidia/cloud-native`; staging keeps both at the
	// same path under `nvcr.io/nvstaging/mellanox`.
	OperatorRepository string   `yaml:"operatorRepository,omitempty"`
	Namespace          string   `yaml:"namespace"`
	ImagePullSecrets   []string `yaml:"imagePullSecrets,omitempty"`
	// HelmRepoURL is the Helm chart repository URL for the network-operator
	// chart. Populated by ApplyNetworkOperatorRelease from the embedded catalog;
	// users may override it in l8k-config.yaml. Empty means "no helm install"
	// — `l8k deploy` falls back to assuming the operator is managed out-of-band.
	HelmRepoURL string `yaml:"helmRepoURL,omitempty"`
}

type DOCADriverConfig struct {
	Enable                      bool `yaml:"enable"`
	Version                     string `yaml:"version"`
	UnloadStorageModules        bool `yaml:"unloadStorageModules"`
	EnableNFSRDMA               bool `yaml:"enableNFSRDMA"`
	UnloadThirdPartyRDMAModules bool `yaml:"unloadThirdPartyRDMAModules"`
	// SkipPreflightChecks controls the init container's module dependency check.
	// When false (l8k default), the check runs and any blocking dependency fails
	// the init container, preventing MOFED load. When true, the check is skipped
	// entirely and init succeeds immediately. The init container binary's own
	// default (`envDefault:"true"`) is overridden here because l8k is opinionated:
	// a deployment tool should surface hardware-compat issues early rather than
	// letting a broken MOFED reload happen silently downstream.
	SkipPreflightChecks bool `yaml:"skipPreflightChecks"`
}

type NvIpamConfig struct {
	PoolName       string               `yaml:"poolName"`
	Subnets        []NvIpamSubnetConfig `yaml:"subnets,omitempty"`
	StartingSubnet string               `yaml:"startingSubnet,omitempty"`
	Mask           int                  `yaml:"mask,omitempty"`
	Offset         int                  `yaml:"offset,omitempty"`
	// ReserveFirstIPs excludes the first N host addresses of EVERY subnet
	// (network address upward, e.g. 10 → .0–.9 on a /24). Applied to both
	// auto-generated and manually-listed subnets. Mask-agnostic.
	ReserveFirstIPs int `yaml:"reserveFirstIPs,omitempty"`
	// ReserveLastIPs excludes the last N host addresses of EVERY subnet
	// (broadcast address downward, e.g. 6 → .250–.255 on a /24).
	ReserveLastIPs int `yaml:"reserveLastIPs,omitempty"`
}

type NvIpamSubnetConfig struct {
	Subnet  string `yaml:"subnet"`
	Gateway string `yaml:"gateway"`
	// Exclusions are IP ranges removed from the pool's allocatable set. The
	// computed ReserveFirstIPs/ReserveLastIPs ranges are prepended to any
	// ranges listed here (reserve first, then explicit).
	Exclusions []NvIpamExclusion `yaml:"exclusions,omitempty"`
}

// NvIpamExclusion is a contiguous [StartIP, EndIP] range (inclusive) excluded
// from an IPPool. Mirrors the NV-IPAM IPPool CRD's spec.exclusions[] shape.
type NvIpamExclusion struct {
	StartIP string `yaml:"startIP"`
	EndIP   string `yaml:"endIP"`
}

type SriovConfig struct {
	EthernetMtu   int    `yaml:"ethernetMtu"`
	InfinibandMtu int    `yaml:"infinibandMtu"`
	NumVfs        int    `yaml:"numVfs"`
	Priority      int    `yaml:"priority"`
	ResourceName  string `yaml:"resourceName"`
	NetworkName   string `yaml:"networkName"`
}

type SpectrumXConfig struct {
	NicType      string `yaml:"nicType"`      // "1023" for ConnectX-8, "1025" for ConnectX-9, "a2dc" for BlueField-3 SuperNIC
	Overlay      string `yaml:"overlay"`      // "none"
	RdmaPrefix   string `yaml:"rdmaPrefix"`   // e.g., "roce_p%plane_id%_r%rail_id%"
	NetdevPrefix string `yaml:"netdevPrefix"` // e.g., "eth_p%plane_id%_r%rail_id%"
}

type NicConfigurationOperatorConfig struct {
	DeployNicInterfaceNameTemplate bool   `yaml:"deployNicInterfaceNameTemplate"`
	RdmaPrefix                     string `yaml:"rdmaPrefix"`   // e.g., "rdma_r%rail_id%"
	NetdevPrefix                   string `yaml:"netdevPrefix"` // e.g., "eth_r%rail_id%"
	// UpdateFW gates the firmware-update path of the NIC Configuration
	// Operator. Today it only controls whether the `nicFirmwareStorage` PVC
	// block is emitted in NicClusterPolicy (needed for the operator to stage
	// firmware images before flashing). When false (default), the storage
	// block is omitted so no PVC / StorageClass dependency is introduced.
	UpdateFW bool `yaml:"updateFW,omitempty"`
}

type HostdevConfig struct {
	ResourceName string `yaml:"resourceName"`
	NetworkName  string `yaml:"networkName"`
}

type RdmaSharedConfig struct {
	ResourceName string `yaml:"resourceName"`
	HcaMax       int    `yaml:"hcaMax"`
}

type IpoibConfig struct {
	NetworkName string `yaml:"networkName"`
}

type MacvlanConfig struct {
	NetworkName string `yaml:"networkName"`
}

type WorkloadConfig struct {
	Manifest string `yaml:"manifest,omitempty"`
}

type Profile struct {
	Fabric     string            `yaml:"fabric"`
	Deployment string            `yaml:"deployment"`
	Multirail  bool              `yaml:"multirail"`
	SpectrumX  *ProfileSpectrumX `yaml:"spectrumX,omitempty"`
}

type ProfileSpectrumX struct {
	Enable         bool   `yaml:"enable"`         // must be true for Spectrum-X profiles to match
	SPCXVersion    string `yaml:"spcxVersion"`    // e.g., "RA2.2"
	MultiplaneMode string `yaml:"multiplaneMode"` // swplb, hwplb, uniplane
	NumberOfPlanes int    `yaml:"numberOfPlanes"` // 2 or 4
}

type ClusterConfig struct {
	Identifier           string               `yaml:"identifier"`
	MachineType          string               `yaml:"machineType,omitempty"`
	GPUType          string               `yaml:"gpuType,omitempty"`
	// LinkType is the fabric type discovered for the group's east-west PFs:
	// "Ethernet" or "InfiniBand". Set only when *every* east-west PF probe
	// returns a confirmed verdict (port ACTIVE + matching link_layer + for
	// IB, a non-zero sm_lid) and the verdicts agree. Otherwise omitted —
	// the discovery couldn't prove the cluster is using a specific fabric,
	// and downstream code should treat the field's absence as "unknown".
	LinkType             string               `yaml:"linkType,omitempty"`
	PresetApplied        bool                 `yaml:"presetApplied,omitempty"`
	// PresetDeviation lists discrepancies between the matched preset and
	// the cluster's actually-discovered hardware. When non-empty, the
	// preset was applied (so rail/NUMA topology fields are populated) but
	// the cluster differs from the certified configuration. l8k re-warns
	// every time the config is loaded.
	PresetDeviation []PresetDeviationEntry `yaml:"presetDeviation,omitempty"`
	Capabilities         *ClusterCapabilities `yaml:"capabilities"`
	PFs                  []PFConfig           `yaml:"pfs"`
	WorkerNodes          []string             `yaml:"workerNodes"`
	NodeSelector         map[string]string    `yaml:"nodeSelector,omitempty"`
	ThirdPartyRDMAModules []string            `yaml:"thirdPartyRDMAModules,omitempty"`
	StorageModules        []string            `yaml:"storageModules,omitempty"`
	RailPciAddresses     [][]string           `yaml:"-"` // Transient: per-rail merged PCI addresses (not serialized)
	// MergedIdentifier is the bucket-level identifier shared by all source
	// groups that merge together by (gpuType, railCount). Used in templates
	// for shared-resource references (resourceName, networkName, poolName,
	// cidrPoolRef) so per-source NodePolicies under Mode B all register the
	// same kubelet resource. In Mode A this equals Identifier; in Mode B's
	// per-source render units it differs.
	MergedIdentifier string `yaml:"-"`
	// SourceMachineLabels lists the machine-label values
	// (`<machineType>-<gpuType>`) of every source group represented by the
	// merged bucket. Populated only when this is a merged render group and
	// the filtered set is a strict subset of its (gpuType, railCount)
	// bucket — used by Scope-Aggregate templates to emit a
	// `matchExpressions In: [...]` selector. Empty in Mode A and for
	// per-source render units.
	SourceMachineLabels []string `yaml:"-"`
}

// PresetDeviationEntry records a single field-level discrepancy between a
// preset and the cluster's actually-discovered hardware. Field is one of
// "pciAddress", "deviceID", or "pfCount".
type PresetDeviationEntry struct {
	Field    string `yaml:"field"`
	Expected string `yaml:"expected,omitempty"`
	Got      string `yaml:"got,omitempty"`
	Detail   string `yaml:"detail,omitempty"`
}

type ClusterCapabilities struct {
	Nodes *NodesCapabilities `yaml:"nodes"`
}

type NodesCapabilities struct {
	Sriov bool `yaml:"sriov"`
	Rdma  bool `yaml:"rdma"`
	Ib    bool `yaml:"ib"`
}

type PFConfig struct {
	DeviceID         string `yaml:"deviceID"`
	RdmaDevice       string `yaml:"rdmaDevice"`
	PciAddress       string `yaml:"pciAddress"`
	NetworkInterface string `yaml:"networkInterface"`
	Traffic          string `yaml:"traffic"`
	Rail             *int   `yaml:"rail,omitempty"`
	PSID             string `yaml:"psid,omitempty"`
	PartNumber       string `yaml:"partNumber,omitempty"`
	// Model is the human-readable NIC model/description string read from the
	// device VPD (e.g. "Nvidia ConnectX-7 NDR200/HDR QSFP112 2-port PCIe Gen5
	// x16 InfiniBand Adapter"). Populated during discovery from
	// NicDevice.Status.ModelName. It records why the rail-collapse decision was
	// made (a "2-port"/"Dual-port" model keeps one rail per port; anything else
	// collapses to one rail per NIC) and is empty when the daemon couldn't read
	// VPD.
	Model string `yaml:"model,omitempty"`
	// Topology fields (populated from presets when available)
	NumaNode     *int   `yaml:"numaNode,omitempty"`
	ConnectedGPU string `yaml:"connectedGPU,omitempty"`
	GPUProximity string `yaml:"gpuProximity,omitempty"`
}

// AggregateCapabilities computes the union of capabilities across all cluster config groups.
// If any group has a capability, the aggregate has it.
func AggregateCapabilities(groups []ClusterConfig) *ClusterCapabilities {
	result := &ClusterCapabilities{Nodes: &NodesCapabilities{}}
	for _, g := range groups {
		if g.Capabilities != nil && g.Capabilities.Nodes != nil {
			result.Nodes.Sriov = result.Nodes.Sriov || g.Capabilities.Nodes.Sriov
			result.Nodes.Rdma = result.Nodes.Rdma || g.Capabilities.Nodes.Rdma
			result.Nodes.Ib = result.Nodes.Ib || g.Capabilities.Nodes.Ib
		}
	}
	return result
}

// LoadFullConfig loads and parses the cluster configuration from the specified
// path. When configPath is empty, returns the embedded default configuration
// — the library-mode entry point that lets a caller produce a working
// LaunchKitConfig without any filesystem layout on the host. A non-empty path
// that does not exist remains an error (a typo'd --user-config should NOT
// silently fall back to defaults).
func LoadFullConfig(configPath string, logger logr.Logger) (*LaunchKitConfig, error) {
	if configPath == "" {
		logger.Info("Loading embedded default l8k-config (no path provided)")
		cfg, err := DefaultLaunchKitConfig()
		if err != nil {
			return nil, err
		}
		// emitPresetDeviationWarnings is a no-op for defaults (no
		// ClusterConfig entries yet) but kept for shape parity.
		emitPresetDeviationWarnings(cfg, logger)
		return cfg, nil
	}

	logger.Info("Loading cluster configuration", "path", configPath)

	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("cluster config file does not exist: %s", configPath)
	}

	// Read the configuration file
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cluster config file %s: %w", configPath, err)
	}

	// Parse the YAML configuration
	var config LaunchKitConfig
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("failed to parse cluster config YAML %s: %w", configPath, err)
	}
	if err := NormalizeMaintenance(&config); err != nil {
		return nil, fmt.Errorf("invalid maintenance config in %s: %w", configPath, err)
	}

	logger.Info("Cluster configuration loaded successfully",
		"networkOperatorVersion", config.NetworkOperator.Version,
		"namespace", config.NetworkOperator.Namespace)

	if err := validateNvIpam(config.NvIpam); err != nil {
		return nil, fmt.Errorf("invalid nvIpam config in %s: %w", configPath, err)
	}

	// Finalize exclusions for explicitly-listed (manual) subnets. Auto-generated
	// subnets are finalized in the render path after GenerateSubnets. l8k never
	// rewrites nvIpam back to disk, so this runs exactly once per load.
	if config.NvIpam != nil && len(config.NvIpam.Subnets) > 0 {
		if err := ApplyReservedExclusions(
			config.NvIpam.Subnets, config.NvIpam.ReserveFirstIPs, config.NvIpam.ReserveLastIPs); err != nil {
			return nil, fmt.Errorf("invalid nvIpam config in %s: %w", configPath, err)
		}
	}

	emitPresetDeviationWarnings(&config, logger)

	return &config, nil
}

// emitPresetDeviationWarnings logs a warning for every group whose config
// records preset deviations. Designed to fire on every load — operators
// running against hardware that differs from the matched preset are
// reminded each run.
func emitPresetDeviationWarnings(cfg *LaunchKitConfig, logger logr.Logger) {
	for _, g := range cfg.ClusterConfig {
		if len(g.PresetDeviation) == 0 {
			continue
		}
		logger.Info(
			"WARNING: cluster differs from the matched preset — manifests are still produced, but the deployment is not certified",
			"group", g.Identifier,
			"machineType", g.MachineType,
			"gpuType", g.GPUType,
			"deviationCount", len(g.PresetDeviation),
		)
		for _, d := range g.PresetDeviation {
			logger.Info("  preset deviation",
				"group", g.Identifier,
				"field", d.Field,
				"expected", d.Expected,
				"got", d.Got,
				"detail", d.Detail,
			)
		}
	}
}

// ValidateClusterConfig validates that essential fields are present in the cluster config
func ValidateClusterConfig(config *LaunchKitConfig, profile string) error {
	// Public callers may construct a config directly instead of using
	// LoadFullConfig. Normalize again here so validation covers both paths.
	if err := NormalizeMaintenance(config); err != nil {
		return err
	}

	if config.NetworkOperator.Repository == "" {
		return fmt.Errorf("networkOperator.repository is required")
	}

	if config.NetworkOperator.ComponentVersion == "" {
		return fmt.Errorf("networkOperator.componentVersion is required")
	}

	if config.NetworkOperator.Namespace == "" {
		return fmt.Errorf("networkOperator.namespace is required")
	}

	// Validate Spectrum-X specific requirements
	if config.Profile != nil && config.Profile.SpectrumX != nil && config.SpectrumX != nil {
		if err := validateSpectrumXTemplates(config); err != nil {
			return err
		}
	}

	// Validate profile-specific requirements based on the selected profile
	if profile == "host-device-rdma" || profile == "hostdevice" {
		if config.Hostdev.ResourceName == "" {
			return fmt.Errorf("hostdev.resourceName is required for hostdevice profiles")
		}
		if config.Hostdev.NetworkName == "" {
			return fmt.Errorf("hostdev.networkName is required for hostdevice profiles")
		}
	}

	if profile == "sriov-rdma" || profile == "sriov-ib-rdma" {
		if config.Sriov.ResourceName == "" {
			return fmt.Errorf("sriov.resourceName is required for SR-IOV profiles")
		}
		if config.Sriov.NetworkName == "" {
			return fmt.Errorf("sriov.networkName is required for SR-IOV profiles")
		}
	}

	return nil
}

// SupportedSPCXVersions lists the Spectrum-X RA versions for which l8k can
// emit non-`none` multiplane configurations. RA2.1 ships on Network Operator
// 26.1; RA2.2 on 26.4+. Order is preserved in error messages.
var SupportedSPCXVersions = []string{"RA2.1", "RA2.2"}

// SupportedMultiplaneModes lists the Spectrum-X multiplane modes the CLI
// accepts. `none` and `uniplane` collapse to one plane; `swplb` and `hwplb`
// require numberOfPlanes > 1.
var SupportedMultiplaneModes = []string{"none", "swplb", "hwplb", "uniplane"}

// SupportedNumberOfPlanes lists the values numberOfPlanes can take.
var SupportedNumberOfPlanes = []int{1, 2, 4}

// SPCXVersionAllowedReleases is the authoritative mapping from SPC-X RA
// version to the Network Operator releases that ship that version's CRD
// set. When a future release line picks up an existing RA version (e.g.
// 27.0 continues to ship the v1alpha2 SpectrumXRailPoolConfig), append
// it to the matching slice. When a future RA version arrives, add a
// new key. Lives in pkg/config because it's shared between Phase 1
// syntax checks (pkg/cmd) and Phase 2 cohort validation +
// hardware-defaulting (pkg/resolve).
var SPCXVersionAllowedReleases = map[string][]string{
	"RA2.1": {"26.1"},
	"RA2.2": {"26.4"},
}

// DefaultSPCXReleaseFor returns the canonical (first-listed) Network
// Operator release for an SPC-X RA version, or "" when the RA is not
// registered. Used by `pkg/resolve.ApplyHardwareDefaults` to fill
// `--network-operator-release` when the user passes `--spectrum-x`
// without an explicit release.
func DefaultSPCXReleaseFor(ra string) string {
	releases, ok := SPCXVersionAllowedReleases[ra]
	if !ok || len(releases) == 0 {
		return ""
	}
	return releases[0]
}

// validateSpectrumXTemplates validates that Spectrum-X templates have required placeholders
func validateSpectrumXTemplates(config *LaunchKitConfig) error {
	netdevPrefix := config.SpectrumX.NetdevPrefix
	rdmaPrefix := config.SpectrumX.RdmaPrefix

	// Non-`none` multiplane modes (swplb, hwplb, uniplane) require a supported
	// RA version.
	if config.Profile.SpectrumX.MultiplaneMode != "none" && config.Profile.SpectrumX.MultiplaneMode != "" {
		got := config.Profile.SpectrumX.SPCXVersion
		supported := false
		for _, v := range SupportedSPCXVersions {
			if got == v {
				supported = true
				break
			}
		}
		if !supported {
			return fmt.Errorf("multiplane mode %s requires spcxVersion in %v, got %q",
				config.Profile.SpectrumX.MultiplaneMode, SupportedSPCXVersions, got)
		}
	}

	isMultiplane := config.Profile.SpectrumX.NumberOfPlanes > 1
	isMultirail := config.Profile.Multirail
	
	// Check netdevPrefix (accept both %plane%/%rail% and %plane_id%/%rail_id%)
	hasPlaneInNetdev := containsPlaceholder(netdevPrefix, "%plane%") || containsPlaceholder(netdevPrefix, "%plane_id%")
	hasRailInNetdev := containsPlaceholder(netdevPrefix, "%rail%") || containsPlaceholder(netdevPrefix, "%rail_id%")

	if isMultiplane && !hasPlaneInNetdev {
		return fmt.Errorf("spectrumX.netdevPrefix must contain %%plane_id%% placeholder when numberOfPlanes > 1 (multiplane mode)")
	}

	if isMultirail && !hasRailInNetdev {
		return fmt.Errorf("spectrumX.netdevPrefix must contain %%rail_id%% placeholder when multirail is enabled")
	}

	// Check rdmaPrefix (same rules)
	hasPlaneInRdma := containsPlaceholder(rdmaPrefix, "%plane%") || containsPlaceholder(rdmaPrefix, "%plane_id%")
	hasRailInRdma := containsPlaceholder(rdmaPrefix, "%rail%") || containsPlaceholder(rdmaPrefix, "%rail_id%")

	if isMultiplane && !hasPlaneInRdma {
		return fmt.Errorf("spectrumX.rdmaPrefix must contain %%plane_id%% placeholder when numberOfPlanes > 1 (multiplane mode)")
	}

	if isMultirail && !hasRailInRdma {
		return fmt.Errorf("spectrumX.rdmaPrefix must contain %%rail_id%% placeholder when multirail is enabled")
	}
	
	return nil
}

// containsPlaceholder checks if a string contains a specific placeholder
func containsPlaceholder(s, placeholder string) bool {
	return len(s) > 0 && len(placeholder) > 0 &&
		len(s) >= len(placeholder) &&
		strings.Contains(s, placeholder)
}

// GenerateSubnets creates a list of subnet configurations by incrementing from a
// starting network address. Each subsequent subnet is offset by `offset` subnet-sized
// blocks. The gateway for each subnet is the first usable address (network + 1).
func GenerateSubnets(startingSubnet string, mask, offset, count int) ([]NvIpamSubnetConfig, error) {
	if count < 1 {
		return nil, fmt.Errorf("subnet count must be >= 1, got %d", count)
	}
	if mask < 1 || mask > 30 {
		return nil, fmt.Errorf("mask must be between 1 and 30, got %d", mask)
	}
	if offset < 1 {
		return nil, fmt.Errorf("offset must be >= 1, got %d", offset)
	}

	ip := net.ParseIP(startingSubnet)
	if ip == nil {
		return nil, fmt.Errorf("invalid starting subnet IP: %q", startingSubnet)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("starting subnet must be an IPv4 address, got %q", startingSubnet)
	}

	baseIP := binary.BigEndian.Uint32(ip4)
	blockSize := uint32(1) << uint(32-mask)
	hostMask := blockSize - 1

	// Verify the starting address is properly aligned (host bits must be zero)
	if baseIP&hostMask != 0 {
		return nil, fmt.Errorf("starting subnet %s is not aligned for /%d (host bits are not zero)", startingSubnet, mask)
	}

	// Check that the last subnet won't overflow the IPv4 address space
	lastIndex := uint64(count-1) * uint64(offset)
	lastAddr := uint64(baseIP) + lastIndex*uint64(blockSize)
	if lastAddr > math.MaxUint32 {
		return nil, fmt.Errorf("subnet generation would overflow IPv4 address space: starting %s/%d, offset %d, count %d",
			startingSubnet, mask, offset, count)
	}

	subnets := make([]NvIpamSubnetConfig, count)
	for i := 0; i < count; i++ {
		networkAddr := baseIP + uint32(i*offset)*blockSize
		subnetIP := make(net.IP, 4)
		binary.BigEndian.PutUint32(subnetIP, networkAddr)
		gatewayIP := make(net.IP, 4)
		binary.BigEndian.PutUint32(gatewayIP, networkAddr+1)

		subnets[i] = NvIpamSubnetConfig{
			Subnet:  fmt.Sprintf("%s/%d", subnetIP.String(), mask),
			Gateway: gatewayIP.String(),
		}
	}

	return subnets, nil
}

// uint32ToIP renders a big-endian uint32 as a dotted-quad IPv4 string.
func uint32ToIP(v uint32) string {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, v)
	return ip.String()
}

// reservedExclusions computes the low/high reserved IP ranges for a subnet CIDR.
// reserveFirst host addresses from the network address upward and reserveLast
// from the broadcast address downward are returned as inclusive ranges. Returns
// nil when both counts are <= 0.
func reservedExclusions(subnetCIDR string, reserveFirst, reserveLast int) ([]NvIpamExclusion, error) {
	if reserveFirst <= 0 && reserveLast <= 0 {
		return nil, nil
	}

	_, ipnet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet %q: %w", subnetCIDR, err)
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("subnet %q must be IPv4", subnetCIDR)
	}

	network := binary.BigEndian.Uint32(ip4)
	ones, bits := ipnet.Mask.Size()
	blockSize := uint32(1) << uint(bits-ones)
	broadcast := network + blockSize - 1

	if uint64(reserveFirst)+uint64(reserveLast) >= uint64(blockSize) {
		return nil, fmt.Errorf(
			"reserveFirstIPs (%d) + reserveLastIPs (%d) leaves no usable addresses in %s (block size %d)",
			reserveFirst, reserveLast, subnetCIDR, blockSize)
	}

	var out []NvIpamExclusion
	if reserveFirst > 0 {
		out = append(out, NvIpamExclusion{
			StartIP: uint32ToIP(network),
			EndIP:   uint32ToIP(network + uint32(reserveFirst) - 1),
		})
	}
	if reserveLast > 0 {
		out = append(out, NvIpamExclusion{
			StartIP: uint32ToIP(broadcast - uint32(reserveLast) + 1),
			EndIP:   uint32ToIP(broadcast),
		})
	}
	return out, nil
}

// ApplyReservedExclusions finalizes each subnet's Exclusions list by prepending
// the computed reserve ranges (ReserveFirstIPs/ReserveLastIPs) to any explicit
// exclusions already present. It is a no-op when both reserve counts are <= 0.
// Call exactly once per subnet slice (slices built by GenerateSubnets are fresh
// per render path; manual subnets are finalized once at load time).
func ApplyReservedExclusions(subnets []NvIpamSubnetConfig, reserveFirst, reserveLast int) error {
	if reserveFirst <= 0 && reserveLast <= 0 {
		return nil
	}
	for i := range subnets {
		reserved, err := reservedExclusions(subnets[i].Subnet, reserveFirst, reserveLast)
		if err != nil {
			return err
		}
		if len(reserved) > 0 {
			subnets[i].Exclusions = append(reserved, subnets[i].Exclusions...)
		}
	}
	return nil
}

// validateNvIpam checks the nvIpam exclusion settings before any rendering.
// It validates the reserve counts and any explicit per-subnet exclusion ranges.
func validateNvIpam(nv *NvIpamConfig) error {
	if nv == nil {
		return nil
	}
	if nv.ReserveFirstIPs < 0 {
		return fmt.Errorf("nvIpam.reserveFirstIPs must be >= 0, got %d", nv.ReserveFirstIPs)
	}
	if nv.ReserveLastIPs < 0 {
		return fmt.Errorf("nvIpam.reserveLastIPs must be >= 0, got %d", nv.ReserveLastIPs)
	}

	// For auto-generated subnets there are no entries to iterate at load time,
	// so verify the reserve counts are feasible against the configured mask up
	// front rather than failing mid-render.
	if len(nv.Subnets) == 0 && nv.StartingSubnet != "" && nv.Mask > 0 &&
		(nv.ReserveFirstIPs > 0 || nv.ReserveLastIPs > 0) {
		cidr := fmt.Sprintf("%s/%d", nv.StartingSubnet, nv.Mask)
		if _, err := reservedExclusions(cidr, nv.ReserveFirstIPs, nv.ReserveLastIPs); err != nil {
			return err
		}
	}

	for _, s := range nv.Subnets {
		var ipnet *net.IPNet
		if s.Subnet != "" {
			var err error
			if _, ipnet, err = net.ParseCIDR(s.Subnet); err != nil {
				return fmt.Errorf("nvIpam subnet %q is not valid CIDR: %w", s.Subnet, err)
			}
		}
		// Reserve counts must leave usable addresses in each manual subnet.
		if ipnet != nil && (nv.ReserveFirstIPs > 0 || nv.ReserveLastIPs > 0) {
			if _, err := reservedExclusions(s.Subnet, nv.ReserveFirstIPs, nv.ReserveLastIPs); err != nil {
				return err
			}
		}
		for _, ex := range s.Exclusions {
			start := net.ParseIP(ex.StartIP)
			end := net.ParseIP(ex.EndIP)
			if start == nil || start.To4() == nil {
				return fmt.Errorf("nvIpam exclusion has invalid IPv4 startIP %q", ex.StartIP)
			}
			if end == nil || end.To4() == nil {
				return fmt.Errorf("nvIpam exclusion has invalid IPv4 endIP %q", ex.EndIP)
			}
			if binary.BigEndian.Uint32(start.To4()) > binary.BigEndian.Uint32(end.To4()) {
				return fmt.Errorf("nvIpam exclusion startIP %q must be <= endIP %q", ex.StartIP, ex.EndIP)
			}
			// Explicit ranges must fall within their own subnet, else NV-IPAM
			// rejects the IPPool at apply time — surface it at config load.
			if ipnet != nil && (!ipnet.Contains(start) || !ipnet.Contains(end)) {
				return fmt.Errorf("nvIpam exclusion %s-%s is outside subnet %s", ex.StartIP, ex.EndIP, s.Subnet)
			}
		}
	}
	return nil
}
