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
	"fmt"
	"maps"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/allocpolicy"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// RecipeProfileAPIVersion is the RecipeMetadata and RecipeResult version used
// when a configuration profile is present. Other AICR artifact kinds remain on
// header.GroupVersion.
const RecipeProfileAPIVersion = header.RecipeResultGroupVersion

const (
	profileComponentEnabledPath = "enabled"
	profileJSONSafeIntegerMax   = 1<<53 - 1
)

var profileIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ProfileDeclaration defines one overlay-scoped configuration choice.
type ProfileDeclaration struct {
	Name        string                  `json:"name" yaml:"name"`
	Description string                  `json:"description,omitempty" yaml:"description,omitempty"`
	Default     string                  `json:"default" yaml:"default"`
	Values      map[string]ProfileValue `json:"values" yaml:"values"`
}

// ProfileValue is the closed fragment applied for one declared value.
//
// Advertiser declares who advertises nvidia.com/gpu for this value. The
// vocabulary is closed (pkg/allocpolicy.ValidateAdvertiser): empty means
// the recipe's own components advertise (the AKS shape), and "external"
// (allocpolicy.AdvertiserExternal, the GKE gke-default shape) declares a
// provider-managed plugin outside the recipe as THE advertiser in the
// #1327 exactly-one invariant — the declaration is copied into
// metadata.selectedProfile.advertiser and extends the dual-advertisement
// gates fail-closed. Any other value is rejected.
type ProfileValue struct {
	Advertiser    string                `json:"advertiser,omitempty" yaml:"advertiser,omitempty"`
	Constraints   []Constraint          `json:"constraints,omitempty" yaml:"constraints,omitempty"`
	ComponentRefs []ProfileComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`
}

// ProfileComponentRef is deliberately smaller than ComponentRef. A profile
// may only assign values on an existing component; component identity,
// deployment shape, manifests, health checks, and ordering are not profile
// effects.
type ProfileComponentRef struct {
	Name      string         `json:"name" yaml:"name"`
	Overrides map[string]any `json:"overrides,omitempty" yaml:"overrides,omitempty"`
}

// SelectedProfile is persisted in a hydrated RecipeResult. OwnedPaths is the
// declaration-wide, deterministic lock surface keyed by canonical component
// name. The synthetic "enabled" path records component presence.
type SelectedProfile struct {
	Name       string              `json:"name" yaml:"name"`
	Value      string              `json:"value" yaml:"value"`
	Advertiser string              `json:"advertiser,omitempty" yaml:"advertiser,omitempty"`
	OwnedPaths map[string][]string `json:"ownedPaths" yaml:"ownedPaths"`
}

// OwnershipDomain identifies one closed configuration owner and the component
// value paths it controls. Paths use canonical component names and dot notation;
// the synthetic "enabled" path represents ownership of component presence.
type OwnershipDomain struct {
	Name  string
	Paths map[string][]string
}

// ProfileSummary is the compact catalog projection of an effective profile.
type ProfileSummary struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Default     string   `json:"default" yaml:"default"`
	Values      []string `json:"values" yaml:"values"`
}

// ProfileSelection is the parsed name=value selection shared by all public
// resolution surfaces.
type ProfileSelection struct {
	Name  string
	Value string
}

// ParseProfileSelection parses the canonical name=value profile selection.
// Empty input means "use the declaration's default".
func ParseProfileSelection(raw string) (*ProfileSelection, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // nil selection means use the declaration's default
	}
	if strings.TrimSpace(raw) != raw || strings.Count(raw, "=") != 1 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"profile must use the exact name=value form")
	}
	name, value, _ := strings.Cut(raw, "=")
	if !validProfileIdentifier(name) || !validProfileIdentifier(value) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"profile name and value must match [A-Za-z0-9._-]+")
	}
	return &ProfileSelection{Name: name, Value: value}, nil
}

func validProfileIdentifier(value string) bool {
	return profileIdentifierPattern.MatchString(value)
}

// caseUniqueValueNames returns the declaration's value names sorted,
// rejecting names that differ only by case: evidence and corroboration
// derive lowercase path segments from the selected value, so "Operator"
// and "operator" would collapse onto one evidence directory and overwrite
// each other's results.
func caseUniqueValueNames(decl *ProfileDeclaration) ([]string, error) {
	valueNames := make([]string, 0, len(decl.Values))
	lowered := make(map[string]string, len(decl.Values))
	for name := range decl.Values {
		valueNames = append(valueNames, name)
		lower := strings.ToLower(name)
		if prev, dup := lowered[lower]; dup {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile %q values %q and %q differ only by case; "+
					"evidence path segments are lowercase, so value names must be "+
					"case-insensitively unique", decl.Name, prev, name))
		}
		lowered[lower] = name
	}
	sort.Strings(valueNames)
	return valueNames, nil
}

// ValidateProfileDeclaration validates the closed v1 profile declaration and
// returns its declaration-wide ownership record.
func ValidateProfileDeclaration(decl *ProfileDeclaration) (map[string][]string, error) {
	if decl == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "profile declaration is required")
	}
	if !validProfileIdentifier(decl.Name) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"profile name must match [A-Za-z0-9._-]+")
	}
	if !validProfileIdentifier(decl.Default) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"profile default must match [A-Za-z0-9._-]+")
	}
	if len(decl.Values) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("profile %q must declare at least one value", decl.Name))
	}
	if _, ok := decl.Values[decl.Default]; !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("profile %q default %q is not a declared value", decl.Name, decl.Default))
	}

	valueNames, nameErr := caseUniqueValueNames(decl)
	if nameErr != nil {
		return nil, nameErr
	}

	var expected []string
	ownedSet := make(map[string]map[string]struct{})
	for i, valueName := range valueNames {
		if !validProfileIdentifier(valueName) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile value %q must match [A-Za-z0-9._-]+", valueName))
		}
		value := decl.Values[valueName]
		// ValidateAdvertiser already returns ErrCodeInvalidRequest naming
		// the offending value; propagate as-is rather than double-wrap.
		if err := allocpolicy.ValidateAdvertiser(value.Advertiser); err != nil {
			return nil, err
		}

		// Profile constraints reach the merged spec unchanged, and
		// BuildRecipeResultWithProfile — the snapshot-less generation path —
		// calls applyEffectiveProfile with a nil evaluator, so on that path
		// nothing downstream inspects their shape. Overlay and mixin
		// constraints already fail closed on an empty name or value
		// (validateConstraintWarningSource); catalog load is the equivalent
		// boundary for profile-contributed ones.
		seenConstraints := make(map[string]struct{}, len(value.Constraints))
		for _, constraint := range value.Constraints {
			if constraint.Name == "" {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile %q value %q declares a constraint with no name", decl.Name, valueName))
			}
			if constraint.Value == "" {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile %q value %q constraint %q has no value",
						decl.Name, valueName, constraint.Name))
			}
			if _, repeat := seenConstraints[constraint.Name]; repeat {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile %q value %q repeats constraint %q",
						decl.Name, valueName, constraint.Name))
			}
			seenConstraints[constraint.Name] = struct{}{}
		}

		seenComponents := make(map[string]struct{}, len(value.ComponentRefs))
		var valuePaths []string
		for _, ref := range value.ComponentRefs {
			if ref.Name == "" {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile %q value %q has a componentRef with an empty name", decl.Name, valueName))
			}
			if _, duplicate := seenComponents[ref.Name]; duplicate {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile %q value %q repeats componentRef %q", decl.Name, valueName, ref.Name))
			}
			seenComponents[ref.Name] = struct{}{}

			if _, assignsEnabled := ref.Overrides[profileComponentEnabledPath]; assignsEnabled {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile %q value %q component %q may not assign overrides.enabled",
						decl.Name, valueName, ref.Name))
			}
			paths, err := flattenProfileOverrides(ref.Overrides)
			if err != nil {
				return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
					"invalid profile overrides", err,
					map[string]any{"profile": decl.Name, "value": valueName, "component": ref.Name})
			}
			for _, path := range paths {
				valuePaths = append(valuePaths, ref.Name+":"+path)
				if ownedSet[ref.Name] == nil {
					ownedSet[ref.Name] = make(map[string]struct{})
				}
				ownedSet[ref.Name][path] = struct{}{}
			}
			if ownedSet[ref.Name] == nil {
				ownedSet[ref.Name] = make(map[string]struct{})
			}
			// The synthetic presence marker is declaration-wide and is
			// deliberately kept out of valuePaths: ADR-015 evaluates union
			// totality over the leaf-flattened override paths alone, "before
			// synthetic presence paths are added", and exempts the marker
			// because fragments may not assign it. Folding it in would compare
			// component-reference sets instead, rejecting a declaration whose
			// values legitimately differ in which components they reference.
			ownedSet[ref.Name][profileComponentEnabledPath] = struct{}{}
		}
		sort.Strings(valuePaths)
		if i == 0 {
			expected = valuePaths
			continue
		}
		if !slices.Equal(valuePaths, expected) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile %q violates union totality: value %q assigns %v, expected %v",
					decl.Name, valueName, valuePaths, expected))
		}
	}

	owned := make(map[string][]string, len(ownedSet))
	for component, paths := range ownedSet {
		owned[component] = make([]string, 0, len(paths))
		for path := range paths {
			owned[component] = append(owned[component], path)
		}
		sort.Strings(owned[component])
	}
	return owned, nil
}

func flattenProfileOverrides(overrides map[string]any) ([]string, error) {
	if err := validateProfileOverrideMapKeys(overrides, ""); err != nil {
		return nil, err
	}

	var paths []string
	var walk func(map[string]any, []string) error
	walk = func(values map[string]any, prefix []string) error {
		for key, value := range values {
			if key == "" || strings.Contains(key, ".") {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile override key %q must be nonempty and may not contain a literal dot", key))
			}
			path := append(append([]string(nil), prefix...), key)
			if nested, ok := value.(map[string]any); ok {
				if len(nested) == 0 {
					return errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("profile override %q assigns an empty map", strings.Join(path, ".")))
				}
				if err := walk(nested, path); err != nil {
					return err
				}
				continue
			}
			paths = append(paths, strings.Join(path, "."))
		}
		return nil
	}
	if err := walk(overrides, nil); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

type profileOverrideReference struct {
	kind     reflect.Kind
	pointer  uintptr
	length   int
	capacity int
}

func validateProfileOverrideMapKeys(value any, path string) error {
	return validateProfileOverrideTree(
		value,
		path,
		make(map[profileOverrideReference]struct{}),
		true,
	)
}

// validateProfileDeepCopyCycles mirrors serializer.DeepCopyAny: only
// map[string]any and []any recurse, so only those canonical containers can
// exhaust the stack during adoption or value hydration.
func validateProfileDeepCopyCycles(value any, path string) error {
	return validateProfileOverrideTree(
		value,
		path,
		make(map[profileOverrideReference]struct{}),
		false,
	)
}

func validateProfileOverrideTree(
	value any,
	path string,
	active map[profileOverrideReference]struct{},
	validateValues bool,
) error {

	reflected := reflect.ValueOf(value)
	if reference, ok := profileOverrideReferenceFor(reflected); ok {
		if _, exists := active[reference]; exists {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile override %q contains a cyclic reference", path))
		}
		active[reference] = struct{}{}
		defer delete(active, reference)
	}

	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			nestedPath := key
			if path != "" {
				nestedPath = path + "." + key
			}
			if err := validateProfileOverrideTree(nested, nestedPath, active, validateValues); err != nil {
				return err
			}
		}
	case map[any]any:
		if !validateValues {
			return nil
		}
		for key := range typed {
			if _, ok := key.(string); !ok {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile override %q contains non-string mapping key %v", path, key))
			}
		}
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("profile override %q must use a string-keyed mapping", path))
	case []any:
		for index, nested := range typed {
			nestedPath := fmt.Sprintf("%s[%d]", path, index)
			if err := validateProfileOverrideTree(nested, nestedPath, active, validateValues); err != nil {
				return err
			}
		}
	default:
		if !validateValues {
			return nil
		}
		if !reflected.IsValid() {
			return nil
		}
		if reflected.Kind() == reflect.Map {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile override %q uses unsupported map type %T; "+
					"nested mappings must use map[string]any", path, value))
		}
		if reflected.Kind() == reflect.Pointer {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile override %q uses unsupported pointer type %T", path, value))
		}
		if reflected.Kind() == reflect.Array || reflected.Kind() == reflect.Slice {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile override %q uses unsupported list type %T; "+
					"nested lists must use []any", path, value))
		}
		return validateProfileOverrideScalar(value, path)
	}
	return nil
}

func validateProfileOverrideScalar(value any, path string) error {
	switch typed := value.(type) {
	case bool, string:
		return nil
	case int, int8, int16, int32, int64:
		integer := reflect.ValueOf(typed).Int()
		if integer < -profileJSONSafeIntegerMax || integer > profileJSONSafeIntegerMax {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile override %q contains integer %v outside the JSON round-trip-safe range", path, typed))
		}
		return nil
	case uint, uint8, uint16, uint32, uint64, uintptr:
		if reflect.ValueOf(typed).Uint() > profileJSONSafeIntegerMax {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile override %q contains integer %v outside the JSON round-trip-safe range", path, typed))
		}
		return nil
	case float32:
		return validateProfileOverrideFloat(float64(typed), path)
	case float64:
		return validateProfileOverrideFloat(typed, path)
	case time.Time:
		// yaml.v3 resolves unquoted timestamps into time.Time when decoding
		// into any, so retain this decoder-reachable scalar.
		if _, err := typed.MarshalJSON(); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile override %q contains an invalid timestamp", path), err)
		}
		return nil
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("profile override %q uses unsupported scalar type %T", path, value))
	}
}

func validateProfileOverrideFloat(value float64, path string) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("profile override %q contains a non-finite float", path))
	}
	if math.Trunc(value) == value && math.Abs(value) > profileJSONSafeIntegerMax {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("profile override %q contains an integer-valued float outside the JSON round-trip-safe range", path))
	}
	return nil
}

func profileOverrideReferenceFor(value reflect.Value) (profileOverrideReference, bool) {
	if !value.IsValid() || value.Kind() != reflect.Map &&
		value.Kind() != reflect.Slice {

		return profileOverrideReference{}, false
	}
	if value.IsNil() {
		return profileOverrideReference{}, false
	}

	reference := profileOverrideReference{
		kind:    value.Kind(),
		pointer: value.Pointer(),
	}
	if value.Kind() == reflect.Slice {
		reference.length = value.Len()
		reference.capacity = value.Cap()
	}
	return reference, true
}

// ownsAllocationPolicySelectorPath reports whether an owned path
// structurally intersects one of the canonical #1327 policy-selector paths
// (pkg/allocpolicy). Owning one is the second closure trigger alongside
// advertiser "external": locks follow ownership (ADR-015 GKE amendment).
func ownsAllocationPolicySelectorPath(component, path string) bool {
	if path == profileComponentEnabledPath {
		// The synthetic presence path never triggers the closure:
		// referencing an advertiser component is not policy ownership.
		return false
	}
	for _, selectorPath := range allocpolicy.SelectorPaths(component) {
		if PathsIntersect(path, selectorPath) {
			return true
		}
	}
	return false
}

func cloneOwnedPaths(paths map[string][]string) map[string][]string {
	if paths == nil {
		return nil
	}
	out := make(map[string][]string, len(paths))
	for component, values := range paths {
		out[component] = append([]string(nil), values...)
	}
	return out
}

func profileSummary(decl *ProfileDeclaration) *ProfileSummary {
	if decl == nil {
		return nil
	}
	values := make([]string, 0, len(decl.Values))
	for name := range decl.Values {
		values = append(values, name)
	}
	sort.Strings(values)
	return &ProfileSummary{
		Name:        decl.Name,
		Description: decl.Description,
		Default:     decl.Default,
		Values:      values,
	}
}

// ValidateRecipeMetadataProfile enforces the bidirectional version/declaration
// contract for typed RecipeMetadata callers. Byte decoders additionally use
// strict decoding for RecipeProfileAPIVersion so unknown keys cannot vanish.
func ValidateRecipeMetadataProfile(metadata *RecipeMetadata) error {
	if metadata == nil {
		return nil
	}
	switch {
	case metadata.APIVersion == RecipeProfileAPIVersion && metadata.Spec.Profile == nil:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("RecipeMetadata uses apiVersion %q but has no spec.profile declaration",
				RecipeProfileAPIVersion))
	case metadata.Spec.Profile != nil && metadata.APIVersion != RecipeProfileAPIVersion:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("RecipeMetadata declares spec.profile but uses apiVersion %q; expected %q",
				metadata.APIVersion, RecipeProfileAPIVersion))
	case metadata.Spec.Profile != nil:
		_, err := ValidateProfileDeclaration(metadata.Spec.Profile)
		return err
	default:
		return nil
	}
}

// ValidateProfileContract enforces the bidirectional version/profile coupling
// and validates profile metadata for typed callers that did not pass through
// a strict byte decoder.
func (r *RecipeResult) ValidateProfileContract() error {
	if r == nil {
		return nil
	}
	switch r.APIVersion {
	case "", RecipeAPIVersion:
		if r.Metadata.SelectedProfile != nil {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("recipe apiVersion %q cannot carry metadata.selectedProfile", r.APIVersion))
		}
		return r.validateInlineDeepCopyCycles()
	case RecipeProfileAPIVersion:
		if err := r.validateProfileMetadataItems(); err != nil {
			return err
		}
		if r.Metadata.SelectedProfile == nil {
			if _, accountingConfigured := r.AccountingMode(); accountingConfigured {
				return r.validateInlineDeepCopyCycles()
			}
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("recipe apiVersion %q requires metadata.selectedProfile or configuration.slurm.accounting",
					RecipeProfileAPIVersion))
		}
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("recipe has unsupported apiVersion %q; expected %q or %q",
				r.APIVersion, RecipeAPIVersion, RecipeProfileAPIVersion))
	}

	selected := r.Metadata.SelectedProfile
	if !validProfileIdentifier(selected.Name) || !validProfileIdentifier(selected.Value) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"metadata.selectedProfile name and value must match [A-Za-z0-9._-]+")
	}
	// ValidateAdvertiser already returns ErrCodeInvalidRequest naming the
	// offending value; propagate as-is rather than double-wrap.
	if err := allocpolicy.ValidateAdvertiser(selected.Advertiser); err != nil {
		return err
	}
	if selected.OwnedPaths == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"metadata.selectedProfile.ownedPaths is required")
	}
	for component, paths := range selected.OwnedPaths {
		if component == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				"metadata.selectedProfile.ownedPaths contains an empty component name")
		}
		if !sort.StringsAreSorted(paths) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("metadata.selectedProfile.ownedPaths[%q] must be lexicographically sorted", component))
		}
		if !slices.Contains(paths, profileComponentEnabledPath) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("metadata.selectedProfile.ownedPaths[%q] must include synthetic path %q",
					component, profileComponentEnabledPath))
		}
		for i, path := range paths {
			if path == "" || strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") ||
				strings.Contains(path, "..") {

				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("metadata.selectedProfile.ownedPaths[%q] contains invalid path %q", component, path))
			}
			if i > 0 && paths[i-1] == path {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("metadata.selectedProfile.ownedPaths[%q] repeats path %q", component, path))
			}
		}
	}
	return r.validateInlineDeepCopyCycles()
}

func (r *RecipeResult) validateInlineDeepCopyCycles() error {
	for index := range r.ComponentRefs {
		ref := &r.ComponentRefs[index]
		path := ref.Name
		if path == "" {
			path = fmt.Sprintf("componentRefs[%d].overrides", index)
		}
		if err := validateProfileDeepCopyCycles(ref.Overrides, path); err != nil {
			return err
		}
	}
	return nil
}

func (r *RecipeResult) validateProfileMetadataItems() error {
	for index, overlay := range r.Metadata.ExcludedOverlays {
		if overlay.Name == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("metadata.excludedOverlays[%d].name is required", index))
		}
		// Reason remains optional for compatibility with stored object-form
		// entries that predate machine-readable exclusion reasons.
		switch overlay.Reason {
		case "", ExcludedOverlayReasonConstraintFailed, ExcludedOverlayReasonMixinConstraintFailed:
		default:
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("metadata.excludedOverlays[%d].reason %q is unsupported",
					index, overlay.Reason))
		}
	}
	for index, warning := range r.Metadata.ConstraintWarnings {
		if warning.Overlay == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("metadata.constraintWarnings[%d].overlay is required", index))
		}
		if warning.Constraint == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("metadata.constraintWarnings[%d].constraint is required", index))
		}
		if warning.Expected == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("metadata.constraintWarnings[%d].expected is required", index))
		}
		if warning.Reason == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("metadata.constraintWarnings[%d].reason is required", index))
		}
	}
	return nil
}

// PrepareAndValidateWithContext runs the shared raw-artifact gate and, only
// for profiled artifacts, hydrates locked component values to reject an
// incoherent ownership record. Legacy artifacts perform no additional I/O.
func (r *RecipeResult) PrepareAndValidateWithContext(ctx context.Context) error {
	return r.prepareAndValidateWithSource(ctx, "")
}

// prepareAndValidateWithSource is PrepareAndValidateWithContext with the
// originating file name, so a rejected constraint path can name it.
//
// Only loader.go has a file: the other callers (pkg/bundler, pkg/mirror,
// pkg/client/v1) receive a RecipeResult from an SDK caller with no source, and
// use the exported form. An empty source omits the file prefix and the "file"
// error-context key rather than reporting a placeholder.
func (r *RecipeResult) prepareAndValidateWithSource(ctx context.Context, source string) error {
	if err := r.PrepareAndValidate(); err != nil {
		return err
	}
	if r == nil {
		return nil
	}

	// A hydrated RecipeResult read from disk never builds a metadata store, so
	// the load-time constraint-path gate in buildMetadataStore does not see it.
	// Without this, `aicr bundle -r hydrated.yaml` and `aicr validate -r
	// hydrated.yaml` would skip the check on the very artifact whose
	// constraints feed the readiness pre-flight (#1783).
	if err := validateConstraintPaths(r.Constraints, source, locResultConstraints); err != nil {
		return err
	}
	if r.Validation != nil && r.Validation.Readiness != nil {
		if err := validateConstraintPaths(
			r.Validation.Readiness.Constraints, source, locResultReadiness); err != nil {
			return err
		}
	}

	if r.Metadata.SelectedProfile == nil {
		return nil
	}
	return r.ValidateProfileValuesWithContext(ctx)
}

