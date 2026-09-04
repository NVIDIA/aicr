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
	"os"

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

// Example_resolveFromSnapshot approximates `aicr recipe --snapshot`: resolve
// against a captured snapshot with the relax-and-retry policy the CLI applies.
//
// # The facade does not derive criteria from the snapshot
//
// ResolveRecipeFromSnapshotWithOptions takes your Criteria verbatim and uses
// the snapshot only to evaluate constraints and drive snapshot-aware
// post-processing. Producing criteria from a snapshot's measurements is the
// CLI's job, and there is no facade entry point for it yet — the integration
// guide shows the pkg/fingerprint escape hatch and the coupling it costs.
//
// So SUPPLY every dimension yourself, then use WithSnapshotCriteriaRelaxation
// to say which ones the user actually typed. Below, intent was typed and
// service and os were derived, so only those two may be relaxed.
//
// These values are chosen so relaxation genuinely fires: no kind overlay
// states an os, so the derived os comes back uncovered and is cleared, while
// the stated intent is protected. Two ways to make the policy inert, both
// silent: name every dimension you supplied (a specified-and-stated dimension
// is never cleared), or leave dimensions unset (the coverage post-condition
// only reports dimensions you SPECIFIED, so an unset one is never uncovered).
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

	criteria := &aicr.Criteria{
		Service: "kind",      // derived: read off the snapshot
		OS:      "ubuntu",    // derived, and uncovered by every kind overlay
		Intent:  "inference", // stated: the user asked for this
	}

	result, err := client.ResolveRecipeFromSnapshotWithOptions(ctx, criteria, snap,
		aicr.WithSnapshotCriteriaRelaxation(aicr.DimensionIntent))
	if err != nil {
		log.Print(err)
		return
	}

	// Prints "relaxed os" for the criteria above: no kind overlay distinguishes
	// ubuntu, so the derived os is cleared and the retry succeeds. Intent can
	// never appear here — it was declared stated.
	for _, dim := range result.RelaxedDimensions {
		fmt.Printf("relaxed %s; resolved recipe is broader than requested\n", dim)
	}
}

// ExampleClient_DiffSnapshots compares two previously captured snapshots in
// memory. Loading local files needs no cluster access; cm:// sources use the
// kubeconfig argument passed to LoadSnapshot.
func ExampleClient_DiffSnapshots() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	baseline, err := client.LoadSnapshot(ctx, "before.yaml", "")
	if err != nil {
		log.Print(err)
		return
	}
	target, err := client.LoadSnapshot(ctx, "after.yaml", "")
	if err != nil {
		log.Print(err)
		return
	}

	result, err := client.DiffSnapshots(ctx, baseline, target, aicr.SnapshotDiffOptions{
		BaselineSource: "before.yaml",
		TargetSource:   "after.yaml",
	})
	if err != nil {
		log.Print(err)
		return
	}
	if result.HasDrift() {
		fmt.Printf("detected %d change(s)\n", result.Summary.Total)
	}
}

