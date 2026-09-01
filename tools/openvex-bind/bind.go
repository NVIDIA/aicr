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
	stderrors "errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ociPURLPrefix is the purl scheme+type every product identifier in
// .openvex.json uses. Product matching and rewriting both key off it.
const ociPURLPrefix = "pkg:oci/"

// aicrPURLPrefix is the repository-name prefix carried by half of the product
// aliases in .openvex.json. Statements list both `pkg:oci/aiperf-bench` and
// `pkg:oci/aicr-aiperf-bench` because scanners disagree on which name they
// report for the same image, so matching normalizes it away on both sides.
const aicrPURLPrefix = "aicr-"

// digestPattern is the only accepted form for a platform manifest digest. The
// binding is the whole point of this tool, so a malformed digest must stop the
// release rather than produce a VEX bound to nothing.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Options names the platform manifest a projection binds to.
type Options struct {
	// Image is the image name without tag or digest, e.g.
	// ghcr.io/nvidia/aicr-validators/aiperf-bench. Only its basename
	// participates in product identity; the registry and namespace do not,
	// because purl `pkg:oci` names are repository-independent.
	Image string
	// Digest is the linux/<arch> child manifest digest, never the index
	// digest: an SBOM and its VEX describe one root filesystem.
	Digest string
}

// Result is a rendered projection plus the counts a caller logs. Kept is the
// number of source statements that named this image; Dropped is the number
// that named some other one.
type Result struct {
	Document []byte
	Kept     int
	Dropped  int
}

// Bind projects the committed OpenVEX document onto one platform manifest.
//
// Two things happen and nothing else does. Statements that do not name this
// image are dropped, because binding them to this digest would publish a
// signed claim about a product they were never triaged against. Statements
// that do name it keep their status, justification, impact statement and
// subcomponents byte-for-byte, with `products` replaced by the single
// digest-qualified identifier `pkg:oci/<basename>@<digest>` that makes the
// claim verifiable against the manifest it ships with.
//
// The output is a pure function of (source, Image, Digest): no wall clock, no
// UUID, and encoding/json orders object keys, so re-running on the same inputs
// reproduces the same bytes. The source document's own `timestamp` and
// `version` pass through, which keeps provenance pointing back at the reviewed
// file rather than at the moment the release happened to run.
func Bind(source []byte, o Options) (*Result, error) {
	if !digestPattern.MatchString(o.Digest) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("digest %q must be in the form sha256:<64 lowercase hex>", o.Digest))
	}
	name, err := imageBasename(o.Image)
	if err != nil {
		return nil, err
	}

	doc, err := decodeDocument(source)
	if err != nil {
		return nil, err
	}
	sourceID, ok := nonEmptyString(doc["@id"])
	if !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "source document must set @id to a non-empty string")
	}
	statements, ok := doc["statements"].([]any)
	if !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "source document must set statements to an array")
	}

	purl := ociPURLPrefix + name + "@" + o.Digest
	kept := make([]any, 0, len(statements))
	for i, entry := range statements {
		statement, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("statement %d must be a JSON object", i))
		}
		products, ok := statement["products"].([]any)
		if !ok || len(products) == 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("statement %d must list at least one product", i))
		}
		bound, matched, bindErr := bindProducts(i, products, name, purl)
		if bindErr != nil {
			return nil, bindErr
		}
		if !matched {
			continue
		}
		// Copy before mutating so the decoded source tree is left intact and a
		// later failure cannot leave a half-rewritten document behind.
		projected := make(map[string]any, len(statement))
		for key, value := range statement {
			projected[key] = value
		}
		projected["products"] = []any{bound}
		kept = append(kept, projected)
	}

	// A projection is a distinct document from the file it derives from, so it
	// needs a distinct @id. Deriving it from the source @id plus the bound
	// purl keeps that identity stable across runs and self-describing about
	// which image and platform it covers.
	doc["@id"] = sourceID + "#" + name + "@" + o.Digest
	doc["statements"] = kept

	rendered, err := encodeDocument(doc)
	if err != nil {
		return nil, err
	}
	return &Result{Document: rendered, Kept: len(kept), Dropped: len(statements) - len(kept)}, nil
}

// bindProducts returns the single digest-qualified product replacing a
// statement's product list. The bool reports whether any listed product named
// this image; false means the statement belongs to a different one and is
// dropped. Subcomponents survive the collapse: they are gathered from every
// matching product, in source order, deduplicated by encoded form.
func bindProducts(index int, products []any, name, purl string) (map[string]any, bool, error) {
	matched := false
	subcomponents := make([]any, 0)
	seen := map[string]struct{}{}
	for _, entry := range products {
		product, ok := entry.(map[string]any)
		if !ok {
			return nil, false, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("statement %d has a product that is not a JSON object", index))
		}
		if !productNames(product, name) {
			continue
		}
		matched = true
		listed, listErr := subcomponentList(product)
		if listErr != nil {
			return nil, false, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("statement %d has a malformed product", index), listErr)
		}
		for _, sub := range listed {
			key, err := json.Marshal(sub)
			if err != nil {
				return nil, false, errors.Wrap(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("statement %d has an unencodable subcomponent", index), err)
			}
			if _, duplicate := seen[string(key)]; duplicate {
				continue
			}
			seen[string(key)] = struct{}{}
			subcomponents = append(subcomponents, sub)
		}
	}
	if !matched {
		return nil, false, nil
	}
	bound := map[string]any{
		"@id":         purl,
		"identifiers": map[string]any{"purl": purl},
	}
	if len(subcomponents) > 0 {
		bound["subcomponents"] = subcomponents
	}
	return bound, true, nil
}

