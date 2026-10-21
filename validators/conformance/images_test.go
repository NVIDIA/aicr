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

package main

import (
	"testing"

	"github.com/distribution/reference"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

// TestTestPodImagesFullyQualified guards against short image names, which
// CRI-O in enforcing short-name mode (OKE) rejects as ambiguous.
func TestTestPodImagesFullyQualified(t *testing.T) {
	tests := []struct {
		name  string
		image string
	}{
		{"probe", defaults.ProbeImage},
		{"cuda", cudaTestImage},
		{"cluster autoscaling", clusterAutoTestImage},
		{"slinky slurm gpu", slinkySlurmGPUContainerImage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := reference.ParseNamed(tt.image); err != nil {
				t.Errorf("image %q is not fully qualified: %v", tt.image, err)
			}
		})
	}
}
