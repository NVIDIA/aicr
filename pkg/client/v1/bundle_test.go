// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package aicr_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// resolveEmbeddedTrainingRecipe resolves a known-good embedded recipe
// (H100/EKS/Ubuntu/Training) through a fresh Client and returns both the
// facade result and the Client so callers can drive MakeBundle. The
// recipe carries multiple components, so the generated bundle exercises
// per-component directory emission.
func resolveEmbeddedTrainingRecipe(t *testing.T) (*aicr.Client, *aicr.RecipeResult) {
	t.Helper()
	client, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithVersion("v-test"),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	rec, err := client.ResolveRecipe(t.Context(), aicr.RecipeRequest{
		Service:     "eks",
		Accelerator: "h100",
		OS:          "ubuntu",
		Intent:      "training",
	})
	if err != nil {
		t.Fatalf("ResolveRecipe: %v", err)
	}
	if len(rec.Components) == 0 {
		t.Fatal("resolved recipe has no components; cannot exercise MakeBundle")
	}
	return client, rec
}

// listBundleFiles returns the sorted relative paths of every regular
// file under dir. Used to compare the on-disk layout of two bundle runs.
func listBundleFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk bundle dir %s: %v", dir, err)
	}
	sort.Strings(files)
	return files
}

// TestMakeBundle_ProducesArtifact verifies MakeBundle emits a non-empty
// bundle for a resolved recipe across the deployer modes.
func TestMakeBundle_ProducesArtifact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		deployer config.DeployerType
	}{
		{"helm", config.DeployerHelm},
		{"argocd", config.DeployerArgoCD},
		{"flux", config.DeployerFlux},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, rec := resolveEmbeddedTrainingRecipe(t)

			out, err := client.MakeBundle(t.Context(), rec, aicr.BundleOptions{
				Config: config.NewConfig(
					config.WithVersion("v-test"),
					config.WithDeployer(tt.deployer),
				),
				OutputDir: t.TempDir(),
			})
			if err != nil {
				t.Fatalf("MakeBundle(%s): %v", tt.deployer, err)
			}
			if out == nil {
				t.Fatal("MakeBundle returned nil artifact")
			}
			if out.HasErrors() {
				t.Fatalf("MakeBundle reported bundle errors: %v", out.Errors)
			}
			if out.TotalFiles == 0 {
				t.Error("MakeBundle produced zero files")
			}
			if out.TotalSize == 0 {
				t.Error("MakeBundle produced zero bytes")
			}
			if files := listBundleFiles(t, out.OutputDir); len(files) == 0 {
				t.Error("no files written to output dir")
			}
		})
	}
}

// TestMakeBundle_ParityWithDirectBundler proves the facade reproduces
// the SAME bundle a direct bundler.New + Make produces for identical
// input. Both runs resolve the same recipe and bundle with the same
// config; the assertion is that the on-disk file layout matches. This
// is the parity gate guarding criterion #2 (bundle output unchanged
// when routed through the facade).
func TestMakeBundle_ParityWithDirectBundler(t *testing.T) {
	t.Parallel()

	client, rec := resolveEmbeddedTrainingRecipe(t)

	// Facade path.
	facadeDir := t.TempDir()
	facadeOut, err := client.MakeBundle(t.Context(), rec, aicr.BundleOptions{
		Config: config.NewConfig(
			config.WithVersion("v-test"),
			config.WithDeployer(config.DeployerHelm),
		),
		OutputDir: facadeDir,
	})
	if err != nil {
		t.Fatalf("facade MakeBundle: %v", err)
	}

	// Direct path: build a bundler exactly as MakeBundle does and Make
	// against the same internal recipe (rec.Resolved()).
	directDir := t.TempDir()
	b, err := bundler.New(bundler.WithConfig(config.NewConfig(
		config.WithVersion("v-test"),
		config.WithDeployer(config.DeployerHelm),
	)))
	if err != nil {
		t.Fatalf("bundler.New: %v", err)
	}
	directOut, err := b.Make(t.Context(), rec.Resolved(), directDir)
	if err != nil {
		t.Fatalf("direct Make: %v", err)
	}

	if facadeOut.TotalFiles != directOut.TotalFiles {
		t.Errorf("file count mismatch: facade=%d direct=%d", facadeOut.TotalFiles, directOut.TotalFiles)
	}

	facadeFiles := listBundleFiles(t, facadeDir)
	directFiles := listBundleFiles(t, directDir)
	if len(facadeFiles) != len(directFiles) {
		t.Fatalf("file-set length mismatch: facade=%v direct=%v", facadeFiles, directFiles)
	}
	for i := range facadeFiles {
		if facadeFiles[i] != directFiles[i] {
			t.Errorf("file-set mismatch at %d: facade=%q direct=%q", i, facadeFiles[i], directFiles[i])
		}
	}
}

