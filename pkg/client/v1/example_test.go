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

// Runnable examples for the integrator surface (issue #2029).
//
// These are the canonical form of the flows in docs/integrator/go-library.md.
// `go test` compiles every one of them, so a facade change that breaks a
// documented flow fails in this tree rather than in a consumer's — the guide's
// prose can still drift, but its code cannot.
//
// Two kinds live here:
//
//   - Examples with an "Output:" comment RUN. Keep their output stable: print
//     criteria strings and error codes, never component counts or versions,
//     which change as the catalog evolves and would fail unrelated PRs.
//   - Examples without one are COMPILE-ONLY. Those use realistic paths
//     ("aicr-config.yaml") that read correctly in godoc but do not exist here.
//     They still pin every signature, field name, and option they touch.
//
// Errors are handled with log.Print + return rather than log.Fatal: these
// examples hold a Client whose Close is deferred, and log.Fatal exits the
// process without running deferred functions. Copying the wrong idiom out of
// a godoc example is how it spreads.
package aicr_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"log"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// Example is the quick start: build a Client over the embedded recipe data and
// resolve a recipe from explicit criteria.
func Example() {
	ctx := context.Background()

	client, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithVersion("v0.19.0"),
	)
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	result, err := client.ResolveRecipeFromCriteria(ctx, &aicr.Criteria{
		Service:     "eks",
		Accelerator: "h100",
		Intent:      "training",
	})
	if err != nil {
		log.Print(err)
		return
	}

	// Name is the resolved criteria's canonical string. Unstated dimensions
	// still render, with an empty value.
	fmt.Println(result.Name)
	// Output: criteria(service=eks, accelerator=h100, intent=training, os=, platform=)
}

// Example_errorCodes shows the error-handling contract. Every facade error is a
// *pkg/errors.StructuredError carrying an ErrorCode, and StructuredError.Is
// matches on that code — so errors.Is works through wrap chains without
// unwrapping by hand.
func Example_errorCodes() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	// A service no catalog defines. Membership is checked against this
	// Client's CriteriaRegistry, so the request is rejected rather than
	// silently resolving something broader.
	_, err = client.ResolveRecipeFromCriteria(ctx, &aicr.Criteria{Service: "no-such-service"})

	switch {
	case err == nil:
		fmt.Println("resolved")
	case stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")):
		fmt.Println("invalid request")
	case stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeNotFound, "")):
		fmt.Println("not found")
	default:
		fmt.Println("other")
	}
	// Output: invalid request
}

// Example_committedConfig resolves from an AICRConfig a team commits alongside
// their code, so snapshot / recipe / bundle / verify settings are not retyped
// on each invocation.
//
// The ORDER matters and is the reason this example exists. Criteria membership
// is validated against a CriteriaRegistry, which is per-DataProvider — so the
// Client must exist and its catalog must be loaded before RecipeCriteria can
// resolve a value an external --data overlay contributed. Calling
// RecipeCriteria first works only for values in the embedded catalog.
func Example_committedConfig() {
	ctx := context.Background()

	cfg, err := aicr.LoadConfig(ctx, "aicr-config.yaml")
	if err != nil {
		log.Print(err)
		return
	}

	// spec.recipe.data, when the document sets one.
	source, ok := cfg.RecipeSource()
	if !ok {
		source = aicr.EmbeddedSource()
	}

	client, err := aicr.NewClient(aicr.WithRecipeSource(source))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	// Seeds the registry RecipeCriteria validates against.
	if err = client.LoadCatalog(ctx); err != nil {
		log.Print(err)
		return
	}

	criteria, err := cfg.RecipeCriteria(client.CriteriaRegistry())
	if err != nil {
		log.Print(err)
		return
	}

	opts, err := cfg.RecipeResolveOptions()
	if err != nil {
		log.Print(err)
		return
	}

	result, err := client.ResolveRecipeFromCriteriaWithOptions(ctx, criteria, opts...)
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Println(result.Name)
}

// Example_resolveFromSnapshot reproduces `aicr recipe --snapshot`: load a
// previously captured snapshot, derive criteria from it, and resolve with the
// relax-and-retry policy the CLI applies.
//
// WithSnapshotCriteriaRelaxation names the dimensions the caller stated
// EXPLICITLY; everything else is treated as derived and may be cleared if no
// overlay distinguishes it. Passing none — as here — means every dimension came
// from the snapshot and all are relaxable.
func Example_resolveFromSnapshot() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	// File path, HTTP(S) URL, or cm://namespace/name ConfigMap.
	snap, err := client.LoadSnapshot(ctx, "snapshot.yaml", "")
	if err != nil {
		log.Print(err)
		return
	}

	criteria := &aicr.Criteria{Intent: "training"} // the one value the user stated

	result, err := client.ResolveRecipeFromSnapshotWithOptions(ctx, criteria, snap,
		aicr.WithSnapshotCriteriaRelaxation(aicr.DimensionIntent))
	if err != nil {
		log.Print(err)
		return
	}

	// Non-empty only when the first attempt failed coverage on derived
	// dimensions and the retry succeeded: the recipe is broader than asked for.
	for _, dim := range result.RelaxedDimensions {
		fmt.Printf("relaxed %s; resolved recipe is broader than requested\n", dim)
	}
}