// Example_bundleAndVerify is the integrator path end to end: resolve a recipe,
// render its deployment bundle, then check what was written.
//
// It runs hermetically against the embedded catalog, into a temporary
// directory, with no signing and no network — which is why it can assert its
// output, and why that output is "unverified".
//
// # Reading the result
//
// Failure arrives on THREE independent channels, and checking one is not
// enough:
//
//   - the returned error — the bundle could not be produced at all;
//   - BundleArtifact.HasErrors() — per-bundler failures that did not abort
//     the run, so files exist but the set is incomplete;
//   - on verification, BundleVerification.PolicyFailure (the trust floor was
//     not met) AND Report.Errors (a check itself failed, e.g. a bad checksum).
//
// # Why "unverified"
//
// BundleOptions.Attester is nil here, so MakeBundle uses the no-op attester —
// the same default as `aicr bundle` without --attest. An unsigned bundle can
// reach "unverified" (checksums valid, no attestation) and no higher, so
// demanding MinTrustLevel "verified" would fail every time. Leaving it empty
// selects the "max" default: verify against the highest level this bundle can
// actually achieve. To reach "verified", pass an Attester and a binary
// attestation.
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

	// Per-component Helm values and stitched manifests, without touching disk.
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

	// Or write a full bundle directory.
	outputDir, err := os.MkdirTemp("", "aicr-bundle-")
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = os.RemoveAll(outputDir) }()

	artifact, err := client.MakeBundle(ctx, result, aicr.BundleOptions{
		OutputDir: outputDir,
	})
	if err != nil {
		log.Print(err)
		return
	}
	// Non-fatal per-bundler failures: files were written, but not all of them.
	if artifact.HasErrors() {
		log.Printf("bundle completed with %d errors", len(artifact.Errors))
		return
	}

	verification, err := client.VerifyBundle(ctx, outputDir, aicr.BundleVerifyOptions{
		// Empty means "max": verify against the highest level achievable.
		MinTrustLevel: "",
	})
	if err != nil {
		log.Print(err)
		return
	}
	if verification.PolicyFailure != "" {
		log.Printf("policy: %s", verification.PolicyFailure)
		return
	}
	if len(verification.Report.Errors) > 0 {
		log.Printf("verification: %s", verification.Report.Errors[0])
		return
	}

	fmt.Println(verification.Report.TrustLevel)
	// Output: unverified
}

// Example_workflow runs the full Snapshot -> Recipe -> Bundle workflow this
// guide documents: load a snapshot, derive criteria from it, layer the
// caller's own intent on top, resolve a recipe, render a bundle, and verify
// what was written.
//
// It runs hermetically against the embedded catalog and a checked-in
// snapshot fixture (testdata/snapshot.yaml), with no cluster and no network
// — which is why it can assert its output.
//
// Two legs of the real workflow are cluster-dependent and are therefore NOT
// part of this runnable body: capturing the snapshot in the first place
// (Client.CollectSnapshot, see ExampleClient_CollectSnapshot) and validating
// the resolved recipe against observed state (Client.ValidateState, see
// ExampleClient_ValidateState). Both need a reachable cluster and are only
// mentioned here.
func Example_workflow() {
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

	// A snapshot already captured elsewhere (see CollectSnapshot above) and
	// delivered to this pipeline stage as a file.
	snap, err := client.LoadSnapshot(ctx, "testdata/snapshot.yaml", "")
	if err != nil {
		log.Print(err)
		return
	}

	// Every dimension the snapshot could not determine stays "any"; nothing
	// is guessed.
	criteria, err := client.CriteriaFromSnapshot(snap)
	if err != nil {
		log.Print(err)
		return
	}
	// Intent is a recipe-author choice the cluster cannot reveal, so the
	// caller states it explicitly on top of what was derived.
	criteria.Intent = "training"

	result, err := client.ResolveRecipeFromCriteria(ctx, criteria)
	if err != nil {
		log.Print(err)
		return
	}

	outputDir, err := os.MkdirTemp("", "aicr-workflow-")
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = os.RemoveAll(outputDir) }()

	artifact, err := client.MakeBundle(ctx, result, aicr.BundleOptions{
		OutputDir: outputDir,
	})
	if err != nil {
		log.Print(err)
		return
	}
	if artifact.HasErrors() {
		log.Printf("bundle completed with %d errors", len(artifact.Errors))
		return
	}

	verification, err := client.VerifyBundle(ctx, outputDir, aicr.BundleVerifyOptions{
		// Empty means "max": verify against the highest level achievable.
		MinTrustLevel: "",
	})
	if err != nil {
		log.Print(err)
		return
	}
	if verification.PolicyFailure != "" {
		log.Printf("policy: %s", verification.PolicyFailure)
		return
	}
	if len(verification.Report.Errors) > 0 {
		log.Printf("verification: %s", verification.Report.Errors[0])
		return
	}

	fmt.Println(result.Name)
	fmt.Println(verification.Report.TrustLevel)
	// Output:
	// criteria(service=eks, accelerator=h100, intent=training)
	// unverified
}

