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
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The Gateway API resources the validators read, declared once so a version
// bump is one edit for every check that reads them.
var (
	GatewayGVR   = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
	HTTPRouteGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
)

// The inference gateway profile and its vocabulary live in pkg/validator/v1
// beside the allocation-policy resolver, so the SDK facade (CNCF evidence
// collection) and the validator binaries read one declaration. The aliases
// keep check code on the package it already imports.
type (
	InferenceGatewayProfile = v1.InferenceGatewayProfile
	GatewayDataPlaneProbe   = v1.GatewayDataPlaneProbe
)

const (
	GatewayProbeServiceEndpoints = v1.GatewayProbeServiceEndpoints
	GatewayProbeGatewayAddress   = v1.GatewayProbeGatewayAddress
	DefaultInferenceGatewayName  = v1.DefaultInferenceGatewayName
)

// DefaultInferenceGatewayProfile is the built-in agentgateway profile, the
// target of a validator that runs without a recipe.
func DefaultInferenceGatewayProfile() InferenceGatewayProfile {
	return v1.DefaultInferenceGatewayProfile()
}

// ResolveInferenceGatewayProfile resolves the recipe's inference gateway from
// the validation input. Nil-safe: a nil Context or no recipe reports not
// found, which is the standalone path.
func ResolveInferenceGatewayProfile(ctx *Context) (InferenceGatewayProfile, bool, error) {
	if ctx == nil || ctx.ValidationInput == nil {
		return InferenceGatewayProfile{}, false, nil
	}
	return v1.ResolveInferenceGatewayProfile(ctx.ValidationInput.ComponentRefs)
}