// Example_bundleAndVerify is the integrator path end to end: resolve a recipe,
// render its deployment bundle, then check the result's checksums and
// attestation chain against a trust policy.
func Example_bundleAndVerify() {
	ctx := context.Background()

	client, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithVersion("v0.19.0"),
	)
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	result, err := client.ResolveRecipeFromCriteria(ctx, &aicr.Criteria{
		Service:     "eks",
		Accelerator: "h100",
		Intent:      "training",
	})
	if err != nil {
		log.Print(err)
		return
	}

	// Per-component Helm values and stitched manifests, without writing to disk.
	bundles, err := client.BundleComponents(ctx, result)
	if err != nil {
		log.Print(err)
		return
	}
	for _, b := range bundles {
		_ = b.Component.Name
		_ = b.HelmValues
		_ = b.Manifests
	}

	// Or write a full bundle directory, then verify what was written.
	if _, err = client.MakeBundle(ctx, result, aicr.BundleOptions{
		OutputDir: "./bundles",
	}); err != nil {
		log.Print(err)
		return
	}

	verification, err := client.VerifyBundle(ctx, "./bundles", aicr.BundleVerifyOptions{
		MinTrustLevel: "verified",
	})
	if err != nil {
		log.Print(err)
		return
	}
	if verification.PolicyFailure != "" {
		log.Printf("policy: %s", verification.PolicyFailure)
		return
	}
	fmt.Println(verification.Report.TrustLevel)
}

// ExampleClient_VerifyEvidence checks a recipe-evidence bundle's signature and
// hash chain. Input accepts a pointer file, a directory, or an OCI reference.
func ExampleClient_VerifyEvidence() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	verification, err := client.VerifyEvidence(ctx, aicr.EvidenceVerifyOptions{
		Input: "evidence.json",
	})
	if err != nil {
		log.Print(err)
		return
	}

	switch verification.Exit {
	case aicr.EvidenceExitValidPassed:
		fmt.Println("valid, all phases passed")
	case aicr.EvidenceExitValidPhaseFailures:
		fmt.Println("valid, but phases failed")
	case aicr.EvidenceExitInvalid:
		fmt.Println("invalid")
	case aicr.EvidenceExitIncomplete:
		fmt.Println("incomplete")
	}

	fmt.Println(aicr.RenderEvidenceMarkdown(verification))
}

// ExampleVerifyBinaryAttestation proves an aicr binary was built by NVIDIA CI.
// It is package-level rather than a Client method: verifying a binary needs no
// recipe data, so it requires no Client.
func ExampleVerifyBinaryAttestation() {
	ctx := context.Background()

	builder, err := aicr.VerifyBinaryAttestation(ctx, aicr.BinaryAttestationVerifyOptions{
		Attestation: []byte(`{}`), // the .intoto.jsonl bundle shipped with the release
		BinaryDigest: []byte{
			0x00, 0x01, 0x02, 0x03,
		},
		// Defaults to the release workflow on tag refs. An override must still
		// begin with the NVIDIA/aicr repository prefix; ValidateIdentityPattern
		// reports whether a candidate is acceptable before you use it.
		IdentityRegexp: aicr.TrustedIdentityPattern,
	})
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Println(builder)
}

// Example_trustLevels enumerates the bundle trust levels
// BundleVerifyOptions.MinTrustLevel accepts. The CLI's --min-trust-level
// completion is generated from this same list.
//
// Two properties to note before validating input against it. The order is
// ALPHABETICAL, not by rank — do not treat position as severity. And the list
// is not the full accepted set: the default "max" (auto-detect the highest
// achievable level) and the empty string are both valid and both absent here,
// so a membership check built from this list alone rejects the option's own
// default.
func Example_trustLevels() {
	for _, level := range aicr.TrustLevels() {
		fmt.Println(level)
	}
	// Output:
	// attested
	// unknown
	// unverified
	// verified
}

// Example_criteriaDimensions lists the criteria dimensions subject to the
// coverage post-condition — the values WithSnapshotCriteriaRelaxation accepts.
//
// nodes is deliberately absent: no overlay gates on it, so it never
// participates in overlay selection or coverage.
func Example_criteriaDimensions() {
	for _, dim := range aicr.AllCriteriaDimensions() {
		fmt.Println(dim)
	}
	// Output:
	// service
	// accelerator
	// intent
	// os
	// platform
}
