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

package checksum

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// ChecksumFileName is the standard name for checksum files.
const ChecksumFileName = "checksums.txt"

// GenerateChecksums creates a checksums.txt file containing SHA256 checksums
// for all provided files. The checksums are written relative to the bundle directory.
//
// Parameters:
//   - ctx: Context for cancellation
//   - bundleDir: The base directory for relative path calculation
//   - files: List of absolute file paths to include in checksums
//
// Returns an error if the context is canceled, any file cannot be read,
// or the checksums file cannot be written.
func GenerateChecksums(ctx context.Context, bundleDir string, files []string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable, "context canceled before checksum generation", err)
	}

	checksums := make([]string, 0, len(files))

	for _, file := range files {
		digest, err := SHA256Raw(file)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to compute checksum for %s", file), err)
		}

		relPath, err := filepath.Rel(bundleDir, file)
		if err != nil {
			// If relative path fails, use absolute path
			relPath = file
		}

		checksums = append(checksums, fmt.Sprintf("%s  %s", hex.EncodeToString(digest), relPath))
	}

	checksumPath, joinErr := deployer.SafeJoin(bundleDir, ChecksumFileName)
	if joinErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "unsafe checksum path", joinErr)
	}
	content := strings.Join(checksums, "\n") + "\n"

	if err := os.WriteFile(checksumPath, []byte(content), 0600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write checksums", err)
	}

	slog.Debug("checksums generated",
		"file_count", len(checksums),
		"path", checksumPath,
	)

	return nil
}

// GetChecksumFilePath returns the full path to the checksums.txt file
// in the given bundle directory.
// filepath.Join is safe here: ChecksumFileName is a compile-time constant
// and the return type (string) has no error channel for SafeJoin.
func GetChecksumFilePath(bundleDir string) string {
	return filepath.Join(bundleDir, ChecksumFileName)
}

// VerifyChecksums reads a checksums.txt file and verifies each file's SHA256 digest.
// Returns a list of error descriptions for any mismatches or read failures.
// An empty return means all checksums are valid.
func VerifyChecksums(bundleDir string) []string {
	checksumPath, joinErr := deployer.SafeJoin(bundleDir, ChecksumFileName)
	if joinErr != nil {
		return []string{fmt.Sprintf("unsafe checksum path: %v", joinErr)}
	}
	data, err := os.ReadFile(checksumPath)
	if err != nil {
		return []string{fmt.Sprintf("failed to read %s: %v", ChecksumFileName, err)}
	}
	return VerifyChecksumsFromData(bundleDir, data)
}

// VerifyChecksumsFromData verifies checksums using pre-read checksums.txt content.
// This avoids re-reading the file, preventing TOCTOU races when the same data
// is also used for digest computation.
func VerifyChecksumsFromData(bundleDir string, data []byte) []string {
	var errs []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Format: <hex-digest>  <relative-path> (two spaces, sha256sum compatible)
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			errs = append(errs, fmt.Sprintf("invalid checksum line: %s", line))
			continue
		}

		expectedDigest := parts[0]
		relativePath := parts[1]
		filePath, joinErr := deployer.SafeJoin(bundleDir, relativePath)
		if joinErr != nil {
			errs = append(errs, fmt.Sprintf("path traversal detected in checksum entry: %s", relativePath))
			continue
		}

		digest, readErr := SHA256Raw(filePath)
		if readErr != nil {
			errs = append(errs, fmt.Sprintf("failed to read %s: %v", relativePath, readErr))
			continue
		}

		actualDigest := hex.EncodeToString(digest)
		if actualDigest != expectedDigest {
			errs = append(errs, fmt.Sprintf("checksum mismatch: %s (expected %s, got %s)", relativePath, expectedDigest, actualDigest))
		}
	}

	return errs
}

// CountEntries returns the number of entries in a checksums.txt file.
// filepath.Join is safe here: ChecksumFileName is a compile-time constant
// and the return type (int) has no error channel for SafeJoin.
func CountEntries(bundleDir string) int {
	checksumPath := filepath.Join(bundleDir, ChecksumFileName)
	data, err := os.ReadFile(checksumPath)
	if err != nil {
		return 0
	}

	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// SHA256Raw computes a file's SHA256 digest using streaming I/O and returns
// the raw bytes. Does not load the entire file into memory.
func SHA256Raw(path string) ([]byte, error) {
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // G703: path from internal callers only
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to open file for digest: %s", path), err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to read file for digest: %s", path), err)
	}
	return h.Sum(nil), nil
}
