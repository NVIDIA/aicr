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
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// pointerSizeCeiling caps the bytes the verifier will read from a
// pointer file. 1 MiB matches defaults.MaxRecipePOSTBytes. Pointers
// are tiny; anything past this is either a bug or hostile input.
var pointerSizeCeiling = defaults.MaxRecipePOSTBytes

// LoadAndValidatePointer reads and validates the pointer file at path.
// V1 enforces schema 1.0.x with exactly one attestation entry — schema
// 2.0 (multi-instance pointers) is reserved.
func LoadAndValidatePointer(path string) (*attestation.Pointer, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to open pointer file", err)
	}
	defer func() { _ = f.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(f, pointerSizeCeiling+1))
	if readErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read pointer file", readErr)
	}
	if int64(len(body)) > pointerSizeCeiling {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"pointer file exceeds size limit (1 MiB)")
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
	if att.Bundle.PredicateType != attestation.PredicateTypeV1 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"unsupported predicateType "+att.Bundle.PredicateType)
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
