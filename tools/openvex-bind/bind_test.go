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

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

const (
	amd64Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	arm64Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	benchImage  = "ghcr.io/nvidia/aicr-validators/aiperf-bench"
)

// sourceDocument is a minimal but structurally faithful stand-in for
// .openvex.json: two product aliases per statement, one statement scoped to a
// different image, and the document-level fields the projection must carry
// through unchanged.
const sourceDocument = `{
  "@context": "https://openvex.dev/ns/v0.2.0",
  "@id": "https://github.com/NVIDIA/aicr/.openvex.json",
  "author": "NVIDIA AICR maintainers",
  "role": "document creator",
  "timestamp": "2026-08-27T00:00:00Z",
  "version": 14,
  "tooling": "manual",
  "statements": [
    {
      "vulnerability": {"name": "GHSA-vjc4-5qp5-m44j", "description": "pillow"},
      "products": [
        {"@id": "pkg:oci/aicr-aiperf-bench", "identifiers": {"purl": "pkg:oci/aicr-aiperf-bench"}},
        {"@id": "pkg:oci/aiperf-bench", "identifiers": {"purl": "pkg:oci/aiperf-bench"}}
      ],
      "status": "not_affected",
      "justification": "vulnerable_code_cannot_be_controlled_by_adversary",
      "impact_statement": "peers are cluster-internal"
    },
    {
      "vulnerability": {"name": "GO-2026-5942"},
      "products": [
        {"@id": "pkg:oci/aicr-gate", "identifiers": {"purl": "pkg:oci/aicr-gate"}}
      ],
      "status": "not_affected",
      "justification": "component_not_present"
    }
  ]
}`

// decode renders a projection back into a tree so assertions read against
// structure rather than against formatting.
func decode(t *testing.T, document []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatalf("projection is not valid JSON: %v\n%s", err, document)
	}
	return doc
}

// statementsOf extracts the statement list from a decoded projection.
func statementsOf(t *testing.T, doc map[string]any) []any {
	t.Helper()
	statements, ok := doc["statements"].([]any)
	if !ok {
		t.Fatalf("statements is %T, want []any", doc["statements"])
	}
	return statements
}

func TestBindSelectsAndRewritesProducts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		image       string
		digest      string
		wantKept    int
		wantDropped int
		wantPURL    string
	}{
		{
			name:        "bare alias matches the image basename",
			image:       benchImage,
			digest:      amd64Digest,
			wantKept:    1,
			wantDropped: 1,
			wantPURL:    "pkg:oci/aiperf-bench@" + amd64Digest,
		},
		{
			name:        "arm64 binds the other platform manifest",
			image:       benchImage,
			digest:      arm64Digest,
			wantKept:    1,
			wantDropped: 1,
			wantPURL:    "pkg:oci/aiperf-bench@" + arm64Digest,
		},
		{
			name:        "aicr- prefixed image basename folds to the bare alias",
			image:       "ghcr.io/nvidia/aicr-gate",
			digest:      amd64Digest,
			wantKept:    1,
			wantDropped: 1,
			wantPURL:    "pkg:oci/aicr-gate@" + amd64Digest,
		},
		{
			name:        "image with no statements yields an empty projection",
			image:       "ghcr.io/nvidia/aicr",
			digest:      amd64Digest,
			wantKept:    0,
			wantDropped: 2,
		},
		{
			name:        "tagged reference resolves to the same basename",
			image:       benchImage + ":v1.2.3",
			digest:      amd64Digest,
			wantKept:    1,
			wantDropped: 1,
			wantPURL:    "pkg:oci/aiperf-bench@" + amd64Digest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := Bind([]byte(sourceDocument), Options{Image: tt.image, Digest: tt.digest})
			if err != nil {
				t.Fatalf("Bind() error = %v", err)
			}
			if result.Kept != tt.wantKept || result.Dropped != tt.wantDropped {
				t.Fatalf("Bind() kept/dropped = %d/%d, want %d/%d",
					result.Kept, result.Dropped, tt.wantKept, tt.wantDropped)
			}
			statements := statementsOf(t, decode(t, result.Document))
			if len(statements) != tt.wantKept {
				t.Fatalf("projection has %d statements, want %d", len(statements), tt.wantKept)
			}
			if tt.wantKept == 0 {
				return
			}
			products := statements[0].(map[string]any)["products"].([]any)
			if len(products) != 1 {
				t.Fatalf("statement has %d products, want exactly the bound one", len(products))
			}
			product := products[0].(map[string]any)
			identifiers := product["identifiers"].(map[string]any)
			if got := product["@id"]; got != tt.wantPURL {
				t.Errorf("product @id = %v, want %s", got, tt.wantPURL)
			}
			if got := identifiers["purl"]; got != tt.wantPURL {
				t.Errorf("product purl = %v, want %s", got, tt.wantPURL)
			}
		})
	}
}

