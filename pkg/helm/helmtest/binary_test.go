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

package helmtest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// recordingTB records Skip and Fatal, and stops the calling goroutine as the
// real methods do.
type recordingTB struct {
	testing.TB
	skipped, failed bool
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Skip(...any) {
	r.skipped = true
	runtime.Goexit()
}

func (r *recordingTB) Fatal(...any) {
	r.failed = true
	runtime.Goexit()
}

func TestRequireBinary(t *testing.T) {
	withHelm := t.TempDir()
	if err := os.WriteFile(filepath.Join(withHelm, "helm"), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test needs an executable stub
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		path        string
		ci          string
		wantSkipped bool
		wantFailed  bool
	}{
		{"helm on PATH", withHelm, "true", false, false},
		{"missing locally", t.TempDir(), "", true, false},
		{"missing in CI", t.TempDir(), "true", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", tt.path)
			t.Setenv("CI", tt.ci)

			rec := &recordingTB{TB: t}
			done := make(chan struct{})
			go func() {
				defer close(done)
				RequireBinary(rec)
			}()
			<-done

			if rec.skipped != tt.wantSkipped || rec.failed != tt.wantFailed {
				t.Errorf("skipped=%v failed=%v, want skipped=%v failed=%v",
					rec.skipped, rec.failed, tt.wantSkipped, tt.wantFailed)
			}
		})
	}
}
