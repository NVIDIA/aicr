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
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/urfave/cli/v3"
)

// TestParseOutputTarget is now in pkg/oci/reference_test.go
// The oci.ParseOutputTarget function handles OCI URI parsing.
// ParseValueOverrides is tested in pkg/bundler/config/config_test.go.

func TestBundleCmd(t *testing.T) {
	cmd := bundleCmd()

	// Verify command configuration
	if cmd.Name != "bundle" {
		t.Errorf("expected command name 'bundle', got %q", cmd.Name)
	}

	// Verify required flags exist
	flagNames := make(map[string]bool)
	for _, flag := range cmd.Flags {
		names := flag.Names()
		for _, name := range names {
			flagNames[name] = true
		}
	}

	// Required flags for the new URI-based output approach
	requiredFlags := []string{"recipe", "r", "output", "o", "set", "plain-http", "insecure-tls"}
	for _, flag := range requiredFlags {
		if !flagNames[flag] {
			t.Errorf("expected flag %q to be defined", flag)
		}
	}

	// Verify node selector/toleration and scheduling flags exist
	nodeFlags := []string{
		"system-node-selector",
		"system-node-toleration",
		"accelerated-node-selector",
		"accelerated-node-toleration",
		"nodes",
	}
	for _, flag := range nodeFlags {
		if !flagNames[flag] {
			t.Errorf("expected flag %q to be defined", flag)
		}
	}

	// Verify attestation flag exists
	if !flagNames["attest"] {
		t.Error("expected flag 'attest' to be defined")
	}

	// Verify removed flags don't exist (replaced by oci:// URI in --output)
	removedFlags := []string{"output-format", "registry", "repository", "tag", "push", "F"}
	for _, flag := range removedFlags {
		if flagNames[flag] {
			t.Errorf("flag %q should have been removed (use --output oci://... instead)", flag)
		}
	}

	// Verify headless-OIDC flags are wired
	for _, flag := range []string{"identity-token", "oidc-device-flow"} {
		if !flagNames[flag] {
			t.Errorf("expected flag %q to be defined", flag)
		}
	}
}

// TestSelectAttester_WiresEnvAndFlags is a thin smoke test for the CLI shim
// over attestation.ResolveAttesterLazy. The OIDC source-precedence logic
// itself is exhaustively covered in the attestation package's
// resolver_test.go; here we only verify that selectAttester forwards CLI
// flags and the two ACTIONS_ID_TOKEN_REQUEST_* env vars into the
// resolver correctly, and that --attest=true produces the lazy attester
// (so the OIDC token is resolved at first Attest() rather than at
// bundler construction).
func TestSelectAttester_WiresEnvAndFlags(t *testing.T) {
	// Disabled path: shim must short-circuit without inspecting env or flags.
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.invalid")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "x")

	att, err := selectAttester(context.Background(), &bundleCmdOptions{attest: false})
	if err != nil {
		t.Fatalf("selectAttester returned error: %v", err)
	}
	if _, ok := att.(*attestation.NoOpAttester); !ok {
		t.Fatalf("expected *NoOpAttester when attest=false, got %T", att)
	}

	// Identity-token flag: the shim must pass identityToken through; the
	// lazy resolver returns a *LazyKeylessAttester synchronously without
	// any network call.
	att, err = selectAttester(context.Background(), &bundleCmdOptions{
		attest:        true,
		identityToken: "pre-fetched-token",
	})
	if err != nil {
		t.Fatalf("selectAttester returned error: %v", err)
	}
	if _, ok := att.(*attestation.LazyKeylessAttester); !ok {
		t.Errorf("expected *LazyKeylessAttester (deferred token resolution), got %T", att)
	}
}

// TestParseBundleCmdOptions_OCIRepoURLDerivation verifies the auto-population
// rules for opts.repoURL when bundling to an OCI target:
//
//   - --deployer argocd: derive `oci://...` from --output. Argo CD v3.1+
//     parses a schemeless registry/repo string as a Git remote and fails
//     on ssh-agent; the `oci://` prefix routes to its native OCI source.
//   - --deployer argocd-helm: do NOT derive. That bundle is URL-portable
//     by design — the publish location is supplied at `helm install`
//     time via `--set repoURL=...`. Auto-deriving would surface the
//     "--repo is ignored" warning even when the user never passed --repo.
//   - --deployer helm: never derive.
//   - explicit --repo: never overwritten.
func TestParseBundleCmdOptions_OCIRepoURLDerivation(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	const ociOutput = "oci://reg.example.com:5000/aicr/foo:1.2.3_build.5"

	tests := []struct {
		name           string
		args           []string
		expectedRepo   string
		expectedTarget string
	}{
		{
			name:           "argocd OCI derives oci:// scheme",
			args:           []string{"--recipe", recipePath, "--output", ociOutput, "--deployer", "argocd"},
			expectedRepo:   "oci://reg.example.com:5000/aicr/foo",
			expectedTarget: "1.2.3_build.5",
		},
		{
			name:           "argocd-helm OCI does NOT derive repoURL (URL-portable contract)",
			args:           []string{"--recipe", recipePath, "--output", ociOutput, "--deployer", "argocd-helm"},
			expectedRepo:   "",
			expectedTarget: "1.2.3+build.5",
		},
		{
			name:           "explicit --repo not overwritten",
			args:           []string{"--recipe", recipePath, "--output", ociOutput, "--deployer", "argocd", "--repo", "https://github.com/x/y"},
			expectedRepo:   "https://github.com/x/y",
			expectedTarget: "1.2.3_build.5",
		},
		{
			name:           "helm deployer leaves repoURL empty",
			args:           []string{"--recipe", recipePath, "--output", ociOutput, "--deployer", "helm"},
			expectedRepo:   "",
			expectedTarget: "1.2.3_build.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := captureBundleOpts(t, tt.args)
			if opts == nil {
				t.Fatal("captureBundleOpts returned nil")
			}
			if opts.repoURL != tt.expectedRepo {
				t.Errorf("repoURL = %q, want %q", opts.repoURL, tt.expectedRepo)
			}
			if opts.targetRevision != tt.expectedTarget {
				t.Errorf("targetRevision = %q, want %q", opts.targetRevision, tt.expectedTarget)
			}
		})
	}
}

// TestParseBundleCmdOptions_OCIChartNameDerivation verifies the bundle
// chart name is derived from the OCI artifact's last path segment when
// --output is OCI, and stays empty (deployer default applies) for local
// directory output. Regression coverage for issue #1019.
func TestParseBundleCmdOptions_OCIChartNameDerivation(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	tests := []struct {
		name          string
		args          []string
		wantChartName string
	}{
		{
			name:          "OCI output derives chart name from last path segment",
			args:          []string{"--recipe", recipePath, "--output", "oci://reg.example.com/myorg/my-bundle:1.2.3_build.5", "--deployer", "argocd-helm"},
			wantChartName: "my-bundle",
		},
		{
			name:          "OCI output with deeply nested repo takes only the tail",
			args:          []string{"--recipe", recipePath, "--output", "oci://reg.example.com/org/sub/team/custom-bundle:1.2.3_build.5", "--deployer", "argocd-helm"},
			wantChartName: "custom-bundle",
		},
		{
			name:          "local directory output leaves chart name empty",
			args:          []string{"--recipe", recipePath, "--output", filepath.Join(tmp, "out"), "--deployer", "argocd-helm"},
			wantChartName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := captureBundleOpts(t, tt.args)
			if opts == nil {
				t.Fatal("captureBundleOpts returned nil")
			}
			if opts.bundleChartName != tt.wantChartName {
				t.Errorf("bundleChartName = %q, want %q", opts.bundleChartName, tt.wantChartName)
			}
		})
	}
}

