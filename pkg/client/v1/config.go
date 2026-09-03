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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	appconfig "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
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

	// Every generation-time selection must be projected here. This method is
	// the canonical config-to-options conversion for SDK callers, so omitting
	// one silently drops it for anyone who configures it in a document rather
	// than through an option.
	riMode, riSet, err := spec.ResolveRuntimeInventoryMode()
	if err != nil {
		return nil, err
	}
	if riSet {
		out = append(out, WithRuntimeInventoryMode(string(riMode)))
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

// RecipeRuntimeInventoryMode returns
// spec.recipe.configuration.runtimeInventory.mode and whether the document set
// one. Same raw-accessor rationale as RecipeAccountingMode.
//
// Returns an error when the configured value is not a valid mode.
func (c *Config) RecipeRuntimeInventoryMode() (string, bool, error) {
	if c == nil || c.internal == nil {
		return "", false, nil
	}
	mode, set, err := c.internal.Recipe().ResolveRuntimeInventoryMode()
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

// BundleOptions derives Client.MakeBundle options from spec.bundle, as plain
// fields rather than a built *BundleConfig — so a caller (the CLI in
// particular) can read and override individual settings with its own flag
// precedence instead of reaching into an opaque built config.
//
// Eighteen flat fields project the bundler settings the section configures —
// deployment (deployer, repo, value overrides, dynamic values, vendoring, app
// name), scheduling (system/accelerated selectors and tolerations, DRA
// eviction label, workload gate and selector, node count, storage classes),
// and the two attestation flags the bundler itself reads (attest, certificate
// identity regexp). OIDCResolve carries what reaches the attester rather than
// the bundler: the Attest gate, DeviceFlow, FulcioURL, RekorURL, SigningKey,
// and the derived UseTUFSigningConfig.
//
// # What is deliberately NOT projected here
//
// RecipeInput, OutputTarget, OutputTargetRaw, ImageRefs, InsecureTLS and
// PlainHTTP are CALLER-side settings — which recipe to bundle, where to push
// the result, and how to reach that registry — not bundler settings, so they
// have no home on BundleOptions. BundleInputOptions carries them.
//
// # Zero values
//
// PromptWriter is also left nil, because config cannot carry an io.Writer. A
// nil writer is treated as io.Discard, so a derived DeviceFlow discards the
// verification URL and user code and the lazy attester then blocks until the
// context deadline on first Attest(). That fails closed — no wrong signature —
// but the caller must set OIDCResolve.PromptWriter to use device flow at all.
// Erroring here instead would break derive-don't-apply: a caller may well
// supply their own Attester and never reach the device flow.
//
// Attester, BinaryAttestation, OutputDir and Timeout are left at their zero
// values. None has a spec.bundle counterpart, and defaulting them here would
// hide which layer chose. A caller sets them after deriving, which is the
// same precedence the CLI applies to an explicitly-set flag.
//
// Returns an error when spec.bundle is present but malformed.
func (c *Config) BundleOptions() (BundleOptions, error) {
	if c == nil || c.internal == nil {
		return BundleOptions{}, nil
	}
	resolved, err := c.internal.Bundle().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return BundleOptions{}, err
	}

	// Signing mode is exclusive: a KMS key or keyless OIDC, never both.
	// ResolveAttesterLazy picks KMS whenever SigningKey is non-empty, so a
	// document setting both would silently sign with the key while its
	// fulcioURL/oidcDeviceFlow settings did nothing. The CLI rejects that
	// combination on the MERGED opts (validateSigningKeyExclusivity, and
	// TestValidateSigningKeyExclusivity_ConfigSourcedConflict covers exactly
	// the config-sourced case) — this is what an SDK caller who never merges
	// flags relies on for the same guarantee.
	//
	// oidcDeviceFlow is deliberately NOT checked here, unlike fulcioURL. This
	// method runs before the CLI's flag-over-config merge (BundleOptions is
	// derived, then the CLI layers flags on top — see parseBundleCmdOptions),
	// so an eager oidcDeviceFlow check would reject a document that sets both
	// signingKey and oidcDeviceFlow: true even when the caller passes
	// --oidc-device-flow=false specifically to correct it: the error fires
	// before that flag is ever read, and validateSigningKeyExclusivity on the
	// merged opts never gets a chance to see the correction. Checking it only
	// on the merged opts — the same layer that already re-validates
	// fulcioURL — restores that per-field override for a boolean flag whose
	// zero value (false) cannot be told apart from "explicitly cleared"
	// without cmd.IsSet, which only the CLI layer has.
	//
	// Trimmed first for the same reason the CLI trims: a YAML block scalar
	// carries surrounding whitespace, and an untrimmed key fails late in the
	// KMS URI parser instead of here. rekorURL is deliberately not a conflict;
	// it has its own exclusivity rule against signingConfig.
	signingKey := strings.TrimSpace(resolved.SigningKey)
	if resolved.SigningKey != "" && signingKey == "" {
		return BundleOptions{}, errors.New(errors.ErrCodeInvalidRequest,
			"spec.bundle.attestation.signingKey must not be blank")
	}
	if signingKey != "" && resolved.FulcioURL != "" {
		return BundleOptions{}, errors.New(errors.ErrCodeInvalidRequest,
			"spec.bundle.attestation.signingKey is mutually exclusive with "+
				"spec.bundle.attestation.fulcioURL")
	}

	return BundleOptions{
		Deployer:                   resolved.Deployer,
		Repo:                       resolved.Repo,
		ValueOverrides:             resolved.ValueOverrides,
		DynamicValues:              resolved.DynamicValues,
		SystemNodeSelector:         resolved.SystemNodeSelector,
		SystemNodeTolerations:      resolved.SystemNodeTolerations,
		AcceleratedNodeSelector:    resolved.AcceleratedNodeSelector,
		AcceleratedNodeTolerations: resolved.AcceleratedNodeTolerations,
		// Pointer field: nil means the document said nothing. bundlerConfig
		// leaves the bundler's NVIDIA-documented default in place rather than
		// overwriting it with a zero label; passing the pointer straight
		// through here preserves that distinction.
		DRAEvictionNodeLabel: resolved.DRAEvictionNodeLabel,
		WorkloadGate:         resolved.WorkloadGate,
		WorkloadSelector:     resolved.WorkloadSelector,
		Nodes:                resolved.Nodes,
		StorageClass:         resolved.StorageClass,
		SharedStorageClass:   resolved.SharedStorageClass,
		Attest:               resolved.Attest,
		CertIDRegexp:         resolved.CertIDRegexp,
		VendorCharts:         resolved.VendorCharts,
		AppName:              resolved.AppName,
		OIDCResolve: OIDCResolveOptions{
			Attest:     resolved.Attest,
			DeviceFlow: resolved.OIDCDeviceFlow,
			FulcioURL:  resolved.FulcioURL,
			RekorURL:   resolved.RekorURL,
			SigningKey: signingKey,
			// Mirrors the CLI's signingTargetFromFlags: with no explicit Rekor
			// URL, sign against the TUF-distributed signing config (Rekor v2,
			// #1650) rather than falling through transparencyForOptions to
			// NewRekorPolicy("") — public Rekor v1.
			//
			// Without this a config-driven SDK sign silently records to the
			// legacy log while the identical CLI invocation records to v2.
			// AttestationSpec carries no signing-config or TUF field, so
			// rekorURL is the only signal config can give: setting it is an
			// explicit v1 choice, and leaving it empty takes the same default
			// the CLI does.
			UseTUFSigningConfig: resolved.RekorURL == "",
		},
	}, nil
}

// BundleInputOptions carries the spec.bundle fields the CALLER consumes, not
// the bundler: which recipe to load, which image-refs file to write to, and
// where to push the finished bundle.
//
// Separate from BundleOptions on purpose. MakeBundle takes an already-resolved
// RecipeResult and does not push — the caller does, after it returns — so
// these on MakeBundle's parameter would be surface that nothing reads. The
// transport pair mirrors EvidenceOptions/SignOptions carrying
// PlainHTTP/InsecureTLS for the same "the caller reaches a registry,
// MakeBundle does not" reason.
type BundleInputOptions struct {
	RecipePath      string
	ImageRefsPath   string
	OutputTarget    *oci.Reference
	OutputTargetRaw string
	InsecureTLS     bool
	PlainHTTP       bool
}

// BundleInputOptions derives the caller-side spec.bundle settings: which
// recipe to bundle, which image-refs file to write to, where to push the
// finished bundle, and how to reach that registry. None of these reach
// MakeBundle — BundleOptions carries what the bundler itself reads.
//
// Returns the zero value for a nil Config or an absent spec.bundle, and an
// error when the section is present but malformed.
func (c *Config) BundleInputOptions() (BundleInputOptions, error) {
	if c == nil || c.internal == nil {
		return BundleInputOptions{}, nil
	}
	resolved, err := c.internal.Bundle().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return BundleInputOptions{}, err
	}
	return BundleInputOptions{
		RecipePath:      resolved.RecipeInput,
		ImageRefsPath:   resolved.ImageRefs,
		OutputTarget:    resolved.OutputTarget,
		OutputTargetRaw: resolved.OutputTargetRaw,
		InsecureTLS:     resolved.InsecureTLS,
		PlainHTTP:       resolved.PlainHTTP,
	}, nil
}

