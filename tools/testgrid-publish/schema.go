// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package main

// Column metadata key schema — emitted in started.json and configured
// as column_header entries in tools/config-gen (aicr-testgrid repo).
//
// These constants define the single source of truth for the 7-key schema
// specified in docs/design/012-recipe-coordinate-mapping.md (PR #1409).
// Both the publish tool (here) and config-gen must use the same names;
// drift causes column headers to show "missing" in the TestGrid UI.
const (
	metaKeyAICRVersion    = "aicr_version"
	metaKeyK8sVersion     = "k8s_version"
	metaKeyK8sConstraint  = "k8s_constraint"
	metaKeySignerIdentity = "signer_identity"
	metaKeySignerIssuer   = "signer_issuer"
	metaKeySourceClass    = "source_class"
	metaKeyEvidenceDigest = "evidence_digest"
)

// MetaKeys returns all metadata keys in stable order.
// Used by contract tests to verify config-gen column headers match.
func MetaKeys() []string {
	return []string{
		metaKeyAICRVersion,
		metaKeyK8sVersion,
		metaKeyK8sConstraint,
		metaKeySignerIdentity,
		metaKeySignerIssuer,
		metaKeySourceClass,
		metaKeyEvidenceDigest,
	}
}

// Source class values.
const (
	sourceClassUAT       = "uat"
	sourceClassCommunity = "community"
)

// startedJSON is the started.json format TestGrid updater reads.
type startedJSON struct {
	Timestamp int64             `json:"timestamp"` // Unix seconds
	Metadata  map[string]string `json:"metadata"`
}

// finishedJSON is the finished.json format TestGrid updater reads.
type finishedJSON struct {
	Timestamp int64             `json:"timestamp"` // Unix seconds
	Passed    bool              `json:"passed"`
	Result    string            `json:"result"` // "SUCCESS" or "FAILURE"
	Metadata  map[string]string `json:"metadata"`
}