// ValidateProfileValuesWithContext rejects recipe-side blocked paths,
// unsupported values, missing/disabled locked components, and value-bearing
// ownership of Kustomize components, which do not consume Helm values
// overrides.
func (r *RecipeResult) ValidateProfileValuesWithContext(ctx context.Context) error {
	_, err := r.validateProfileValuesWithContext(ctx)
	return err
}

func (r *RecipeResult) validateProfileValuesWithContext(
	ctx context.Context,
) (map[string]map[string]any, error) {

	if r == nil || r.Metadata.SelectedProfile == nil {
		return map[string]map[string]any{}, nil
	}
	// The effective lock set is the declared ownedPaths plus, when the
	// profile owns advertisement, the recomputed #1327 closure. Closure
	// paths are locked and hydrated exactly like declared paths, but the
	// inline-ownership requirement applies only to declared paths: closure
	// paths are assigned by component values files by design, never by the
	// profile fragment.
	lockSet := r.EffectiveLockSet()
	declared := r.Metadata.SelectedProfile.OwnedPaths
	hydrated := make(map[string]map[string]any, len(lockSet))
	components := slices.Sorted(maps.Keys(lockSet))
	for _, component := range components {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Wrap(
				errors.ErrCodeTimeout, "profile value validation canceled", ctxErr)
		}
		paths := lockSet[component]
		ref := r.GetComponentRef(component)
		if ref == nil || !ref.IsEnabled() {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile-owned component %q is missing or disabled", component))
		}
		if ref.Type == ComponentTypeKustomize && slices.ContainsFunc(paths, func(path string) bool {
			return path != profileComponentEnabledPath
		}) {

			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile-owned component %q has type %q, which does not consume values overrides",
					component, ref.Type))
		}
		// Inline canonical maps and slices must be acyclic before hydration:
		// resolveComponentValues deep-copies those containers recursively.
		// Value-shape restrictions below remain scoped to owned paths.
		if err := validateProfileDeepCopyCycles(ref.Overrides, component); err != nil {
			return nil, err
		}
		if err := validateProfileOwnedValues(ref.Overrides, component, paths); err != nil {
			return nil, err
		}
		if err := validateProfileInlineOwnership(ref.Overrides, component, declared[component]); err != nil {
			return nil, err
		}
		values, err := r.GetValuesForComponentWithContext(ctx, component)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				fmt.Sprintf("failed to hydrate profile-owned component %q", component))
		}
		if err := validateProfileOwnedValues(values, component, paths); err != nil {
			return nil, err
		}
		hydrated[component] = values
		for _, path := range paths {
			if path == profileComponentEnabledPath {
				continue
			}
			observation, err := ObserveValuePath(values, path)
			if err != nil {
				return nil, err
			}
			if observation.State == PathBlocked {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile-owned recipe path %s.%s is blocked by a non-map ancestor", component, path))
			}
		}
	}
	if err := r.checkAdvertiserCoherence(hydrated); err != nil {
		return nil, err
	}
	return hydrated, nil
}

