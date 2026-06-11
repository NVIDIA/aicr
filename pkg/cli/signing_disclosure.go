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

package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// assumeYesFlag builds the shared --yes / --assume-yes bypass flag for the
// keyless-signing identity-disclosure prompt. category groups it alongside
// the owning command's other signing flags (Evidence for validate/publish,
// Deployment for bundle).
func assumeYesFlag(category string) cli.Flag {
	return &cli.BoolFlag{
		Name:    flagAssumeYes,
		Aliases: []string{"assume-yes"},
		Usage: "Skip the interactive confirmation prompt shown before keyless signing publishes " +
			"your OIDC identity. The disclosure banner is still printed. For trusted interactive " +
			"automation; CI / non-TTY runs already proceed without prompting.",
		Sources:  cli.EnvVars("AICR_ASSUME_YES"),
		Category: category,
	}
}

// keylessOIDCPathIsInteractive reports whether signing with opts would fall
// through to an interactive login (browser callback or device-code) that
// mints a Fulcio certificate from the signer's personal identity.
//
// The OIDC source-precedence decision is delegated to
// bundleattest.SelectOIDCSource — the same classifier ResolveOIDCToken
// switches on — so the gate cannot drift from the resolver: a new
// non-interactive source added there is reflected here automatically. The
// only thing the gate adds is the KMS short-circuit: a SigningKey selects
// key-based signing in ResolveAttester before any OIDC source applies, and
// embeds no OIDC identity in the artifact.
func keylessOIDCPathIsInteractive(opts bundleattest.ResolveOptions) bool {
	if opts.SigningKey != "" {
		return false
	}
	return bundleattest.SelectOIDCSource(opts).Interactive()
}

// buildKeylessSigningBanner renders the privacy disclosure shown before an
// interactive keyless-signing login. It is endpoint-aware: it names the
// Fulcio and Rekor URLs actually in effect (the configured private instance
// or the public-good defaults) and calls out the irreversible, world-readable
// consequence only when the public-good Rekor is the destination.
func buildKeylessSigningBanner(opts bundleattest.ResolveOptions) string {
	fulcioURL := opts.FulcioURL
	if fulcioURL == "" {
		fulcioURL = defaults.SigstoreFulcioURL
	}
	rekorURL := opts.RekorURL
	if rekorURL == "" {
		rekorURL = defaults.SigstoreRekorURL
	}
	publicRekor := rekorURL == defaults.SigstoreRekorURL

	var b strings.Builder
	b.WriteString("─────────────────────────────────────────────────────────────────────\n")
	b.WriteString("  Keyless signing — identity disclosure\n")
	b.WriteString("─────────────────────────────────────────────────────────────────────\n")
	b.WriteString("Interactive keyless signing will open a browser/device-code login and\n")
	b.WriteString("exchange your OIDC identity for a short-lived signing certificate at:\n")
	fmt.Fprintf(&b, "  Fulcio (CA):   %s\n", fulcioURL)
	fmt.Fprintf(&b, "  Rekor (log):   %s\n", rekorURL)
	b.WriteString("\n")
	b.WriteString("The certificate embeds your authenticated identity (your email address\n")
	b.WriteString("and OIDC issuer). That identity is then:\n")
	fmt.Fprintf(&b, "  • recorded in the Rekor transparency log (%s), and\n", rekorURL)
	b.WriteString("  • attached to the pushed OCI artifact, visible to anyone who can pull it.\n")
	b.WriteString("\n")
	if publicRekor {
		b.WriteString("This is the PUBLIC-GOOD Sigstore log: the entry is public, append-only,\n")
		b.WriteString("and PERMANENT — it cannot be deleted, and your email becomes globally\n")
		b.WriteString("searchable. To avoid publishing a personal identity, use a CI/service\n")
		b.WriteString("ambient identity, supply a pre-fetched --identity-token from a\n")
		b.WriteString("non-personal identity, or point --fulcio-url/--rekor-url at a private\n")
		b.WriteString("Sigstore instance.\n")
	} else {
		b.WriteString("These are PRIVATE Sigstore endpoints, so the identity stays inside your\n")
		b.WriteString("organization's infrastructure rather than a public log. The identity is\n")
		b.WriteString("still recorded permanently in that log and attached to the artifact.\n")
	}
	b.WriteString("─────────────────────────────────────────────────────────────────────")
	return b.String()
}

// confirmKeylessSigningDisclosure gates an interactive keyless-signing login
// behind an informed-consent banner. It lives in the CLI layer so the
// business-logic packages stay non-interactive.
//
// When the resolved OIDC path is non-interactive (pre-fetched token, ambient
// OIDC, or KMS key — see keylessOIDCPathIsInteractive) it returns nil with no
// output: those paths neither open a browser nor surprise the user.
//
// For an interactive path it always emits the disclosure banner to out, then:
//   - assumeYes (--yes / AICR_ASSUME_YES): prints a notice and proceeds.
//   - non-TTY stdin (CI, pipes): proceeds without blocking — a fail-safe so
//     scripted/CI signing is never wedged waiting on input that won't arrive.
//   - TTY stdin: pauses for an explicit y/N confirmation, defaulting to no.
//     A declined or empty answer returns an error so the caller aborts before
//     any browser opens.
//
// in is treated as a TTY unless it is a non-terminal *os.File (the pattern
// used by confirmOverwrite); test readers such as strings.Reader exercise the
// interactive branch directly.
func confirmKeylessSigningDisclosure(opts bundleattest.ResolveOptions, assumeYes bool, in io.Reader, out io.Writer) error {
	if !keylessOIDCPathIsInteractive(opts) {
		return nil
	}

	fmt.Fprintln(out, buildKeylessSigningBanner(opts))

	if assumeYes {
		fmt.Fprintln(out, "Proceeding without confirmation (--yes / AICR_ASSUME_YES set).")
		return nil
	}

	if !isInteractiveStream(in) {
		fmt.Fprintln(out, "Non-interactive stdin detected; emitting disclosure and proceeding.")
		return nil
	}

	fmt.Fprint(out, "\nProceed with interactive keyless signing and publish this identity? [y/N]: ")

	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to read signing confirmation", err)
		}
		return errSigningDeclined()
	}
	switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
	case "y", "yes":
		return nil
	default:
		return errSigningDeclined()
	}
}

// errSigningDeclined is the clean-abort error returned when the user declines
// the interactive keyless-signing prompt. ErrCodeUnavailable matches the
// classification the OIDC helpers use for a caller-side cancel, so a declined
// prompt and a canceled login surface the same way to the CLI exit-code mapper.
func errSigningDeclined() error {
	return errors.New(errors.ErrCodeUnavailable,
		"keyless signing declined at the identity-disclosure prompt; no identity was published "+
			"(re-run with --yes / AICR_ASSUME_YES to skip this prompt, or supply --identity-token)")
}

// isInteractiveStream reports whether in is an interactive terminal. A
// non-terminal *os.File (pipe, redirect, /dev/null) is non-interactive;
// other reader types (e.g. a strings.Reader in tests) bypass the TTY check
// and are treated as interactive so the confirmation branch is exercisable.
func isInteractiveStream(in io.Reader) bool {
	if f, ok := in.(*os.File); ok {
		return term.IsTerminal(int(f.Fd())) //nolint:gosec // G115: os file descriptors fit safely in int
	}
	return true
}
