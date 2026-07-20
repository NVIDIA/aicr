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

package checksum

import (
	"bytes"
	"context"
	"crypto/sha256"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/defaults"
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
	return generateChecksumsWithDependencies(
		ctx, bundleDir, files, defaultChecksumGenerationDependencies())
}

type checksumGenerationDependencies struct {
	afterValidation    func() error
	beforeVerification func() error
	randomName         func() (string, error)
	closeRoot          func(*os.Root) error
}

func defaultChecksumGenerationDependencies() checksumGenerationDependencies {
	return checksumGenerationDependencies{
		afterValidation:    func() error { return nil },
		beforeVerification: func() error { return nil },
		randomName: func() (string, error) {
			name, err := randomPrivateBundleStageName()
			if err != nil {
				return "", err
			}
			return ".checksums-" + name, nil
		},
		closeRoot: func(root *os.Root) error { return root.Close() },
	}
}

func normalizeChecksumGenerationDependencies(
	deps checksumGenerationDependencies,
) checksumGenerationDependencies {

	defaults := defaultChecksumGenerationDependencies()
	if deps.afterValidation == nil {
		deps.afterValidation = defaults.afterValidation
	}
	if deps.beforeVerification == nil {
		deps.beforeVerification = defaults.beforeVerification
	}
	if deps.randomName == nil {
		deps.randomName = defaults.randomName
	}
	if deps.closeRoot == nil {
		deps.closeRoot = defaults.closeRoot
	}
	return deps
}

