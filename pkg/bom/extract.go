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

package bom

import (
	"bytes"
	stderrors "errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// helmTemplatePlaceholder replaces Go-template directives ({{...}}) before
// YAML parsing. Files under recipes/components/*/manifests/ are sometimes
// Helm-template-shaped (the bundler processes them as chart templates), so
// raw YAML parsing would fail on the bare directives.
const helmTemplatePlaceholder = "_aicr_helm_template_"

var helmTemplateRE = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// stripHelmTemplates pre-processes a YAML document so the parser doesn't
// choke on Go-template directives. Two passes:
//  1. Drop any line whose non-whitespace content consists entirely of one or
//     more Helm directives (e.g., `  {{- if foo }}`, `  {{- end }}`,
//     `  {{- toYaml . | nindent 4 }}`). These are control-flow scaffolding
//     that produces no YAML node when rendered.
//  2. On surviving lines, replace inline directives with a placeholder so a
//     value like `key: {{ .Values.x }}` becomes `key: _aicr_helm_template_`
//     instead of breaking YAML parsing. The placeholder is filtered out by
//     isLikelyImage so it never appears as an "image".
func stripHelmTemplates(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	for _, l := range lines {
		stripped := helmTemplateRE.ReplaceAll(l, nil)
		if len(bytes.TrimSpace(stripped)) == 0 && bytes.Contains(l, []byte("{{")) {
			continue
		}
		out = append(out, helmTemplateRE.ReplaceAll(l, []byte(helmTemplatePlaceholder)))
	}
	return bytes.Join(out, []byte("\n"))
}

// ExtractImagesFromYAML walks every YAML document in data and returns the
// sorted, de-duplicated set of `image:` scalar values. It skips empty values,
// `null`, and any value still containing an unrendered Go template directive.
//
// Helm template directives ({{ ... }}) are replaced with a placeholder before
// parsing, so files mixing YAML with Helm templates (those under
// recipes/components/*/manifests/ that are processed as chart templates) can
// still be surveyed for static `image:` values.
func ExtractImagesFromYAML(data []byte) ([]string, error) {
	data = stripHelmTemplates(data)
	seen := map[string]struct{}{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if stderrors.Is(err, io.EOF) {
				break
			}
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "decode yaml", err)
		}
		if err := walkForImages(&node, seen); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(seen))
	for img := range seen {
		out = append(out, img)
	}
	sort.Strings(out)
	return out, nil
}

func walkForImages(n *yaml.Node, seen map[string]struct{}) error {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		// First pass: collect sibling scalars for `image`, `repository`,
		// `version`, and `containerSHA` so we can recognize the CRD-style
		// pattern used by NicClusterPolicy, Skyhook, and similar operators
		// where these fields are siblings (not concatenated into a single
		// `image:` value). Without this, the bare `image: doca-driver` part
		// looks like an untagged image when in fact `repository` and
		// `version` siblings carry the registry and tag. A sibling
		// `containerSHA` (Skyhook Package CRD; ghcr.io/nvidia/nodewright)
		// supplies the OCI digest and is folded in as `@<sha>`.
		var imgScalar, repoScalar, verScalar, shaScalar string
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			target := v
			if v.Kind == yaml.AliasNode && v.Alias != nil {
				target = v.Alias
			}
			if target.Kind != yaml.ScalarNode {
				continue
			}
			switch k.Value {
			case "image":
				imgScalar = strings.TrimSpace(target.Value)
			case "repository":
				repoScalar = strings.TrimSpace(target.Value)
			case "version":
				verScalar = strings.TrimSpace(target.Value)
			case "containerSHA":
				shaScalar = strings.TrimSpace(target.Value)
			}
		}
		if imgScalar != "" {
			combined := combineCRDTriplet(imgScalar, repoScalar, verScalar)
			withSHA, err := appendContainerSHA(combined, shaScalar)
			if err != nil {
				return err
			}
			if isLikelyImage(withSHA) {
				seen[withSHA] = struct{}{}
			}
		}

		// Second pass: recurse into every value to catch image references
		// nested deeper in the document.
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := walkForImages(n.Content[i+1], seen); err != nil {
				return err
			}
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			if err := walkForImages(c, seen); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		// Follow the anchor target so an `image:` value reached via *alias
		// is still surveyed. Rare in K8s manifests but cheap to handle.
		return walkForImages(n.Alias, seen)
	case yaml.ScalarNode:
		// Scalar leaf — no nested image references.
	}
	return nil
}

