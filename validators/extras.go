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

// EmitExtra writes extra to stdout as one ctrf.ExtraLinePrefix line, which the
// orchestrator parses into ctrf.TestResult.Extra. Keys are the ExtraKey*
// constants, and values must take the shapes documented on
// ctrf.TestResult.Extra. Calls merge, a later value winning per key. An empty
// extra writes nothing.
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
