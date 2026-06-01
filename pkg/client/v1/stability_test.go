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
// would. The package follows semver: any change that breaks these
// assertions (renaming, removing, or changing the signature of an
// exported identifier) is a breaking change that requires a major bump.
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

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// TestStability_Client pins the Client constructor, its option type, and
// the lifecycle methods every consumer is expected to call.
func TestStability_Client(t *testing.T) {
	t.Parallel()

	var (
		_ *aicr.Client
		_ aicr.Option
	)
	_ = func() (*aicr.Client, error) { return aicr.NewClient() }
	_ = func(c *aicr.Client) error { return c.Close() }
	_ = func(c *aicr.Client, ctx context.Context) error { return c.LoadCatalog(ctx) }
	_ = func(c *aicr.Client) *aicr.CriteriaRegistry { return c.CriteriaRegistry() }
}

// TestStability_RecipeResolution pins the resolution surface: the
// RecipeRequest input shape and the three Resolve* entry points.
func TestStability_RecipeResolution(t *testing.T) {
	t.Parallel()

	_ = aicr.RecipeRequest{}

	_ = func(c *aicr.Client, ctx context.Context, req aicr.RecipeRequest) (*aicr.RecipeResult, error) {
		return c.ResolveRecipe(ctx, req)
	}
	_ = func(c *aicr.Client, ctx context.Context, cr *aicr.Criteria) (*aicr.RecipeResult, error) {
		return c.ResolveRecipeFromCriteria(ctx, cr)
	}
	_ = func(c *aicr.Client, ctx context.Context, cr *aicr.Criteria, s *aicr.Snapshot) (*aicr.RecipeResult, error) {
		return c.ResolveRecipeFromSnapshot(ctx, cr, s)
	}
	_ = func(c *aicr.Client, ctx context.Context, path, kubeconfig string) (*aicr.RecipeResult, error) {
		return c.LoadRecipe(ctx, path, kubeconfig)
	}
	_ = func(c *aicr.Client, ctx context.Context) (*aicr.Snapshot, error) {
		return c.CollectSnapshot(ctx, &aicr.AgentConfig{})
	}
}

// TestStability_RecipeResult pins the consumer-visible fields and methods
// on the result returned by every resolve/load entry point.
func TestStability_RecipeResult(t *testing.T) {
	t.Parallel()

	var r aicr.RecipeResult
	_ = r.Name
	_ = r.Version
	_ = r.Components
	_ = r.Resolved()
}

// TestStability_Bundle pins the bundle surface: options shape, MakeBundle /
// BundleComponents signatures, and AdoptRecipe for the decode-then-bundle
// REST boundary.
func TestStability_Bundle(t *testing.T) {
	t.Parallel()

	_ = aicr.BundleOptions{}

	_ = func(c *aicr.Client, ctx context.Context, r *aicr.RecipeResult, o aicr.BundleOptions) (aicr.BundleArtifact, error) {
		return c.MakeBundle(ctx, r, o)
	}
	_ = func(c *aicr.Client, ctx context.Context, r *aicr.RecipeResult) ([]aicr.ComponentBundle, error) {
		return c.BundleComponents(ctx, r)
	}
}

// TestStability_Validate pins the validation surface and every
// WithValidation* option exported today.
func TestStability_Validate(t *testing.T) {
	t.Parallel()

	_ = func(c *aicr.Client, ctx context.Context, r *aicr.RecipeResult, opts ...aicr.ValidateOption) ([]*aicr.PhaseResult, error) {
		return c.ValidateState(ctx, r, &aicr.Snapshot{}, opts...)
	}

	// Element type pins each WithValidation* return to aicr.ValidateOption;
	// dropping or retyping any factory becomes a compile error.
	_ = []aicr.ValidateOption{
		aicr.WithValidationNamespace("ns"),
		aicr.WithValidationRunID("rid"),
		aicr.WithValidationCleanup(true),
		aicr.WithValidationImagePullSecrets([]string{"ips"}),
		aicr.WithValidationNoCluster(true),
		aicr.WithValidationTolerations([]corev1.Toleration{}),
		aicr.WithValidationTimeout(time.Second),
		aicr.WithValidationNodeSelector(map[string]string{"k": "v"}),
		aicr.WithValidationPhases(aicr.Phase("deployment")),
		aicr.WithValidationCommit("sha"),
		aicr.WithValidationImageRegistryOverride("reg"),
		aicr.WithValidationImageTagOverride("tag"),
	}
}

// TestStability_ClientOptions pins WithVersion / WithAllowLists / and the
// recipe-source factories an out-of-tree consumer uses to construct a
// Client.
func TestStability_ClientOptions(t *testing.T) {
	t.Parallel()

	// Element type pins each With* factory return to aicr.Option.
	_ = []aicr.Option{
		aicr.WithVersion("v"),
		aicr.WithAllowLists(&aicr.AllowLists{}),
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithRecipeSource(aicr.FilesystemSource("/x")),
		aicr.WithRecipeSource(aicr.OCISource("reg", "tag")),
	}

	// Typed variable pins the function signature at compile time. The
	// explicit LHS type is the whole point of this assertion (a signature
	// change must be a compile error here), so QF1011 is intentionally
	// suppressed.
	//nolint:staticcheck // QF1011: explicit type pins public-API signature
	var _ func() (*aicr.AllowLists, error) = aicr.ParseAllowListsFromEnv
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
	_ = aicr.ReportSummary{}
	_ = aicr.PhaseResult{}
	_ = aicr.ComponentBundle{}
	_ = aicr.ComponentRef{}
	_ = aicr.RecipeSourceOption{}

	// Phase is a string-backed enum; callers spell phases as Phase("name").
	var _ aicr.Phase = "deployment"

	// Type-alias surface — these are explicitly documented as alias passthroughs
	// (#1078) and are exercised here so a future drop or retype is a compile
	// error.
	var (
		_ *aicr.CriteriaRegistry
		_ aicr.BundleAttester // interface alias
		_ aicr.BundleArtifact // pointer-to-internal alias
		_ *aicr.BundleConfig  // struct-to-internal alias
	)
}

// TestStability_Query pins the package-level query selector.
func TestStability_Query(t *testing.T) {
	t.Parallel()

	// Typed variable pins the function signature at compile time. See
	// TestStability_ClientOptions for rationale on the staticcheck suppression.
	//nolint:staticcheck // QF1011: explicit type pins public-API signature
	var _ func(*aicr.RecipeResult, string) (any, error) = aicr.SelectFromRecipe
}
