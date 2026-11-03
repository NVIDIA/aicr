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
// on a host that cannot run the image's binary skips the check.
func TestRefuses(t *testing.T) {
	t.Parallel()
	const refused = "refuses development.skipDiscovery"
	hostOut := refused
	if _, ok := hostPlatforms[runtime.GOOS+"/"+runtime.GOARCH]; !ok {
		hostOut = "skipping the development settings refusal check"
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
		{"host platform", "", hostOut, "", both, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
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
