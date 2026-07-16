// Copyright 2026 The Sigstore Authors.
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

package tiles

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/transparency-dev/formats/log"
	"github.com/transparency-dev/merkle/proof"
	"github.com/transparency-dev/merkle/rfc6962"
	tclient "github.com/transparency-dev/tessera/client"
)

// VerifyConsistencyWithCheckpoint verifies that the log has been consistent between
// the previous checkpoint and the current state. This is the core consistency check
// logic that can be used with any checkpoint source (file, database, etc.).
//
// If prevCheckpoint is nil, no consistency check is performed (first run scenario).
// Returns the current checkpoint from the latest shard.
func VerifyConsistencyWithCheckpoint[client Client](ctx context.Context, shards map[string]client, latestShardOrigin string, prevCheckpoint *log.Checkpoint) (*log.Checkpoint, error) {
	latestShardCheckpoint, _, err := (shards[latestShardOrigin]).ReadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading checkpoint: %w", err)
	}

	if prevCheckpoint != nil {
		client := shards[latestShardOrigin]
		newCheckpoint := latestShardCheckpoint
		if prevCheckpoint.Origin != latestShardOrigin {
			client = shards[prevCheckpoint.Origin]
			newCheckpoint, _, err = client.ReadCheckpoint(ctx)
			if err != nil {
				return nil, fmt.Errorf("getting current checkpoint: %w", err)
			}
		}

		pb, err := tclient.NewProofBuilder(ctx, newCheckpoint.Size, client.ReadTile)
		if err != nil {
			return nil, fmt.Errorf("getting proof builder: %w", err)
		}
		consistencyProof, err := pb.ConsistencyProof(ctx, prevCheckpoint.Size, newCheckpoint.Size)
		if err != nil {
			return nil, fmt.Errorf("building consistency proof: %w", err)
		}
		err = proof.VerifyConsistency(rfc6962.DefaultHasher, prevCheckpoint.Size, newCheckpoint.Size, consistencyProof, prevCheckpoint.Hash, newCheckpoint.Hash)
		if err != nil {
			return nil, fmt.Errorf("consistency check failed: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Root hash consistency verified - Current Size: %d Root Hash: %s - Previous Size: %d Root Hash %s\n",
			newCheckpoint.Size, hex.EncodeToString(newCheckpoint.Hash), prevCheckpoint.Size, hex.EncodeToString(prevCheckpoint.Hash))
	}

	return latestShardCheckpoint, nil
}
