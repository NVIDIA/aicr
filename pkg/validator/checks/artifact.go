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

package checks

import (
	"encoding/base64"
	"encoding/json"
	"sync"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Artifact represents a captured piece of diagnostic evidence from a conformance check.
// Each artifact has a human-readable label and a data payload (kubectl output,
// metric samples, resource YAML, etc.) that is rendered as a fenced code block
// in evidence markdown.
type Artifact struct {
	// Label is the human-readable title (e.g., "DRA Driver Pods").
	Label string `json:"label"`

	// Data is the captured content (command output, metric text, YAML, etc.).
	Data string `json:"data"`
}

// Encode returns a base64-encoded JSON representation of the artifact,
// suitable for emission via t.Logf("ARTIFACT:%s", encoded).
func (a Artifact) Encode() (string, error) {
	jsonBytes, err := json.Marshal(a)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to marshal artifact", err)
	}
	return base64.StdEncoding.EncodeToString(jsonBytes), nil
}

// DecodeArtifact decodes a base64-encoded JSON artifact string.
func DecodeArtifact(encoded string) (*Artifact, error) {
	jsonBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to decode artifact base64", err)
	}
	var a Artifact
	if err := json.Unmarshal(jsonBytes, &a); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to unmarshal artifact JSON", err)
	}
	return &a, nil
}

// ArtifactCollector is a thread-safe accumulator for artifacts within a single check execution.
// It enforces per-artifact size limits and per-check count limits.
type ArtifactCollector struct {
	mu        sync.Mutex
	artifacts []Artifact
}

// NewArtifactCollector creates a new empty artifact collector.
func NewArtifactCollector() *ArtifactCollector {
	return &ArtifactCollector{}
}

// Record adds a labeled artifact. Data exceeding defaults.ArtifactMaxDataSize is truncated.
// Returns an error if the per-check artifact count limit is reached.
func (c *ArtifactCollector) Record(label, data string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.artifacts) >= defaults.ArtifactMaxPerCheck {
		return errors.New(errors.ErrCodeInvalidRequest, "artifact limit reached")
	}

	if len(data) > defaults.ArtifactMaxDataSize {
		data = data[:defaults.ArtifactMaxDataSize] + "\n... [truncated]"
	}

	c.artifacts = append(c.artifacts, Artifact{Label: label, Data: data})
	return nil
}

// Drain returns the collected artifacts and resets the internal list.
// Returns nil if no artifacts were recorded.
func (c *ArtifactCollector) Drain() []Artifact {
	c.mu.Lock()
	defer c.mu.Unlock()
	arts := c.artifacts
	c.artifacts = nil
	return arts
}
