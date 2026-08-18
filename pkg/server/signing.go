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

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// kmsURISchemes are the cosign KMS URI prefixes accepted for Mode A signing.
var kmsURISchemes = []string{"awskms://", "gcpkms://", "azurekms://", "hashivault://"}

// signingConfig holds the validated server signing identity parsed from the
// environment. The server signs as itself: no field here ever comes from a
// request. enabled reports whether any signing mode is configured; when false
// an attest=true request is rejected. keyless reports Mode B (token file read
// fresh per request in resolveOptions).
type signingConfig struct {
	enabled bool
	keyless bool

	signingKey        string // Mode A KMS URI
	fulcioURL         string // Mode B
	rekorURL          string // both
	identityTokenFile string // Mode B token source
	ambientURL        string // Mode B GHA ambient (optional)
	ambientToken      string
	tlogUpload        bool // Mode A only; default true
	signingConfigPath string

	// binaryAttestation is the server's own pre-verified binary attestation
	// (tool provenance), loaded and cryptographically verified ONCE at startup
	// and embedded into every attested bundle. Populated by loadBinaryAttestation
	// when signing is enabled.
	binaryAttestation []byte
}

// parseSigningConfig reads the AICR_* signing env vars, validates mode
// exclusivity and completeness, and fails fast on ambiguous or malformed
// configuration so a misconfigured server does not start.
func parseSigningConfig() (*signingConfig, error) {
	cfg := &signingConfig{
		signingKey:        os.Getenv(defaults.EnvSigningKey),
		fulcioURL:         os.Getenv(defaults.EnvFulcioURL),
		rekorURL:          os.Getenv(defaults.EnvRekorURL),
		identityTokenFile: os.Getenv(defaults.EnvIdentityTokenFile),
		ambientURL:        os.Getenv(defaults.EnvGitHubActionsIDTokenRequestURL),
		ambientToken:      os.Getenv(defaults.EnvGitHubActionsIDTokenRequestToken),
		signingConfigPath: os.Getenv(defaults.EnvSigningConfigPath),
		// tlogUpload is KMS-only; parsed in the KMS branch below so a stray
		// malformed AICR_TLOG_UPLOAD does not fail startup for keyless or
		// unconfigured servers where it has no effect.
	}

	hasKMS := cfg.signingKey != ""
	hasKeyless := cfg.fulcioURL != "" || cfg.identityTokenFile != ""

	switch {
	case !hasKMS && !hasKeyless:
		return cfg, nil // capability off; server starts unsigned
	case hasKMS && hasKeyless:
		return nil, aicrerrors.New(aicrerrors.ErrCodeInternal,
			"ambiguous signing identity: set either "+defaults.EnvSigningKey+
				" (KMS) or "+defaults.EnvFulcioURL+"/"+defaults.EnvIdentityTokenFile+
				" (keyless), not both")
	case hasKMS:
		if !hasValidKMSScheme(cfg.signingKey) {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInternal,
				"invalid "+defaults.EnvSigningKey+": must be a cosign KMS URI "+
					"(awskms:// | gcpkms:// | azurekms:// | hashivault://)")
		}
		tlogUpload, err := parseTLogUpload()
		if err != nil {
			return nil, err
		}
		cfg.tlogUpload = tlogUpload
		cfg.enabled = true
	default: // keyless
		if cfg.fulcioURL == "" {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInternal,
				"keyless signing requires "+defaults.EnvFulcioURL)
		}
		if cfg.identityTokenFile == "" && (cfg.ambientURL == "" || cfg.ambientToken == "") {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInternal,
				"keyless signing requires a token source: "+defaults.EnvIdentityTokenFile+
					" or GitHub Actions ambient OIDC env")
		}
		cfg.enabled = true
		cfg.keyless = true
	}
	return cfg, nil
}

// parseTLogUpload interprets AICR_TLOG_UPLOAD (Mode A Rekor upload toggle).
// Unset or empty defaults to true; otherwise the value is parsed with
// strconv.ParseBool (accepts 1/t/T/TRUE/true/True and 0/f/F/FALSE/false/False).
// A non-empty, unparseable value is a server-side operator/config fault and
// fails startup fast rather than silently defaulting.
func parseTLogUpload() (bool, error) {
	v := os.Getenv(defaults.EnvTLogUpload)
	if v == "" {
		return true, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, aicrerrors.New(aicrerrors.ErrCodeInternal,
			"invalid "+defaults.EnvTLogUpload+": must be a boolean "+
				"(true/false), got "+strconv.Quote(v))
	}
	return b, nil
}

// hasValidKMSScheme reports whether uri carries one of the accepted cosign KMS
// URI prefixes.
func hasValidKMSScheme(uri string) bool {
	for _, s := range kmsURISchemes {
		if strings.HasPrefix(uri, s) {
			return true
		}
	}
	return false
}

// resolveOptions builds a per-request attestation.ResolveOptions with Attest
// set. For keyless it reads the identity-token file FRESH each call: SA tokens
// rotate and Fulcio binds the cert to a fresh token, so the attester must be
// rebuilt per request rather than cached.
func (c *signingConfig) resolveOptions() (attestation.ResolveOptions, error) {
	o := attestation.ResolveOptions{
		Attest:            true,
		RekorURL:          c.rekorURL,
		SigningConfigPath: c.signingConfigPath,
	}
	if c.keyless {
		o.FulcioURL = c.fulcioURL
		o.AmbientURL = c.ambientURL
		o.AmbientToken = c.ambientToken
		if c.identityTokenFile != "" {
			tok, err := os.ReadFile(c.identityTokenFile) //nolint:gosec // operator-configured path
			if err != nil {
				return attestation.ResolveOptions{}, aicrerrors.Wrap(aicrerrors.ErrCodeInternal,
					"failed to read identity token file", err)
			}
			o.IdentityToken = strings.TrimSpace(string(tok))
		}
		return o, nil
	}
	o.SigningKey = c.signingKey
	o.DisableTLogUpload = !c.tlogUpload
	return o, nil
}

