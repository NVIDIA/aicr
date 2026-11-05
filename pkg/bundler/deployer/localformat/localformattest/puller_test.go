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
		name       string
		puller     StubPuller
		component  localformat.Component
		wantChart  string
		wantRecord string
	}{
		{"chart name set", StubPuller{}, localformat.Component{Name: "gpu-operator", ChartName: "gpu-operator-chart", Version: "1.0.0"}, "gpu-operator-chart", ""},
		{"chart name falls back to component name", StubPuller{}, localformat.Component{Name: "gpu-operator", Version: "1.0.0"}, "gpu-operator", ""},
		{"record SHA256 set", StubPuller{RecordSHA256: "not-the-archive"}, localformat.Component{Name: "gpu-operator", Version: "1.0.0"}, "gpu-operator", "not-the-archive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tgz, rec, tarball, err := tt.puller.Pull(t.Context(), tt.component)
			if err != nil {
				t.Fatalf("Pull() error = %v", err)
			}
			if string(tgz) != string(StubArchive(tt.component)) {
				t.Errorf("Pull() archive = %q, want StubArchive(c) = %q", tgz, StubArchive(tt.component))
			}
			sum := sha256.Sum256(tgz)
			wantRecord := tt.wantRecord
			if wantRecord == "" {
				wantRecord = hex.EncodeToString(sum[:])
			}
			if rec.SHA256 != wantRecord {
				t.Errorf("record SHA256 = %q, want %q", rec.SHA256, wantRecord)
			}
			wantTarball := tt.wantChart + "-" + tt.component.Version + ".tgz"
			if rec.Chart != tt.wantChart || tarball != wantTarball || rec.TarballName != wantTarball {
				t.Errorf("chart = %q, tarball = %q, record tarball = %q, want %q and %q",
					rec.Chart, tarball, rec.TarballName, tt.wantChart, wantTarball)
			}
		})
	}
}
