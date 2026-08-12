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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	bundlerconfig "github.com/NVIDIA/aicr/pkg/bundler/config"
	bundlerresult "github.com/NVIDIA/aicr/pkg/bundler/result"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/health"
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
}

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
	requireSignature[func(string) aicr.RecipeResolveOption](aicr.WithProfile)
	requireSignature[func(string) aicr.RecipeResolveOption](aicr.WithAccountingMode)

	_ = []aicr.RecipeResolveOption{
		aicr.WithProfile("profile"),
		aicr.WithAccountingMode("disabled"),
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
	requireSignature[func() (*aicr.AllowLists, error)](aicr.ParseAllowListsFromEnv)

	_ = []aicr.Option{
		aicr.WithVersion("v"),
		aicr.WithAllowLists(&aicr.AllowLists{}),
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithRecipeSource(aicr.FilesystemSource("/x")),
		aicr.WithRecipeSource(aicr.OCISource("reg", "tag")),
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
}

// TestStability_HealthAndEvidence pins the health and evidence surfaces.
func TestStability_HealthAndEvidence(t *testing.T) {
	t.Parallel()

	requireSignature[func(*aicr.Client, context.Context, *aicr.Criteria) (*health.Report, error)]((*aicr.Client).ComputeHealth)
	requireSignature[func(*aicr.Client, []*aicr.PhaseResult) *ctrf.Report]((*aicr.Client).MergeReports)
	requireSignature[func(*aicr.Client, context.Context, *aicr.RecipeResult, *aicr.Snapshot, []*aicr.PhaseResult, aicr.EvidenceOptions) error]((*aicr.Client).EmitRecipeEvidence)
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
