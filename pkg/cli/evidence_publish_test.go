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
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestEvidenceCmd_RegistersPublishSubcommand(t *testing.T) {
	cmd := evidenceCmd()
	found := false
	for _, sub := range cmd.Commands {
		if sub.Name == "publish" {
			found = true
		}
	}
	if !found {
		t.Errorf("evidence publish subcommand not registered")
	}
}

func TestEvidencePublishCmd_HasExpectedFlags(t *testing.T) {
	cmd := evidencePublishCmd()
	wanted := []string{
		"push", "plain-http", "insecure-tls", "identity-token", "oidc-device-flow",
	}
	for _, name := range wanted {
		found := false
		for _, f := range cmd.Flags {
			if f.Names()[0] == name {
				found = true
			}
		}
		if !found {
			t.Errorf("missing expected flag: --%s", name)
		}
	}
}

func TestEvidencePublishCmd_RejectsInvalidInvocations(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantSubstr string // case-specific reason embedded in the error
	}{
		{
			name:       "missing bundle dir",
			args:       []string{"--push", "ghcr.io/example/aicr-evidence"},
			wantSubstr: "bundle directory is required",
		},
		{
			name:       "missing push",
			args:       []string{t.TempDir()},
			wantSubstr: "--push <oci-ref> is required",
		},
		{
			// Valid arg + push, but the directory has no bundle markers:
			// the command must fail at bundle load, before any network work.
			name:       "directory is not a bundle",
			args:       []string{t.TempDir(), "--push", "ghcr.io/example/aicr-evidence"},
			wantSubstr: "does not look like a summary bundle",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newRootCmd()
			var out bytes.Buffer
			root.Writer = &out
			root.ErrWriter = &out
			argv := append([]string{"aicr", "evidence", "publish"}, tt.args...)
			err := root.Run(context.Background(), argv)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			// Every rejection here is a malformed invocation → invalid-request.
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}
