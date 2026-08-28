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
const (
	helmTemplatePlaceholder = "_aicr_helm_template_"
	imageRepositoryKey      = "repository"
	imageTagKey             = "tag"
	imageDigestKey          = "digest"
)

var helmTemplateRE = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// stripHelmTemplates pre-processes a YAML document so the parser doesn't
// choke on Go-template directives. Two passes:
//  1. Blank out any line whose non-whitespace content consists entirely of
//     one or more Helm directives (e.g., `  {{- if foo }}`, `  {{- end }}`,
//     `  {{- toYaml . | nindent 4 }}`). These are control-flow scaffolding
//     that produces no YAML node when rendered. The line is kept (empty)
//     rather than deleted so yaml.Node positions — and therefore the
//     line/column reported by descriptor errors — still index the original
//     file.
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
			out = append(out, nil)
			continue
		}
		out = append(out, helmTemplateRE.ReplaceAll(l, []byte(helmTemplatePlaceholder)))
	}
	return bytes.Join(out, []byte("\n"))
}

// ExtractImagesFromYAML walks every YAML document in data and returns the
// sorted, de-duplicated set of container image references. It recognizes both
// scalar `image:` values and operator-style `image: {name, repository, tag}`
// mappings. Empty or null scalar `image:` values and values still containing an
// unrendered Go template directive are skipped. A recognized structured
// descriptor returns an invalid-request error when a present name or
// repository field is null, empty, or non-scalar, when a tag or digest field
// is non-scalar, or when a registry member is present (which cannot be folded
// into the reference without losing information). A null or empty tag or digest
// follows the Helm appVersion/unpinned idiom and is treated as absent. A
// non-empty digest is validated as sha256:<64 lowercase hex chars> and folded
// in as an @<digest> suffix.
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
		var (
			imgScalar, repoScalar, verScalar, shaScalar string
			imgMappings                                 []*yaml.Node
		)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			target := v
			if v.Kind == yaml.AliasNode && v.Alias != nil {
				target = v.Alias
			}
			if isStructuredImageKey(k.Value) && target.Kind == yaml.MappingNode {
				imgMappings = append(imgMappings, target)
				continue
			}
			if target.Kind != yaml.ScalarNode {
				continue
			}
			switch k.Value {
			case "image":
				imgScalar = strings.TrimSpace(target.Value)
			case imageRepositoryKey:
				repoScalar = strings.TrimSpace(target.Value)
			case "version":
				verScalar = strings.TrimSpace(target.Value)
			case "containerSHA":
				shaScalar = strings.TrimSpace(target.Value)
			}
		}
		for _, imgMapping := range imgMappings {
			mapped, err := imageReferenceFromMapping(imgMapping)
			if err != nil {
				return err
			}
			if isLikelyImage(mapped) {
				seen[mapped] = struct{}{}
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

// isStructuredImageKey allowlists mapping-valued image descriptor keys known
// to be consumed as container images. Matching every *Image key would risk
// inventorying chart metadata or templates that never become workloads.
func isStructuredImageKey(key string) bool {
	return key == "image" || key == "scalingPodImage"
}

// imageReferenceFromMapping recognizes the image descriptor shape used by
// operator-managed workloads such as KAI Scheduler:
//
//	image:
//	  name: binder
//	  repository: ghcr.io/kai-scheduler/kai-scheduler
//	  tag: v0.14.1
//
// It also handles the digest-pinned form used by Helm charts that separate
// repository, tag, and digest into sibling fields (e.g., Bitnami-style):
//
//	image:
//	  repository: docker.io/library/postgres
//	  tag: "17.4"
//	  digest: sha256:304ab813518754228f9f792f79d6da36359b82d8ecf418096c636725f8c930ad
//
// Some charts omit name because repository already carries the full image
// path. In that form, repository becomes the image name before tag is
// appended. A present name or repository must be a non-null, non-empty
// scalar. A null or empty tag is the Helm idiom for "default to the chart
// appVersion" and is treated like an absent tag. A present digest is appended
// as @<digest> so the extracted reference is fully pinned. A present registry
// member is rejected because dropping it would resolve to the wrong registry.
// Other members (pullPolicy, pullSecrets, ...) do not affect reference
// identity and are ignored.
func imageReferenceFromMapping(n *yaml.Node) (string, error) {
	var name, repository, tag, digest string
	var namePresent bool
	var digestNode *yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		switch key.Value {
		case "name", imageRepositoryKey, imageTagKey, imageDigestKey:
		case "registry":
			return "", errors.Wrap(
				errors.ErrCodeInvalidRequest,
				"invalid image descriptor member",
				&invalidStructuredImageDescriptorError{
					field:  key.Value,
					line:   key.Line,
					column: key.Column,
					reason: "is not combined into the extracted reference; fold it into repository",
				},
			)
		default:
			continue
		}

		scalar, ok := nonNullImageMappingScalar(value)
		if !ok {
			if (key.Value == imageTagKey || key.Value == imageDigestKey) && isNullOrEmptyScalar(value) {
				// tag: "" / tag: null — the Helm "use appVersion" idiom.
				// digest: "" / digest: null — unpinned default in charts
				// that optionally carry a digest pin.
				// Treat both as absent rather than failing the whole survey.
				continue
			}
			return "", errors.Wrap(
				errors.ErrCodeInvalidRequest,
				"invalid image descriptor member",
				&invalidStructuredImageDescriptorError{
					field:  key.Value,
					line:   value.Line,
					column: value.Column,
					reason: "must be a non-null, non-empty scalar",
				},
			)
		}
		switch key.Value {
		case "name":
			namePresent = true
			name = scalar
		case imageRepositoryKey:
			repository = scalar
		case imageTagKey:
			tag = scalar
		case imageDigestKey:
			digest = scalar
			digestNode = value
		}
	}
	if !namePresent {
		// combineCRDTriplet trims a trailing slash from repository only
		// when it is prepended to a separate name, so trim here before
		// repository itself becomes the name.
		name = strings.TrimRight(repository, "/")
		repository = ""
	}
	if name == "" {
		return "", nil
	}
	ref := combineCRDTriplet(name, repository, tag)
	if digest == "" || strings.Contains(ref, "@") {
		return ref, nil
	}
	if !containerSHARE.MatchString(digest) {
		return "", errors.Wrap(
			errors.ErrCodeInvalidRequest,
			"invalid image descriptor member",
			&invalidStructuredImageDescriptorError{
				field:  imageDigestKey,
				line:   digestNode.Line,
				column: digestNode.Column,
				reason: fmt.Sprintf("must match sha256:<64 lowercase hex chars>, got %q", digest),
			},
		)
	}
	return ref + "@" + digest, nil
}

func nonNullImageMappingScalar(n *yaml.Node) (string, bool) {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	if n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return "", false
	}
	value := strings.TrimSpace(n.Value)
	return value, value != ""
}

// isNullOrEmptyScalar reports whether n is a null or empty-string scalar —
// the shapes a Helm chart's `tag: ""` appVersion default renders to. A
// non-scalar node is not covered; that stays a descriptor error.
func isNullOrEmptyScalar(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n.Kind == yaml.ScalarNode &&
		(n.Tag == "!!null" || strings.TrimSpace(n.Value) == "")
}

type invalidStructuredImageDescriptorError struct {
	field  string
	line   int
	column int
	reason string
}

func (e *invalidStructuredImageDescriptorError) Error() string {
	return fmt.Sprintf(
		"field %q at line %d, column %d %s",
		e.field,
		e.line,
		e.column,
		e.reason,
	)
}

// IsInvalidStructuredImageDescriptor reports whether err was caused by a
// recognized mapping-valued image descriptor whose name, repository, or tag
// field was present but null, empty, or non-scalar.
func IsInvalidStructuredImageDescriptor(err error) bool {
	var target *invalidStructuredImageDescriptorError
	return stderrors.As(err, &target)
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