// TestBindPassesCuratedFieldsThrough is the substantive guarantee: the
// published judgment must be the reviewed judgment. Any status, justification
// or impact rewrite here would be a silent claim we never triaged.
func TestBindPassesCuratedFieldsThrough(t *testing.T) {
	t.Parallel()
	result, err := Bind([]byte(sourceDocument), Options{Image: benchImage, Digest: amd64Digest})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	doc := decode(t, result.Document)
	statement := statementsOf(t, doc)[0].(map[string]any)

	for field, want := range map[string]string{
		"status":           "not_affected",
		"justification":    "vulnerable_code_cannot_be_controlled_by_adversary",
		"impact_statement": "peers are cluster-internal",
	} {
		if got := statement[field]; got != want {
			t.Errorf("statement %s = %v, want %q", field, got, want)
		}
	}
	if got := statement["vulnerability"].(map[string]any)["name"]; got != "GHSA-vjc4-5qp5-m44j" {
		t.Errorf("vulnerability name = %v, want GHSA-vjc4-5qp5-m44j", got)
	}

	// Document-level identity carries through so a consumer can trace the
	// projection back to the reviewed file; only @id changes, because the
	// projection is a different document from its source.
	for field, want := range map[string]any{
		"@context":  "https://openvex.dev/ns/v0.2.0",
		"author":    "NVIDIA AICR maintainers",
		"role":      "document creator",
		"timestamp": "2026-08-27T00:00:00Z",
		"tooling":   "manual",
	} {
		if got := doc[field]; got != want {
			t.Errorf("document %s = %v, want %v", field, got, want)
		}
	}
	if got := doc["version"]; got != float64(14) {
		t.Errorf("document version = %v, want 14", got)
	}
	wantID := "https://github.com/NVIDIA/aicr/.openvex.json#aiperf-bench@" + amd64Digest
	if got := doc["@id"]; got != wantID {
		t.Errorf("document @id = %v, want %s", got, wantID)
	}
}

// TestBindIsDeterministic covers the artifact-generator rule in CLAUDE.md: no
// wall clock and no UUID, so the same inputs reproduce the same bytes. Go
// randomizes map iteration, so repeating the call also exercises the encoder's
// key ordering.
func TestBindIsDeterministic(t *testing.T) {
	t.Parallel()
	var first []byte
	for i := range 8 {
		result, err := Bind([]byte(sourceDocument), Options{Image: benchImage, Digest: amd64Digest})
		if err != nil {
			t.Fatalf("Bind() error = %v", err)
		}
		if i == 0 {
			first = result.Document
			continue
		}
		if !bytes.Equal(first, result.Document) {
			t.Fatalf("run %d differs from run 0:\n%s\n---\n%s", i, first, result.Document)
		}
	}
	// A wall-clock stamp would show up as a year that is not the source's.
	if bytes.Contains(first, []byte("\"timestamp\": \"2026-08-27T00:00:00Z\"")) == false {
		t.Errorf("projection must carry the source timestamp verbatim:\n%s", first)
	}
}

// TestBindDoesNotMutateSource guards the invariant that .openvex.json remains
// the untouched source of truth.
//
// The byte comparison alone is weak: json.Unmarshal never writes into the
// caller's buffer, so it cannot fail. The invariant that can actually break is
// on the decoded tree, because Bind rewrites `products` and `@id` on every
// statement it keeps. Binding the same buffer twice against different digests
// is what exercises that: if the first projection leaked into shared state, the
// second would carry the first digest.
func TestBindDoesNotMutateSource(t *testing.T) {
	t.Parallel()
	source := []byte(sourceDocument)
	before := append([]byte(nil), source...)
	first, err := Bind(source, Options{Image: benchImage, Digest: amd64Digest})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if !bytes.Equal(before, source) {
		t.Error("Bind mutated its source buffer")
	}

	second, err := Bind(source, Options{Image: benchImage, Digest: arm64Digest})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	want := "pkg:oci/aiperf-bench@" + arm64Digest
	product := statementsOf(t, decode(t, second.Document))[0].(map[string]any)["products"].([]any)[0].(map[string]any)
	if got := product["@id"]; got != want {
		t.Errorf("second projection product @id = %v, want %s", got, want)
	}
	if identifiers := product["identifiers"].(map[string]any); identifiers["purl"] != want {
		t.Errorf("second projection purl = %v, want %s", identifiers["purl"], want)
	}
	if bytes.Equal(first.Document, second.Document) {
		t.Error("projections for two different digests are byte-identical")
	}
}

