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

package aicrjob

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// maxArtifactBytes bounds any AICR artifact the operator decodes; it matches
// AICR's own limit for bundle-info.yaml and fits a ConfigMap.
const maxArtifactBytes = 1 << 20

// withHeader returns the required fields of an AICR artifact: its header
// (identity and the metadata carrying its AICR version) plus fields.
func withHeader(fields ...string) []string {
	return append([]string{"apiVersion", "kind", "metadata"}, fields...)
}

// requireFields rejects a document that is not a JSON object or lacks one of
// the required top-level fields.
func requireFields(data []byte, kind string, required []string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return unsupported("%s is not a JSON object: %v", kind, err)
	}
	for _, f := range required {
		if raw, ok := fields[f]; !ok || string(raw) == "null" {
			return unsupported("%s is missing required field %q", kind, f)
		}
	}
	return nil
}

// yamlToJSON converts a single bounded YAML document to JSON, so YAML
// artifacts go through the same decoding rules as JSON ones. A second
// document, a duplicate key or a non-string key is rejected rather than
// silently dropped.
func yamlToJSON(data []byte, kind string) ([]byte, error) {
	if len(data) > maxArtifactBytes {
		return nil, unsupported("%s is %d bytes, over the %d-byte limit", kind, len(data), maxArtifactBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, unsupported("%s is not valid YAML: %v", kind, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, unsupported("%s holds more than one YAML document", kind)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, unsupported("%s cannot be represented as JSON: %v", kind, err)
	}
	return out, nil
}

func unsupported(format string, args ...any) error {
	return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf(format, args...)).
		WithReason(ReasonArtifactUnsupported)
}
