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

// CanonicalizeRecipeYAML applies the V1 canonicalizer to a recipe YAML
// document and returns the canonical bytes. The transform is:
//
//  1. Parse YAML into a generic node tree.
//  2. Recursively sort mapping keys.
//  3. Strip comments at every node level.
//  4. Re-marshal with line endings normalized to \n.
//
// The result is deterministic for any input that round-trips through
// yaml.v3 — equal recipes produce equal bytes regardless of key order
// or comment placement. UTF-8 encoding is implicit in yaml.v3 output.
//
// V1 is intentionally simple: any recipe edit (including non-material
// edits like reformatting) changes the canonical bytes and invalidates
// the bundle. A material-slice canonicalizer is reserved for V2.
func CanonicalizeRecipeYAML(input []byte) ([]byte, error) {
	if len(input) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "cannot canonicalize empty recipe")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(input, &doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse recipe YAML", err)
	}

	canonicalize(&doc)

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal canonical recipe YAML", err)
	}

	return out, nil
}

// SubjectDigest returns the V1 subject digest for a recipe: the
// hex-encoded sha256 of the canonical YAML bytes. The returned digest
// is the lowercase 64-character hex string used in the in-toto
// Statement's subject[0].digest.sha256 field.
func SubjectDigest(recipeYAML []byte) (string, error) {
	canon, err := CanonicalizeRecipeYAML(recipeYAML)
	if err != nil {
		return "", err
	}
	return DigestOfCanonical(canon), nil
}

// DigestOfCanonical hashes already-canonicalized recipe bytes. Callers
// that already hold the canonical form (e.g., the Builder, which writes
// recipe.yaml first) use this to avoid re-running the canonicalizer.
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
		// Nothing to recurse into.
	}
}

type mapEntry struct {
	key   *yaml.Node
	value *yaml.Node
}