// checkAdvertiserCoherence runs the shared #1327 tuple-coherence rules
// against the hydrated advertiser components for EVERY closure-triggering
// profile — a declared external advertiser AND the empty (operator-advertised)
// advertiser shape alike. The verdicts come from the single shared
// evaluator (allocpolicy.CheckCoherence), which applies the same #1327
// tuple-coherence rows as the validation-time resolver (pkg/validator/v1
// ResolveGPUAllocationPolicy) — gate/resolver symmetry over the shared
// tuple verdicts (ADR-015): for those tuple rows, an artifact this gate
// emits is exactly an artifact validation accepts. The #1685 dual-operator
// rejection is resolution-time-only and deliberately not mirrored here
// (it is outside #1685's scope); bundle-time rejection is a separate
// follow-up. The gate is gated on the profile owning advertisement: an
// AKS-shaped profile performs no evaluation here, and the conflicting
// toggle typically lives in a component values file — which is exactly
// why this runs at the hydration boundary rather than only at resolution
// (disk-loaded, POSTed, and direct-bundler recipes bypass resolution).
func (r *RecipeResult) checkAdvertiserCoherence(hydrated map[string]map[string]any) error {
	if !r.profileClosureTriggered() {
		return nil
	}
	observation := allocpolicy.Observation{Advertiser: r.Metadata.SelectedProfile.Advertiser}
	external := observation.Advertiser == allocpolicy.AdvertiserExternal
	// The closure guarantees every enabled descriptor component was
	// hydrated above. An absent devicePlugin.enabled on an enabled
	// operator component follows the upstream chart default (true) — the
	// same reading the #1327 resolver applies. The aggregation mirrors the
	// resolver's per-advertiser reading for the shared tuple verdicts:
	// under a declared external advertiser EVERY enabled operator
	// component is a potential second advertiser (OR semantics); under an
	// empty advertiser the gate reads the first present operator component
	// (gpu-operator before gpu-operator-ocp in the iteration). The
	// resolver's #1685 rejection of a recipe with both operators enabled
	// is resolution-time-only and deliberately not mirrored here —
	// diverging on the shared tuple rows would emit artifacts validation
	// resolves differently.
	for _, component := range []string{allocpolicy.ComponentGPUOperator, allocpolicy.ComponentGPUOperatorOCP} {
		values, ok := hydrated[component]
		if !ok {
			continue
		}
		enabled, known, err := profileBoolAtPath(values, component, allocpolicy.PathDevicePluginEnabled)
		if err != nil {
			return err
		}
		if !known {
			// Absent devicePlugin.enabled on an enabled operator component
			// follows the upstream chart default (true) — the same reading
			// the #1327 resolver applies.
			enabled = true
		}
		if observation.DevicePluginEnabled == nil {
			observation.DevicePluginEnabled = &enabled
			observation.GPUOperatorComponent = component
		} else if external && enabled {
			observation.DevicePluginEnabled = &enabled
		}
	}
	if values, ok := hydrated[allocpolicy.ComponentDRADriver]; ok {
		enabled, known, err := profileBoolAtPath(values, allocpolicy.ComponentDRADriver, allocpolicy.PathDRAGPUsEnabled)
		if err != nil {
			return err
		}
		if !known {
			// Fail closed, mirroring the validation-time resolver
			// (pkg/validator/v1 ResolveGPUAllocationPolicy): the upstream
			// chart's DECLARED default is gpus.enabled=true, so an enabled
			// DRA component with an absent switch would deploy a whole-GPU
			// advertiser this gate never observed — under a declared
			// external advertiser that is exactly the #1327 dual
			// advertisement. Skipping (leaving the reading nil) would let
			// the artifact through generation and bundling; validation is
			// not guaranteed to run before deploy. The rejection is
			// deliberately advertiser-independent: the resolver's
			// absent-switch check fires before any advertiser branching
			// (a chart-default whole-GPU advertiser next to an operator
			// plugin is the same latent dual advertisement), so scoping
			// this gate to advertiser==external would emit artifacts
			// validation later rejects. Stock recipes always pin
			// the switch via the component values; only custom/SDK values
			// files can omit it.
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q is enabled but %q is not set — the upstream chart's declared default (true) would diverge from the artifact's advertiser-coherence evaluation; pin the value explicitly in the recipe (issue #1327)",
				allocpolicy.ComponentDRADriver, allocpolicy.PathDRAGPUsEnabled))
		}
		observation.DRAGPUsEnabled = &enabled
		waiver, waiverKnown, err := profileBoolAtPath(values, allocpolicy.ComponentDRADriver, allocpolicy.PathDRAGPUsEnabledOverride)
		if err != nil {
			return err
		}
		// Absent gpuResourcesEnabledOverride reads false — the upstream
		// chart default, the same reading the #1327 resolver applies.
		if !waiverKnown {
			waiver = false
		}
		observation.DRAGPUsEnabledOverride = &waiver
	}
	return allocpolicy.CheckCoherence(observation)
}

