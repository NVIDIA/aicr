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
	"encoding/json"
	"fmt"
	"io"
	"os"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// extrasOut is the destination for EmitExtra's sentinel line. It is stdout in
// production (the pod-boundary transport the orchestrator reads back); tests
// redirect it to capture the emitted line.
var extrasOut io.Writer = os.Stdout

// EmitExtra marshals a check's structured, low-cardinality outcome data to a
// single JSON line prefixed with ctrf.ExtraLinePrefix and writes it to stdout.
// The orchestrator (pkg/validator/job.ExtractResult) parses this line back into
// ctrf.TestResult.Extra, which — unlike the free-form Stdout/Message evidence —
// survives the default "minimal" redaction policy for allowlisted keys.
//
// CONTRACT: values MUST be counts or enum codes only (e.g. "2", "no-gpu-nodes"),
// never node names, IPs, or hostnames. Anything operator-identifying belongs in
// fmt.Printf stdout, which is redacted by default. Emitting an empty map is a
// no-op. Key order in the JSON is not significant — the line is re-parsed
// structurally, and only redact-allowlisted keys are published.
func EmitExtra(extra map[string]string) error {
	if len(extra) == 0 {
		return nil
	}
	data, err := json.Marshal(extra)
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to marshal validator extra", err)
	}
	if _, err := fmt.Fprintln(extrasOut, ctrf.ExtraLinePrefix+string(data)); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to write validator extra", err)
	}
	return nil
}
