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
			return nil, fmt.Errorf("decode yaml: %w", err)
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
			if k.Value == "image" && v.Kind == yaml.ScalarNode {
				img := strings.TrimSpace(v.Value)
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
	case yaml.ScalarNode, yaml.AliasNode:
		// Leaf nodes carry no nested image references.
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
// Format: pkg:oci/<name>@<version>?repository_url=<registry>/<namespace>
// where <name> is the last path segment, <namespace> is the prefix, and
// <version> is the digest if available else the tag.
func (r ImageRef) PURL() string {
	name := r.Repository
	namespace := ""
	if i := strings.LastIndex(r.Repository, "/"); i >= 0 {
		namespace = r.Repository[:i]
		name = r.Repository[i+1:]
	}
	version := r.Digest
	if version == "" {
		version = r.Tag
	}

	repoURL := r.Registry
	if namespace != "" {
		repoURL = repoURL + "/" + namespace
	}

	if version != "" {
		return fmt.Sprintf("pkg:oci/%s@%s?repository_url=%s", name, version, repoURL)
	}
	return fmt.Sprintf("pkg:oci/%s?repository_url=%s", name, repoURL)
}