// TestMakeBundle_NilInputsRejected pins the bounds-checking: nil
// RecipeResult, caller-constructed RecipeResult (nil internal), and nil
// receiver all surface ErrCodeInvalidRequest cleanly rather than
// panicking.
func TestMakeBundle_NilInputsRejected(t *testing.T) {
	t.Parallel()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	t.Run("nil recipe", func(t *testing.T) {
		t.Parallel()
		_, err := client.MakeBundle(t.Context(), nil, aicr.BundleOptions{})
		assertInvalidRequest(t, err)
	})

	t.Run("caller-constructed recipe", func(t *testing.T) {
		t.Parallel()
		bogus := &aicr.RecipeResult{Name: "made-up", Version: "v0"}
		_, err := client.MakeBundle(t.Context(), bogus, aicr.BundleOptions{})
		assertInvalidRequest(t, err)
	})

	t.Run("nil receiver", func(t *testing.T) {
		t.Parallel()
		var nilClient *aicr.Client
		_, err := nilClient.MakeBundle(t.Context(), &aicr.RecipeResult{}, aicr.BundleOptions{})
		assertInvalidRequest(t, err)
	})
}

// TestMakeBundle_RejectsCrossClientRecipe pins the owner-token guard for
// MakeBundle: a RecipeResult resolved by Client A cannot be bundled by
// Client B.
func TestMakeBundle_RejectsCrossClientRecipe(t *testing.T) {
	t.Parallel()

	clientA, recA := resolveEmbeddedTrainingRecipe(t)
	_ = clientA

	clientB, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient B: %v", err)
	}
	t.Cleanup(func() { _ = clientB.Close() })

	_, err = clientB.MakeBundle(t.Context(), recA, aicr.BundleOptions{OutputDir: t.TempDir()})
	assertInvalidRequest(t, err)
}

// TestMakeBundle_TimeoutOptIn pins the opt-in timeout contract: with
// Timeout unset (0), MakeBundle adds NO facade-level deadline, so a bundle
// that fits well within the caller's context succeeds; an already-expired
// caller deadline still governs (the facade does not paper over it). This
// guards the regression where the CLI bundle path inherited the REST 60s
// cap. The REST handler's own 60s cap is asserted separately in the api
// package; here we only verify the facade no longer self-imposes one.
func TestMakeBundle_TimeoutOptIn(t *testing.T) {
	t.Parallel()

	t.Run("zero timeout adds no facade cap", func(t *testing.T) {
		t.Parallel()
		client, rec := resolveEmbeddedTrainingRecipe(t)

		// Caller passes a generous context and Timeout: 0. The bundle
		// must complete without the facade injecting a deadline.
		out, err := client.MakeBundle(t.Context(), rec, aicr.BundleOptions{
			Config: config.NewConfig(
				config.WithVersion("v-test"),
				config.WithDeployer(config.DeployerHelm),
			),
			OutputDir: t.TempDir(),
			Timeout:   0,
		})
		if err != nil {
			t.Fatalf("MakeBundle with Timeout=0: %v", err)
		}
		if out == nil || out.TotalFiles == 0 {
			t.Fatal("MakeBundle with Timeout=0 produced no artifact")
		}
	})

	t.Run("already-expired caller deadline governs", func(t *testing.T) {
		t.Parallel()
		client, rec := resolveEmbeddedTrainingRecipe(t)

		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()

		_, err := client.MakeBundle(ctx, rec, aicr.BundleOptions{
			Config: config.NewConfig(
				config.WithVersion("v-test"),
				config.WithDeployer(config.DeployerHelm),
			),
			OutputDir: t.TempDir(),
			Timeout:   0,
		})
		if err == nil {
			t.Fatal("expected error from already-expired caller deadline, got nil")
		}
	})
}