// TestParseBundleCmdOptions_AppName verifies --app-name parsing:
//   - empty default
//   - flows into opts.appName for argocd and argocd-helm
//   - rejected with ErrCodeInvalidRequest on non-Argo deployers
//   - rejected with ErrCodeInvalidRequest on invalid DNS-1123 names
//
// Regression coverage for issue #1011.
func TestParseBundleCmdOptions_AppName(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	out := filepath.Join(tmp, "out")

	t.Run("default empty for argocd-helm", func(t *testing.T) {
		opts := captureBundleOpts(t, []string{"--recipe", recipePath, "--output", out, "--deployer", "argocd-helm"})
		if opts == nil {
			t.Fatal("captureBundleOpts returned nil")
		}
		if opts.appName != "" {
			t.Errorf("appName = %q, want empty (deployer default applies)", opts.appName)
		}
	})

	t.Run("flows to opts.appName for argocd-helm", func(t *testing.T) {
		opts := captureBundleOpts(t, []string{"--recipe", recipePath, "--output", out, "--deployer", "argocd-helm", "--app-name", "gpu-runtime"})
		if opts == nil {
			t.Fatal("captureBundleOpts returned nil")
		}
		if opts.appName != "gpu-runtime" {
			t.Errorf("appName = %q, want %q", opts.appName, "gpu-runtime")
		}
	})

	t.Run("flows to opts.appName for argocd", func(t *testing.T) {
		opts := captureBundleOpts(t, []string{"--recipe", recipePath, "--output", out, "--deployer", "argocd", "--app-name", "ops-runtime"})
		if opts == nil {
			t.Fatal("captureBundleOpts returned nil")
		}
		if opts.appName != "ops-runtime" {
			t.Errorf("appName = %q, want %q", opts.appName, "ops-runtime")
		}
	})

	t.Run("rejected on helm deployer", func(t *testing.T) {
		opts, err := tryCaptureBundleOpts(t, []string{"--recipe", recipePath, "--output", out, "--deployer", "helm", "--app-name", "gpu-runtime"})
		if err == nil {
			t.Fatalf("expected error rejecting --app-name on helm deployer, got opts=%+v", opts)
		}
		if !strings.Contains(err.Error(), "only valid with") {
			t.Errorf("error should mention deployer restriction, got: %v", err)
		}
	})

	t.Run("rejected on flux deployer", func(t *testing.T) {
		opts, err := tryCaptureBundleOpts(t, []string{"--recipe", recipePath, "--output", out, "--deployer", "flux", "--app-name", "gpu-runtime"})
		if err == nil {
			t.Fatalf("expected error rejecting --app-name on flux deployer, got opts=%+v", opts)
		}
	})

	t.Run("rejected on invalid DNS-1123 name", func(t *testing.T) {
		opts, err := tryCaptureBundleOpts(t, []string{"--recipe", recipePath, "--output", out, "--deployer", "argocd-helm", "--app-name", "GPU_Runtime"})
		if err == nil {
			t.Fatalf("expected error rejecting invalid DNS name, got opts=%+v", opts)
		}
		if !strings.Contains(err.Error(), "DNS-1123") {
			t.Errorf("error should mention DNS-1123, got: %v", err)
		}
	})
}

// TestParseBundleCmdOptions_SigstoreURLs covers the --fulcio-url / --rekor-url
// flags: valid HTTPS endpoints land on the parsed options, unset leaves them
// empty (public-good defaults apply downstream), and a non-HTTPS endpoint is
// rejected at parse time. See issue #408.
func TestParseBundleCmdOptions_SigstoreURLs(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	out := filepath.Join(tmp, "out")
	base := []string{"--recipe", recipePath, "--output", out}

	t.Run("unset leaves both empty", func(t *testing.T) {
		opts := captureBundleOpts(t, base)
		if opts.fulcioURL != "" || opts.rekorURL != "" {
			t.Errorf("expected empty URLs, got fulcio=%q rekor=%q", opts.fulcioURL, opts.rekorURL)
		}
	})

	t.Run("env vars populate the endpoints", func(t *testing.T) {
		t.Setenv("AICR_FULCIO_URL", "https://fulcio.env.example.com")
		t.Setenv("AICR_REKOR_URL", "https://rekor.env.example.com")
		opts := captureBundleOpts(t, base)
		if opts.fulcioURL != "https://fulcio.env.example.com" {
			t.Errorf("fulcioURL from env = %q", opts.fulcioURL)
		}
		if opts.rekorURL != "https://rekor.env.example.com" {
			t.Errorf("rekorURL from env = %q", opts.rekorURL)
		}
	})

	t.Run("explicit flags override env vars", func(t *testing.T) {
		t.Setenv("AICR_FULCIO_URL", "https://fulcio.env.example.com")
		t.Setenv("AICR_REKOR_URL", "https://rekor.env.example.com")
		opts := captureBundleOpts(t, append(append([]string{}, base...),
			"--fulcio-url", "https://fulcio.flag.example.com",
			"--rekor-url", "https://rekor.flag.example.com"))
		if opts.fulcioURL != "https://fulcio.flag.example.com" {
			t.Errorf("fulcioURL = %q, want the flag value to win over AICR_FULCIO_URL", opts.fulcioURL)
		}
		if opts.rekorURL != "https://rekor.flag.example.com" {
			t.Errorf("rekorURL = %q, want the flag value to win over AICR_REKOR_URL", opts.rekorURL)
		}
	})

	t.Run("valid HTTPS endpoints flow to opts", func(t *testing.T) {
		opts := captureBundleOpts(t, append(append([]string{}, base...),
			"--fulcio-url", "https://fulcio.internal.example.com",
			"--rekor-url", "https://rekor.internal.example.com"))
		if opts.fulcioURL != "https://fulcio.internal.example.com" {
			t.Errorf("fulcioURL = %q", opts.fulcioURL)
		}
		if opts.rekorURL != "https://rekor.internal.example.com" {
			t.Errorf("rekorURL = %q", opts.rekorURL)
		}
	})

	t.Run("non-HTTPS fulcio-url is rejected", func(t *testing.T) {
		_, err := tryCaptureBundleOpts(t, append(append([]string{}, base...),
			"--fulcio-url", "http://fulcio.internal.example.com"))
		if err == nil {
			t.Fatal("expected error rejecting non-HTTPS --fulcio-url")
		}
		if !strings.Contains(err.Error(), "https") {
			t.Errorf("error should mention https requirement, got: %v", err)
		}
	})

	t.Run("malformed rekor-url is rejected", func(t *testing.T) {
		_, err := tryCaptureBundleOpts(t, append(append([]string{}, base...),
			"--rekor-url", "not-a-url"))
		if err == nil {
			t.Fatal("expected error rejecting malformed --rekor-url")
		}
	})
}

