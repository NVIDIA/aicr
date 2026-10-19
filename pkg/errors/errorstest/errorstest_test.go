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

package errorstest

import (
	stderrors "errors"
	"fmt"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestReportedCode(t *testing.T) {
	unavailable := errors.New(errors.ErrCodeUnavailable, "apiserver is down")

	tests := []struct {
		name string
		err  error
		want errors.ErrorCode
	}{
		{"nil", nil, ""},
		{"unstructured", stderrors.New("plain"), ""},
		{"structured", unavailable, errors.ErrCodeUnavailable},
		{"outermost wins over a buried code", errors.Wrap(errors.ErrCodeInternal, "flattened", unavailable), errors.ErrCodeInternal},
		{"sees through fmt.Errorf", fmt.Errorf("context: %w", unavailable), errors.ErrCodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReportedCode(tt.err); got != tt.want {
				t.Errorf("ReportedCode() = %q, want %q", got, tt.want)
			}
		})
	}
}

type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Helper()               {}
func (r *recordingTB) Errorf(string, ...any) { r.failed = true }

func TestWantReportedCode(t *testing.T) {
	buried := errors.Wrap(errors.ErrCodeInternal, "flattened", errors.New(errors.ErrCodeUnavailable, "down"))

	// errors.Is matches the buried code, so it cannot assert the reported one.
	if !stderrors.Is(buried, errors.New(errors.ErrCodeUnavailable, "")) {
		t.Fatal("precondition: errors.Is should match a buried code")
	}

	rec := &recordingTB{TB: t}
	WantReportedCode(rec, buried, errors.ErrCodeUnavailable)
	if !rec.failed {
		t.Error("WantReportedCode passed on a code buried under a different outer code")
	}

	rec = &recordingTB{TB: t}
	WantReportedCode(rec, buried, errors.ErrCodeInternal)
	if rec.failed {
		t.Error("WantReportedCode failed on the outermost code")
	}
}
