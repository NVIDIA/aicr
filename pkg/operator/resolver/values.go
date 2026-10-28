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

package resolver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"go.yaml.in/yaml/v3"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Values files of a rendered release, in the order AICR's install script
// passes them to Helm (`-f values.yaml -f cluster-values.yaml`).
// chartValuesFile is a chart's default values file, and the first of AICR's
// values files.
const chartValuesFile = "values.yaml"

var valuesFiles = []string{chartValuesFile, "cluster-values.yaml"}

// releaseValues merges a release's values files with Helm's semantics and
// returns the canonical JSON (sorted keys) the plan stores and engines apply.
func releaseValues(files map[string][]byte) ([]byte, error) {
	merged := map[string]any{}
	for _, name := range valuesFiles {
		raw, ok := files[name]
		if !ok {
			continue
		}
		doc, err := parseValues(raw)
		if err != nil {
			return nil, operrors.New(operrors.ErrCodeInvalidRequest, name+": "+err.Error())
		}
		merged = mergeValues(merged, doc)
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInvalidRequest, "values cannot be represented as JSON", err)
	}
	return out, nil
}

// parseValues decodes one YAML values document; an empty file is no values.
func parseValues(raw []byte) (map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("holds more than one YAML document")
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// mergeValues overlays src onto dst as Helm merges several -f files: maps
// merge key by key and any other value, including an explicit null, replaces.
// A null is kept, so Helm still removes that key from the chart's defaults.
func mergeValues(dst, src map[string]any) map[string]any {
	for k, v := range src {
		srcMap, srcIsMap := v.(map[string]any)
		dstMap, dstIsMap := dst[k].(map[string]any)
		if srcIsMap && dstIsMap {
			dst[k] = mergeValues(dstMap, srcMap)
			continue
		}
		dst[k] = v
	}
	return dst
}
