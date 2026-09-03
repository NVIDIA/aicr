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

// stability_test pins the public surface of pkg/client/v1 by exercising
// every exported type and function the way an out-of-tree library consumer
// would. Any change that breaks these assertions (renaming, removing, or
// changing the signature of an exported identifier) is incompatible. During
// v0 it requires explicit compatibility review and acknowledgement; starting
// with v1.0 it requires a major bump.
// Every new export must add a compile-time assertion here in the same PR.
//
// The tests do not execute network or filesystem I/O; they exist to make
// the compiler enforce the surface. A future refactor that quietly drops
// a method or renames a struct field will fail to compile here, surfacing
// the breakage before it reaches downstream consumers.

package aicr_test

import (
	"context"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	bundlerconfig "github.com/NVIDIA/aicr/pkg/bundler/config"
	bundlerresult "github.com/NVIDIA/aicr/pkg/bundler/result"
	bundleverifier "github.com/NVIDIA/aicr/pkg/bundler/verifier"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	appconfig "github.com/NVIDIA/aicr/pkg/config"
	evverifier "github.com/NVIDIA/aicr/pkg/evidence/verifier"
	"github.com/NVIDIA/aicr/pkg/health"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// TestStability_Client pins the Client constructor, its option type, and
// the lifecycle methods every consumer is expected to call.
func TestStability_Client(t *testing.T) {
	t.Parallel()

	var (
		_ *aicr.Client
		_ aicr.Option
	)
	requireSignature[func(...aicr.Option) (*aicr.Client, error)](aicr.NewClient)
	requireSignature[func(*aicr.Client) error]((*aicr.Client).Close)
	requireSignature[func(*aicr.Client, context.Context) error]((*aicr.Client).LoadCatalog)
	requireSignature[func(*aicr.Client) *aicr.CriteriaRegistry]((*aicr.Client).CriteriaRegistry)

	// Client must stay comparable. Adding an unexported func, map, or slice
	// field silently takes that away, which api-diff reports as "old is
	// comparable, new is not" — a breaking change to a frozen v1 type, caught
	// here at compile time instead of at the release gate. Any injectable
	// dependency therefore has to be held behind a pointer.
	requireComparable[aicr.Client]()
}

// requireComparable fails to compile when T stops satisfying comparable.
func requireComparable[T comparable]() {}

// requireType fails to compile when its argument is not exactly T.
//
// Used where a composite literal alone would not pin the field type: assigning
// a *time.Duration into a field still compiles if that field is `any`.
func requireType[T any](T) {}

// TestStability_RecipeResolution pins the resolution surface: the
// RecipeRequest input shape and the three Resolve* entry points.
func TestStability_RecipeResolution(t *testing.T) {
	t.Parallel()

	var req aicr.RecipeRequest
	_ = req.Profile
	_ = req.AccountingMode
	requireSignature[func(*aicr.Client, context.Context, aicr.RecipeRequest) (*aicr.RecipeResult, error)]((*aicr.Client).ResolveRecipe)
	requireSignature[func(*aicr.Client, context.Context, *aicr.Criteria) (*aicr.RecipeResult, error)]((*aicr.Client).ResolveRecipeFromCriteria)
	requireSignature[func(*aicr.Client, context.Context, *aicr.Criteria, string) (*aicr.RecipeResult, error)]((*aicr.Client).ResolveRecipeFromCriteriaWithProfile)
	requireSignature[func(*aicr.Client, context.Context, *aicr.Criteria, ...aicr.RecipeResolveOption) (*aicr.RecipeResult, error)]((*aicr.Client).ResolveRecipeFromCriteriaWithOptions)
	requireSignature[func(*aicr.Client, context.Context, *aicr.Criteria, *aicr.Snapshot) (*aicr.RecipeResult, error)]((*aicr.Client).ResolveRecipeFromSnapshot)
	requireSignature[func(*aicr.Client, context.Context, *aicr.Criteria, *aicr.Snapshot, string) (*aicr.RecipeResult, error)]((*aicr.Client).ResolveRecipeFromSnapshotWithProfile)
	requireSignature[func(*aicr.Client, context.Context, *aicr.Criteria, *aicr.Snapshot, ...aicr.RecipeResolveOption) (*aicr.RecipeResult, error)]((*aicr.Client).ResolveRecipeFromSnapshotWithOptions)
	requireSignature[func(*aicr.Client, context.Context, string, string) (*aicr.RecipeResult, error)]((*aicr.Client).LoadRecipe)
	requireSignature[func(*aicr.Client, context.Context, *aicr.AgentConfig) (*aicr.Snapshot, error)]((*aicr.Client).CollectSnapshot)
	requireSignature[func(*aicr.Client, context.Context, string, string) (*aicr.Snapshot, error)]((*aicr.Client).LoadSnapshot)
	// Snapshot-to-criteria is the step that used to require pkg/fingerprint,
	// so pinning it is what keeps the workflow completable through this package
	// alone (#2437).
	requireSignature[func(*aicr.Client, *aicr.Snapshot) (*aicr.Criteria, error)]((*aicr.Client).CriteriaFromSnapshot)

	// Mirror inventory: air-gap tooling depends on this shape, and rendering
	// deliberately stays out of the SDK (#2025).
	requireSignature[func(*aicr.Client, context.Context, *aicr.RecipeResult, ...aicr.MirrorInventoryOption) (*aicr.MirrorInventory, error)]((*aicr.Client).MirrorInventory)
	requireSignature[func([]aicr.MirrorValueOverride) aicr.MirrorInventoryOption](aicr.WithMirrorValueOverrides)

	var override aicr.MirrorValueOverride
	_ = override.Component
	_ = override.Path
	_ = override.Value
	requireSignature[func(string) aicr.MirrorInventoryOption](aicr.WithMirrorKubeVersion)

	var inventory aicr.MirrorInventory
	_ = inventory.Images
	_ = inventory.Charts
	_ = inventory.Components
	_ = inventory.RecipeVersion
	_ = inventory.Criteria

	var chart aicr.MirrorChart
	_ = chart.Name
	_ = chart.Repository
	_ = chart.Chart
	_ = chart.Version
	_ = chart.Namespace

	var component aicr.MirrorComponent
	_ = component.Component
	_ = component.Type
	_ = component.Images
	_ = component.Warnings
	requireSignature[func(string) aicr.RecipeResolveOption](aicr.WithProfile)
	requireSignature[func(string) aicr.RecipeResolveOption](aicr.WithAccountingMode)
	requireSignature[func(...aicr.CriteriaDimension) aicr.RecipeResolveOption](aicr.WithSnapshotCriteriaRelaxation)
	requireSignature[func() []aicr.CriteriaDimension](aicr.AllCriteriaDimensions)

	_ = []aicr.RecipeResolveOption{
		aicr.WithProfile("profile"),
		aicr.WithAccountingMode("disabled"),
		// Both the no-argument form (every dimension derived, all relaxable)
		// and the narrowing form are part of the contract.
		aicr.WithSnapshotCriteriaRelaxation(),
		aicr.WithSnapshotCriteriaRelaxation(aicr.DimensionOS),
	}

	// The dimension vocabulary is public API: callers name these constants to
	// declare what they stated, so renaming or dropping one breaks them.
	_ = []aicr.CriteriaDimension{
		aicr.DimensionService,
		aicr.DimensionAccelerator,
		aicr.DimensionIntent,
		aicr.DimensionOS,
		aicr.DimensionPlatform,
	}
}

// TestStability_SnapshotDiff pins the facade-owned drift-detection surface.
func TestStability_SnapshotDiff(t *testing.T) {
	t.Parallel()

	requireSignature[func(*aicr.Client, context.Context, *aicr.Snapshot, *aicr.Snapshot, aicr.SnapshotDiffOptions) (*aicr.SnapshotDiff, error)]((*aicr.Client).DiffSnapshots)
	requireSignature[func(io.Writer, *aicr.SnapshotDiff) error](aicr.WriteSnapshotDiffTable)
	requireSignature[func(*aicr.SnapshotDiff) bool]((*aicr.SnapshotDiff).HasDrift)

	var opts aicr.SnapshotDiffOptions
	_ = opts.BaselineSource
	_ = opts.TargetSource

	var result aicr.SnapshotDiff
	_ = result.BaselineSource
	_ = result.TargetSource
	_ = result.Changes
	_ = result.Summary

	var change aicr.SnapshotChange
	_ = change.Kind
	_ = change.Severity
	_ = change.Path
	_ = change.Baseline
	_ = change.Target

	var summary aicr.SnapshotDiffSummary
	_ = summary.Added
	_ = summary.Removed
	_ = summary.Modified
	_ = summary.Total

	_ = []aicr.SnapshotChangeKind{
		aicr.SnapshotChangeAdded,
		aicr.SnapshotChangeRemoved,
		aicr.SnapshotChangeModified,
	}
	_ = []aicr.SnapshotChangeSeverity{
		aicr.SnapshotChangeSeverityInfo,
	}
}

func requireSignature[T any](_ T) {}

// TestStability_RecipeResult pins the consumer-visible fields and methods
// on the result returned by every resolve/load entry point.
func TestStability_RecipeResult(t *testing.T) {
	t.Parallel()

	var r aicr.RecipeResult
	_ = r.Name
	_ = r.Version
	_ = r.Components
	_ = r.SelectedProfile
	_ = r.RelaxedDimensions
	requireSignature[func(*aicr.RecipeResult) *recipe.RecipeResult]((*aicr.RecipeResult).Resolved)
	_ = r.Resolved()
}

// TestStability_Profile pins the profile resolution and catalog projections.
func TestStability_Profile(t *testing.T) {
	t.Parallel()

	var selected aicr.SelectedProfile
	_ = selected.Name
	_ = selected.Value
	_ = selected.Advertiser
	_ = selected.OwnedPaths

	var summary aicr.ProfileSummary
	_ = summary.Name
	_ = summary.Description
	_ = summary.Default
	_ = summary.Values

	var entry aicr.CatalogEntry
	_ = entry.Profile
	requireSignature[func(*aicr.Client, context.Context, *aicr.Criteria) ([]aicr.CatalogEntry, error)]((*aicr.Client).ListCatalog)

	const (
		_ string = aicr.CatalogSourceEmbedded
		_ string = aicr.CatalogSourceExternal
	)
}

// TestStability_Bundle pins the bundle surface: options shape, MakeBundle /
// BundleComponents signatures, and AdoptRecipe for the decode-then-bundle
// REST boundary.
func TestStability_Bundle(t *testing.T) {
	t.Parallel()

	_ = aicr.BundleOptions{}
	// OIDCResolve lets a committed spec.bundle.attestation reach the
	// bundler without the caller constructing an Attester. Pinned by name
	// and type: a bare composite literal above would still compile if the
	// field were renamed or retyped.
	_ = aicr.BundleOptions{OIDCResolve: aicr.OIDCResolveOptions{}}
	requireType[aicr.OIDCResolveOptions](aicr.BundleOptions{}.OIDCResolve)
	// Config is the escape hatch a caller-built *BundleConfig still wins
	// through (see BundleOptions' "Two ways to supply the bundler
	// configuration" godoc); the CLI's own bundle-generation call and the
	// aicrd /v1/bundle handler both rely on it.
	requireType[*aicr.BundleConfig](aicr.BundleOptions{}.Config)
	// The 18 flat fields Config.BundleOptions derives from spec.bundle.
	// Pinned by name and type together: a bare zero-value literal above
	// would still compile through a rename or retype of any one of them.
	_ = aicr.BundleOptions{
		Deployer:                   bundlerconfig.DeployerHelm,
		Repo:                       "",
		ValueOverrides:             nil,
		DynamicValues:              nil,
		SystemNodeSelector:         nil,
		SystemNodeTolerations:      nil,
		AcceleratedNodeSelector:    nil,
		AcceleratedNodeTolerations: nil,
		DRAEvictionNodeLabel:       nil,
		WorkloadGate:               nil,
		WorkloadSelector:           nil,
		Nodes:                      0,
		StorageClass:               "",
		SharedStorageClass:         "",
		Attest:                     false,
		CertIDRegexp:               "",
		VendorCharts:               false,
		AppName:                    "",
	}
	requireType[bundlerconfig.DeployerType](aicr.BundleOptions{}.Deployer)
	requireType[[]bundlerconfig.ComponentPath](aicr.BundleOptions{}.ValueOverrides)
	requireType[[]bundlerconfig.ComponentPath](aicr.BundleOptions{}.DynamicValues)
	requireType[[]corev1.Toleration](aicr.BundleOptions{}.SystemNodeTolerations)
	requireType[[]corev1.Toleration](aicr.BundleOptions{}.AcceleratedNodeTolerations)
	requireType[*bundlerconfig.NodeLabel](aicr.BundleOptions{}.DRAEvictionNodeLabel)
	requireType[*corev1.Taint](aicr.BundleOptions{}.WorkloadGate)
	// The three selectors are pinned by TYPE, not only by the nil literals in
	// the struct above: an untyped nil satisfies any map or pointer, so the
	// literal alone would keep compiling if one of these were retyped.
	requireType[map[string]string](aicr.BundleOptions{}.SystemNodeSelector)
	requireType[map[string]string](aicr.BundleOptions{}.AcceleratedNodeSelector)
	requireType[map[string]string](aicr.BundleOptions{}.WorkloadSelector)
	requireType[int](aicr.BundleOptions{}.Nodes)
	requireType[bool](aicr.BundleOptions{}.Attest)
	requireType[bool](aicr.BundleOptions{}.VendorCharts)

	var bi aicr.BundleInputOptions
	_ = bi.RecipePath
	_ = bi.ImageRefsPath
	_ = bi.OutputTarget
	_ = bi.OutputTargetRaw
	_ = bi.InsecureTLS
	_ = bi.PlainHTTP
	requireType[*oci.Reference](aicr.BundleInputOptions{}.OutputTarget)

	requireSignature[func(*aicr.Client, context.Context, *recipe.RecipeResult) (*aicr.RecipeResult, error)]((*aicr.Client).AdoptRecipe)
	requireSignature[func(*aicr.Client, context.Context, *aicr.RecipeResult, aicr.BundleOptions) (aicr.BundleArtifact, error)]((*aicr.Client).MakeBundle)
	requireSignature[func(*aicr.Client, context.Context, *aicr.RecipeResult) ([]aicr.ComponentBundle, error)]((*aicr.Client).BundleComponents)
}

// TestStability_Validate pins the validation surface and every
// WithValidation* option exported today.
func TestStability_Validate(t *testing.T) {
	t.Parallel()

	requireSignature[func(*aicr.Client, context.Context, *aicr.RecipeResult, *aicr.Snapshot, ...aicr.ValidateOption) ([]*aicr.PhaseResult, error)]((*aicr.Client).ValidateState)
	requireSignature[func(string) aicr.ValidateOption](aicr.WithValidationKubeconfig)
	requireSignature[func(string) aicr.ValidateOption](aicr.WithValidationNamespace)
	requireSignature[func(string) aicr.ValidateOption](aicr.WithValidationRunID)
	requireSignature[func(bool) aicr.ValidateOption](aicr.WithValidationCleanup)
	requireSignature[func([]string) aicr.ValidateOption](aicr.WithValidationImagePullSecrets)
	requireSignature[func(bool) aicr.ValidateOption](aicr.WithValidationNoCluster)
	requireSignature[func([]corev1.Toleration) aicr.ValidateOption](aicr.WithValidationTolerations)
	requireSignature[func(time.Duration) aicr.ValidateOption](aicr.WithValidationTimeout)
	requireSignature[func(map[string]string) aicr.ValidateOption](aicr.WithValidationNodeSelector)
	requireSignature[func(...aicr.Phase) aicr.ValidateOption](aicr.WithValidationPhases)
	requireSignature[func(string) aicr.ValidateOption](aicr.WithValidationCommit)
	requireSignature[func(string) aicr.ValidateOption](aicr.WithValidationImageRegistryOverride)
	requireSignature[func(string) aicr.ValidateOption](aicr.WithValidationImageTagOverride)
	requireSignature[func(bool) aicr.ValidateOption](aicr.WithValidationFailFast)

	_ = []aicr.ValidateOption{
		aicr.WithValidationKubeconfig("/path/to/kubeconfig"),
		aicr.WithValidationNamespace("ns"),
		aicr.WithValidationRunID("rid"),
		aicr.WithValidationCleanup(true),
		aicr.WithValidationImagePullSecrets([]string{"ips"}),
		aicr.WithValidationNoCluster(true),
		aicr.WithValidationTolerations([]corev1.Toleration{}),
		aicr.WithValidationTimeout(time.Second),
		aicr.WithValidationNodeSelector(map[string]string{"k": "v"}),
		aicr.WithValidationPhases(aicr.PhaseDeployment),
		aicr.WithValidationCommit("sha"),
		aicr.WithValidationImageRegistryOverride("reg"),
		aicr.WithValidationImageTagOverride("tag"),
		aicr.WithValidationFailFast(true),
	}
}

// TestStability_ClientOptions pins WithVersion / WithAllowLists / and the
// recipe-source factories an out-of-tree consumer uses to construct a
// Client.
func TestStability_ClientOptions(t *testing.T) {
	t.Parallel()

	requireSignature[func(string) aicr.Option](aicr.WithVersion)
	requireSignature[func(*aicr.AllowLists) aicr.Option](aicr.WithAllowLists)
	requireSignature[func(aicr.RecipeSourceOption) aicr.Option](aicr.WithRecipeSource)
	requireSignature[func() aicr.RecipeSourceOption](aicr.EmbeddedSource)
	requireSignature[func(string) aicr.RecipeSourceOption](aicr.FilesystemSource)
	requireSignature[func(string, string) aicr.RecipeSourceOption](aicr.OCISource)
	requireSignature[func(context.Context, ...aicr.Option) (*aicr.Client, error)](aicr.NewClientContext)
	requireSignature[func(string) aicr.Option](aicr.WithOCISourceTempDir)
	requireSignature[func() (*aicr.AllowLists, error)](aicr.ParseAllowListsFromEnv)

	_ = []aicr.Option{
		aicr.WithVersion("v"),
		aicr.WithAllowLists(&aicr.AllowLists{}),
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithRecipeSource(aicr.FilesystemSource("/x")),
		aicr.WithRecipeSource(aicr.OCISource("reg", "tag")),
		aicr.WithOCISourceTempDir("/tmp"),
	}
}

// TestStability_TypesAndAliases pins the consumer-visible structs and the
// transparent aliases. The aliases are documented internal-target aliases;
// keeping them assignable from external code is part of the contract.
func TestStability_TypesAndAliases(t *testing.T) {
	t.Parallel()

	_ = aicr.Criteria{}
	_ = aicr.AllowLists{}
	_ = aicr.AgentConfig{}
	// Pinned by name and type, not just shape: Config.SnapshotAgentConfig
	// populates these, and the bare literal above would still compile
	// through a rename or retype. Cleanup and Privileged especially --
	// both are derived through a transform (an inversion and a
	// defaults-to-true), so a silent flip has no other guard here.
	_ = aicr.AgentConfig{
		Namespace: "", Image: "", JobName: "", ServiceAccountName: "",
		RuntimeClassName: "", OS: "", Output: "", TemplatePath: "",
		MaxNodesPerEntry: 0, RequireGPU: false, Cleanup: false, Privileged: false,
	}
	requireType[bool](aicr.AgentConfig{}.Cleanup)
	requireType[bool](aicr.AgentConfig{}.Privileged)
	requireType[time.Duration](aicr.AgentConfig{}.Timeout)
	_ = aicr.SnapshotOutputOptions{Path: "", Format: "", Template: ""}
	_ = aicr.RecipeOutputOptions{Path: "", Format: ""}
	_ = aicr.Snapshot{}
	_ = aicr.Snapshot{}.Raw
	_ = aicr.ReportSummary{}
	_ = aicr.PhaseResult{}
	_ = aicr.ComponentBundle{}
	_ = aicr.ComponentRef{}
	_ = aicr.RecipeSourceOption{}
	_ = aicr.EvidenceOptions{}

	// Phase is a string-backed enum; callers spell phases as Phase("name").
	const (
		_ aicr.Phase = aicr.PhaseDeployment
		_ aicr.Phase = aicr.PhasePerformance
		_ aicr.Phase = aicr.PhaseConformance
	)

	// Type-alias surface — these are explicitly documented as alias passthroughs
	// and are exercised here so a future drop or retype is a compile error. The
	// API-diff gate separately scopes checks to each target definition.
	//nolint:staticcheck // QF1011: explicit types pin the aliases' target types.
	var (
		_ *recipe.CriteriaRegistry    = (*aicr.CriteriaRegistry)(nil)
		_ bundleattest.Attester       = (aicr.BundleAttester)(nil)
		_ *bundlerresult.Output       = (aicr.BundleArtifact)(nil)
		_ *bundlerconfig.Config       = (*aicr.BundleConfig)(nil)
		_ bundleattest.ResolveOptions = aicr.OIDCResolveOptions{}
	)
}

// TestStability_Translations pins the facade/internal boundary helpers.
func TestStability_Translations(t *testing.T) {
	t.Parallel()

	requireSignature[func(*snapshotter.Snapshot) *aicr.Snapshot](aicr.WrapSnapshot)
	requireSignature[func(*aicr.Snapshot) *snapshotter.Snapshot]((*aicr.Snapshot).Unwrap)
	requireSignature[func(*recipe.Criteria) *aicr.Criteria](aicr.WrapCriteria)
	requireSignature[func(*recipe.AllowLists) *aicr.AllowLists](aicr.WrapAllowLists)
	requireSignature[func(*aicr.AllowLists) *recipe.AllowLists](aicr.ToInternalAllowLists)
	requireSignature[func(*aicr.Criteria) *recipe.Criteria](aicr.ToInternalCriteria)
}

// TestStability_HealthAndEvidence pins the health and evidence surfaces.
func TestStability_HealthAndEvidence(t *testing.T) {
	t.Parallel()

	requireSignature[func(*aicr.Client, context.Context, *aicr.Criteria) (*health.Report, error)]((*aicr.Client).ComputeHealth)
	requireSignature[func(*aicr.Client, []*aicr.PhaseResult) *ctrf.Report]((*aicr.Client).MergeReports)
	requireSignature[func(*aicr.Client, context.Context, *aicr.RecipeResult, *aicr.Snapshot, []*aicr.PhaseResult, aicr.EvidenceOptions) error]((*aicr.Client).EmitRecipeEvidence)
}

// TestStability_Verification pins the consumer-side verification surface: the
// four Client-bound entry points, the stateless primitives, the option and
// result shapes, and the re-exported verdict constants a CI gate branches on.
func TestStability_Verification(t *testing.T) {
	t.Parallel()

	requireSignature[func(*aicr.Client, context.Context, string, aicr.BundleVerifyOptions) (*aicr.BundleVerification, error)]((*aicr.Client).VerifyBundle)
	requireSignature[func(*aicr.Client, context.Context, aicr.EvidenceVerifyOptions) (*aicr.EvidenceVerification, error)]((*aicr.Client).VerifyEvidence)
	requireSignature[func(*aicr.Client, context.Context, string, aicr.CatalogVerifyOptions) (*aicr.CatalogVerification, error)]((*aicr.Client).VerifyCatalog)
	requireSignature[func(*aicr.Client, context.Context, aicr.RecipeDigestOptions) (string, error)]((*aicr.Client).RecipeDigest)

	requireSignature[func(context.Context, aicr.BinaryAttestationVerifyOptions) (string, error)](aicr.VerifyBinaryAttestation)
	requireSignature[func(string) error](aicr.ValidateIdentityPattern)
	requireSignature[func() []string](aicr.TrustLevels)
	requireSignature[func(*aicr.EvidenceVerification) ([]byte, error)](aicr.RenderEvidenceJSON)
	requireSignature[func(*aicr.EvidenceVerification) string](aicr.RenderEvidenceMarkdown)

	// The per-call cap override is part of the contract: without it the facade
	// ceiling is unconditional and a slow-registry verification cannot finish
	// (#2225). Nil must keep the default, so the field's presence and its
	// pointer-ness both matter.
	var timeoutOverride *time.Duration
	_ = aicr.BundleVerifyOptions{Timeout: timeoutOverride}
	_ = aicr.EvidenceVerifyOptions{Timeout: timeoutOverride}
	_ = aicr.CatalogVerifyOptions{Timeout: timeoutOverride}
	_ = aicr.RecipeDigestOptions{Timeout: timeoutOverride}

	// Read the field back AS a *time.Duration too. The literals above still
	// compile if the field widens to `any`, which would silently drop the
	// nil-vs-zero distinction the whole design rests on.
	requireType[*time.Duration](aicr.BundleVerifyOptions{}.Timeout)
	requireType[*time.Duration](aicr.EvidenceVerifyOptions{}.Timeout)
	requireType[*time.Duration](aicr.CatalogVerifyOptions{}.Timeout)
	requireType[*time.Duration](aicr.RecipeDigestOptions{}.Timeout)

	var bv aicr.BundleVerifyOptions
	_ = bv.CertificateIdentityRegexp
	_ = bv.Key
	_ = bv.TrustRoot
	_ = bv.MinTrustLevel
	_ = bv.RequireCreator
	_ = bv.CLIVersionConstraint
	_ = bv.IgnoreTLog

	var verification aicr.BundleVerification
	_ = verification.Report
	_ = verification.PolicyFailure

	var ev aicr.EvidenceVerifyOptions
	_ = ev.Input
	_ = ev.BundleRef
	_ = ev.ExpectedIssuer
	_ = ev.ExpectedIdentityRegexp
	_ = ev.PlainHTTP
	_ = ev.InsecureTLS
	_ = ev.AllowUnpinnedTag

	_ = aicr.CatalogVerifyOptions{}.CertificateIdentityRegexp
	_ = aicr.CatalogVerification{}.Identity
	_ = aicr.CatalogVerification{}.Digest

	var rd aicr.RecipeDigestOptions
	_ = rd.Path
	_ = rd.Kubeconfig
	_ = rd.Profile

	var ba aicr.BinaryAttestationVerifyOptions
	_ = ba.Attestation
	_ = ba.BinaryDigest
	_ = ba.IdentityRegexp

	const (
		_ string = aicr.TrustedIdentityPattern
		_ string = aicr.EvidenceCauseCanceled
		_ int    = aicr.EvidenceExitValidPassed
		_ int    = aicr.EvidenceExitValidPhaseFailures
		_ int    = aicr.EvidenceExitInvalid
		_ int    = aicr.EvidenceExitIncomplete
	)

	// Transparent aliases over the two report trees. Same contract as the
	// aliases in TestStability_TypesAndAliases: keeping them assignable
	// from external code is part of the promise.
	//nolint:staticcheck // QF1011: explicit types pin the aliases' target types.
	var (
		_ *bundleverifier.VerifyResult = (*aicr.BundleVerifyReport)(nil)
		_ *evverifier.VerifyResult     = (*aicr.EvidenceVerification)(nil)
	)
}

// TestStability_Signing pins the producer-side supply-chain surface.
func TestStability_Signing(t *testing.T) {
	t.Parallel()

	requireSignature[func(*aicr.Client, context.Context, aicr.EvidencePublishOptions) error]((*aicr.Client).PublishEvidence)
	requireSignature[func(*aicr.Client, context.Context, aicr.CatalogSignOptions) (*aicr.CatalogSignResult, error)]((*aicr.Client).SignCatalog)

	var ep aicr.EvidencePublishOptions
	_ = ep.BundleDir
	_ = ep.Push
	_ = ep.PlainHTTP
	_ = ep.InsecureTLS
	_ = ep.NoSign
	_ = ep.OIDCResolve

	var cs aicr.CatalogSignOptions
	_ = cs.Output
	_ = cs.OIDCResolve

	_ = aicr.CatalogSignResult{}.Digest
	_ = aicr.CatalogSignResult{}.BundleJSON
}

// TestStability_Config pins the AICRConfig binding: loading, the bridge from
// an externally-parsed document, and the per-section derivations.
func TestStability_Config(t *testing.T) {
	t.Parallel()

	requireSignature[func(context.Context, string) (*aicr.Config, error)](aicr.LoadConfig)
	requireSignature[func(*appconfig.AICRConfig) *aicr.Config](aicr.WrapConfig)
	requireSignature[func(*aicr.Config) *appconfig.AICRConfig]((*aicr.Config).Unwrap)

	requireSignature[func(*aicr.Config) (aicr.BundleVerifyOptions, error)]((*aicr.Config).BundleVerifyOptions)
	requireSignature[func(*aicr.Config) (aicr.BundleOptions, error)]((*aicr.Config).BundleOptions)
	requireSignature[func(*aicr.Config) (aicr.BundleInputOptions, error)]((*aicr.Config).BundleInputOptions)
	requireSignature[func(*aicr.Config) (aicr.ValidateSettings, bool, error)]((*aicr.Config).ValidateSettings)
	requireSignature[func(*aicr.Config) (aicr.ValidateInputOptions, error)]((*aicr.Config).ValidateInputOptions)
	requireSignature[func(*aicr.Config) (aicr.CNCFEvidenceOptions, error)]((*aicr.Config).CNCFEvidenceOptions)
	requireSignature[func(*aicr.Config) (*aicr.AgentConfig, bool, error)]((*aicr.Config).SnapshotAgentConfig)
	requireSignature[func(*aicr.Config) (aicr.SnapshotOutputOptions, error)]((*aicr.Config).SnapshotOutputOptions)
	requireSignature[func(*aicr.Config) aicr.RecipeOutputOptions]((*aicr.Config).RecipeOutputOptions)

	// ValidateSettings and ValidateInputOptions pinned by field name AND type
	// together, mirroring BundleInputOptions above (TestStability_Bundle): a
	// bare read of each field would still compile through a retype (FailFast
	// *bool -> bool, Timeout *time.Duration -> time.Duration, both of which
	// destroy the nil-vs-explicit-zero distinction the design rests on) or a
	// silently-flipped Cleanup polarity.
	var vs aicr.ValidateSettings
	_ = vs.Namespace
	_ = vs.Image
	_ = vs.ImagePullSecrets
	_ = vs.JobName
	_ = vs.ServiceAccountName
	_ = vs.NodeSelector
	_ = vs.Tolerations
	_ = vs.RequireGPU
	_ = vs.Phases
	_ = vs.NoCluster
	_ = vs.Cleanup
	_ = vs.FailFast
	_ = vs.Timeout
	requireType[*bool](aicr.ValidateSettings{}.FailFast)
	requireType[*time.Duration](aicr.ValidateSettings{}.Timeout)
	requireType[bool](aicr.ValidateSettings{}.Cleanup)

	var vi aicr.ValidateInputOptions
	_ = vi.RecipePath
	_ = vi.SnapshotPath
	_ = vi.FailOnError
	requireType[*bool](aicr.ValidateInputOptions{}.FailOnError)

	// CNCFEvidenceOptions pinned the same way as its SnapshotOutputOptions
	// sibling (TestStability_Bundle's aicr.BundleInputOptions block): field
	// reads plus an explicit type pin on the one slice field.
	var ce aicr.CNCFEvidenceOptions
	_ = ce.Dir
	_ = ce.CNCFSubmission
	_ = ce.Features
	// Pin the scalars by TYPE too, not only by presence: a bare `_ = ce.Dir`
	// keeps compiling if Dir becomes a named string type or CNCFSubmission
	// becomes a *bool, which would silently change the nil-vs-set contract.
	requireType[string](aicr.CNCFEvidenceOptions{}.Dir)
	requireType[bool](aicr.CNCFEvidenceOptions{}.CNCFSubmission)
	requireType[[]string](aicr.CNCFEvidenceOptions{}.Features)
	// The bool is load-bearing, not decoration: it separates "the document
	// declined the bundle" from "the document fumbled it", which a zero
	// EvidenceOptions alone cannot express. Dropping it would compile at every
	// call site that ignores it.
	requireSignature[func(*aicr.Config) (aicr.EvidenceOptions, bool, error)]((*aicr.Config).EvidenceAttestationOptions)
	requireSignature[func(*aicr.Config) string]((*aicr.Config).RecipeProfile)
	requireSignature[func(*aicr.Config) (string, bool, error)]((*aicr.Config).RecipeAccountingMode)
	requireSignature[func(*aicr.Config) (aicr.RecipeSourceOption, bool)]((*aicr.Config).RecipeSource)
	requireSignature[func(*aicr.Config, *aicr.CriteriaRegistry) (*aicr.Criteria, error)]((*aicr.Config).RecipeCriteria)
	requireSignature[func(*aicr.Config) ([]aicr.RecipeResolveOption, error)]((*aicr.Config).RecipeResolveOptions)
	requireSignature[func(*aicr.Config) string]((*aicr.Config).SnapshotPath)
	requireSignature[func(*aicr.Config) bool]((*aicr.Config).IsCriteriaStrict)
}

// TestStability_Query pins the package-level query selector, in both its
// context-aware and legacy context-less spellings, plus the WrapResolved
// constructor that makes an externally-projected pkg/recipe.RecipeResult
// queryable through the facade.
func TestStability_Query(t *testing.T) {
	t.Parallel()

	requireSignature[func(*aicr.RecipeResult, string) (any, error)](aicr.SelectFromRecipe)
	requireSignature[func(context.Context, *aicr.RecipeResult, string) (any, error)](aicr.SelectFromRecipeWithContext)
	requireSignature[func(*recipe.RecipeResult) *aicr.RecipeResult](aicr.WrapResolved)
}
