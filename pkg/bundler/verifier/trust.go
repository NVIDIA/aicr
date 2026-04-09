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

package verifier

import (
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/constraints"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// TrustLevel represents the verification trust level of a bundle.
type TrustLevel string

const (
	// TrustUnknown indicates missing attestation or checksum files.
	TrustUnknown TrustLevel = "unknown"

	// TrustUnverified indicates checksums are valid but no attestation files exist
	// (bundle was created with --attest not used).
	TrustUnverified TrustLevel = "unverified"

	// TrustAttested indicates the full chain is cryptographically verified but
	// external data (--data) was used, capping trust because the data's own
	// provenance is unknown.
	TrustAttested TrustLevel = "attested"

	// TrustVerified indicates checksums valid, bundle attestation verified,
	// binary attestation verified with identity pinned to NVIDIA CI, and no
	// external data.
	TrustVerified TrustLevel = "verified"
)

// trustOrder defines the ordering for trust level comparison.
var trustOrder = map[TrustLevel]int{
	TrustUnknown:    1,
	TrustUnverified: 2,
	TrustAttested:   3,
	TrustVerified:   4,
}

// String returns the trust level name.
func (t TrustLevel) String() string {
	return string(t)
}

// MeetsMinimum returns true if this trust level is at least the given minimum.
func (t TrustLevel) MeetsMinimum(minimum TrustLevel) bool {
	return trustOrder[t] >= trustOrder[minimum]
}

// ParseTrustLevel parses a string into a TrustLevel.
func ParseTrustLevel(s string) (TrustLevel, error) {
	level := TrustLevel(strings.ToLower(strings.TrimSpace(s)))
	if _, ok := trustOrder[level]; !ok {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid trust level %q: must be one of unknown, unverified, attested, verified", s))
	}
	return level, nil
}

// GetTrustLevels returns all valid trust level names sorted alphabetically.
// This excludes "max" which is a meta-value for auto-detection, not a real level.
func GetTrustLevels() []string {
	levels := make([]string, 0, len(trustOrder))
	for level := range trustOrder {
		levels = append(levels, string(level))
	}
	sort.Strings(levels)
	return levels
}

// VerifyResult contains the outcome of bundle verification.
type VerifyResult struct {
	// TrustLevel is the computed trust level for the bundle.
	TrustLevel TrustLevel `json:"trustLevel"`

	// ChecksumsPassed indicates whether all content files match checksums.txt.
	ChecksumsPassed bool `json:"checksumsPassed"`

	// ChecksumFiles is the number of files verified by checksum.
	ChecksumFiles int `json:"checksumFiles"`

	// BundleAttested indicates whether the bundle attestation was verified.
	BundleAttested bool `json:"bundleAttested"`

	// BinaryAttested indicates whether the binary attestation was verified.
	BinaryAttested bool `json:"binaryAttested"`

	// IdentityPinned indicates whether the binary attestation identity was pinned to NVIDIA CI.
	IdentityPinned bool `json:"identityPinned"`

	// BundleCreator is the OIDC identity from the bundle attestation signing certificate.
	BundleCreator string `json:"bundleCreator,omitempty"`

	// BinaryBuilder is the certificate subject from the binary attestation.
	BinaryBuilder string `json:"binaryBuilder,omitempty"`

	// ToolVersion is the aicr version extracted from the attestation predicate.
	ToolVersion string `json:"toolVersion,omitempty"`

	// HasExternalData indicates the bundle contains external data files (data/ directory).
	HasExternalData bool `json:"hasExternalData"`

	// Errors contains verification failure messages.
	Errors []string `json:"errors,omitempty"`
}

// Policy defines verification requirements to enforce after verification.
type Policy struct {
	// MinTrustLevel is the minimum required trust level ("max" resolves to
	// the highest achievable level for the bundle).
	MinTrustLevel string

	// RequireCreator requires the bundle attestation creator to match.
	RequireCreator string

	// VersionConstraint is a version constraint expression for the CLI version.
	// Supports operators: >=, >, <=, <, ==, !=.
	// A bare version (e.g. "0.8.0") is treated as ">= 0.8.0".
	VersionConstraint string
}

// CheckPolicy validates the verification result against a policy.
// Returns an empty string if all checks pass, or a failure description.
func (r *VerifyResult) CheckPolicy(p Policy) (string, error) {
	// Trust level check
	if p.MinTrustLevel == "max" {
		maxLevel := r.MaxAchievableTrustLevel()
		if !r.TrustLevel.MeetsMinimum(maxLevel) {
			return fmt.Sprintf("trust level %q does not meet maximum achievable %q for this bundle",
				r.TrustLevel, maxLevel), nil
		}
	} else if p.MinTrustLevel != "" {
		minLevel, err := ParseTrustLevel(p.MinTrustLevel)
		if err != nil {
			return "", err
		}
		if !r.TrustLevel.MeetsMinimum(minLevel) {
			return fmt.Sprintf("trust level %q does not meet minimum %q",
				r.TrustLevel, minLevel), nil
		}
	}

	// Creator check
	if p.RequireCreator != "" && r.BundleCreator != p.RequireCreator {
		return fmt.Sprintf("bundle creator %q does not match required %q",
			r.BundleCreator, p.RequireCreator), nil
	}

	// Tool version constraint check: bare versions default to ">=" semantics
	if p.VersionConstraint != "" {
		if r.ToolVersion == "" {
			return "tool version not available in attestation (bundle may be unattested)", nil
		}
		expr := strings.TrimSpace(p.VersionConstraint)
		if !hasOperatorPrefix(expr) {
			expr = ">= " + expr
		}
		constraint, err := constraints.ParseConstraintExpression(expr)
		if err != nil {
			return "", errors.Wrap(errors.ErrCodeInvalidRequest, "invalid tool version constraint", err)
		}
		passed, err := constraint.Evaluate(r.ToolVersion)
		if err != nil {
			return "", errors.Wrap(errors.ErrCodeInvalidRequest, "tool version evaluation failed", err)
		}
		if !passed {
			return fmt.Sprintf("tool version %q does not satisfy constraint %q",
				r.ToolVersion, constraint.String()), nil
		}
	}

	return "", nil
}

// MaxAchievableTrustLevel returns the highest trust level this bundle could
// achieve based on its contents. Used by --min-trust-level max to enforce
// that verification reached the expected level:
//   - verified: standard bundle with both attestations, no external data
//   - attested: external data present (caps trust regardless of attestation chain)
//   - unverified: no attestation files (--attest not used was used)
//   - unknown: checksums failed or missing
func (r *VerifyResult) MaxAchievableTrustLevel() TrustLevel {
	if r == nil || !r.ChecksumsPassed {
		return TrustUnknown
	}
	if !r.BundleAttested {
		return TrustUnverified
	}
	if r.HasExternalData {
		return TrustAttested
	}
	return TrustVerified
}

// hasOperatorPrefix returns true if the expression starts with a comparison operator.
func hasOperatorPrefix(expr string) bool {
	for _, prefix := range []string{">=", "<=", "!=", "==", ">", "<"} {
		if strings.HasPrefix(expr, prefix) {
			return true
		}
	}
	return false
}
