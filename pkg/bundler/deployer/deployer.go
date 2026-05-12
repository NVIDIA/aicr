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

// Package deployer defines the shared interface and types for bundle deployers.
//
// Each deployer (helm, argocd, argocd-helm, flux) produces deployment artifacts from
// a configured recipe. Deployers are configured as structs with all required
// data, then Generate is called to produce the output.
//
// The Deployer interface enables mockability in bundler tests and provides a
// consistent contract across deployer implementations. All four deployers
// (helm, argocd, argocd-helm, flux) implement this interface.
package deployer

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
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

// AddDataFiles resolves each relative data file path against outputDir (via
// SafeJoin, rejecting traversal), stats the file, and appends the absolute
// path to Files while adding the file size to TotalSize. Used by generators
// to include external --data files in their output so they are covered by
// checksum generation.
func (o *Output) AddDataFiles(outputDir string, dataFiles []string) error {
	if o == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "output is required")
	}
	for _, dataFile := range dataFiles {
		absPath, joinErr := SafeJoin(outputDir, dataFile)
		if joinErr != nil {
			return joinErr
		}
		info, statErr := os.Stat(absPath)
		if statErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to stat data file %s", dataFile), statErr)
		}
		o.Files = append(o.Files, absPath)
		o.TotalSize += info.Size()
	}
	return nil
}

// Deployer generates deployment bundles from configured inputs.
// Implementations are configured as structs, then Generate is called.
type Deployer interface {
	Generate(ctx context.Context, outputDir string) (*Output, error)
}