// ValidateSettings carries settings from both spec.validate.agent and
// spec.validate.execution. Fields are exported so a caller derives, then
// overrides any of them before use — the same derive-don't-apply precedence
// the other derivations use.
//
// Not every field reaches Client.ValidateState. Image, JobName,
// ServiceAccountName and RequireGPU configure the validator's own
// Kubernetes Job, but pkg/validator exposes no WithValidationImage,
// WithValidationJobName, WithValidationServiceAccountName or
// WithValidationRequireGPU option for ValidateState to accept them through —
// see options.go. They are carried here anyway so a caller (the CLI's
// parseValidateAgentConfig, in particular) can read spec.validate.agent with
// its own flag-over-config precedence instead of reaching for Unwrap(). The
// remaining nine fields (Namespace, ImagePullSecrets, NodeSelector,
// Tolerations, Phases, NoCluster, Cleanup, FailFast, Timeout) do reach
// ValidateState via a matching WithValidation* option.
type ValidateSettings struct {
	Namespace string

	// Image, JobName, ServiceAccountName and RequireGPU have no
	// WithValidation* option — see the type godoc above. A caller feeds them
	// into its own agent/Job construction instead of ValidateState.
	Image              string
	ImagePullSecrets   []string
	JobName            string
	ServiceAccountName string
	NodeSelector       map[string]string
	Tolerations        []corev1.Toleration
	RequireGPU         bool
	Phases             []Phase
	NoCluster          bool

	// Cleanup is INVERTED against spec.validate.execution.noCleanup. The
	// config field says "do not clean up"; this says "clean up". Passing it
	// through straight would delete artifacts a post-mortem asked to keep.
	Cleanup bool

	// FailFast and Timeout stay pointers so "config said nothing" remains
	// distinct from an explicit false / 0s, letting the caller's own default
	// apply rather than being overridden by a zero value.
	FailFast *bool
	Timeout  *time.Duration
}

