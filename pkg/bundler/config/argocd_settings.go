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

package config

import (
	"fmt"
	"sort"
	"time"

	"github.com/NVIDIA/aicr/pkg/deprecation"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// ArgoCDSettings is the typed form of the Argo CD bundle settings, shared by
// the --argocd-* CLI flags, spec.bundle.deployment.argocd in a config file,
// the argocd-* REST query parameters, and BundleOptions.ArgoCD in the Go SDK.
//
// A nil field is unset, which is distinct from an explicit false or 0: it
// leaves the field to a lower-precedence source (see Overlay) and, at the
// end of the chain, to the generator default.
//
// NamePrefix, DestinationServer and Project apply to child Applications
// only; CascadeDelete applies to the parent and every child. The sync retry
// fields set spec.syncPolicy.retry on every child Application, and backoff
// fields require SyncRetryLimit because Argo CD reads an absent limit as 0
// (no retries).
type ArgoCDSettings struct {
	NamePrefix                  *string
	DestinationServer           *string
	Project                     *string
	CascadeDelete               *bool
	SyncRetryLimit              *int64
	SyncRetryBackoffDuration    *string
	SyncRetryBackoffFactor      *int64
	SyncRetryBackoffMaxDuration *string
}

// IsZero reports whether no setting is set.
func (s ArgoCDSettings) IsZero() bool {
	return s == ArgoCDSettings{}
}

// Overlay returns s with every field set in o replacing the same field in s.
// It is the per-field precedence rule for layering a higher-precedence
// source (o, e.g. CLI flags) over a lower one (s, e.g. a config file).
func (s ArgoCDSettings) Overlay(o ArgoCDSettings) ArgoCDSettings {
	out := s.Clone()
	if o.NamePrefix != nil {
		out.NamePrefix = clonePtr(o.NamePrefix)
	}
	if o.DestinationServer != nil {
		out.DestinationServer = clonePtr(o.DestinationServer)
	}
	if o.Project != nil {
		out.Project = clonePtr(o.Project)
	}
	if o.CascadeDelete != nil {
		out.CascadeDelete = clonePtr(o.CascadeDelete)
	}
	if o.SyncRetryLimit != nil {
		out.SyncRetryLimit = clonePtr(o.SyncRetryLimit)
	}
	if o.SyncRetryBackoffDuration != nil {
		out.SyncRetryBackoffDuration = clonePtr(o.SyncRetryBackoffDuration)
	}
	if o.SyncRetryBackoffFactor != nil {
		out.SyncRetryBackoffFactor = clonePtr(o.SyncRetryBackoffFactor)
	}
	if o.SyncRetryBackoffMaxDuration != nil {
		out.SyncRetryBackoffMaxDuration = clonePtr(o.SyncRetryBackoffMaxDuration)
	}
	return out
}

// Clone returns a deep copy of s, so a caller mutating its own pointers
// cannot reach into a stored copy.
func (s ArgoCDSettings) Clone() ArgoCDSettings {
	return ArgoCDSettings{
		NamePrefix:                  clonePtr(s.NamePrefix),
		DestinationServer:           clonePtr(s.DestinationServer),
		Project:                     clonePtr(s.Project),
		CascadeDelete:               clonePtr(s.CascadeDelete),
		SyncRetryLimit:              clonePtr(s.SyncRetryLimit),
		SyncRetryBackoffDuration:    clonePtr(s.SyncRetryBackoffDuration),
		SyncRetryBackoffFactor:      clonePtr(s.SyncRetryBackoffFactor),
		SyncRetryBackoffMaxDuration: clonePtr(s.SyncRetryBackoffMaxDuration),
	}
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Validate reports the first invalid setting with ErrCodeInvalidRequest,
// including backoff set without a limit. It is for the final, layered
// settings; see ValidateFields for one source on its own.
func (s ArgoCDSettings) Validate() error {
	if err := s.ValidateFields(); err != nil {
		return err
	}
	if s.SyncRetryLimit == nil && (s.SyncRetryBackoffDuration != nil ||
		s.SyncRetryBackoffFactor != nil || s.SyncRetryBackoffMaxDuration != nil) {

		return errors.New(errors.ErrCodeInvalidRequest,
			"Argo CD sync retry backoff requires a sync retry limit: Argo CD reads a missing limit as 0, which disables retries")
	}
	return nil
}

// ValidateFields reports the first set field whose value is invalid on its
// own, with ErrCodeInvalidRequest. Unlike Validate it skips the rule that
// backoff needs a limit, so it suits one source that a higher-precedence
// source may still complete.
func (s ArgoCDSettings) ValidateFields() error {
	if s.DestinationServer != nil {
		if err := ValidateDestinationServer(*s.DestinationServer); err != nil {
			return err
		}
	}
	if s.NamePrefix != nil {
		if err := ValidateNamePrefix(*s.NamePrefix); err != nil {
			return err
		}
	}
	if s.Project != nil {
		if err := validateExplicitProject(*s.Project); err != nil {
			return err
		}
	}
	// SyncRetry uses a zero factor for "unset", so an explicit 0 has to be
	// caught here, while the input is still a pointer.
	if f := s.SyncRetryBackoffFactor; f != nil && *f < 1 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid Argo CD sync retry backoff factor %d: must be >= 1", *f))
	}
	for _, d := range []struct {
		label string
		value *string
	}{
		{"backoff duration", s.SyncRetryBackoffDuration},
		{"backoff max duration", s.SyncRetryBackoffMaxDuration},
	} {
		if d.value != nil && *d.value == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid Argo CD sync retry %s \"\": must not be empty; omit it to keep Argo CD's default", d.label))
		}
	}
	// A zero Limit stands in for an unset one: it is valid, so only the set
	// fields can fail.
	r := SyncRetry{}
	if s.SyncRetryLimit != nil {
		r.Limit = *s.SyncRetryLimit
	}
	if s.SyncRetryBackoffDuration != nil {
		r.BackoffDuration = *s.SyncRetryBackoffDuration
	}
	if s.SyncRetryBackoffMaxDuration != nil {
		r.BackoffMaxDuration = *s.SyncRetryBackoffMaxDuration
	}
	return r.Validate()
}

