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
	nvsentinelMongoDBComponentName   = "nvsentinel-mongodb"
	defaultStorageClassSubtypeName   = "default-storage-class"
	perconaServerMongoDBSubtypeName  = "percona-server-mongodb"
	datastoreEvidenceCollectionState = "collection-state"
)

// computeSnapshotEvidenceState scans every K8s measurement for subtype's
// collection-state reading and returns the highest-priority state. A missing
// subtype is left empty so snapshots created before the collector existed
// remain compatible; a missing, non-string, or unrecognized reading fails
// closed to unknown. priority returns 0 for unrecognized states.
func computeSnapshotEvidenceState(
	ctx context.Context,
	snap *snapshotter.Snapshot,
	subtypeName string,
	unknown string,
	priority func(string) int,
) (string, error) {

	if ctx == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if err := ctx.Err(); err != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout,
			"context cancelled while scanning "+subtypeName+" snapshot evidence", err)
	}
	if snap == nil {
		return "", nil
	}
	bestState := ""
	for _, m := range snap.Measurements {
		if err := ctx.Err(); err != nil {
			return "", errors.Wrap(errors.ErrCodeTimeout,
				"context cancelled while scanning "+subtypeName+" snapshot evidence", err)
		}
		if m == nil || m.Type != measurement.TypeK8s {
			continue
		}
		subtype := m.GetSubtype(subtypeName)
		if subtype == nil {
			continue
		}
		state := unknown
		if reading := subtype.Get(datastoreEvidenceCollectionState); reading != nil {
			if observed, ok := reading.Any().(string); ok && priority(observed) > 0 {
				state = observed
			}
		}
		if priority(state) > priority(bestState) {
			bestState = state
		}
	}
	return bestState, nil
}

func defaultStorageClassStatePriority(state string) int {
	switch state {
	case recipe.DefaultStorageClassStateAbsent:
		return 4
	case recipe.DefaultStorageClassStateUnknown:
		return 3
	case recipe.DefaultStorageClassStateMultiple:
		return 2
	case recipe.DefaultStorageClassStatePresent:
		return 1
	default:
		return 0
	}
}

func perconaOperatorStatePriority(state string) int {
	switch state {
	case recipe.PerconaOperatorStateCRsDetected:
		return 6
	case recipe.PerconaOperatorStateOperatorDetected:
		return 5
	case recipe.PerconaOperatorStateUnknown:
		return 4
	case recipe.PerconaOperatorStateAPIDetected:
		return 3
	case recipe.PerconaOperatorStateAICROwned:
		return 2
	case recipe.PerconaOperatorStateAbsent:
		return 1
	default:
		return 0
	}
}

// applyNVSentinelDatastoreState records default StorageClass and Percona
// Operator evidence for a recipe containing nvsentinel-mongodb and warns
// about risky states without failing recipe generation. Bundle generation
// applies the blocking policy.
func applyNVSentinelDatastoreState(
	ctx context.Context,
	result *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) error {

	if result == nil {
		return nil
	}
	// Clear stale evidence: the fields must describe this snapshot only.
	result.Metadata.DefaultStorageClassState = ""
	result.Metadata.PerconaOperatorState = ""
	if result.GetComponentRef(nvsentinelMongoDBComponentName) == nil {
		return nil
	}

	storageState, err := computeSnapshotEvidenceState(ctx, snap, defaultStorageClassSubtypeName,
		recipe.DefaultStorageClassStateUnknown, defaultStorageClassStatePriority)
	if err != nil {
		return err
	}
	perconaState, err := computeSnapshotEvidenceState(ctx, snap, perconaServerMongoDBSubtypeName,
		recipe.PerconaOperatorStateUnknown, perconaOperatorStatePriority)
	if err != nil {
		return err
	}
	result.Metadata.DefaultStorageClassState = storageState
	result.Metadata.PerconaOperatorState = perconaState

	switch storageState {
	case recipe.DefaultStorageClassStateAbsent:
		slog.Warn("the snapshot found no default StorageClass, so nvsentinel-mongodb's volumes would stay "+
			"Pending; `aicr bundle` will block unless a default StorageClass is created or "+
			"nvsentinelmongodb:replsets.rs0.volumeSpec.pvc.storageClassName is set",
			"component", nvsentinelMongoDBComponentName,
			"state", storageState)
	case recipe.DefaultStorageClassStateUnknown:
		slog.Warn("default StorageClass evidence is inconclusive for nvsentinel-mongodb; capture a fresh "+
			"snapshot with permission to list storageclasses",
			"component", nvsentinelMongoDBComponentName,
			"state", storageState)
	case recipe.DefaultStorageClassStateMultiple:
		slog.Warn("the snapshot found more than one default StorageClass; Kubernetes binds "+
			"nvsentinel-mongodb's volumes to the newest one",
			"component", nvsentinelMongoDBComponentName,
			"state", storageState)
	case recipe.DefaultStorageClassStatePresent:
		// A single default StorageClass is the expected state.
	}

	switch perconaState {
	case recipe.PerconaOperatorStateCRsDetected, recipe.PerconaOperatorStateOperatorDetected:
		slog.Warn("the snapshot found another Percona Operator for MongoDB deployment; AICR's psmdb-operator "+
			"would replace its psmdb.percona.com CRDs, so `aicr bundle` will block installation",
			"component", "psmdb-operator",
			"state", perconaState)
	case recipe.PerconaOperatorStateUnknown:
		slog.Warn("Percona Operator for MongoDB conflict evidence is inconclusive; capture a fresh snapshot "+
			"with sufficient Kubernetes discovery permissions",
			"component", "psmdb-operator",
			"state", perconaState)
	case recipe.PerconaOperatorStateAPIDetected:
		slog.Warn("the psmdb.percona.com API is already served with no PerconaServerMongoDB resources; "+
			"AICR's psmdb-operator will replace those CRDs with its own",
			"component", "psmdb-operator",
			"state", perconaState)
	case recipe.PerconaOperatorStateAbsent, recipe.PerconaOperatorStateAICROwned:
		// No foreign Percona installation was observed.
	}
	return nil
}