// combineCRDTriplet builds a fully-qualified image reference from
// sibling `image`, `repository`, and `version` scalars in a CRD-style
// mapping (e.g., NicClusterPolicy, Skyhook Package). Behavior:
//
//   - If `image` already starts with a registry host (its first path
//     segment contains "." or ":" or is "localhost"), it is treated as
//     fully qualified and `repository` is ignored.
//   - Otherwise `repository` is prepended — even when `image` itself
//     contains slashes (e.g., `image: nvidia/mellanox/doca-driver` with
//     `repository: nvcr.io`) — so the registry information is preserved.
//   - `version` is appended as a tag when the result does not already
//     carry one.
//
// Returns the combined ref, or the original `image` value if no
// combination is applicable.
func combineCRDTriplet(image, repository, version string) string {
	out := image
	if repository != "" {
		first, _, hasSlash := strings.Cut(image, "/")
		if !hasSlash || !isRegistryHost(first) {
			out = strings.TrimRight(repository, "/") + "/" + strings.TrimLeft(image, "/")
		}
	}
	hasTag := false
	if i := strings.LastIndex(out, ":"); i >= 0 && !strings.Contains(out[i+1:], "/") {
		hasTag = true
	}
	if version != "" && !hasTag && !strings.Contains(out, "@") {
		out = out + ":" + version
	}
	return out
}

// containerSHARE matches a well-formed sha256 OCI digest payload
// (`sha256:` + 64 lowercase hex chars). The recipes/ digest-pin test
// uses the same shape downstream; validating at extraction time means
// a bogus `containerSHA` fails fast at BOM render rather than silently
// shipping a malformed ref into the SBOM/PURL output.
var containerSHARE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// appendContainerSHA folds a sibling `containerSHA` value onto a
// CRD-style combined image ref as an `@<digest>` suffix. The Skyhook
// Package CRD carries the OCI digest in a separate `containerSHA`
// scalar (e.g., `containerSHA: sha256:<hex>`) rather than splicing it
// into the `image` value, so the extractor has to merge them.
//
// Behavior:
//   - Empty `sha` → returned image unchanged.
//   - Image already carries an `@`-digest → returned unchanged (the
//     in-line digest wins; we do not silently overwrite).
//   - `sha` does not match `^sha256:[a-f0-9]{64}$` → error. This is
//     the fail-loud guard: a malformed digest (typo, truncation, or
//     a user-supplied value override that lands in a Skyhook Package)
//     must not silently propagate into the BOM, PURL, or SBOM output.
//   - Otherwise the digest is appended as `image@sha`, preserving any
//     tag already present (e.g., `repo:0.1.2@sha256:abc…`).
func appendContainerSHA(image, sha string) (string, error) {
	if sha == "" {
		return image, nil
	}
	if strings.Contains(image, "@") {
		return image, nil
	}
	if !containerSHARE.MatchString(sha) {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid containerSHA %q for image %q: expected sha256:<64 lowercase hex chars>", sha, image))
	}
	return image + "@" + sha, nil
}

func isLikelyImage(v string) bool {
	if v == "" || v == "null" || strings.EqualFold(v, "true") || strings.EqualFold(v, "false") {
		return false
	}
	if strings.Contains(v, "{{") || strings.Contains(v, "}}") {
		return false
	}
	if strings.Contains(v, helmTemplatePlaceholder) {
		return false
	}
	if strings.HasPrefix(v, "/") || strings.HasPrefix(v, "./") {
		return false
	}
	// A real container image reference carries at least one of:
	//   - a registry host as the first path segment (contains "." or ":"
	//     or equals "localhost"),
	//   - a ":tag" after the last "/",
	//   - an "@<digest>" suffix.
	// Bare scalars like "vgpu-manager" or "driver" that the extractor
	// sometimes lifts from disabled CRD-style placeholders (chart-default
	// sub-images whose enclosing section sets `enabled: false`) don't
	// represent real deployments and dilute the published BOM. Reject
	// them here rather than chase per-chart enable flags.
	if !hasTagOrDigest(v) && !hasRegistryFirstSegment(v) {
		return false
	}
	return true
}

