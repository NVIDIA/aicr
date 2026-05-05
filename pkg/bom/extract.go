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
		walkForImages(&node, seen)
	}
	out := make([]string, 0, len(seen))
	for img := range seen {
		out = append(out, img)
	}
	sort.Strings(out)
	return out, nil
}

func walkForImages(n *yaml.Node, seen map[string]struct{}) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			// Resolve `image: *anchor` so a scalar reached through an alias
			// is captured. Without this the alias falls through to recursion
			// and the AliasNode → ScalarNode hop drops the value.
			target := v
			if v.Kind == yaml.AliasNode && v.Alias != nil {
				target = v.Alias
			}
			if k.Value == "image" && target.Kind == yaml.ScalarNode {
				img := strings.TrimSpace(target.Value)
				if isLikelyImage(img) {
					seen[img] = struct{}{}
				}
			}
			walkForImages(v, seen)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			walkForImages(c, seen)
		}
	case yaml.AliasNode:
		// Follow the anchor target so an `image:` value reached via *alias
		// is still surveyed. Rare in K8s manifests but cheap to handle.
		walkForImages(n.Alias, seen)
	case yaml.ScalarNode:
		// Scalar leaf — no nested image references.
	}
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
	return true
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
