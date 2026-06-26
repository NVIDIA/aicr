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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// criteriaFromCoordinate inverts a coordinate back to its five-dimension
// Criteria for display in the dashboard grid. The coordinate carries every
// dimension (group=service, dashboard=accelerator-os, tab=intent[-platform]),
// so no recipe resolution or metadata.name parsing is needed.
//
// It splits the dashboard on a known OS suffix and the tab on a known intent
// prefix (both drawn from the criteria registry, never a closed enum), then
// proves the inversion by round-tripping through the shared
// recipe.CoordinateFor — a mismatch means the on-disk coordinate is
// inconsistent and fails closed.
func criteriaFromCoordinate(co recipe.Coordinate) (recipe.Criteria, error) {
	accel, osName, err := splitDashboard(co.Dashboard)
	if err != nil {
		return recipe.Criteria{}, err
	}
	intent, platform, err := splitTab(co.Tab)
	if err != nil {
		return recipe.Criteria{}, err
	}

	crit := recipe.Criteria{
		Service:     recipe.CriteriaServiceType(co.Group),
		Accelerator: recipe.CriteriaAcceleratorType(accel),
		OS:          recipe.CriteriaOSType(osName),
		Intent:      recipe.CriteriaIntentType(intent),
		Platform:    recipe.CriteriaPlatformType(platform),
	}

	got, err := recipe.CoordinateFor(&crit)
	if err != nil {
		// CoordinateFor already returns a coded *StructuredError; PropagateOrWrap
		// preserves that code instead of overwriting it with a fresh wrapper.
		return recipe.Criteria{}, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
			"coordinate "+co.Path()+" does not round-trip through CoordinateFor")
	}
	if got.Path() != co.Path() {
		return recipe.Criteria{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("coordinate %q inverts to criteria that re-maps to %q", co.Path(), got.Path()))
	}
	return crit, nil
}

// splitDashboard splits "<accelerator>-<os>". It prefers the longest matching
// OS suffix from the registry (so a multi-token accelerator like "rtx-pro-6000"
// stays intact), and falls back to the last hyphen for an unknown-but-
// well-formed OS — the taxonomy is open, not a closed enum, so a future or
// community OS must still invert. The caller's round-trip through CoordinateFor
// validates the split.
func splitDashboard(dashboard string) (accel, osName string, err error) {
	best := ""
	for _, os := range recipe.GetCriteriaOSTypes() {
		if os == recipe.CriteriaAnyValue {
			continue
		}
		if strings.HasSuffix(dashboard, "-"+os) && len(dashboard) > len(os)+1 {
			if len(os) > len(best) {
				best = os
			}
		}
	}
	if best != "" {
		return strings.TrimSuffix(dashboard, "-"+best), best, nil
	}
	i := strings.LastIndex(dashboard, "-")
	if i <= 0 || i >= len(dashboard)-1 {
		return "", "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("dashboard %q is not <accelerator>-<os>", dashboard))
	}
	return dashboard[:i], dashboard[i+1:], nil
}

// splitTab splits "<intent>" or "<intent>-<platform>". It prefers a known
// intent prefix from the registry and falls back to the first hyphen for an
// unknown-but-well-formed intent (open taxonomy). A tab with no hyphen is a
// bare intent (empty platform); an empty tab is rejected.
func splitTab(tab string) (intent, platform string, err error) {
	for _, in := range recipe.GetCriteriaIntentTypes() {
		if in == recipe.CriteriaAnyValue {
			continue
		}
		if tab == in {
			return in, "", nil
		}
		if strings.HasPrefix(tab, in+"-") {
			return in, strings.TrimPrefix(tab, in+"-"), nil
		}
	}
	if tab == "" {
		return "", "", errors.New(errors.ErrCodeInvalidRequest, "tab is empty")
	}
	if i := strings.Index(tab, "-"); i > 0 {
		return tab[:i], tab[i+1:], nil
	}
	return tab, "", nil
}

// labelFor derives a stable, human-readable source label from a run's verified
// signer. First-party signers get a fixed label; others are named by their
// "<org>/<repo>" when the identity is a recognized code-host URL, else by host
// (falling back to the IDHash when the identity yields no host).
func labelFor(m RunMeta, class Class) string {
	if class == ClassFirstParty {
		return "NVIDIA UAT"
	}
	id := m.Signer.Identity
	if i := strings.Index(id, "://"); i >= 0 {
		id = id[i+len("://"):]
	}
	var segs []string
	for _, p := range strings.Split(id, "/") {
		if p != "" {
			segs = append(segs, p)
		}
	}
	switch {
	case len(segs) >= 3 && isCodeHost(segs[0]):
		// Only code-host subjects carry a meaningful <org>/<repo>. A non-code
		// OIDC subject (e.g. a partner attestation endpoint with a deep path)
		// would otherwise be mislabeled as "<path[1]>/<path[2]>".
		return segs[1] + "/" + segs[2]
	case len(segs) >= 1:
		return segs[0]
	default:
		return m.Signer.IDHash
	}
}

// isCodeHost reports whether host is a recognized code-hosting platform whose
// identity URL path is "<org>/<repo>/...". Matches the hosts the allowlist's
// signer identities are drawn from.
func isCodeHost(host string) bool {
	switch host {
	case "github.com", "gitlab.com":
		return true
	default:
		return false
	}
}

// signerIdentityKey is the anti-sybil distinct-signer key for consensus: the
// verified (issuer, identity) pair, NOT meta.json's contributor-controlled
// IDHash. Keying consensus on the IDHash would let one verified identity
// submitted under two IDHashes count as two distinct allowlisted signers and
// fabricate a CONFIRMED. The "\n" separator cannot occur in a URL, so distinct
// pairs never collide. This key is internal to consensus and is never
// serialized; display still uses the IDHash.
func signerIdentityKey(s RunMetaSigner) string {
	return s.Issuer + "\n" + s.Identity
}

// canonicalSourceID is the locally-derived storage/index key for a verified
// signer: the FULL hex sha256(issuer\nidentity). The grid, the Sources map, and
// the per-recipe series are all keyed by this value rather than meta.json's
// contributor-controlled IDHash. Because GP4 computes it itself from the verified
// (issuer, identity) pair, two distinct verified identities can never collide on
// one key — which, if the IDHash were trusted, would let a claimed hash overwrite
// another signer's row and silently drop it from the dashboard. The full digest
// (not a truncated prefix) is used so the key space cannot be narrowed into a
// birthday collision either. It is derived from the same identity pair as the
// consensus distinct-signer key (signerIdentityKey), so display and consensus
// stay consistent.
func canonicalSourceID(s RunMetaSigner) string {
	sum := sha256.Sum256([]byte(signerIdentityKey(s)))
	return hex.EncodeToString(sum[:])
}

// formatWhen renders an RFC3339 AttestedAt as "YYYY-MM-DD HH:MM UTC" for the
// grid. It derives the display string from the predicate timestamp, never the
// wall clock; an unparseable value is passed through verbatim.
func formatWhen(attestedAt string) string {
	t, err := time.Parse(time.RFC3339, attestedAt)
	if err != nil {
		return attestedAt
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// formatWhenDate renders an RFC3339 AttestedAt as "YYYY-MM-DD" for series build
// columns.
func formatWhenDate(attestedAt string) string {
	t, err := time.Parse(time.RFC3339, attestedAt)
	if err != nil {
		return attestedAt
	}
	return t.UTC().Format("2006-01-02")
}