// generateChecksumsWithDependencies keeps the output root anchored for every
// publication and rollback mutation. Pathname validation remains a fail-closed
// guard; the retained root is the authority for writes.
func generateChecksumsWithDependencies(
	ctx context.Context,
	bundleDir string,
	files []string,
	deps checksumGenerationDependencies,
) (retErr error) {

	deps = normalizeChecksumGenerationDependencies(deps)
	if err := ctx.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeTimeout, "context canceled before checksum generation", err)
	}
	if len(files) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest, "at least one checksum input file is required")
	}
	stable, err := openStableDirectoryRoot(
		ctx, bundleDir, "checksum output", errors.ErrCodeInvalidRequest, nil, deps.closeRoot)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := deps.closeRoot(stable.root); closeErr != nil {
			retErr = stderrors.Join(retErr, errors.Wrap(
				errors.ErrCodeInternal, "failed to close checksum output root", closeErr))
		}
	}()
	if validateErr := ValidateOutputRoot(ctx, bundleDir); validateErr != nil {
		return validateErr
	}
	if stableErr := ensureStableDirectoryRootWithCode(
		stable, errors.ErrCodeInvalidRequest, "checksum output"); stableErr != nil {
		return stableErr
	}
	root := stable.path

	entries := make([]Entry, 0, len(files))
	exactPaths := make(map[string]struct{}, len(files))
	foldedPaths := make(map[string]struct{}, len(files))
	for _, file := range files {
		if ctxErr := contextErr(ctx, "checksum generation canceled"); ctxErr != nil {
			return ctxErr
		}
		absolute, absErr := filepath.Abs(file)
		if absErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to resolve checksum input path", absErr)
		}
		relative, relErr := filepath.Rel(root, absolute)
		if relErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to resolve checksum input relative path", relErr)
		}
		relative = filepath.ToSlash(relative)
		if validateErr := validatePayloadPath(relative, InventoryOptions{}); validateErr != nil {
			return validateErr
		}
		if uniqueErr := addUniquePath(relative, exactPaths, foldedPaths); uniqueErr != nil {
			return uniqueErr
		}
		verified, openErr := openRegular(ctx, absolute)
		if openErr != nil {
			return openErr
		}
		entries = append(entries, Entry{Digest: fmt.Sprintf("%x", verified.digest), Path: relative})
	}

	manifest := &Manifest{entries: entries, opts: InventoryOptions{}}
	data, err := manifest.MarshalText()
	if err != nil {
		return err
	}
	if int64(len(data)) > defaults.MaxChecksumFileBytes {
		return errors.New(errors.ErrCodeInvalidRequest, "generated checksums.txt exceeds the maximum allowed size")
	}
	if validateErr := validateGenerationTree(ctx, root, manifest); validateErr != nil {
		return validateErr
	}
	if hookErr := deps.afterValidation(); hookErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"checksum post-validation hook failed", hookErr)
	}
	if stableErr := ensureStableDirectoryRootWithCode(
		stable, errors.ErrCodeInvalidRequest, "checksum output"); stableErr != nil {
		return stableErr
	}
	previous, err := captureChecksumPublication(ctx, stable.root)
	if err != nil {
		return err
	}
	if err := writeChecksumAtomic(ctx, stable.root, data, deps.randomName); err != nil {
		return err
	}
	var verifyErr error
	if hookErr := deps.beforeVerification(); hookErr != nil {
		verifyErr = errors.Wrap(errors.ErrCodeInternal,
			"checksum pre-verification hook failed", hookErr)
	} else if stableErr := ensureStableDirectoryRootWithCode(
		stable, errors.ErrCodeInvalidRequest, "checksum output"); stableErr != nil {
		verifyErr = stableErr
	} else {
		_, _, verifyErr = VerifyBundle(ctx, root, data, InventoryOptions{})
		if verifyErr == nil {
			verifyErr = ensureStableDirectoryRootWithCode(
				stable, errors.ErrCodeInvalidRequest, "checksum output")
		}
	}
	if verifyErr != nil {
		rollbackErr := rollbackChecksumPublication(ctx, stable.root, previous)
		if rollbackErr == nil {
			return verifyErr
		}
		return stderrors.Join(verifyErr, rollbackErr)
	}

	slog.Debug("checksums generated",
		"file_count", len(entries),
		"path", GetChecksumFilePath(root),
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

// WriteChecksums generates checksums.txt over output.Files, then appends the
// checksum file to output.Files and updates output.TotalSize. Used by deployer
// generators to finalize bundles when IncludeChecksums is true.
func WriteChecksums(ctx context.Context, bundleDir string, output *deployer.Output) error {
	if output == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "output is required")
	}
	if err := GenerateChecksums(ctx, bundleDir, output.Files); err != nil {
		return err
	}
	checksumPath := GetChecksumFilePath(bundleDir)
	info, statErr := os.Stat(checksumPath)
	if statErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to stat checksums file", statErr)
	}
	output.Files = append(output.Files, checksumPath)
	output.TotalSize += info.Size()
	return nil
}

// VerifyChecksums reads a checksums.txt file and verifies each file's SHA256 digest.
// Returns a list of error descriptions for any mismatches or read failures.
// An empty return means all checksums are valid.
func VerifyChecksums(bundleDir string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	manifest, inventory, data, err := ReadAndVerifyBundle(ctx, bundleDir, InventoryOptions{})
	if err != nil {
		return []string{err.Error()}
	}
	if manifest == nil || inventory == nil || len(data) == 0 {
		return []string{"bundle verification returned an incomplete result"}
	}
	return nil
}