// syncRetry returns the SyncRetry the settings describe, or nil when
// SyncRetryLimit is unset. It does not validate.
func (s ArgoCDSettings) syncRetry() *SyncRetry {
	if s.SyncRetryLimit == nil {
		return nil
	}
	r := &SyncRetry{Limit: *s.SyncRetryLimit}
	if s.SyncRetryBackoffDuration != nil {
		r.BackoffDuration = *s.SyncRetryBackoffDuration
	}
	if s.SyncRetryBackoffFactor != nil {
		r.BackoffFactor = *s.SyncRetryBackoffFactor
	}
	if s.SyncRetryBackoffMaxDuration != nil {
		r.BackoffMaxDuration = *s.SyncRetryBackoffMaxDuration
	}
	return r
}

// legacySet reports whether the field the deprecated deployer:<key> maps to
// is set in s.
func (s ArgoCDSettings) legacySet(key string) bool {
	switch key {
	case deployerKeyNamePrefix:
		return s.NamePrefix != nil
	case deployerKeyDestinationServer:
		return s.DestinationServer != nil
	case deployerKeyProject:
		return s.Project != nil
	case deployerKeyCascadeDelete:
		return s.CascadeDelete != nil
	default:
		return false
	}
}

// legacyKeys returns the deprecated deployer:<key> names whose fields are
// set in s.
func (s ArgoCDSettings) legacyKeys() map[string]bool {
	keys := make(map[string]bool)
	for _, key := range []string{deployerKeyCascadeDelete, deployerKeyDestinationServer, deployerKeyNamePrefix, deployerKeyProject} {
		if s.legacySet(key) {
			keys[key] = true
		}
	}
	return keys
}

