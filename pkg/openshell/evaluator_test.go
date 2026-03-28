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

import (
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestEvaluateRequiredProtocols(t *testing.T) {
	tests := []struct {
		name   string
		rules  PolicyRules
		pctx   PolicyContext
		wantOK bool
	}{
		{"allowed protocol", PolicyRules{RequiredProtocols: []string{"mcp", "a2a"}}, PolicyContext{Protocol: "mcp"}, true},
		{"denied protocol", PolicyRules{RequiredProtocols: []string{"a2a"}}, PolicyContext{Protocol: "mcp"}, false},
		{"empty protocol", PolicyRules{RequiredProtocols: []string{"mcp"}}, PolicyContext{}, false},
		{"no rule set", PolicyRules{}, PolicyContext{Protocol: "mcp"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: tt.rules}
			result := Evaluate(doc, tt.pctx, LayerCaller)
			if result.Allowed != tt.wantOK {
				t.Errorf("Allowed = %v, want %v; violations: %v", result.Allowed, tt.wantOK, result.Reason())
			}
		})
	}
}

func TestEvaluateRequiredAuthTypes(t *testing.T) {
	tests := []struct {
		name   string
		rules  PolicyRules
		pctx   PolicyContext
		wantOK bool
	}{
		{"allowed auth", PolicyRules{RequiredAuthTypes: []string{"mtls", "oauth2"}}, PolicyContext{AuthType: "mtls"}, true},
		{"denied auth", PolicyRules{RequiredAuthTypes: []string{"mtls"}}, PolicyContext{AuthType: "bearer"}, false},
		{"no auth provided", PolicyRules{RequiredAuthTypes: []string{"mtls"}}, PolicyContext{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: tt.rules}
			result := Evaluate(doc, tt.pctx, LayerCaller)
			if result.Allowed != tt.wantOK {
				t.Errorf("Allowed = %v, want %v", result.Allowed, tt.wantOK)
			}
		})
	}
}

func TestEvaluateRequireDNSSEC(t *testing.T) {
	tests := []struct {
		name   string
		rules  PolicyRules
		pctx   PolicyContext
		wantOK bool
	}{
		{"dnssec required and present", PolicyRules{RequireDNSSEC: true}, PolicyContext{DNSSECValidated: true}, true},
		{"dnssec required but missing", PolicyRules{RequireDNSSEC: true}, PolicyContext{DNSSECValidated: false}, false},
		{"dnssec not required", PolicyRules{RequireDNSSEC: false}, PolicyContext{DNSSECValidated: false}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: tt.rules}
			result := Evaluate(doc, tt.pctx, LayerCaller)
			if result.Allowed != tt.wantOK {
				t.Errorf("Allowed = %v, want %v", result.Allowed, tt.wantOK)
			}
		})
	}
}

func TestEvaluateRequireMutualTLS(t *testing.T) {
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{RequireMutualTLS: true}}

	result := Evaluate(doc, PolicyContext{HasMutualTLS: true}, LayerCaller)
	if !result.Allowed {
		t.Error("expected allowed with mutual TLS present")
	}

	result = Evaluate(doc, PolicyContext{HasMutualTLS: false}, LayerCaller)
	if result.Allowed {
		t.Error("expected denied without mutual TLS")
	}
}

func TestEvaluateMinTLSVersion(t *testing.T) {
	tests := []struct {
		name   string
		min    string
		have   string
		wantOK bool
	}{
		{"meets minimum", "1.2", "1.3", true},
		{"exact match", "1.2", "1.2", true},
		{"below minimum", "1.3", "1.2", false},
		{"no TLS version", "1.2", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{MinTLSVersion: &tt.min}}
			result := Evaluate(doc, PolicyContext{TLSVersion: tt.have}, LayerCaller)
			if result.Allowed != tt.wantOK {
				t.Errorf("Allowed = %v, want %v", result.Allowed, tt.wantOK)
			}
		})
	}
}

func TestEvaluateCallerTrustScore(t *testing.T) {
	tests := []struct {
		name     string
		required float64
		score    *float64
		wantOK   bool
	}{
		{"score meets threshold", 0.8, ptr(0.9), true},
		{"score below threshold", 0.8, ptr(0.5), false},
		{"no score provided", 0.8, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{RequiredCallerTrustScore: &tt.required}}
			result := Evaluate(doc, PolicyContext{CallerTrustScore: tt.score}, LayerCaller)
			if result.Allowed != tt.wantOK {
				t.Errorf("Allowed = %v, want %v", result.Allowed, tt.wantOK)
			}
		})
	}
}

