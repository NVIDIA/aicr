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

package validator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

func TestExtractArtifacts(t *testing.T) {
	// Build a valid encoded artifact for test cases.
	sample := checks.Artifact{Label: "DRA Controller", Data: "Status: Available"}
	encoded, err := sample.Encode()
	if err != nil {
		t.Fatalf("failed to encode test artifact: %v", err)
	}

	tests := []struct {
		name              string
		output            []string
		wantArtifactCount int
		wantArtifactLabel string
		wantReasonCount   int
		wantReasonHas     string // substring that must appear in reason lines
	}{
		{
			name: "bare ARTIFACT line (no source prefix)",
			output: []string{
				"normal log line",
				"ARTIFACT:" + encoded,
				"another log line",
			},
			wantArtifactCount: 1,
			wantArtifactLabel: "DRA Controller",
			wantReasonCount:   2,
		},
		{
			name: "t.Logf source-prefixed ARTIFACT line",
			output: []string{
				"=== RUN   TestDRASupport",
				fmt.Sprintf("    runner.go:102: ARTIFACT:%s", encoded),
				"--- PASS: TestDRASupport (1.23s)",
			},
			wantArtifactCount: 1,
			wantArtifactLabel: "DRA Controller",
			wantReasonCount:   2,
		},
		{
			name: "deep source prefix with tab",
			output: []string{
				fmt.Sprintf("    conformance_test.go:45: ARTIFACT:%s", encoded),
			},
			wantArtifactCount: 1,
			wantArtifactLabel: "DRA Controller",
			wantReasonCount:   0,
		},
		{
			name: "no artifacts",
			output: []string{
				"just normal test output",
				"nothing special here",
			},
			wantArtifactCount: 0,
			wantReasonCount:   2,
		},
		{
			name: "malformed artifact preserved in reason",
			output: []string{
				"    runner.go:102: ARTIFACT:not-valid-base64!!!",
				"normal line",
			},
			wantArtifactCount: 0,
			wantReasonCount:   2,
			wantReasonHas:     "ARTIFACT:",
		},
		{
			name:              "empty output",
			output:            []string{},
			wantArtifactCount: 0,
			wantReasonCount:   0,
		},
		{
			name: "multiple artifacts with interleaved output",
			output: []string{
				"log line 1",
				fmt.Sprintf("    runner.go:102: ARTIFACT:%s", encoded),
				"log line 2",
				fmt.Sprintf("    runner.go:102: ARTIFACT:%s", encoded),
				"log line 3",
			},
			wantArtifactCount: 2,
			wantArtifactLabel: "DRA Controller",
			wantReasonCount:   3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifacts, reasonLines := extractArtifacts(tt.output)

			if len(artifacts) != tt.wantArtifactCount {
				t.Errorf("artifact count: got %d, want %d", len(artifacts), tt.wantArtifactCount)
			}

			if tt.wantArtifactLabel != "" && len(artifacts) > 0 {
				if artifacts[0].Label != tt.wantArtifactLabel {
					t.Errorf("artifact label: got %q, want %q", artifacts[0].Label, tt.wantArtifactLabel)
				}
			}

			if len(reasonLines) != tt.wantReasonCount {
				t.Errorf("reason line count: got %d, want %d\nlines: %v",
					len(reasonLines), tt.wantReasonCount, reasonLines)
			}

			if tt.wantReasonHas != "" {
				found := false
				for _, line := range reasonLines {
					if strings.Contains(line, tt.wantReasonHas) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("reason lines should contain %q, got: %v", tt.wantReasonHas, reasonLines)
				}
			}
		})
	}
}
