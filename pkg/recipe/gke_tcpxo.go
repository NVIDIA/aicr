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

package recipe

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
)

// Component names the GKE TCPXO wiring spans. Declared as constants rather
// than inlined so the coupling between the recipe configuration, the
// registry entries, and the kubeflow-trainer manifest is greppable from
// both ends.
const (
	kubeflowTrainerComponentName = "kubeflow-trainer"
	gkeNCCLTCXOComponentName     = "gke-nccl-tcpxo"

	// gkeTCPXOInterfacesValueKey is the kubeflow-trainer values path the
	// ordered mapping is recorded under and the torch-distributed-tcpxo
	// manifest renders from.
	gkeTCPXOInterfacesValueKey = "tcpxoInterfaces"

	// gkeTCPXORequiredInterfaceCount is the number of secondary (GPU NIC)
	// interfaces on an a3-megagpu-8g node. The mapping is exactly eth1..eth8;
	// eth0 is the default interface and is rendered statically by the
	// manifest, never recorded here.
	gkeTCPXORequiredInterfaceCount = 8
)

// gkeTCPXOInterfaceNamePattern pins secondary interface names to eth1..eth8 —
// the guest interface names GKE assigns eight additional VPC networks on an
// a3-megagpu-8g node. Anything else means the value was authored for a
// different machine shape, which this runtime does not claim to support.
var gkeTCPXOInterfaceNamePattern = regexp.MustCompile(`^eth[1-8]$`)

// gcpNetworkNamePattern is the GCP VPC network name contract (lowercase
// letters, digits, dashes; starts with a letter; ends with a letter or
// digit; 1-63 chars). Enforced so the value rendered into the
// networking.gke.io/interfaces annotation is one GKE accepts, and so the
// JSON the template hand-renders cannot carry characters that need escaping.
var gcpNetworkNamePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// NetworkInterfaceMapping binds one secondary guest interface (eth1..eth8)
// to the VPC network that backs it. It mirrors the entry shape of GKE's
// networking.gke.io/interfaces annotation exactly, so the recorded recipe
// value needs no transformation at render time.
//
// The mapping is carried as a slice, not a map: Go map iteration order is
// randomized, and an unordered carrier would reintroduce the silent
// ethN→network misassignment this type exists to prevent.
type NetworkInterfaceMapping struct {
	InterfaceName string `json:"interfaceName" yaml:"interfaceName"`
	Network       string `json:"network" yaml:"network"`
}

// GKEConfiguration records GKE-specific desired state.
type GKEConfiguration struct {
	// TCPXOInterfaces records the ordered eth1..eth8 → VPC network mapping
	// rendered into the torch-distributed-tcpxo ClusterTrainingRuntime's
	// networking.gke.io/interfaces annotation.
	//
	// The names are cluster-specific — chosen by whoever created the VPC
	// networks — and AICR has no cluster access at recipe generation time,
	// so they are a required typed input on any recipe that ships the
	// runtime (see ShipsGKETCPXORuntime), recorded here for auditability.
	TCPXOInterfaces []NetworkInterfaceMapping `json:"tcpxoInterfaces,omitempty" yaml:"tcpxoInterfaces,omitempty"`
}

// WithGKETCPXOInterfaces supplies the ordered eth1..eth8 → VPC network
// mapping for one build. The value is validated again at the build boundary.
//
// Required — with no default — when the resolved recipe ships the
// torch-distributed-tcpxo runtime; rejected when it does not.
func WithGKETCPXOInterfaces(mapping []NetworkInterfaceMapping) BuildOption {
	return func(cfg *buildConfig) {
		cfg.tcpxoInterfaces = &mapping
	}
}

// ParseGKETCPXOInterfaces parses the CLI/server string form:
//
//	eth1=<network>,eth2=<network>,...,eth8=<network>
//
// Entry order in the string is preserved in the returned slice. Parse and
// validate are deliberately one function so every transport (flag, query
// parameter, config document canonicalization) produces the same normalized
// value or the same error.
func ParseGKETCPXOInterfaces(value string) ([]NetworkInterfaceMapping, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"GKE TCPXO interface mapping cannot be empty: expected eth1=<network>,...,eth8=<network>")
	}
	segments := strings.Split(trimmed, ",")
	mapping := make([]NetworkInterfaceMapping, 0, len(segments))
	for _, segment := range segments {
		name, network, found := strings.Cut(strings.TrimSpace(segment), "=")
		if !found {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid GKE TCPXO interface entry %q: expected <interface>=<network>", segment))
		}
		mapping = append(mapping, NetworkInterfaceMapping{
			InterfaceName: strings.TrimSpace(name),
			Network:       strings.TrimSpace(network),
		})
	}
	if err := ValidateGKETCPXOInterfaces(mapping); err != nil {
		return nil, err
	}
	return mapping, nil
}

