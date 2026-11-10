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
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

const (
	kmodParamSubtypeName   = "kmod-param"
	ibCoreNetnsModeReading = "/sys/module/ib_core/parameters/netns_mode"
)

// computeRDMANamespaceMode maps the snapshot's ib_core netns_mode reading to
// a RDMANamespaceMode value: "N" is exclusive and "Y" shared. A snapshot
// without the reading (no RDMA module loaded, or captured before the
// collector existed) records nothing; any other value records unknown.
func computeRDMANamespaceMode(ctx context.Context, snap *snapshotter.Snapshot) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout,
			"context cancelled while reading RDMA namespace mode evidence", err)
	}
	if snap == nil {
		return "", nil
	}
	for _, m := range snap.Measurements {
		if m == nil || m.Type != measurement.TypeOS {
			continue
		}
		subtype := m.GetSubtype(kmodParamSubtypeName)
		if subtype == nil {
			continue
		}
		reading := subtype.Get(ibCoreNetnsModeReading)
		if reading == nil {
			continue
		}
		switch reading.Any() {
		case "N":
			return recipe.RDMANamespaceModeExclusive, nil
		case "Y":
			return recipe.RDMANamespaceModeShared, nil
		default:
			return recipe.RDMANamespaceModeUnknown, nil
		}
	}
	return "", nil
}

// applyRDMANamespaceMode records the snapshot's RDMA namespace mode as
// recipe evidence. Recipe generation does not judge it: whether the bundle
// attaches RDMA devices with the rdma CNI is a bundle-time setting, so
// CheckRDMANamespaceModeCoherent compares the two.
func applyRDMANamespaceMode(ctx context.Context, result *recipe.RecipeResult, snap *snapshotter.Snapshot) error {
	if result == nil {
		return nil
	}
	result.Metadata.RDMANamespaceMode = ""
	mode, err := computeRDMANamespaceMode(ctx, snap)
	if err != nil {
		return err
	}
	result.Metadata.RDMANamespaceMode = mode
	return nil
}