// TestPrintArgoCDHelmOCIInstructions exercises the post-#1051 install-hint
// contract: `helm install` against the full OCI artifact reference, and
// `--set repoURL` carrying only the parent namespace (chart name omitted
// — the chart template appends .Chart.Name itself). See issue #1020.
func TestPrintArgoCDHelmOCIInstructions(t *testing.T) {
	tests := []struct {
		name        string
		ref         *oci.Reference
		wantContain []string
		wantSkip    bool // true means no output expected
	}{
		{
			name: "registry with nested namespace",
			ref: &oci.Reference{
				IsOCI:      true,
				Registry:   "ghcr.io",
				Repository: "nvidia/aicr-bundle",
				Tag:        "1.0.0_build.5",
			},
			wantContain: []string{
				"helm install <release> oci://ghcr.io/nvidia/aicr-bundle \\",
				"--version 1.0.0+build.5",
				"# repoURL defaults to oci://ghcr.io/nvidia",
				"--namespace argocd",
			},
		},
		{
			name: "deeply nested namespace",
			ref: &oci.Reference{
				IsOCI:      true,
				Registry:   "registry.example.com",
				Repository: "team/platform/aicr-bundle",
				Tag:        "0.42.0",
			},
			wantContain: []string{
				"helm install <release> oci://registry.example.com/team/platform/aicr-bundle \\",
				"--version 0.42.0",
				"# repoURL defaults to oci://registry.example.com/team/platform",
			},
		},
		{
			name: "single-segment repo: parent collapses to registry-only",
			ref: &oci.Reference{
				IsOCI:      true,
				Registry:   "localhost:5000",
				Repository: "aicr-bundle",
				Tag:        "1.2.3",
			},
			wantContain: []string{
				"helm install <release> oci://localhost:5000/aicr-bundle \\",
				"--version 1.2.3",
				"# repoURL defaults to oci://localhost:5000",
			},
		},
		{
			name:     "nil ref skips silently",
			ref:      nil,
			wantSkip: true,
		},
		{
			name: "non-OCI ref skips silently",
			ref: &oci.Reference{
				IsOCI:     false,
				LocalPath: "./bundle",
			},
			wantSkip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			printArgoCDHelmOCIInstructions(&buf, tt.ref)
			got := buf.String()

			if tt.wantSkip {
				if got != "" {
					t.Errorf("expected no output, got: %q", got)
				}
				return
			}
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\nfull output:\n%s", want, got)
				}
			}
			// Sanity check: the repoURL line must NOT include the chart
			// name segment — the chart template appends .Chart.Name itself,
			// and a chart-name-bearing repoURL would cause double-append at
			// render time.
			if tt.ref != nil && tt.ref.IsOCI {
				chartName := tt.ref.ChartName()
				if chartName != "" {
					// The repoURL line is the last one; isolate it to avoid
					// false positives from the chartRef line above.
					var repoURLLine string
					for line := range strings.SplitSeq(got, "\n") {
						if strings.Contains(line, "--set repoURL=") {
							repoURLLine = line
							break
						}
					}
					if strings.Contains(repoURLLine, "/"+chartName) {
						t.Errorf("repoURL must not include chart name %q; got line: %q", chartName, repoURLLine)
					}
				}
			}
		})
	}
}

// runExclusivityCheck drives validateSigningKeyExclusivity through a real
// bundle command so cmd.IsSet reflects the parsed flags exactly as it would
// at runtime. The Action is swapped to build opts from the parsed flags (with
// no config, the resolved opts equal the flag values) and call only the
// exclusivity helper, avoiding the full parse/bundle path.
func runExclusivityCheck(t *testing.T, args []string) error {
	t.Helper()
	cmd := bundleCmd()
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		opts := &bundleCmdOptions{
			signingKey:        c.String(flagSigningKey),
			identityToken:     c.String(flagIdentityToken),
			oidcDeviceFlow:    c.Bool(flagOIDCDeviceFlow),
			fulcioURL:         c.String(flagFulcioURL),
			signingConfigPath: c.String(flagSigningConfig),
		}
		return validateSigningKeyExclusivity(c, opts)
	}
	return cmd.Run(context.Background(), append([]string{"bundle"}, args...))
}

