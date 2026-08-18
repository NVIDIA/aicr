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

	appconfig "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Config is a parsed AICRConfig document — the version-controlled file a team
// commits so their snapshot / recipe / bundle / validate / verify settings
// live beside the code they configure, rather than being retyped on each
// invocation.
//
// # Deriving options, not applying them
//
// A Config does not attach to a Client and is never consulted implicitly.
// Instead each method below DERIVES a populated options value, which the
// caller may then override:
//
//	cfg, err := aicr.LoadConfig(ctx, "aicr-config.yaml")
//	opts, err := cfg.BundleVerifyOptions()
//	opts.MinTrustLevel = "verified"   // caller wins, visibly
//	v, err := client.VerifyBundle(ctx, dir, opts)
//
// That shape is deliberate. The facade's options are plain structs, so a
// field left at its zero value is indistinguishable from one a caller set to
// the zero value on purpose — there is no equivalent of the CLI's
// cmd.IsSet. An implicit merge would therefore have to guess, and would
// silently hand back the config's value to a caller who deliberately cleared
// a setting. Deriving makes precedence one readable line at the call site
// instead of a merge rule the caller has to remember.
//
// It also matches what the CLI does: build options from config, then let an
// explicitly-set flag win. The flag half necessarily stays in pkg/cli, which
// is the only layer that knows a flag was set.
//
// # Nil safety
//
// Every method tolerates a nil Config and nil spec sections, returning zero
// values rather than erroring. A caller that did not supply a config can
// derive unconditionally and get "nothing configured", which is what the CLI
// does when --config is absent.
type Config struct {
	internal *appconfig.AICRConfig
}

// LoadConfig reads and validates an AICRConfig from a file path or an
// HTTP(S) URL.
//
// Errors keep the loader's structured codes — ErrCodeNotFound for a missing
// file, ErrCodeInvalidRequest for malformed input or a strict-decode
// rejection, ErrCodeUnavailable for an HTTP failure — rather than being
// flattened.
//
// # Criteria values are validated later, not here
//
// Loading checks structure, not criteria MEMBERSHIP. Whether "eks" or some
// value your own catalog defines is legal depends on the CriteriaRegistry,
// which is per-DataProvider — and the provider named by spec.recipe.data does
// not exist yet at load time. Validating here could only check the embedded
// catalog, which would reject every externally-contributed value and make a
// config-driven external catalog unusable.
//
// So membership is checked at RecipeCriteria, where a registry is in hand:
//
//	cfg, err := aicr.LoadConfig(ctx, path)          // structure
//	source, _ := cfg.RecipeSource()                 // spec.recipe.data
//	client, err := aicr.NewClient(aicr.WithRecipeSource(source))
//	err = client.LoadCatalog(ctx)                   // seeds the registry
//	criteria, err := cfg.RecipeCriteria(client.CriteriaRegistry())  // membership
//
// A value in no catalog still fails — at that last step rather than the
// first.
func LoadConfig(ctx context.Context, source string) (*Config, error) {
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if source == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "config source is required (got empty)")
	}
	loaded, err := appconfig.Load(ctx, source)
	if err != nil {
		// Don't re-wrap: Load already returns coded errors, and the code is
		// how a caller tells "no such file" from "this file is malformed".
		return nil, err
	}
	return WrapConfig(loaded), nil
}

// WrapConfig lifts an AICRConfig parsed elsewhere into the facade type, for
// callers that already hold one from pkg/config. Returns nil for nil input,
// mirroring WrapSnapshot.
func WrapConfig(c *appconfig.AICRConfig) *Config {
	if c == nil {
		return nil
	}
	return &Config{internal: c}
}

// Unwrap returns the underlying AICRConfig, for callers that need a spec field
// this facade does not project. Returns nil for a nil Config.
//
// Reaching for this is a signal worth acting on: it means the facade is
// missing a derivation someone needs. Prefer opening an issue over building
// on the raw document, since pkg/config carries no stability guarantee.
func (c *Config) Unwrap() *appconfig.AICRConfig {
	if c == nil {
		return nil
	}
	return c.internal
}

// BundleVerifyOptions derives Client.VerifyBundle options from spec.verify.
//
// The mapping is one-to-one: spec.verify.trust supplies
// CertificateIdentityRegexp, Key, and TrustRoot, and spec.verify.policy
// supplies MinTrustLevel, RequireCreator, and CLIVersionConstraint. That
// alignment is not a coincidence — BundleVerifyOptions was shaped to mirror
// VerifySpec so this stayed a copy rather than a translation table.
//
// IgnoreTLog has no config counterpart and is left false. It weakens the trust
// floor by dropping the transparency-log requirement, and keeping it
// command-line-only means a checked-in file can never silently disable that
// check.
//
// An empty MinTrustLevel is preserved rather than defaulted here, so
// VerifyBundle applies its own "max" default. Setting it in this layer would
// hide which of the two chose the floor.
//
// Returns an error when spec.verify is present but malformed.
func (c *Config) BundleVerifyOptions() (BundleVerifyOptions, error) {
	if c == nil || c.internal == nil {
		return BundleVerifyOptions{}, nil
	}
	resolved, err := c.internal.Verification().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return BundleVerifyOptions{}, err
	}
	if resolved == nil {
		return BundleVerifyOptions{}, nil
	}
	return BundleVerifyOptions{
		CertificateIdentityRegexp: resolved.CertificateIdentityRegexp,
		Key:                       resolved.Key,
		TrustRoot:                 resolved.TrustRoot,
		MinTrustLevel:             resolved.MinTrustLevel,
		RequireCreator:            resolved.RequireCreator,
		CLIVersionConstraint:      resolved.VersionConstraint,
	}, nil
}

