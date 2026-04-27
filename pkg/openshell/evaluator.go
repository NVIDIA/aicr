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
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // embed IANA timezone database
)

// Evaluate runs all applicable policy rules for the given layer against the
// provided context and returns a PolicyResult. This is a pure function with
// no I/O — the policy document must already be fetched.
func Evaluate(doc *PolicyDocument, pctx PolicyContext, layer EnforcementLayer) PolicyResult {
	var violations []PolicyViolation
	var warnings []PolicyViolation

	rules := doc.Rules

	// Native rules: each check appends to violations or warnings.
	// Rules not applicable to this layer are skipped.

	if len(rules.RequiredProtocols) > 0 && appliesToLayer("required_protocols", layer) {
		if pctx.Protocol == "" || !contains(rules.RequiredProtocols, pctx.Protocol) {
			violations = append(violations, PolicyViolation{
				Rule:   "required_protocols",
				Detail: fmt.Sprintf("protocol %q not in %v", pctx.Protocol, rules.RequiredProtocols),
				Layer:  string(layer),
			})
		}
	}

	if len(rules.RequiredAuthTypes) > 0 && appliesToLayer("required_auth_types", layer) {
		if pctx.AuthType == "" || !contains(rules.RequiredAuthTypes, pctx.AuthType) {
			violations = append(violations, PolicyViolation{
				Rule:   "required_auth_types",
				Detail: fmt.Sprintf("auth type %q not in %v", pctx.AuthType, rules.RequiredAuthTypes),
				Layer:  string(layer),
			})
		}
	}

	if rules.RequireDNSSEC && appliesToLayer("require_dnssec", layer) {
		if !pctx.DNSSECValidated {
			violations = append(violations, PolicyViolation{
				Rule:   "require_dnssec",
				Detail: "DNSSEC validation required but not present",
				Layer:  string(layer),
			})
		}
	}

	if rules.RequireMutualTLS && appliesToLayer("require_mutual_tls", layer) {
		if !pctx.HasMutualTLS {
			violations = append(violations, PolicyViolation{
				Rule:   "require_mutual_tls",
				Detail: "mutual TLS required but not present",
				Layer:  string(layer),
			})
		}
	}

	if rules.MinTLSVersion != nil && appliesToLayer("min_tls_version", layer) {
		if pctx.TLSVersion == "" || compareTLSVersion(pctx.TLSVersion, *rules.MinTLSVersion) < 0 {
			violations = append(violations, PolicyViolation{
				Rule:   "min_tls_version",
				Detail: fmt.Sprintf("TLS version %q below minimum %q", pctx.TLSVersion, *rules.MinTLSVersion),
				Layer:  string(layer),
			})
		}
	}

	if rules.RequiredCallerTrustScore != nil && appliesToLayer("required_caller_trust_score", layer) {
		if pctx.CallerTrustScore == nil || *pctx.CallerTrustScore < *rules.RequiredCallerTrustScore {
			score := "nil"
			if pctx.CallerTrustScore != nil {
				score = fmt.Sprintf("%.2f", *pctx.CallerTrustScore)
			}
			violations = append(violations, PolicyViolation{
				Rule:   "required_caller_trust_score",
				Detail: fmt.Sprintf("caller trust score %s below required %.2f", score, *rules.RequiredCallerTrustScore),
				Layer:  string(layer),
			})
		}
	}

	// rate_limits: enforced at the layer named in ruleEnforcementLayers
	// (currently Layer 2 only). At Layer 1 we emit an informational warning
	// so callers are aware that the target has rate limits in effect.
	if rules.RateLimits != nil {
		switch {
		case appliesToLayer("rate_limits", layer):
			violations = append(violations, PolicyViolation{
				Rule:   "rate_limits",
				Detail: "rate limits exceeded",
				Layer:  string(layer),
			})
		case layer == LayerCaller:
			warnings = append(warnings, PolicyViolation{
				Rule:   "rate_limits",
				Detail: "target has rate limits configured (enforced at target)",
				Layer:  string(layer),
			})
		}
	}

	// max_payload_bytes: Layer 2 only — skip at Layer 1
	// (appliesToLayer returns false for LayerCaller)

	if len(rules.AllowedCallerDomains) > 0 && appliesToLayer("allowed_caller_domains", layer) {
		if !matchesAnyGlob(rules.AllowedCallerDomains, pctx.CallerDomain) {
			violations = append(violations, PolicyViolation{
				Rule:   "allowed_caller_domains",
				Detail: fmt.Sprintf("caller domain %q not in allowed list", pctx.CallerDomain),
				Layer:  string(layer),
			})
		}
	}

	if len(rules.BlockedCallerDomains) > 0 && appliesToLayer("blocked_caller_domains", layer) {
		if matchesAnyGlob(rules.BlockedCallerDomains, pctx.CallerDomain) {
			violations = append(violations, PolicyViolation{
				Rule:   "blocked_caller_domains",
				Detail: fmt.Sprintf("caller domain %q is blocked", pctx.CallerDomain),
				Layer:  string(layer),
			})
		}
	}

	if len(rules.AllowedMethods) > 0 && appliesToLayer("allowed_methods", layer) {
		if pctx.Method == "" || !contains(rules.AllowedMethods, pctx.Method) {
			violations = append(violations, PolicyViolation{
				Rule:   "allowed_methods",
				Detail: fmt.Sprintf("method %q not in %v", pctx.Method, rules.AllowedMethods),
				Layer:  string(layer),
			})
		}
	}

	if len(rules.AllowedIntents) > 0 && appliesToLayer("allowed_intents", layer) {
		if pctx.Intent == "" || !contains(rules.AllowedIntents, pctx.Intent) {
			violations = append(violations, PolicyViolation{
				Rule:   "allowed_intents",
				Detail: fmt.Sprintf("intent %q not in %v", pctx.Intent, rules.AllowedIntents),
				Layer:  string(layer),
			})
		}
	}

	if len(rules.GeoRestrictions) > 0 && appliesToLayer("geo_restrictions", layer) {
		if pctx.GeoCountry == "" || !contains(rules.GeoRestrictions, pctx.GeoCountry) {
			violations = append(violations, PolicyViolation{
				Rule:   "geo_restrictions",
				Detail: fmt.Sprintf("geo country %q not in %v", pctx.GeoCountry, rules.GeoRestrictions),
				Layer:  string(layer),
			})
		}
	}

	if rules.Availability != nil && appliesToLayer("availability", layer) {
		if !isWithinAvailability(rules.Availability, time.Now()) {
			violations = append(violations, PolicyViolation{
				Rule:   "availability",
				Detail: fmt.Sprintf("current time outside availability window %q (%s)", rules.Availability.Hours, rules.Availability.Timezone),
				Layer:  string(layer),
			})
		}
	}

	if rules.DataClassification != nil && appliesToLayer("data_classification", layer) {
		// Data classification is informational at Layer 1; warn so callers are aware.
		warnings = append(warnings, PolicyViolation{
			Rule:   "data_classification",
			Detail: fmt.Sprintf("target data classification: %s", *rules.DataClassification),
			Layer:  string(layer),
		})
	}

	if rules.ConsentRequired && appliesToLayer("consent_required", layer) {
		if pctx.ConsentToken == "" {
			violations = append(violations, PolicyViolation{
				Rule:   "consent_required",
				Detail: "consent token required but not provided",
				Layer:  string(layer),
			})
		}
	}

	// CEL rules: deferred — log warning if present
	if len(rules.CELRules) > 0 {
		slog.Debug("CEL rules present but not yet supported in OpenShell",
			"count", len(rules.CELRules),
			"agent", doc.Agent,
		)
	}

	return PolicyResult{
		Allowed:    len(violations) == 0,
		Violations: violations,
		Warnings:   warnings,
	}
}