// withoutLegacyKeys returns a copy of s with the fields that the given
// deprecated deployer:<key> names map to cleared.
func (s ArgoCDSettings) withoutLegacyKeys(keys map[string]bool) ArgoCDSettings {
	out := s.Clone()
	if keys[deployerKeyCascadeDelete] {
		out.CascadeDelete = nil
	}
	if keys[deployerKeyDestinationServer] {
		out.DestinationServer = nil
	}
	if keys[deployerKeyNamePrefix] {
		out.NamePrefix = nil
	}
	if keys[deployerKeyProject] {
		out.Project = nil
	}
	return out
}

// SyncRetry is a child Application's spec.syncPolicy.retry. Limit is always
// rendered; a zero BackoffFactor or empty BackoffDuration/BackoffMaxDuration
// is left out, so Argo CD applies its own default for that field (5s, 2, 3m).
type SyncRetry struct {
	Limit              int64
	BackoffDuration    string
	BackoffFactor      int64
	BackoffMaxDuration string
}

// HasBackoff reports whether any backoff field is set.
func (r *SyncRetry) HasBackoff() bool {
	return r.BackoffDuration != "" || r.BackoffFactor != 0 || r.BackoffMaxDuration != ""
}

// Validate reports an invalid retry with ErrCodeInvalidRequest. A nil
// SyncRetry is valid (retry unset).
func (r *SyncRetry) Validate() error {
	if r == nil {
		return nil
	}
	if r.Limit < 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid Argo CD sync retry limit %d: must be >= 0 (Argo CD retries a negative limit forever)", r.Limit))
	}
	if r.BackoffFactor < 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid Argo CD sync retry backoff factor %d: must be >= 1", r.BackoffFactor))
	}
	if err := validateRetryDuration("backoff duration", r.BackoffDuration); err != nil {
		return err
	}
	return validateRetryDuration("backoff max duration", r.BackoffMaxDuration)
}

// validateRetryDuration accepts empty (unset) or a positive Go duration,
// the format Argo CD parses for retry backoff.
func validateRetryDuration(label, raw string) error {
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid Argo CD sync retry %s %q: must be a duration such as 30s or 5m", label, raw), err)
	}
	if d <= 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid Argo CD sync retry %s %q: must be positive", label, raw))
	}
	return nil
}

// ResolveArgoDeployerOptions validates the dedicated Argo CD settings and the
// deprecated `--set deployer:<key>` overrides from one source and merges
// them into ArgoDeployerOptions. A setting given both ways is rejected rather
// than resolved by precedence: within one source the two spellings are
// ambiguous. A caller layering several sources (the CLI over a config file)
// applies its precedence before calling. Returns (nil, nil) when neither is
// set.
func ResolveArgoDeployerOptions(legacy map[string]string, settings ArgoCDSettings) (*ArgoDeployerOptions, error) {
	if len(legacy) == 0 && settings.IsZero() {
		//nolint:nilnil // returning (nil, nil) is intentional for optional configuration
		return nil, nil
	}
	merged, err := mergeLegacy(legacy, settings)
	if err != nil {
		return nil, err
	}
	if err := merged.Validate(); err != nil {
		return nil, err
	}
	opts := &ArgoDeployerOptions{SyncRetry: merged.syncRetry()}
	if merged.NamePrefix != nil {
		opts.NamePrefix = *merged.NamePrefix
	}
	if merged.DestinationServer != nil {
		opts.DestinationServer = *merged.DestinationServer
	}
	if merged.Project != nil {
		opts.Project = *merged.Project
	}
	if merged.CascadeDelete != nil {
		opts.CascadeDelete = *merged.CascadeDelete
	}
	return opts, nil
}

// ValidateArgoCDSource checks one source of Argo CD settings, such as a config
// file, before it is layered under a higher-precedence one: each field on its
// own, and a setting given both as a deprecated deployer:<key> and as a
// dedicated field. Rules that a later source can satisfy, such as backoff
// needing a limit, are left to ResolveArgoDeployerOptions.
func ValidateArgoCDSource(legacy map[string]string, settings ArgoCDSettings) error {
	merged, err := mergeLegacy(legacy, settings)
	if err != nil {
		return err
	}
	return merged.ValidateFields()
}

