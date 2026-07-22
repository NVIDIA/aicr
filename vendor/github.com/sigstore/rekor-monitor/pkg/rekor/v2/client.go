// Copyright 2025 The Sigstore Authors.
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

package v2

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/sigstore/rekor-monitor/pkg/tiles"
	"github.com/sigstore/rekor-monitor/pkg/util"
	tiles_client "github.com/sigstore/rekor-tiles/v2/pkg/client"
	"github.com/sigstore/rekor-tiles/v2/pkg/client/read"
	"github.com/sigstore/rekor-tiles/v2/pkg/generated/protobuf"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/transparency-dev/formats/log"
	"github.com/transparency-dev/tessera/api"
	"github.com/transparency-dev/tessera/api/layout"
	"golang.org/x/mod/sumdb/note"
	"google.golang.org/protobuf/encoding/protojson"
)

type ShardInfo struct {
	client      *read.Client
	verifier    *signature.Verifier
	validityEnd time.Time
}

func (s ShardInfo) ReadCheckpoint(ctx context.Context) (*log.Checkpoint, *note.Note, error) {
	return (*s.client).ReadCheckpoint(ctx)
}

func (s ShardInfo) ReadTile(ctx context.Context, level, index uint64, p uint8) ([]byte, error) {
	return (*s.client).ReadTile(ctx, level, index, p)
}

func (s ShardInfo) ReadEntryBundle(ctx context.Context, index uint64, p uint8) ([]byte, error) {
	return (*s.client).ReadEntryBundle(ctx, index, p)
}

type Entry struct {
	ProtoEntry *protobuf.Entry
	Index      int64
}

func RefreshSigningConfig(tufClient *tuf.Client) (*root.SigningConfig, error) {
	err := tufClient.Refresh()
	if err != nil {
		return nil, fmt.Errorf("error refreshing TUF client: %v", err)
	}

	signingConfig, err := root.GetSigningConfig(tufClient)
	if err != nil {
		return nil, fmt.Errorf("error getting SigningConfig target: %v", err)
	}
	return signingConfig, nil
}

func filterV2Shards(rekorServices []root.Service) []root.Service {
	// First we sort and filter the Rekor services so that they're ordered from
	// newest to oldest. We filter them so that we:
	// - only include the v2 logs.
	// - only include shards that are (or were) valid. No shards that will be valid in the future
	sortedServices := make([]root.Service, len(rekorServices))
	copy(sortedServices, rekorServices)
	slices.SortFunc(sortedServices, func(i, j root.Service) int {
		return i.ValidityPeriodStart.Compare(j.ValidityPeriodStart)
	})

	var rekorV2Services []root.Service
	now := time.Now()
	for _, s := range slices.Backward(sortedServices) {
		if s.MajorAPIVersion == 2 && !s.ValidityPeriodStart.IsZero() && s.ValidityPeriodStart.Before(now) {
			rekorV2Services = append(rekorV2Services, s)
		}
	}

	return rekorV2Services
}

func ShardsNeedUpdating(currentShards map[string]ShardInfo, newSigningConfig *root.SigningConfig) (bool, error) {
	newShards := newSigningConfig.RekorLogURLs()
	newV2Shards := filterV2Shards(newShards)

	if len(newV2Shards) == 0 {
		return false, fmt.Errorf("error fetching Rekor shards: no v2 shards found in SigningConfig")
	}

	if len(currentShards) != len(newV2Shards) {
		// Shards were added/removed, need to update
		return true, nil
	}

	for _, newShard := range newV2Shards {
		newShardOrigin, err := tiles.GetOrigin(newShard.URL)
		if err != nil {
			return false, err
		}

		matchingShard, ok := currentShards[newShardOrigin]
		switch {
		case !ok:
			// The shard in the new SigningConfig is not present
			// in the existing shards, so we need to update
			return true, nil
		case matchingShard.validityEnd != newShard.ValidityPeriodEnd:
			// The newest shard in the SigningConfig is present in
			// the existing shards, but the end validity time changed
			return true, nil
		}
	}

	// All the shards in the new SigningConfig are present in
	// the existing shards, and they have the same validity end time
	return false, nil
}

func GetRekorShards(ctx context.Context, trustedRoot *root.TrustedRoot, rekorServices []root.Service, userAgent, certChain string) (map[string]ShardInfo, string, error) {
	rekorV2Services := filterV2Shards(rekorServices)
	if len(rekorV2Services) == 0 {
		return nil, "", fmt.Errorf("failed to find any Rekor v2 shards")
	}

	clientOpts := []tiles_client.Option{tiles_client.WithUserAgent(userAgent)}
	var tlsConfig *tls.Config
	if certChain != "" {
		var err error
		tlsConfig, err = util.TLSConfigForCA(certChain)
		if err != nil {
			return nil, "", fmt.Errorf("getting TLS config: %w", err)
		}
		clientOpts = append(clientOpts, tiles_client.WithTLSConfig(tlsConfig))
	}

	rekorShards := make(map[string]ShardInfo)
	latestShardOrigin := ""
	for _, service := range rekorV2Services {
		var err error
		origin, err := tiles.GetOrigin(service.URL)
		if err != nil {
			return nil, "", err
		}

		// The services in rekorV2Services are ordered from newest to oldest,
		// so we store the origin of the first one as the origin
		// of the latest shard
		if latestShardOrigin == "" {
			latestShardOrigin = origin
		}
		parsedURL, err := url.Parse(service.URL)
		if err != nil {
			return nil, "", fmt.Errorf("error parsing Rekor url: %v", err)
		}

		verifier, err := GetLogVerifier(ctx, parsedURL, trustedRoot, userAgent, tlsConfig)
		if err != nil {
			return nil, "", err
		}

		rekorClient, err := read.NewReader(service.URL, origin, verifier, clientOpts...)
		if err != nil {
			return nil, "", fmt.Errorf("getting Rekor client: %v", err)
		}

		// ReadCheckpoint fetches and verifies the current checkpoint
		// We verify the checkpoints of all v2 shards
		checkpoint, _, err := rekorClient.ReadCheckpoint(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("failed to get current checkpoint for log '%v': %v", origin, err)
		}

		rekorShards[checkpoint.Origin] = ShardInfo{&rekorClient, &verifier, service.ValidityPeriodEnd}
	}
	return rekorShards, latestShardOrigin, nil
}

func getEntriesFromTile(ctx context.Context, client tiles.Client, fullTileIndex int64, partialTileWidth uint8) ([]Entry, error) {
	bundleBytes, err := client.ReadEntryBundle(ctx, uint64(fullTileIndex), partialTileWidth) //nolint: gosec // G115
	if err != nil {
		return nil, fmt.Errorf("failed to fetch entry bundle")
	}
	var bundle api.EntryBundle
	err = bundle.UnmarshalText(bundleBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse entry bundle")
	}
	var entries []Entry
	for i, entryBytes := range bundle.Entries {
		logEntry := protobuf.Entry{}
		err = protojson.Unmarshal(entryBytes, &logEntry)
		if err != nil {
			return nil, fmt.Errorf("failed to parse entry")
		}
		entries = append(entries, Entry{ProtoEntry: &logEntry, Index: fullTileIndex*layout.TileWidth + int64(i)})
	}
	return entries, nil
}
