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

package attestation

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/sigstore/sigstore-go/pkg/root"
)

// publicGoodDomain is the DNS suffix every public-good Sigstore service lives
// under.
//
// The check is on the domain rather than a list of exact URLs on purpose. The
// public-good Rekor shards carry the year in their hostname
// (log2025-1.rekor.sigstore.dev), so an exact-URL allowlist would pass today
// and start rejecting legitimate release signing at the next rotation — a gate
// that breaks the release path on a calendar boundary is worse than no gate.
// The operator's domain is the stable identity; the shard names are not.
const publicGoodDomain = "sigstore.dev"

// endpointKey names the offending URL in the error's structured context so a
// caller can report which endpoint to change.
const endpointKey = "endpoint"

// ValidateSigningConfigIsPublicGood reports whether a Sigstore signing config
// targets only public-good services.
//
// This exists because catalog signing and catalog verification must agree.
// VerifyCatalog verifies against the public-good Sigstore root, requires a
// GitHub Actions OIDC issuer, and requires a transparency-log entry. A signing
// config naming a private Fulcio or Rekor produces a catalog that fails all
// three — signing appears to succeed and the artifact is unverifiable by the
// documented counterpart.
//
// It FAILS CLOSED, matching the four sibling rejections in the facade and the
// project's allowlist guidance: an endpoint outside the known-good domain is
// rejected rather than warned about. The alternative — warn and sign anyway —
// produces exactly the artifact this check exists to prevent, and the operator
// discovers it at verification time instead of signing time.
//
// A nil config is accepted: "no config supplied" is the default path, not a
// departure from it.
func ValidateSigningConfigIsPublicGood(sc *root.SigningConfig) error {
	if sc == nil {
		return nil
	}

	groups := []struct {
		label    string
		services []root.Service
	}{
		{"certificate authority", sc.FulcioCertificateAuthorityURLs()},
		{"transparency log", sc.RekorLogURLs()},
		{"OIDC provider", sc.OIDCProviderURLs()},
		{"timestamp authority", sc.TimestampAuthorityURLs()},
	}

	for _, group := range groups {
		for _, service := range group.services {
			if err := requirePublicGoodURL(group.label, service.URL); err != nil {
				return err
			}
		}
	}
	return nil
}

// requirePublicGoodURL rejects a single endpoint outside the public-good domain.
func requirePublicGoodURL(label, raw string) error {
	if raw == "" {
		return nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.WrapWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("signing config %s URL is not parseable", label), err,
			map[string]any{endpointKey: raw})
	}

	// Scheme before host. The public-good services are HTTPS-only, and these
	// URLs are handed to sign.NewRekor / sign.NewTimestampAuthority as-is, so
	// an "http://rekor.sigstore.dev" would pass a host-only check while sending
	// signing traffic in the clear — a downgrade wearing the right hostname.
	if !strings.EqualFold(parsed.Scheme, "https") {
		return errors.NewWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("signing config %s URL is not HTTPS (%s); the public-good "+
				"Sigstore services are HTTPS-only", label, raw),
			map[string]any{endpointKey: raw, "scheme": parsed.Scheme})
	}

	// Lowercased: DNS names are case-insensitive, so "FULCIO.SIGSTORE.DEV" is
	// the public-good host. url.Hostname() does not normalize case, and a
	// case-sensitive comparison would reject a legitimate config.
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return errors.NewWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("signing config %s URL has no host", label),
			map[string]any{endpointKey: raw})
	}

	// Suffix match on a label boundary. A bare strings.HasSuffix would accept
	// "evilsigstore.dev", which is the whole point of checking.
	if host != publicGoodDomain && !strings.HasSuffix(host, "."+publicGoodDomain) {
		return errors.NewWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("signing config names a %s outside the public-good Sigstore "+
				"instance (%s); a catalog signed against it cannot be verified by "+
				"VerifyCatalog, which checks the public-good root", label, raw),
			map[string]any{endpointKey: raw, "expectedDomain": publicGoodDomain})
	}
	return nil
}

// LoadSigningConfigForValidation reads a signing config from disk using the same
// bounded read as the signing path.
//
// Callers that only need to validate a config should use this rather than
// os.ReadFile: the path is operator-supplied, so the size cap that protects the
// signing path has to protect the validation path too, or the guard becomes a
// way to OOM the process before signing ever runs.
func LoadSigningConfigForValidation(path string) (*root.SigningConfig, error) {
	data, err := readSigningConfigBytes(path)
	if err != nil {
		return nil, err
	}

	sc, err := root.NewSigningConfigFromJSON(data)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to load signing config", err)
	}
	return sc, nil
}
