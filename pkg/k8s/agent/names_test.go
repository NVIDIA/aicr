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

package agent

import (
	"strings"
	"testing"
)

func TestNameWithRunID(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55" // 32 chars

	tests := []struct {
		name   string
		prefix string
		runID  string
		want   string
	}{
		{"short prefix", "aicr", runID, "aicr-" + runID},
		{"exactly at budget", strings.Repeat("a", 30), runID, strings.Repeat("a", 30) + "-" + runID},
		{"over budget truncates", strings.Repeat("b", 40), runID, strings.Repeat("b", 30) + "-" + runID},
		{"trailing dash trimmed", strings.Repeat("c", 29) + "-", runID, strings.Repeat("c", 29) + "-" + runID},
		{"empty prefix", "", runID, runID},
		// A zero-value Config.RunID (before a caller wires it in) must fall
		// back to the bare prefix, never a prefix with a trailing "-" — that
		// would be an invalid Kubernetes object name.
		{"empty runID falls back to bare prefix", "aicr", "", "aicr"},
		{"empty prefix and empty runID", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nameWithRunID(tt.prefix, tt.runID)
			if got != tt.want {
				t.Errorf("nameWithRunID(%q, %q) = %q, want %q", tt.prefix, tt.runID, got, tt.want)
			}
			if len(got) > 63 {
				t.Errorf("len = %d, exceeds 63-char ceiling", len(got))
			}
			if strings.HasSuffix(got, "-") {
				t.Errorf("nameWithRunID(%q, %q) = %q, ends in a trailing separator (invalid Kubernetes name)", tt.prefix, tt.runID, got)
			}
		})
	}
}

func TestDeployerNameAccessors(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55" // 32 chars

	tests := []struct {
		name   string
		config Config
		get    func(*Deployer) string
		want   string
	}{
		{
			name:   "jobName uses configured JobName",
			config: Config{JobName: "my-job", RunID: runID},
			get:    (*Deployer).jobName,
			want:   "my-job-" + runID,
		},
		{
			name:   "jobName falls back to NameBase",
			config: Config{NameBase: "custom-base", RunID: runID},
			get:    (*Deployer).jobName,
			want:   "custom-base-" + runID,
		},
		{
			name:   "jobName falls back to default base",
			config: Config{RunID: runID},
			get:    (*Deployer).jobName,
			want:   "aicr-" + runID,
		},
		{
			name:   "saName uses configured ServiceAccountName",
			config: Config{ServiceAccountName: "my-sa", RunID: runID},
			get:    (*Deployer).saName,
			want:   "my-sa-" + runID,
		},
		{
			name:   "saName falls back to default base",
			config: Config{RunID: runID},
			get:    (*Deployer).saName,
			want:   "aicr-" + runID,
		},
		{
			name:   "roleName matches saName",
			config: Config{ServiceAccountName: "my-sa", RunID: runID},
			get:    (*Deployer).roleName,
			want:   "my-sa-" + runID,
		},
		{
			name:   "clusterRoleName is run-scoped",
			config: Config{RunID: runID},
			get:    (*Deployer).clusterRoleName,
			want:   "aicr-node-reader-" + runID,
		},
		{
			name:   "stagingConfigMapName is run-scoped",
			config: Config{RunID: runID},
			get:    (*Deployer).stagingConfigMapName,
			want:   "aicr-snapshot-" + runID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Deployer{config: tt.config}
			if got := tt.get(d); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