// TestValidateSigningKeyExclusivity verifies that --signing-key (KMS, key-based
// signing) is rejected when combined with any keyless-OIDC-only flag, and is
// accepted on its own. KMS and keyless OIDC are distinct signing paths; mixing
// them is a request error rather than a silently-ignored flag. See #407.
func TestValidateSigningKeyExclusivity(t *testing.T) {
	const key = "awskms://arn:aws:kms:us-east-1:111:key/abc"

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			name:    "signing-key alone is valid",
			args:    []string{"--signing-key", key},
			wantErr: false,
		},
		{
			name:    "no signing-key is valid",
			args:    []string{"--attest"},
			wantErr: false,
		},
		{
			name:    "empty signing-key is rejected",
			args:    []string{"--signing-key", ""},
			wantErr: true,
		},
		{
			name:    "signing-key with identity-token is rejected",
			args:    []string{"--signing-key", key, "--identity-token", "tok"},
			wantErr: true,
		},
		{
			name:    "signing-key with oidc-device-flow is rejected",
			args:    []string{"--signing-key", key, "--oidc-device-flow"},
			wantErr: true,
		},
		{
			name:    "signing-key with fulcio-url is rejected",
			args:    []string{"--signing-key", key, "--fulcio-url", "https://fulcio.example.com"},
			wantErr: true,
		},
		{
			// KMS signs to Rekor v2 by default and accepts a custom signing
			// config, so --signing-key + --signing-config is valid (#1650).
			name:    "signing-key with signing-config is allowed",
			args:    []string{"--signing-key", key, "--signing-config", "sc.json"},
			wantErr: false,
		},
		{
			// --rekor-url is orthogonal to keyless-vs-KMS: KMS signing uploads
			// to Rekor too, so a private --rekor-url is valid with --signing-key.
			name:    "signing-key with rekor-url is allowed",
			args:    []string{"--signing-key", key, "--rekor-url", "https://rekor.example.com"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runExclusivityCheck(t, tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestValidateSigningKeyExclusivity_ConfigSourcedConflict proves the check
// catches a keyless option that arrives via config (resolved into opts) rather
// than an explicit flag: cmd.IsSet is false for it, but the resolved opts field
// is set, so the conflict must still be rejected.
func TestValidateSigningKeyExclusivity_ConfigSourcedConflict(t *testing.T) {
	cmd := bundleCmd() // unparsed: cmd.IsSet(...) is false for every flag
	opts := &bundleCmdOptions{
		signingKey: "awskms://arn:aws:kms:us-east-1:111:key/abc",
		fulcioURL:  "https://fulcio.example.com", // as if sourced from config
	}
	err := validateSigningKeyExclusivity(cmd, opts)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("want ErrCodeInvalidRequest for config-sourced fulcio-url, got %v", err)
	}
}

// TestBundleCmd_SigningKeyFlag verifies the --signing-key flag is wired onto
// the bundle command.
func TestBundleCmd_SigningKeyFlag(t *testing.T) {
	cmd := bundleCmd()
	for _, flag := range cmd.Flags {
		for _, name := range flag.Names() {
			if name == flagSigningKey {
				return
			}
		}
	}
	t.Errorf("expected flag %q to be defined", flagSigningKey)
}

// TestParseBundleCmdOptions_SigningKey verifies --signing-key flows onto the
// resolved options and that selectAttester forwards it into the attestation
// resolver, yielding a *KMSAttester. The KMS-vs-keyless precedence itself is
// covered by the attestation package's resolver tests; here we only confirm
// the CLI wiring. See #407.
func TestParseBundleCmdOptions_SigningKey(t *testing.T) {
	const key = "awskms://arn:aws:kms:us-east-1:111:key/abc"
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	out := filepath.Join(tmp, "out")

	opts := captureBundleOpts(t, []string{
		"--recipe", recipePath, "--output", out,
		"--attest", "--signing-key", key,
	})
	if opts.signingKey != key {
		t.Fatalf("signingKey = %q, want %q", opts.signingKey, key)
	}

	att, err := selectAttester(context.Background(), opts)
	if err != nil {
		t.Fatalf("selectAttester returned error: %v", err)
	}
	if _, ok := att.(*attestation.KMSAttester); !ok {
		t.Errorf("expected *KMSAttester when signing-key is set, got %T", att)
	}
}

func TestResolveBundleOCIChartVersion(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	t.Run("Argo CD Helm keeps raw tag and derives semantic chart version", func(t *testing.T) {
		opts := captureBundleOpts(t, []string{
			"--recipe", recipePath,
			"--output", "oci://registry.example.com/team/aicr-bundle:1.2.3_build.5",
			"--deployer", "argocd-helm",
		})
		if opts.ociRef.Tag != "1.2.3_build.5" {
			t.Fatalf("raw OCI tag = %q", opts.ociRef.Tag)
		}
		if opts.bundleChartVersion != "1.2.3+build.5" {
			t.Fatalf("bundle chart version = %q", opts.bundleChartVersion)
		}
		if opts.targetRevision != "1.2.3+build.5" {
			t.Fatalf("target revision = %q", opts.targetRevision)
		}
	})

	t.Run("generic OCI keeps raw tag", func(t *testing.T) {
		opts := captureBundleOpts(t, []string{
			"--recipe", recipePath,
			"--output", "oci://registry.example.com/team/aicr-bundle:dev",
			"--deployer", "helm",
		})
		if opts.targetRevision != "dev" || opts.bundleChartVersion != "" {
			t.Fatalf("targetRevision=%q bundleChartVersion=%q", opts.targetRevision, opts.bundleChartVersion)
		}
	})

	t.Run("local Argo CD Helm leaves chart version empty", func(t *testing.T) {
		opts := captureBundleOpts(t, []string{
			"--recipe", recipePath,
			"--output", filepath.Join(tmp, "local"),
			"--deployer", "argocd-helm",
		})
		if opts.bundleChartVersion != "" {
			t.Fatalf("bundle chart version = %q", opts.bundleChartVersion)
		}
	})

	for _, tag := range []string{"dev", "latest", "v1.2.3"} {
		t.Run("invalid Helm tag "+tag, func(t *testing.T) {
			_, err := tryCaptureBundleOpts(t, []string{
				"--recipe", recipePath,
				"--output", "oci://registry.example.com/team/aicr-bundle:" + tag,
				"--deployer", "argocd-helm",
			})
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("error = %v, want invalid request", err)
			}
		})
	}

	t.Run("local image refs rejected during parsing", func(t *testing.T) {
		_, err := tryCaptureBundleOpts(t, []string{
			"--recipe", recipePath,
			"--output", filepath.Join(tmp, "local-with-refs"),
			"--image-refs", filepath.Join(tmp, "refs.txt"),
		})
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("error = %v, want invalid request", err)
		}
	})
}

func TestBundleOutputTargetRetainsGeneratedIdentity(t *testing.T) {
	base := realTempDir(t)
	planned := filepath.Join(base, "nested", "bundle")
	target, prepErr := prepareBundleOutputTarget(context.Background(), planned)
	if prepErr != nil {
		t.Fatalf("prepareBundleOutputTarget() error = %v", prepErr)
	}
	t.Cleanup(func() {
		if closeErr := target.close(); closeErr != nil {
			t.Errorf("close() error = %v", closeErr)
		}
	})
	if !filepath.IsAbs(target.path) {
		t.Fatalf("target path is not absolute: %q", target.path)
	}
	if target.ancestorPath != base {
		t.Fatalf("ancestor = %q, want %q", target.ancestorPath, base)
	}

	if err := os.MkdirAll(planned, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(planned, "payload.txt"), []byte("original"), 0o640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := target.captureGenerated(context.Background(), planned); err != nil {
		t.Fatalf("captureGenerated() error = %v", err)
	}
	if err := target.validate(context.Background()); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
	infos, infosErr := target.regularFileInfos(context.Background())
	if infosErr != nil {
		t.Fatalf("regularFileInfos() error = %v", infosErr)
	}
	if len(infos) != 1 || infos[0].Size() != int64(len("original")) {
		t.Fatalf("regular file infos = %#v", infos)
	}

	moved := filepath.Join(base, "moved-bundle")
	if err := os.Rename(planned, moved); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if err := os.MkdirAll(planned, 0o755); err != nil {
		t.Fatalf("MkdirAll(replacement) error = %v", err)
	}
	if err := target.validate(context.Background()); !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("validate() after replacement = %v, want internal", err)
	}
}

