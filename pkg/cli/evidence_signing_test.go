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

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// runSigningFlagsCmd parses args against flags and returns what action
// computed from the parsed command.
func runSigningFlagsCmd[T any](t *testing.T, flags []cli.Flag, args []string, action func(*cli.Command) (T, error)) (T, error) {
	t.Helper()
	var got T
	cmd := &cli.Command{
		Name:  "test",
		Flags: flags,
		Action: func(_ context.Context, c *cli.Command) error {
			var err error
			got, err = action(c)
			return err
		},
	}
	err := cmd.Run(context.Background(), append([]string{"test"}, args...))
	return got, err
}

func evidenceSigningTestFlags() []cli.Flag {
	return append([]cli.Flag{
		&cli.StringFlag{Name: flagIdentityToken},
		&cli.BoolFlag{Name: flagOIDCDeviceFlow},
		&cli.BoolFlag{Name: flagNoSign},
	}, evidenceSigningFlags()...)
}

func TestEvidenceSigningOptionsFromFlags(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")

	const key = "awskms:///alias/aicr-evidence"
	const signingConfigFixture = "../bundler/attestation/testdata/signing_config_v2.json"
	tests := []struct {
		name              string
		args              []string
		want              aicr.OIDCResolveOptions
		wantSigningConfig bool
		wantErr           bool
	}{
		{
			name: "defaults to keyless on Rekor v2",
			want: aicr.OIDCResolveOptions{UseTUFSigningConfig: true},
		},
		{
			name: "signing key is trimmed and keeps Rekor v2",
			args: []string{"--signing-key", "  " + key + "  "},
			want: aicr.OIDCResolveOptions{SigningKey: key, UseTUFSigningConfig: true},
		},
		{
			name: "tlog upload opt-out with a key",
			args: []string{"--signing-key", key, "--tlog-upload=false"},
			want: aicr.OIDCResolveOptions{SigningKey: key, DisableTLogUpload: true, UseTUFSigningConfig: true},
		},
		{
			name: "key with a private Rekor v1",
			args: []string{"--signing-key", key, "--rekor-url", "https://rekor.example.com"},
			want: aicr.OIDCResolveOptions{SigningKey: key, RekorURL: "https://rekor.example.com"},
		},
		{
			name:              "keyless with a private Fulcio and signing config",
			args:              []string{"--fulcio-url", "https://fulcio.example.com", "--signing-config", signingConfigFixture},
			want:              aicr.OIDCResolveOptions{FulcioURL: "https://fulcio.example.com", SigningConfigPath: signingConfigFixture},
			wantSigningConfig: true,
		},
		{name: "unreadable signing config", args: []string{"--signing-config", "/no/such/signing_config.json"}, wantErr: true},
		{
			name: "keyless token sources pass through",
			args: []string{"--identity-token", "tok", "--oidc-device-flow"},
			want: aicr.OIDCResolveOptions{IdentityToken: "tok", DeviceFlow: true, UseTUFSigningConfig: true},
		},
		{name: "blank signing key", args: []string{"--signing-key", "  "}, wantErr: true},
		{name: "local key file", args: []string{"--signing-key", "cosign.key"}, wantErr: true},
		{name: "key with identity token", args: []string{"--signing-key", key, "--identity-token", "tok"}, wantErr: true},
		{name: "key with device flow", args: []string{"--signing-key", key, "--oidc-device-flow"}, wantErr: true},
		{name: "key with fulcio url", args: []string{"--signing-key", key, "--fulcio-url", "https://fulcio.example.com"}, wantErr: true},
		{name: "rekor url with signing config", args: []string{"--rekor-url", "https://rekor.example.com", "--signing-config", "/tmp/sc.json"}, wantErr: true},
		{name: "plaintext rekor url", args: []string{"--rekor-url", "http://rekor.example.com"}, wantErr: true},
		{name: "plaintext fulcio url", args: []string{"--fulcio-url", "http://fulcio.example.com"}, wantErr: true},
		{name: "tlog opt-out without a key", args: []string{"--tlog-upload=false"}, wantErr: true},
		{name: "tlog opt-out with a rekor url", args: []string{"--signing-key", key, "--tlog-upload=false", "--rekor-url", "https://rekor.example.com"}, wantErr: true},
		{name: "tlog opt-out with a signing config", args: []string{"--signing-key", key, "--tlog-upload=false", "--signing-config", "/tmp/sc.json"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runSigningFlagsCmd(t, evidenceSigningTestFlags(), tt.args, evidenceSigningOptionsFromFlags)
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("evidenceSigningOptionsFromFlags() error = %v", err)
			}
			if got.PromptWriter == nil {
				t.Error("PromptWriter is nil, want stderr so device-flow prompts stay off stdout")
			}
			got.PromptWriter = nil
			if (got.SigningConfig != nil) != tt.wantSigningConfig {
				t.Errorf("SigningConfig parsed = %v, want %v", got.SigningConfig != nil, tt.wantSigningConfig)
			}
			got.SigningConfig = nil
			if got != tt.want {
				t.Errorf("evidenceSigningOptionsFromFlags() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestEvidenceSigningOptionsFromFlags_UnregisteredTLogFlagUploads pins that a
// command which never registers --tlog-upload signs to the transparency log:
// reading an absent bool flag as false would silently opt out.
func TestEvidenceSigningOptionsFromFlags_UnregisteredTLogFlagUploads(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: flagSigningKey}}
	got, err := runSigningFlagsCmd(t, flags, []string{"--signing-key", "awskms:///alias/k"}, evidenceSigningOptionsFromFlags)
	if err != nil {
		t.Fatalf("evidenceSigningOptionsFromFlags() error = %v", err)
	}
	if got.DisableTLogUpload {
		t.Error("DisableTLogUpload = true for a command without --tlog-upload, want false")
	}
}

func TestValidateNoSignWithoutSigningFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "no-sign alone", args: []string{"--no-sign"}},
		{name: "signing flags without no-sign", args: []string{"--signing-key", "awskms:///alias/k", "--tlog-upload=false"}},
		{name: "no-sign with signing key", args: []string{"--no-sign", "--signing-key", "awskms:///alias/k"}, wantErr: true},
		{name: "no-sign with tlog upload", args: []string{"--no-sign", "--tlog-upload=false"}, wantErr: true},
		{name: "no-sign with fulcio url", args: []string{"--no-sign", "--fulcio-url", "https://fulcio.example.com"}, wantErr: true},
		{name: "no-sign with rekor url", args: []string{"--no-sign", "--rekor-url", "https://rekor.example.com"}, wantErr: true},
		{name: "no-sign with signing config", args: []string{"--no-sign", "--signing-config", "/tmp/sc.json"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runSigningFlagsCmd(t, evidenceSigningTestFlags(), tt.args, func(c *cli.Command) (struct{}, error) {
				return struct{}{}, validateNoSignWithoutSigningFlags(c)
			})
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Errorf("validateNoSignWithoutSigningFlags() error = %v", err)
			}
		})
	}
}
