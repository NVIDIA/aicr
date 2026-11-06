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

package v1

import (
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Fields of the recipe.InferenceGatewayOverrideKey mapping.
const (
	inferenceGatewayClassField     = "gatewayClass"
	inferenceGatewayNameField      = "gateway"
	inferenceGatewayDataPlaneField = "dataPlane"
)

// DefaultInferenceGatewayName is the Gateway object name a profile uses when
// its declaration sets none; the AICR-shipped agentgateway manifest creates a
// Gateway of this name.
const DefaultInferenceGatewayName = "inference-gateway"

// GatewayDataPlaneProbe names how the inference-gateway validators confirm a
// gateway's data plane is serving. A controller that provisions a Service for
// its proxy is observed through that Service's EndpointSlices. A data plane
// that is not a Kubernetes workload (a hardware or DPU-hosted proxy) carries
// its address in the Gateway object and is observed there.
type GatewayDataPlaneProbe string

const (
	GatewayProbeServiceEndpoints GatewayDataPlaneProbe = "service-endpoints"
	GatewayProbeGatewayAddress   GatewayDataPlaneProbe = "gateway-address"
)

// InferenceGatewayProfile is what the inference-gateway conformance check
// and the performance validator's gateway-epp mode read instead of literals. Namespace is the resolved componentRef's
// namespace, hydrated from the registry at recipe resolution, or the built-in
// default when the ref carries none.
type InferenceGatewayProfile struct {
	Component    string
	GatewayClass string
	GatewayName  string
	Namespace    string
	Probe        GatewayDataPlaneProbe
}

// builtinAgentgateway is the inference gateway AICR ships; its profile is the
// fallback for validators that run without a recipe.
const builtinAgentgateway = "agentgateway"

// builtinInferenceGatewayProfiles covers the implementations AICR ships. A
// recipe may override a built-in's gatewayClass, gateway or dataPlane through
// the override key; its namespace follows the ref.
var builtinInferenceGatewayProfiles = map[string]InferenceGatewayProfile{
	builtinAgentgateway: {
		Component:    builtinAgentgateway,
		GatewayClass: builtinAgentgateway,
		GatewayName:  DefaultInferenceGatewayName,
		Namespace:    "agentgateway-system",
		Probe:        GatewayProbeServiceEndpoints,
	},
}

// gatewayControlPlaneNameMarkers identify control-plane components (controller
// manager, webhook, metrics) that a gateway implementation deploys under the
// Gateway's name prefix; they are not the data-plane proxy and must not be
// counted as one.
var gatewayControlPlaneNameMarkers = []string{"controller-manager", "webhook", "metrics"}

// DefaultInferenceGatewayProfile is the profile a validator falls back to when
// it runs without a recipe: the AICR-shipped agentgateway in its registry
// namespace, which is what every validator assumed before profiles existed.
func DefaultInferenceGatewayProfile() InferenceGatewayProfile {
	return builtinInferenceGatewayProfiles[builtinAgentgateway]
}

// IsProxyName reports whether name is the Gateway's data-plane proxy Service
// or EndpointSlice: the Gateway's own name, or that name with a "-" suffix
// that does not mark a control-plane component. The match is exact-or-prefix
// rather than substring because the Gateway name is whatever the recipe
// chose, and a short name would otherwise claim unrelated objects in the
// namespace; the markers are matched on the suffix alone so a Gateway whose
// own name carries one (metrics-gateway) still matches its proxy. The
// conformance check and the performance validator share this predicate so
// readiness, exposure and the benchmark target agree on which Service is the
// proxy.
func (p InferenceGatewayProfile) IsProxyName(name string) bool {
	if p.GatewayName == "" || name == "" {
		return false
	}
	if name == p.GatewayName {
		return true
	}
	suffix, suffixed := strings.CutPrefix(name, p.GatewayName+"-")
	if !suffixed {
		return false
	}
	for _, marker := range gatewayControlPlaneNameMarkers {
		if strings.Contains(suffix, marker) {
			return false
		}
	}
	return true
}

// overrideKeyNormalizer folds the spellings a near miss of the override key
// takes: case, dashes and underscores.
var overrideKeyNormalizer = strings.NewReplacer("-", "", "_", "")

// ResolveInferenceGatewayProfile finds the one enabled component among refs
// that is an inference gateway, either because it is a built-in
// implementation or because its componentRef declares
// recipe.InferenceGatewayOverrideKey. found is false when no component is an
// inference gateway, which is the capability-gated skip. More than one
// enabled gateway is an error: routes and the performance validator need a
// single Gateway to attach to. A malformed declaration is an error, not a
// fallback: the recipe claims a gateway it cannot describe.
func ResolveInferenceGatewayProfile(refs []recipe.ComponentRef) (profile InferenceGatewayProfile, found bool, err error) {
	var declared []InferenceGatewayProfile
	for i := range refs {
		ref := &refs[i]
		if !ref.IsEnabled() {
			continue
		}
		p, isGateway, refErr := inferenceGatewayProfileFromRef(ref)
		if refErr != nil {
			return InferenceGatewayProfile{}, false, refErr
		}
		if isGateway {
			declared = append(declared, p)
		}
	}
	switch len(declared) {
	case 0:
		return InferenceGatewayProfile{}, false, nil
	case 1:
		return declared[0], true, nil
	default:
		names := make([]string, 0, len(declared))
		for _, p := range declared {
			names = append(names, p.Component)
		}
		sort.Strings(names)
		return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("recipe enables more than one inference gateway component [%s]; exactly one may be enabled",
				strings.Join(names, ", ")))
	}
}

