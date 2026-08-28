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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// LoadFromFileWithProvider loads a recipe from the given path bound to an explicit DataProvider.
// Overlay inputs (kind: RecipeMetadata) are hydrated through a builder bound to
// dp (so external --data overlays resolve against dp, not the embedded
// default), and the returned result carries dp via its provider field. A nil
// dp falls back to the embedded catalog.
//
// Overlay hydration applies the declaration's default profile selection;
// use LoadFromFileWithProviderProfile to hydrate with an explicit
// name=value selection.
func LoadFromFileWithProvider(ctx context.Context, path, kubeconfig, version string, dp DataProvider) (*RecipeResult, error) {
	return LoadFromFileWithProviderProfile(ctx, path, kubeconfig, version, dp, "")
}

// LoadFromFileWithProviderProfile is LoadFromFileWithProvider with an
// explicit name=value profile selection (same semantics as
// `aicr recipe --profile`). The selection applies only to overlay inputs
// (kind: RecipeMetadata), which are hydrated through the builder with the
// selection; an empty profile keeps the declaration's default. A hydrated
// RecipeResult input already carries its selection baked into
// metadata.selectedProfile, so combining it with a non-empty profile is
// rejected as invalid.
func LoadFromFileWithProviderProfile(
	ctx context.Context,
	path, kubeconfig, version string,
	dp DataProvider,
	profile string,
) (*RecipeResult, error) {

	// Validate the selection's wire form up front so a malformed --profile
	// fails with the profile-core error before any file I/O results are
	// misattributed to it.
	selection, err := ParseProfileSelection(profile)
	if err != nil {
		return nil, err
	}
	sourceData, sourceFormat, err := serializer.ReadFileBytesWithKubeconfigContext(
		ctx, path, kubeconfig,
	)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			fmt.Sprintf("failed to load recipe from %q", path))
	}
	sourceReader, err := serializer.NewReader(sourceFormat, bytes.NewReader(sourceData))
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to create recipe reader for %q", path))
	}
	var source RecipeResult
	if deserializeErr := sourceReader.Deserialize(&source); deserializeErr != nil {
		return nil, errors.PropagateOrWrap(deserializeErr, errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to load recipe from %q", path))
	}
	rec := &source

	// Capture the apiVersion declared in the source file before any overlay
	// hydration reassigns rec; otherwise an unsupported apiVersion on a
	// RecipeMetadata overlay input would slip past the gate below (the
	// hydrated RecipeResult always carries a supported version).
	inputAPIVersion := rec.APIVersion

	// Reject an artifact stamped with an apiVersion this build does not
	// understand before an overlay can trigger provider-backed hydration.
	// The accepted set is selected by wire kind/schema track; an empty value
	// remains tolerated for pre-apiVersion RecipeResult files only, and is
	// rejected for RecipeMetadata so this path agrees with the catalog scanner.
	if versionErr := validateRecipeInputAPIVersion(rec.Kind, inputAPIVersion); versionErr != nil {
		return nil, versionErr
	}

	// Users often pass overlay files directly; auto-hydrate so they don't need
	// a separate "aicr recipe" step before consuming the recipe.
	if rec.Kind == RecipeMetadataKind {
		slog.Info("input is a RecipeMetadata overlay; auto-hydrating via recipe builder",
			"file", path)

		var readerOpts []serializer.ReaderOption
		if header.IsSupportedProfileAPIVersion(inputAPIVersion) {
			readerOpts = append(readerOpts, serializer.WithStrict())
		}
		overlayReader, parseErr := serializer.NewReader(
			sourceFormat, bytes.NewReader(sourceData), readerOpts...,
		)
		if parseErr != nil {
			return nil, errors.PropagateOrWrap(parseErr, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to create overlay reader for %q", path))
		}
		var overlay RecipeMetadata
		if parseErr := overlayReader.Deserialize(&overlay); parseErr != nil {
			return nil, errors.PropagateOrWrap(parseErr, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to parse overlay from %q", path))
		}
		if profileErr := ValidateRecipeMetadataProfile(&overlay); profileErr != nil {
			return nil, profileErr
		}

		if overlay.Spec.Criteria == nil {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("overlay %q has no criteria; only leaf overlays (with spec.criteria) "+
					"can be used directly — run \"aicr recipe\" with explicit criteria instead",
					path))
		}

		// Bind the builder to dp when supplied so the overlay resolves
		// against the caller's provider; the hydrated result inherits dp
		// via the metadata store (finalizeRecipeResult threads it through).
		// A nil dp omits the option, so the builder falls back to the
		// embedded catalog.
		opts := []Option{WithVersion(version)}
		if dp != nil {
			opts = append(opts, WithDataProvider(dp))
		}
		builder := NewBuilder(opts...)
		rec, err = builder.BuildFromCriteriaWithProfile(ctx, overlay.Spec.Criteria, profile)
		if err != nil {
			return nil, err
		}
		if profileErr := ensureDirectOverlayProfileApplied(ctx, path, &overlay, rec, dp, selection); profileErr != nil {
			return nil, profileErr
		}

		slog.Info("overlay hydrated successfully",
			"appliedOverlays", rec.Metadata.AppliedOverlays)
	} else {
		if profile != "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("recipe file %q is a hydrated RecipeResult whose profile selection "+
					"is already baked into metadata.selectedProfile; a profile selection applies "+
					"only to overlay inputs", path))
		}
		if header.IsSupportedProfileAPIVersion(inputAPIVersion) {
			rec, err = DecodeRecipeResult(sourceData, sourceFormat)
			if err != nil {
				return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
					fmt.Sprintf("failed to strictly decode profile recipe from %q", path))
			}
		}
		if dp != nil {
			// Already-hydrated RecipeResult: the builder never runs, so bind the
			// caller's provider directly so downstream value/manifest reads route
			// through dp rather than the embedded default.
			rec.provider = dp
		}
	}

	// The strict profile artifact version requires its discriminator. Empty
	// kind remains allowed only for legacy RecipeResult files that predate it.
	if rec.Kind == "" && header.IsSupportedProfileAPIVersion(inputAPIVersion) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("recipe file apiVersion %q requires kind %q",
				inputAPIVersion, RecipeResultKind))
	}
	if rec.Kind != "" && rec.Kind != RecipeResultKind {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("recipe file has kind %q, but %q is required; "+
				"run \"aicr recipe\" to generate a hydrated RecipeResult first",
				rec.Kind, RecipeResultKind))
	}

	// Back-fill missing types from the registry (this boundary does not run
	// ApplyRegistryDefaults), canonicalize case, then reject incoherent refs. A
	// hydrated RecipeResult read from disk bypasses finalizeRecipeResult, so
	// without this a hand-authored recipe with e.g. a Helm ref carrying a
	// Kustomize tag/path would reach the bundler/attestation unchecked, and a
	// lowercase type would deploy inconsistently — while a type-less registry
	// ref (valid before #1584) must still resolve, not be rejected. See #1584.
	if err := rec.prepareAndValidateWithSource(ctx, path); err != nil {
		return nil, err
	}

	return rec, nil
}