// ExampleClient_LoadRecipe reads a recipe emitted earlier by `aicr recipe -o`,
// instead of resolving a new one. The result is interchangeable with a
// resolved one: bundle it, or validate it against a snapshot.
func ExampleClient_LoadRecipe() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	// A local file path, an HTTP(S) URL, or a cm://namespace/name ConfigMap
	// URI. The kubeconfig argument is consulted only for the cm:// form.
	result, err := client.LoadRecipe(ctx, "recipe.yaml", "")
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Println(result.Name)
}

// ExampleClient_CollectSnapshot captures cluster state by deploying the
// snapshotter Job, for callers that do not already have a snapshot file.
// Requires a reachable cluster and RBAC to create the Job.
//
// Namespace is the only field that must be set. Image, JobName, and
// ServiceAccountName are defaulted when empty — the image to the tag matching
// the Client's WithVersion. This example pins the image anyway, which is what an
// air-gapped or version-skew-sensitive deployment wants.
func ExampleClient_CollectSnapshot() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	snap, err := client.CollectSnapshot(ctx, &aicr.AgentConfig{
		Namespace:          "aicr-system",
		Image:              "ghcr.io/nvidia/aicr:v0.19.0",
		JobName:            "aicr",
		ServiceAccountName: "aicr",
		Cleanup:            true,
	})
	if err != nil {
		log.Print(err)
		return
	}

	// Persist Raw rather than re-serializing: a newer agent image can emit
	// fields this binary's Snapshot type does not model, and a typed round
	// trip silently drops them.
	if err = os.WriteFile("snapshot.yaml", snap.Raw, 0o600); err != nil {
		log.Print(err)
		return
	}
}

// ExampleClient_ValidateState evaluates a resolved recipe against observed
// cluster state.
//
// Validation comprises three phases, executed in order: deployment,
// conformance, performance. This example narrows to the first two with
// WithValidationPhases; omitting that option runs all three.
//
// WithValidationNoCluster(true) keeps constraint evaluation but skips
// everything needing a cluster — the mode CI uses to check a recipe against a
// captured snapshot without provisioning hardware.
func ExampleClient_ValidateState() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	recipe, err := client.LoadRecipe(ctx, "recipe.yaml", "")
	if err != nil {
		log.Print(err)
		return
	}
	snap, err := client.LoadSnapshot(ctx, "snapshot.yaml", "")
	if err != nil {
		log.Print(err)
		return
	}

	phases, err := client.ValidateState(ctx, recipe, snap,
		aicr.WithValidationNoCluster(true),
		aicr.WithValidationPhases(aicr.PhaseDeployment, aicr.PhaseConformance),
	)
	if err != nil {
		log.Print(err)
		return
	}
	for _, p := range phases {
		fmt.Printf("%s: %d passed, %d failed\n", p.Phase, p.Summary.Passed, p.Summary.Failed)
	}
}

// ExampleClient_RecipeDigest computes the canonical digest an evidence
// predicate records. A CI gate compares this against the digest inside a
// published evidence bundle to detect evidence that has gone stale relative to
// the recipe it claims to describe.
func ExampleClient_RecipeDigest() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	digest, err := client.RecipeDigest(ctx, aicr.RecipeDigestOptions{
		Path: "recipe.yaml",
	})
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Println(digest)
}

// ExampleClient_VerifyCatalog checks the Sigstore signature over this Client's
// recipe catalog — that the recipe data resolution is about to use was
// published by NVIDIA CI and has not been altered. The bundle ships as the
// recipe-catalog.sigstore.json release asset.
//
// The digest is computed over THIS Client's DataProvider. A Client layering
// external data over the embedded tree is verifying different content, so it
// will not match the released signature — that is the correct answer to "is
// the catalog I am resolving against the signed one", not a failure to
// work around.
func ExampleClient_VerifyCatalog() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource("/etc/aicr/recipes")))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	verification, err := client.VerifyCatalog(ctx, "recipe-catalog.sigstore.json", aicr.CatalogVerifyOptions{})
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Printf("signed by %s over %s\n", verification.Identity, verification.Digest)
}