// ValidateSettings derives settings from both spec.validate.agent and
// spec.validate.execution, as a plain value rather than an opaque option
// slice — so a caller (the CLI, in particular) can read and override
// individual fields with its own flag precedence instead of replaying a
// built option list.
//
// Thirteen settings project: namespace, image, image pull secrets, job name,
// service account name, node selector, tolerations, require-GPU, phases,
// no-cluster, cleanup, fail-fast and timeout. Only nine of them reach
// Client.ValidateState — image, job name, service account name and
// require-GPU have no WithValidation* counterpart and are carried purely for
// the caller's own agent/Job construction. See the ValidateSettings type
// godoc for which is which.
//
// # Where the rest of spec.validate goes
//
// The section is not served by this method alone, which the per-section table
// in docs/integrator/go-library.md records:
//
//   - FailOnError, RecipePath and SnapshotPath are consumed by the CALLER, not
//     by ValidateState, so a caller still needs them projected to apply its
//     own flag-over-config precedence — just not on this type.
//     ValidateInputOptions carries them.
//   - EvidenceAttestation configures the recipe-evidence bundle.
//     EvidenceAttestationOptions derives it; it is not folded in here because
//     it targets Client.EmitRecipeEvidence rather than ValidateState.
//   - EvidenceCNCF configures the CNCF AI Conformance markdown path, which has
//     no facade emission method to receive it. See EvidenceAttestationOptions
//     for why that half stays un-projected.
//
// # Two mappings that are not pass-throughs
//
// NoCleanup is INVERTED: the config field says "do not clean up", the field
// says "clean up". Passing it straight through would delete artifacts a
// post-mortem asked to keep, silently and in either direction.
//
// Phases are cast, not re-parsed, because Validation().Resolve() already
// rejects an unknown entry and names the spec field — on the WrapConfig path
// too, since that check lives in Resolve rather than in the loader.
//
// # A zero value is not a safe default
//
// Cleanup: false is the opposite of the CLI's own default (clean up). A
// caller that cannot distinguish "no spec.validate at all" from "spec.validate
// is present but silent about cleanup" and always applies the returned value
// as-is will leave the cluster-admin ClusterRoleBinding and validator Jobs
// active on the plain, no-config invocation — silently, since nothing errors.
// This is the same hazard SnapshotAgentConfig documents for Privileged; see
// "# The bool" below for the signal that resolves it.
//
// # The bool
//
// The second return reports whether spec.validate is present — true when the
// section exists (even if silent about every field), false for a nil Config,
// a nil internal document, or a document that omits the section — mirroring
// SnapshotAgentConfig's bool exactly, including why it exists: a caller
// deriving unconditionally (before it knows whether --config was even given)
// otherwise cannot tell "the document made no validate decisions, supply your
// own defaults" from "the document decided every field, apply them as-is",
// and only one of those is safe to treat as-is.
//
// Returns an error when spec.validate is present but malformed.
func (c *Config) ValidateSettings() (ValidateSettings, bool, error) {
	// The section-presence check is load-bearing and cannot be replaced by a
	// nil check on the resolved value: Resolve() returns a NON-nil
	// ValidateResolved for an absent section (same contract as
	// SnapshotSpec.Resolve), so falling through would apply Cleanup: true to a
	// document that never opted into validate configuration at all. It is also
	// exactly the presence signal the second return value surfaces.
	if c == nil || c.internal == nil || c.internal.Validation() == nil {
		return ValidateSettings{}, false, nil
	}
	resolved, err := c.internal.Validation().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return ValidateSettings{}, true, err
	}

	// Cast, don't re-parse: Validation().Resolve() rejects an unknown phase
	// before returning, on both the LoadConfig and WrapConfig paths.
	var phases []Phase
	if len(resolved.Phases) > 0 {
		phases = make([]Phase, len(resolved.Phases))
		for i, p := range resolved.Phases {
			phases[i] = Phase(p)
		}
	}

	return ValidateSettings{
		Namespace:          resolved.Namespace,
		Image:              resolved.Image,
		ImagePullSecrets:   resolved.ImagePullSecrets,
		JobName:            resolved.JobName,
		ServiceAccountName: resolved.ServiceAccountName,
		NodeSelector:       resolved.NodeSelector,
		Tolerations:        resolved.Tolerations,
		RequireGPU:         resolved.RequireGPU,
		Phases:             phases,
		NoCluster:          resolved.NoCluster,
		// Inverted on purpose. See the field godoc.
		Cleanup:  !resolved.NoCleanup,
		FailFast: resolved.FailFast,
		Timeout:  resolved.Timeout,
	}, true, nil
}

