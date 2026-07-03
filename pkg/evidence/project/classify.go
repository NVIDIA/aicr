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

package project

import (
	"regexp"

	"github.com/NVIDIA/aicr/pkg/evidence/allowlist"
)

// Class is the trust classification recorded for a verified signer. It
// drives how the GP4 consensus model weighs a source: only allowlisted
// sources count toward consensus; everything else is reported but
// contributes zero weight.
//
// Classification delegates to the shared authoritative loader in
// pkg/evidence/allowlist so the producer (GP2) and the GP4 consumer
// (pkg/corroborate) read the identical recipes/evidence/allowlist.yaml
// (owned by GP1, identityPattern/source schema) and classify a verified
// signer identically (#1505).
type Class string

const (
	// ClassFirstParty marks evidence produced by AICR's own UAT pipeline.
	ClassFirstParty Class = Class(allowlist.ClassFirstParty)

	// ClassCommunity marks evidence from an allowlisted community signer,
	// and is also the fail-closed fallback for a verified-but-unallowlisted
	// signer (reported, but never counted).
	ClassCommunity Class = Class(allowlist.ClassCommunity)

	// ClassPartner marks evidence from an allowlisted partner signer.
	ClassPartner Class = Class(allowlist.ClassPartner)
)

// firstPartyIssuer is the GitHub Actions OIDC token issuer used by the
// interim first-party heuristic (see Classify).
const firstPartyIssuer = "https://token.actions.githubusercontent.com"

// firstPartyIdentity matches the SubjectAlternativeName of AICR's own UAT
// workflows (uat-aws.yaml / uat-gcp.yaml on a branch ref) — the only
// identity the interim heuristic may admit as first-party. Fully anchored
// (^…$) so neither a look-alike host nor an arbitrary other path/ref under
// NVIDIA/aicr (e.g. a fork-PR workflow or a non-UAT workflow) can satisfy
// it. Mirrors FIRST_PARTY_IDENTITY in evidence-ingest.yaml and the
// firstParty entries in recipes/evidence/allowlist.yaml. Used only when no
// allowlist file is loaded (nil *Allowlist).
var firstPartyIdentity = regexp.MustCompile(
	`^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-(aws|gcp)\.yaml@refs/heads/.+$`)

// Allowlist wraps the shared GP1 allowlist loader (pkg/evidence/allowlist)
// for the GP2 producer. A nil *Allowlist is valid and applies only the
// interim first-party heuristic plus the community fail-closed default.
type Allowlist struct {
	shared *allowlist.Allowlist
}

// LoadAllowlist reads, parses, and validates the allowlist at path via the
// shared loader: size-bounded read, schema-version gate (1.0.x), anchored
// entries, disjoint classes, no overlaps. A malformed file fails closed
// with ErrCodeInvalidRequest (ErrCodeNotFound when missing).
func LoadAllowlist(path string) (*Allowlist, error) {
	al, err := allowlist.Load(path)
	if err != nil {
		return nil, err // already coded by the shared loader
	}
	return &Allowlist{shared: al}, nil
}

// Classify resolves a verified (issuer, identity) to its class and
// whether it counts toward corroboration. Resolution order:
//
//  1. With an allowlist loaded, the shared classifier decides: a matching
//     entry wins (allowlisted=true); an unmatched signer is community, not
//     allowlisted — the fail-closed default (reported, never counted).
//  2. When no allowlist is loaded (nil receiver), the built-in first-party
//     heuristic admits AICR's own UAT identity so it is not mislabeled
//     community; everything else falls through to community, not
//     allowlisted.
func (a *Allowlist) Classify(issuer, identity string) (Class, bool) {
	if a != nil && a.shared != nil {
		if class, _, ok := a.shared.Classify(issuer, identity); ok {
			return Class(class), true
		}
		return ClassCommunity, false
	}
	if issuer == firstPartyIssuer && firstPartyIdentity.MatchString(identity) {
		return ClassFirstParty, true
	}
	return ClassCommunity, false
}
