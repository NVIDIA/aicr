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

package corroborate

import (
	"github.com/NVIDIA/aicr/pkg/evidence/allowlist"
)

// Class is a corroboration source class. A signer's class is derived from its
// verified OIDC identity against the allowlist — never a free-text field.
type Class string

const (
	// ClassFirstParty is the project's own UAT signer (GH-Actions OIDC pinned
	// to NVIDIA/aicr).
	ClassFirstParty Class = Class(allowlist.ClassFirstParty)

	// ClassCommunity is an allowlisted community signer (and the fallback class
	// for a verified-but-unallowlisted "reported" signer).
	ClassCommunity Class = Class(allowlist.ClassCommunity)

	// ClassPartner is an allowlisted partner signer.
	ClassPartner Class = Class(allowlist.ClassPartner)
)

// Allowlist is the in-tree, PR-reviewed signer allowlist
// (recipes/evidence/allowlist.yaml, owned by GP1). GP4 consumes the shared
// authoritative loader in pkg/evidence/allowlist so producer (GP2) and
// consumer (GP4) parse the identical identityPattern/source schema (#1505).
type Allowlist = allowlist.Allowlist

// LoadAllowlist reads and validates the allowlist at path via the shared
// loader. The read is size-bounded before parse, the schema version is
// gated (1.0.x), and the anti-sybil invariants (anchored entries, disjoint
// classes, no overlaps) are enforced — a malformed file fails closed.
func LoadAllowlist(path string) (*Allowlist, error) {
	al, err := allowlist.Load(path)
	if err != nil {
		return nil, err // already coded (ErrCodeNotFound / ErrCodeInvalidRequest)
	}
	return al, nil
}

// classifySigner adapts the shared allowlist Classify to GP4 semantics: a
// verified signer matching no entry is admitted as a zero-weight reported
// dot — class community, allowlisted false.
func classifySigner(a *Allowlist, issuer, identity string) (Class, bool) {
	class, _, ok := a.Classify(issuer, identity)
	if !ok {
		return ClassCommunity, false
	}
	return Class(class), true
}