// mergeLegacy parses legacy and adds it to settings, rejecting a key set both
// ways.
func mergeLegacy(legacy map[string]string, settings ArgoCDSettings) (ArgoCDSettings, error) {
	fromLegacy, err := argoCDSettingsFromLegacy(legacy)
	if err != nil {
		return ArgoCDSettings{}, err
	}
	for _, key := range sortedKeys(legacy) {
		if settings.legacySet(key) {
			return ArgoCDSettings{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("Argo CD option %s is set twice, by the deprecated deployer:%s value override and by %s; remove deployer:%s",
					key, key, legacyReplacements[key], key))
		}
	}
	return settings.Overlay(fromLegacy), nil
}

// LayerArgoCDSettings applies CLI-over-config precedence across both
// spellings of the Argo CD settings. overrides are the value overrides the
// CLI already resolved from exactly one source: --set when cliSet, else the
// config file. A deprecated --set deployer:<key> clears the config file's
// dedicated field for that key, and a dedicated flag drops the config file's
// deployer:<key> entry. Both spellings from the same source are left in place
// for ResolveArgoDeployerOptions to reject as ambiguous. Returns the
// overrides to use and fromFlags layered over fromConfig.
func LayerArgoCDSettings(
	cliSet bool,
	overrides []ComponentPath,
	fromConfig, fromFlags ArgoCDSettings,
) ([]ComponentPath, ArgoCDSettings) {

	if cliSet {
		keys := make(map[string]bool)
		for key := range LegacyDeployerOverrides(overrides) {
			keys[key] = true
		}
		fromConfig = fromConfig.withoutLegacyKeys(keys)
	} else {
		overrides = withoutLegacyDeployerKeys(overrides, fromFlags.legacyKeys())
	}
	return overrides, fromConfig.Overlay(fromFlags)
}

// LegacyDeployerOverrides returns the deprecated `deployer:<key>` entries of
// paths as a key-to-value map, later entries winning as they do when the
// paths are applied. Returns nil when there are none.
func LegacyDeployerOverrides(paths []ComponentPath) map[string]string {
	var out map[string]string
	for _, p := range paths {
		if p.Component != DeployerOverrideKey || p.Value == nil {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[p.Path] = *p.Value
	}
	return out
}

// withoutLegacyDeployerKeys returns paths minus the `deployer:<key>` entries
// for keys, preserving order.
func withoutLegacyDeployerKeys(paths []ComponentPath, keys map[string]bool) []ComponentPath {
	if len(keys) == 0 {
		return paths
	}
	out := make([]ComponentPath, 0, len(paths))
	for _, p := range paths {
		if p.Component == DeployerOverrideKey && keys[p.Path] {
			continue
		}
		out = append(out, p)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// legacyReplacements names, for each deprecated deployer:<key>, the CLI flag,
// config field and REST query parameter that replace it.
var legacyReplacements = map[string]string{
	deployerKeyCascadeDelete:     "--argocd-cascade-delete (config spec.bundle.deployment.argocd.cascadeDelete, REST argocd-cascade-delete)",
	deployerKeyDestinationServer: "--argocd-destination-server (config spec.bundle.deployment.argocd.destinationServer, REST argocd-destination-server)",
	deployerKeyNamePrefix:        "--argocd-name-prefix (config spec.bundle.deployment.argocd.namePrefix, REST argocd-name-prefix)",
	deployerKeyProject:           "--argocd-project (config spec.bundle.deployment.argocd.project, REST argocd-project)",
}

// legacyDeployerAnchor is the docs/user/deprecations.md entry for the
// deployer:<key> value overrides.
const legacyDeployerAnchor = "argo-cd-options-as-deployer-value-overrides"

// LegacyDeployerNotice returns the deprecation notice for the deprecated
// `deployer:<key>` value override. No removal release is scheduled; the
// earliest possible is v2.0.0 (RELEASE.md § Deprecation Policy).
func LegacyDeployerNotice(key string) deprecation.Notice {
	return deprecation.Notice{
		Subject:     "--set deployer:" + key,
		Replacement: legacyReplacements[key],
		Anchor:      legacyDeployerAnchor,
	}
}