func TestBundleCommandBundleOutputAndImageRefsPreflightOrder(t *testing.T) {
	workDir := realTempDir(t)
	t.Chdir(workDir)
	recipePath := filepath.Join(workDir, "recipe.yaml")
	const bareRecipe = `kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: test
componentRefs: []
`
	if err := os.WriteFile(recipePath, []byte(bareRecipe), 0o600); err != nil {
		t.Fatalf("WriteFile(recipe) error = %v", err)
	}
	refsPath := filepath.Join(workDir, "published", "refs.txt")
	if err := os.Mkdir(filepath.Dir(refsPath), 0o755); err != nil {
		t.Fatalf("Mkdir(refs parent) error = %v", err)
	}

	var order []string
	deps := defaultBundleCommandDependencies()
	deps.prepareBundleOutput = func(ctx context.Context, path string) (*bundleOutputTarget, error) {
		order = append(order, "bundle-preflight")
		assertDeadlineWithin(t, ctx, 30*time.Second)
		if !filepath.IsAbs(path) {
			t.Fatalf("planned bundle path is not absolute: %q", path)
		}
		return prepareBundleOutputTarget(ctx, path)
	}
	deps.prepareImageRefsTarget = func(ctx context.Context, bundle *bundleOutputTarget, path string) (*imageRefsTarget, error) {
		order = append(order, "refs-preflight")
		assertDeadlineWithin(t, ctx, 30*time.Second)
		return prepareImageRefsTarget(ctx, bundle, path)
	}
	deps.makeBundle = func(
		_ context.Context,
		_ *aicr.Client,
		_ *aicr.RecipeResult,
		opts aicr.BundleOptions,
	) (aicr.BundleArtifact, error) {

		order = append(order, "make")
		if opts.Config.BundleChartVersion() != "1.2.3+build.5" {
			t.Fatalf("BundleChartVersion() = %q", opts.Config.BundleChartVersion())
		}
		writeVerifiedCLITestBundle(
			t, opts.OutputDir, map[string][]byte{"payload.txt": []byte("generated")})
		return &result.Output{OutputDir: opts.OutputDir}, nil
	}
	deps.pushOCIBundle = func(
		publishCtx context.Context,
		opts *bundleCmdOptions,
		out *result.Output,
		bundle *bundleOutputTarget,
		refs *imageRefsTarget,
	) error {

		order = append(order, "push")
		if opts.ociRef.Tag != "1.2.3_build.5" || opts.targetRevision != "1.2.3+build.5" {
			t.Fatalf("raw tag=%q targetRevision=%q", opts.ociRef.Tag, opts.targetRevision)
		}
		if out.OutputDir != bundle.path || refs == nil || refs.bundle != bundle {
			t.Fatalf("retained owners do not match generated output")
		}
		return bundle.validate(publishCtx)
	}

	cmd := bundleCmd()
	cmd.Action = func(ctx context.Context, command *cli.Command) error {
		return runBundleCmdWithDependencies(ctx, command, deps)
	}
	err := cmd.Run(context.Background(), []string{
		"bundle",
		"--recipe", recipePath,
		"--output", "oci://registry.example.com/team/aicr-bundle:1.2.3_build.5",
		"--deployer", "argocd-helm",
		"--image-refs", refsPath,
	})
	if err != nil {
		t.Fatalf("runBundleCmdWithDependencies() error = %v", err)
	}
	want := []string{"bundle-preflight", "refs-preflight", "make", "push"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %#v, want %#v", order, want)
	}
}

func TestBundleCommandPushesCapturedBundleAfterPublishedOutputMutation(t *testing.T) {
	const (
		generatedPayload = "generated"
		replacedPayload  = "replaced after local publication"
	)

	deps := defaultBundleCommandDependencies()
	deps.makeBundle = func(
		_ context.Context,
		_ *aicr.Client,
		_ *aicr.RecipeResult,
		opts aicr.BundleOptions,
	) (aicr.BundleArtifact, error) {

		writeVerifiedCLITestBundle(t, opts.OutputDir, map[string][]byte{
			"payload.txt": []byte(generatedPayload),
		})
		return &result.Output{OutputDir: opts.OutputDir}, nil
	}

	var pushedPayload []byte
	var publicationStageCalls int
	deps.pushOCIBundle = func(
		ctx context.Context,
		opts *bundleCmdOptions,
		out *result.Output,
		bundle *bundleOutputTarget,
		imageRefs *imageRefsTarget,
	) error {

		writeVerifiedCLITestBundle(t, out.OutputDir, map[string][]byte{
			"payload.txt": []byte(replacedPayload),
		})
		publishDeps := defaultBundlePublishDependencies()
		stageVerifiedBundle := publishDeps.stageVerifiedBundle
		publishDeps.stageVerifiedBundle = func(
			ctx context.Context,
			source string,
			options checksum.InventoryOptions,
		) (string, *checksum.Inventory, func() error, error) {

			publicationStageCalls++
			return stageVerifiedBundle(ctx, source, options)
		}
		publishDeps.packageAndPush = func(
			_ context.Context,
			cfg oci.OutputConfig,
		) (*oci.PackageAndPushResult, error) {

			var readErr error
			pushedPayload, readErr = os.ReadFile(filepath.Join(cfg.SourceDir, "payload.txt"))
			if readErr != nil {
				return nil, readErr
			}
			return &oci.PackageAndPushResult{
				Digest:    "sha256:captured",
				Reference: "registry.example.com/team/bundle:dev",
			}, nil
		}
		return pushOCIBundleWithDependencies(
			ctx, opts, out, bundle, imageRefs, publishDeps)
	}

	if err := runDeadlineBundleCommand(t, context.Background(), deps, false); err != nil {
		t.Fatalf("runBundleCmdWithDependencies() error = %v", err)
	}
	if publicationStageCalls != 0 {
		t.Fatalf("caller-visible publication stage calls = %d, want 0", publicationStageCalls)
	}
	if got := string(pushedPayload); got != generatedPayload {
		t.Fatalf("pushed payload = %q, want captured payload %q", got, generatedPayload)
	}
}

func TestBundleCommandRejectsGenerationTempInsideExistingOutput(t *testing.T) {
	deps := defaultBundleCommandDependencies()
	var outputDir string
	deps.prepareBundleOutput = func(ctx context.Context, path string) (*bundleOutputTarget, error) {
		outputDir = path
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(path, "existing.txt"), []byte("existing"), 0o600); err != nil {
			return nil, err
		}
		t.Setenv("TMPDIR", path)
		return prepareBundleOutputTarget(ctx, path)
	}

	var makeCalls int
	deps.makeBundle = func(
		context.Context,
		*aicr.Client,
		*aicr.RecipeResult,
		aicr.BundleOptions,
	) (aicr.BundleArtifact, error) {

		makeCalls++
		if err := os.WriteFile(filepath.Join(outputDir, "mutated.txt"), []byte("mutated"), 0o600); err != nil {
			return nil, err
		}
		return nil, errors.New(errors.ErrCodeInternal, "generation should not run")
	}

	err := runDeadlineBundleCommand(t, context.Background(), deps, false)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("error = %v, want ErrCodeInternal", err)
	}
	if makeCalls != 0 {
		t.Fatalf("MakeBundle() calls = %d, want 0", makeCalls)
	}
	if _, statErr := os.Lstat(filepath.Join(outputDir, "mutated.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("planned output mutated before workspace rejection: %v", statErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(outputDir, "existing.txt")); readErr != nil || string(got) != "existing" {
		t.Fatalf("existing output = %q, %v; want unchanged", got, readErr)
	}
}

func TestBundleOutputTargetRejectsSymlinkComponent(t *testing.T) {
	base := realTempDir(t)
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	_, err := prepareBundleOutputTarget(context.Background(), filepath.Join(link, "bundle"))
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("error = %v, want invalid request", err)
	}
}

