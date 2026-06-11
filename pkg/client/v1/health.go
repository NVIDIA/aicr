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

package aicr

import (
	"context"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/health"
)

// ComputeHealth scores the structural health of every leaf recipe in this
// Client's catalog and returns a deterministic *health.Report, optionally
// narrowed by filter. Computation is delegated wholesale to pkg/health.Compute;
// this facade only binds the Client's own DataProvider and version so health is
// scored against the same catalog (including any --data overlays) the Client
// resolves with — never the process-global embedded catalog.
//
// filter narrows enumeration to leaf overlays carrying every explicitly set
// criteria dimension; nil scores all leaf combos. Empty/"any" filter dimensions
// place no constraint.
//
// health.Compute applies its own catalog-wide timeout
// (defaults.HealthComputeTimeout), so this method does not impose the shorter
// per-operation timeout the resolve methods use.
//
// Returns ErrCodeInvalidRequest on a nil/closed Client or nil context, and
// propagates the underlying structured code (or ErrCodeInternal) if health
// computation fails.
func (c *Client) ComputeHealth(ctx context.Context, filter *Criteria) (*health.Report, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}

	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	report, err := health.Compute(ctx, health.Options{
		Provider: dp,
		Version:  c.version,
		Filter:   toInternalCriteria(filter),
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to compute recipe health")
	}
	return report, nil
}
