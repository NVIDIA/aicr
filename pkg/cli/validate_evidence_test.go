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
	"context"
	"testing"

	"github.com/urfave/cli/v3"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/config"
)

// runEvidenceCmd builds a minimal cli.Command that exposes the
// flag-set buildRecipeEvidenceConfig reads, runs it with the supplied
// args, and returns the resolved *recipeEvidenceConfig (or nil) plus
// any action error.
func runEvidenceCmd(t *testing.T, args []string, resolved *config.ValidateResolved) (*recipeEvidenceConfig, error) {
	t.Helper()
	var got *recipeEvidenceConfig
	cmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "emit-attestation"},
			&cli.StringFlag{Name: "bom"},
			&cli.StringFlag{Name: "push"},
			&cli.BoolFlag{Name: "plain-http"},
			&cli.BoolFlag{Name: "insecure-tls"},
			&cli.StringFlag{Name: "identity-token"},
			&cli.BoolFlag{Name: "oidc-device-flow"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			got = buildRecipeEvidenceConfig(c, resolved)
			return nil
		},
	}
	if err := cmd.Run(context.Background(), append([]string{"test"}, args...)); err != nil {
		return nil, err
	}
	return got, nil
}

func TestBuildRecipeEvidenceConfig(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		resolved *config.ValidateResolved
		wantNil  bool
		want     *recipeEvidenceConfig
	}{
		{
			name:     "neither flag nor config returns nil",
			args:     nil,
			resolved: &config.ValidateResolved{},
			wantNil:  true,
		},
		{
			name: "flag-only populates from flags",
			args: []string{
				"--emit-attestation", "/tmp/out",
				"--bom", "/tmp/bom.json",
				"--push", "ttl.sh/x:1h",
				"--plain-http",
				"--identity-token", "tok",
				"--oidc-device-flow",
			},
			resolved: &config.ValidateResolved{},
			want: &recipeEvidenceConfig{
				OutDir:    "/tmp/out",
				BOMPath:   "/tmp/bom.json",
				Push:      "ttl.sh/x:1h",
				PlainHTTP: true,
				OIDCResolve: bundleattest.ResolveOptions{
					IdentityToken: "tok",
					DeviceFlow:    true,
				},
			},
		},
		{
			name: "config-only fallback when flags unset",
			args: nil,
			resolved: &config.ValidateResolved{
				EvidenceAttestation: &config.EvidenceAttestationResolved{
					Out:         "/cfg/out",
					BOM:         "/cfg/bom.json",
					Push:        "ttl.sh/cfg:1h",
					PlainHTTP:   true,
					InsecureTLS: true,
				},
			},
			want: &recipeEvidenceConfig{
				OutDir:      "/cfg/out",
				BOMPath:     "/cfg/bom.json",
				Push:        "ttl.sh/cfg:1h",
				PlainHTTP:   true,
				InsecureTLS: true,
			},
		},
		{
			name: "flag overrides config when both set",
			args: []string{"--emit-attestation", "/flag/out", "--push", "ttl.sh/flag:1h"},
			resolved: &config.ValidateResolved{
				EvidenceAttestation: &config.EvidenceAttestationResolved{
					Out:  "/cfg/out",
					Push: "ttl.sh/cfg:1h",
					BOM:  "/cfg/bom.json",
				},
			},
			want: &recipeEvidenceConfig{
				OutDir:  "/flag/out",
				Push:    "ttl.sh/flag:1h",
				BOMPath: "/cfg/bom.json",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runEvidenceCmd(t, tt.args, tt.resolved)
			if err != nil {
				t.Fatalf("cmd.Run: %v", err)
			}
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil config")
			}
			if got.OutDir != tt.want.OutDir {
				t.Errorf("OutDir = %q, want %q", got.OutDir, tt.want.OutDir)
			}
			if got.BOMPath != tt.want.BOMPath {
				t.Errorf("BOMPath = %q, want %q", got.BOMPath, tt.want.BOMPath)
			}
			if got.Push != tt.want.Push {
				t.Errorf("Push = %q, want %q", got.Push, tt.want.Push)
			}
			if got.PlainHTTP != tt.want.PlainHTTP {
				t.Errorf("PlainHTTP = %v, want %v", got.PlainHTTP, tt.want.PlainHTTP)
			}
			if got.InsecureTLS != tt.want.InsecureTLS {
				t.Errorf("InsecureTLS = %v, want %v", got.InsecureTLS, tt.want.InsecureTLS)
			}
			if got.OIDCResolve.IdentityToken != tt.want.OIDCResolve.IdentityToken {
				t.Errorf("IdentityToken = %q, want %q",
					got.OIDCResolve.IdentityToken, tt.want.OIDCResolve.IdentityToken)
			}
			if got.OIDCResolve.DeviceFlow != tt.want.OIDCResolve.DeviceFlow {
				t.Errorf("DeviceFlow = %v, want %v",
					got.OIDCResolve.DeviceFlow, tt.want.OIDCResolve.DeviceFlow)
			}
		})
	}
}
