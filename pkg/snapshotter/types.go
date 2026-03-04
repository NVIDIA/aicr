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

package snapshotter

import (
	"context"

	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Snapshotter defines the interface for collecting system configuration snapshots.
// Implementations gather measurements from various system components and serialize
// the results for analysis or recommendation generation.
type Snapshotter interface {
	Measure(ctx context.Context) error
}

// NewSnapshot creates a new Snapshot instance with an initialized Measurements slice.
func NewSnapshot() *Snapshot {
	return &Snapshot{
		Measurements: make([]*measurement.Measurement, 0),
	}
}

// Snapshot represents a collected configuration snapshot from a system node.
// It contains metadata and measurements from various collectors including
// Kubernetes, GPU, OS configuration, and systemd services.
type Snapshot struct {
	header.Header `json:",inline" yaml:",inline"`

	// Measurements contains the collected measurements from various collectors.
	Measurements []*measurement.Measurement `json:"measurements" yaml:"measurements"`
}