func TestBindSubcomponents(t *testing.T) {
	t.Parallel()
	const withSubcomponents = `{
  "@context": "https://openvex.dev/ns/v0.2.0",
  "@id": "https://example.test/vex",
  "author": "a",
  "timestamp": "2026-01-01T00:00:00Z",
  "version": 1,
  "statements": [
    {
      "vulnerability": {"name": "CVE-2026-0001"},
      "products": [
        {"@id": "pkg:oci/aicr-aiperf-bench", "subcomponents": [{"@id": "pkg:deb/debian/libssl3t64"}]},
        {"@id": "pkg:oci/aiperf-bench", "subcomponents": [
          {"@id": "pkg:deb/debian/libssl3t64"},
          {"@id": "pkg:deb/debian/openssl-provider-fips"}
        ]}
      ],
      "status": "not_affected",
      "justification": "vulnerable_code_not_in_execute_path"
    }
  ]
}`
	result, err := Bind([]byte(withSubcomponents), Options{Image: benchImage, Digest: amd64Digest})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	statements := statementsOf(t, decode(t, result.Document))
	product := statements[0].(map[string]any)["products"].([]any)[0].(map[string]any)
	want := []any{
		map[string]any{"@id": "pkg:deb/debian/libssl3t64"},
		map[string]any{"@id": "pkg:deb/debian/openssl-provider-fips"},
	}
	if got := product["subcomponents"]; !reflect.DeepEqual(got, want) {
		t.Errorf("subcomponents = %#v, want %#v (union of matching products, deduplicated, source order)", got, want)
	}
}

func TestBindRejectsBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		image  string
		digest string
	}{
		{name: "empty digest", source: sourceDocument, image: benchImage},
		{name: "short digest", source: sourceDocument, image: benchImage, digest: "sha256:abc"},
		{
			name:   "uppercase digest",
			source: sourceDocument,
			image:  benchImage,
			digest: "sha256:AAAA111111111111111111111111111111111111111111111111111111111111",
		},
		{name: "unprefixed digest", source: sourceDocument, image: benchImage, digest: strings.TrimPrefix(amd64Digest, "sha256:")},
		{name: "empty image", source: sourceDocument, digest: amd64Digest},
		{name: "image with whitespace", source: sourceDocument, image: " ghcr.io/nvidia/aicr", digest: amd64Digest},
		{name: "image with no repository name", source: sourceDocument, image: "ghcr.io/nvidia/", digest: amd64Digest},
		{name: "source is not JSON", source: "not json", image: benchImage, digest: amd64Digest},
		{name: "source is a JSON array", source: "[]", image: benchImage, digest: amd64Digest},
		{name: "source is JSON null", source: "null", image: benchImage, digest: amd64Digest},
		{
			name:   "document has no @id",
			source: `{"statements":[]}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			name:   "statements is not an array",
			source: `{"@id":"https://example.test/vex","statements":{}}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			name:   "statement is not an object",
			source: `{"@id":"https://example.test/vex","statements":["nope"]}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			name:   "statement lists no products",
			source: `{"@id":"https://example.test/vex","statements":[{"products":[]}]}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			name:   "statement omits products",
			source: `{"@id":"https://example.test/vex","statements":[{"status":"fixed"}]}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			name:   "product is not an object",
			source: `{"@id":"https://example.test/vex","statements":[{"products":["pkg:oci/aiperf-bench"]}]}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			// json.Decoder stops after the first value, so without an explicit
			// EOF check the suffix would be dropped and the truncated document
			// signed as if it were the whole thing.
			name:   "trailing JSON value after the document",
			source: sourceDocument + `{"@id":"https://example.test/second"}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			name:   "trailing scalar after the document",
			source: sourceDocument + ` 42`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			// Dropping a malformed subcomponents value would widen the
			// statement from specific packages to the whole image.
			name: "subcomponents is not an array",
			source: `{"@id":"https://example.test/vex","statements":[
				{"vulnerability":{"name":"CVE-2026-0001"},
				 "products":[{"@id":"pkg:oci/aiperf-bench","subcomponents":"libssl3t64"}],
				 "status":"fixed"}]}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			name: "subcomponents is an object",
			source: `{"@id":"https://example.test/vex","statements":[
				{"vulnerability":{"name":"CVE-2026-0001"},
				 "products":[{"@id":"pkg:oci/aiperf-bench","subcomponents":{"@id":"pkg:deb/x"}}],
				 "status":"fixed"}]}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			// OpenVEX types every subcomponents entry as a Component object. A
			// scalar marshals without complaint, so without an explicit check
			// it would be copied verbatim into the signed projection.
			name: "subcomponents array has a scalar member",
			source: `{"@id":"https://example.test/vex","statements":[
				{"vulnerability":{"name":"CVE-2026-0001"},
				 "products":[{"@id":"pkg:oci/aiperf-bench","subcomponents":[42]}],
				 "status":"fixed"}]}`,
			image:  benchImage,
			digest: amd64Digest,
		},
		{
			name: "subcomponents array mixes objects and scalars",
			source: `{"@id":"https://example.test/vex","statements":[
				{"vulnerability":{"name":"CVE-2026-0001"},
				 "products":[{"@id":"pkg:oci/aiperf-bench",
				   "subcomponents":[{"@id":"pkg:deb/debian/libssl3t64"},"libssl"]}],
				 "status":"fixed"}]}`,
			image:  benchImage,
			digest: amd64Digest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Bind([]byte(tt.source), Options{Image: tt.image, Digest: tt.digest}); err == nil {
				t.Fatal("Bind() error = nil, want a failure")
			}
		})
	}
}

func TestImageBasename(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		image   string
		want    string
		wantErr bool
	}{
		{name: "nested namespace", image: benchImage, want: "aiperf-bench"},
		{name: "single namespace", image: "ghcr.io/nvidia/aicr", want: "aicr"},
		{name: "tagged", image: "ghcr.io/nvidia/aicrd:v0.20.0", want: "aicrd"},
		{name: "digest pinned", image: "ghcr.io/nvidia/aicr-gate@" + amd64Digest, want: "aicr-gate"},
		{name: "bare name", image: "aicr", want: "aicr"},
		{name: "empty", image: "", wantErr: true},
		{name: "trailing slash", image: "ghcr.io/nvidia/", wantErr: true},
		{name: "leading space", image: " aicr", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := imageBasename(tt.image)
			if (err != nil) != tt.wantErr {
				t.Fatalf("imageBasename() error = %v, wantErr %t", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("imageBasename() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseOCIPURLName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		identifier string
		want       string
		wantOK     bool
	}{
		{name: "bare", identifier: "pkg:oci/aiperf-bench", want: "aiperf-bench", wantOK: true},
		{name: "digest qualified", identifier: "pkg:oci/aiperf-bench@" + amd64Digest, want: "aiperf-bench", wantOK: true},
		{
			name:       "with repository_url qualifier",
			identifier: "pkg:oci/aiperf-bench?repository_url=ghcr.io/nvidia",
			want:       "aiperf-bench",
			wantOK:     true,
		},
		{name: "non-oci purl", identifier: "pkg:deb/debian/libssl3t64", wantOK: false},
		{name: "not a purl", identifier: "ghcr.io/nvidia/aicr", wantOK: false},
		{name: "empty name", identifier: "pkg:oci/", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseOCIPURLName(tt.identifier)
			if ok != tt.wantOK {
				t.Fatalf("parseOCIPURLName() ok = %t, want %t", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("parseOCIPURLName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNormalizeProductNameKeepsReleaseImagesDistinct is the safety check on
// the `aicr-` folding rule: collapsing two released images onto one normalized
// name would publish one image's triage on the other.
func TestNormalizeProductNameKeepsReleaseImagesDistinct(t *testing.T) {
	t.Parallel()
	released := []string{"aicr", "aicrd", "aiperf-bench", "conformance", "deployment", "performance", "aicr-gate"}
	seen := map[string]string{}
	for _, image := range released {
		normalized := normalizeProductName(image)
		if other, clash := seen[normalized]; clash {
			t.Errorf("%s and %s both normalize to %q", other, image, normalized)
		}
		seen[normalized] = image
	}
}

// TestBindCommittedDocument runs the real .openvex.json through the generator.
// It is the check that the committed file and the tool have not drifted apart:
// a product-identifier convention change lands here as a zero-statement
// projection for aiperf-bench.
func TestBindCommittedDocument(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("../../.openvex.json")
	if err != nil {
		t.Fatalf("read committed OpenVEX document: %v", err)
	}
	result, err := Bind(source, Options{Image: benchImage, Digest: amd64Digest})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if result.Kept == 0 {
		t.Fatal("no committed statement bound to aiperf-bench; product identifiers have drifted from the tool")
	}
	for i, entry := range statementsOf(t, decode(t, result.Document)) {
		products := entry.(map[string]any)["products"].([]any)
		if len(products) != 1 {
			t.Fatalf("statement %d has %d products, want exactly the bound one", i, len(products))
		}
		want := "pkg:oci/aiperf-bench@" + amd64Digest
		if got := products[0].(map[string]any)["@id"]; got != want {
			t.Errorf("statement %d product @id = %v, want %s", i, got, want)
		}
	}
	// Every other released image is currently untriaged, so its projection
	// must be empty rather than inherit aiperf-bench's claims.
	for _, image := range []string{"ghcr.io/nvidia/aicr", "ghcr.io/nvidia/aicrd", "ghcr.io/nvidia/aicr-gate"} {
		other, err := Bind(source, Options{Image: image, Digest: amd64Digest})
		if err != nil {
			t.Fatalf("Bind(%s) error = %v", image, err)
		}
		if other.Kept != 0 {
			t.Errorf("Bind(%s) kept %d statements, want 0", image, other.Kept)
		}
	}
}