// fixtureBundleAttester returns fixed, non-nil bundle JSON from Attest so the
// bundler reaches verifyAndCopyBinaryAttestation (a nil return short-circuits
// attestBundle before the binary attestation is embedded). It stands in for the
// real keyless attester the CLI/server build under --attest.
type fixtureBundleAttester struct {
	bundleJSON []byte
}

func (a *fixtureBundleAttester) Attest(_ context.Context, _ attestation.AttestSubject) ([]byte, error) {
	return a.bundleJSON, nil
}

func (a *fixtureBundleAttester) Identity() string { return "fixture" }

func (a *fixtureBundleAttester) HasRekorEntry() bool { return false }

// TestMakeBundle_InjectedBinaryAttestation proves that BundleOptions.BinaryAttestation
// bytes flow through MakeBundle into the produced bundle as the embedded tool
// provenance, and confirms the additivity contract: with no attestation enabled
// and no injected bytes, no attestation directory is emitted (the CLI/default path).
func TestMakeBundle_InjectedBinaryAttestation(t *testing.T) {
	t.Parallel()

	embeddedPathOf := func(dir string) string {
		return filepath.Join(dir, filepath.FromSlash(attestation.BinaryAttestationFile))
	}

	t.Run("injected bytes are embedded when attest enabled", func(t *testing.T) {
		t.Parallel()
		client, rec := resolveEmbeddedTrainingRecipe(t)

		fixture := []byte(`{"pre-verified":"binary-attestation"}`)
		outDir := t.TempDir()

		out, err := client.MakeBundle(t.Context(), rec, aicr.BundleOptions{
			Config: config.NewConfig(
				config.WithVersion("v-test"),
				config.WithDeployer(config.DeployerHelm),
				config.WithAttest(true),
			),
			// A non-nil bundle JSON is required so attestBundle does not
			// short-circuit before embedding the binary attestation.
			Attester:          &fixtureBundleAttester{bundleJSON: []byte(`{"bundle":true}`)},
			OutputDir:         outDir,
			BinaryAttestation: fixture,
		})
		if err != nil {
			t.Fatalf("MakeBundle: %v", err)
		}
		if out == nil || out.HasErrors() {
			t.Fatalf("MakeBundle produced errors: %+v", out)
		}

		got, err := os.ReadFile(embeddedPathOf(outDir))
		if err != nil {
			t.Fatalf("reading embedded binary attestation %s: %v", attestation.BinaryAttestationFile, err)
		}
		if !bytes.Equal(got, fixture) {
			t.Errorf("embedded binary attestation = %q, want %q", got, fixture)
		}
	})

	t.Run("default path emits no attestation dir", func(t *testing.T) {
		t.Parallel()
		client, rec := resolveEmbeddedTrainingRecipe(t)

		outDir := t.TempDir()
		out, err := client.MakeBundle(t.Context(), rec, aicr.BundleOptions{
			Config: config.NewConfig(
				config.WithVersion("v-test"),
				config.WithDeployer(config.DeployerHelm),
			),
			OutputDir: outDir,
			// No Attester, no BinaryAttestation, no --attest: the CLI default.
		})
		if err != nil {
			t.Fatalf("MakeBundle (default): %v", err)
		}
		if out == nil || out.HasErrors() {
			t.Fatalf("MakeBundle (default) produced errors: %+v", out)
		}
		if _, statErr := os.Stat(embeddedPathOf(outDir)); !os.IsNotExist(statErr) {
			t.Errorf("expected no binary attestation on default path, stat err = %v", statErr)
		}
	})
}