// profileBoolAtPath reads a boolean at a dotted path. known is false when
// the path is absent; a present non-bool or a blocking ancestor fails
// closed.
func profileBoolAtPath(values map[string]any, component, path string) (value, known bool, err error) {
	raw, state := profileValueAtPath(values, path)
	switch state {
	case PathPresent:
		b, ok := raw.(bool)
		if !ok {
			return false, false, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q value %s must be a boolean, got %T", component, path, raw))
		}
		return b, true, nil
	case PathBlocked:
		return false, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q value %s is blocked by a non-map ancestor", component, path))
	case PathAbsent:
		return false, false, nil
	default:
		return false, false, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("unexpected path state observing %s.%s", component, path))
	}
}

// validateProfileInlineOwnership requires every non-synthetic owned path to be
// assigned by the component's inline overrides.
//
// Generation always satisfies this: applyEffectiveProfile merges the selected
// fragment's overrides into the componentRef, and union totality guarantees the
// selected fragment assigns every non-synthetic path in the declaration-wide
// union. Raw artifacts reaching LoadFromFile, AdoptRecipe, or POST /v2/bundle
// carry author-supplied ownedPaths, so the invariant has to be re-established
// here rather than assumed.
//
// ADR-015 states an owned path is never inherited from the baseline:
// supersession applies only the selected fragment, so an owned path missing
// from the overrides would let a values-file or external-overlay assignment
// survive the selection and then be locked and attested as if the profile had
// qualified it. An explicit null is an assignment and stays valid.
//
// The check requires PathPresent, not merely "not absent". A blocking ancestor
// cannot be deferred to the hydrated-values check, because the two states
// collapse into each other across the merge: overrides of {"driver": nil} block
// the owned path driver.version inline, and mergeValues deletes a key whose
// source value is nil, so the hydrated observation is PathAbsent rather than
// PathBlocked and neither check fires. Rejecting anything non-present inline
// closes that gap; an explicit null at the owned leaf itself is still an
// assignment and stays valid.
func validateProfileInlineOwnership(overrides map[string]any, component string, paths []string) error {
	for _, path := range paths {
		if path == profileComponentEnabledPath {
			continue
		}
		switch _, state := profileValueAtPath(overrides, path); state {
		case PathPresent:
		case PathBlocked:
			// Same defect the hydrated check reports, caught one step earlier;
			// the wording is shared so the surface does not depend on which
			// observation happens to fire first.
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile-owned recipe path %s.%s is blocked by a non-map ancestor",
					component, path))
		case PathAbsent:
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile-owned path %s.%s is not assigned by the component overrides; "+
					"an owned path may not be inherited from the baseline values",
					component, path))
		default:
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("unexpected path state %q observing %s.%s", state, component, path))
		}
	}
	return nil
}

