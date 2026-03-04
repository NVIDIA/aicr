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

package collector

import (
	"context"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Collector defines the interface for collecting system measurement data.
// Implementations gather data from various sources including system services,
// hardware components, OS configuration, and cluster state.
// All collectors must support context-based cancellation.
type Collector interface {
	Collect(ctx context.Context) (*measurement.Measurement, error)
}
