// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package aicr

import (
	"context"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

const (
	mariaDBOperatorSubtypeName = "mariadb-operator"
	mariaDBCollectionStateKey  = "collection-state"
)

// computeMariaDBOperatorState extracts the official MariaDB Operator
// conflict-evidence state from a snapshot. A missing subtype is left empty so
// snapshots created before the collector was added remain compatible.
// Malformed or unrecognized evidence fails closed to unknown.
func computeMariaDBOperatorState(
	ctx context.Context,
	snap *snapshotter.Snapshot,
) (string, error) {

	if ctx == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if err := ctx.Err(); err != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout,
			"context cancelled while scanning MariaDB Operator snapshot evidence", err)
	}
	if snap == nil {
		return "", nil
	}
	bestState := ""
	for _, m := range snap.Measurements {
		if err := ctx.Err(); err != nil {
			return "", errors.Wrap(errors.ErrCodeTimeout,
				"context cancelled while scanning MariaDB Operator snapshot evidence", err)
		}
		if m == nil || m.Type != measurement.TypeK8s {
			continue
		}
		subtype := m.GetSubtype(mariaDBOperatorSubtypeName)
		if subtype == nil {
			continue
		}
		reading := subtype.Get(mariaDBCollectionStateKey)
		state := recipe.MariaDBOperatorStateUnknown
		if reading == nil {
			if mariaDBOperatorStatePriority(state) > mariaDBOperatorStatePriority(bestState) {
				bestState = state
			}
			continue
		}
		observed, ok := reading.Any().(string)
		if ok {
			switch observed {
			case recipe.MariaDBOperatorStateAbsent,
				recipe.MariaDBOperatorStateAPIDetected,
				recipe.MariaDBOperatorStateCRsDetected,
				recipe.MariaDBOperatorStateUnknown:
				state = observed
			}
		}
		if mariaDBOperatorStatePriority(state) > mariaDBOperatorStatePriority(bestState) {
			bestState = state
		}
	}
	return bestState, nil
}

func mariaDBOperatorStatePriority(state string) int {
	switch state {
	case recipe.MariaDBOperatorStateCRsDetected:
		return 4
	case recipe.MariaDBOperatorStateUnknown:
		return 3
	case recipe.MariaDBOperatorStateAPIDetected:
		return 2
	case recipe.MariaDBOperatorStateAbsent:
		return 1
	default:
		return 0
	}
}

// applyMariaDBOperatorState records snapshot evidence for AICR-provided
// accounting and reports risk without making recipe generation a deployment
// gate. Bundle generation applies the fail-closed ownership policy.
func applyMariaDBOperatorState(
	ctx context.Context,
	result *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) error {

	if result == nil {
		return nil
	}
	// Always clear stale evidence before considering this resolution. The
	// field must describe this snapshot and accounting mode only.
	result.Metadata.MariaDBOperatorState = ""
	mode, configured := result.AccountingMode()
	if !configured || mode != recipe.AccountingModeAICRProvided {
		return nil
	}

	state, err := computeMariaDBOperatorState(ctx, snap)
	if err != nil {
		return err
	}
	result.Metadata.MariaDBOperatorState = state

	switch state {
	case recipe.MariaDBOperatorStateCRsDetected:
		slog.Warn("AICR-provided Slurm accounting conflicts with existing official MariaDB resources; "+
			"`aicr bundle` will block installation. Regenerate the recipe with "+
			"--slurm-accounting-mode customer-managed to use the existing database",
			"component", "mariadb-operator",
			"state", state)
	case recipe.MariaDBOperatorStateUnknown:
		slog.Warn("MariaDB Operator conflict evidence is inconclusive for AICR-provided Slurm accounting; "+
			"`aicr bundle` will block installation. Capture a fresh snapshot with sufficient "+
			"Kubernetes discovery permissions, or regenerate the recipe with "+
			"--slurm-accounting-mode customer-managed",
			"component", "mariadb-operator",
			"state", state)
	case recipe.MariaDBOperatorStateAPIDetected:
		slog.Warn("the official MariaDB Operator API is present but no MariaDB resources were detected; "+
			"AICR-provided Slurm accounting may reuse the existing CRDs, and bundling is allowed. "+
			"Use --slurm-accounting-mode customer-managed if the existing operator owns the database",
			"component", "mariadb-operator",
			"state", state)
	case recipe.MariaDBOperatorStateAbsent:
		// Conclusive absence is the safe installation state.
	}
	return nil
}
