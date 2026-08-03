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

package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// fixtureBundleAttester returns fixed, non-nil bundle JSON from Attest so the
// bundler does not short-circuit attestBundle (a nil return skips embedding the
// binary attestation). It stands in for the real keyless/KMS attester the
// server builds under ?attest=true.
type fixtureBundleAttester struct {
	bundleJSON []byte
}

func (a *fixtureBundleAttester) Attest(_ context.Context, _ attestation.AttestSubject) ([]byte, error) {
	return a.bundleJSON, nil
}

func (a *fixtureBundleAttester) Identity() string { return "fixture" }

func (a *fixtureBundleAttester) HasRekorEntry() bool { return false }

var testBundleZipHeaders = []string{
	"Content-Disposition",
	"X-Bundle-Files",
	"X-Bundle-Size",
	"X-Bundle-Duration",
}

func newTestBundleHandler(t *testing.T) *bundleHandler {
	t.Helper()
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return newBundleHandler(client, nil, nil)
}

func TestDecodeRecipeResultRequestStrictForConfiguredRecipes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "legacy permits unknown field",
			body: `{"apiVersion":"aicr.run/v1alpha2","kind":"RecipeResult","legacyExtension":true}`,
		},
		{
			name: "configured recipe succeeds",
			body: `{"apiVersion":"aicr.run/v1alpha3","kind":"RecipeResult","configuration":{"slurm":{"accounting":{"mode":"disabled"}}}}`,
		},
		{
			name:    "configured rejects unknown field",
			body:    `{"apiVersion":"aicr.run/v1alpha3","kind":"RecipeResult","configuration":{"slurm":{"accounting":{"mode":"disabled"}}},"unknownField":true}`,
			wantErr: true,
		},
		{
			name:    "rejects trailing document",
			body:    `{"apiVersion":"aicr.run/v1alpha3","kind":"RecipeResult"} {}`,
			wantErr: true,
		},
		{
			name:    "legacy rejects trailing document too",
			body:    `{"apiVersion":"aicr.run/v1alpha2","kind":"RecipeResult"} {}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var result recipe.RecipeResult
			err := decodeRecipeResultRequest(strings.NewReader(tt.body), &result)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeRecipeResultRequest() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// resolveEmbeddedBundleBody resolves a known-good embedded recipe and returns
// its wire-format JSON (the pkg/recipe.RecipeResult shape the bundle handler
// decodes), so attest tests can drive a real, successful bundle end-to-end.
func resolveEmbeddedBundleBody(t *testing.T) []byte {
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
	body, err := json.Marshal(rec.Resolved())
	if err != nil {
		t.Fatalf("marshal recipe: %v", err)
	}
	return body
}

func profileBundleBody(t *testing.T) []byte {
	t.Helper()
	result := &recipe.RecipeResult{
		APIVersion: recipe.RecipeProfileAPIVersion,
		Kind:       recipe.RecipeResultKind,
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceAKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			Intent:      recipe.CriteriaIntentTraining,
		},
		ComponentRefs: []recipe.ComponentRef{{
			Name:    "gpu-operator",
			Version: "v25.3.3",
			Type:    recipe.ComponentTypeHelm,
			Source:  "https://helm.ngc.nvidia.com/nvidia",
			Chart:   "gpu-operator",
			Overrides: map[string]any{
				"driver": map[string]any{"enabled": false},
			},
		}},
		DeploymentOrder: []string{"gpu-operator"},
	}
	result.Metadata.SelectedProfile = &recipe.SelectedProfile{
		Name:  "gpuStack",
		Value: "driver-installed",
		OwnedPaths: map[string][]string{
			"gpu-operator": {"driver.enabled", "enabled"},
		},
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal profile recipe: %v", err)
	}
	return body
}

func TestDecodeBundleRecipeV2ContentType(t *testing.T) {
	body := profileBundleBody(t)
	tests := []struct {
		name        string
		contentType string
		wantErr     string
	}{
		{
			name:        "JSON parameter containing yaml remains JSON",
			contentType: "application/json; note=yaml",
		},
		{
			name:    "missing content type",
			wantErr: "Content-Type is required",
		},
		{
			name:        "unsupported content type",
			contentType: "text/plain",
			wantErr:     `unsupported Content-Type "text/plain"`,
		},
		{
			name:        "undeclared application YAML alias",
			contentType: "application/yaml",
			wantErr:     `unsupported Content-Type "application/yaml"`,
		},
		{
			name:        "undeclared text YAML alias",
			contentType: "text/yaml",
			wantErr:     `unsupported Content-Type "text/yaml"`,
		},
		{
			name:        "vendor JSON is not declared",
			contentType: "application/vnd.example+json",
			wantErr:     `unsupported Content-Type "application/vnd.example+json"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeBundleRecipe(bytes.NewReader(body), tt.contentType, true)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("decodeBundleRecipe() error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("decodeBundleRecipe() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestProfileAwareBundleEndpoints(t *testing.T) {
	h := newTestBundleHandler(t)
	post := func(t *testing.T, v2 bool, query string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		path := "/v1/bundle"
		if v2 {
			path = "/v2/bundle"
		}
		path += query
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		if v2 {
			h.HandleBundlesV2(w, req)
		} else {
			h.HandleBundles(w, req)
		}
		return w
	}

	t.Run("v1 rejects profile artifact", func(t *testing.T) {
		w := post(t, false, "", profileBundleBody(t))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("v2 accepts profile artifact", func(t *testing.T) {
		w := post(t, true, "", profileBundleBody(t))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Content-Type") != "application/zip" {
			t.Fatalf("Content-Type = %q, want application/zip", w.Header().Get("Content-Type"))
		}
	})

	t.Run("v2 accepts legacy artifact", func(t *testing.T) {
		w := post(t, true, "", resolveEmbeddedBundleBody(t))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("v2 strictly rejects unknown artifact field", func(t *testing.T) {
		body := profileBundleBody(t)
		body = bytes.Replace(body, []byte(`"componentRefs"`), []byte(`"profie":true,"componentRefs"`), 1)
		w := post(t, true, "", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("v2 strictly rejects unknown excluded overlay field", func(t *testing.T) {
		body := profileBundleBody(t)
		body = bytes.Replace(
			body,
			[]byte(`"metadata":{`),
			[]byte(`"metadata":{"excludedOverlays":[{"name":"overlay-a","reasn":"constraint-failed"}],`),
			1,
		)
		w := post(t, true, "", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})

	invalidMetadataItems := []struct {
		name        string
		fixtureBody func(*testing.T) []byte
		replacement string
	}{
		{
			name:        "v2 rejects incomplete profile metadata item",
			fixtureBody: profileBundleBody,
			replacement: `"metadata":{"excludedOverlays":[{"reason":"constraint-failed"}],`,
		},
		{
			name:        "v2 rejects null legacy excluded overlay",
			fixtureBody: resolveEmbeddedBundleBody,
			replacement: `"metadata":{"excludedOverlays":[null],`,
		},
	}
	for _, tt := range invalidMetadataItems {
		t.Run(tt.name, func(t *testing.T) {
			body := bytes.Replace(
				tt.fixtureBody(t),
				[]byte(`"metadata":{`),
				[]byte(tt.replacement),
				1,
			)
			w := post(t, true, "", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
		})
	}

	t.Run("v2 rejects profile version without selection", func(t *testing.T) {
		var result recipe.RecipeResult
		if err := json.Unmarshal(profileBundleBody(t), &result); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		result.Metadata.SelectedProfile = nil
		body, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		w := post(t, true, "", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("v2 rejects unknown query parameter", func(t *testing.T) {
		w := post(t, true, "?profie=gpuStack%3Ddriver-installed", profileBundleBody(t))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})

	for _, tt := range []struct {
		name  string
		query string
	}{
		{
			name:  "v2 rejects set override of profile-owned path",
			query: "?set=gpu-operator:driver.enabled=true",
		},
		{
			name:  "v2 rejects dynamic override of profile-owned path",
			query: "?dynamic=gpu-operator:driver.enabled",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := post(t, true, tt.query, profileBundleBody(t))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestBundleHandler_Attest pins the ?attest=true signing seam: a configured
// server signs via the injected attesterBuilder, an unconfigured server rejects
// the request with 400, and the default (no attest) path never touches signing.
func TestBundleHandler_Attest(t *testing.T) {
	const kmsKey = "awskms:///alias/aicr-signing"

	body := resolveEmbeddedBundleBody(t)

	newHandler := func(t *testing.T, signing *signingConfig, builder attesterBuilder) *bundleHandler {
		t.Helper()
		h := newTestBundleHandler(t)
		h.signing = signing
		if builder != nil {
			h.newAttester = builder
		}
		return h
	}

	post := func(h *bundleHandler, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.HandleBundles(w, req)
		return w
	}

	t.Run("attest=true wires configured KMS signing", func(t *testing.T) {
		var called bool
		var gotOpts attestation.ResolveOptions
		h := newHandler(t,
			// binaryAttestation is required now that attest=true enables
			// Config.Attest(): bundler.New's fail-fast gate is satisfied by the
			// injected bytes (the startup-verified tool provenance).
			&signingConfig{
				enabled:           true,
				signingKey:        kmsKey,
				tlogUpload:        true,
				binaryAttestation: []byte("fixture-attestation"),
			},
			func(_ context.Context, opts attestation.ResolveOptions) (attestation.Attester, error) {
				called = true
				gotOpts = opts
				return attestation.NewNoOpAttester(), nil
			})

		w := post(h, "/v1/bundle?attest=true")

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d. Body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if !called {
			t.Fatal("newAttester was not called for attest=true")
		}
		if !gotOpts.Attest {
			t.Error("ResolveOptions.Attest = false, want true")
		}
		if gotOpts.SigningKey != kmsKey {
			t.Errorf("ResolveOptions.SigningKey = %q, want %q", gotOpts.SigningKey, kmsKey)
		}
	})

	t.Run("attest=true fails 500 without leaking builder error", func(t *testing.T) {
		const secretErr = "super-secret-kms-internal-failure-detail"
		h := newHandler(t,
			&signingConfig{enabled: true, signingKey: kmsKey, tlogUpload: true},
			func(context.Context, attestation.ResolveOptions) (attestation.Attester, error) {
				return nil, aicrerrors.New(aicrerrors.ErrCodeInternal, secretErr)
			})

		w := post(h, "/v1/bundle?attest=true")

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d. Body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
		}
		if strings.Contains(w.Body.String(), secretErr) {
			t.Errorf("response leaked internal builder error: %s", w.Body.String())
		}
	})

	t.Run("attest=true fails 500 when identity token file is missing", func(t *testing.T) {
		// Use the DEFAULT newAttester (no injected builder) so the real
		// resolveOptions runs and fails reading the non-existent token file.
		h := newHandler(t, &signingConfig{
			enabled:           true,
			keyless:           true,
			fulcioURL:         "https://fulcio.example",
			identityTokenFile: filepath.Join(t.TempDir(), "does-not-exist.token"),
		}, nil)

		w := post(h, "/v1/bundle?attest=true")

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d. Body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
		}
	})

	t.Run("attest=notabool rejected with 400", func(t *testing.T) {
		h := newHandler(t,
			&signingConfig{enabled: true, signingKey: kmsKey, tlogUpload: true},
			nil)

		w := post(h, "/v1/bundle?attest=notabool")

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d. Body: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("attest=true rejected when not configured", func(t *testing.T) {
		cases := []struct {
			name    string
			signing *signingConfig
		}{
			{"nil signing", nil},
			{"disabled signing", &signingConfig{enabled: false, signingKey: kmsKey}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var called bool
				h := newHandler(t, tc.signing,
					func(context.Context, attestation.ResolveOptions) (attestation.Attester, error) {
						called = true
						return attestation.NewNoOpAttester(), nil
					})

				w := post(h, "/v1/bundle?attest=true")

				if w.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want %d. Body: %s", w.Code, http.StatusBadRequest, w.Body.String())
				}
				if called {
					t.Error("newAttester was called despite signing being unconfigured")
				}
			})
		}
	})

	t.Run("attest absent leaves signing untouched", func(t *testing.T) {
		var called bool
		h := newHandler(t,
			&signingConfig{enabled: true, signingKey: kmsKey, tlogUpload: true},
			func(context.Context, attestation.ResolveOptions) (attestation.Attester, error) {
				called = true
				return attestation.NewNoOpAttester(), nil
			})

		w := post(h, "/v1/bundle")

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d. Body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if called {
			t.Error("newAttester was called for a request without attest=true")
		}
	})
}

// TestBundleHandler_AttestEmbedsBinaryAttestation is the end-to-end proof that
// server signing embeds the cached tool provenance: with signing enabled, a
// non-empty binaryAttestation, and a real (non-nil JSON) attester, the streamed
// bundle zip contains attestation/aicr-attestation.sigstore.json equal to the
// cached binary attestation bytes. The zip staging step re-verifies checksums
// (not Sigstore signatures), so a fixture attestation survives the stream path.
func TestBundleHandler_AttestEmbedsBinaryAttestation(t *testing.T) {
	const kmsKey = "awskms:///alias/aicr-signing"
	fixtureBinary := []byte(`{"pre-verified":"server-binary-attestation"}`)
	body := resolveEmbeddedBundleBody(t)

	h := newTestBundleHandler(t)
	h.signing = &signingConfig{
		enabled:           true,
		signingKey:        kmsKey,
		tlogUpload:        true,
		binaryAttestation: fixtureBinary,
	}
	// A non-nil bundle JSON is required so attestBundle does not short-circuit
	// before embedding the binary attestation.
	h.newAttester = func(context.Context, attestation.ResolveOptions) (attestation.Attester, error) {
		return &fixtureBundleAttester{bundleJSON: []byte(`{"bundle":true}`)}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/bundle?attest=true", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleBundles(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. Body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("open bundle zip: %v", err)
	}

	var got []byte
	for _, f := range zr.File {
		if f.Name != attestation.BinaryAttestationFile {
			continue
		}
		rc, openErr := f.Open()
		if openErr != nil {
			t.Fatalf("open %s in zip: %v", f.Name, openErr)
		}
		got, err = io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s in zip: %v", f.Name, err)
		}
		break
	}
	if got == nil {
		t.Fatalf("bundle zip missing %s", attestation.BinaryAttestationFile)
	}
	if !bytes.Equal(got, fixtureBinary) {
		t.Errorf("embedded binary attestation = %q, want %q", got, fixtureBinary)
	}
}

// TestBundleHandler_MethodGate verifies only POST is accepted.
func TestBundleHandler_MethodGate(t *testing.T) {
	t.Parallel()
	h := newTestBundleHandler(t)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(method, "/v1/bundle", nil)
			w := httptest.NewRecorder()
			h.HandleBundles(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
			}
			if allow := w.Header().Get("Allow"); allow != http.MethodPost {
				t.Errorf("Allow = %q, want %q", allow, http.MethodPost)
			}
		})
	}
}

// TestBundleHandler_EmptyComponentRefs verifies a recipe with no components is
// rejected with 400.
func TestBundleHandler_EmptyComponentRefs(t *testing.T) {
	t.Parallel()
	h := newTestBundleHandler(t)

	body := `{"apiVersion": "aicr.run/v1alpha2", "kind": "RecipeResult", "componentRefs": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleBundles(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d. Body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestBundleHandler_IncoherentComponentRef verifies the HTTP decode-to-bundle
// path rejects an incoherent ref (a Helm component carrying a Kustomize tag)
// with 400 rather than producing a mismatched bundle. Pins issue #1584 at the
// POST /v1/bundle boundary.
func TestBundleHandler_IncoherentComponentRef(t *testing.T) {
	t.Parallel()
	h := newTestBundleHandler(t)

	body := `{"apiVersion": "aicr.run/v1alpha2", "kind": "RecipeResult", "componentRefs": [` +
		`{"name": "gpu-operator", "type": "Helm", "version": "v1", "tag": "v2"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleBundles(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d. Body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestBundleHandler_LegacyRecipeHeaders pins the backward compatibility the
// BundleRecipeRequest schema advertises: POST /v1/bundle accepts a recipe whose
// header fields are absent, empty, or carry the legacy Recipe kind this
// contract published through v0.18.0.
//
// The handler validates supported non-empty apiVersions while preserving this
// explicit legacy window. The canonical case is included deliberately as a
// control: it proves a 200 here means the body was accepted, not that the
// assertion is vacuous.
func TestBundleHandler_LegacyRecipeHeaders(t *testing.T) {
	t.Parallel()

	canonical := resolveEmbeddedBundleBody(t)

	// Fail closed if the fixture ever stops being the canonical shape: a
	// "legacy" case below must differ from the baseline to be meaningful.
	var fixture map[string]any
	if err := json.Unmarshal(canonical, &fixture); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if got := fixture["kind"]; got != recipe.RecipeResultKind {
		t.Fatalf("fixture kind = %v, want %q", got, recipe.RecipeResultKind)
	}
	if got := fixture["apiVersion"]; got != recipe.RecipeAPIVersion {
		t.Fatalf("fixture apiVersion = %v, want %q", got, recipe.RecipeAPIVersion)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name:   "canonical headers (control)",
			mutate: func(map[string]any) {},
		},
		{
			name: "both headers absent",
			mutate: func(body map[string]any) {
				delete(body, "kind")
				delete(body, "apiVersion")
			},
		},
		{
			name: "both headers empty-string",
			mutate: func(body map[string]any) {
				body["kind"] = ""
				body["apiVersion"] = ""
			},
		},
		// The schema constrains apiVersion and kind independently — neither is
		// in a required[] and each admits its own empty value — and
		// pkg/recipe/loader.go tolerates each one missing on its own ("empty
		// kind allowed", "empty apiVersion allowed for backward compat" in
		// loader_test.go). Mutating both together would leave a paired check
		// ("either both canonical or both absent") passing every case above
		// while it broke the single-field forms below.
		{
			name: "kind absent, apiVersion canonical",
			mutate: func(body map[string]any) {
				delete(body, "kind")
			},
		},
		{
			name: "apiVersion absent, kind canonical",
			mutate: func(body map[string]any) {
				delete(body, "apiVersion")
			},
		},
		{
			name: "kind empty, apiVersion canonical",
			mutate: func(body map[string]any) {
				body["kind"] = ""
			},
		},
		{
			name: "apiVersion empty, kind canonical",
			mutate: func(body map[string]any) {
				body["apiVersion"] = ""
			},
		},
		{
			name: "legacy Recipe kind with apiVersion absent",
			mutate: func(body map[string]any) {
				body["kind"] = string(header.KindRecipe)
				delete(body, "apiVersion")
			},
		},
		{
			name: "legacy Recipe kind",
			mutate: func(body map[string]any) {
				body["kind"] = string(header.KindRecipe)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var body map[string]any
			if err := json.Unmarshal(canonical, &body); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			tt.mutate(body)
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}

			h := newTestBundleHandler(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/bundle", bytes.NewReader(encoded))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.HandleBundles(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want %d. Body: %s", w.Code, http.StatusOK, w.Body.String())
			}
		})
	}
}

func TestBundleHandler_StreamZipFailureBeforeCommit(t *testing.T) {
	tests := []struct {
		name       string
		streamErr  error
		wantStatus int
		wantCode   aicrerrors.ErrorCode
	}{
		{
			name:       "integrity failure becomes internal",
			streamErr:  aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "private bundle path is unmanaged"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   aicrerrors.ErrCodeInternal,
		},
		{
			name:       "internal failure remains internal",
			streamErr:  aicrerrors.New(aicrerrors.ErrCodeInternal, "private archive implementation failure"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   aicrerrors.ErrCodeInternal,
		},
		{
			name:       "timeout remains timeout",
			streamErr:  aicrerrors.New(aicrerrors.ErrCodeTimeout, "private archive deadline detail"),
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   aicrerrors.ErrCodeTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &bundleHandler{
				streamZip: func(_ context.Context, w http.ResponseWriter, _ string, _ *result.Output) error {
					w.Header().Set("Content-Type", "application/zip")
					w.Header().Set("Content-Disposition", "attachment; filename=private.zip")
					w.Header().Set("X-Bundle-Files", "99")
					w.Header().Set("X-Bundle-Size", "999")
					w.Header().Set("X-Bundle-Duration", "private-duration")
					return tt.streamErr
				},
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/bundle", nil)
			recorder := httptest.NewRecorder()
			h.writeZipResponse(req.Context(), recorder, req, "unused", &result.Output{})

			if recorder.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", contentType)
			}
			for _, header := range testBundleZipHeaders {
				if value := recorder.Header().Get(header); value != "" {
					t.Errorf("header %s = %q, want empty", header, value)
				}
			}
			if strings.Contains(recorder.Body.String(), "private") {
				t.Fatalf("response leaked private archive error: %s", recorder.Body.String())
			}
			var response errorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if response.Code != string(tt.wantCode) {
				t.Errorf("error code = %q, want %q", response.Code, tt.wantCode)
			}
		})
	}
}

func TestBundleHandler_StreamZipFailureAfterCommit(t *testing.T) {
	h := &bundleHandler{
		streamZip: func(_ context.Context, w http.ResponseWriter, _ string, _ *result.Output) error {
			w.Header().Set("Content-Type", "application/zip")
			if _, err := w.Write([]byte("partial zip")); err != nil {
				return err
			}
			return aicrerrors.New(aicrerrors.ErrCodeInternal, "private archive failure")
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", nil)
	recorder := httptest.NewRecorder()
	h.writeZipResponse(req.Context(), recorder, req, "unused", &result.Output{})

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got, want := recorder.Body.String(), "partial zip"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", contentType)
	}
}