func TestEvaluateAllowedCallerDomains(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		domain  string
		wantOK  bool
	}{
		{"exact match", []string{"example.com"}, "example.com", true},
		{"wildcard match", []string{"*.example.com"}, "api.example.com", true},
		{"no match", []string{"*.internal.com"}, "api.example.com", false},
		{"empty domain", []string{"*.example.com"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{AllowedCallerDomains: tt.allowed}}
			result := Evaluate(doc, PolicyContext{CallerDomain: tt.domain}, LayerCaller)
			if result.Allowed != tt.wantOK {
				t.Errorf("Allowed = %v, want %v", result.Allowed, tt.wantOK)
			}
		})
	}
}

func TestEvaluateBlockedCallerDomains(t *testing.T) {
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{
		BlockedCallerDomains: []string{"*.evil.com"},
	}}

	result := Evaluate(doc, PolicyContext{CallerDomain: "bot.evil.com"}, LayerCaller)
	if result.Allowed {
		t.Error("expected blocked domain to be denied")
	}

	result = Evaluate(doc, PolicyContext{CallerDomain: "good.example.com"}, LayerCaller)
	if !result.Allowed {
		t.Error("expected non-blocked domain to be allowed")
	}
}

func TestEvaluateAllowedMethods(t *testing.T) {
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{
		AllowedMethods: []string{"tools/call", "tools/list"},
	}}

	result := Evaluate(doc, PolicyContext{Method: "tools/call"}, LayerCaller)
	if !result.Allowed {
		t.Error("expected allowed method")
	}

	result = Evaluate(doc, PolicyContext{Method: "admin/delete"}, LayerCaller)
	if result.Allowed {
		t.Error("expected denied method")
	}
}

func TestEvaluateAllowedIntents(t *testing.T) {
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{
		AllowedIntents: []string{"query", "discover"},
	}}

	result := Evaluate(doc, PolicyContext{Intent: "query"}, LayerCaller)
	if !result.Allowed {
		t.Error("expected allowed intent")
	}

	result = Evaluate(doc, PolicyContext{Intent: "admin"}, LayerCaller)
	if result.Allowed {
		t.Error("expected denied intent")
	}
}

func TestEvaluateGeoRestrictions(t *testing.T) {
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{
		GeoRestrictions: []string{"US", "EU"},
	}}

	result := Evaluate(doc, PolicyContext{GeoCountry: "US"}, LayerCaller)
	if !result.Allowed {
		t.Error("expected allowed geo")
	}

	result = Evaluate(doc, PolicyContext{GeoCountry: "CN"}, LayerCaller)
	if result.Allowed {
		t.Error("expected denied geo")
	}
}

func TestEvaluateConsentRequired(t *testing.T) {
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{ConsentRequired: true}}

	result := Evaluate(doc, PolicyContext{ConsentToken: "tok_123"}, LayerCaller)
	if !result.Allowed {
		t.Error("expected allowed with consent token")
	}

	result = Evaluate(doc, PolicyContext{}, LayerCaller)
	if result.Allowed {
		t.Error("expected denied without consent token")
	}
}

func TestEvaluateLayerFiltering(t *testing.T) {
	// rate_limits only applies at Layer 2 — should produce warning at Layer 1, not violation
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{
		RateLimits: &RateLimitConfig{MaxPerMinute: ptr(10)},
	}}

	result := Evaluate(doc, PolicyContext{}, LayerCaller)
	if !result.Allowed {
		t.Error("rate_limits should not deny at Layer 1")
	}
	// rate_limits at Layer 1 doesn't apply (appliesToLayer returns false for caller),
	// but we emit a warning if there are rate limits. Check we got no violations.
	if len(result.Violations) > 0 {
		t.Errorf("expected 0 violations for rate_limits at Layer 1, got %d", len(result.Violations))
	}
}

