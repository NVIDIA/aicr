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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/oci"
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

	const ociOutput = "oci://reg.example.com:5000/aicr/foo:v1"

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
			expectedTarget: "v1",
		},
		{
			name:           "argocd-helm OCI does NOT derive repoURL (URL-portable contract)",
			args:           []string{"--recipe", recipePath, "--output", ociOutput, "--deployer", "argocd-helm"},
			expectedRepo:   "",
			expectedTarget: "v1",
		},
		{
			name:           "explicit --repo not overwritten",
			args:           []string{"--recipe", recipePath, "--output", ociOutput, "--deployer", "argocd", "--repo", "https://github.com/x/y"},
			expectedRepo:   "https://github.com/x/y",
			expectedTarget: "v1",
		},
		{
			name:           "helm deployer leaves repoURL empty",
			args:           []string{"--recipe", recipePath, "--output", ociOutput, "--deployer", "helm"},
			expectedRepo:   "",
			expectedTarget: "v1",
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
			args:          []string{"--recipe", recipePath, "--output", "oci://reg.example.com/myorg/my-bundle:v1", "--deployer", "argocd-helm"},
			wantChartName: "my-bundle",
		},
		{
			name:          "OCI output with deeply nested repo takes only the tail",
			args:          []string{"--recipe", recipePath, "--output", "oci://reg.example.com/org/sub/team/custom-bundle:v1", "--deployer", "argocd-helm"},
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
				Tag:        "v1.0.0",
			},
			wantContain: []string{
				"oci://ghcr.io/nvidia/aicr-bundle:v1.0.0",
				"--set repoURL=oci://ghcr.io/nvidia",
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
				"oci://registry.example.com/team/platform/aicr-bundle:0.42.0",
				"--set repoURL=oci://registry.example.com/team/platform",
			},
		},
		{
			name: "single-segment repo: parent collapses to registry-only",
			ref: &oci.Reference{
				IsOCI:      true,
				Registry:   "localhost:5000",
				Repository: "aicr-bundle",
				Tag:        "dev",
			},
			wantContain: []string{
				"oci://localhost:5000/aicr-bundle:dev",
				"--set repoURL=oci://localhost:5000",
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