// subcomponentList returns a product's subcomponents, or nil when it has none.
// A present-but-malformed value is fatal rather than ignored: subcomponents
// narrow a statement to specific packages, so silently dropping one would
// publish a signed claim broader than the curated source made, and passing one
// through unchecked would sign a component that is not a component.
//
// OpenVEX v0.2.0 types every entry as a Component object, so a scalar member is
// rejected too: it marshals without complaint and would otherwise be copied
// verbatim into the signed projection.
func subcomponentList(product map[string]any) ([]any, error) {
	value, present := product["subcomponents"]
	if !present || value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("subcomponents must be an array, got %T", value))
	}
	for i, entry := range list {
		if _, ok := entry.(map[string]any); !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("subcomponents[%d] must be a JSON object, got %T", i, entry))
		}
	}
	return list, nil
}

// productNames reports whether a product identifies the image named by
// basename. Both `@id` and `identifiers.purl` are consulted because the
// OpenVEX spec makes either sufficient to identify a component.
func productNames(product map[string]any, basename string) bool {
	candidates := []any{product["@id"]}
	if identifiers, ok := product["identifiers"].(map[string]any); ok {
		candidates = append(candidates, identifiers["purl"])
	}
	want := normalizeProductName(basename)
	for _, candidate := range candidates {
		value, ok := nonEmptyString(candidate)
		if !ok {
			continue
		}
		if parsed, ok := parseOCIPURLName(value); ok && normalizeProductName(parsed) == want {
			return true
		}
	}
	return false
}

// parseOCIPURLName pulls the package name out of a `pkg:oci/...` identifier,
// discarding any version (`@sha256:...`) or qualifier (`?repository_url=...`)
// the source happens to carry. A non-OCI identifier yields false.
func parseOCIPURLName(identifier string) (string, bool) {
	if !strings.HasPrefix(identifier, ociPURLPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(identifier, ociPURLPrefix)
	for _, separator := range []string{"@", "?", "#"} {
		if at := strings.Index(name, separator); at >= 0 {
			name = name[:at]
		}
	}
	if name == "" {
		return "", false
	}
	return name, true
}

// normalizeProductName folds the `aicr-` repository-name prefix so the two
// aliases for one image compare equal. It is applied to both sides, so the
// released image set stays unambiguous: aicr, aicrd, aiperf-bench,
// conformance, deployment, performance, and aicr-gate (folding to `gate`) all
// normalize to distinct names.
func normalizeProductName(name string) string {
	return strings.TrimPrefix(name, aicrPURLPrefix)
}

// imageBasename strips the registry, namespace, and any tag or digest from an
// image reference, leaving the repository name purl `pkg:oci` uses.
func imageBasename(image string) (string, error) {
	if image == "" || strings.TrimSpace(image) != image {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"image must be a non-empty reference with no surrounding whitespace")
	}
	trimmed := image
	if at := strings.Index(trimmed, "@"); at >= 0 {
		trimmed = trimmed[:at]
	}
	// Deliberately not path.Base: it strips trailing slashes, so
	// "ghcr.io/nvidia/" would silently yield the namespace "nvidia" as the
	// repository name. The last slash-separated segment must be present.
	base := trimmed
	if slash := strings.LastIndex(base, "/"); slash >= 0 {
		base = base[slash+1:]
	}
	if colon := strings.LastIndex(base, ":"); colon >= 0 {
		base = base[:colon]
	}
	if base == "" || base == "." {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("image %q has no repository name", image))
	}
	return base, nil
}

// decodeDocument parses the source with UseNumber so integer fields such as
// `version` re-encode as the literal they arrived as, not as a float.
func decodeDocument(source []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "source is not a JSON object", err)
	}
	if doc == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "source is not a JSON object")
	}
	// json.Decoder stops at the end of the first value, so a document followed
	// by a second one would be silently truncated to the first. Input that
	// ambiguous must not become signed evidence.
	var trailing any
	if err := decoder.Decode(&trailing); !stderrors.Is(err, io.EOF) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"source must hold exactly one JSON object")
	}
	return doc, nil
}

// encodeDocument renders the projection. encoding/json sorts map keys, so the
// byte output is stable for a given input even though the source key order is
// not preserved.
func encodeDocument(doc map[string]any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to encode the bound document", err)
	}
	return buffer.Bytes(), nil
}

// nonEmptyString reports whether value is a string with non-whitespace content.
func nonEmptyString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}
