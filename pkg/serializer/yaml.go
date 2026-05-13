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

package serializer

import (
	"bytes"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// MarshalYAMLDeterministic marshals v to YAML with mapping keys sorted
// recursively so the output is byte-stable across runs. Callers feeding
// the output into a hash, signature, OCI manifest digest, or diff baseline
// must use this helper rather than yaml.Marshal, since Go map iteration
// is randomized per process.
func MarshalYAMLDeterministic(v any) ([]byte, error) {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal YAML", err)
	}
	return sortYAMLBytes(raw)
}

// EncodeYAMLDeterministic encodes v to the given buffer with mapping keys
// sorted recursively and the standard 2-space indent.
func EncodeYAMLDeterministic(buf *bytes.Buffer, v any) error {
	sorted, err := MarshalYAMLDeterministic(v)
	if err != nil {
		return err
	}
	_, _ = buf.Write(sorted)
	return nil
}

// sortYAMLBytes parses YAML bytes, sorts every mapping node's keys, and
// re-marshals. Round-tripping preserves scalars, sequences, anchors, and
// comments; only key order changes.
func sortYAMLBytes(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse YAML for sort", err)
	}
	sortYAMLNode(&node)
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		_ = enc.Close()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to re-marshal sorted YAML", err)
	}
	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to close YAML encoder", err)
	}
	return out.Bytes(), nil
}

// sortYAMLNode recursively sorts the key/value pairs of every MappingNode.
// MappingNode.Content stores keys and values in alternating slots
// (idx 0 key, idx 1 value, idx 2 key, idx 3 value, ...).
func sortYAMLNode(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.DocumentNode {
		for _, c := range n.Content {
			sortYAMLNode(c)
		}
		return
	}
	if n.Kind == yaml.MappingNode {
		pairs := make([]yamlPair, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			pairs = append(pairs, yamlPair{key: n.Content[i], value: n.Content[i+1]})
		}
		sort.SliceStable(pairs, func(i, j int) bool {
			return pairs[i].key.Value < pairs[j].key.Value
		})
		n.Content = n.Content[:0]
		for _, p := range pairs {
			sortYAMLNode(p.value)
			n.Content = append(n.Content, p.key, p.value)
		}
		return
	}
	if n.Kind == yaml.SequenceNode {
		for _, c := range n.Content {
			sortYAMLNode(c)
		}
	}
}

type yamlPair struct {
	key   *yaml.Node
	value *yaml.Node
}