// VerifyChecksumsFromData verifies checksums using pre-read checksums.txt content.
// This avoids re-reading the file, preventing TOCTOU races when the same data
// is also used for digest computation.
func VerifyChecksumsFromData(bundleDir string, data []byte) []string {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	if _, _, err := VerifyBundle(ctx, bundleDir, data, InventoryOptions{}); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// CountEntries returns the number of entries in a checksums.txt file.
// filepath.Join is safe here: ChecksumFileName is a compile-time constant
// and the return type (int) has no error channel for SafeJoin.
func CountEntries(bundleDir string) int {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	root, _, err := requireBundleRoot(bundleDir)
	if err != nil {
		return 0
	}
	checksumPath, err := bundlePath(root, ChecksumFileName)
	if err != nil {
		return 0
	}
	data, err := readRegularBounded(ctx, checksumPath)
	if err != nil {
		return 0
	}
	manifest, err := ParseManifest(ctx, data, InventoryOptions{})
	if err != nil {
		return 0
	}
	return manifest.Len()
}

func validateGenerationTree(ctx context.Context, root string, manifest *Manifest) error {
	expectedFiles := make(map[string]*Entry, manifest.Len()+1)
	for _, entry := range manifest.entries {
		expectedFiles[entry.Path] = &entry
	}
	checksumPath, err := bundlePath(root, ChecksumFileName)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(checksumPath); err == nil {
		expectedFiles[ChecksumFileName] = nil
	} else if !os.IsNotExist(err) {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to inspect existing checksums.txt", err)
	}
	return validateExactTree(ctx, root, expectedFiles, directoryAncestors(expectedFiles))
}

type checksumPublicationState struct {
	existed bool
	data    []byte
	mode    os.FileMode
}

func captureChecksumPublication(ctx context.Context, root *os.Root) (checksumPublicationState, error) {
	before, err := root.Lstat(ChecksumFileName)
	if os.IsNotExist(err) {
		return checksumPublicationState{}, nil
	}
	if err != nil {
		return checksumPublicationState{}, errors.Wrap(
			errors.ErrCodeInternal, "failed to inspect prior checksums.txt", err)
	}
	if !before.Mode().IsRegular() {
		return checksumPublicationState{}, errors.New(
			errors.ErrCodeInvalidRequest, "prior checksums.txt is not a regular file")
	}
	data, err := readRegularBoundedFromRoot(ctx, root, ChecksumFileName)
	if err != nil {
		return checksumPublicationState{}, err
	}
	after, err := root.Lstat(ChecksumFileName)
	if err != nil {
		return checksumPublicationState{}, errors.Wrap(
			errors.ErrCodeInvalidRequest, "prior checksums.txt changed while retaining it", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return checksumPublicationState{}, errors.New(
			errors.ErrCodeInvalidRequest, "prior checksums.txt changed while retaining it")
	}
	return checksumPublicationState{
		existed: true,
		data:    data,
		mode:    after.Mode().Perm(),
	}, nil
}

func rollbackChecksumPublication(
	parent context.Context,
	root *os.Root,
	previous checksumPublicationState,
) error {

	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), defaults.FileReadTimeout)
	defer cancel()
	if err := contextErr(ctx, "checksums.txt rollback canceled"); err != nil {
		return err
	}
	if previous.existed {
		if err := writeChecksumAtomicWithMode(
			ctx, root, previous.data, previous.mode,
			defaultChecksumGenerationDependencies().randomName,
		); err != nil {
			return errors.PropagateOrWrap(
				err, errors.ErrCodeInternal, "failed to restore prior checksums.txt")
		}
		return nil
	}
	if err := root.Remove(ChecksumFileName); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to remove rejected checksums.txt", err)
	}
	return nil
}

func writeChecksumAtomic(
	ctx context.Context,
	root *os.Root,
	data []byte,
	randomName func() (string, error),
) error {

	return writeChecksumAtomicWithMode(ctx, root, data, 0600, randomName)
}

