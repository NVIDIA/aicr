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

// Deployment validation.
const (
	// ValidationPhase is the only phase the operator runs; conformance and
	// performance take GPUs and run in lane qualification.
	ValidationPhase = "deployment"
	// EvidenceDir is where a validation run writes its attestation bundle.
	EvidenceDir = ScratchPath + "/evidence"
	// validationReport keeps the CTRF report out of the log, so a failed
	// run's log tail ends with AICR's error.
	validationReport = ScratchPath + "/report.json"
)

// Validation configures one validation run.
type Validation struct {
	AICRVersion string
	// Evidence is the oci:// repository and run tag AICR pushes the
	// evidence bundle to.
	Evidence  string
	PlainHTTP bool
	// ValidatorServiceAccount is the cluster-admin identity AICR v1.1.0 and
	// later run validators as.
	ValidatorServiceAccount string
	// SigningKey is the KMS key URI AICR v1.1.0 and later sign the evidence
	// with; empty pushes it unsigned.
	SigningKey string
	// TransparencyLog uploads a key signature to the Rekor transparency log.
	TransparencyLog bool
}

// ValidationArgs are the validate arguments over a run's recipe and
// snapshot copies. The evidence is signed with the KMS key when one is set
// and AICR supports it, and pushed unsigned otherwise; AICR v1.1.0 and later
// report its digest through the termination message. The exit status fails
// on any failed check; the operator never passes
// --allow-mutable-validator-tags, so a mutable validator image fails the run.
func ValidationArgs(r *Request, v *Validation) []string {
	args := []string{
		"--recipe", InputURI(r, InputTo),
		"--snapshot", InputURI(r, InputSnapshot),
		"--namespace", r.Namespaces.Validation,
		"--phase", ValidationPhase,
		"--fail-on-error=true",
		"--output", validationReport,
		"--emit-attestation", EvidenceDir,
		"--push", v.Evidence,
	}
	switch {
	case v.SigningKey == "" || !SupportsV11(v.AICRVersion):
		args = append(args, "--no-sign")
	case v.TransparencyLog:
		args = append(args, "--signing-key", v.SigningKey)
	default:
		args = append(args, "--signing-key", v.SigningKey, "--tlog-upload=false")
	}
	if v.PlainHTTP {
		args = append(args, "--plain-http")
	}
	if SupportsV11(v.AICRVersion) {
		args = append(args, "--validator-service-account-name", v.ValidatorServiceAccount)
	}
	return append(args, ImageRefsArgs(v.AICRVersion)...)
}
