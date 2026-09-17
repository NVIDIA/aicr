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
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// CanonicalizeRecipeYAML sorts recipe YAML mapping keys recursively and
// strips comments. Any edit to input, including to metadata.version (the
// CLI version that generated the recipe), changes the output. A digest
// computed from it is therefore sensitive to which aicr binary produced
// the recipe, not just to recipe content. New evidence should use
// CanonicalizeRecipeYAMLV3. This form is retained only so already-signed
// V1/V2 evidence keeps verifying under the algorithm it was signed with.
func CanonicalizeRecipeYAML(input []byte) ([]byte, error) {
	return canonicalizeRecipeYAML(input, false)
}

// CanonicalizeRecipeYAMLV3 applies CanonicalizeRecipeYAML, then strips the
// top-level metadata.version field (the CLI version that generated the
// recipe, not recipe content) so two binaries built differently for the
// same commit and the same recipe produce identical output.
func CanonicalizeRecipeYAMLV3(input []byte) ([]byte, error) {
	return canonicalizeRecipeYAML(input, true)
}

func canonicalizeRecipeYAML(input []byte, stripVersion bool) ([]byte, error) {
	if len(input) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "cannot canonicalize empty recipe")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(input, &doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse recipe YAML", err)
	}

	canonicalize(&doc)
	if stripVersion {
		stripMetadataVersion(&doc)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal canonical recipe YAML", err)
	}

	return out, nil
}

// SubjectDigest returns the lowercase hex sha256 of the
// CanonicalizeRecipeYAML bytes, the V1/V2 subject digest for a recipe. New
// evidence should use SubjectDigestV3. This form exists only so
// already-signed V1/V2 evidence keeps verifying under the algorithm it was
// signed with.
func SubjectDigest(recipeYAML []byte) (string, error) {
	canon, err := CanonicalizeRecipeYAML(recipeYAML)
	if err != nil {
		return "", err
	}
	return DigestOfCanonical(canon), nil
}

// SubjectDigestV3 returns the lowercase hex sha256 of the
// CanonicalizeRecipeYAMLV3 bytes (metadata.version excluded). This is the
// digest new evidence (PredicateTypeV3) is built and verified against.
func SubjectDigestV3(recipeYAML []byte) (string, error) {
	canon, err := CanonicalizeRecipeYAMLV3(recipeYAML)
	if err != nil {
		return "", err
	}
	return DigestOfCanonical(canon), nil
}

// SubjectDigestForType returns SubjectDigestV3 for PredicateTypeV3, and
// SubjectDigest for every other recorded type. Callers verifying existing
// evidence must pass the type actually recorded on the bundle being
// checked, never an assumed V3, so historic V1/V2 evidence keeps verifying
// under the algorithm it was signed with.
func SubjectDigestForType(recipeYAML []byte, predicateType string) (string, error) {
	if predicateType == PredicateTypeV3 {
		return SubjectDigestV3(recipeYAML)
	}
	return SubjectDigest(recipeYAML)
}

// DigestOfCanonical hashes already-canonicalized recipe bytes — for
// callers that hold the canonical form and want to skip re-canonicalizing.
func DigestOfCanonical(canon []byte) string {
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// canonicalize walks a yaml.Node tree, stripping comments and sorting
// mapping keys recursively. Operates in place.
func canonicalize(n *yaml.Node) {
	if n == nil {
		return
	}
	n.HeadComment = ""
	n.LineComment = ""
	n.FootComment = ""

	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range n.Content {
			canonicalize(child)
		}
	case yaml.MappingNode:
		// Mapping nodes carry [k1, v1, k2, v2, ...] in Content. Pair
		// them up, recurse into each, then sort by key value.
		pairs := make([]mapEntry, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			canonicalize(n.Content[i])
			canonicalize(n.Content[i+1])
			pairs = append(pairs, mapEntry{
				key:   n.Content[i],
				value: n.Content[i+1],
			})
		}
		sort.Slice(pairs, func(i, j int) bool {
			return pairs[i].key.Value < pairs[j].key.Value
		})
		n.Content = n.Content[:0]
		for _, p := range pairs {
			n.Content = append(n.Content, p.key, p.value)
		}
	case yaml.ScalarNode, yaml.AliasNode:
	}
}

type mapEntry struct {
	key   *yaml.Node
	value *yaml.Node
}

// stripMetadataVersion removes the top-level metadata.version entry from
// doc, in place. doc must already be canonicalized, so its root is a
// mapping node. If removing version empties the metadata mapping, the
// metadata key is removed too, so a recipe whose only metadata field was
// version canonicalizes identically to one with no metadata key at all. A
// document with no metadata mapping, no version key within it, or a
// metadata mapping that stays non-empty after removal, is otherwise left
// unchanged.
func stripMetadataVersion(doc *yaml.Node) {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		metadata := root.Content[i+1]
		if root.Content[i].Value != "metadata" || metadata.Kind != yaml.MappingNode {
			continue
		}
		filtered := make([]*yaml.Node, 0, len(metadata.Content))
		removedVersion := false
		for j := 0; j+1 < len(metadata.Content); j += 2 {
			if metadata.Content[j].Value == "version" {
				removedVersion = true
				continue
			}
			filtered = append(filtered, metadata.Content[j], metadata.Content[j+1])
		}
		if removedVersion && len(filtered) == 0 {
			root.Content = append(root.Content[:i], root.Content[i+2:]...)
		} else {
			metadata.Content = filtered
		}
		return
	}
}
