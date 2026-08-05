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

package attestation

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/allocpolicy"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// emitUnsignedBundle produces a real on-disk, unsigned bundle (the
// artifact `validate --emit-attestation` without --push leaves behind)
// and returns the OutDir that holds summary-bundle/ + pointer.yaml.
func emitUnsignedBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.run/v1alpha2",
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			Intent:      recipe.CriteriaIntentTraining,
		},
	}
	_, err := Emit(context.Background(), EmitOptions{
		OutDir:      dir,
		Recipe:      rec,
		Snapshot:    &snapshotter.Snapshot{},
		AICRVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return dir
}

func wantInvalidRequest(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}

func wantTimeout(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("expected ErrCodeTimeout, got %v", err)
	}
}

// TestBundleReaders_CanceledContextSurfacesTimeout proves each on-disk reader
// path is bounded by the caller's context: against a real, valid bundle (so an
// unbounded read would succeed instantly), an already-canceled context makes
// every reader fail closed with ErrCodeTimeout rather than block on a
// potentially hung mount. This is the load-bearing guarantee of issue #2054 —
// a dead NFS/FUSE mount surfaces as a timeout, not an indefinite hang.
func TestBundleReaders_CanceledContextSurfacesTimeout(t *testing.T) {
	dir := emitUnsignedBundle(t)
	summaryDir := filepath.Join(dir, SummaryBundleDirName)

	tests := []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"readBundlePredicate", func(ctx context.Context) error {
			_, _, err := readBundlePredicate(ctx, summaryDir)
			return err
		}},
		{"readBundleRecipeProfile", func(ctx context.Context) error {
			_, _, _, err := readBundleRecipeProfile(ctx, summaryDir)
			return err
		}},
		{"HasBundleMarkers", func(ctx context.Context) error {
			_, err := HasBundleMarkers(ctx, summaryDir)
			return err
		}},
		{"resolveSummaryDir", func(ctx context.Context) error {
			_, _, err := resolveSummaryDir(ctx, dir)
			return err
		}},
		{"loadOnDiskBundle", func(ctx context.Context) error {
			_, _, err := loadOnDiskBundle(ctx, dir)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			wantTimeout(t, tt.run(ctx))
		})
	}
}

// TestWithFileReadTimeout_InFlightTimeout exercises the goroutine+select arm of
// withFileReadTimeout — the part that actually delivers hang-immunity. The
// canceled-context tests above return at the ctx.Err() pre-check and never
// reach the select, so this covers the case a wedged mount hits: fn is already
// running (blocked in the syscall) when the caller context is canceled. It uses
// a started/release/finished handshake so the cancellation is observed
// in-flight, asserts ErrCodeTimeout, then unblocks fn and joins it — proving
// the parked worker returns rather than leaking.
func TestWithFileReadTimeout_InFlightTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	errCh := make(chan error, 1)

	go func() {
		errCh <- withFileReadTimeout(ctx, "blocked read", func() error {
			close(started)
			<-release // stand in for a syscall wedged on a hung mount
			close(finished)
			return nil
		})
	}()

	<-started // fn is now running inside the goroutine+select path
	cancel()  // cancel the caller context while fn is still blocked
	wantTimeout(t, <-errCh)

	close(release) // release the parked worker...
	<-finished     // ...and confirm it returns (no leaked goroutine)
}