// ValidateInputOptions carries the spec.validate fields the CALLER consumes
// rather than the validator: which recipe and snapshot to validate, and
// whether a failed check should fail the caller.
//
// Separate from ValidateSettings on purpose. ValidateState takes an
// already-resolved recipe and snapshot, and it reports check results without
// acting on them — so these three on ValidateSettings would be surface the
// validator never reads. The CLI needs them to apply flag-over-config
// precedence, which is why they are derived at all.
type ValidateInputOptions struct {
	RecipePath   string
	SnapshotPath string

	// FailOnError decides whether a failed check fails the CALLER. Pointer so
	// "config said nothing" stays distinct from an explicit false, letting the
	// caller's own default apply.
	FailOnError *bool
}

// ValidateInputOptions derives spec.validate.input and
// spec.validate.execution.failOnError — the three spec.validate fields a
// caller (not the validator) consumes, so a caller applying its own
// flag-over-config precedence does not need Unwrap() to read them.
//
// Returns the zero value for a nil Config or an absent spec.validate, and an
// error when the section is present but malformed.
func (c *Config) ValidateInputOptions() (ValidateInputOptions, error) {
	if c == nil || c.internal == nil {
		return ValidateInputOptions{}, nil
	}
	resolved, err := c.internal.Validation().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return ValidateInputOptions{}, err
	}
	return ValidateInputOptions{
		RecipePath:   resolved.RecipePath,
		SnapshotPath: resolved.SnapshotPath,
		FailOnError:  resolved.FailOnError,
	}, nil
}