func TestBundleOutputTargetDetectsAncestorAndGeneratedSwaps(t *testing.T) {
	t.Run("ancestor swap before retained open", func(t *testing.T) {
		base := realTempDir(t)
		planned := filepath.Join(base, "bundle")
		moved := base + "-moved"
		deps := defaultBundleOutputDependencies()
		deps.beforeAncestorOpen = func(path string) error {
			if path != base {
				t.Fatalf("ancestor path = %q, want %q", path, base)
			}
			if err := os.Rename(base, moved); err != nil {
				return err
			}
			return os.Mkdir(base, 0o755)
		}
		t.Cleanup(func() { _ = os.RemoveAll(moved) })
		_, err := prepareBundleOutputTargetWithDependencies(context.Background(), planned, deps)
		if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
			t.Fatalf("error = %v, want internal", err)
		}
	})

	t.Run("generated root swap before retained open", func(t *testing.T) {
		base := realTempDir(t)
		planned := filepath.Join(base, "bundle")
		moved := filepath.Join(base, "moved")
		deps := defaultBundleOutputDependencies()
		deps.beforeGeneratedOpen = func(*bundleOutputTarget) error {
			if err := os.Rename(planned, moved); err != nil {
				return err
			}
			return os.Mkdir(planned, 0o755)
		}
		target, prepErr := prepareBundleOutputTargetWithDependencies(context.Background(), planned, deps)
		if prepErr != nil {
			t.Fatalf("prepare error = %v", prepErr)
		}
		t.Cleanup(func() { _ = target.close() })
		if err := os.Mkdir(planned, 0o755); err != nil {
			t.Fatalf("Mkdir(bundle) error = %v", err)
		}
		captureErr := target.captureGenerated(context.Background(), planned)
		if !stderrors.Is(captureErr, errors.New(errors.ErrCodeInternal, "")) {
			t.Fatalf("capture error = %v, want internal", captureErr)
		}
	})
}