func validateProfileOwnedValues(values map[string]any, component string, paths []string) error {
	for _, path := range paths {
		if path == profileComponentEnabledPath {
			continue
		}
		value, state := profileValueAtPath(values, path)
		if state != PathPresent {
			continue
		}
		if err := validateProfileOverrideMapKeys(value, component+"."+path); err != nil {
			return err
		}
	}
	return nil
}

// PathState is the three-valued observation used by the profile lock.
type PathState string

const (
	// PathPresent means the leaf key exists, including an explicit null.
	PathPresent PathState = "present"
	// PathAbsent means traversal reached a map where the next key was absent.
	PathAbsent PathState = "absent"
	// PathBlocked means a non-map ancestor prevented traversal to the leaf.
	PathBlocked PathState = "blocked"
)

// PathObservation is the canonical state of one dotted values path.
type PathObservation struct {
	State PathState
	Bytes []byte
}

// ObserveValuePath observes a dotted path without conflating absence with an
// ancestor scalar/list/null that blocks traversal.
func ObserveValuePath(values map[string]any, path string) (PathObservation, error) {
	value, state := profileValueAtPath(values, path)
	if state != PathPresent {
		return PathObservation{State: state}, nil
	}
	data, err := serializer.MarshalYAMLDeterministic(value)
	if err != nil {
		return PathObservation{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to serialize value at path %q", path), err)
	}
	return PathObservation{State: PathPresent, Bytes: data}, nil
}

func profileValueAtPath(values map[string]any, path string) (any, PathState) {
	parts := strings.Split(path, ".")
	current := values
	for i, part := range parts {
		value, ok := current[part]
		if !ok {
			return nil, PathAbsent
		}
		if i == len(parts)-1 {
			return value, PathPresent
		}
		next, ok := value.(map[string]any)
		if !ok {
			return nil, PathBlocked
		}
		current = next
	}
	return nil, PathAbsent
}

// PathsIntersect reports exact, ancestor, or descendant path intersection.
func PathsIntersect(a, b string) bool {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	limit := min(len(aParts), len(bParts))
	for i := range limit {
		if aParts[i] != bParts[i] {
			return false
		}
	}
	return true
}

// ValidateOwnershipDisjoint rejects exact, ancestor, or descendant path
// intersection between two independent configuration owners. Components are
// compared by canonical name; iteration is sorted so the first reported
// conflict is deterministic.
func ValidateOwnershipDisjoint(first, second OwnershipDomain) error {
	components := make([]string, 0, len(first.Paths))
	for component := range first.Paths {
		if _, ok := second.Paths[component]; ok {
			components = append(components, component)
		}
	}
	sort.Strings(components)
	for _, component := range components {
		firstPaths := append([]string(nil), first.Paths[component]...)
		secondPaths := append([]string(nil), second.Paths[component]...)
		sort.Strings(firstPaths)
		sort.Strings(secondPaths)
		for _, firstPath := range firstPaths {
			for _, secondPath := range secondPaths {
				if PathsIntersect(firstPath, secondPath) {
					return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
						"configuration ownership conflict: %q owns %s.%s, which intersects %q path %s.%s",
						first.Name, component, firstPath, second.Name, component, secondPath))
				}
			}
		}
	}
	return nil
}