func inferenceGatewayProfileFromRef(ref *recipe.ComponentRef) (InferenceGatewayProfile, bool, error) {
	profile, builtin := builtinInferenceGatewayProfiles[ref.Name]
	raw, declared := ref.Overrides[recipe.InferenceGatewayOverrideKey]
	if !declared {
		// A near miss (case, dashes or underscores) would otherwise pass
		// silently as an ordinary chart value, and the component would not be
		// a gateway. Keys are visited in order so the error names the same
		// key on every run.
		for _, key := range sortedKeys(ref.Overrides) {
			if key != recipe.InferenceGatewayOverrideKey &&
				strings.EqualFold(overrideKeyNormalizer.Replace(key), recipe.InferenceGatewayOverrideKey) {

				return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("component %q: override key %q is not recognised; the inference gateway declaration key is %q",
						ref.Name, key, recipe.InferenceGatewayOverrideKey))
			}
		}
	}
	if !builtin && !declared {
		return InferenceGatewayProfile{}, false, nil
	}
	path := "overrides." + recipe.InferenceGatewayOverrideKey
	if !builtin {
		profile = InferenceGatewayProfile{
			Component:   ref.Name,
			GatewayName: DefaultInferenceGatewayName,
			Probe:       GatewayProbeServiceEndpoints,
		}
	}
	if declared {
		fields, ok := raw.(map[string]any)
		if !ok {
			return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q: %s must be a mapping", ref.Name, path))
		}
		for _, key := range sortedKeys(fields) {
			value := fields[key]
			s, ok := value.(string)
			if !ok {
				return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("component %q: %s.%s must be a string", ref.Name, path, key))
			}
			switch key {
			case inferenceGatewayClassField:
				profile.GatewayClass = s
			case inferenceGatewayNameField:
				profile.GatewayName = s
			case inferenceGatewayDataPlaneField:
				probe := GatewayDataPlaneProbe(s)
				if probe != GatewayProbeServiceEndpoints && probe != GatewayProbeGatewayAddress {
					return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("component %q: %s.%s %q is not one of %q, %q",
							ref.Name, path, key, s, GatewayProbeServiceEndpoints, GatewayProbeGatewayAddress))
				}
				profile.Probe = probe
			default:
				return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("component %q: unknown %s key %q", ref.Name, path, key))
			}
		}
	}
	if ref.Namespace != "" {
		profile.Namespace = ref.Namespace
	}
	if profile.GatewayClass == "" {
		return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q declares %s without %s", ref.Name, path, inferenceGatewayClassField))
	}
	if profile.GatewayName == "" {
		return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: %s.%s may not be empty", ref.Name, path, inferenceGatewayNameField))
	}
	if profile.Namespace == "" {
		return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q is an inference gateway but has no resolved namespace; set componentRef.namespace or the registry defaultNamespace", ref.Name))
	}
	// The names are rendered into the kubectl reproduction commands the
	// inference-gateway check records on its evidence page and used as API
	// object names by both consumers, so a value that is not a Kubernetes
	// object name (a leading dash reads as a flag) is refused here, at
	// resolution, rather than inside a validator Job.
	for _, check := range []struct {
		field, value string
		problems     []string
	}{
		{path + "." + inferenceGatewayClassField, profile.GatewayClass, validation.IsDNS1123Subdomain(profile.GatewayClass)},
		{path + "." + inferenceGatewayNameField, profile.GatewayName, validation.IsDNS1123Subdomain(profile.GatewayName)},
		{"namespace", profile.Namespace, validation.IsDNS1123Label(profile.Namespace)},
	} {
		if len(check.problems) > 0 {
			return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q: %s %q is not a valid Kubernetes name: %s",
					ref.Name, check.field, check.value, strings.Join(check.problems, "; ")))
		}
	}
	return profile, true, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