// ExampleClient_SignCatalog signs a recipe catalog with keyless Sigstore.
//
// # This is a release-CI flow, not a local one
//
// VerifyCatalog pins the certificate identity to this repository's tag-release
// workflow. A catalog signed anywhere else verifies against nothing, so this
// is only useful from that workflow, with ambient credentials supplied.
//
// Leaving OIDCResolve zero-valued is the trap: SelectOIDCSource then falls
// through to the interactive BROWSER flow, which blocks on a human and mints a
// certificate issued by oauth2.sigstore.dev. SignCatalog succeeds and emits a
// bundle VerifyCatalog rejects — it fails the issuer pin before identity
// matching is even reached.
//
// SignCatalog does reject settings that break verifiability — a signing key, a
// private Fulcio or Rekor, or a disabled transparency-log upload — but it does
// NOT police the identity SOURCE, which is the asymmetry an SDK caller is most
// likely to hit.
//
// Signing is also deliberately not bounded by a facade timeout, because keyless
// OIDC can block on a human.
func ExampleClient_SignCatalog() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource("/etc/aicr/recipes")))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	signed, err := client.SignCatalog(ctx, aicr.CatalogSignOptions{
		Output: "recipe-catalog.sigstore.json",
		OIDCResolve: aicr.OIDCResolveOptions{
			// Ambient workload credentials. Both must be set, or resolution
			// falls through to the browser flow described above.
			AmbientURL:   os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
			AmbientToken: os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
		},
	})
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Printf("signed catalog digest %s (%d bytes)\n", signed.Digest, len(signed.BundleJSON))
}

// ExampleClient_PublishEvidence signs a recipe-evidence bundle and pushes it to
// an OCI registry, the producing half of ExampleClient_VerifyEvidence.
func ExampleClient_PublishEvidence() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	if err = client.PublishEvidence(ctx, aicr.EvidencePublishOptions{
		BundleDir: "./evidence",
		Push:      "ghcr.io/example/aicr-evidence:v1",
	}); err != nil {
		log.Print(err)
		return
	}
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

// ExampleClient_CriteriaFromSnapshot derives recipe criteria from a captured
// snapshot without reaching past the facade.
//
// This step previously required pkg/fingerprint, so the documented workflow
// could not be completed with pkg/client/v1 alone.
func ExampleClient_CriteriaFromSnapshot() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = client.Close() }()

	snap, err := client.LoadSnapshot(ctx, "snapshot.yaml", "")
	if err != nil {
		log.Print(err)
		return
	}

	// Every dimension the snapshot could not determine stays "any"; nothing is
	// guessed. Layer your own stated values on top before resolving.
	criteria, err := client.CriteriaFromSnapshot(snap)
	if err != nil {
		log.Print(err)
		return
	}
	criteria.Intent = "training"

	result, err := client.ResolveRecipeFromCriteria(ctx, criteria)
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Printf("resolved %d components\n", len(result.Components))
}

// ExampleClient_MirrorInventory lists every image and chart a recipe needs, for
// staging into an air-gapped registry.
//
// Rendering is the caller's job: the facade returns data, and formats such as
// Hauler or Zarf stay in the CLI so the SDK does not freeze third-party schemas
// as contract.
func ExampleClient_MirrorInventory() {
	ctx := context.Background()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
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

	// Pass the same overrides you will bundle with. Disabling a sub-component
	// removes its images, so mirroring without them stages the wrong set.
	disabled := "false"
	inventory, err := client.MirrorInventory(ctx, result,
		aicr.WithMirrorValueOverrides([]aicr.MirrorValueOverride{
			{Component: "gpuoperator", Path: "driver.enabled", Value: &disabled},
		}))
	if err != nil {
		log.Print(err)
		return
	}

	for _, image := range inventory.Images {
		fmt.Println(image)
	}
	for _, chart := range inventory.Charts {
		fmt.Printf("%s %s from %s\n", chart.Chart, chart.Version, chart.Repository)
	}
}
