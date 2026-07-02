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

// Command evidence-project is the GP2 ingest step. Given a published,
// signed evidence bundle (OCI ref, pointer file, or unpacked
// directory), it verifies the signature/issuer/identity and the source
// registry BEFORE counting, then synthesizes the source-keyed evidence
// tree (pkg/evidence/project) under -out for upload to the corroboration
// bucket by a separate, credentialed step.
//
// It holds no bucket-write credentials: its only outputs are local
// files. The verification it performs is the same engine the
// `aicr evidence verify` CLI uses, with non-empty issuer + identity pins
// and an explicit trusted-registry allowlist on the OCI reference.
//
// Usage:
//
//	evidence-project -in <oci-ref|pointer.yaml|dir> -out <tree-root> \
//	  --expected-issuer <url> --expected-identity-regexp <re> \
//	  --trusted-registry ghcr.io/nvidia,ghcr.io/nvidia/aicr-evidence \
//	  [--allowlist recipes/evidence/allowlist.yaml] [--run-id <id>]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/evidence/project"
	"github.com/NVIDIA/aicr/pkg/evidence/verifier"
)

// options is the parsed command line.
type options struct {
	in            string
	out           string
	allowlist     string
	issuer        string
	identityRE    string
	trusted       string
	runID         string
	evidenceRef   string
	bundleRef     string
	allowUnpinned bool
	plainHTTP     bool
}

func main() {
	var o options
	flag.StringVar(&o.in, "in", "", "input bundle: OCI ref, pointer .yaml, or unpacked directory (required)")
	flag.StringVar(&o.out, "out", "", "output root for the source-keyed tree (required)")
	flag.StringVar(&o.allowlist, "allowlist", "", "optional signer allowlist (issuer + identityRegexp -> class)")
	flag.StringVar(&o.issuer, "expected-issuer", "", "pin the OIDC issuer URL on the signing cert (required)")
	flag.StringVar(&o.identityRE, "expected-identity-regexp", "", "pin the signer SubjectAlternativeName via regexp (required)")
	flag.StringVar(&o.trusted, "trusted-registry", "", "comma-separated registry/repo prefixes the bundle ref must match")
	flag.StringVar(&o.runID, "run-id", "", "override the run identifier (default: derived from attestedAt)")
	flag.StringVar(&o.evidenceRef, "evidence-ref", "", "override the evidenceRef recorded in meta.json")
	flag.StringVar(&o.bundleRef, "bundle", "", "OCI ref to pull when a pointer carries no bundle.oci")
	flag.BoolVar(&o.allowUnpinned, "allow-unpinned-tag", false, "accept a tag-only OCI ref; skips the trusted-registry gate for unpinned refs only — digest-pinned refs are still gated (debug only)")
	flag.BoolVar(&o.plainHTTP, "plain-http", false, "use HTTP for registry traffic (local-registry tests only)")
	flag.Parse()

	if err := runMain(o); err != nil {
		fmt.Fprintln(os.Stderr, "evidence-project:", err)
		os.Exit(1)
	}
}

// runMain owns the signal-aware, deadline-bounded root context so its
// cleanup defers run before main's os.Exit. A SIGINT/SIGTERM (CI
// cancellation) or the overall timeout cancels the in-flight registry pull
// and verification rather than letting the CLI hang; all downstream I/O
// inherits this context.
func runMain(o options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, defaults.EvidenceIngestTimeout)
	defer cancel()
	return run(ctx, o, os.Stdout)
}

func run(ctx context.Context, o options, stdout io.Writer) error {
	if o.in == "" || o.out == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "-in and -out are required")
	}
	if o.issuer == "" || o.identityRE == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"-expected-issuer and -expected-identity-regexp are required (unpinned verification never counts)")
	}

	form, err := verifier.DetectInputForm(o.in)
	if err != nil {
		return err
	}

	// Load the pointer once up front (pointer input form); it feeds both
	// the registry-gate ref resolution and materialization below, so the
	// file is parsed exactly once.
	var pointer *attestation.Pointer
	if form == verifier.InputFormPointer {
		pointer, err = verifier.LoadAndValidatePointer(o.in)
		if err != nil {
			return err
		}
	}

	// Resolve the OCI reference (if any) and enforce the trusted-registry
	// allowlist before any network pull. A directory input has no remote
	// ref and is exempt.
	ref, err := resolveRef(form, o.in, o.bundleRef, pointer)
	if err != nil {
		return err
	}
	// Enforce the trusted-registry allowlist before any network pull.
	// --allow-unpinned-tag is a debug escape hatch for the unpinned-tag
	// restriction only: it must never relax the registry gate for a
	// digest-pinned ref, or a pinned ref from an arbitrary registry could
	// be pulled unchecked. A genuinely unpinned (tag-only) ref still skips
	// the gate under the flag (local-registry tests).
	if ref != "" {
		pinned := strings.Contains(ref, "@")
		if pinned || !o.allowUnpinned {
			if err = checkTrustedRegistry(ref, parseTrusted(o.trusted)); err != nil {
				return err
			}
		}
	}

	var allowlist *project.Allowlist
	if o.allowlist != "" {
		allowlist, err = project.LoadAllowlist(o.allowlist)
		if err != nil {
			return err
		}
	}

	// Step 1 — materialize once, capturing OCI provenance. The verify
	// step below runs on this unpacked directory so we never pull twice;
	// the digest pin + cross-check were enforced here.
	matOpts := verifier.VerifyOptions{
		Input:            o.in,
		BundleRef:        o.bundleRef,
		AllowUnpinnedTag: o.allowUnpinned,
		PlainHTTP:        o.plainHTTP,
	}
	mat, err := verifier.MaterializeBundle(ctx, matOpts, form, pointer)
	if err != nil {
		return err
	}
	defer mat.Cleanup()

	// Step 2 — verify on the materialized directory with non-empty pins.
	vr, err := verifier.Verify(ctx, verifier.VerifyOptions{
		Input:                  mat.BundleDir,
		ExpectedIssuer:         o.issuer,
		ExpectedIdentityRegexp: o.identityRE,
	})
	if err != nil {
		return err
	}

	evidenceRef := o.evidenceRef
	if evidenceRef == "" {
		evidenceRef = mat.Reference
	}
	if evidenceRef == "" {
		evidenceRef = ref
	}

	return synthesizeVerified(ctx, vr, mat.BundleDir, allowlist, evidenceRef, o.runID, o.out, stdout)
}