// hasTagOrDigest reports whether v carries a `:tag` after its last `/`
// or an `@digest` suffix.
func hasTagOrDigest(v string) bool {
	if strings.Contains(v, "@") {
		return true
	}
	lastSlash := strings.LastIndex(v, "/")
	lastColon := strings.LastIndex(v, ":")
	return lastColon > lastSlash
}

// hasRegistryFirstSegment reports whether v's first path segment looks
// like a registry host (contains "." or ":" or equals "localhost").
func hasRegistryFirstSegment(v string) bool {
	first, _, _ := strings.Cut(v, "/")
	return isRegistryHost(first)
}

// ImageRef is a parsed container image reference.
type ImageRef struct {
	Raw        string // original string
	Registry   string // host[:port], e.g., "nvcr.io" or "docker.io"
	Repository string // path after registry, e.g., "nvidia/gpu-operator"
	Tag        string // ":tag" portion if present
	Digest     string // "@sha256:..." portion if present
}

// ParseImageRef splits a container image reference into its parts using the
// standard Docker rules: a leading segment is treated as the registry when it
// contains a "." or ":" or equals "localhost"; otherwise the registry defaults
// to "docker.io".
func ParseImageRef(s string) ImageRef {
	ref := ImageRef{Raw: s}
	rest := s

	if i := strings.Index(rest, "@"); i >= 0 {
		ref.Digest = rest[i+1:]
		rest = rest[:i]
	}

	if first, tail, ok := strings.Cut(rest, "/"); ok && isRegistryHost(first) {
		ref.Registry = first
		rest = tail
	} else {
		ref.Registry = "docker.io"
	}

	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i+1:], "/") {
		ref.Tag = rest[i+1:]
		rest = rest[:i]
	}
	// Docker Hub canonicalization: a single-segment name like "nginx" or
	// "busybox" lives under the implicit "library/" namespace. Normalizing
	// here means `nginx` and `docker.io/library/nginx` produce the same
	// PURL and de-dupe correctly in the BOM.
	if ref.Registry == "docker.io" && !strings.Contains(rest, "/") {
		rest = "library/" + rest
	}
	ref.Repository = rest
	return ref
}

func isRegistryHost(s string) bool {
	if s == "localhost" {
		return true
	}
	return strings.ContainsAny(s, ".:")
}

// PURL returns the Package URL for the image reference using the OCI type.
//
// Per the purl-spec OCI definition
// (https://github.com/package-url/purl-spec/blob/main/types-doc/oci-definition.md),
// the canonical form is:
//
//	pkg:oci/<name>@<digest>?repository_url=<registry>/<namespace>/<name>&tag=<tag>
//
// where <name> is the last path segment of the image repository, the
// repository_url is the FULL artifact path (including the name), and the
// digest is the canonical version. Tags are mutable and live in qualifiers.
//
// When a digest is not available (the common case for our reference BOM
// today, since most chart defaults pin only by tag), this function falls back
// to using the tag in the @<version> position. That deviates from strict
// spec conformance but preserves the version information consumers need.
// As soon as we adopt digest pinning end-to-end, the output becomes
// fully spec-conformant with no callsite changes.
func (r ImageRef) PURL() string {
	name := r.Repository
	namespace := ""
	if i := strings.LastIndex(r.Repository, "/"); i >= 0 {
		namespace = r.Repository[:i]
		name = r.Repository[i+1:]
	}

	repoURL := r.Registry
	if namespace != "" {
		repoURL += "/" + namespace
	}
	repoURL += "/" + name

	var version string
	switch {
	case r.Digest != "":
		version = r.Digest
	case r.Tag != "":
		version = r.Tag
	}

	out := "pkg:oci/" + name
	if version != "" {
		out += "@" + version
	}
	out += "?repository_url=" + repoURL
	if r.Digest != "" && r.Tag != "" {
		out += "&tag=" + r.Tag
	}
	return out
}