// ValidateProfileLock compares the final candidate values and component set
// against the hydrated selected recipe before an output is written. Dynamic
// paths model install-time mutability and are rejected on structural
// intersection even when the current value is identical.
func (r *RecipeResult) ValidateProfileLock(
	ctx context.Context,
	candidateRefs []ComponentRef,
	candidateValues map[string]map[string]any,
	dynamicPaths map[string][]string,
) error {

	if r == nil || r.Metadata.SelectedProfile == nil {
		return nil
	}
	baselineValuesByComponent, err := r.validateProfileValuesWithContext(ctx)
	if err != nil {
		return err
	}

	candidateEnabled := make(map[string]bool, len(candidateRefs))
	for _, ref := range candidateRefs {
		candidateEnabled[ref.Name] = ref.IsEnabled()
	}

	// Iterate the EFFECTIVE lock set: a subset omitting a closure-locked
	// component (e.g. nvidia-dra-driver-gpu on an advertisement-owning GKE
	// profile) fails below exactly like one omitting a declaration-named
	// component — the invariant cannot evaluate locked paths absent from
	// the output.
	lockSet := r.EffectiveLockSet()
	components := slices.Sorted(maps.Keys(lockSet))
	for _, component := range components {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Wrap(
				errors.ErrCodeTimeout, "profile lock validation canceled", ctxErr)
		}
		paths := lockSet[component]
		baselineRef := r.GetComponentRef(component)
		if baselineRef == nil {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile-owned component %q is absent from the recipe", component))
		}
		if !candidateEnabled[component] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile-owned component %q is absent or disabled in the output", component))
		}
		baselineValues := baselineValuesByComponent[component]
		candidate, ok := candidateValues[component]
		if !ok {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile-owned component %q has no candidate values", component))
		}

		for _, lockedPath := range paths {
			for _, dynamicPath := range dynamicPaths[component] {
				if PathsIntersect(lockedPath, dynamicPath) {
					return errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("install-time path %s.%s intersects profile-owned path %s.%s",
							component, dynamicPath, component, lockedPath))
				}
			}
			if lockedPath == profileComponentEnabledPath {
				if baselineRef.IsEnabled() != candidateEnabled[component] {
					return errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("profile-owned component presence diverged for %q", component))
				}
				continue
			}
			want, err := ObserveValuePath(baselineValues, lockedPath)
			if err != nil {
				return err
			}
			got, err := ObserveValuePath(candidate, lockedPath)
			if err != nil {
				return err
			}
			if want.State == PathBlocked {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile-owned recipe path %s.%s is blocked", component, lockedPath))
			}
			if want.State != got.State || !bytes.Equal(want.Bytes, got.Bytes) {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile-owned path %s.%s diverged from selected profile %s=%s",
						component, lockedPath, r.Metadata.SelectedProfile.Name, r.Metadata.SelectedProfile.Value))
			}
		}
	}
	return nil
}

