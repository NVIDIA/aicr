// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package result

import (
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/types"
)

// DeploymentInfo contains structured deployment instructions.
// Deployers populate this to provide user-facing guidance.
type DeploymentInfo struct {
	// Type describes the deployment method (e.g., "Helm per-component bundle", "Argo CD applications").
	Type string `json:"type" yaml:"type"`

	// Steps contains ordered deployment instructions (e.g., ["cd ./bundle", "helm install ..."]).
	Steps []string `json:"steps" yaml:"steps"`

	// Notes contains optional warnings or additional information.
	Notes []string `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// Output contains the aggregated results of all bundler executions.
type Output struct {
	// Results contains individual bundler results.
	Results []*Result `json:"results" yaml:"results"`

	// TotalSize is the total size in bytes of all generated files.
	TotalSize int64 `json:"total_size_bytes" yaml:"total_size_bytes"`

	// TotalFiles is the total count of generated files.
	TotalFiles int `json:"total_files" yaml:"total_files"`

	// TotalDuration is the total time taken for all bundlers.
	TotalDuration time.Duration `json:"total_duration" yaml:"total_duration"`

	// Errors contains errors from failed bundlers.
	Errors []BundleError `json:"errors,omitempty" yaml:"errors,omitempty"`

	// OutputDir is the directory where bundles were generated.
	OutputDir string `json:"output_dir" yaml:"output_dir"`

	// Deployment contains structured deployment instructions from the deployer.
	Deployment *DeploymentInfo `json:"deployment,omitempty" yaml:"deployment,omitempty"`
}

// BundleError represents an error from a specific bundler.
type BundleError struct {
	BundlerType types.BundleType `json:"bundler_type" yaml:"bundler_type"`
	Error       string           `json:"error" yaml:"error"`
}

// HasErrors returns true if any bundler failed.
func (o *Output) HasErrors() bool {
	return len(o.Errors) > 0
}
