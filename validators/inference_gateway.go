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

package validators

import (
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Reserved ComponentRef.Overrides keys through which a recipe names its
// inference gateway. The nesting is overrides.aicr.inferenceGateway, so a
// chart never sees a top-level key it did not define and the "aicr" namespace
// stays free for other AICR-interpreted settings.
const (
	AICROverrideKey             = "aicr"
	InferenceGatewayOverrideKey = "inferenceGateway"

	inferenceGatewayClassField     = "gatewayClass"
	inferenceGatewayNameField      = "gateway"
	inferenceGatewayDataPlaneField = "dataPlane"

	// DefaultInferenceGatewayName is the Gateway object every AICR inference
	// recipe creates, whichever implementation programs it. Dynamo's GAIE
	// docs and the performance validator's HTTPRoute attach to it by name.
	DefaultInferenceGatewayName = "inference-gateway"
)

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

// InferenceGatewayProfile is what the inference-gateway conformance check and
// the performance validator's gateway-epp mode read instead of literals.
// Namespace is the resolved componentRef's namespace, hydrated from the
// registry at recipe resolution, or the built-in default when the ref carries
// none.
type InferenceGatewayProfile struct {
	Component    string
	GatewayClass string
	GatewayName  string
	Namespace    string
	Probe        GatewayDataPlaneProbe
	// Source records where the profile came from, for evidence artifacts.
	Source string
}

// builtinAgentgateway is the inference gateway AICR ships; its profile is the
// fallback for validators that run without a recipe.
const builtinAgentgateway = "agentgateway"

// gatewayControlPlaneNameMarkers identify co-located gateway control-plane
// components that share the Gateway's name prefix but are not the data-plane
// proxy. Under enforce mode a control-plane LoadBalancer (for example
// inference-gateway-controller-manager) would otherwise fail the exposure
// check, and a control-plane EndpointSlice would inflate the proxy readiness
// count. See #1160.
var gatewayControlPlaneNameMarkers = []string{"controller-manager", "webhook", "metrics"}

// IsProxyName reports whether name is the Gateway's data-plane proxy Service
// or EndpointSlice: the Gateway's own name, or that name with a "-" suffix,
// never a control-plane component. The match is exact-or-prefix rather than
// substring because the Gateway name is whatever the recipe chose, and a
// short name would otherwise claim unrelated objects in the namespace. The
// conformance check and the performance validator share this predicate so
// readiness, exposure and the benchmark target agree on which Service is the
// proxy.
func (p InferenceGatewayProfile) IsProxyName(name string) bool {
	if p.GatewayName == "" || name == "" {
		return false
	}
	if name != p.GatewayName && !strings.HasPrefix(name, p.GatewayName+"-") {
		return false
	}
	for _, marker := range gatewayControlPlaneNameMarkers {
		if strings.Contains(name, marker) {
			return false
		}
	}
	return true
}

// builtinInferenceGatewayProfiles covers the implementations AICR ships. A
// recipe may still override any field of a built-in through the reserved key.
var builtinInferenceGatewayProfiles = map[string]InferenceGatewayProfile{
	builtinAgentgateway: {
		Component:    builtinAgentgateway,
		GatewayClass: builtinAgentgateway,
		GatewayName:  DefaultInferenceGatewayName,
		Namespace:    "agentgateway-system",
		Probe:        GatewayProbeServiceEndpoints,
		Source:       "built-in profile",
	},
}

// DefaultInferenceGatewayProfile is the profile a validator falls back to when
// it runs without a recipe: the AICR-shipped agentgateway in its registry
// namespace, which is what every validator assumed before profiles existed.
func DefaultInferenceGatewayProfile() InferenceGatewayProfile {
	return builtinInferenceGatewayProfiles[builtinAgentgateway]
}

// ResolveInferenceGatewayProfile finds the one enabled component in the
// resolved recipe that is an inference gateway, either because it is a
// built-in implementation or because its componentRef declares
// overrides.aicr.inferenceGateway. found is false when the recipe declares no
// inference gateway (or there is no recipe), which is the capability-gated
// skip. More than one enabled gateway is an error: routes and the performance
// validator need a single Gateway to attach to.
func ResolveInferenceGatewayProfile(ctx *Context) (profile InferenceGatewayProfile, found bool, err error) {
	if ctx == nil || ctx.ValidationInput == nil {
		return InferenceGatewayProfile{}, false, nil
	}
	var declared []InferenceGatewayProfile
	for i := range ctx.ValidationInput.ComponentRefs {
		ref := &ctx.ValidationInput.ComponentRefs[i]
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
	raw, declared := nestedOverride(ref.Overrides, AICROverrideKey, InferenceGatewayOverrideKey)
	if !builtin && !declared {
		return InferenceGatewayProfile{}, false, nil
	}
	path := "overrides." + AICROverrideKey + "." + InferenceGatewayOverrideKey
	if !builtin {
		profile = InferenceGatewayProfile{
			Component:   ref.Name,
			GatewayName: DefaultInferenceGatewayName,
			Probe:       GatewayProbeServiceEndpoints,
			Source:      "componentRef " + path,
		}
	}
	if declared {
		fields, ok := raw.(map[string]any)
		if !ok {
			return InferenceGatewayProfile{}, false, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q: %s must be a mapping", ref.Name, path))
		}
		if builtin {
			profile.Source = "built-in profile with componentRef " + path
		}
		for key, value := range fields {
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
	return profile, true, nil
}

// nestedOverride reports the value at path inside overrides, descending
// through nested mappings only.
func nestedOverride(overrides map[string]any, path ...string) (any, bool) {
	var current any = overrides
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