// contains checks if a string slice contains the given value.
func contains(ss []string, val string) bool {
	for _, s := range ss {
		if s == val {
			return true
		}
	}
	return false
}

// matchesAnyGlob checks if val matches any of the glob patterns.
// Uses filepath.Match which supports *, ?, and [] — similar to Python's fnmatch.
// Malformed patterns are logged and skipped.
func matchesAnyGlob(patterns []string, val string) bool {
	for _, p := range patterns {
		matched, err := filepath.Match(p, val)
		if err != nil {
			slog.Warn("invalid glob pattern in policy rule, skipping",
				"pattern", p, "error", err)
			continue
		}
		if matched {
			return true
		}
	}
	return false
}

// compareTLSVersion compares two TLS version strings (e.g. "1.2", "1.3").
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
// Falls back to lexicographic comparison if parsing fails.
func compareTLSVersion(a, b string) int {
	aMajor, aMinor, aOK := parseTLSVersion(a)
	bMajor, bMinor, bOK := parseTLSVersion(b)
	if !aOK || !bOK {
		return strings.Compare(a, b)
	}
	if aMajor != bMajor {
		if aMajor < bMajor {
			return -1
		}
		return 1
	}
	if aMinor != bMinor {
		if aMinor < bMinor {
			return -1
		}
		return 1
	}
	return 0
}

// parseTLSVersion parses a "major.minor" TLS version string into integers.
func parseTLSVersion(v string) (major, minor int, ok bool) {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// isWithinAvailability checks if the given time falls within the
// availability window specified by hours and timezone.
func isWithinAvailability(avail *AvailabilityConfig, now time.Time) bool {
	tz := avail.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		slog.Warn("failed to load timezone for availability check",
			"timezone", tz, "error", err)
		return true // fail-open on bad timezone
	}

	now = now.In(loc)
	parts := strings.SplitN(avail.Hours, "-", 2)
	if len(parts) != 2 {
		return true // malformed hours string — fail-open
	}

	start, err := time.Parse("15:04", strings.TrimSpace(parts[0]))
	if err != nil {
		return true
	}
	end, err := time.Parse("15:04", strings.TrimSpace(parts[1]))
	if err != nil {
		return true
	}

	nowMinutes := now.Hour()*60 + now.Minute()
	startMinutes := start.Hour()*60 + start.Minute()
	endMinutes := end.Hour()*60 + end.Minute()

	// Handle midnight wrap (e.g., "22:00-06:00")
	if startMinutes <= endMinutes {
		return nowMinutes >= startMinutes && nowMinutes < endMinutes
	}
	return nowMinutes >= startMinutes || nowMinutes < endMinutes
}
