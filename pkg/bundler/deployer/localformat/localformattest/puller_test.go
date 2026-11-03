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

package localformattest

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
)

func TestStubPuller(t *testing.T) {
	tests := []struct {
		name      string
		component localformat.Component
		wantChart string
	}{
		{"chart name set", localformat.Component{Name: "gpu-operator", ChartName: "gpu-operator-chart", Version: "1.0.0"}, "gpu-operator-chart"},
		{"chart name falls back to component name", localformat.Component{Name: "gpu-operator", Version: "1.0.0"}, "gpu-operator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tgz, rec, tarball, err := StubPuller{}.Pull(t.Context(), tt.component)
			if err != nil {
				t.Fatalf("Pull() error = %v", err)
			}
			if string(tgz) != string(StubArchive(tt.component)) {
				t.Errorf("Pull() archive = %q, want StubArchive(c) = %q", tgz, StubArchive(tt.component))
			}
			sum := sha256.Sum256(tgz)
			if rec.SHA256 != hex.EncodeToString(sum[:]) {
				t.Errorf("record SHA256 = %q, want the archive's", rec.SHA256)
			}
			wantTarball := tt.wantChart + "-" + tt.component.Version + ".tgz"
			if rec.Chart != tt.wantChart || tarball != wantTarball || rec.TarballName != wantTarball {
				t.Errorf("chart = %q, tarball = %q, record tarball = %q, want %q and %q",
					rec.Chart, tarball, rec.TarballName, tt.wantChart, wantTarball)
			}
		})
	}
}
