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

	bundlerconfig "github.com/NVIDIA/aicr/pkg/bundler/config"
	appconfig "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
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

// BundleOptions derives Client.MakeBundle options from spec.bundle.
//
// Config carries the 18 bundler settings the section configures — deployment
// (deployer, repo, value overrides, dynamic values, vendoring, app name),
// scheduling (system/accelerated selectors and tolerations, DRA eviction
// label, workload gate and selector, node count, storage classes), and the
// two attestation flags the bundler itself reads (attest, certificate
// identity regexp). OIDCResolve carries what reaches the attester rather than
// the bundler: the Attest gate, DeviceFlow, FulcioURL, RekorURL, SigningKey,
// and the derived UseTUFSigningConfig.
//
// # What is deliberately NOT projected
//
// Six resolved fields have no BundleOptions counterpart, by design:
//
//   - RecipeInput names which recipe to bundle. The caller already passes
//     the recipe to MakeBundle, so projecting it would give the same
//     decision two homes.
//   - OutputTarget, OutputTargetRaw and ImageRefs are output destinations
//     chosen per invocation. OutputDir is the analog MakeBundle honors,
//     and the CLI owns flag-vs-config precedence for the rest.
//   - InsecureTLS and PlainHTTP configure OCI transport. MakeBundle does not
//     push — the CLI does, after it returns — so a field here would be
//     surface that nothing reads. EvidenceOptions and SignOptions carry them
//     because those operations do reach a registry.
//
// Reading any of the six still requires Unwrap(), which is the signal the
// type's godoc describes: a field that is genuinely un-projected, not one
// whose derivation is missing.
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
	if resolved == nil {
		return BundleOptions{}, nil
	}

	opts := []bundlerconfig.Option{
		bundlerconfig.WithDeployer(resolved.Deployer),
		bundlerconfig.WithRepoURL(resolved.Repo),
		bundlerconfig.WithValueOverridePaths(resolved.ValueOverrides),
		bundlerconfig.WithDynamicValuePaths(resolved.DynamicValues),
		bundlerconfig.WithSystemNodeSelector(resolved.SystemNodeSelector),
		bundlerconfig.WithSystemNodeTolerations(resolved.SystemNodeTolerations),
		bundlerconfig.WithAcceleratedNodeSelector(resolved.AcceleratedNodeSelector),
		bundlerconfig.WithAcceleratedNodeTolerations(resolved.AcceleratedNodeTolerations),
		bundlerconfig.WithWorkloadGateTaint(resolved.WorkloadGate),
		bundlerconfig.WithWorkloadSelector(resolved.WorkloadSelector),
		bundlerconfig.WithEstimatedNodeCount(resolved.Nodes),
		bundlerconfig.WithStorageClass(resolved.StorageClass),
		bundlerconfig.WithSharedStorageClass(resolved.SharedStorageClass),
		bundlerconfig.WithVendorCharts(resolved.VendorCharts),
		bundlerconfig.WithAppName(resolved.AppName),
		bundlerconfig.WithAttest(resolved.Attest),
		bundlerconfig.WithCertificateIdentityRegexp(resolved.CertIDRegexp),
	}
	// DRAEvictionNodeLabel resolves as a pointer specifically so "unset" is
	// distinguishable from "set to the zero label", and the option takes a
	// value. Appending unconditionally would dereference nil and, worse,
	// would overwrite the NVIDIA-documented default the bundler applies when
	// config said nothing.
	if resolved.DRAEvictionNodeLabel != nil {
		opts = append(opts, bundlerconfig.WithDRAEvictionNodeLabel(*resolved.DRAEvictionNodeLabel))
	}

	// Signing mode is exclusive: a KMS key or keyless OIDC, never both.
	// ResolveAttesterLazy picks KMS whenever SigningKey is non-empty, so a
	// document setting both would silently sign with the key while its
	// fulcioURL/oidcDeviceFlow settings did nothing. The CLI rejects that
	// combination (validateSigningKeyExclusivity, and
	// TestValidateSigningKeyExclusivity_ConfigSourcedConflict covers exactly
	// the config-sourced case) — enforcing it only there would leave SDK
	// callers with the silent behavior.
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
	if signingKey != "" {
		for _, conflict := range []struct {
			field  string
			active bool
		}{
			{"oidcDeviceFlow", resolved.OIDCDeviceFlow},
			{"fulcioURL", resolved.FulcioURL != ""},
		} {
			if conflict.active {
				return BundleOptions{}, errors.New(errors.ErrCodeInvalidRequest,
					"spec.bundle.attestation.signingKey is mutually exclusive with "+
						"spec.bundle.attestation."+conflict.field)
			}
		}
	}

	return BundleOptions{
		Config: bundlerconfig.NewConfig(opts...),
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

// ValidateOptions derives Client.ValidateState options from spec.validate.
//
// Nine settings map onto the WithValidation* option set: namespace, image pull
// secrets, node selector, tolerations, no-cluster, cleanup, phases, fail-fast
// and timeout. The returned slice is appendable, so a caller layers its own
// options after the derived ones and the later value wins — the same shape
// RecipeResolveOptions uses, and the reason this returns options rather than a
// built value.
//
// # Where the rest of spec.validate goes
//
// The section is not served by one destination, which the per-section table in
// docs/integrator/go-library.md now records:
//
//   - Image, JobName, ServiceAccountName and RequireGPU configure the
//     validator's Kubernetes Job, and reach it through AgentConfig rather than
//     a validator option — pkg/validator exposes no With* for any of them.
//     Deriving them here would produce options with nothing to translate into.
//   - FailOnError decides whether a failed check makes the CALLER fail. The
//     validator reports; it does not act on it, so there is nothing to pass
//     through. Command-line-only for the same reason IgnoreTLog is on
//     BundleVerifyOptions: a checked-in file should not be able to make a
//     failing run report success.
//   - RecipePath and SnapshotPath name what to validate. The caller already
//     passes both to ValidateState.
//   - EvidenceAttestation configures the recipe-evidence bundle.
//     EvidenceAttestationOptions derives it; it is not folded in here because
//     it targets Client.EmitRecipeEvidence rather than ValidateState.
//   - EvidenceCNCF configures the CNCF AI Conformance markdown path, which has
//     no facade emission method to receive it. See EvidenceAttestationOptions
//     for why that half stays un-projected.
//
// # Two mappings that are not pass-throughs
//
// NoCleanup is INVERTED: the config field says "do not clean up", the option
// says "clean up". Passing it straight through would delete artifacts a
// post-mortem asked to keep, silently and in either direction.
//
// Phases are cast, not re-parsed, because Validation().Resolve() already
// rejects an unknown entry and names the spec field — on the WrapConfig path
// too, since that check lives in Resolve rather than in the loader.
//
// Returns an error when spec.validate is present but malformed.
func (c *Config) ValidateOptions() ([]ValidateOption, error) {
	if c == nil || c.internal == nil {
		return nil, nil
	}
	resolved, err := c.internal.Validation().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return nil, err
	}
	if resolved == nil {
		return nil, nil
	}

	opts := []ValidateOption{
		WithValidationNamespace(resolved.Namespace),
		WithValidationImagePullSecrets(resolved.ImagePullSecrets),
		WithValidationNodeSelector(resolved.NodeSelector),
		WithValidationTolerations(resolved.Tolerations),
		WithValidationNoCluster(resolved.NoCluster),
		// Inverted on purpose. See the godoc above.
		WithValidationCleanup(!resolved.NoCleanup),
	}

	if len(resolved.Phases) > 0 {
		// Cast, don't re-parse. Validation().Resolve() rejects an unknown phase
		// before returning — on both the LoadConfig and WrapConfig paths, since
		// the check lives in Resolve rather than the loader — so by here every
		// entry is known-good. A defensive re-parse would be unreachable, and
		// unreachable validation reads as a guarantee nobody is actually
		// providing.
		facade := make([]Phase, len(resolved.Phases))
		for i, p := range resolved.Phases {
			facade[i] = Phase(p)
		}
		opts = append(opts, WithValidationPhases(facade...))
	}
	// FailFast and Timeout resolve as pointers so "unset" stays distinct from
	// an explicit false/0, and both options take values. Emitting them
	// unconditionally would turn "config said nothing" into an explicit
	// choice, overriding the validator's own default.
	if resolved.FailFast != nil {
		opts = append(opts, WithValidationFailFast(*resolved.FailFast))
	}
	if resolved.Timeout != nil {
		opts = append(opts, WithValidationTimeout(*resolved.Timeout))
	}
	return opts, nil
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
//     and IgnoreTLog are. Both WEAKEN a run — NoSign pushes an unsigned
//     bundle, Full ships unredacted payloads — and a checked-in file that can
//     quietly turn off signing is a supply-chain downgrade no reviewer would
//     see in a diff. Adding spec fields for them would close a "gap" that is
//     actually a control.
//
// # spec.validate.evidence.cncf is NOT projected
//
// The evidence section carries two kinds; this method covers one. CNCF AI
// Conformance emission has no facade entry point at all — there is no
// Client.Emit* that consumes dir/cncfSubmission/features — so there is
// nothing for a derivation to feed. Projecting it would mean designing the
// emission API, not mapping config onto an existing one. Reading that half
// still needs Unwrap(), and this method is named for the half it carries so
// the name cannot drift into covering both.
//
// Returns (zero, false, nil) for a nil Config, an absent spec.validate, an
// absent evidence.attestation, or an empty out, and an error when the section
// is present but malformed.
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
	// The spec's own gate, not an extra one: EvidenceAttestationSpec.Out
	// documents that setting Out enables the path and an empty Out leaves it
	// off regardless of the other fields. The CLI applies the same rule in
	// buildRecipeEvidenceConfig, so honoring it here keeps a config-driven run
	// and a flag-driven run from diverging on the same document.
	if att.Out == "" {
		return EvidenceOptions{}, false, nil
	}
	return EvidenceOptions{
		OutDir:      att.Out,
		BOMPath:     att.BOM,
		Push:        att.Push,
		PlainHTTP:   att.PlainHTTP,
		InsecureTLS: att.InsecureTLS,
	}, true, nil
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
func (c *Config) SnapshotAgentConfig() (*AgentConfig, error) {
	// A zero-value AgentConfig rather than nil, so a caller that did not supply
	// a config (or supplied one without spec.snapshot) can derive
	// unconditionally and then set the caller-owned fields — matching the
	// "returns zero values" contract in the Config godoc.
	//
	// The section-presence check is load-bearing and cannot be replaced by a
	// nil check on the resolved value: Resolve() returns a NON-nil
	// SnapshotResolved for an absent section, so falling through would apply
	// the in-section defaults (Cleanup and Privileged both true) to a document
	// that never opted into snapshot configuration at all.
	if c == nil || c.internal == nil || c.internal.Snapshot() == nil {
		return &AgentConfig{}, nil
	}
	resolved, err := c.internal.Snapshot().Resolve()
	if err != nil {
		// Already coded, and the message carries the spec path that failed.
		return nil, err
	}
	if resolved == nil {
		return &AgentConfig{}, nil
	}

	requests, err := snapshotter.ParseResourceList(resolved.Requests)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"invalid spec.snapshot.agent.requests", err)
	}
	limits, err := snapshotter.ParseResourceList(resolved.Limits)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
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
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
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
	return cfg, nil
}
