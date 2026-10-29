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

package aicrjob

import (
	"fmt"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// AICR CLI exit codes, frozen at v1 (NVIDIA/aicr pkg/errors/exitcode.go at
// v1.0.0, commit 82bccef).
const (
	ExitSuccess      = 0
	ExitError        = 1
	ExitInvalidInput = 2
	ExitNotFound     = 3
	ExitUnauthorized = 4
	ExitTimeout      = 5
	ExitUnavailable  = 6
	ExitRateLimited  = 7
	ExitInternal     = 8
	ExitCanceled     = 9
)

// ErrorFromExitCode maps a finished AICR container's exit code onto the
// operator's error codes: invalid input, not found and unauthorized
// are permanent; timeout, unavailable and rate limited are transient; internal
// and unclassified failures get bounded retries; canceled means shutdown.
// Exit codes outside AICR's range, such as a kill by signal or OOM, are
// internal: retrying the same input may fail the same way, so it is bounded.
func ErrorFromExitCode(stage Stage, exitCode int32, message string) error {
	code := operrors.ErrCodeInternal
	switch exitCode {
	case ExitSuccess:
		return nil
	case ExitInvalidInput:
		code = operrors.ErrCodeInvalidRequest
	case ExitNotFound:
		code = operrors.ErrCodeNotFound
	case ExitUnauthorized:
		code = operrors.ErrCodeUnauthorized
	case ExitTimeout:
		code = operrors.ErrCodeTimeout
	case ExitUnavailable:
		code = operrors.ErrCodeUnavailable
	case ExitRateLimited:
		code = operrors.ErrCodeRateLimitExceeded
	case ExitCanceled:
		code = operrors.ErrCodeCanceled
	case ExitError, ExitInternal:
		code = operrors.ErrCodeInternal
	}
	return operrors.New(code, fmt.Sprintf("aicr %s exited %d: %s", stage, exitCode, message)).
		WithContext(map[string]any{"stage": string(stage), "exitCode": exitCode})
}