func TestImageRefsTargetRejectsBundleAliases(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	if err := os.Mkdir(bundleDir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	payload := filepath.Join(bundleDir, "payload.txt")
	if err := os.WriteFile(payload, []byte("bundle"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	bundle := mustPreparedBundleOutput(t, bundleDir)

	for _, path := range []string{bundleDir, filepath.Join(bundleDir, "refs.txt")} {
		if target, err := prepareImageRefsTarget(context.Background(), bundle, path); err == nil {
			_ = target.close()
			t.Fatalf("prepareImageRefsTarget(%q) unexpectedly succeeded", path)
		}
	}

	external := realTempDir(t)
	hardlink := filepath.Join(external, "hardlink.txt")
	if err := os.Link(payload, hardlink); err != nil {
		t.Fatalf("Link() error = %v", err)
	}
	if target, err := prepareImageRefsTarget(context.Background(), bundle, hardlink); err == nil {
		_ = target.close()
		t.Fatal("hardlink target unexpectedly accepted")
	}

	symlink := filepath.Join(external, "symlink.txt")
	if err := os.Symlink(filepath.Join(external, "real.txt"), symlink); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if target, err := prepareImageRefsTarget(context.Background(), bundle, symlink); err == nil {
		_ = target.close()
		t.Fatal("symlink target unexpectedly accepted")
	}
}

func TestImageRefsWriteAtomicUsesAnchoredRenameAndMode(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	if err := os.Mkdir(bundleDir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "payload.txt"), []byte("bundle"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	bundle := mustPreparedBundleOutput(t, bundleDir)

	parent := realTempDir(t)
	path := filepath.Join(parent, "refs.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile(old) error = %v", err)
	}
	target, prepErr := prepareImageRefsTarget(context.Background(), bundle, path)
	if prepErr != nil {
		t.Fatalf("prepareImageRefsTarget() error = %v", prepErr)
	}
	t.Cleanup(func() {
		if closeErr := target.close(); closeErr != nil {
			t.Errorf("close() error = %v", closeErr)
		}
	})

	if err := target.writeAtomic(context.Background(), []byte("sha256:abc\n")); err != nil {
		t.Fatalf("writeAtomic() error = %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if string(got) != "sha256:abc\n" {
		t.Fatalf("content = %q", got)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		t.Fatalf("Lstat() error = %v", statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	entries, readDirErr := os.ReadDir(parent)
	if readDirErr != nil {
		t.Fatalf("ReadDir() error = %v", readDirErr)
	}
	if len(entries) != 1 || entries[0].Name() != "refs.txt" {
		t.Fatalf("unexpected residue: %#v", entries)
	}
}

func TestImageRefsWriteAtomicDetectsInjectedHardlinkSwap(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	if err := os.Mkdir(bundleDir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	payload := filepath.Join(bundleDir, "payload.txt")
	original := []byte("immutable bundle payload")
	if err := os.WriteFile(payload, original, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	bundle := mustPreparedBundleOutput(t, bundleDir)

	parent := realTempDir(t)
	path := filepath.Join(parent, "refs.txt")
	deps := defaultImageRefsTargetDependencies()
	swapped := false
	deps.beforeTargetRevalidate = func(_ *imageRefsTarget) error {
		if swapped {
			return nil
		}
		swapped = true
		return os.Link(payload, path)
	}
	target, err := prepareImageRefsTargetWithDependencies(context.Background(), bundle, path, deps)
	if err != nil {
		t.Fatalf("prepareImageRefsTargetWithDependencies() error = %v", err)
	}
	t.Cleanup(func() { _ = target.close() })

	err = target.writeAtomic(context.Background(), []byte("attacker-controlled\n"))
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("writeAtomic() error = %v, want internal", err)
	}
	got, readErr := os.ReadFile(payload)
	if readErr != nil {
		t.Fatalf("ReadFile(payload) error = %v", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("bundle payload changed: %q", got)
	}
}

func TestImageRefsWriteAtomicDetectsInjectedParentAndTempSwaps(t *testing.T) {
	newBundle := func(t *testing.T) *bundleOutputTarget {
		t.Helper()
		bundleDir := filepath.Join(realTempDir(t), "bundle")
		if err := os.Mkdir(bundleDir, 0o755); err != nil {
			t.Fatalf("Mkdir(bundle) error = %v", err)
		}
		return mustPreparedBundleOutput(t, bundleDir)
	}

	t.Run("parent", func(t *testing.T) {
		bundle := newBundle(t)
		parent := realTempDir(t)
		moved := parent + "-moved"
		deps := defaultImageRefsTargetDependencies()
		deps.beforeParentRevalidate = func(*imageRefsTarget) error {
			if err := os.Rename(parent, moved); err != nil {
				return err
			}
			return os.Mkdir(parent, 0o755)
		}
		t.Cleanup(func() { _ = os.RemoveAll(moved) })
		target, err := prepareImageRefsTargetWithDependencies(
			context.Background(), bundle, filepath.Join(parent, "refs.txt"), deps)
		if err != nil {
			t.Fatalf("prepare error = %v", err)
		}
		t.Cleanup(func() { _ = target.close() })
		writeErr := target.writeAtomic(context.Background(), []byte("sha256:abc\n"))
		if !stderrors.Is(writeErr, errors.New(errors.ErrCodeInternal, "")) {
			t.Fatalf("write error = %v, want internal", writeErr)
		}
		if _, err := os.Lstat(filepath.Join(parent, "refs.txt")); !os.IsNotExist(err) {
			t.Fatalf("replacement parent was modified: %v", err)
		}
	})

	t.Run("temporary entry", func(t *testing.T) {
		bundle := newBundle(t)
		parent := realTempDir(t)
		path := filepath.Join(parent, "refs.txt")
		replacement := []byte("replacement!")
		var replacedName string
		deps := defaultImageRefsTargetDependencies()
		deps.beforeTempRevalidate = func(_ *imageRefsTarget, name string, _ os.FileInfo) error {
			replacedName = name
			if err := os.Rename(filepath.Join(parent, name), filepath.Join(parent, "moved-temp")); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(parent, name), replacement, 0o600)
		}
		target, err := prepareImageRefsTargetWithDependencies(context.Background(), bundle, path, deps)
		if err != nil {
			t.Fatalf("prepare error = %v", err)
		}
		t.Cleanup(func() { _ = target.close() })
		writeErr := target.writeAtomic(context.Background(), []byte("sha256:abc\n"))
		if !stderrors.Is(writeErr, errors.New(errors.ErrCodeInternal, "")) {
			t.Fatalf("write error = %v, want internal", writeErr)
		}
		got, err := os.ReadFile(filepath.Join(parent, replacedName))
		if err != nil {
			t.Fatalf("ReadFile(replacement temp) error = %v", err)
		}
		if !bytes.Equal(got, replacement) {
			t.Fatalf("replacement temp changed: %q", got)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("target unexpectedly published: %v", err)
		}
	})
}

func TestImageRefsTargetDetectsParentAndTargetPreflightSwaps(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	if err := os.Mkdir(bundleDir, 0o755); err != nil {
		t.Fatalf("Mkdir(bundle) error = %v", err)
	}
	bundle := mustPreparedBundleOutput(t, bundleDir)

	t.Run("parent swap", func(t *testing.T) {
		parent := realTempDir(t)
		moved := parent + "-moved"
		deps := defaultImageRefsTargetDependencies()
		deps.beforeParentOpen = func(string) error {
			if err := os.Rename(parent, moved); err != nil {
				return err
			}
			return os.Mkdir(parent, 0o755)
		}
		t.Cleanup(func() { _ = os.RemoveAll(moved) })
		_, err := prepareImageRefsTargetWithDependencies(
			context.Background(), bundle, filepath.Join(parent, "refs.txt"), deps)
		if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
			t.Fatalf("error = %v, want internal", err)
		}
	})

	t.Run("target swap", func(t *testing.T) {
		parent := realTempDir(t)
		path := filepath.Join(parent, "refs.txt")
		moved := filepath.Join(parent, "moved.txt")
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatalf("WriteFile(target) error = %v", err)
		}
		deps := defaultImageRefsTargetDependencies()
		deps.beforeTargetOpen = func(*imageRefsTarget) error {
			if err := os.Rename(path, moved); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("replacement"), 0o600)
		}
		_, err := prepareImageRefsTargetWithDependencies(context.Background(), bundle, path, deps)
		if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
			t.Fatalf("error = %v, want internal", err)
		}
	})
}

func TestImageRefsWriteAtomicCleansUpShortWrite(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	if err := os.Mkdir(bundleDir, 0o755); err != nil {
		t.Fatalf("Mkdir(bundle) error = %v", err)
	}
	bundle := mustPreparedBundleOutput(t, bundleDir)
	parent := realTempDir(t)
	path := filepath.Join(parent, "refs.txt")
	deps := defaultImageRefsTargetDependencies()
	deps.fileWrite = func(file *os.File, data []byte) (int, error) {
		written, err := file.Write(data[:len(data)-1])
		return written, err
	}
	target, prepErr := prepareImageRefsTargetWithDependencies(context.Background(), bundle, path, deps)
	if prepErr != nil {
		t.Fatalf("prepare error = %v", prepErr)
	}
	t.Cleanup(func() { _ = target.close() })
	writeErr := target.writeAtomic(context.Background(), []byte("sha256:abc\n"))
	if !stderrors.Is(writeErr, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("write error = %v, want internal", writeErr)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatalf("ReadDir() error = %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary residue remains: %#v", entries)
	}
}

func TestPushOCIBundleStagesExactInventoryBeforePublishing(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	writeVerifiedCLITestBundle(t, bundleDir, map[string][]byte{"payload.txt": []byte("payload")})
	bundle := mustPreparedBundleOutput(t, bundleDir)

	refsPath := filepath.Join(realTempDir(t), "refs.txt")
	refs, err := prepareImageRefsTarget(context.Background(), bundle, refsPath)
	if err != nil {
		t.Fatalf("prepareImageRefsTarget() error = %v", err)
	}
	t.Cleanup(func() { _ = refs.close() })

	var order []string
	var stagedPath, workspacePath string
	deps := defaultBundlePublishDependencies()
	deps.stageVerifiedBundle = func(ctx context.Context, source string, opts checksum.InventoryOptions) (string, *checksum.Inventory, func() error, error) {
		order = append(order, "stage")
		assertDeadlineWithin(t, ctx, 2*time.Minute)
		staged, inventory, cleanup, stageErr := checksum.StageVerifiedBundle(ctx, source, opts)
		stagedPath = staged
		return staged, inventory, cleanup, stageErr
	}
	deps.newWorkspace = func(ctx context.Context, prefix string, excluded ...string) (*oci.Workspace, error) {
		order = append(order, "workspace")
		workspace, workspaceErr := oci.NewPrivateWorkspace(ctx, prefix, excluded...)
		if workspace != nil {
			workspacePath = workspace.Path()
		}
		return workspace, workspaceErr
	}
	deps.packageAndPush = func(ctx context.Context, cfg oci.OutputConfig) (*oci.PackageAndPushResult, error) {
		order = append(order, "publish")
		assertDeadlineWithin(t, ctx, 35*time.Minute)
		if cfg.SourceDir != stagedPath {
			t.Fatalf("SourceDir = %q, want staged %q", cfg.SourceDir, stagedPath)
		}
		if cfg.OutputDir != workspacePath {
			t.Fatalf("OutputDir = %q, want workspace %q", cfg.OutputDir, workspacePath)
		}
		wantFiles := []string{"checksums.txt", "payload.txt"}
		if !reflect.DeepEqual(cfg.SourceFiles, wantFiles) {
			t.Fatalf("SourceFiles = %#v, want %#v", cfg.SourceFiles, wantFiles)
		}
		return &oci.PackageAndPushResult{Digest: "sha256:abc", Reference: "registry.example.com/team/bundle:dev"}, nil
	}
	deps.writeImageRefs = func(ctx context.Context, target *imageRefsTarget, data []byte) error {
		order = append(order, "refs")
		assertDeadlineWithin(t, ctx, 30*time.Second)
		if target != refs || string(data) != "sha256:abc\n" {
			t.Fatalf("target=%p data=%q", target, data)
		}
		return target.writeAtomic(ctx, data)
	}

	opts := &bundleCmdOptions{
		deployer:      config.DeployerHelm,
		ociRef:        &oci.Reference{IsOCI: true, Registry: "registry.example.com", Repository: "team/bundle", Tag: "dev"},
		imageRefsPath: refsPath,
	}
	out := &result.Output{OutputDir: bundleDir}
	if err := pushOCIBundleWithDependencies(context.Background(), opts, out, bundle, refs, deps); err != nil {
		t.Fatalf("pushOCIBundleWithDependencies() error = %v", err)
	}
	if !reflect.DeepEqual(order, []string{"stage", "workspace", "publish", "refs"}) {
		t.Fatalf("order = %#v", order)
	}
	if _, err := os.Lstat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged path still exists: %v", err)
	}
	if _, err := os.Lstat(workspacePath); !os.IsNotExist(err) {
		t.Fatalf("workspace path still exists: %v", err)
	}
}

func TestPushOCIBundlePreservesPrimaryErrorWhenCleanupFails(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	writeVerifiedCLITestBundle(t, bundleDir, map[string][]byte{"payload.txt": []byte("payload")})
	bundle := mustPreparedBundleOutput(t, bundleDir)

	deps := defaultBundlePublishDependencies()
	deps.stageVerifiedBundle = func(ctx context.Context, source string, opts checksum.InventoryOptions) (string, *checksum.Inventory, func() error, error) {
		staged, inventory, cleanup, err := checksum.StageVerifiedBundle(ctx, source, opts)
		if err != nil {
			return "", nil, nil, err
		}
		return staged, inventory, func() error {
			return stderrors.Join(cleanup(), stderrors.New("injected stage cleanup failure"))
		}, nil
	}
	deps.packageAndPush = func(context.Context, oci.OutputConfig) (*oci.PackageAndPushResult, error) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "injected publication failure")
	}

	opts := &bundleCmdOptions{
		deployer: config.DeployerHelm,
		ociRef:   &oci.Reference{IsOCI: true, Registry: "registry.example.com", Repository: "team/bundle", Tag: "dev"},
	}
	err := pushOCIBundleWithDependencies(context.Background(), opts, &result.Output{OutputDir: bundleDir}, bundle, nil, deps)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("error = %v, want primary invalid request", err)
	}
}

