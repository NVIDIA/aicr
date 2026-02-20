// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package component

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/eidos/pkg/errors"
)

// ComputeChecksum computes the SHA256 checksum of the given content.
func ComputeChecksum(content []byte) string {
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:])
}

// MarshalYAML serializes a value to YAML format.
func MarshalYAML(v any) ([]byte, error) {
	// Import yaml package inline to avoid adding it as a top-level dependency
	// for packages that don't need it
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(v); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal YAML", err)
	}
	return buf.Bytes(), nil
}

// ValuesHeader contains metadata for values.yaml file headers.
type ValuesHeader struct {
	ComponentName  string
	BundlerVersion string
	RecipeVersion  string
}

// MarshalYAMLWithHeader serializes a value to YAML format with a metadata header.
func MarshalYAMLWithHeader(v any, header ValuesHeader) ([]byte, error) {
	var buf bytes.Buffer

	// Write header comments
	fmt.Fprintf(&buf, "# %s Helm Values\n", header.ComponentName)
	buf.WriteString("# Generated from Cloud Native Stack Recipe\n")
	fmt.Fprintf(&buf, "# Bundler Version: %s\n", header.BundlerVersion)
	fmt.Fprintf(&buf, "# Recipe Version: %s\n", header.RecipeVersion)
	buf.WriteString("\n")

	// Serialize the values
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(v); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal YAML", err)
	}
	return buf.Bytes(), nil
}

// GetConfigValue gets a value from config map with a default fallback.
func GetConfigValue(config map[string]string, key, defaultValue string) string {
	if val, ok := config[key]; ok && val != "" {
		return val
	}

	slog.Debug("config value not found, using default", "key", key, "default", defaultValue)

	return defaultValue
}

// ExtractCustomLabels extracts custom labels from config map with "label_" prefix.
func ExtractCustomLabels(config map[string]string) map[string]string {
	labels := make(map[string]string)
	for k, v := range config {
		if len(k) > 6 && k[:6] == "label_" {
			labels[k[6:]] = v
		}
	}
	return labels
}

// ExtractCustomAnnotations extracts custom annotations from config map with "annotation_" prefix.
func ExtractCustomAnnotations(config map[string]string) map[string]string {
	annotations := make(map[string]string)
	for k, v := range config {
		if len(k) > 11 && k[:11] == "annotation_" {
			annotations[k[11:]] = v
		}
	}
	return annotations
}

// Common string constants for boolean values in Helm templates.
const (
	StrTrue  = "true"
	StrFalse = "false"
)

// BoolToString converts a boolean to "true" or "false" string.
// Use this for Helm values that require string booleans.
func BoolToString(b bool) string {
	if b {
		return StrTrue
	}
	return StrFalse
}

// ParseBoolString parses a string boolean value.
// Returns true if the value is "true" or "1", false otherwise.
func ParseBoolString(s string) bool {
	return s == StrTrue || s == "1"
}
