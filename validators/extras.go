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

// extrasOut is the destination for the sentinel lines. Nil means the CURRENT
// os.Stdout (the pod-boundary transport the orchestrator reads back), resolved
// at write time so a test that swaps os.Stdout for a pipe captures the line;
// tests in this package set it directly instead.
var extrasOut io.Writer

func sentinelOut() io.Writer {
	if extrasOut != nil {
		return extrasOut
	}
	return os.Stdout
}

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
	return emitSentinel(ctrf.ExtraLinePrefix, extra, len(extra) == 0, "extra")
}

// EmitRuntimeProvenance writes a derived runtime's RuntimeProvenance record as
// a single JSON line prefixed with ctrf.ProvenanceLinePrefix. The orchestrator
// parses it into ctrf.TestResult.RuntimeProvenance, which survives minimal
// redaction under the bounded rules in pkg/evidence/redact (sha256 digests,
// template KEYS only — never values). A nil record is a no-op.
func EmitRuntimeProvenance(p *ctrf.RuntimeProvenance) error {
	return emitSentinel(ctrf.ProvenanceLinePrefix, p, p == nil, "runtime provenance")
}

func emitSentinel(prefix string, payload any, empty bool, what string) error {
	if empty {
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to marshal validator "+what, err)
	}
	if _, err := fmt.Fprintln(sentinelOut(), prefix+string(data)); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to write validator "+what, err)
	}
	return nil
}
