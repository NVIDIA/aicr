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

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
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
// over attestation.ResolveAttester. The OIDC source-precedence logic itself
// is exhaustively covered in the attestation package's resolver_test.go;
// here we only verify that selectAttester forwards CLI flags and the two
// ACTIONS_ID_TOKEN_REQUEST_* env vars into the resolver correctly.
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

	// Identity-token flag: the shim must pass identityToken through; resolver
	// returns a KeylessAttester synchronously without any network call.
	att, err = selectAttester(context.Background(), &bundleCmdOptions{
		attest:        true,
		identityToken: "pre-fetched-token",
	})
	if err != nil {
		t.Fatalf("selectAttester returned error: %v", err)
	}
	if _, ok := att.(*attestation.KeylessAttester); !ok {
		t.Errorf("expected *KeylessAttester, got %T", att)
	}
}
