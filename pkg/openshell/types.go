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

package openshell

import "strings"

// EnforcementLayer identifies where a policy rule is enforced.
type EnforcementLayer string

const (
	// LayerDNS is Layer 0: enforcement at the DNS resolver.
	LayerDNS EnforcementLayer = "layer0"
	// LayerCaller is Layer 1: enforcement at the calling agent (OpenShell).
	LayerCaller EnforcementLayer = "layer1"
	// LayerTarget is Layer 2: enforcement at the target agent.
	LayerTarget EnforcementLayer = "layer2"
)

// PolicyDocument is the top-level policy structure served at a target's
// policy URI (SvcParamKey 65403). Schema matches dns-aid-core PolicyDocument.
type PolicyDocument struct {
	Version string      `json:"version"`
	Agent   string      `json:"agent"`
	Rules   PolicyRules `json:"rules"`
}

// RateLimitConfig specifies request rate limits for a target agent.
type RateLimitConfig struct {
	MaxPerMinute *int `json:"max_per_minute,omitempty"`
	MaxPerHour   *int `json:"max_per_hour,omitempty"`
}

// AvailabilityConfig specifies when a target agent accepts connections.
type AvailabilityConfig struct {
	Hours    string `json:"hours"`    // format: "HH:MM-HH:MM"
	Timezone string `json:"timezone"` // IANA timezone, default "UTC"
}

// CELRule is a custom policy rule using CEL expressions.
type CELRule struct {
	ID                string   `json:"id"`
	Expression        string   `json:"expression"`
	Effect            string   `json:"effect"`  // "deny" or "warn"
	Message           string   `json:"message"` // human-readable
	EnforcementLayers []string `json:"enforcement_layers,omitempty"`
}

// PolicyRules contains the 16 native policy rules and optional CEL rules.
// Most "value-bearing" fields use pointers or slices to distinguish "not set"
// from zero values. The three boolean rules (RequireDNSSEC, RequireMutualTLS,
// ConsentRequired) are plain bool by design: a missing JSON key and an
// explicit "false" both mean "not required", which is the correct default.
type PolicyRules struct {
	RequiredProtocols        []string            `json:"required_protocols,omitempty"`
	RequiredAuthTypes        []string            `json:"required_auth_types,omitempty"`
	RequireDNSSEC            bool                `json:"require_dnssec"`
	RequireMutualTLS         bool                `json:"require_mutual_tls"`
	MinTLSVersion            *string             `json:"min_tls_version,omitempty"`
	RequiredCallerTrustScore *float64            `json:"required_caller_trust_score,omitempty"`
	RateLimits               *RateLimitConfig    `json:"rate_limits,omitempty"`
	MaxPayloadBytes          *int                `json:"max_payload_bytes,omitempty"`
	AllowedCallerDomains     []string            `json:"allowed_caller_domains,omitempty"`
	BlockedCallerDomains     []string            `json:"blocked_caller_domains,omitempty"`
	AllowedMethods           []string            `json:"allowed_methods,omitempty"`
	AllowedIntents           []string            `json:"allowed_intents,omitempty"`
	GeoRestrictions          []string            `json:"geo_restrictions,omitempty"`
	Availability             *AvailabilityConfig `json:"availability,omitempty"`
	DataClassification       *string             `json:"data_classification,omitempty"`
	ConsentRequired          bool                `json:"consent_required"`
	CELRules                 []CELRule           `json:"cel_rules,omitempty"`
}

// PolicyContext describes the calling agent and request metadata for
// policy evaluation. Matches dns-aid-core PolicyContext fields.
type PolicyContext struct {
	CallerID         string   `json:"caller_id,omitempty"`
	CallerDomain     string   `json:"caller_domain,omitempty"`
	Protocol         string   `json:"protocol,omitempty"`
	Method           string   `json:"method,omitempty"`
	Intent           string   `json:"intent,omitempty"`
	AuthType         string   `json:"auth_type,omitempty"`
	DNSSECValidated  bool     `json:"dnssec_validated"`
	TLSVersion       string   `json:"tls_version,omitempty"`
	CallerTrustScore *float64 `json:"caller_trust_score,omitempty"`
	GeoCountry       string   `json:"geo_country,omitempty"`
	PayloadBytes     *int     `json:"payload_bytes,omitempty"`
	HasMutualTLS     bool     `json:"has_mutual_tls"`
	ConsentToken     string   `json:"consent_token,omitempty"`
}

// PolicyViolation describes a single rule violation or warning.
type PolicyViolation struct {
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
	Layer  string `json:"layer"`
}

// PolicyResult is the outcome of policy evaluation.
type PolicyResult struct {
	Allowed    bool              `json:"allowed"`
	Violations []PolicyViolation `json:"violations,omitempty"`
	Warnings   []PolicyViolation `json:"warnings,omitempty"`
}

// Reason returns a human-readable summary of the policy result.
func (r PolicyResult) Reason() string {
	if r.Allowed {
		return "allowed"
	}
	details := make([]string, 0, len(r.Violations))
	for _, v := range r.Violations {
		details = append(details, v.Rule+": "+v.Detail)
	}
	return strings.Join(details, "; ")
}

// ruleEnforcementLayers maps each native rule name to the layers where it
// is enforced. Matches dns-aid-core RULE_ENFORCEMENT_LAYERS.
var ruleEnforcementLayers = map[string][]EnforcementLayer{
	"required_protocols":          {LayerDNS, LayerCaller, LayerTarget},
	"required_auth_types":         {LayerCaller, LayerTarget},
	"require_dnssec":              {LayerDNS, LayerCaller},
	"require_mutual_tls":          {LayerCaller, LayerTarget},
	"min_tls_version":             {LayerCaller, LayerTarget},
	"required_caller_trust_score": {LayerCaller, LayerTarget},
	"rate_limits":                 {LayerTarget},
	"max_payload_bytes":           {LayerTarget},
	"allowed_caller_domains":      {LayerCaller, LayerTarget},
	"blocked_caller_domains":      {LayerCaller, LayerTarget},
	"allowed_methods":             {LayerCaller, LayerTarget},
	"allowed_intents":             {LayerCaller, LayerTarget},
	"geo_restrictions":            {LayerCaller, LayerTarget},
	"availability":                {LayerCaller, LayerTarget},
	"data_classification":         {LayerCaller, LayerTarget},
	"consent_required":            {LayerCaller, LayerTarget},
}

// appliesToLayer returns true if the named rule should be evaluated at the given layer.
func appliesToLayer(rule string, layer EnforcementLayer) bool {
	layers, ok := ruleEnforcementLayers[rule]
	if !ok {
		return false
	}
	for _, l := range layers {
		if l == layer {
			return true
		}
	}
	return false
}
