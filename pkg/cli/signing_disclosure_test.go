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
	"bytes"
	"os"
	"strings"
	"testing"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/defaults"
)

func TestKeylessOIDCPathIsInteractive(t *testing.T) {
	tests := []struct {
		name string
		opts bundleattest.ResolveOptions
		want bool
	}{
		{
			name: "pre-fetched identity token is non-interactive",
			opts: bundleattest.ResolveOptions{IdentityToken: "eyJ..."},
			want: false,
		},
		{
			name: "ambient github actions OIDC is non-interactive",
			opts: bundleattest.ResolveOptions{AmbientURL: "https://token", AmbientToken: "tok"},
			want: false,
		},
		{
			name: "ambient with only URL falls through to interactive",
			opts: bundleattest.ResolveOptions{AmbientURL: "https://token"},
			want: true,
		},
		{
			name: "KMS signing key is non-interactive (no OIDC identity in cert)",
			opts: bundleattest.ResolveOptions{SigningKey: "awskms://key"},
			want: false,
		},
		{
			name: "device-code flow is interactive",
			opts: bundleattest.ResolveOptions{DeviceFlow: true},
			want: true,
		},
		{
			name: "empty options default to interactive browser flow",
			opts: bundleattest.ResolveOptions{},
			want: true,
		},
		{
			name: "identity token wins over device flow",
			opts: bundleattest.ResolveOptions{IdentityToken: "eyJ...", DeviceFlow: true},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keylessOIDCPathIsInteractive(tt.opts); got != tt.want {
				t.Errorf("keylessOIDCPathIsInteractive() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildKeylessSigningBanner(t *testing.T) {
	t.Run("public-good Sigstore names the permanent public log", func(t *testing.T) {
		banner := buildKeylessSigningBanner(bundleattest.ResolveOptions{})
		// Endpoint URLs are matched verbatim; the consent keywords are
		// matched case-insensitively since the banner emphasizes some in
		// caps (PERMANENT) for readability.
		for _, want := range []string{defaults.SigstoreRekorURL, defaults.SigstoreFulcioURL} {
			if !strings.Contains(banner, want) {
				t.Errorf("public banner missing %q\nbanner:\n%s", want, banner)
			}
		}
		lower := strings.ToLower(banner)
		for _, want := range []string{"identity", "public", "permanent", "email"} {
			if !strings.Contains(lower, want) {
				t.Errorf("public banner missing concept %q\nbanner:\n%s", want, banner)
			}
		}
	})

	t.Run("private Sigstore names the configured endpoints", func(t *testing.T) {
		banner := buildKeylessSigningBanner(bundleattest.ResolveOptions{
			FulcioURL: "https://fulcio.internal.example.com",
			RekorURL:  "https://rekor.internal.example.com",
		})

		if !strings.Contains(banner, "https://rekor.internal.example.com") {
			t.Errorf("private banner missing custom rekor URL\nbanner:\n%s", banner)
		}
		if strings.Contains(banner, defaults.SigstoreRekorURL) {
			t.Errorf("private banner should not name the public-good Rekor\nbanner:\n%s", banner)
		}
	})
}

func TestConfirmKeylessSigningDisclosure_TTYConfirmation(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantErr   bool
		wantInOut string
	}{
		{"yes lower proceeds", "y\n", false, "rekor.sigstore.dev"},
		{"yes word proceeds", "yes\n", false, "rekor.sigstore.dev"},
		{"yes uppercase proceeds", "Y\n", false, "rekor.sigstore.dev"},
		{"no declines", "n\n", true, "rekor.sigstore.dev"},
		{"empty defaults to decline", "\n", true, "rekor.sigstore.dev"},
		{"eof defaults to decline", "", true, "rekor.sigstore.dev"},
		{"unrecognized declines", "maybe\n", true, "rekor.sigstore.dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			// strings.Reader is not an *os.File, so it bypasses the TTY
			// check and is read directly (simulates an interactive shell).
			err := confirmKeylessSigningDisclosure(
				bundleattest.ResolveOptions{}, false, strings.NewReader(tt.input), &out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("confirmKeylessSigningDisclosure() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !strings.Contains(out.String(), tt.wantInOut) {
				t.Errorf("banner missing %q from output:\n%s", tt.wantInOut, out.String())
			}
			if !strings.Contains(out.String(), "[y/N]") {
				t.Errorf("TTY prompt missing from output:\n%s", out.String())
			}
		})
	}
}

func TestConfirmKeylessSigningDisclosure_NonInteractiveTokenSkipsGate(t *testing.T) {
	nonInteractive := []struct {
		name string
		opts bundleattest.ResolveOptions
	}{
		{"identity token", bundleattest.ResolveOptions{IdentityToken: "eyJ..."}},
		{"ambient github actions", bundleattest.ResolveOptions{AmbientURL: "https://t", AmbientToken: "tok"}},
		{"KMS signing key", bundleattest.ResolveOptions{SigningKey: "gcpkms://key"}},
	}

	for _, tt := range nonInteractive {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			// An input that *would* decline if the gate were consulted.
			err := confirmKeylessSigningDisclosure(tt.opts, false, strings.NewReader("n\n"), &out)
			if err != nil {
				t.Fatalf("non-interactive source should not gate, got error: %v", err)
			}
			if out.String() != "" {
				t.Errorf("non-interactive source should emit no banner, got:\n%s", out.String())
			}
		})
	}
}

func TestConfirmKeylessSigningDisclosure_NonTTYProceeds(t *testing.T) {
	// /dev/null is a non-terminal *os.File, simulating piped/redirected
	// stdin in CI. The gate must emit the banner and proceed (return nil)
	// without reading a confirmation.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("failed to open /dev/null: %v", err)
	}
	defer func() { _ = f.Close() }()

	var out bytes.Buffer
	if err := confirmKeylessSigningDisclosure(bundleattest.ResolveOptions{}, false, f, &out); err != nil {
		t.Fatalf("non-TTY stdin should proceed, got error: %v", err)
	}
	if !strings.Contains(out.String(), defaults.SigstoreRekorURL) {
		t.Errorf("non-TTY banner missing, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("non-TTY path must not prompt, got:\n%s", out.String())
	}
}

func TestConfirmKeylessSigningDisclosure_AssumeYesBypass(t *testing.T) {
	var out bytes.Buffer
	// Input would decline, but --yes must bypass the prompt entirely.
	err := confirmKeylessSigningDisclosure(
		bundleattest.ResolveOptions{}, true, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatalf("--yes should bypass the prompt, got error: %v", err)
	}
	if !strings.Contains(out.String(), defaults.SigstoreRekorURL) {
		t.Errorf("--yes path should still emit the banner, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("--yes path must not prompt, got:\n%s", out.String())
	}
}

// TestConfirmKeylessSigningDisclosure_DeviceFlowGated proves the device-code
// path is gated identically to the default browser path: an interactive
// decline aborts before any login.
func TestConfirmKeylessSigningDisclosure_DeviceFlowGated(t *testing.T) {
	var out bytes.Buffer
	err := confirmKeylessSigningDisclosure(
		bundleattest.ResolveOptions{DeviceFlow: true}, false, strings.NewReader("n\n"), &out)
	if err == nil {
		t.Fatal("device-flow decline should abort, got nil error")
	}
	if !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("device-flow path should prompt, got:\n%s", out.String())
	}
}

// TestConfirmKeylessSigningDisclosure_PrivateEndpointGated exercises the gate
// over a private Sigstore instance: the banner names the private endpoint
// (not the public-good Rekor) and the interactive decision is still honored.
func TestConfirmKeylessSigningDisclosure_PrivateEndpointGated(t *testing.T) {
	opts := bundleattest.ResolveOptions{
		FulcioURL: "https://fulcio.internal.example.com",
		RekorURL:  "https://rekor.internal.example.com",
	}

	t.Run("accept proceeds", func(t *testing.T) {
		var out bytes.Buffer
		if err := confirmKeylessSigningDisclosure(opts, false, strings.NewReader("y\n"), &out); err != nil {
			t.Fatalf("private-endpoint accept should proceed, got error: %v", err)
		}
		if !strings.Contains(out.String(), "rekor.internal.example.com") {
			t.Errorf("banner should name the private rekor endpoint, got:\n%s", out.String())
		}
		if strings.Contains(out.String(), defaults.SigstoreRekorURL) {
			t.Errorf("private-endpoint banner must not name the public-good Rekor, got:\n%s", out.String())
		}
	})

	t.Run("decline aborts", func(t *testing.T) {
		var out bytes.Buffer
		if err := confirmKeylessSigningDisclosure(opts, false, strings.NewReader("n\n"), &out); err == nil {
			t.Fatal("private-endpoint decline should abort, got nil error")
		}
	})
}