// synthesizeVerified records a verified bundle into the source-keyed
// tree: it enforces the verify verdict (fail-closed on an invalid bundle
// or a missing verified signer), classifies the signer from the
// allowlist, and writes the run directory. It is the seam between the
// verifier's result and the synthesis library.
func synthesizeVerified(
	ctx context.Context,
	vr *verifier.VerifyResult,
	bundleDir string,
	allowlist *project.Allowlist,
	evidenceRef, runID, outRoot string,
	stdout io.Writer,
) error {

	if vr == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "nil verify result")
	}
	if vr.Exit == verifier.ExitInvalid {
		return errors.New(errors.ErrCodeInvalidRequest, "bundle verification failed — refusing to ingest (see steps)")
	}
	if vr.Signer == nil || vr.Signer.Identity == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"no verified signer — only signed bundles (attestation.intoto.jsonl) can be ingested")
	}
	if vr.Predicate == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "verification produced no predicate")
	}

	class, allowlisted := allowlist.Classify(vr.Signer.Issuer, vr.Signer.Identity)

	res, err := project.Synthesize(ctx, project.In{
		BundleDir:      bundleDir,
		Predicate:      vr.Predicate,
		SignerIdentity: vr.Signer.Identity,
		SignerIssuer:   vr.Signer.Issuer,
		RekorLogIndex:  vr.Signer.RekorLogIndex,
		Class:          class,
		Allowlisted:    allowlisted,
		EvidenceRef:    evidenceRef,
		RunID:          runID,
		OutRoot:        outRoot,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "ingested %s [%s, allowlisted=%v] -> %s\n",
		res.Coordinate.Path(), class, allowlisted, res.RunDir)
	return nil
}

// resolveRef returns the OCI reference for the input, or "" for a
// directory input (no remote pull). For the OCI form it strips the
// oci:// scheme off in (held on the options); for a pointer it prefers
// the -bundle override, else the already-loaded pointer's bundle.oci.
func resolveRef(form verifier.InputForm, ociInput, bundleOverride string, pointer *attestation.Pointer) (string, error) {
	switch form {
	case verifier.InputFormDir:
		return "", nil
	case verifier.InputFormOCI:
		return strings.TrimPrefix(ociInput, "oci://"), nil
	case verifier.InputFormPointer:
		if bundleOverride != "" {
			return bundleOverride, nil
		}
		if pointer == nil || len(pointer.Attestations) == 0 || pointer.Attestations[0].Bundle.OCI == "" {
			return "", errors.New(errors.ErrCodeInvalidRequest,
				"pointer carries no bundle.oci — pass -bundle <oci-ref>")
		}
		return pointer.Attestations[0].Bundle.OCI, nil
	default:
		return "", errors.New(errors.ErrCodeInvalidRequest, "unknown input form "+string(form))
	}
}

// parseTrusted splits the comma-separated trusted-registry list,
// trimming whitespace and dropping empties.
func parseTrusted(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// checkTrustedRegistry fails closed unless ref's registry/repo matches
// one of the trusted prefixes. An empty allowlist rejects everything —
// the ingest must be told which registries it trusts. Matching is on a
// path-segment boundary so "ghcr.io/nvidia" does not match
// "ghcr.io/nvidia-evil/...".
func checkTrustedRegistry(ref string, trusted []string) error {
	if len(trusted) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"no -trusted-registry configured — refusing to pull "+ref+
				" (every ingested bundle must come from an allowlisted registry)")
	}
	// Compare against the registry/repo portion (drop any :tag / @digest).
	repoPart := ref
	if at := strings.IndexByte(repoPart, '@'); at >= 0 {
		repoPart = repoPart[:at]
	}
	if colon := strings.LastIndexByte(repoPart, ':'); colon > strings.LastIndexByte(repoPart, '/') {
		repoPart = repoPart[:colon]
	}
	for _, t := range trusted {
		if repoPart == t || strings.HasPrefix(repoPart, t+"/") {
			return nil
		}
	}
	return errors.New(errors.ErrCodeInvalidRequest,
		"bundle ref "+ref+" is not under any --trusted-registry prefix")
}