// FormatGKETCPXOInterfaces renders the canonical string form
// ParseGKETCPXOInterfaces accepts. It exists so the config-document path
// (which holds the structured list) can feed the string-typed resolve option
// without a second, divergent parser.
func FormatGKETCPXOInterfaces(mapping []NetworkInterfaceMapping) string {
	parts := make([]string, 0, len(mapping))
	for _, entry := range mapping {
		parts = append(parts, entry.InterfaceName+"="+entry.Network)
	}
	return strings.Join(parts, ",")
}

// ValidateGKETCPXOInterfaces enforces the a3-megagpu-8g contract: exactly
// eight entries, interface names covering eth1..eth8 exactly once, and eight
// unique, syntactically valid GCP network names.
func ValidateGKETCPXOInterfaces(mapping []NetworkInterfaceMapping) error {
	if len(mapping) != gkeTCPXORequiredInterfaceCount {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("GKE TCPXO interface mapping must contain exactly %d entries "+
				"(eth1..eth8 on a3-megagpu-8g), got %d",
				gkeTCPXORequiredInterfaceCount, len(mapping)))
	}
	seenInterfaces := make(map[string]struct{}, len(mapping))
	seenNetworks := make(map[string]struct{}, len(mapping))
	for _, entry := range mapping {
		if !gkeTCPXOInterfaceNamePattern.MatchString(entry.InterfaceName) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid GKE TCPXO interface name %q: must be one of eth1..eth8",
					entry.InterfaceName))
		}
		if _, dup := seenInterfaces[entry.InterfaceName]; dup {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("duplicate GKE TCPXO interface name %q: eth1..eth8 must each appear exactly once",
					entry.InterfaceName))
		}
		seenInterfaces[entry.InterfaceName] = struct{}{}

		if !gcpNetworkNamePattern.MatchString(entry.Network) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid VPC network name %q for %s: must match GCP network naming "+
					"(lowercase letters, digits, dashes; start with a letter; 1-63 chars)",
					entry.Network, entry.InterfaceName))
		}
		if _, dup := seenNetworks[entry.Network]; dup {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("duplicate VPC network name %q: each GPU NIC interface must map to a distinct network",
					entry.Network))
		}
		seenNetworks[entry.Network] = struct{}{}
	}
	return nil
}

// GKETCPXOInterfaces returns the recorded ordered mapping, and whether the
// recipe records one at all. A recipe built before this input existed (or
// for a recipe family that does not ship the runtime) records nothing.
func (r *RecipeResult) GKETCPXOInterfaces() ([]NetworkInterfaceMapping, bool) {
	if r == nil || r.Configuration == nil || r.Configuration.GKE == nil {
		return nil, false
	}
	return r.Configuration.GKE.TCPXOInterfaces, true
}

// ShipsGKETCPXORuntime reports whether the resolved recipe ships the
// torch-distributed-tcpxo ClusterTrainingRuntime — the condition under which
// the interface mapping is required. The runtime renders from the
// kubeflow-trainer component's manifests and is only meaningful where the
// TCPXO fabric is installed, so the fingerprint is both components present
// and enabled. It is scoped to h100 because the recorded contract (eight
// secondary interfaces, eth1..eth8) is a3-megagpu-8g-shaped; other
// accelerators are unaffected until their fabric shape is qualified.
func (r *RecipeResult) ShipsGKETCPXORuntime() bool {
	if r == nil || r.Criteria == nil || r.Criteria.Accelerator != CriteriaAcceleratorH100 {
		return false
	}
	return componentPresentAndEnabled(r, kubeflowTrainerComponentName) &&
		componentPresentAndEnabled(r, gkeNCCLTCXOComponentName)
}

func componentPresentAndEnabled(r *RecipeResult, name string) bool {
	ref := r.GetComponentRef(name)
	return ref != nil && ref.IsEnabled()
}

// gkeTCPXOIntrospectionMapping is a fixed, clearly-labeled mapping that lets
// catalog-wide introspection tooling (parity goldens, health and tuning
// reports) resolve the TCPXO fingerprint leaf without a cluster to name real
// networks for. The value is never valid for a generated artifact:
// user-facing surfaces (CLI, server, SDK resolve) take the operator's real
// network names, and only they can produce a recipe meant to be bundled.
var gkeTCPXOIntrospectionMapping = []NetworkInterfaceMapping{
	{InterfaceName: "eth1", Network: "gpu-nic-0"},
	{InterfaceName: "eth2", Network: "gpu-nic-1"},
	{InterfaceName: "eth3", Network: "gpu-nic-2"},
	{InterfaceName: "eth4", Network: "gpu-nic-3"},
	{InterfaceName: "eth5", Network: "gpu-nic-4"},
	{InterfaceName: "eth6", Network: "gpu-nic-5"},
	{InterfaceName: "eth7", Network: "gpu-nic-6"},
	{InterfaceName: "eth8", Network: "gpu-nic-7"},
}

