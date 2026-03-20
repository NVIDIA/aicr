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

// Package result provides types for tracking bundle generation results.
//
// This package defines structures for capturing generation results and
// aggregating them into a final output report with deployment instructions.
//
// # Core Types
//
// Result: Individual bundle generation result
//
//	type Result struct {
//	    Type         types.BundleType   // Bundle type (e.g., "helm-bundle")
//	    Success      bool               // Whether generation succeeded
//	    Files        []string           // Generated file paths
//	    Size         int64              // Total size in bytes
//	    Duration     time.Duration      // Generation time
//	    Checksum     string             // SHA256 checksum
//	    Errors       []string           // Non-fatal errors
//	    OCIDigest    string             // OCI digest (if pushed)
//	    OCIReference string             // OCI reference (if pushed)
//	    Pushed       bool               // Whether pushed to OCI registry
//	}
//
// Output: Aggregated results with deployment instructions
//
//	type Output struct {
//	    Results       []*Result          // Bundle results
//	    TotalSize     int64              // Total bytes generated
//	    TotalFiles    int                // Total files generated
//	    TotalDuration time.Duration      // Total generation time
//	    Errors        []BundleError      // Errors from failed bundlers
//	    OutputDir     string             // Output directory path
//	    Deployment    *DeploymentInfo    // Deployment instructions
//	}
//
// # Usage
//
// Results are created by the bundler and returned from Make:
//
//	b, _ := bundler.New()
//	output, err := b.Make(ctx, recipeResult, "./bundle")
//
//	if output.HasErrors() {
//	    // handle errors
//	}
//
// # Deployment Instructions
//
// Output includes structured deployment steps from the deployer:
//
//	if output.Deployment != nil {
//	    fmt.Println(output.Deployment.Type) // "Helm per-component bundle"
//	    for _, step := range output.Deployment.Steps {
//	        fmt.Println(step)
//	    }
//	}
//
// # Thread Safety
//
// Individual Result instances are not thread-safe. However, Output can safely
// aggregate results since each Result is independent.
package result
