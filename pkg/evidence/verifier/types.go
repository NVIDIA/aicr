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

// InputForm enumerates supported bundle transport shapes.
type InputForm string

const (
	InputFormDir     InputForm = "dir"
	InputFormPointer InputForm = "pointer"
	InputFormOCI     InputForm = "oci"
)

// StepStatus is the per-step verdict.
type StepStatus string

const (
	StepPassed        StepStatus = "passed"
	StepFailed        StepStatus = "failed"
	StepSkipped       StepStatus = "skipped"
	StepInformational StepStatus = "informational"
)

// Exit codes returned by Verify in VerifyResult.Exit. The CLI maps
// these to OS exit codes via pkg/errors error codes.
const (
	ExitValidPassed        = 0
	ExitValidPhaseFailures = 1
	ExitInvalid            = 2
)

// VerifyOptions configures one Verify run.
type VerifyOptions struct {
	// Input is the user-supplied positional argument: pointer path,
	// OCI reference (with or without oci:// prefix), or unpacked
	// bundle directory. Required.
	Input string

	// BundleRef overrides the OCI reference when the input does not
	// embed one — e.g., a pointer file whose bundle.oci is empty.
	BundleRef string

	// ExpectedIssuer pins the OIDC issuer URL recorded on the signing
	// certificate. Empty allows any issuer.
	ExpectedIssuer string

	// ExpectedIdentityRegexp pins the signer's SubjectAlternativeName
	// via regex. Empty allows any identity.
	ExpectedIdentityRegexp string

	// PlainHTTP forces HTTP for registry traffic (local-registry tests
	// only).
	PlainHTTP bool

	// InsecureTLS disables TLS verification for the registry
	// (self-signed certificates).
	InsecureTLS bool

	// AllowUnpinnedTag opts into accepting OCI references that resolve
	// to a tag rather than a digest. By default the verifier refuses
	// unpinned refs because tags can be rewritten by the registry, so
	// "verify this artifact at this tag" is not content-addressable.
	// Pointer-driven flows ignore this flag when the pointer carries a
	// non-empty bundle.digest (the pointer's digest claim becomes the
	// pin and is cross-checked against the actual pulled digest).
	AllowUnpinnedTag bool
}

// SignerClaims records the OIDC identity from the signing certificate.
// nil on unsigned bundles.
type SignerClaims struct {
	Identity      string `json:"identity" yaml:"identity"`
	Issuer        string `json:"issuer" yaml:"issuer"`
	RekorLogIndex *int64 `json:"rekorLogIndex,omitempty" yaml:"rekorLogIndex,omitempty"`
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

// Failure-cause classes recorded in FailureCause.Class. Stable strings so
// the gate script (and future tooling) can branch on them without parsing
// human prose. They classify *why* a verification failed, complementing the
// coarse Exit code.
const (
	CauseRegistryForbidden = "registry-forbidden" // 401/403 pulling the bundle
	CauseNotFound          = "not-found"          // 404 — bundle/referrer absent at ref
	CauseRegistry          = "registry"           // other registry/transport failure
	CauseSignature         = "signature"          // signature/cert/identity verification failed
	CauseIntegrity         = "integrity"          // manifest hash-chain mismatch
	CauseSchema            = "schema"             // predicate parse / schema / type error
	CauseUnknown           = "unknown"            // unclassified
)

// FailureCause is the structured, machine-readable reason a verification
// produced a non-zero Exit. The gate renders Class/Hint into the PR comment
// so a contributor can self-serve the fix (e.g. a 403 → "make the fork's
// aicr-evidence package public") instead of seeing a bare "invalid".
type FailureCause struct {
	// Class is one of the Cause* constants — a stable identifier.
	Class string `json:"class" yaml:"class"`
	// Detail is the underlying error message (sanitized).
	Detail string `json:"detail" yaml:"detail"`
	// HTTPStatus is the registry HTTP status when the failure was a
	// registry response (0 otherwise).
	HTTPStatus int `json:"httpStatus,omitempty" yaml:"httpStatus,omitempty"`
	// Hint is an actionable, human-facing remediation when one is known.
	Hint string `json:"hint,omitempty" yaml:"hint,omitempty"`
}

// VerifyResult is what Verify returns to its caller.
type VerifyResult struct {
	Input        InputForm              `json:"input" yaml:"input"`
	Pointer      *attestation.Pointer   `json:"pointer,omitempty" yaml:"pointer,omitempty"`
	Predicate    *attestation.Predicate `json:"predicate,omitempty" yaml:"predicate,omitempty"`
	Signer       *SignerClaims          `json:"signer,omitempty" yaml:"signer,omitempty"`
	RecipeName   string                 `json:"recipeName,omitempty" yaml:"recipeName,omitempty"`
	BundleDigest string                 `json:"bundleDigest,omitempty" yaml:"bundleDigest,omitempty"`
	Steps        []StepResult           `json:"steps" yaml:"steps"`
	Exit         int                    `json:"exit" yaml:"exit"`

	// Pending is true when the bundle is unsigned — a "pending signature"
	// state, not a failure. An in-flight PR that committed an unsigned
	// pointer (via `--no-sign`) verifies with Exit 0 and Pending true, so
	// the gate can render "pending signature" instead of a misleading
	// "all checks passed" or a false "invalid".
	Pending bool `json:"pending,omitempty" yaml:"pending,omitempty"`

	// FailureCause classifies why the bundle was rejected. Set only when
	// Exit is ExitInvalid (2); nil for Exit 0 (valid, possibly pending) and
	// Exit 1 (valid bundle with recorded phase failures), which are not
	// bundle-invalid outcomes.
	FailureCause *FailureCause `json:"failureCause,omitempty" yaml:"failureCause,omitempty"`
}
