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

package aicr

import (
	"context"
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
	evidenceindex "github.com/NVIDIA/aicr/pkg/evidence/index"
)

// PublishedEvidenceRequired reports whether this call should reject a resolved
// recipe that has no committed evidence pointer.
//
// raw is a query spelling. 1, true, yes, and on enable the check. 0, false,
// no, off, and empty do not. Any other non-empty spelling is
// ErrCodeInvalidRequest. requested is the caller's flag or config, already
// reduced to a bool. AICR_REQUIRE_EVIDENCE is OR-ed in. A false raw value or
// a false requested flag does not turn that environment off.
func PublishedEvidenceRequired(raw string, requested bool) (bool, error) {
	enabled, ok := evidenceindex.ParseToggle(raw)
	if !ok {
		return false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("requireEvidence value %q is not one of 1, true, yes, on, 0, false, no, off", raw))
	}
	if enabled || requested {
		return true, nil
	}
	return evidenceindex.EnabledFromEnv(), nil
}

// RequirePublishedEvidence rejects result when the resolved leaf has no
// committed evidence pointer. It reads the embedded pointer index only: no
// signature check and no registry fetch. A pointer on the unprofiled id does
// not cover a profiled resolve, and a leaf whose catalog source is not the
// embedded catalog fails even when that id has a pointer.
//
// ResolveRecipe does not call this. Bundle, mirror, and upgrade stay
// unaffected unless their caller invokes it. Call LoadCatalog first; a
// catalog error is returned as-is.
func (c *Client) RequirePublishedEvidence(ctx context.Context, result *RecipeResult) error {
	if result == nil || result.Resolved() == nil {
		return errors.New(errors.ErrCodeInternal, "resolved recipe is missing")
	}
	resolved := result.Resolved()
	entries, err := c.ListCatalog(ctx, nil)
	if err != nil {
		return err
	}
	sources := make([]evidenceindex.CatalogSource, len(entries))
	for i := range entries {
		sources[i] = evidenceindex.CatalogSource{Name: entries[i].Name, Source: entries[i].Source}
	}
	return evidenceindex.Check(resolved, evidenceindex.LeafSource(resolved.Metadata.AppliedOverlays, sources))
}