// RecipeSource derives the Client recipe source from spec.recipe.data, and
// reports whether the document configured one.
//
// This is the piece that lets a committed config stand up a Client at all: a
// non-empty data directory yields a FilesystemSource layered over the embedded
// recipe data, matching `aicr recipe --data`. When false is returned the
// caller supplies its own source, normally EmbeddedSource.
//
// Deliberately NOT folded into a Client option. Recipe source is fixed at
// construction — a Client owns its DataProvider for its whole lifetime — so
// this belongs in the NewClient call rather than in a per-operation options
// value.
func (c *Config) RecipeSource() (RecipeSourceOption, bool) {
	if c == nil || c.internal == nil {
		return RecipeSourceOption{}, false
	}
	dir := c.internal.Recipe().DataDir()
	if dir == "" {
		return RecipeSourceOption{}, false
	}
	return FilesystemSource(dir), true
}

// RecipeCriteria derives resolve criteria from spec.recipe.criteria, parsed
// against the supplied registry so a value contributed by a --data overlay
// validates against the same DataProvider the Client resolves with. Pass
// Client.CriteriaRegistry(); a nil registry falls back to the embedded
// catalog.
//
// Returns an empty (non-nil) Criteria when the document states none, so the
// result is always safe to hand to a resolve call or to overwrite field by
// field.
func (c *Config) RecipeCriteria(reg *CriteriaRegistry) (*Criteria, error) {
	if c == nil || c.internal == nil {
		return &Criteria{}, nil
	}
	resolved, err := c.internal.Recipe().ResolveCriteriaWithRegistry(reg)
	if err != nil {
		// Coded ErrCodeInvalidRequest, naming the offending spec field.
		return nil, err
	}
	return WrapCriteria(resolved), nil
}

// RecipeResolveOptions derives the resolve options spec.recipe carries:
// the configuration profile selection (spec.recipe.profile) and the Slurm
// accounting mode (spec.recipe.configuration.slurm.accounting.mode).
//
// Returns a nil slice when the document sets neither, so it can be appended to
// a caller's own options unconditionally:
//
//	opts, err := cfg.RecipeResolveOptions()
//	opts = append(opts, aicr.WithProfile(flagProfile))  // caller wins: later option overwrites
func (c *Config) RecipeResolveOptions() ([]RecipeResolveOption, error) {
	if c == nil || c.internal == nil {
		return nil, nil
	}
	spec := c.internal.Recipe()

	var out []RecipeResolveOption
	if profile := spec.ProfileSelection(); profile != "" {
		out = append(out, WithProfile(profile))
	}

	mode, set, err := spec.ResolveAccountingMode()
	if err != nil {
		return nil, err
	}
	if set {
		out = append(out, WithAccountingMode(string(mode)))
	}
	return out, nil
}

// RecipeProfile returns spec.recipe.profile, the configuration-profile
// selection in name=value form. Empty when unset.
//
// RecipeResolveOptions already folds this into a ready-to-use option; this
// raw accessor exists for callers that must apply their own precedence first,
// which is exactly what the CLI does when overlaying an explicitly-set
// --profile flag. Reach for the options form unless you need the raw value.
func (c *Config) RecipeProfile() string {
	if c == nil || c.internal == nil {
		return ""
	}
	return c.internal.Recipe().ProfileSelection()
}

// RecipeAccountingMode returns the Slurm accounting mode from
// spec.recipe.configuration.slurm.accounting.mode, and reports whether the
// document set one. Same raw-accessor rationale as RecipeProfile.
//
// Returns an error when the configured value is not a valid accounting mode.
func (c *Config) RecipeAccountingMode() (string, bool, error) {
	if c == nil || c.internal == nil {
		return "", false, nil
	}
	mode, set, err := c.internal.Recipe().ResolveAccountingMode()
	if err != nil {
		return "", false, err
	}
	return string(mode), set, nil
}

// SnapshotPath returns spec.recipe.input.snapshot, the snapshot a committed
// config resolves against. Empty when unset; hand a non-empty value to
// Client.LoadSnapshot.
func (c *Config) SnapshotPath() string {
	if c == nil || c.internal == nil {
		return ""
	}
	return c.internal.Recipe().SnapshotPath()
}

// IsCriteriaStrict reports spec.recipe.criteriaStrict, which rejects criteria
// values outside the embedded catalog — hiding registry entries contributed by
// a --data overlay.
//
// Exposed as a plain read rather than applied inside RecipeCriteria on
// purpose: strictness is a property of the CriteriaRegistry, which is shared
// per-DataProvider, so a derivation method that set it would mutate state the
// caller shares with every other operation on that Client. The caller applies
// it deliberately, or not at all.
func (c *Config) IsCriteriaStrict() bool {
	if c == nil || c.internal == nil {
		return false
	}
	return c.internal.Recipe().IsCriteriaStrict()
}
