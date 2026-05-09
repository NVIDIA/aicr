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

package attestation

import (
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// PointerInputs carries everything the pointer file needs that is not
// already in the Bundle. Push results (when available) populate the
// bundle and signer fields; tests can pass empties to capture the
// pre-push form.
type PointerInputs struct {
	Bundle     *Bundle
	BundleOCI  string
	BundleHash string
	Signer     PointerSigner
	LogsBundle *PointerLogsBundle
}

// BuildPointer assembles the pointer YAML schema 1.0 from a built
// bundle plus the OCI/signer claims gathered after push and sign.
// When the bundle has not been pushed (BundleOCI/BundleHash empty),
// those fields are emitted as empty strings; consumers know "not yet
// published."
func BuildPointer(in PointerInputs) (*Pointer, error) {
	if in.Bundle == nil || in.Bundle.Predicate == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle and predicate are required")
	}

	pred := in.Bundle.Predicate
	att := PointerAttestation{
		Bundle: PointerBundle{
			OCI:           in.BundleOCI,
			Digest:        in.BundleHash,
			PredicateType: PredicateTypeV1,
		},
		Signer:        in.Signer,
		AttestedAt:    pred.AttestedAt.UTC().Truncate(time.Second),
		Fingerprint:   pointerFingerprintFrom(pred.Fingerprint),
		CriteriaMatch: PointerCriteriaMatch{Matched: pred.CriteriaMatch.Matched},
		PhaseSummary:  pointerPhaseSummaryFrom(pred.Phases),
		LogsBundle:    in.LogsBundle,
	}

	return &Pointer{
		SchemaVersion: PointerSchemaVersion,
		Recipe:        in.Bundle.RecipeName,
		Attestations:  []PointerAttestation{att},
	}, nil
}

// MarshalPointer renders a pointer as YAML with deterministic output
// (sorted keys via yaml.v3 default behavior, trailing newline).
func MarshalPointer(p *Pointer) ([]byte, error) {
	if p == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "pointer is required")
	}
	body, err := yaml.Marshal(p)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal pointer", err)
	}
	return body, nil
}

// WritePointer writes the pointer file to outputDir/pointer.yaml.
// The expected workflow: contributor copies this file into
// recipes/evidence/<recipe>.yaml in their PR.
func WritePointer(outputDir string, p *Pointer) (string, error) {
	body, err := MarshalPointer(p)
	if err != nil {
		return "", err
	}
	out := filepath.Join(outputDir, PointerFilename)
	if err := os.WriteFile(out, body, 0o600); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to write pointer file", err)
	}
	return out, nil
}

func pointerFingerprintFrom(fp FingerprintBlock) PointerFingerprint {
	out := PointerFingerprint{}
	if fp.Service != nil {
		out.Service = fp.Service.Value
	}
	if fp.Accelerator != nil {
		out.Accelerator = fp.Accelerator.Value
	}
	if fp.OS != nil {
		out.OS = fp.OS.Value
	}
	if fp.K8sVersion != nil {
		out.K8sVersion = fp.K8sVersion.Value
	}
	if fp.Intent != nil {
		out.Intent = fp.Intent.Value
	}
	if fp.Platform != nil {
		out.Platform = fp.Platform.Value
	}
	return out
}

func pointerPhaseSummaryFrom(phases map[Phase]PhaseSummary) map[Phase]PointerPhaseStat {
	out := map[Phase]PointerPhaseStat{}
	for p, s := range phases {
		out[p] = PointerPhaseStat{Passed: s.Passed, Failed: s.Failed}
	}
	return out
}