// EvidenceAttestationOptions derives Client.EmitRecipeEvidence's options from
// spec.validate.evidence.attestation, and reports whether the document asked
// for a recipe-evidence bundle at all.
//
// Out is the enable gate, matching the spec field's own contract: an empty
// out leaves the path off even when bom/push/plainHTTP/insecureTLS are
// populated, so a half-filled section does not start emitting evidence. False
// therefore means "not configured", not "misconfigured" — a malformed section
// is an error instead. That is why this returns a bool rather than a
// zero-value EvidenceOptions: EmitRecipeEvidence rejects an empty OutDir with
// ErrCodeInvalidRequest, so a zero value alone could not tell a caller whether
// the document declined the bundle or fumbled it.
//
// Five fields project (out, bom, push, plainHTTP, insecureTLS). The rest of
// EvidenceOptions is deliberately caller-owned:
//
//   - Commit has no spec counterpart. It selects the validator catalog the
//     bundle's BOM is built against, and it is a property of the running
//     binary rather than of the document. Set it after deriving.
//   - OIDCResolve is excluded by the spec itself: a keyless-signing identity
//     token is a short-lived secret and must not live in a version-controlled
//     file. The caller resolves it at sign time.
//   - NoSign and Full are command-line-only, for the same reason FailOnError
//     and IgnoreTLog are. Both weaken the ARTIFACT: NoSign pushes an unsigned
//     bundle, Full ships unredacted payloads. A checked-in file that can
//     quietly turn off signing is a supply-chain downgrade no reviewer would
//     see in a diff, so adding spec fields for them would close a "gap" that
//     is actually a control.
//
// # Why plainHTTP and insecureTLS project anyway
//
// They weaken a run too, so the NoSign rule above is not "config may never
// weaken anything" — stated that broadly it would be contradicted by the two
// fields three lines into the return below. The line is the ARTIFACT versus
// the HOP.
//
// PlainHTTP and InsecureTLS configure the transport to a registry the same
// document already names in push. A document trusted to choose the push
// destination is trusted to describe how to reach it, which is why
// EvidenceOptions and SignOptions carry these at all while the bundler's own
// options do not — MakeBundle never reaches a registry.
//
// Neither field changes what the bundle attests or whether it is signed, and
// that is structural rather than a promise: both reach only the OCI transport,
// never SignStatement's Fulcio/Rekor call and never predicate or redaction
// construction.
//
// How the subject digest is pinned differs by path, and neither path reads it
// back from the weakened hop. Emit-and-push binds the digest computed locally
// while packaging, before any push begins. Signing an already-pushed artifact
// resolves the digest at pull time instead, but the pull is content-addressed
// and the materialized digest is checked for equality against the value the
// original packaging run recorded, failing closed on mismatch.
//
// So a tampered hop can corrupt or break the transfer; it cannot make the
// signature vouch for content that was never packaged.
//
// That is a narrower claim than "harmless". A committed plainHTTP or
// insecureTLS does weaken that hop, and it widens the threat model rather than
// just restating it: redirecting push needs a malicious document, whereas
// downgrading TLS on a destination the operator believes is protected only
// needs someone on the network path. It is accepted here because the
// destination is already the document's call. Treat it as a reviewable
// transport decision, not as evidence that excluding NoSign and Full is
// arbitrary.
//
// # spec.validate.evidence.cncf is projected separately
//
// The evidence section carries two kinds; this method covers one.
// CNCFEvidenceOptions covers the other — it is a separate method, not folded
// in here, because the two target different consumers: this one feeds
// Client.EmitRecipeEvidence, while CNCF AI Conformance markdown has no
// Client.Emit* counterpart and is consumed directly by the caller (the CLI's
// validateFlagCombinations, cncf.New and runCNCFSubmission). Reading that
// half through Unwrap() is no longer necessary.
//
// Returns (zero, false, nil) for a nil Config, an absent spec.validate, or an
// absent evidence.attestation. An empty out also returns ok=false, but unlike
// those three absent cases the other four fields (BOMPath, Push, PlainHTTP,
// InsecureTLS) still populate from the section — only OutDir stays empty.
// That split matters because out can come from elsewhere: the CLI's
// --emit-attestation flag can supply out itself while bom/push are
// configured (buildRecipeEvidenceConfig reads att.BOMPath/att.Push
// independently of att.OutDir). Zeroing the whole struct whenever out is
// empty would silently drop that half of the configuration on every run
// where out arrives some other way. Returns an error when the section is
// present but malformed.
func (c *Config) EvidenceAttestationOptions() (EvidenceOptions, bool, error) {
	if c == nil || c.internal == nil {
		return EvidenceOptions{}, false, nil
	}
	resolved, err := c.internal.Validation().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return EvidenceOptions{}, false, err
	}
	if resolved == nil || resolved.EvidenceAttestation == nil {
		return EvidenceOptions{}, false, nil
	}
	att := resolved.EvidenceAttestation
	opts := EvidenceOptions{
		BOMPath:     att.BOM,
		Push:        att.Push,
		PlainHTTP:   att.PlainHTTP,
		InsecureTLS: att.InsecureTLS,
	}
	// The spec's own gate, not an extra one: EvidenceAttestationSpec.Out
	// documents that setting Out enables the path and an empty Out leaves it
	// off regardless of the other fields. The CLI applies the same rule in
	// buildRecipeEvidenceConfig, so honoring it here keeps a config-driven run
	// and a flag-driven run from diverging on the same document.
	//
	// The other four fields still populate above even in this branch: an
	// empty out here means "config didn't enable the path," not "config said
	// nothing about bom/push," and a caller resolving out from elsewhere
	// (e.g. a CLI flag) still needs them.
	if att.Out == "" {
		return opts, false, nil
	}
	opts.OutDir = att.Out
	return opts, true, nil
}