func TestEvaluateMultipleViolations(t *testing.T) {
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{
		RequiredProtocols: []string{"a2a"},
		RequireDNSSEC:     true,
		ConsentRequired:   true,
	}}

	result := Evaluate(doc, PolicyContext{Protocol: "mcp"}, LayerCaller)
	if result.Allowed {
		t.Error("expected denied with multiple violations")
	}
	if len(result.Violations) != 3 {
		t.Errorf("expected 3 violations, got %d: %s", len(result.Violations), result.Reason())
	}
}

func TestEvaluateEmptyRulesAllowed(t *testing.T) {
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{}}
	result := Evaluate(doc, PolicyContext{}, LayerCaller)
	if !result.Allowed {
		t.Error("empty rules should allow everything")
	}
}

func TestPolicyResultReason(t *testing.T) {
	allowed := PolicyResult{Allowed: true}
	if allowed.Reason() != "allowed" {
		t.Errorf("expected 'allowed', got %q", allowed.Reason())
	}

	denied := PolicyResult{
		Allowed: false,
		Violations: []PolicyViolation{
			{Rule: "require_dnssec", Detail: "not validated"},
			{Rule: "consent_required", Detail: "no token"},
		},
	}
	reason := denied.Reason()
	if reason != "require_dnssec: not validated; consent_required: no token" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestCompareTLSVersion(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int // -1, 0, 1
	}{
		{"equal", "1.2", "1.2", 0},
		{"less", "1.2", "1.3", -1},
		{"greater", "1.3", "1.2", 1},
		{"double digit minor", "1.3", "1.12", -1},
		{"major differs", "2.0", "1.3", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareTLSVersion(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("compareTLSVersion(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestIsWithinAvailability(t *testing.T) {
	tests := []struct {
		name   string
		hours  string
		tz     string
		now    time.Time
		wantIn bool
	}{
		{
			"within window",
			"09:00-17:00", "UTC",
			time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC),
			true,
		},
		{
			"before window",
			"09:00-17:00", "UTC",
			time.Date(2026, 3, 27, 8, 0, 0, 0, time.UTC),
			false,
		},
		{
			"after window",
			"09:00-17:00", "UTC",
			time.Date(2026, 3, 27, 18, 0, 0, 0, time.UTC),
			false,
		},
		{
			"midnight wrap — inside (late night)",
			"22:00-06:00", "UTC",
			time.Date(2026, 3, 27, 23, 0, 0, 0, time.UTC),
			true,
		},
		{
			"midnight wrap — inside (early morning)",
			"22:00-06:00", "UTC",
			time.Date(2026, 3, 27, 3, 0, 0, 0, time.UTC),
			true,
		},
		{
			"midnight wrap — outside",
			"22:00-06:00", "UTC",
			time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC),
			false,
		},
		{
			"bad timezone — fail-open",
			"09:00-17:00", "Invalid/TZ",
			time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC),
			true,
		},
		{
			"malformed hours — fail-open",
			"invalid", "UTC",
			time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC),
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			avail := &AvailabilityConfig{Hours: tt.hours, Timezone: tt.tz}
			got := isWithinAvailability(avail, tt.now)
			if got != tt.wantIn {
				t.Errorf("isWithinAvailability(%q, %q, %v) = %v, want %v",
					tt.hours, tt.tz, tt.now, got, tt.wantIn)
			}
		})
	}
}

func TestEvaluateRateLimitsWarningAtLayer1(t *testing.T) {
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{
		RateLimits: &RateLimitConfig{MaxPerMinute: ptr(10)},
	}}

	result := Evaluate(doc, PolicyContext{}, LayerCaller)
	if !result.Allowed {
		t.Error("rate_limits should not deny at Layer 1")
	}
	if len(result.Warnings) == 0 {
		t.Error("expected warning for rate_limits at Layer 1")
	}
	foundWarning := false
	for _, w := range result.Warnings {
		if w.Rule == "rate_limits" {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Error("expected rate_limits warning at Layer 1")
	}
}

func TestEvaluateDataClassificationWarning(t *testing.T) {
	classification := "confidential"
	doc := &PolicyDocument{Version: "1.0", Agent: "test", Rules: PolicyRules{
		DataClassification: &classification,
	}}

	result := Evaluate(doc, PolicyContext{}, LayerCaller)
	if !result.Allowed {
		t.Error("data_classification should not deny")
	}
	if len(result.Warnings) == 0 {
		t.Error("expected warning for data_classification")
	}
}
