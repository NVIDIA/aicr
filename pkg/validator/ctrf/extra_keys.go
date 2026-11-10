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
	// spoke, stamped on every result before the check runs; the value is
	// ResultSchemaVersion. A reader that knows a check to be observation-only
	// reads its unmarked pass as an observation whether or not this key is
	// present; the key only tells the reader whether the image predates
	// ExtraKeyObservationOnly or dropped the marker.
	ExtraKeyResultSchema = "resultSchema"

	// ExtraValueTrue is the published form of a true boolean extra.
	ExtraValueTrue = "true"

	// ResultSchemaVersion is the current value of ExtraKeyResultSchema. Bump
	// it when the meaning of a marker changes or a reader needs to tell one
	// image generation from another:
	//   - 1: every result carries this key; an observation-only pass carries
	//     ExtraKeyObservationOnly.
	ResultSchemaVersion = "1"
)
