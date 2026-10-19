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

// Package errorstest provides test assertions on the error code a
// pkg/errors chain reports.
//
// Consumers such as errors.ExitCodeFromError read the code of the
// outermost *errors.StructuredError. errors.Is matches a code anywhere in
// the chain, so an assertion built on it still passes when the expected
// code is buried under a wrap that reports a different one. Use these
// helpers to assert what a consumer reads. Keep errors.Is for asserting
// that a code is absent, where matching anywhere in the chain is the
// stricter check.
package errorstest

import (
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ReportedCode returns the Code of the outermost *errors.StructuredError in
// err's Unwrap chain, or the empty code when the chain has none. It sees
// through non-structured wraps such as fmt.Errorf("%w", ...).
func ReportedCode(err error) errors.ErrorCode {
	se, ok := stderrors.AsType[*errors.StructuredError](err)
	if !ok {
		return ""
	}
	return se.Code
}

// WantReportedCode fails t unless err reports code.
func WantReportedCode(t testing.TB, err error, code errors.ErrorCode) {
	t.Helper()
	if got := ReportedCode(err); got != code {
		t.Errorf("reported code = %q, want %q: %v", got, code, err)
	}
}
