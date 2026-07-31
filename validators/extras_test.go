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

package validators

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"io"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// errWriter always fails, to exercise EmitExtra's write-error branch.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestEmitExtra(t *testing.T) {
	tests := []struct {
		name       string
		extra      map[string]string
		wantLine   bool // whether a sentinel line is expected
		wantParsed map[string]string
	}{
		{
			name:       "coverage counts",
			extra:      map[string]string{"nodesValidated": "1", "nodesTotal": "2"},
			wantLine:   true,
			wantParsed: map[string]string{"nodesValidated": "1", "nodesTotal": "2"},
		},
		{
			name:       "skip reason",
			extra:      map[string]string{"skipReason": "no-gpu-nodes"},
			wantLine:   true,
			wantParsed: map[string]string{"skipReason": "no-gpu-nodes"},
		},
		{
			name:     "empty map is a no-op",
			extra:    map[string]string{},
			wantLine: false,
		},
		{
			name:     "nil map is a no-op",
			extra:    nil,
			wantLine: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := extrasOut
			extrasOut = &buf
			defer func() { extrasOut = orig }()

			if err := EmitExtra(tt.extra); err != nil {
				t.Fatalf("EmitExtra() error = %v", err)
			}

			out := buf.String()
			if !tt.wantLine {
				if out != "" {
					t.Errorf("expected no output, got %q", out)
				}
				return
			}

			if !strings.HasPrefix(out, ctrf.ExtraLinePrefix) {
				t.Fatalf("output %q missing prefix %q", out, ctrf.ExtraLinePrefix)
			}
			if !strings.HasSuffix(out, "\n") {
				t.Errorf("output %q must end in newline (single line contract)", out)
			}
			payload := strings.TrimSuffix(strings.TrimPrefix(out, ctrf.ExtraLinePrefix), "\n")
			var got map[string]string
			if err := json.Unmarshal([]byte(payload), &got); err != nil {
				t.Fatalf("payload %q is not valid JSON: %v", payload, err)
			}
			if len(got) != len(tt.wantParsed) {
				t.Fatalf("parsed %v, want %v", got, tt.wantParsed)
			}
			for k, v := range tt.wantParsed {
				if got[k] != v {
					t.Errorf("parsed[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestEmitExtraWriteError covers the sink-write failure branch: a failed write
// must surface a wrapped ErrCodeInternal (which emitExtraOrWarn callers then
// swallow so a check's verdict is never flipped by a stdout write error).
func TestEmitExtraWriteError(t *testing.T) {
	orig := extrasOut
	extrasOut = errWriter{}
	defer func() { extrasOut = orig }()

	err := EmitExtra(map[string]string{"nodesValidated": "1"})
	if err == nil {
		t.Fatal("EmitExtra() = nil, want error when the sink write fails")
	}
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
		t.Errorf("EmitExtra() error = %v, want ErrCodeInternal", err)
	}
}
