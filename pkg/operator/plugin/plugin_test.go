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

package plugin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Command lines the plugin cannot run fail before any API call, with usage.
func TestRunRejectsBadCommandLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "a command is required"},
		{[]string{"delete", "rollout-x"}, "unknown command delete"},
		{[]string{"approve"}, "approve needs a rollout name"},
		{[]string{"approve", "rollout-x"}, "approve needs --reason"},
		{[]string{"revoke", "--reason", "x"}, "revoke needs a rollout name"},
		{[]string{"approve", "rollout-x", "--reason", "r", "--expires", "-1h"}, "--expires must not be negative"},
		{[]string{"approve", "rollout-x", "--reason", "r", "extra"}, "unexpected arguments after rollout-x"},
		{[]string{"approve", "rollout-x", "--username", "bob"}, "flag provided but not defined: -username"},
	} {
		err := Run(context.Background(), tc.args, &bytes.Buffer{})
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Run(%q) = %v, want usage naming %q", tc.args, err, tc.want)
		}
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"help"}, &out); err != nil || !strings.Contains(out.String(), "kubectl aicr COMMAND") {
		t.Errorf("help = %q, %v", out.String(), err)
	}
}

// A kubeconfig that cannot be loaded is an error naming it, not a panic.
func TestRunNeedsAKubeconfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("not: [a kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), []string{"describe", "rollout-x", "--kubeconfig", path}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "load kubeconfig") {
		t.Fatalf("Run() = %v", err)
	}
}