// validateRecipeInputAPIVersion gates a directly supplied recipe input by wire
// kind.
//
// RecipeMetadata is a catalog kind wherever it arrives from, so it is held to
// the same fail-closed authoring gate the catalog scanner applies in
// classifyRecipeMetadataCatalogHeader — including rejecting an empty value.
// ADR-022 §8 requires that for in-scope catalog kinds, and §3's "existing
// tolerances remain" clause does not reach a document the catalog path already
// rejects; the two paths disagreeing on the same bytes was the fail-open seam
// in #2421.
//
// The empty-value tolerance survives only for RecipeResult inputs, which
// genuinely predate the apiVersion field. ADR-022 §3 retires that at Release
// N+2 (#2417).
func validateRecipeInputAPIVersion(kind, apiVersion string) error {
	if kind == RecipeMetadataKind {
		if header.IsSupportedAuthoringAPIVersion(apiVersion) ||
			header.IsSupportedProfileAPIVersion(apiVersion) {

			return nil
		}
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("recipe metadata file has apiVersion %q, which this aicr build does not support (expected %q, %q, %q, or %q); "+
				"update the catalog header for this aicr release",
				apiVersion, RecipeMetadataAPIVersion, header.GroupVersionV1Beta1,
				RecipeProfileAPIVersion, header.GroupVersionV1Beta2))
	}

	if apiVersion == "" {
		return nil
	}

	if header.IsSupportedRecipeResultAPIVersion(apiVersion) {
		return nil
	}
	return errors.New(errors.ErrCodeInvalidRequest,
		fmt.Sprintf("recipe file has apiVersion %q, which this aicr build does not support (expected %q, %q, %q, or %q); "+
			"regenerate the recipe with a matching aicr version",
			apiVersion, RecipeResultAPIVersion, header.GroupVersionV1,
			RecipeProfileAPIVersion, header.GroupVersionV1Beta2))
}

func ensureDirectOverlayProfileApplied(
	ctx context.Context,
	path string,
	overlay *RecipeMetadata,
	result *RecipeResult,
	dp DataProvider,
	selection *ProfileSelection,
) error {

	if overlay == nil || overlay.Spec.Profile == nil {
		return nil
	}

	store, err := LoadMetadataStoreFor(ctx, dp)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			fmt.Sprintf("failed to verify profile declaration from directly loaded overlay %q", path))
	}
	var selected *SelectedProfile
	var effective *effectiveProfileDeclaration
	if result != nil {
		selected = result.Metadata.SelectedProfile
		effective, err = store.resolveAppliedProfileDeclaration(result.Metadata.AppliedOverlays)
		if err != nil {
			return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to resolve the profile applied for directly loaded overlay %q", path))
		}
	}
	declarationMatches := false
	if effective != nil {
		declarationMatches, err = profileDeclarationsEqual(effective.Declaration, overlay.Spec.Profile)
		if err != nil {
			return err
		}
	}
	// The expected recorded value is the declaration's default, unless the
	// caller supplied an explicit selection — then the hydrated result must
	// carry exactly that value.
	expectedValue := overlay.Spec.Profile.Default
	if selection != nil {
		expectedValue = selection.Value
	}
	if !declarationMatches ||
		selected == nil ||
		selected.Name != overlay.Spec.Profile.Name ||
		selected.Value != expectedValue {

		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("profile declaration from directly loaded overlay %q was not applied; "+
				"add a structurally matching declaration to the active catalog before loading it directly", path))
	}
	return nil
}

func profileDeclarationsEqual(first, second *ProfileDeclaration) (bool, error) {
	firstJSON, err := json.Marshal(first)
	if err != nil {
		return false, errors.Wrap(
			errors.ErrCodeInternal, "failed to canonicalize resolved profile declaration", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		return false, errors.Wrap(
			errors.ErrCodeInternal, "failed to canonicalize directly loaded profile declaration", err)
	}
	return bytes.Equal(firstJSON, secondJSON), nil
}