func TestHasBundleMarkers(t *testing.T) {
	valid := filepath.Join(emitUnsignedBundle(t), SummaryBundleDirName)

	tests := []struct {
		name    string
		dir     string
		want    bool
		wantErr bool
	}{
		{"valid summary bundle", valid, true, false},
		{"empty dir is not a bundle", t.TempDir(), false, false},
		{"missing dir is not a bundle", filepath.Join(t.TempDir(), "nope"), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HasBundleMarkers(context.Background(), tt.dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("HasBundleMarkers error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("HasBundleMarkers = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHasBundleMarkers_DirectoryMarkerIsNotABundle guards the IsDir() branch:
// a directory named recipe.yaml must not be mistaken for the marker file, and
// the non-timeout path must still report (false, nil), not an error.
func TestHasBundleMarkers_DirectoryMarkerIsNotABundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, RecipeFilename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFilename), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := HasBundleMarkers(context.Background(), dir)
	if err != nil {
		t.Fatalf("HasBundleMarkers returned unexpected error: %v", err)
	}
	if got {
		t.Errorf("HasBundleMarkers = true, want false (recipe.yaml is a directory)")
	}
}

func TestPublish_RequiresPush(t *testing.T) {
	err := Publish(context.Background(), PublishOptions{BundleDir: t.TempDir()})
	wantInvalidRequest(t, err)
}

func TestPublish_InvalidPushReference(t *testing.T) {
	dir := emitUnsignedBundle(t)
	err := Publish(context.Background(), PublishOptions{
		BundleDir: dir,
		Push:      "oci://not a valid ref",
	})
	wantInvalidRequest(t, err)
}

func TestPublish_MissingBundleDir(t *testing.T) {
	// A valid push ref but a directory with no bundle markers must fail
	// before any network work.
	err := Publish(context.Background(), PublishOptions{
		BundleDir: t.TempDir(),
		Push:      "ghcr.io/example/aicr-evidence",
	})
	wantInvalidRequest(t, err)
}

func TestLoadOnDiskBundle_ParentDir(t *testing.T) {
	dir := emitUnsignedBundle(t)

	bundle, outDir, err := loadOnDiskBundle(context.Background(), dir)
	if err != nil {
		t.Fatalf("loadOnDiskBundle: %v", err)
	}
	if outDir != filepath.Clean(dir) {
		t.Errorf("outDir = %q, want %q (pointer beside summary-bundle/)", outDir, dir)
	}
	if bundle.SummaryDir != filepath.Join(dir, SummaryBundleDirName) {
		t.Errorf("SummaryDir = %q, want %q", bundle.SummaryDir, filepath.Join(dir, SummaryBundleDirName))
	}
	if bundle.RecipeName != "h100-eks-training" {
		t.Errorf("RecipeName = %q, want h100-eks-training", bundle.RecipeName)
	}
	if bundle.Predicate == nil || bundle.SubjectDigest == "" {
		t.Fatalf("expected populated Predicate + SubjectDigest, got %+v", bundle)
	}
	if bundle.SubjectDigest != bundle.Predicate.Recipe.Digest {
		t.Errorf("SubjectDigest %q != predicate.recipe.digest %q",
			bundle.SubjectDigest, bundle.Predicate.Recipe.Digest)
	}
}

func TestLoadOnDiskBundle_SummaryDirItself(t *testing.T) {
	dir := emitUnsignedBundle(t)
	summaryDir := filepath.Join(dir, SummaryBundleDirName)

	bundle, outDir, err := loadOnDiskBundle(context.Background(), summaryDir)
	if err != nil {
		t.Fatalf("loadOnDiskBundle: %v", err)
	}
	// Pointing at summary-bundle/ directly puts the pointer in its parent,
	// matching the one-shot output layout.
	if outDir != filepath.Clean(dir) {
		t.Errorf("outDir = %q, want parent %q", outDir, dir)
	}
	if bundle.SummaryDir != filepath.Clean(summaryDir) {
		t.Errorf("SummaryDir = %q, want %q", bundle.SummaryDir, summaryDir)
	}
}

func TestLoadOnDiskBundle_EmptyArg(t *testing.T) {
	_, _, err := loadOnDiskBundle(context.Background(), "")
	wantInvalidRequest(t, err)
}

func TestResolveSummaryDir_NotABundle(t *testing.T) {
	_, _, err := resolveSummaryDir(context.Background(), t.TempDir())
	wantInvalidRequest(t, err)
}

func TestReadBundlePredicate_MissingStatement(t *testing.T) {
	_, _, err := readBundlePredicate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatalf("expected error for missing statement")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}

func TestReadBundlePredicate_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, StatementFilename), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readBundlePredicate(context.Background(), dir)
	wantInvalidRequest(t, err)
}

func TestReadBundlePredicate_WrongPredicateType(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"predicateType":"https://example.com/other/v1","predicate":{}}`)
	if err := os.WriteFile(filepath.Join(dir, StatementFilename), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readBundlePredicate(context.Background(), dir)
	wantInvalidRequest(t, err)
}

func TestLoadOnDiskBundle_MissingRecipeIdentity(t *testing.T) {
	// A V1 statement whose predicate lacks recipe.{name,digest} must be
	// rejected — BuildArtifactStatement requires both downstream.
	dir := t.TempDir()
	summaryDir := filepath.Join(dir, SummaryBundleDirName)
	if err := os.MkdirAll(summaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{RecipeFilename, ManifestFilename} {
		if err := os.WriteFile(filepath.Join(summaryDir, f), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte(`{"predicateType":"` + PredicateTypeV1 + `","predicate":{"recipe":{}}}`)
	if err := os.WriteFile(filepath.Join(summaryDir, StatementFilename), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadOnDiskBundle(context.Background(), dir)
	wantInvalidRequest(t, err)
}

func TestLoadOnDiskBundle_ProfilePredicateIncoherenceFailsBeforeSideEffects(t *testing.T) {
	// A PROFILED recipe.yaml paired with a v1 statement (no profile block)
	// must fail at bundle load — before Publish's Fulcio sign and OCI push
	// — instead of doing registry side effects and then emitting a pointer
	// the verifier rejects.
	dir := t.TempDir()
	summaryDir := filepath.Join(dir, SummaryBundleDirName)
	if err := os.MkdirAll(summaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const recipeYAML = `kind: RecipeResult
apiVersion: aicr.run/v1alpha3
metadata:
  selectedProfile:
    name: gpuStack
    value: gcp-managed
    advertiser: external
    ownedPaths:
      gpu-operator:
        - devicePlugin.enabled
        - enabled
componentRefs:
  - name: gpu-operator
`
	if err := os.WriteFile(filepath.Join(summaryDir, RecipeFilename), []byte(recipeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(summaryDir, ManifestFilename), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"predicateType":"` + PredicateTypeV1 +
		`","predicate":{"recipe":{"name":"h100-gke-cos-training-gpustack-gcp-managed","digest":"abc"}}}`)
	if err := os.WriteFile(filepath.Join(summaryDir, StatementFilename), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadOnDiskBundle(context.Background(), dir)
	wantInvalidRequest(t, err)
	if !strings.Contains(err.Error(), "no profile block") {
		t.Fatalf("error = %v, want profile/predicate incoherence rejection", err)
	}
}

