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

package oci

import (
	"context"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

type recipeMaterializationAuthorization uint8

const (
	recipeMaterializationUnauthorized recipeMaterializationAuthorization = iota
	recipeMaterializationDigestAuthorized
)

// AuthorizeDigestMaterialization unlocks a staged artifact only when it was
// selected by the same immutable sha256 manifest digest resolved by the
// registry. Mutable tag selection can be staged, but cannot cross this gate.
// Repeated successful calls are idempotent.
func (a *StagedRecipeArtifact) AuthorizeDigestMaterialization(ctx context.Context) error {
	if a == nil {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"staged OCI recipe artifact is required")
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.OCIRecipePullTimeout)
	defer cancel()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return apperrors.New(apperrors.ErrCodeUnavailable,
			"staged OCI recipe artifact is closed")
	}
	if err := recipeContextError(ctx, "OCI recipe authorization canceled"); err != nil {
		return err
	}
	if a.authorization == recipeMaterializationDigestAuthorized {
		return nil
	}
	if a.selectorHash == "" || a.selectorHash != a.descriptor.Digest {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe materialization requires an immutable sha256 manifest selector")
	}
	a.authorization = recipeMaterializationDigestAuthorized
	return nil
}
