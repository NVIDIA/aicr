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

package recipe

import (
	"bytes"
	stderrors "errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"gopkg.in/yaml.v3"
)

// patchOverlayProfile adds the values of a profile-only external overlay to
// the embedded overlay at the same path and returns the embedded overlay
// re-serialized with the merged profile. The patch only adds values. It may
// not redeclare an embedded value or set the default or description, which
// stay with the declaring overlay.
//
// patched is false, with external returned unchanged, when external is not a
// profile patch. Reporting a malformed external file is left to the catalog
// loader, since re-marshaling would drop what the loader needs to see. Once
// external is a patch, every inconsistency with the embedded overlay is an
// error, and union totality is checked by the loader over the merged values.
func patchOverlayProfile(path string, embedded, external []byte) (merged []byte, patched bool, err error) {
	var patch RecipeMetadata
	decoder := yaml.NewDecoder(bytes.NewReader(external))
	decoder.KnownFields(true)
	if decoder.Decode(&patch) != nil {
		return external, false, nil
	}
	var trailing any
	if !stderrors.Is(decoder.Decode(&trailing), io.EOF) {
		return external, false, nil
	}
	// Zero-valuing the rest of the spec means a field added to it later makes
	// a file a replacement, never a patch that silently drops the new field.
	rest := patch.Spec
	rest.Profile = nil
	if patch.Spec.Profile == nil || !reflect.ValueOf(rest).IsZero() ||
		patch.Kind != RecipeMetadataKind || !header.IsSupportedProfileAPIVersion(patch.APIVersion) {

		return external, false, nil
	}

	var base RecipeMetadata
	if unmarshalErr := yaml.Unmarshal(embedded, &base); unmarshalErr != nil {
		return nil, false, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to parse embedded overlay %s", path), unmarshalErr)
	}
	declared, added := base.Spec.Profile, patch.Spec.Profile
	switch {
	case declared == nil:
		return nil, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("external %s is a profile-only patch, but the embedded overlay declares no profile to extend", path))
	case added.Name != declared.Name:
		return nil, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("external %s patches profile %q, but the embedded overlay declares %q",
				path, added.Name, declared.Name))
	case patch.Metadata.Name != "" && patch.Metadata.Name != base.Metadata.Name:
		return nil, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("external %s is named %q, but the embedded overlay at that path is %q",
				path, patch.Metadata.Name, base.Metadata.Name))
	case added.Default != "" || added.Description != "":
		return nil, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("external %s patches profile %q and may only add values, not set default or description",
				path, added.Name))
	case len(added.Values) == 0:
		return nil, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("external %s patches profile %q but adds no values", path, added.Name))
	}
	var redeclared []string
	for name := range added.Values {
		if _, exists := declared.Values[name]; exists {
			redeclared = append(redeclared, name)
		}
	}
	if len(redeclared) > 0 {
		slices.Sort(redeclared)
		return nil, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("external %s patches profile %q with values the embedded overlay already declares %v, "+
				"and a patch may only add values", path, added.Name, redeclared))
	}

	values := make(map[string]ProfileValue, len(declared.Values)+len(added.Values))
	maps.Copy(values, declared.Values)
	maps.Copy(values, added.Values)
	declared.Values = values

	merged, marshalErr := yaml.Marshal(&base)
	if marshalErr != nil {
		return nil, false, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to serialize patched overlay %s", path), marshalErr)
	}
	return merged, true, nil
}