func TestPushOCIBundleHelmUsesStagedExactInventoryAndExternalWorkspace(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "aicr-bundle")
	writeVerifiedCLITestBundle(t, bundleDir, map[string][]byte{
		"Chart.yaml":  []byte("apiVersion: v2\nname: aicr-bundle\nversion: 1.2.3+build.5\n"),
		"values.yaml": []byte("replicas: 1\n"),
	})
	bundle := mustPreparedBundleOutput(t, bundleDir)
	deps := defaultBundlePublishDependencies()
	deps.packageAndPush = func(context.Context, oci.OutputConfig) (*oci.PackageAndPushResult, error) {
		t.Fatal("generic publisher called for Argo CD Helm")
		return nil, stderrors.New("unreachable generic publisher")
	}
	deps.packageAndPushHelm = func(_ context.Context, opts oci.HelmChartOptions) (*oci.PackageAndPushResult, error) {
		if opts.SourceDir == bundleDir || sameOrBelowPath(opts.OutputDir, bundleDir) ||
			sameOrBelowPath(opts.OutputDir, opts.SourceDir) {

			t.Fatalf("unsafe publication paths: source=%q output=%q", opts.SourceDir, opts.OutputDir)
		}
		want := []string{"Chart.yaml", "checksums.txt", "values.yaml"}
		if !reflect.DeepEqual(opts.SourceFiles, want) {
			t.Fatalf("SourceFiles = %#v, want %#v", opts.SourceFiles, want)
		}
		if opts.Reference.Tag != "1.2.3_build.5" {
			t.Fatalf("raw tag = %q", opts.Reference.Tag)
		}
		return &oci.PackageAndPushResult{
			Digest:    "sha256:def",
			Reference: "registry.example.com/team/aicr-bundle:1.2.3_build.5",
		}, nil
	}
	opts := &bundleCmdOptions{
		deployer:           config.DeployerArgoCDHelm,
		bundleChartVersion: "1.2.3+build.5",
		ociRef: &oci.Reference{
			IsOCI:      true,
			Registry:   "registry.example.com",
			Repository: "team/aicr-bundle",
			Tag:        "1.2.3_build.5",
		},
	}
	if err := pushOCIBundleWithDependencies(
		context.Background(), opts, &result.Output{OutputDir: bundleDir}, bundle, nil, deps); err != nil {
		t.Fatalf("pushOCIBundleWithDependencies() error = %v", err)
	}
}

func TestPushOCIBundleRejectsMismatchedGeneratedOutputBeforeStaging(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	if err := os.Mkdir(bundleDir, 0o755); err != nil {
		t.Fatalf("Mkdir(bundle) error = %v", err)
	}
	bundle := mustPreparedBundleOutput(t, bundleDir)
	stageCalls := 0
	deps := defaultBundlePublishDependencies()
	deps.stageVerifiedBundle = func(
		context.Context,
		string,
		checksum.InventoryOptions,
	) (string, *checksum.Inventory, func() error, error) {

		stageCalls++
		return "", nil, nil, nil
	}
	opts := &bundleCmdOptions{
		deployer: config.DeployerHelm,
		ociRef: &oci.Reference{
			IsOCI: true, Registry: "registry.example.com", Repository: "team/bundle", Tag: "dev",
		},
	}
	err := pushOCIBundleWithDependencies(context.Background(), opts,
		&result.Output{OutputDir: filepath.Join(filepath.Dir(bundleDir), "other")}, bundle, nil, deps)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("error = %v, want internal", err)
	}
	if stageCalls != 0 {
		t.Fatalf("stage calls = %d, want zero", stageCalls)
	}
}

func TestPushOCIBundleCanceledWholePublication(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	writeVerifiedCLITestBundle(t, bundleDir, map[string][]byte{"payload.txt": []byte("payload")})
	bundle := mustPreparedBundleOutput(t, bundleDir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := pushOCIBundle(ctx, &bundleCmdOptions{
		deployer: config.DeployerHelm,
		ociRef:   &oci.Reference{IsOCI: true, Registry: "registry.example.com", Repository: "team/bundle", Tag: "dev"},
	}, &result.Output{OutputDir: bundleDir}, bundle, nil)
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Fatalf("error = %v, want timeout", err)
	}
}

func mustPreparedBundleOutput(t *testing.T, path string) *bundleOutputTarget {
	t.Helper()
	target, err := prepareBundleOutputTarget(context.Background(), path)
	if err != nil {
		t.Fatalf("prepareBundleOutputTarget() error = %v", err)
	}
	if err := target.captureGenerated(context.Background(), path); err != nil {
		_ = target.close()
		t.Fatalf("captureGenerated() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := target.close(); closeErr != nil {
			t.Errorf("bundle target close error = %v", closeErr)
		}
	})
	return target
}

func realTempDir(t *testing.T) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp dir) error = %v", err)
	}
	return real
}

func writeVerifiedCLITestBundle(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	paths := make([]string, 0, len(files))
	for rel, data := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", rel, err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", rel, err)
		}
		paths = append(paths, rel)
	}
	slicesSort(paths)
	var manifest strings.Builder
	for _, rel := range paths {
		digest := sha256.Sum256(files[rel])
		manifest.WriteString(hex.EncodeToString(digest[:]))
		manifest.WriteString("  ")
		manifest.WriteString(filepath.ToSlash(rel))
		manifest.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, checksum.ChecksumFileName), []byte(manifest.String()), 0o600); err != nil {
		t.Fatalf("WriteFile(checksums) error = %v", err)
	}
}

func slicesSort(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func assertDeadlineWithin(t *testing.T, ctx context.Context, max time.Duration) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > max {
		t.Fatalf("deadline remaining = %s, want (0,%s]", remaining, max)
	}
}
