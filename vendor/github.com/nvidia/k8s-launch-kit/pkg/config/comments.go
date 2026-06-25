// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"

	yaml3 "gopkg.in/yaml.v3"
)

// MarshalConfigWithComments marshals cfg to YAML and transplants the head, line,
// and foot comments from srcYAML (the annotated source config the user started
// from) onto matching keys, so the saved file keeps its field documentation for
// reference. Comments are copied by walking both YAML trees and matching on key
// names recursively — there is no hardcoded copy of the comment text, so the
// output stays in sync with whatever the source config documents.
//
// The clusterConfig section is intentionally skipped: discovery regenerates it,
// so the source's example comments would be misleading. When banner is
// non-empty it is emitted as the document's head comment. If srcYAML is empty or
// unparseable, cfg is marshaled without transplanted comments (banner still
// applied). Output uses 2-space indentation to match l8k's config style.
func MarshalConfigWithComments(cfg *LaunchKitConfig, srcYAML []byte, banner string) ([]byte, error) {
	// Render cfg to a fresh node tree via a marshal/unmarshal round-trip.
	raw, err := yaml3.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var dst yaml3.Node
	if err := yaml3.Unmarshal(raw, &dst); err != nil {
		return nil, err
	}

	var src yaml3.Node
	if len(bytes.TrimSpace(srcYAML)) > 0 {
		// Best-effort: a malformed source just means no comments to transplant.
		if err := yaml3.Unmarshal(srcYAML, &src); err == nil {
			transplantComments(&src, &dst)
		}
	}

	if banner != "" && dst.Kind == yaml3.DocumentNode {
		dst.HeadComment = banner
	}

	var out bytes.Buffer
	enc := yaml3.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&dst); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// transplantComments recursively copies comments from src onto dst for nodes
// that correspond (matching mapping keys). The top-level "clusterConfig" key is
// skipped since discovery regenerates that section.
func transplantComments(src, dst *yaml3.Node) {
	if src == nil || dst == nil {
		return
	}

	switch {
	case src.Kind == yaml3.DocumentNode && dst.Kind == yaml3.DocumentNode:
		copyComments(src, dst)
		if len(src.Content) > 0 && len(dst.Content) > 0 {
			transplantComments(src.Content[0], dst.Content[0])
		}

	case src.Kind == yaml3.MappingNode && dst.Kind == yaml3.MappingNode:
		// Index src key nodes and their value nodes by key name.
		srcKeyNode := map[string]*yaml3.Node{}
		srcValNode := map[string]*yaml3.Node{}
		for i := 0; i+1 < len(src.Content); i += 2 {
			srcKeyNode[src.Content[i].Value] = src.Content[i]
			srcValNode[src.Content[i].Value] = src.Content[i+1]
		}
		for i := 0; i+1 < len(dst.Content); i += 2 {
			dk := dst.Content[i]
			dv := dst.Content[i+1]
			if dk.Value == "clusterConfig" {
				continue
			}
			sk, ok := srcKeyNode[dk.Value]
			if !ok {
				continue
			}
			copyComments(sk, dk)
			transplantComments(srcValNode[dk.Value], dv)
		}

	default:
		// Scalars (and sequences we don't recurse into): copy the node's own
		// comments, e.g. an inline `value # note`. Only when the shapes line up
		// — a value-kind mismatch (e.g. source section is a mapping but cfg
		// marshaled it as a scalar) means there are no value-level comments
		// worth moving. Section doc comments live on the key nodes and are
		// already copied in the mapping branch, so nothing is lost.
		if src.Kind == dst.Kind {
			copyComments(src, dst)
		}
	}
}

// copyComments copies head/line/foot comments from src to dst, never
// overwriting a comment dst already carries.
func copyComments(src, dst *yaml3.Node) {
	if dst.HeadComment == "" {
		dst.HeadComment = src.HeadComment
	}
	if dst.LineComment == "" {
		dst.LineComment = src.LineComment
	}
	if dst.FootComment == "" {
		dst.FootComment = src.FootComment
	}
}