// GKETCPXOIntrospectionInterfaces returns a copy of the fixed introspection
// mapping, for callers outside this package (facade-level tooling) that need
// the value rather than the build option.
func GKETCPXOIntrospectionInterfaces() []NetworkInterfaceMapping {
	return slices.Clone(gkeTCPXOIntrospectionMapping)
}

// GKETCPXOIntrospectionBuildOptions returns the build options catalog-wide
// introspection needs for the fingerprint family, and nil for every other
// criteria — so tooling enumerating the whole catalog can pass it uniformly
// without supplying the mapping to recipes that must not record one.
func GKETCPXOIntrospectionBuildOptions(c *Criteria) []BuildOption {
	if c == nil || c.Service != CriteriaServiceGKE ||
		c.Accelerator != CriteriaAcceleratorH100 || c.Platform != CriteriaPlatformKubeflow {

		return nil
	}
	return []BuildOption{WithGKETCPXOInterfaces(gkeTCPXOIntrospectionMapping)}
}

// GKETCPXOOwnership returns the canonical component paths controlled by the
// recorded mapping. Composed with the selected profile's ownership at apply
// time and enforced against bundle-time override channels by the bundler, so
// a --set cannot become a second representation of the recipe-recorded value.
func GKETCPXOOwnership() OwnershipDomain {
	return OwnershipDomain{
		Name: "configuration.gke.tcpxoInterfaces",
		Paths: map[string][]string{
			kubeflowTrainerComponentName: {gkeTCPXOInterfacesValueKey},
		},
	}
}

// applyGKETCPXOInterfaces records the mapping and projects it into the
// kubeflow-trainer component's overrides. It runs on every build — including
// builds that supply no mapping — because fail-closed on a TCPXO recipe is
// the contract: a recipe that ships torch-distributed-tcpxo without the
// mapping cannot render a usable runtime.
//
// Like the runtime inventory selection, the guard depends on the resolved
// recipe (component presence and enablement), not criteria alone, so it
// lives here rather than in resolveBuildConfig.
func applyGKETCPXOInterfaces(result *RecipeResult, mapping *[]NetworkInterfaceMapping) error {
	if mapping == nil {
		if result.ShipsGKETCPXORuntime() {
			return errors.New(errors.ErrCodeInvalidRequest,
				"this recipe ships the torch-distributed-tcpxo ClusterTrainingRuntime, which "+
					"requires the ordered eth1..eth8 VPC network mapping recorded in the recipe; "+
					"supply it with --gke-tcpxo-interfaces eth1=<network>,...,eth8=<network> "+
					"(or spec.recipe.configuration.gke.tcpxoInterfaces in an AICRConfig)")
		}
		return nil
	}

	// Re-validate at the build boundary: BuildOption is public API and the
	// caller may have constructed the slice by hand.
	if err := ValidateGKETCPXOInterfaces(*mapping); err != nil {
		return err
	}

	// Fail closed on a recipe that cannot use the value — wrong criteria, a
	// typo'd component set, an accelerator shape the contract does not cover.
	// Silently recording a decision the recipe cannot honor is how a recipe
	// ends up stating one thing and deploying another.
	if !componentPresentAndEnabled(result, kubeflowTrainerComponentName) ||
		!componentPresentAndEnabled(result, gkeNCCLTCXOComponentName) {

		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("GKE TCPXO interfaces require the recipe to declare and enable components %q and %q; "+
				"this recipe does not resolve both", kubeflowTrainerComponentName, gkeNCCLTCXOComponentName))
	}
	if result.Criteria == nil || result.Criteria.Accelerator != CriteriaAcceleratorH100 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"GKE TCPXO interfaces are supported on h100 (a3-megagpu-8g) recipes only: "+
				"the recorded contract is eight secondary interfaces, eth1..eth8")
	}

	if err := validateGKETCPXOProfileOwnership(result); err != nil {
		return err
	}

	// Update in place rather than assigning a fresh RecipeConfiguration,
	// mirroring the accounting selection: another selection may already have
	// recorded its own section.
	if result.Configuration == nil {
		result.Configuration = &RecipeConfiguration{}
	}
	recorded := make([]NetworkInterfaceMapping, len(*mapping))
	copy(recorded, *mapping)
	result.Configuration.GKE = &GKEConfiguration{TCPXOInterfaces: recorded}
	result.APIVersion = ConfiguredRecipeResultAPIVersion

	override := make([]any, 0, len(recorded))
	for _, entry := range recorded {
		override = append(override, map[string]any{
			"interfaceName": entry.InterfaceName,
			"network":       entry.Network,
		})
	}
	return setComponentOverride(result, kubeflowTrainerComponentName,
		map[string]any{gkeTCPXOInterfacesValueKey: override})
}