// binaryAttestationVerifier resolves and cryptographically verifies the running
// server binary's attestation (tool provenance), returning the raw Sigstore
// bundle bytes to embed in signed bundles. Injectable so tests can supply a
// fixture: the real verification pins NVIDIA-CI identity + the running binary's
// digest, which cannot be satisfied by a `go test` executable.
type binaryAttestationVerifier func(ctx context.Context) ([]byte, error)

// resolveBinaryAttestationPath returns the path to the server's binary
// attestation, in precedence order: the AICR_BINARY_ATTESTATION_FILE operator
// override, then ko's per-architecture attestation in KO_DATA_PATH (auto-set
// inside ko-built images), then the conventional file next to the running
// executable (direct-binary / CLI-style deployments).
func resolveBinaryAttestationPath(binPath string) (string, error) {
	// 1. Explicit operator override.
	if p := os.Getenv(defaults.EnvBinaryAttestationFile); p != "" {
		return p, nil
	}
	// 2. ko image: per-architecture attestation shipped in KO_DATA_PATH. ko sets
	// this env automatically inside the image, so no deployment config is needed.
	if koData := os.Getenv(defaults.EnvKoDataPath); koData != "" {
		name := fmt.Sprintf(defaults.BinaryAttestationKoDataNameFormat, runtime.GOARCH)
		return filepath.Join(koData, name), nil
	}
	// 3. Conventional file next to the running binary (direct-binary / CLI-style).
	return attestation.FindBinaryAttestation(binPath)
}

// resolveBinaryAttestationIdentityPattern returns the certificate-identity
// pattern used to verify the server's own binary attestation: the
// AICR_BINARY_ATTESTATION_IDENTITY_REGEXP override when set (validated to stay
// pinned to the NVIDIA org), else the release-workflow default. A custom pattern
// means bundles this server signs will not pass a verifier using the default
// identity, so it is logged.
func resolveBinaryAttestationIdentityPattern() (string, error) {
	p := os.Getenv(defaults.EnvBinaryAttestationIdentityRegexp)
	if p == "" {
		return aicr.TrustedIdentityPattern, nil
	}
	if err := aicr.ValidateIdentityPattern(p); err != nil {
		return "", err // already coded (ErrCodeInvalidRequest)
	}
	slog.Warn("using custom binary-attestation identity pattern; bundles this server signs "+
		"will not pass verification under the default identity", "pattern", p)
	return p, nil
}

// defaultBinaryAttestationVerifier locates the attestation next to the running
// executable (the in-image convention), verifies it against the NVIDIA-CI
// identity pattern and the binary's own digest, and returns its bytes.
func defaultBinaryAttestationVerifier(ctx context.Context) ([]byte, error) {
	binPath, err := os.Executable()
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "could not resolve executable path", err)
	}
	attestPath, err := resolveBinaryAttestationPath(binPath)
	if err != nil {
		return nil, err // already coded (ErrCodeNotFound when absent)
	}
	identityPattern, err := resolveBinaryAttestationIdentityPattern()
	if err != nil {
		return nil, err // already coded (ErrCodeInvalidRequest); a bad override fails startup fast
	}
	digest, err := checksum.SHA256RawContext(ctx, binPath)
	if err != nil {
		return nil, aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal, "failed to compute binary digest")
	}
	// Read the attestation once (bounded: os.Open + LimitReader so an oversized
	// or symlink-swapped path cannot allocate the whole file before the size
	// check), then verify the EXACT bytes we return. Verifying the in-hand bytes
	// (rather than re-reading after a path-based verify) removes any
	// verify-then-reread window.
	f, err := os.Open(attestPath) //nolint:gosec // in-image, operator-trusted path already resolved by resolveBinaryAttestationPath
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to open binary attestation", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, defaults.MaxAttestationFileBytes+1))
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read binary attestation", err)
	}
	if int64(len(data)) > defaults.MaxAttestationFileBytes {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "binary attestation exceeds size limit")
	}
	if _, verifyErr := aicr.VerifyBinaryAttestation(ctx, aicr.BinaryAttestationVerifyOptions{
		Attestation:    data,
		BinaryDigest:   digest,
		IdentityRegexp: identityPattern,
	}); verifyErr != nil {
		return nil, verifyErr // already coded
	}
	return data, nil
}

// loadBinaryAttestation verifies and caches the server's binary attestation onto
// the signingConfig. Fail-fast: an enabled signing server that cannot prove its
// own provenance must not start. No-op when signing is disabled.
func (c *signingConfig) loadBinaryAttestation(ctx context.Context, verify binaryAttestationVerifier) error {
	if !c.enabled {
		return nil
	}
	data, err := verify(ctx)
	if err != nil {
		// PropagateOrWrap preserves an already-coded verify error (e.g.
		// ErrCodeNotFound when the attestation is absent) instead of
		// reclassifying it to ErrCodeInternal.
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal, "server signing enabled but binary attestation verification failed")
	}
	c.binaryAttestation = data
	return nil
}
