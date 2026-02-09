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

package result

import (
	"fmt"
	"time"

	"github.com/NVIDIA/eidos/pkg/bundler/types"
)

// DeploymentInfo contains structured deployment instructions.
// Deployers populate this to provide user-facing guidance.
type DeploymentInfo struct {
	// Type describes the deployment method (e.g., "Helm per-component bundle", "ArgoCD applications").
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

// SuccessCount returns the number of successful bundlers.
func (o *Output) SuccessCount() int {
	count := 0
	for _, r := range o.Results {
		if r.Success {
			count++
		}
	}
	return count
}

// FailureCount returns the number of failed bundlers.
func (o *Output) FailureCount() int {
	return len(o.Results) - o.SuccessCount()
}

// Summary returns a human-readable summary of the bundle generation.
func (o *Output) Summary() string {
	return fmt.Sprintf(
		"Generated %d files (%s) in %v. Success: %d/%d bundlers.",
		o.TotalFiles,
		formatBytes(o.TotalSize),
		o.TotalDuration.Round(time.Millisecond),
		o.SuccessCount(),
		len(o.Results),
	)
}

// formatBytes formats bytes into human-readable format.
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// ByType returns results grouped by bundler type.
// This allows easy lookup of results for specific bundlers.
func (o *Output) ByType() map[types.BundleType]*Result {
	results := make(map[types.BundleType]*Result, len(o.Results))
	for _, r := range o.Results {
		results[r.Type] = r
	}
	return results
}

// FailedBundlers returns list of bundler types that failed.
// This provides a quick way to identify which bundlers encountered errors.
func (o *Output) FailedBundlers() []types.BundleType {
	failed := make([]types.BundleType, 0, len(o.Errors))
	for _, e := range o.Errors {
		failed = append(failed, e.BundlerType)
	}
	return failed
}

// SuccessfulBundlers returns list of bundler types that succeeded.
// This complements FailedBundlers for complete status overview.
func (o *Output) SuccessfulBundlers() []types.BundleType {
	successful := make([]types.BundleType, 0, len(o.Results))
	for _, r := range o.Results {
		if r.Success {
			successful = append(successful, r.Type)
		}
	}
	return successful
}