// CNCFEvidenceOptions carries spec.validate.evidence.cncf — the CNCF AI
// Conformance evidence-markdown settings (--evidence-dir / --cncf-submission
// / --feature). Consumed by the CALLER, not by a Client method: there is no
// Client.Emit* for CNCF evidence, so validateFlagCombinations, cncf.New and
// runCNCFSubmission read this directly. Mirrors SnapshotOutputOptions, which
// carries spec.snapshot.output the same way despite Client.CollectSnapshot
// not consuming it either.
type CNCFEvidenceOptions struct {
	// Dir is spec.validate.evidence.cncf.dir, the directory CNCF AI
	// Conformance evidence markdown is written to.
	Dir string

	// CNCFSubmission is spec.validate.evidence.cncf.cncfSubmission: whether to
	// collect detailed behavioral evidence for a CNCF AI Conformance
	// submission rather than the lighter default evidence.
	CNCFSubmission bool

	// Features is spec.validate.evidence.cncf.features, restricting collection
	// to specific evidence features. Empty means "all features" — only
	// honored when CNCFSubmission is true.
	Features []string
}

// CNCFEvidenceOptions derives spec.validate.evidence.cncf.
//
// Returns the zero value (never an error for an absent section) when the
// document has no spec.validate or no evidence.cncf block, and an error when
// spec.validate is present but malformed.
func (c *Config) CNCFEvidenceOptions() (CNCFEvidenceOptions, error) {
	if c == nil || c.internal == nil {
		return CNCFEvidenceOptions{}, nil
	}
	resolved, err := c.internal.Validation().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return CNCFEvidenceOptions{}, err
	}
	if resolved.EvidenceCNCF == nil {
		return CNCFEvidenceOptions{}, nil
	}
	return CNCFEvidenceOptions{
		Dir:            resolved.EvidenceCNCF.Dir,
		CNCFSubmission: resolved.EvidenceCNCF.CNCFSubmission,
		Features:       resolved.EvidenceCNCF.Features,
	}, nil
}