func validateGKETCPXOProfileOwnership(result *RecipeResult) error {
	if result == nil || result.Metadata.SelectedProfile == nil {
		return nil
	}
	selected := result.Metadata.SelectedProfile
	return ValidateOwnershipDisjoint(
		OwnershipDomain{
			Name:  fmt.Sprintf("profile %s=%s", selected.Name, selected.Value),
			Paths: selected.OwnedPaths,
		},
		GKETCPXOOwnership(),
	)
}

// NormalizeGKETCPXOInterfaces converts a decoded component-values entry
// (a []any of map[string]any, as YAML/JSON round-trips produce) back into
// the typed mapping, so the recorded value and the resolved override value
// can be compared as the same shape. Used by recipe coherence validation and
// the bundle-time final-value check.
func NormalizeGKETCPXOInterfaces(value any) ([]NetworkInterfaceMapping, error) {
	entries, ok := value.([]any)
	if !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("tcpxoInterfaces must be a list of {interfaceName, network} entries, got %T", value))
	}
	mapping := make([]NetworkInterfaceMapping, 0, len(entries))
	for i, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("tcpxoInterfaces entry %d must be a mapping, got %T", i, raw))
		}
		iface, _ := entry["interfaceName"].(string)
		network, _ := entry["network"].(string)
		if iface == "" || network == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("tcpxoInterfaces entry %d must carry non-empty interfaceName and network strings", i))
		}
		mapping = append(mapping, NetworkInterfaceMapping{InterfaceName: iface, Network: network})
	}
	return mapping, nil
}

// validateGKEConfiguration is the ValidateCoherence half of the carrier: a
// loaded or generated recipe that records configuration.gke must be one the
// value is valid for, and the projected component override must agree with
// the recorded value. The second clause catches hand-edited recipes, where
// one half was changed without the other — the recipe would otherwise state
// one mapping and render another.
func (r *RecipeResult) validateGKEConfiguration() error {
	mapping, present := r.GKETCPXOInterfaces()
	if !present {
		return nil
	}
	if !header.IsSupportedProfileAPIVersion(r.APIVersion) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("configuration.gke.tcpxoInterfaces requires apiVersion %q or %q (got %q)",
				ConfiguredRecipeResultAPIVersion, header.GroupVersionV1Beta2, r.APIVersion))
	}
	if r.Criteria == nil || r.Criteria.Service != CriteriaServiceGKE ||
		r.Criteria.Accelerator != CriteriaAcceleratorH100 {

		return errors.New(errors.ErrCodeInvalidRequest,
			"configuration.gke.tcpxoInterfaces is only valid for an h100 GKE recipe")
	}
	if err := ValidateGKETCPXOInterfaces(mapping); err != nil {
		return err
	}
	if err := validateGKETCPXOProfileOwnership(r); err != nil {
		return err
	}
	if !componentPresentAndEnabled(r, kubeflowTrainerComponentName) ||
		!componentPresentAndEnabled(r, gkeNCCLTCXOComponentName) {

		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("configuration.gke.tcpxoInterfaces requires the recipe to declare and enable "+
				"components %q and %q", kubeflowTrainerComponentName, gkeNCCLTCXOComponentName))
	}

	// The projected override must be the recorded value, exactly and in
	// order. A valid-but-different mapping is the failure case this guards:
	// the recipe attests to one wiring while rendering another.
	ref := r.GetComponentRef(kubeflowTrainerComponentName)
	rawOverride, ok := ref.Overrides[gkeTCPXOInterfacesValueKey]
	if !ok {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("configuration.gke.tcpxoInterfaces is recorded but component %q carries no %s override; "+
				"regenerate the recipe rather than editing one half", kubeflowTrainerComponentName, gkeTCPXOInterfacesValueKey))
	}
	projected, err := NormalizeGKETCPXOInterfaces(rawOverride)
	if err != nil {
		return err
	}
	if !networkInterfaceMappingsEqual(projected, mapping) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q override %s disagrees with configuration.gke.tcpxoInterfaces; "+
				"regenerate the recipe rather than editing one half",
				kubeflowTrainerComponentName, gkeTCPXOInterfacesValueKey))
	}
	return nil
}

// networkInterfaceMappingsEqual is ordered equality: the mapping is a
// sequence contract, not a set, and order is part of the recorded value.
func networkInterfaceMappingsEqual(a, b []NetworkInterfaceMapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