func writeChecksumAtomicWithMode(
	ctx context.Context,
	root *os.Root,
	data []byte,
	mode os.FileMode,
	randomName func() (string, error),
) (retErr error) {

	temporaryName, err := randomName()
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to generate temporary checksums file name", err)
	}
	if temporaryName == "" || filepath.Base(temporaryName) != temporaryName ||
		temporaryName == "." || temporaryName == ".." || temporaryName == ChecksumFileName {

		return errors.New(errors.ErrCodeInternal, "temporary checksums file name is unsafe")
	}
	temporary, err := root.OpenFile(
		temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create temporary checksums file", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			if removeErr := root.Remove(temporaryName); removeErr != nil && !os.IsNotExist(removeErr) {
				retErr = stderrors.Join(retErr, errors.Wrap(
					errors.ErrCodeInternal, "failed to remove temporary checksums file", removeErr))
			}
		}
	}()

	writeErr := copyWithContext(ctx, temporary, bytes.NewReader(data))
	chmodErr := temporary.Chmod(mode)
	created, statErr := temporary.Stat()
	closeErr := temporary.Close()
	if writeErr != nil || chmodErr != nil || statErr != nil || closeErr != nil {
		var writeErrors []error
		if writeErr != nil {
			writeErrors = append(writeErrors, errors.PropagateOrWrap(
				writeErr, errors.ErrCodeInternal, "failed to write temporary checksums file"))
		}
		if chmodErr != nil {
			writeErrors = append(writeErrors, errors.Wrap(
				errors.ErrCodeInternal, "failed to set temporary checksums file mode", chmodErr))
		}
		if statErr != nil {
			writeErrors = append(writeErrors, errors.Wrap(
				errors.ErrCodeInternal, "failed to inspect temporary checksums file", statErr))
		}
		if closeErr != nil {
			writeErrors = append(writeErrors, errors.Wrap(
				errors.ErrCodeInternal, "failed to close temporary checksums file", closeErr))
		}
		return stderrors.Join(writeErrors...)
	}
	if ctxErr := contextErr(ctx, "checksum generation canceled before publication"); ctxErr != nil {
		return ctxErr
	}
	if !created.Mode().IsRegular() {
		return errors.New(errors.ErrCodeInternal, "temporary checksums file is not regular")
	}
	named, err := root.Lstat(temporaryName)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to inspect temporary checksums file name", err)
	}
	if !named.Mode().IsRegular() || !os.SameFile(created, named) {
		return errors.New(errors.ErrCodeInternal,
			"temporary checksums file identity changed")
	}
	if err := root.Rename(temporaryName, ChecksumFileName); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to publish checksums.txt atomically", err)
	}
	removeTemporary = false
	return nil
}

// SHA256Raw computes a file's SHA256 digest using streaming I/O and returns
// the raw bytes. Does not load the entire file into memory.
func SHA256Raw(path string) ([]byte, error) {
	return SHA256RawContext(context.Background(), path)
}

// SHA256RawContext computes a file's SHA256 digest with caller cancellation
// and an upper bound of defaults.FileReadTimeout.
func SHA256RawContext(ctx context.Context, path string) ([]byte, error) {
	return sha256RawContextWithOpener(ctx, path, os.OpenFile)
}

func sha256RawContextWithOpener(
	ctx context.Context,
	path string,
	opener regularFileOpener,
) ([]byte, error) {

	ctx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()
	if ctxErr := contextErr(ctx, "file digest read canceled"); ctxErr != nil {
		return nil, ctxErr
	}
	cleanPath := filepath.Clean(path)
	f, err := openReadOnlyNonblocking(cleanPath, opener)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to open file for digest: %s", path), err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to inspect opened file for digest: %s", path), err)
	}
	if !opened.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file for digest is not a regular file: %s", path))
	}
	linked, err := os.Lstat(cleanPath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file for digest changed while opening: %s", path), err)
	}
	if !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file for digest changed while opening: %s", path))
	}

	hash := sha256.New()
	buffer := make([]byte, inventoryIOBufferSize)
	for {
		if ctxErr := contextErr(ctx, "file digest read timed out"); ctxErr != nil {
			return nil, ctxErr
		}
		read, readErr := f.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				break
			}
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to read file for digest: %s", path), readErr)
		}
	}
	after, err := os.Lstat(cleanPath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file for digest changed while reading: %s", path), err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file for digest changed while reading: %s", path))
	}
	return hash.Sum(nil), nil
}
