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

package verifier

import (
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// InputForm enumerates supported bundle transport shapes. Only
// InputFormDir is implemented; pointer and OCI forms are reserved.
type InputForm string

const (
	InputFormDir InputForm = "dir"
)

// StepStatus is the per-step verdict.
type StepStatus string

const (
	StepPassed        StepStatus = "passed"
	StepFailed        StepStatus = "failed"
	StepSkipped       StepStatus = "skipped"
	StepInformational StepStatus = "informational"
)

// Exit codes returned by Verify in VerifyResult.Exit. These are the
// library-API codes from ADR-007; the CLI maps them to OS exit codes
// via pkg/errors error codes.
const (
	ExitValidPassed        = 0
	ExitValidPhaseFailures = 1
	ExitInvalid            = 2
)

// VerifyOptions configures one Verify run.
type VerifyOptions struct {
	Input string
}

// StepResult is the recorded outcome of one verification step.
type StepResult struct {
	Step    int        `json:"step" yaml:"step"`
	Name    string     `json:"name" yaml:"name"`
	Status  StepStatus `json:"status" yaml:"status"`
	Detail  string     `json:"detail,omitempty" yaml:"detail,omitempty"`
	SubRows []KV       `json:"subRows,omitempty" yaml:"subRows,omitempty"`
}

// KV is a flat key-value pair for StepResult.SubRows.
type KV struct {
	Key   string `json:"key" yaml:"key"`
	Value string `json:"value" yaml:"value"`
}

// VerifyResult is what Verify returns to its caller.
type VerifyResult struct {
	Input      InputForm              `json:"input" yaml:"input"`
	Predicate  *attestation.Predicate `json:"predicate,omitempty" yaml:"predicate,omitempty"`
	RecipeName string                 `json:"recipeName,omitempty" yaml:"recipeName,omitempty"`
	Steps      []StepResult           `json:"steps" yaml:"steps"`
	Exit       int                    `json:"exit" yaml:"exit"`
}