// OwnsProfilePath reports whether selectedProfile owns a component value path.
func (r *RecipeResult) OwnsProfilePath(component, path string) bool {
	if r == nil || r.Metadata.SelectedProfile == nil {
		return false
	}
	for _, owned := range r.EffectiveLockSet()[component] {
		if PathsIntersect(owned, path) {
			return true
		}
	}
	return false
}

// profileClosureTriggered reports whether the selected profile owns
// advertisement (ADR-015 GKE amendment): a declared external advertiser,
// or explicit ownership of a non-synthetic #1327 policy-selector path.
// Locks follow ownership — a profile that does not own advertisement (the
// AKS driver/toolkit values) leaves allocation-policy keys on today's WARN
// semantics, and the synthetic per-component presence path never triggers
// the closure.
func (r *RecipeResult) profileClosureTriggered() bool {
	selected := r.Metadata.SelectedProfile
	if selected == nil {
		return false
	}
	if selected.Advertiser == allocpolicy.AdvertiserExternal {
		return true
	}
	for component, paths := range selected.OwnedPaths {
		for _, path := range paths {
			if ownsAllocationPolicySelectorPath(component, path) {
				return true
			}
		}
	}
	return false
}

// ClosureDescriptorEntries returns the canonical #1327 descriptor entries
// contributing to this recipe's effective profile lock closure: when the
// selected profile owns advertisement (the closure trigger), every ENABLED
// descriptor component's entry, in descriptor order; empty otherwise.
// Evidence production records allocpolicy.IdentityFor over this set — the
// deterministic identity of the entries contributing to THIS recipe's
// closure — so a descriptor expansion that does not touch the recipe's
// contributing entries cannot spuriously invalidate its evidence (ADR-015
// descriptor-currentness).
func (r *RecipeResult) ClosureDescriptorEntries() []allocpolicy.Entry {
	if r == nil || !r.profileClosureTriggered() {
		return nil
	}
	var entries []allocpolicy.Entry
	for _, entry := range allocpolicy.Descriptor() {
		ref := r.GetComponentRef(entry.Component)
		if ref == nil || !ref.IsEnabled() {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// EffectiveLockSet returns the profile's effective lock set: the declared
// ownedPaths plus, when the profile owns advertisement, the recomputed
// #1327 closure — every ENABLED descriptor component's selector paths plus
// its synthetic presence path. The closure is recomputed from the
// canonical descriptor (pkg/allocpolicy) at every artifact boundary and is
// never persisted in the artifact: persisting it would freeze the lock at
// authoring time, and a descriptor expansion could then never strengthen
// older authentic recipes. Absent or declared-but-disabled descriptor
// components contribute nothing (ADR-015).
func (r *RecipeResult) EffectiveLockSet() map[string][]string {
	if r == nil || r.Metadata.SelectedProfile == nil {
		return nil
	}
	lock := cloneOwnedPaths(r.Metadata.SelectedProfile.OwnedPaths)
	if !r.profileClosureTriggered() {
		return lock
	}
	if lock == nil {
		// A nil OwnedPaths never survives artifact validation
		// (ValidateProfileContract requires the field), but the closure can
		// trigger on Advertiser alone for a typed SDK caller that skipped
		// validation — return the closure paths rather than panic on the
		// nil-map assignment below.
		lock = make(map[string][]string)
	}
	for _, entry := range allocpolicy.Descriptor() {
		ref := r.GetComponentRef(entry.Component)
		if ref == nil || !ref.IsEnabled() {
			continue
		}
		merged := make(map[string]struct{}, len(lock[entry.Component])+len(entry.SelectorPaths)+1)
		for _, path := range lock[entry.Component] {
			merged[path] = struct{}{}
		}
		merged[profileComponentEnabledPath] = struct{}{}
		for _, path := range entry.SelectorPaths {
			merged[path] = struct{}{}
		}
		lock[entry.Component] = slices.Sorted(maps.Keys(merged))
	}
	return lock
}