// SnapshotAgentConfig derives Client.CollectSnapshot's AgentConfig from
// spec.snapshot.
//
// These settings map onto the agent Job: namespace, image, image pull
// secrets, job name, service account, node selector, tolerations, require-GPU,
// runtime class, OS, max nodes per entry, resource requests and limits,
// timeout, cleanup and privileged.
//
// OS is parsed through the criteria registry rather than copied, matching what
// the CLI does with --os. That keeps undocumented values from reaching the
// agent, and it matters for exact matches: an unparsed "Talos" misses the
// agent's "talos" check and selects incompatible host mounts.
//
// AgentConfig's fields are exported, so a caller overrides any of them after
// deriving — the same derive-don't-apply precedence the other methods use, but
// without needing an options slice, because the type is a plain struct.
//
// # Three mappings that are not pass-throughs
//
// NoCleanup is INVERTED against Cleanup, the same shape spec.validate has.
//
// Privileged defaults to TRUE when config says nothing. The resolved field is
// a pointer precisely so "unset" stays distinct from an explicit false, and
// the CLI applies derefBoolOr(resolved.Privileged, true). Dereferencing a nil
// pointer to false here would silently drop privileges the collector needs,
// and the failure would surface as missing data rather than an error.
//
// Requests and Limits resolve as raw "name=quantity,..." strings — Resolve
// deliberately does not parse them — so they are parsed here and a malformed
// value is an error rather than a silently empty ResourceList.
//
// # What is deliberately NOT projected
//
// The whole spec.snapshot.output section is un-projected, and that is not an
// omission. Output describes DELIVERY; AgentConfig describes the collection
// Job, and the two are different concerns:
//
//   - output.format is applied at delivery. The Job always stages YAML in a
//     ConfigMap, so a format routed through AgentConfig would be silently
//     ignored (#2398).
//   - output.path and output.template are not AgentConfig.Output and
//     .TemplatePath. Per AgentConfig.Output's own godoc, any value that is not
//     a cm:// URI stages to an internal ConfigMap and delivery becomes the
//     caller's job. Projecting a file path there would look configured and
//     write nothing.
//
// Callers deliver with snapshotter.DeliverSnapshot, passing Snapshot.Raw.
//
// Kubeconfig, Debug, ClusterConfigPath, AKSGPUPoolsPath, DiscoverNetwork,
// RunID and NameBase are left at their zero values. None has a spec.snapshot
// counterpart — they are per-invocation or caller-owned. NameBase in
// particular carries the "aicr" default prefix that lets an unset job name
// stay empty while deployed objects keep their released names, which is a
// decision the caller makes, not the document.
//
// Returns a zero-value AgentConfig (never nil) when the document has no
// spec.snapshot, and an error when the section is present but malformed.
//
// A zero value is not a working configuration: Privileged is false, which the
// collector generally needs true. That is deliberate. Defaults apply when the
// section EXISTS and is silent about a field; a document with no spec.snapshot
// at all made no snapshot decisions, so the facade does not invent them. A
// caller in that position supplies its own defaults, as the CLI does from its
// flag defaults.
//
// # The bool
//
// The second return reports whether spec.snapshot is present — true when the
// section exists (even if silent about every field), false for a nil Config,
// a nil internal document, or a document that omits the section. It exists
// for the same reason EvidenceAttestationOptions returns one: a caller
// deriving unconditionally (before it knows whether --config was even given)
// otherwise cannot tell "the document made no snapshot decisions, supply your
// own defaults" from "the document decided every field, apply them as-is" —
// both produce a populated *AgentConfig, and only one of them is safe to
// treat as-is. A caller that skips this bool and always applies the returned
// value silently drops privileges: an absent section returns
// Privileged: false, which the collector generally needs true.
func (c *Config) SnapshotAgentConfig() (*AgentConfig, bool, error) {
	// A zero-value AgentConfig rather than nil, so a caller that did not supply
	// a config (or supplied one without spec.snapshot) can derive
	// unconditionally and then set the caller-owned fields — matching the
	// "returns zero values" contract in the Config godoc.
	//
	// The section-presence check is load-bearing and cannot be replaced by a
	// nil check on the resolved value: Resolve() returns a NON-nil
	// SnapshotResolved for an absent section, so falling through would apply
	// the in-section defaults (Cleanup and Privileged both true) to a document
	// that never opted into snapshot configuration at all. It is also exactly
	// the presence signal the second return value surfaces.
	if c == nil || c.internal == nil || c.internal.Snapshot() == nil {
		return &AgentConfig{}, false, nil
	}
	resolved, err := c.internal.Snapshot().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return nil, true, err
	}

	requests, err := snapshotter.ParseResourceList(resolved.Requests)
	if err != nil {
		return nil, true, errors.Wrap(errors.ErrCodeInvalidRequest,
			"invalid spec.snapshot.agent.requests", err)
	}
	limits, err := snapshotter.ParseResourceList(resolved.Limits)
	if err != nil {
		return nil, true, errors.Wrap(errors.ErrCodeInvalidRequest,
			"invalid spec.snapshot.agent.limits", err)
	}

	// The CLI parses --os through the criteria registry so only documented
	// values reach the agent and the in-pod collector factory, and so an
	// invalid value errors rather than traveling. Passing resolved.OS through
	// raw would skip both: "Talos" would miss the agent's exact "talos" match
	// and select incompatible host mounts.
	osValue := resolved.OS
	if osValue != "" {
		parsed, perr := recipe.NewCriteriaRegistry().ParseOS(osValue)
		if perr != nil {
			return nil, true, errors.Wrap(errors.ErrCodeInvalidRequest,
				"invalid spec.snapshot.agent.os", perr)
		}
		osValue = string(parsed)
	}

	cfg := &AgentConfig{
		Namespace:          resolved.Namespace,
		Image:              resolved.Image,
		ImagePullSecrets:   resolved.ImagePullSecrets,
		JobName:            resolved.JobName,
		ServiceAccountName: resolved.ServiceAccountName,
		NodeSelector:       resolved.NodeSelector,
		Tolerations:        resolved.Tolerations,
		RequireGPU:         resolved.RequireGPU,
		RuntimeClassName:   resolved.RuntimeClassName,
		OS:                 osValue,
		MaxNodesPerEntry:   resolved.MaxNodesPerEntry,
		Requests:           requests,
		Limits:             limits,
		// Inverted on purpose. See the godoc above.
		Cleanup: !resolved.NoCleanup,
		// Nil means config said nothing, and the collector's default is
		// privileged. See the godoc above.
		Privileged: resolved.Privileged == nil || *resolved.Privileged,
	}
	if resolved.Timeout != nil {
		cfg.Timeout = *resolved.Timeout
	}
	return cfg, true, nil
}

