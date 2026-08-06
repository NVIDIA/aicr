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
	"context"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/evidence/internal/boundedio"
)

// pointerSizeCeiling caps the bytes the verifier will read from a
// pointer file. 1 MiB matches defaults.MaxRecipePOSTBytes. Pointers
// are tiny; anything past this is either a bug or hostile input.
var pointerSizeCeiling = defaults.MaxRecipePOSTBytes

// LoadAndValidatePointer reads and validates the pointer file at path.
// V1 enforces schema 1.0.x with exactly one attestation entry — schema
// 2.0 (multi-instance pointers) is reserved.
//
// Deprecated: prefer LoadAndValidatePointerContext. This form derives its own
// defaults.FileReadTimeout-bounded context, so it cannot be aborted by the
// caller; it is retained for source compatibility with the ctx-free evidence
// tree gate (pkg/evidence/verifier/discover.go and tools/evidence-pointercheck).
func LoadAndValidatePointer(path string) (*attestation.Pointer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	return LoadAndValidatePointerContext(ctx, path)
}

// LoadAndValidatePointerContext is LoadAndValidatePointer bounded by the
// caller's context.
func LoadAndValidatePointerContext(ctx context.Context, path string) (*attestation.Pointer, error) {
	body, err := boundedio.ReadFile(ctx, path, "pointer file", pointerSizeCeiling)
	if err != nil {
		return nil, err
	}

	var ptr attestation.Pointer
	if uErr := yaml.Unmarshal(body, &ptr); uErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "pointer file is not valid YAML", uErr)
	}
	if err := validatePointer(&ptr); err != nil {
		return nil, err
	}
	return &ptr, nil
}

func validatePointer(p *attestation.Pointer) error {
	if p == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "pointer is nil")
	}
	if !isSupportedPointerSchema(p.SchemaVersion) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"unsupported pointer schemaVersion "+p.SchemaVersion+" (verifier supports 1.0.x)")
	}
	if p.Recipe == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "pointer.recipe is required")
	}
	if p.Profile != "" {
		// A profiled pointer's recipe name must carry the profile path
		// segment (RecipeNameFor appends it for every profiled recipe).
		// Accepting an unsuffixed name would let two profile values share
		// one evidence directory, defeating per-value protection.
		sp, perr := attestation.ParsePointerProfile(p.Profile)
		if perr != nil {
			return perr
		}
		if !strings.HasSuffix(p.Recipe, "-"+sp) {
			return errors.New(errors.ErrCodeInvalidRequest,
				"pointer.recipe "+p.Recipe+" does not carry the profile path segment -"+sp+
					" required by pointer.profile "+p.Profile)
		}
	}
	switch len(p.Attestations) {
	case 0:
		return errors.New(errors.ErrCodeInvalidRequest,
			"pointer.attestations must have at least one entry")
	case 1:
		// expected
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			"pointer.attestations has multiple entries — schema 2.0 not yet supported")
	}
	att := p.Attestations[0]
	switch att.Bundle.PredicateType {
	case attestation.PredicateTypeV1, attestation.PredicateTypeV2:
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			"unsupported predicateType "+att.Bundle.PredicateType)
	}
	// Bidirectional pointer-level coherence: a profiled pointer must
	// reference v2 evidence, and an unprofiled one must not.
	if p.Profile != "" && att.Bundle.PredicateType != attestation.PredicateTypeV2 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"pointer records a profile selection but references "+att.Bundle.PredicateType+
				" evidence; profile-bearing recipes require "+attestation.PredicateTypeV2)
	}
	if p.Profile == "" && att.Bundle.PredicateType == attestation.PredicateTypeV2 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"pointer references "+attestation.PredicateTypeV2+" evidence without a profile selection")
	}
	if att.Bundle.OCI != "" && !strings.HasPrefix(att.Bundle.Digest, "sha256:") {
		return errors.New(errors.ErrCodeInvalidRequest,
			"pointer.attestations[0].bundle.digest must be sha256:<hex> when OCI is set")
	}
	if att.Signer != nil && (att.Signer.Identity == "" || att.Signer.Issuer == "") {
		return errors.New(errors.ErrCodeInvalidRequest,
			"pointer.attestations[0].signer requires identity and issuer when present")
	}
	return nil
}

func isSupportedPointerSchema(v string) bool {
	return strings.HasPrefix(v, "1.0.") || v == "1.0"
}
