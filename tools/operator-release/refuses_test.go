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

package main

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// refusal returns a stand-in operator binary that refuses the development
// settings in refused. It starts with any other setting, or when a cluster is
// configured.
func refusal(refused ...string) []byte {
	var s strings.Builder
	s.WriteString("#!/bin/sh\ncase \"${KUBECONFIG}\" in */no-kubeconfig) ;; *) exit 0 ;; esac\n")
	s.WriteString("[ -z \"${KUBERNETES_SERVICE_HOST}\" ] || exit 0\ncase \"$1\" in\n")
	for _, d := range developmentSettings {
		for _, r := range refused {
			if r == d.name {
				s.WriteString(d.flag + ") echo \"[INVALID_REQUEST] development." + d.name + " is accepted only by development builds\" >&2; exit 1 ;;\n")
			}
		}
	}
	s.WriteString("esac\nexit 0\n")
	return []byte(s.String())
}

// TestRefuses checks that refuses passes only when the binary refuses every
// development setting, reports a coded error when the binary starts, fails
// for another reason or cannot run, or the image or settings are missing, and
// on a host that cannot run the image's binary skips the check outside CI and
// fails in CI.
//
// Not parallel: a case that extracts the binary writes it and then runs
// it, and a process another test forks while that file is open for writing
// holds a copy of the descriptor until its own exec, so this exec fails
// with ETXTBSY (golang/go#22315), as it did in the merge gate.
func TestRefuses(t *testing.T) {
	const refused = "refuses development.skipDiscovery"
	hostOut, hostWant, hostCode := refused, "", errors.ErrorCode("")
	if _, ok := hostPlatforms[runtime.GOOS+"/"+runtime.GOARCH]; !ok {
		if os.Getenv("CI") != "" {
			hostOut, hostWant, hostCode = "", "does not run on", errors.ErrCodeInvalidRequest
		} else {
			hostOut = "skipping the development settings refusal check"
		}
	}
	pinned := []string{"-platform", "linux/amd64"}
	both := refusal("bypassApprovals", "skipDiscovery")
	tests := []struct {
		name, want, out string
		code            errors.ErrorCode
		binary          []byte
		args            []string
	}{
		{"refuses both", "", refused, "", both, pinned},
		{"starts", "started with development.bypassApprovals", "", errors.ErrCodeInvalidRequest, refusal(), pinned},
		{"refuses only the bypass", "started with development.skipDiscovery", "", errors.ErrCodeInvalidRequest,
			refusal("bypassApprovals"), pinned},
		{"fails otherwise", "failed without refusing development.bypassApprovals", "", errors.ErrCodeInvalidRequest,
			[]byte("#!/bin/sh\necho 'load Kubernetes client config' >&2\nexit 1\n"), pinned},
		{"cannot run", "run the candidate with development.bypassApprovals", "", errors.ErrCodeInternal,
			[]byte("not an executable"), pinned},
		{"no image", "index.json", "", errors.ErrCodeInvalidRequest, nil, pinned},
		{"no settings", "read the pinned AICR image", "", errors.ErrCodeInvalidRequest, both,
			append([]string{"-settings", "missing.yaml"}, pinned...)},
		{"host platform", hostWant, hostOut, hostCode, both, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.binary != nil {
				writeLayout(t, dir, "image", "aicr-operator", tt.binary)
			}
			var out bytes.Buffer
			args := append([]string{"refuses", "-dir", dir, "-settings", "../../.settings.yaml"}, tt.args...)
			err := run(args, &out)
			if tt.want == "" {
				if err != nil || !strings.Contains(out.String(), tt.out) {
					t.Fatalf("refuses = %v, output %q, want output containing %q", err, out.String(), tt.out)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("refuses = %v, want an error naming %q", err, tt.want)
			}
			errorstest.WantReportedCode(t, err, tt.code)
		})
	}
}

// TestHostPlatform checks that hostPlatform maps a Linux host to its image
// platform, and that a host that cannot run the binary skips outside CI and
// fails in CI.
func TestHostPlatform(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, host, want string
		ci, wantErr      bool
	}{
		{"amd64", "linux/amd64", "linux/amd64", false, false},
		{"arm64 in CI", "linux/arm64", "linux/arm64/v8", true, false},
		{"mac skips", "darwin/arm64", "", false, false},
		{"mac in CI", "darwin/arm64", "", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := hostPlatform(tt.host, tt.ci)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("hostPlatform(%q, %v) = %q, %v, want %q, error %v", tt.host, tt.ci, got, err, tt.want, tt.wantErr)
			}
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}
