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
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/trust"
)

// signingConfigPolicy writes transparency-log entries and RFC3161 timestamps
// chosen from a Sigstore SigningConfig, exactly as cosign does. Services are
// picked with root.SelectServices, which prefers the highest supported API
// version, never mixes versions, and honors the config's selector and
// per-service validity windows.
//
// This is how AICR targets Rekor v2: the public-good v2 signing config
// (signing_config_rekor_v2, distributed via TUF) lists both a v2 and a v1 rekor
// service, and SelectServices picks v2 because it is the highest version — the
// v1 entry is only a fallback for clients that support v1 only. Because a Rekor
// v2 entry carries no inline signed timestamp, the config's timestamp authority
// is attached so the bundle still has trusted time; a v2 selection with no TSA
// fails closed rather than emit bundles that cannot be verified for time. See
// #1650.
type signingConfigPolicy struct {
	logs []sign.Transparency
	tsas []*sign.TimestampAuthority
}

// NewSigningConfigPolicyFromPath loads a Sigstore SigningConfig JSON file (e.g.
// the TUF-distributed signing_config_rekor_v2 target written to disk) and
// returns a TransparencyPolicy that signs to the services it selects for the
// current time.
func NewSigningConfigPolicyFromPath(path string) (TransparencyPolicy, error) {
	// Bound the read instead of sigstore-go's bare os.ReadFile: the path is a
	// user-supplied CLI/env argument, so cap it before the bytes hit memory to
	// keep an attacker-influenced path (a /proc symlink, an NFS/FUSE mount) from
	// OOMing the process, mirroring loadPEMPublicKey / the trusted-root loader.
	f, err := os.Open(path) //nolint:gosec // path is an operator-supplied signer input, bounded below
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open signing config", err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, defaults.MaxSigningConfigBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to read signing config", err)
	}
	if int64(len(data)) > defaults.MaxSigningConfigBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "signing config file exceeds size limit")
	}

	sc, err := root.NewSigningConfigFromJSON(data)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to load signing config", err)
	}
	// A user-supplied config file may legitimately be v1, so a v1 selection here
	// is not a downgrade — no warning.
	return newSigningConfigPolicy(sc, time.Now(), false)
}

// NewSigningConfigPolicyFromTUF returns a TransparencyPolicy built from the
// Rekor v2 signing config distributed via TUF — AICR's default signing target.
// The endpoint set is Sigstore-maintained and rotation-safe (shard URLs carry
// validity windows), so nothing is hardcoded and no config file is passed
// around. It prefers the local cache and falls back to a bounded network fetch
// when cold, so signing works without a prior explicit "aicr trust update".
//
// warnOnV1Downgrade is set because this is the v2-intended path: if a stale
// cache selects a v1 service (its v2 shard aged past its validity window before
// the cache refreshed), the caller should see that the entry silently landed in
// Rekor v1 rather than v2.
func NewSigningConfigPolicyFromTUF(ctx context.Context) (TransparencyPolicy, error) {
	sc, err := trust.ResolveSigningConfig(ctx)
	if err != nil {
		return nil, err
	}
	return newSigningConfigPolicy(sc, time.Now(), true)
}

// newSigningConfigPolicy builds the policy from an already-parsed SigningConfig
// at the given time. Split from the public loaders so tests can pin the
// selection time against fixed service validity windows. When warnOnV1Downgrade
// is true (the TUF v2-default path), a v1 selection is logged as a downgrade so
// a stale-cache fall-back to Rekor v1 is visible rather than silent.
func newSigningConfigPolicy(sc *root.SigningConfig, now time.Time, warnOnV1Downgrade bool) (TransparencyPolicy, error) {
	if sc == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "signing config is nil")
	}

	rekorSvcs, tsaSvcs, err := selectSigningServices(sc, now)
	if err != nil {
		return nil, err
	}

	logs := make([]sign.Transparency, 0, len(rekorSvcs))
	usesV2 := false
	for _, svc := range rekorSvcs {
		logs = append(logs, sign.NewRekor(&sign.RekorOptions{
			BaseURL: svc.URL,
			Version: svc.MajorAPIVersion,
		}))
		if svc.MajorAPIVersion >= 2 {
			usesV2 = true
		}
	}

	// A cached TUF config whose v2 shard has aged past its validity window still
	// carries a valid v1 entry, so SelectServices quietly picks v1 — the silent
	// downgrade this signing path otherwise fails closed against. Surface it so
	// the operator knows to run `aicr trust update`.
	if warnOnV1Downgrade && !usesV2 && len(rekorSvcs) > 0 {
		slog.Warn("Rekor v2 signing requested but the signing config selected a Rekor v1 service; "+
			"the cached TUF config may be stale (its v2 shard is past its validity window) — run 'aicr trust update' to refresh",
			"selectedRekorURL", rekorSvcs[0].URL,
			"selectedRekorAPIVersion", rekorSvcs[0].MajorAPIVersion)
	}

	tsas := make([]*sign.TimestampAuthority, 0, len(tsaSvcs))
	for _, svc := range tsaSvcs {
		tsas = append(tsas, sign.NewTimestampAuthority(&sign.TimestampAuthorityOptions{URL: svc.URL}))
	}

	// A Rekor v2 entry has no inline signed timestamp, so a v2 target without a
	// timestamp authority would produce bundles with no trusted time — fail
	// closed instead of shipping unverifiable attestations.
	if usesV2 && len(tsas) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"signing config selects Rekor v2 but has no timestamp authority; bundles would lack trusted time")
	}

	return signingConfigPolicy{logs: logs, tsas: tsas}, nil
}

// selectSigningServices resolves the rekor and timestamp-authority services to
// sign to from a SigningConfig at now, applying each section's selector,
// validity windows, and the supported API versions. A config with no timestamp
// authorities returns an empty tsa slice (valid for Rekor v1); the v2-requires-
// TSA rule is enforced by the caller.
func selectSigningServices(sc *root.SigningConfig, now time.Time) (rekor, tsa []root.Service, err error) {
	rekor, err = root.SelectServices(sc.RekorLogURLs(), sc.RekorLogURLsConfig(), sign.RekorAPIVersions, now)
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInvalidRequest, "no usable rekor service in signing config", err)
	}

	if len(sc.TimestampAuthorityURLs()) == 0 {
		return rekor, nil, nil
	}
	// TSA selection is best-effort. A config may list timestamp authorities that
	// are all outside their validity window, which is harmless for a Rekor v1
	// selection: v1 carries its own signed entry timestamp and uses no TSA.
	// Whether a TSA is *required* is version-dependent and enforced by the caller
	// (newSigningConfigPolicy fails closed when a v2 log has no TSA), so a
	// no-selectable-TSA result here is an empty set, not an error.
	tsa, err = root.SelectServices(sc.TimestampAuthorityURLs(), sc.TimestampAuthorityURLsConfig(), sign.TimestampAuthorityAPIVersions, now)
	if err != nil {
		return rekor, nil, nil
	}
	return rekor, tsa, nil
}

func (p signingConfigPolicy) Logs() []sign.Transparency { return p.logs }

func (p signingConfigPolicy) TimestampAuthorities() []*sign.TimestampAuthority { return p.tsas }
