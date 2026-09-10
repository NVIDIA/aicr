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

const (
	kubeflowTrainerComponentName   = "kubeflow-trainer"
	gkeNCCLTCPXOComponentName      = "gke-nccl-tcpxo"
	gkeTCPXOInterfacesValueKey     = "tcpxoInterfaces"
	gkeTCPXORuntimeManifest        = "components/kubeflow-trainer/manifests/torch-distributed-tcpxo-cluster-training-runtime.yaml"
	gkeTCPXORequiredInterfaceCount = 8
)

// Device-type Network names are limited by GKE's UNIX socket path length.
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/setup-multinetwork-support-for-pods
const gkeNetworkObjectNameMaxLen = 41

var (
	gkeTCPXOInterfaceNamePattern = regexp.MustCompile(`^eth[1-8]$`)
	gkeNetworkNamePattern        = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
)

// NetworkInterfaceMapping binds a guest interface to a GKE Network object,
// not directly to a VPC. Explicit pairs preserve the provisioner's mapping;
// list position must never be used to invent an interface assignment.
type NetworkInterfaceMapping struct {
	InterfaceName string `json:"interfaceName" yaml:"interfaceName"`
	Network       string `json:"network" yaml:"network"`
}

// GKEConfiguration records cluster-specific desired state without adding a
// catalog-matching axis. TCPXOInterfaces preserves the operator's pair order.
type GKEConfiguration struct {
	TCPXOInterfaces []NetworkInterfaceMapping `json:"tcpxoInterfaces,omitempty" yaml:"tcpxoInterfaces,omitempty"`
}

// WithGKETCPXOInterfaces supplies a mapping for one build. Capture a copy so
// later caller mutations cannot change a reusable build option.
func WithGKETCPXOInterfaces(mapping []NetworkInterfaceMapping) BuildOption {
	recorded := slices.Clone(mapping)
	return func(cfg *buildConfig) {
		copy := slices.Clone(recorded)
		cfg.tcpxoInterfaces = &copy
	}
}

// ParseGKETCPXOInterfaces parses eth1=<network>,...,eth8=<network>, preserving
// pair order. The names and interface assignments must come from provisioning.
func ParseGKETCPXOInterfaces(value string) ([]NetworkInterfaceMapping, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"GKE TCPXO interface mapping cannot be empty: expected eth1=<network>,...,eth8=<network>")
	}
	segments := strings.Split(value, ",")
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

// FormatGKETCPXOInterfaces formats explicit pairs without reordering them.
func FormatGKETCPXOInterfaces(mapping []NetworkInterfaceMapping) string {
	parts := make([]string, 0, len(mapping))
	for _, entry := range mapping {
		parts = append(parts, entry.InterfaceName+"="+entry.Network)
	}
	return strings.Join(parts, ",")
}

// ValidateGKETCPXOInterfaces requires eth1..eth8 exactly once and eight unique
// Device-type Network names. The pair sequence may be in any order; only the
// explicit interfaceName determines an assignment. The name alphabet also
// makes these values safe to embed in the manifest's JSON annotation.
func ValidateGKETCPXOInterfaces(mapping []NetworkInterfaceMapping) error {
	if len(mapping) != gkeTCPXORequiredInterfaceCount {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("GKE TCPXO interface mapping must contain exactly %d entries (eth1..eth8 on a3-megagpu-8g), got %d",
				gkeTCPXORequiredInterfaceCount, len(mapping)))
	}
	seenInterfaces := make(map[string]struct{}, len(mapping))
	seenNetworks := make(map[string]struct{}, len(mapping))
	for _, entry := range mapping {
		if !gkeTCPXOInterfaceNamePattern.MatchString(entry.InterfaceName) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid GKE TCPXO interface name %q: must be one of eth1..eth8", entry.InterfaceName))
		}
		if _, dup := seenInterfaces[entry.InterfaceName]; dup {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("duplicate GKE TCPXO interface name %q", entry.InterfaceName))
		}
		seenInterfaces[entry.InterfaceName] = struct{}{}
		if len(entry.Network) > gkeNetworkObjectNameMaxLen || !gkeNetworkNamePattern.MatchString(entry.Network) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid GKE Device Network name %q for %s: use 1-41 lowercase letters, digits or dashes, starting with a letter and ending with a letter or digit",
					entry.Network, entry.InterfaceName))
		}
		if _, dup := seenNetworks[entry.Network]; dup {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("duplicate VPC network name %q: each interface must map to a distinct GKE Network", entry.Network))
		}
		seenNetworks[entry.Network] = struct{}{}
	}
	return nil
}

// GKETCPXOInterfaces returns a copy of the mapping and whether it was recorded.
func (r *RecipeResult) GKETCPXOInterfaces() ([]NetworkInterfaceMapping, bool) {
	if r == nil || r.Configuration == nil || r.Configuration.GKE == nil {
		return nil, false
	}
	return slices.Clone(r.Configuration.GKE.TCPXOInterfaces), true
}

// ShipsGKETCPXORuntime checks the recipe's actual runtime declaration. Trainer
// and installer presence alone do not prove that this manifest ships, notably
// for stored recipes generated before it existed.
func (r *RecipeResult) ShipsGKETCPXORuntime() bool {
	if r == nil {
		return false
	}
	ref := r.GetComponentRef(kubeflowTrainerComponentName)
	return ref != nil && ref.IsEnabled() && slices.Contains(ref.ManifestFiles, gkeTCPXORuntimeManifest)
}

