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

package ctrf

import "strconv"

// TestResult.Extra keys that both ends of the result contract read: the
// validator images publish them (validators.EmitExtra) and the evidence
// readers consume them (pkg/evidence/cncf, pkg/evidence/redact). They live
// here, the one package every side already imports, so the publisher and the
// readers cannot drift apart; validators re-exports them as ExtraKey*.
const (
	// ExtraKeySkipReason carries the closed-set code of a skipped result.
	ExtraKeySkipReason = "skipReason"

	// ExtraKeyObservationOnly marks a passed result as an observation, not a
	// demonstration; the value is ExtraValueTrue.
	ExtraKeyObservationOnly = "observationOnly"

	// ExtraKeyResultSchema is the result-contract version the validator image
	// spoke, stamped on every result before the check runs. The value is one
	// of ResultSchemaVersions. A reader that knows a check to be
	// observation-only reads its unmarked pass as an observation whether or
	// not this key is present, and uses the key to tell whether the image
	// predates ExtraKeyObservationOnly or dropped the marker. The orchestrator
	// also records a result's image and platform lines only when
	// TrustsReportedImages accepts the key and the same stamp carries
	// ExtraKeyResultNonce.
	ExtraKeyResultSchema = "resultSchema"

	// ExtraKeyResultNonce carries, on the result-contract stamp, the nonce
	// the orchestrator gave the validator Job (v1.ResultNonceEnv). The
	// orchestrator strips it from every Extra line, so it is never published.
	ExtraKeyResultNonce = "resultNonce"

	// ExtraValueTrue is the published form of a true boolean extra.
	ExtraValueTrue = "true"

	// ResultSchemaVersion is the current value of ExtraKeyResultSchema. Bump
	// it when the meaning of a marker changes or a reader needs to tell one
	// image generation from another:
	//   - 1: every result carries this key; an observation-only pass carries
	//     ExtraKeyObservationOnly.
	//   - 2: the image breaks every ReservedLinePrefix, at any offset, in
	//     output written to os.Stdout and os.Stderr, so relayed output written
	//     there cannot supply its ImageLinePrefix and PlatformLinePrefix lines.
	ResultSchemaVersion = "2"
)

// ResultSchemaVersions returns every ExtraKeyResultSchema value this reader
// knows, oldest first.
func ResultSchemaVersions() []string {
	return []string{"1", ResultSchemaVersion}
}

// TrustsReportedImages reports whether a result stamped with schema, its
// ExtraKeyResultSchema value, came from an image that neutralizes relayed
// output. Its ImageLinePrefix and PlatformLinePrefix lines are recorded only
// when this holds and the same stamp carries the Job's ExtraKeyResultNonce,
// which a log rotated past the stamp lacks. It reports true for version 2,
// the first image generation that neutralizes relayed output, and for any
// later version. A value that is not a canonical decimal is not trusted.
func TrustsReportedImages(schema string) bool {
	v, err := strconv.Atoi(schema)
	return err == nil && v >= 2 && strconv.Itoa(v) == schema
}
