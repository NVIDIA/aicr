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

// Package deployer defines the shared interface and types for bundle deployers.
//
// Each deployer (helm, argocd, argocdhelm) produces deployment artifacts from
// a configured recipe. Deployers are configured as structs with all required
// data, then Generate is called to produce the output.
//
// The Deployer interface enables mockability in bundler tests and provides a
// consistent contract across deployer implementations. All three deployers
// (helm, argocd, argocdhelm) implement this interface.
package deployer

import (
	"context"
	"time"
)

// Output contains the result of deployer generation.
type Output struct {
	// Files contains the paths of generated files.
	Files []string

	// TotalSize is the total size of all generated files.
	TotalSize int64

	// Duration is the time taken to generate the output.
	Duration time.Duration

	// DeploymentSteps contains ordered deployment instructions for the user.
	DeploymentSteps []string

	// DeploymentNotes contains optional deployment notes or warnings.
	DeploymentNotes []string
}

// Deployer generates deployment bundles from configured inputs.
// Implementations are configured as structs, then Generate is called.
type Deployer interface {
	Generate(ctx context.Context, outputDir string) (*Output, error)
}