func TestLoadOnDiskBundle_ProfiledEmitRoundTrip(t *testing.T) {
	// Positive control for the incoherence rejections in this file: a
	// bundle Emit itself produced from a COHERENT profiled recipe must
	// load cleanly, with the predicate's profile block matching the tuple
	// recomputed from the bundle's recipe bytes (selection, advertiser,
	// recipe-scoped descriptor identity). Without this, every profile
	// gate above could be rejecting for the wrong reason and the tests
	// would still pass.
	dir := t.TempDir()
	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.run/v1alpha3",
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceGKE,
			Accelerator: recipe.CriteriaAcceleratorH100,
			OS:          recipe.CriteriaOSCOS,
			Intent:      recipe.CriteriaIntentTraining,
		},
		ComponentRefs: []recipe.ComponentRef{{
			Name: "gpu-operator",
			Overrides: map[string]any{
				"devicePlugin": map[string]any{"enabled": false},
			},
		}},
	}
	rec.Metadata.SelectedProfile = &recipe.SelectedProfile{
		Name:       "gpuStack",
		Value:      "gcp-managed",
		Advertiser: allocpolicy.AdvertiserExternal,
	}
	if _, err := Emit(context.Background(), EmitOptions{
		OutDir:      dir,
		Recipe:      rec,
		Snapshot:    &snapshotter.Snapshot{},
		AICRVersion: "v0.0.0-test",
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	bundle, _, err := loadOnDiskBundle(context.Background(), dir)
	if err != nil {
		t.Fatalf("loadOnDiskBundle rejected a coherent profiled bundle: %v", err)
	}
	if bundle.Profile != "gpuStack=gcp-managed" {
		t.Errorf("Profile = %q, want gpuStack=gcp-managed", bundle.Profile)
	}
	if bundle.Advertiser != allocpolicy.AdvertiserExternal {
		t.Errorf("Advertiser = %q, want %q", bundle.Advertiser, allocpolicy.AdvertiserExternal)
	}
	// The external advertiser triggers the closure and the enabled
	// gpu-operator ref contributes its descriptor entry — the identity
	// must be the recipe-scoped recomputation, not empty and not the
	// empty-set hash.
	wantIdentity := allocpolicy.IdentityFor(rec.ClosureDescriptorEntries())
	if wantIdentity == "" || wantIdentity == allocpolicy.IdentityFor(nil) {
		t.Fatalf("test setup produced a trivial descriptor identity %q", wantIdentity)
	}
	if bundle.PolicyDescriptorIdentity != wantIdentity {
		t.Errorf("PolicyDescriptorIdentity = %q, want %q", bundle.PolicyDescriptorIdentity, wantIdentity)
	}
	if bundle.Predicate == nil || bundle.Predicate.Profile == nil {
		t.Fatalf("expected a v2 predicate with a profile block, got %+v", bundle.Predicate)
	}
	if p := bundle.Predicate.Profile; p.Selection != bundle.Profile ||
		p.Advertiser != bundle.Advertiser ||
		p.PolicyDescriptorIdentity != bundle.PolicyDescriptorIdentity {

		t.Errorf("predicate profile block %+v does not match recipe-derived tuple", p)
	}
}

func TestLoadOnDiskBundle_StaleDescriptorIdentityFailsBeforeSideEffects(t *testing.T) {
	// A v2 statement whose selection and advertiser match the bundle
	// recipe but whose policy-descriptor identity predates a descriptor
	// expansion must fail at bundle load — the verifier rejects such
	// evidence as historical-only, so the split-leg publish must not
	// spend a Fulcio cert, a Rekor entry, and an OCI push on it.
	dir := t.TempDir()
	summaryDir := filepath.Join(dir, SummaryBundleDirName)
	if err := os.MkdirAll(summaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const recipeYAML = `kind: RecipeResult
apiVersion: aicr.run/v1alpha3
metadata:
  selectedProfile:
    name: gpuStack
    value: gcp-managed
    advertiser: external
    ownedPaths:
      gpu-operator:
        - devicePlugin.enabled
        - enabled
componentRefs:
  - name: gpu-operator
`
	if err := os.WriteFile(filepath.Join(summaryDir, RecipeFilename), []byte(recipeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(summaryDir, ManifestFilename), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"predicateType":"` + PredicateTypeV2 +
		`","predicate":{"recipe":{"name":"h100-gke-cos-training-gpustack-gcp-managed","digest":"abc"},` +
		`"profile":{"selection":"gpuStack=gcp-managed","advertiser":"external",` +
		`"policyDescriptorIdentity":"stale-pre-expansion-identity"}}}`)
	if err := os.WriteFile(filepath.Join(summaryDir, StatementFilename), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadOnDiskBundle(context.Background(), dir)
	wantInvalidRequest(t, err)
	if !strings.Contains(err.Error(), "historical-only") {
		t.Fatalf("error = %v, want stale descriptor-identity rejection", err)
	}
}

func TestValidateBundleProfileCoherence(t *testing.T) {
	pred := func(sel string) *Predicate {
		p := &Predicate{}
		if sel != "" {
			p.Profile = &ProfilePredicate{Selection: sel, PolicyDescriptorIdentity: "d"}
		}
		return p
	}
	predAdv := func(sel, adv string) *Predicate {
		p := pred(sel)
		p.Profile.Advertiser = adv
		return p
	}
	tests := []struct {
		name    string
		bundle  *Bundle
		wantErr bool
	}{
		{"unprofiled coherent", &Bundle{Predicate: pred("")}, false},
		{"profiled coherent", &Bundle{Profile: "gpuStack=gcp-managed", PolicyDescriptorIdentity: "d", Predicate: pred("gpuStack=gcp-managed")}, false},
		{"profiled recipe with v1 predicate", &Bundle{Profile: "gpuStack=gcp-managed", Predicate: pred("")}, true},
		{"unprofiled recipe with profile block", &Bundle{Predicate: pred("gpuStack=gcp-managed")}, true},
		{"selection mismatch", &Bundle{Profile: "gpuStack=gcp-managed", Predicate: pred("gpuStack=operator-managed")}, true},
		{"advertiser coherent", &Bundle{Profile: "gpuStack=gcp-managed", Advertiser: "external", PolicyDescriptorIdentity: "d", Predicate: predAdv("gpuStack=gcp-managed", "external")}, false},
		{"advertiser mismatch: predicate missing it", &Bundle{Profile: "gpuStack=gcp-managed", Advertiser: "external", Predicate: pred("gpuStack=gcp-managed")}, true},
		{"advertiser mismatch: recipe missing it", &Bundle{Profile: "gpuStack=gcp-managed", Predicate: predAdv("gpuStack=gcp-managed", "external")}, true},
		{"descriptor-identity mismatch: evidence predates an expansion", &Bundle{Profile: "gpuStack=gcp-managed", PolicyDescriptorIdentity: "expanded", Predicate: pred("gpuStack=gcp-managed")}, true},
		{"descriptor-identity mismatch: recipe recomputation empty", &Bundle{Profile: "gpuStack=gcp-managed", Predicate: pred("gpuStack=gcp-managed")}, true},
		{"profile block with empty descriptor identity", &Bundle{Profile: "gpuStack=gcp-managed",
			Predicate: &Predicate{Profile: &ProfilePredicate{Selection: "gpuStack=gcp-managed"}}}, true},
		{"nil bundle", nil, true},
		{"nil predicate", &Bundle{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBundleProfileCoherence(tt.bundle)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateBundleProfileCoherence() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				wantInvalidRequest(t, err)
			}
		})
	}
}
