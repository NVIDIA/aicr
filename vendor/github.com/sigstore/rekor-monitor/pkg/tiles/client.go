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
	"fmt"
	"net/url"

	"github.com/transparency-dev/formats/log"
	"github.com/transparency-dev/tessera/api/layout"
	"golang.org/x/mod/sumdb/note"
)

type Client interface {
	ReadCheckpoint(ctx context.Context) (*log.Checkpoint, *note.Note, error)
	ReadTile(ctx context.Context, level, index uint64, p uint8) ([]byte, error)
	ReadEntryBundle(ctx context.Context, index uint64, p uint8) ([]byte, error)
}

func GetOrigin(shardURL string) (string, error) {
	parsedURL, err := url.Parse(shardURL)
	if err != nil {
		return "", fmt.Errorf("parsing URL: %w", err)
	}
	prefixLen := len(parsedURL.Scheme) + len("://")
	if prefixLen >= len(parsedURL.String()) {
		return "", fmt.Errorf("error getting origin from URL %s", shardURL)
	}
	origin := parsedURL.String()[len(parsedURL.Scheme)+len("://"):]
	return origin, nil
}

// GetTileIndex gets the index of the tile a checkpoint belongs to.
func GetTileIndex(checkpointIndex int64) int64 {
	treeSize := checkpointIndex + 1
	fullTilesCount := treeSize / layout.TileWidth

	if treeSize%layout.TileWidth == 0 {
		return fullTilesCount - 1
	}
	return fullTilesCount
}

type Entry interface{}

// GetEntriesByIndexRange fetches all entries by log index, from (start, end]
// If start == end, it doesn't return any entries
// Returns error if start > end
func GetEntriesByIndexRange[entry Entry](ctx context.Context, client Client, start, end int64, getEntriesFromTile func(context.Context, Client, int64, uint8) ([]entry, error)) ([]entry, error) {
	if start > end {
		return nil, fmt.Errorf("start (%d) must be less than or equal to end (%d)", start, end)
	}

	var entries []entry
	if start == end {
		return entries, nil
	}

	startTileIndex := GetTileIndex(start)
	startLogSize := start + 1
	endTileIndex := GetTileIndex(end)
	endLogSize := end + 1

	// If the start and end tiles are different, first we get any remaining unread
	// entries from the start tile
	if startTileIndex < endTileIndex {
		partialBundleSize := startLogSize % layout.TileWidth
		// A partial bundle size of 0 means the start tile was read in full,
		// so no need to read it again. Otherwise, we've only read the tile
		// partially, and we need to read the remaining entries.
		if partialBundleSize > 0 {
			allStartEntries, err := getEntriesFromTile(ctx, client, startTileIndex, 0)
			if err != nil {
				return nil, fmt.Errorf("error getting bundle for tile: %d. Error: %v", startTileIndex, err)
			}
			unreadEntries := allStartEntries[partialBundleSize:]
			entries = append(entries, unreadEntries...)
		}
	}

	// We get all the entries from the full tiles in (start, end)
	for i := startTileIndex + 1; i < endTileIndex; i++ {
		currentEntries, err := getEntriesFromTile(ctx, client, i, 0)
		if err != nil {
			return nil, fmt.Errorf("error getting bundle for tile: %d. Error: %v", i, err)
		}
		entries = append(entries, currentEntries...)
	}

	// Finally, we get all the entries available in the last tile
	partialBundleSize := uint8(endLogSize % layout.TileWidth) //nolint: gosec // G115
	endEntries, err := getEntriesFromTile(ctx, client, endTileIndex, partialBundleSize)
	if err != nil {
		return nil, fmt.Errorf("error getting bundle for tile: %d, width: %d. Error: %v", endTileIndex, partialBundleSize, err)
	}

	// If start and end are in the same tile, we need to skip entries with index <= start
	if startTileIndex == endTileIndex {
		startOffset := startLogSize % layout.TileWidth
		endEntries = endEntries[startOffset:]
	}

	entries = append(entries, endEntries...)

	return entries, nil
}