// TestMakeBundle_RejectsAttestGateMismatch pins the reconciliation MakeBundle
// applies when Attester is nil (so OIDCResolve.Attest would actually be
// consulted to derive one): a supplied Config whose baked-in Attest() gate
// disagrees with OIDCResolve.Attest is rejected up front. Left unchecked,
// Config.Attest()=true with OIDCResolve.Attest=false would silently produce
// unsigned output that looks attested (no signer gets derived, so
// attestBundle falls back to the bundler's no-op attester), and the reverse
// would derive a real signer whose result Config.Attest()=false then
// discards — both fail-open in a way a supply-chain-adjacent gate must not.
func TestMakeBundle_RejectsAttestGateMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		configAttest  bool
		resolveAttest bool
	}{
		{"Config says attest, OIDCResolve says no", true, false},
		{"Config says no, OIDCResolve says attest", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, rec := resolveEmbeddedTrainingRecipe(t)

			_, err := client.MakeBundle(t.Context(), rec, aicr.BundleOptions{
				Config: config.NewConfig(
					config.WithVersion("v-test"),
					config.WithAttest(tt.configAttest),
				),
				OIDCResolve: aicr.OIDCResolveOptions{Attest: tt.resolveAttest},
				OutputDir:   t.TempDir(),
				// Attester left nil: a caller-supplied Attester would win
				// outright and never consult either gate, which is exactly
				// why the check only fires here.
			})
			assertInvalidRequest(t, err)
		})
	}
}

// TestMakeBundle_AttestGateAgreementIsFine proves the reconciliation check
// only rejects a DISAGREEMENT: Config and OIDCResolve.Attest set to the same
// value (in either direction) must bundle successfully, matching how both
// current callers (the CLI, which always supplies Attester so this path is
// never reached, and the REST handler, which sets Config's Attest and
// Attester together) already behave.
// Named for the case it actually covers. The true/true agreement is NOT
// exercised here: it derives a real attester through ResolveAttesterLazy,
// which needs a signing identity this test cannot supply deterministically.
func TestMakeBundle_AttestGateDisabledAgreementIsFine(t *testing.T) {
	t.Parallel()

	client, rec := resolveEmbeddedTrainingRecipe(t)

	out, err := client.MakeBundle(t.Context(), rec, aicr.BundleOptions{
		Config: config.NewConfig(
			config.WithVersion("v-test"),
			config.WithAttest(false),
		),
		OIDCResolve: aicr.OIDCResolveOptions{Attest: false},
		OutputDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("MakeBundle: %v", err)
	}
	if out == nil || out.HasErrors() {
		t.Fatalf("MakeBundle produced errors: %+v", out)
	}
}

// The mismatch must be rejected on the FLAT-field path too, not only when a
// pre-built Config is supplied. Attest:true with OIDCResolve.Attest:false
// derives no signer, so the bundler would fall back to the no-op attester and
// emit an unsigned bundle that looks attested.
func TestMakeBundle_AttestGateMismatchRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts aicr.BundleOptions
	}{
		{
			name: "flat Attest true, OIDCResolve false",
			opts: aicr.BundleOptions{Attest: true},
		},
		{
			name: "flat Attest false, OIDCResolve true",
			opts: aicr.BundleOptions{
				OIDCResolve: aicr.OIDCResolveOptions{Attest: true},
			},
		},
		{
			name: "Config says false, OIDCResolve says true",
			opts: aicr.BundleOptions{
				Config:      config.NewConfig(config.WithAttest(false)),
				OIDCResolve: aicr.OIDCResolveOptions{Attest: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, rec := resolveEmbeddedTrainingRecipe(t)
			opts := tt.opts
			opts.OutputDir = t.TempDir()

			_, err := client.MakeBundle(t.Context(), rec, opts)
			assertInvalidRequest(t, err)
		})
	}
}

func assertInvalidRequest(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
	}
}