// SnapshotOutputOptions carries spec.snapshot.output — where and how a
// collected snapshot is written. Consumed by the caller performing delivery
// (snapshotter.DeliverSnapshot), not by Client.CollectSnapshot.
type SnapshotOutputOptions struct {
	// Path is spec.snapshot.output.path, the file the snapshot is written to.
	Path string

	// Format is spec.snapshot.output.format (yaml, json, or table), validated
	// by the loader.
	Format string

	// Template is spec.snapshot.output.template, a Go template rendered
	// instead of the structured formats. Requires Format yaml.
	Template string
}

// SnapshotOutputOptions derives snapshot DELIVERY settings from
// spec.snapshot.output.
//
// Deliberately separate from SnapshotAgentConfig. That method describes the
// collection Job and projects nothing from spec.snapshot.output, because output
// describes delivery and the Job always stages YAML in a ConfigMap — a format
// routed through AgentConfig would be silently ignored (#2398). These three
// fields are what a caller needs AFTER CollectSnapshot returns, to write the
// snapshot where the document asked.
//
// Returns the zero value (never an error for an absent section) when the
// document has no spec.snapshot or no output block.
func (c *Config) SnapshotOutputOptions() (SnapshotOutputOptions, error) {
	if c == nil || c.internal == nil || c.internal.Snapshot() == nil {
		return SnapshotOutputOptions{}, nil
	}
	resolved, err := c.internal.Snapshot().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return SnapshotOutputOptions{}, err
	}
	return SnapshotOutputOptions{
		Path:     resolved.OutputPath,
		Format:   resolved.OutputFormat,
		Template: resolved.OutputTemplate,
	}, nil
}

// RecipeOutputOptions carries spec.recipe.output — where and in what format a
// generated recipe is written. Consumed by the caller after ResolveRecipe
// returns, not by the resolve itself.
type RecipeOutputOptions struct {
	// Path is spec.recipe.output.path. Empty when unset.
	Path string

	// Format is spec.recipe.output.format. Empty when unset, leaving the
	// caller's own default in place.
	Format string
}

// RecipeOutputOptions derives spec.recipe.output. Returns the zero value for a
// nil Config or an absent section — never an error, because the underlying
// accessors are nil-safe and perform no parsing.
func (c *Config) RecipeOutputOptions() RecipeOutputOptions {
	if c == nil || c.internal == nil {
		return RecipeOutputOptions{}
	}
	return RecipeOutputOptions{
		Path:   c.internal.Recipe().OutputPath(),
		Format: c.internal.Recipe().OutputFormat(),
	}
}