func componentPresentAndEnabled(r *RecipeResult, name string) bool {
	ref := r.GetComponentRef(name)
	return ref != nil && ref.IsEnabled()
}

// GKETCPXOIntrospectionInterfaces provides fresh synthetic inputs for offline
// catalog analysis and tests. These are not discovered deployment values.
func GKETCPXOIntrospectionInterfaces() []NetworkInterfaceMapping {
	mapping := make([]NetworkInterfaceMapping, gkeTCPXORequiredInterfaceCount)
	for i := range mapping {
		mapping[i] = NetworkInterfaceMapping{InterfaceName: fmt.Sprintf("eth%d", i+1), Network: fmt.Sprintf("gpu-nic-%d", i)}
	}
	return mapping
}

// GKETCPXOIntrospectionBuildOptions supplies fixture data only for the embedded
// catalog's TCPXO leaf. Deployment callers must supply their provisioned mapping.
func GKETCPXOIntrospectionBuildOptions(c *Criteria) []BuildOption {
	if c == nil || c.Service != CriteriaServiceGKE ||
		c.Accelerator != CriteriaAcceleratorH100 || c.Platform != CriteriaPlatformKubeflow {

		return nil
	}
	return []BuildOption{WithGKETCPXOInterfaces(GKETCPXOIntrospectionInterfaces())}
}

// GKETCPXOOwnership declares the mapping's non-profile ownership domain.
func GKETCPXOOwnership() OwnershipDomain {
	return OwnershipDomain{
		Name:  "configuration.gke.tcpxoInterfaces",
		Paths: map[string][]string{kubeflowTrainerComponentName: {gkeTCPXOInterfacesValueKey}},
	}
}

// applyGKETCPXOInterfaces runs even without an option, so a runtime declaration
// without its required mapping fails at generation rather than at rendering.
func applyGKETCPXOInterfaces(result *RecipeResult, mapping *[]NetworkInterfaceMapping) error {
	if mapping == nil {
		if result.ShipsGKETCPXORuntime() {
			return errors.New(errors.ErrCodeInvalidRequest,
				"this recipe ships torch-distributed-tcpxo and requires configuration.gke.tcpxoInterfaces; supply --gke-tcpxo-interfaces eth1=<network>,...,eth8=<network> or spec.recipe.configuration.gke.tcpxoInterfaces")
		}
		return nil
	}
	if err := ValidateGKETCPXOInterfaces(*mapping); err != nil {
		return err
	}
	if err := validateGKETCPXOApplicability(result); err != nil {
		return err
	}
	if err := validateGKETCPXOProfileOwnership(result); err != nil {
		return err
	}
	if result.Configuration == nil {
		result.Configuration = &RecipeConfiguration{}
	}
	recorded := slices.Clone(*mapping)
	result.Configuration.GKE = &GKEConfiguration{TCPXOInterfaces: recorded}
	result.APIVersion = ConfiguredRecipeResultAPIVersion
	override := make([]any, 0, len(recorded))
	for _, entry := range recorded {
		override = append(override, map[string]any{"interfaceName": entry.InterfaceName, "network": entry.Network})
	}
	return setComponentOverride(result, kubeflowTrainerComponentName, map[string]any{gkeTCPXOInterfacesValueKey: override})
}

func validateGKETCPXOApplicability(result *RecipeResult) error {
	if result.Criteria == nil || result.Criteria.Service != CriteriaServiceGKE || result.Criteria.Accelerator != CriteriaAcceleratorH100 {
		return errors.New(errors.ErrCodeInvalidRequest, "configuration.gke.tcpxoInterfaces is only valid for an h100 GKE recipe")
	}
	if !result.ShipsGKETCPXORuntime() || !componentPresentAndEnabled(result, gkeNCCLTCPXOComponentName) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"GKE TCPXO interfaces require the recipe to ship torch-distributed-tcpxo and enable gke-nccl-tcpxo")
	}
	return nil
}

func validateGKETCPXOProfileOwnership(result *RecipeResult) error {
	if result == nil || result.Metadata.SelectedProfile == nil {
		return nil
	}
	selected := result.Metadata.SelectedProfile
	return ValidateOwnershipDisjoint(OwnershipDomain{
		Name:  fmt.Sprintf("profile %s=%s", selected.Name, selected.Value),
		Paths: selected.OwnedPaths,
	}, GKETCPXOOwnership())
}

// NormalizeGKETCPXOInterfaces decodes the component-values representation
// without sorting or assigning interface names from list positions.
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
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("tcpxoInterfaces entry %d must be a mapping", i))
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

// validateGKEConfiguration detects drift between recorded intent and its
// projected override when a recipe is loaded, adopted, or generated.
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
	if err := ValidateGKETCPXOInterfaces(mapping); err != nil {
		return err
	}
	if err := validateGKETCPXOApplicability(r); err != nil {
		return err
	}
	if err := validateGKETCPXOProfileOwnership(r); err != nil {
		return err
	}
	ref := r.GetComponentRef(kubeflowTrainerComponentName)
	raw, ok := ref.Overrides[gkeTCPXOInterfacesValueKey]
	if !ok {
		return errors.New(errors.ErrCodeInvalidRequest,
			"configuration.gke.tcpxoInterfaces is recorded but kubeflow-trainer carries no tcpxoInterfaces override")
	}
	projected, err := NormalizeGKETCPXOInterfaces(raw)
	if err != nil {
		return err
	}
	if !slices.Equal(projected, mapping) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"kubeflow-trainer override disagrees with configuration.gke.tcpxoInterfaces; regenerate the recipe rather than editing one half")
	}
	return nil
}
