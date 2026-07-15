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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

const inventoryIOBufferSize = 32 * 1024

// ErrChecksumManifestMissing identifies a missing checksums.txt independently
// of its human-readable structured error message.
var ErrChecksumManifestMissing = stderrors.New("checksum manifest is missing")

type checksumManifestMissingError struct {
	cause error
}

func (e *checksumManifestMissingError) Error() string {
	return e.cause.Error()
}

func (e *checksumManifestMissingError) Unwrap() []error {
	return []error{ErrChecksumManifestMissing, e.cause}
}

type verifiedFile struct {
	info   os.FileInfo
	digest [sha256.Size]byte
}

type regularFileOpener func(string, int, os.FileMode) (*os.File, error)

func openReadOnlyNonblocking(name string, opener regularFileOpener) (*os.File, error) {
	return opener(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

// Inventory is an exact, verified snapshot of bundle filesystem entries.
type Inventory struct {
	root        string
	files       []string
	directories []string
	totalSize   int64
	manifestLen int
	verified    map[string]verifiedFile
}

// ReadAndVerifyBundle reads the bounded checksums.txt and verifies the exact
// filesystem inventory it describes.
func ReadAndVerifyBundle(
	ctx context.Context,
	bundleDir string,
	opts InventoryOptions,
) (*Manifest, *Inventory, []byte, error) {

	if err := contextErr(ctx, "bundle verification canceled"); err != nil {
		return nil, nil, nil, err
	}
	root, _, err := requireBundleRoot(bundleDir)
	if err != nil {
		return nil, nil, nil, err
	}
	checksumPath, err := bundlePath(root, ChecksumFileName)
	if err != nil {
		return nil, nil, nil, err
	}
	data, err := readRegularBounded(ctx, checksumPath)
	if err != nil {
		return nil, nil, nil, err
	}
	manifest, inventory, err := VerifyBundle(ctx, root, data, opts)
	if err != nil {
		return nil, nil, nil, err
	}
	return manifest, inventory, append([]byte(nil), data...), nil
}

// VerifyBundle verifies data as the exact inventory for bundleDir.
func VerifyBundle(
	ctx context.Context,
	bundleDir string,
	data []byte,
	opts InventoryOptions,
) (*Manifest, *Inventory, error) {

	if int64(len(data)) > defaults.MaxChecksumFileBytes {
		return nil, nil, errors.New(errors.ErrCodeInvalidRequest, "checksums.txt exceeds the maximum allowed size")
	}
	manifest, err := ParseManifest(ctx, data, opts)
	if err != nil {
		return nil, nil, err
	}
	root, _, err := requireBundleRoot(bundleDir)
	if err != nil {
		return nil, nil, err
	}

	expectedFiles := make(map[string]*Entry, manifest.Len()+1+len(manifest.opts.AllowedMetadataPaths))
	for _, entry := range manifest.entries {
		expectedFiles[entry.Path] = &entry
	}
	expectedFiles[ChecksumFileName] = nil
	for _, rel := range manifest.opts.AllowedMetadataPaths {
		fullPath, pathErr := bundlePath(root, rel)
		if pathErr != nil {
			return nil, nil, pathErr
		}
		if _, lstatErr := os.Lstat(fullPath); lstatErr == nil {
			expectedFiles[rel] = nil
		} else if !os.IsNotExist(lstatErr) {
			return nil, nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to inspect allowed metadata path %q", rel), lstatErr)
		}
	}

	expectedDirectories := directoryAncestors(expectedFiles)
	if err := validateExactTree(ctx, root, expectedFiles, expectedDirectories); err != nil {
		return nil, nil, err
	}

	files := mapKeys(expectedFiles)
	directories := mapKeys(expectedDirectories)
	verified := make(map[string]verifiedFile, len(files))
	var totalSize int64
	checksumDigest := sha256.Sum256(data)
	for _, rel := range files {
		fullPath, pathErr := bundlePath(root, rel)
		if pathErr != nil {
			return nil, nil, pathErr
		}
		file, openErr := openRegular(ctx, fullPath)
		if openErr != nil {
			return nil, nil, openErr
		}
		if rel == ChecksumFileName && file.digest != checksumDigest {
			return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
				"on-disk checksums.txt does not match the supplied manifest bytes")
		}
		if entry := expectedFiles[rel]; entry != nil {
			if fmt.Sprintf("%x", file.digest) != entry.Digest {
				return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("checksum mismatch for %q", rel))
			}
		}
		verified[rel] = file
		totalSize += file.info.Size()
	}

	return manifest, &Inventory{
		root:        root,
		files:       files,
		directories: directories,
		totalSize:   totalSize,
		manifestLen: manifest.Len(),
		verified:    verified,
	}, nil
}

// ValidateOutputRoot rejects unsafe filesystem objects before generation
// writes into an existing output root.
func ValidateOutputRoot(ctx context.Context, bundleDir string) error {
	if err := contextErr(ctx, "output root validation canceled"); err != nil {
		return err
	}
	root, rootInfo, err := requireBundleRoot(bundleDir)
	if err != nil {
		return err
	}

	walkErr := filepath.WalkDir(root, func(fullPath string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to inspect output root entry", walkErr)
		}
		if ctxErr := contextErr(ctx, "output root validation canceled"); ctxErr != nil {
			return ctxErr
		}
		info, lstatErr := os.Lstat(fullPath)
		if lstatErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to inspect output root entry", lstatErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("output root contains a symlink or special object: %s", fullPath))
		}
		return nil
	})
	if walkErr != nil {
		return errors.PropagateOrWrap(walkErr, errors.ErrCodeInvalidRequest, "failed to validate output root")
	}
	finalInfo, err := os.Lstat(root)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "output root changed during validation", err)
	}
	if finalInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(rootInfo, finalInfo) {
		return errors.New(errors.ErrCodeInvalidRequest, "output root changed during validation")
	}
	return nil
}

// StageVerifiedBundle copies a revalidated bundle snapshot into a fresh,
// private temporary directory and verifies the staged copy again. The caller
// owns the returned cleanup function and must check its cached result.
func StageVerifiedBundle(
	ctx context.Context,
	sourceDir string,
	opts InventoryOptions,
) (stagedDir string, staged *Inventory, cleanup func() error, err error) {

	return stageVerifiedBundleWithDependencies(
		ctx, sourceDir, opts, defaultPrivateBundleStageDependencies())
}

type privateBundleStageDependencies struct {
	beforeSourceOpen      func(string) error
	beforeTempOpen        func(string) error
	beforeChildCreate     func() error
	beforeChildRevalidate func(string, string) error
	randomName            func() (string, error)
	childLstat            func(*os.Root, string) (os.FileInfo, error)
	removeAll             func(*os.Root, string) error
	closeRoot             func(*os.Root) error
}

func defaultPrivateBundleStageDependencies() privateBundleStageDependencies {
	return privateBundleStageDependencies{
		beforeSourceOpen:      func(string) error { return nil },
		beforeTempOpen:        func(string) error { return nil },
		beforeChildCreate:     func() error { return nil },
		beforeChildRevalidate: func(string, string) error { return nil },
		randomName:            randomPrivateBundleStageName,
		childLstat: func(root *os.Root, name string) (os.FileInfo, error) {
			return root.Lstat(name)
		},
		removeAll: func(root *os.Root, name string) error {
			return root.RemoveAll(name)
		},
		closeRoot: func(root *os.Root) error {
			return root.Close()
		},
	}
}

func normalizePrivateBundleStageDependencies(
	deps privateBundleStageDependencies,
) privateBundleStageDependencies {

	defaults := defaultPrivateBundleStageDependencies()
	if deps.beforeSourceOpen == nil {
		deps.beforeSourceOpen = defaults.beforeSourceOpen
	}
	if deps.beforeTempOpen == nil {
		deps.beforeTempOpen = defaults.beforeTempOpen
	}
	if deps.beforeChildCreate == nil {
		deps.beforeChildCreate = defaults.beforeChildCreate
	}
	if deps.beforeChildRevalidate == nil {
		deps.beforeChildRevalidate = defaults.beforeChildRevalidate
	}
	if deps.randomName == nil {
		deps.randomName = defaults.randomName
	}
	if deps.childLstat == nil {
		deps.childLstat = defaults.childLstat
	}
	if deps.removeAll == nil {
		deps.removeAll = defaults.removeAll
	}
	if deps.closeRoot == nil {
		deps.closeRoot = defaults.closeRoot
	}
	return deps
}

type stableDirectoryRoot struct {
	path     string
	resolved string
	info     os.FileInfo
	root     *os.Root
}

type privateBundleStage struct {
	path       string
	parent     stableDirectoryRoot
	childName  string
	childInfo  os.FileInfo
	childRoot  *os.Root
	deps       privateBundleStageDependencies
	closeOnce  sync.Once
	closeError error
}

type privateBundleStageAllocation struct {
	deps       privateBundleStageDependencies
	source     *stableDirectoryRoot
	temp       *stableDirectoryRoot
	childName  string
	childInfo  os.FileInfo
	childRoot  *os.Root
	stagePath  string
	sourceOpen bool
	tempOpen   bool
	childOwned bool
	childOpen  bool
}

func stageVerifiedBundleWithDependencies(
	ctx context.Context,
	sourceDir string,
	opts InventoryOptions,
	deps privateBundleStageDependencies,
) (stagedDir string, staged *Inventory, cleanup func() error, err error) {

	deps = normalizePrivateBundleStageDependencies(deps)

	_, source, checksumData, err := ReadAndVerifyBundle(ctx, sourceDir, opts)
	if err != nil {
		return "", nil, nil, err
	}
	stage, err := newPrivateBundleStage(ctx, source.root, deps)
	if err != nil {
		return "", nil, nil, err
	}
	stagedDir = stage.path
	cleanup = stage.Close
	fail := func(stageErr error) error {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return stderrors.Join(stageErr, cleanupErr)
		}
		return stageErr
	}

	for _, rel := range source.RelativeFiles() {
		if ctxErr := contextErr(ctx, "bundle staging canceled"); ctxErr != nil {
			return "", nil, nil, fail(ctxErr)
		}
		sourceFile, openErr := source.Open(ctx, rel)
		if openErr != nil {
			return "", nil, nil, fail(openErr)
		}
		destinationRel := filepath.FromSlash(rel)
		if mkdirErr := stage.childRoot.MkdirAll(filepath.Dir(destinationRel), 0755); mkdirErr != nil {
			_ = sourceFile.Close()
			return "", nil, nil, fail(errors.Wrap(
				errors.ErrCodeInternal, "failed to create staged bundle directory", mkdirErr))
		}
		mode := source.verified[rel].info.Mode().Perm()
		destination, createErr := stage.childRoot.OpenFile(
			destinationRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if createErr != nil {
			_ = sourceFile.Close()
			return "", nil, nil, fail(errors.Wrap(
				errors.ErrCodeInternal, "failed to create staged bundle file", createErr))
		}
		copyErr := copyWithContext(ctx, destination, sourceFile)
		_ = sourceFile.Close()
		closeErr := destination.Close()
		if copyErr != nil {
			return "", nil, nil, fail(errors.PropagateOrWrap(
				copyErr, errors.ErrCodeInternal, "failed to copy staged bundle file"))
		}
		if closeErr != nil {
			return "", nil, nil, fail(errors.Wrap(
				errors.ErrCodeInternal, "failed to close staged bundle file", closeErr))
		}
		if chmodErr := stage.childRoot.Chmod(destinationRel, mode); chmodErr != nil {
			return "", nil, nil, fail(errors.Wrap(
				errors.ErrCodeInternal, "failed to preserve staged bundle file mode", chmodErr))
		}
	}

	stagedChecksumPath, pathErr := bundlePath(stagedDir, ChecksumFileName)
	if pathErr != nil {
		return "", nil, nil, fail(pathErr)
	}
	stagedChecksumData, readErr := readRegularBounded(ctx, stagedChecksumPath)
	if readErr != nil {
		return "", nil, nil, fail(readErr)
	}
	if !bytes.Equal(stagedChecksumData, checksumData) {
		return "", nil, nil, fail(errors.New(errors.ErrCodeInvalidRequest,
			"staged checksums.txt differs from the verified source bytes"))
	}
	_, staged, stagedData, verifyErr := ReadAndVerifyBundle(ctx, stagedDir, opts)
	if verifyErr != nil {
		return "", nil, nil, fail(verifyErr)
	}
	if !bytes.Equal(stagedData, checksumData) {
		return "", nil, nil, fail(errors.New(errors.ErrCodeInvalidRequest,
			"reverified staged checksums.txt differs from the verified source bytes"))
	}
	if compareErr := compareInventoryDigests(source, staged); compareErr != nil {
		return "", nil, nil, fail(compareErr)
	}
	return stagedDir, staged, cleanup, nil
}

func newPrivateBundleStage(
	ctx context.Context,
	sourceDir string,
	deps privateBundleStageDependencies,
) (stage *privateBundleStage, retErr error) {

	deps = normalizePrivateBundleStageDependencies(deps)
	if ctxErr := contextErr(ctx, "bundle stage allocation canceled"); ctxErr != nil {
		return nil, ctxErr
	}

	source, openErr := openStableDirectoryRoot(
		ctx, sourceDir, "bundle source", errors.ErrCodeInvalidRequest,
		deps.beforeSourceOpen, deps.closeRoot)
	if openErr != nil {
		return nil, openErr
	}
	allocation := &privateBundleStageAllocation{
		deps:       deps,
		source:     source,
		sourceOpen: true,
	}
	defer func() {
		if cleanupErr := allocation.cleanup(); cleanupErr != nil {
			retErr = stderrors.Join(retErr, cleanupErr)
			stage = nil
		}
	}()

	if prepareErr := allocation.prepare(ctx); prepareErr != nil {
		return nil, prepareErr
	}
	if createErr := allocation.createChild(ctx); createErr != nil {
		return nil, createErr
	}
	if closeErr := allocation.closeSource(); closeErr != nil {
		return nil, closeErr
	}
	return allocation.commit(), nil
}

func (a *privateBundleStageAllocation) prepare(ctx context.Context) error {
	temp, openErr := openStableDirectoryRoot(
		ctx, os.TempDir(), "configured temporary root", errors.ErrCodeInternal,
		a.deps.beforeTempOpen, a.deps.closeRoot)
	if openErr != nil {
		return openErr
	}
	a.temp = temp
	a.tempOpen = true

	if os.SameFile(a.source.info, a.temp.info) ||
		pathIsEqualOrBelow(a.temp.path, a.source.path) ||
		pathIsEqualOrBelow(a.temp.resolved, a.source.resolved) {

		return errors.New(errors.ErrCodeInternal,
			"configured temporary root aliases or is inside the bundle source")
	}
	if stableErr := a.validateRoots(); stableErr != nil {
		return stableErr
	}

	childName, nameErr := a.deps.randomName()
	if nameErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to generate verified bundle stage name", nameErr)
	}
	if childName == "" || filepath.Base(childName) != childName ||
		childName == "." || childName == ".." {

		return errors.New(errors.ErrCodeInternal, "generated verified bundle stage name is unsafe")
	}
	a.childName = childName
	return nil
}

func (a *privateBundleStageAllocation) createChild(ctx context.Context) error {
	if hookErr := a.deps.beforeChildCreate(); hookErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"verified bundle stage creation hook failed", hookErr)
	}
	if ctxErr := contextErr(ctx, "bundle stage allocation canceled"); ctxErr != nil {
		return ctxErr
	}
	// Source changes remain caller-input failures. Do not propagate the
	// internal code used for ambient temp-root identity checks here.
	if stableErr := a.validateRoots(); stableErr != nil {
		return stableErr
	}
	if mkdirErr := a.temp.root.Mkdir(a.childName, 0700); mkdirErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to create verified bundle stage", mkdirErr)
	}

	// Ownership starts at successful Mkdir, before any later operation can fail.
	a.childOwned = true
	childInfo, statErr := a.deps.childLstat(a.temp.root, a.childName)
	if statErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to inspect verified bundle stage", statErr)
	}
	a.childInfo = childInfo
	if childInfo == nil || !childInfo.IsDir() || childInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New(errors.ErrCodeInternal,
			"verified bundle stage is not a real directory")
	}
	if chmodErr := a.temp.root.Chmod(a.childName, 0700); chmodErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to set verified bundle stage permissions", chmodErr)
	}
	childRoot, openErr := a.temp.root.OpenRoot(a.childName)
	if openErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to open verified bundle stage", openErr)
	}
	a.childRoot = childRoot
	a.childOpen = true

	openedChildInfo, openedStatErr := childRoot.Stat(".")
	if openedStatErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to inspect opened verified bundle stage", openedStatErr)
	}
	if !openedChildInfo.IsDir() || !os.SameFile(a.childInfo, openedChildInfo) {
		return errors.New(errors.ErrCodeInternal,
			"verified bundle stage changed while opening")
	}
	return a.revalidateChild(ctx, openedChildInfo)
}

func (a *privateBundleStageAllocation) revalidateChild(
	ctx context.Context,
	openedChildInfo os.FileInfo,
) error {

	if hookErr := a.deps.beforeChildRevalidate(a.temp.resolved, a.childName); hookErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"verified bundle stage revalidation hook failed", hookErr)
	}
	if ctxErr := contextErr(ctx, "bundle stage allocation canceled"); ctxErr != nil {
		return ctxErr
	}
	if stableErr := a.validateRoots(); stableErr != nil {
		return stableErr
	}
	postChildInfo, statErr := a.deps.childLstat(a.temp.root, a.childName)
	if statErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"verified bundle stage changed during creation", statErr)
	}
	if postChildInfo == nil || !postChildInfo.IsDir() || postChildInfo.Mode()&os.ModeSymlink != 0 ||
		postChildInfo.Mode().Perm() != 0700 ||
		!os.SameFile(a.childInfo, postChildInfo) || !os.SameFile(openedChildInfo, postChildInfo) {

		return errors.New(errors.ErrCodeInternal,
			"verified bundle stage changed during creation")
	}

	stagePath := filepath.Join(a.temp.resolved, a.childName)
	pathInfo, pathStatErr := os.Lstat(stagePath)
	if pathStatErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to inspect verified bundle stage pathname", pathStatErr)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(a.childInfo, pathInfo) {
		return errors.New(errors.ErrCodeInternal,
			"verified bundle stage pathname changed during creation")
	}
	a.stagePath = stagePath
	return nil
}

func (a *privateBundleStageAllocation) validateRoots() error {
	if sourceErr := ensureStableDirectoryRootWithCode(
		a.source, errors.ErrCodeInvalidRequest, "bundle source"); sourceErr != nil {
		return sourceErr
	}
	return ensureStableDirectoryRootWithCode(
		a.temp, errors.ErrCodeInternal, "configured temporary root")
}

func (a *privateBundleStageAllocation) closeSource() error {
	closeErr := a.deps.closeRoot(a.source.root)
	a.sourceOpen = false
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to close verified bundle source root", closeErr)
	}
	return nil
}

func (a *privateBundleStageAllocation) commit() *privateBundleStage {
	a.tempOpen = false
	a.childOpen = false
	a.childOwned = false
	return &privateBundleStage{
		path:      a.stagePath,
		parent:    *a.temp,
		childName: a.childName,
		childInfo: a.childInfo,
		childRoot: a.childRoot,
		deps:      a.deps,
	}
}

func (a *privateBundleStageAllocation) cleanup() error {
	var cleanupErrors []error
	if a.childOpen {
		if closeErr := a.deps.closeRoot(a.childRoot); closeErr != nil {
			cleanupErrors = append(cleanupErrors, errors.Wrap(errors.ErrCodeInternal,
				"failed to close rejected verified bundle stage", closeErr))
		}
		a.childOpen = false
	}
	if childCleanupErr := a.cleanupChild(); childCleanupErr != nil {
		cleanupErrors = append(cleanupErrors, childCleanupErr)
	}
	if a.tempOpen {
		if closeErr := a.deps.closeRoot(a.temp.root); closeErr != nil {
			cleanupErrors = append(cleanupErrors, errors.Wrap(errors.ErrCodeInternal,
				"failed to close configured temporary root", closeErr))
		}
		a.tempOpen = false
	}
	if a.sourceOpen {
		if closeErr := a.deps.closeRoot(a.source.root); closeErr != nil {
			cleanupErrors = append(cleanupErrors, errors.Wrap(errors.ErrCodeInternal,
				"failed to close verified bundle source root", closeErr))
		}
		a.sourceOpen = false
	}
	return stderrors.Join(cleanupErrors...)
}

func (a *privateBundleStageAllocation) cleanupChild() error {
	if !a.childOwned {
		return nil
	}
	a.childOwned = false
	if a.temp == nil || a.temp.root == nil {
		return errors.New(errors.ErrCodeInternal,
			"rejected verified bundle stage parent is unavailable")
	}
	if a.childInfo == nil {
		// Mkdir established ownership of this anchored name, but the first
		// identity read failed. Direct removal is the only portable cleanup;
		// replacement by the same user remains within the documented race limit.
		return removeRejectedPrivateBundleStage(a.temp.root, a.childName, a.deps)
	}

	named, statErr := a.deps.childLstat(a.temp.root, a.childName)
	switch {
	case os.IsNotExist(statErr):
		return nil
	case statErr != nil:
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to inspect rejected verified bundle stage", statErr)
	case named == nil || named.Mode()&os.ModeSymlink != 0 || !named.IsDir():
		return errors.New(errors.ErrCodeInternal,
			"rejected verified bundle stage identity changed before cleanup")
	case !os.SameFile(a.childInfo, named):
		return errors.New(errors.ErrCodeInternal,
			"rejected verified bundle stage identity changed before cleanup")
	default:
		return removeRejectedPrivateBundleStage(a.temp.root, a.childName, a.deps)
	}
}

func removeRejectedPrivateBundleStage(
	root *os.Root,
	childName string,
	deps privateBundleStageDependencies,
) error {

	if removeErr := deps.removeAll(root, childName); removeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to remove rejected verified bundle stage", removeErr)
	}
	return nil
}

func openStableDirectoryRoot(
	ctx context.Context,
	directoryPath string,
	label string,
	code errors.ErrorCode,
	beforeOpen func(string) error,
	closeRoot func(*os.Root) error,
) (stable *stableDirectoryRoot, retErr error) {

	if err := contextErr(ctx, label+" validation canceled"); err != nil {
		return nil, err
	}
	absPath, err := filepath.Abs(directoryPath)
	if err != nil {
		return nil, errors.Wrap(code, "failed to resolve "+label, err)
	}
	absPath = filepath.Clean(absPath)
	before, err := os.Lstat(absPath)
	if err != nil {
		return nil, errors.Wrap(code, "failed to inspect "+label, err)
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New(code, label+" is not a real directory")
	}
	if beforeOpen != nil {
		if hookErr := beforeOpen(absPath); hookErr != nil {
			return nil, errors.Wrap(code, "failed before opening "+label, hookErr)
		}
	}
	root, err := os.OpenRoot(absPath)
	if err != nil {
		return nil, errors.Wrap(code, "failed to open "+label, err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			closeErr := closeRoot(root)
			if closeErr != nil {
				retErr = stderrors.Join(retErr, errors.Wrap(errors.ErrCodeInternal,
					"failed to close "+label+" after validation failure", closeErr))
				stable = nil
			}
		}
	}()
	opened, err := root.Stat(".")
	if err != nil {
		return nil, errors.Wrap(code, "failed to inspect opened "+label, err)
	}
	after, err := os.Lstat(absPath)
	if err != nil {
		return nil, errors.Wrap(code, label+" changed while opening", err)
	}
	if !opened.IsDir() || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(before, after) || !os.SameFile(opened, after) {

		return nil, errors.New(code, label+" changed while opening")
	}
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, errors.Wrap(code, "failed to canonicalize "+label, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, errors.Wrap(code, "failed to resolve canonical "+label, err)
	}
	resolvedInfo, err := os.Lstat(resolved)
	if err != nil {
		return nil, errors.Wrap(code, "failed to inspect canonical "+label, err)
	}
	if !resolvedInfo.IsDir() || resolvedInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, resolvedInfo) {

		return nil, errors.New(code, label+" canonical identity changed while opening")
	}
	closeOnError = false
	return &stableDirectoryRoot{
		path:     absPath,
		resolved: filepath.Clean(resolved),
		info:     opened,
		root:     root,
	}, nil
}

func ensureStableDirectoryRoot(stable *stableDirectoryRoot) error {
	return ensureStableDirectoryRootWithCode(
		stable, errors.ErrCodeInternal, "held directory root")
}

func ensureStableDirectoryRootWithCode(
	stable *stableDirectoryRoot,
	code errors.ErrorCode,
	label string,
) error {

	if stable == nil || stable.root == nil {
		return errors.New(code, label+" root is unavailable")
	}
	opened, err := stable.root.Stat(".")
	if err != nil {
		return errors.Wrap(code, "failed to inspect held "+label+" root", err)
	}
	named, err := os.Lstat(stable.path)
	if err != nil {
		return errors.Wrap(code, "held "+label+" root pathname changed", err)
	}
	resolved, err := os.Lstat(stable.resolved)
	if err != nil {
		return errors.Wrap(code, "held canonical "+label+" root changed", err)
	}
	if !opened.IsDir() || !named.IsDir() || !resolved.IsDir() ||
		named.Mode()&os.ModeSymlink != 0 || resolved.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(stable.info, opened) || !os.SameFile(stable.info, named) ||
		!os.SameFile(stable.info, resolved) {

		return errors.New(code, "held "+label+" root identity changed")
	}
	return nil
}

func pathIsEqualOrBelow(candidate, root string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func randomPrivateBundleStageName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "aicr-bundle-stage-" + hex.EncodeToString(random[:]), nil
}

// Close removes the unchanged private stage through its held parent root,
// closes both retained roots, and returns the same cached result to all callers.
func (s *privateBundleStage) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		var closeErrors []error
		parentStable := true
		if err := ensureStableDirectoryRoot(&s.parent); err != nil {
			parentStable = false
			closeErrors = append(closeErrors, errors.PropagateOrWrap(
				err, errors.ErrCodeInternal, "verified bundle stage parent changed before cleanup"))
		}
		childStable := false
		if parentStable {
			opened, openedErr := s.childRoot.Stat(".")
			named, namedErr := s.parent.root.Lstat(s.childName)
			switch {
			case openedErr != nil:
				closeErrors = append(closeErrors, errors.Wrap(errors.ErrCodeInternal,
					"failed to inspect held verified bundle stage", openedErr))
			case namedErr != nil:
				closeErrors = append(closeErrors, errors.Wrap(errors.ErrCodeInternal,
					"failed to inspect named verified bundle stage", namedErr))
			case !opened.IsDir() || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 ||
				!os.SameFile(s.childInfo, opened) || !os.SameFile(s.childInfo, named):

				closeErrors = append(closeErrors, errors.New(errors.ErrCodeInternal,
					"verified bundle stage identity changed before cleanup"))
			default:
				childStable = true
			}
		}
		if s.childRoot != nil {
			if err := s.deps.closeRoot(s.childRoot); err != nil {
				closeErrors = append(closeErrors, errors.Wrap(errors.ErrCodeInternal,
					"failed to close verified bundle stage root", err))
			}
		}
		if parentStable && childStable {
			if err := s.deps.removeAll(s.parent.root, s.childName); err != nil {
				closeErrors = append(closeErrors, errors.Wrap(errors.ErrCodeInternal,
					"failed to remove verified bundle stage", err))
			}
		}
		if s.parent.root != nil {
			if err := s.deps.closeRoot(s.parent.root); err != nil {
				closeErrors = append(closeErrors, errors.Wrap(errors.ErrCodeInternal,
					"failed to close verified bundle stage parent root", err))
			}
		}
		s.closeError = stderrors.Join(closeErrors...)
	})
	return s.closeError
}

func compareInventoryDigests(source, staged *Inventory) error {
	if source == nil || staged == nil || len(source.verified) != len(staged.verified) {
		return errors.New(errors.ErrCodeInvalidRequest, "staged bundle inventory differs from source inventory")
	}
	for rel, sourceFile := range source.verified {
		stagedFile, ok := staged.verified[rel]
		if !ok || stagedFile.digest != sourceFile.digest {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("staged bundle digest differs from source for %q", rel))
		}
	}
	return nil
}

// RelativeFiles returns the sorted bundle-relative verified file paths.
func (i *Inventory) RelativeFiles() []string {
	if i == nil {
		return nil
	}
	return append([]string(nil), i.files...)
}

// RelativeDirectories returns the sorted bundle-relative verified directories.
func (i *Inventory) RelativeDirectories() []string {
	if i == nil {
		return nil
	}
	return append([]string(nil), i.directories...)
}

// AbsoluteFiles returns verified file paths rooted at the inventory source.
func (i *Inventory) AbsoluteFiles() []string {
	if i == nil {
		return nil
	}
	files := make([]string, 0, len(i.files))
	for _, rel := range i.files {
		files = append(files, filepath.Join(i.root, filepath.FromSlash(rel)))
	}
	return files
}

// TotalSize returns the aggregate verified file size in bytes.
func (i *Inventory) TotalSize() int64 {
	if i == nil {
		return 0
	}
	return i.totalSize
}

// ManifestLen returns the number of checksum-bound payload entries.
func (i *Inventory) ManifestLen() int {
	if i == nil {
		return 0
	}
	return i.manifestLen
}

// ChecksumDigest returns the verified SHA256 digest of checksums.txt.
func (i *Inventory) ChecksumDigest() [sha256.Size]byte {
	if i == nil {
		return [sha256.Size]byte{}
	}
	return i.verified[ChecksumFileName].digest
}

// Open revalidates and opens an inventoried regular file at byte zero.
func (i *Inventory) Open(ctx context.Context, rel string) (*os.File, error) {
	if err := contextErr(ctx, "verified file open canceled"); err != nil {
		return nil, err
	}
	if i == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle inventory is required")
	}
	stored, ok := i.verified[rel]
	if !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("path %q is not in the verified bundle inventory", rel))
	}
	fullPath, err := bundlePath(i.root, rel)
	if err != nil {
		return nil, err
	}
	file, err := openReadOnlyNonblocking(fullPath, os.OpenFile)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to reopen verified bundle file", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to inspect reopened bundle file", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(stored.info, opened) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "verified bundle file changed while reopening")
	}
	digest, err := hashDescriptor(ctx, file)
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(fullPath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "verified bundle file changed while hashing", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(stored.info, after) || !os.SameFile(opened, after) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "verified bundle file changed while hashing")
	}
	if digest != stored.digest {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "verified bundle file content changed")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to rewind verified bundle file", err)
	}
	closeOnError = false
	return file, nil
}

func openRegular(ctx context.Context, filePath string) (verifiedFile, error) {
	return openRegularWithOpener(ctx, filePath, os.OpenFile)
}

func openRegularWithOpener(
	ctx context.Context,
	filePath string,
	opener regularFileOpener,
) (verifiedFile, error) {

	file, err := openReadOnlyNonblocking(filePath, opener)
	if err != nil {
		if os.IsNotExist(err) {
			return verifiedFile{}, errors.Wrap(
				errors.ErrCodeInvalidRequest, "bundle file is missing", err)
		}
		return verifiedFile{}, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open bundle file", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return verifiedFile{}, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to inspect opened bundle file", err)
	}
	if !opened.Mode().IsRegular() {
		return verifiedFile{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("bundle path is not a regular file: %s", filePath))
	}
	linked, err := os.Lstat(filePath)
	if err != nil {
		return verifiedFile{}, errors.Wrap(
			errors.ErrCodeInvalidRequest, "bundle file changed while opening", err)
	}
	if !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return verifiedFile{}, errors.New(errors.ErrCodeInvalidRequest, "bundle file changed while opening")
	}
	digest, err := hashDescriptor(ctx, file)
	if err != nil {
		return verifiedFile{}, err
	}
	after, err := os.Lstat(filePath)
	if err != nil {
		return verifiedFile{}, errors.Wrap(errors.ErrCodeInvalidRequest, "bundle file changed while hashing", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return verifiedFile{}, errors.New(errors.ErrCodeInvalidRequest, "bundle file changed while hashing")
	}
	return verifiedFile{info: opened, digest: digest}, nil
}

func hashDescriptor(ctx context.Context, file *os.File) ([sha256.Size]byte, error) {
	hash := sha256.New()
	buffer := make([]byte, inventoryIOBufferSize)
	for {
		if err := contextErr(ctx, "bundle file hashing canceled"); err != nil {
			return [sha256.Size]byte{}, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				break
			}
			return [sha256.Size]byte{}, errors.Wrap(errors.ErrCodeInvalidRequest,
				"failed to read bundle file", readErr)
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func readRegularBounded(ctx context.Context, filePath string) ([]byte, error) {
	return readRegularBoundedWithOpener(ctx, filePath, os.OpenFile)
}

func readRegularBoundedWithOpener(
	ctx context.Context,
	filePath string,
	opener regularFileOpener,
) ([]byte, error) {

	file, err := openReadOnlyNonblocking(filePath, opener)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "checksums.txt is missing",
				&checksumManifestMissingError{cause: err})
		}
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open checksums.txt", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to inspect checksums.txt", err)
	}
	if !opened.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "checksums.txt is not a regular file")
	}
	if opened.Size() > defaults.MaxChecksumFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "checksums.txt exceeds the maximum allowed size")
	}

	data := make([]byte, 0, opened.Size())
	buffer := make([]byte, inventoryIOBufferSize)
	for {
		if ctxErr := contextErr(ctx, "checksums.txt read canceled"); ctxErr != nil {
			return nil, ctxErr
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			if int64(len(data)+read) > defaults.MaxChecksumFileBytes {
				return nil, errors.New(errors.ErrCodeInvalidRequest, "checksums.txt exceeds the maximum allowed size")
			}
			data = append(data, buffer[:read]...)
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				break
			}
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to read checksums.txt", readErr)
		}
	}
	after, err := os.Lstat(filePath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "checksums.txt changed while reading", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "checksums.txt changed while reading")
	}
	return data, nil
}

func readRegularBoundedFromRoot(ctx context.Context, root *os.Root, rel string) ([]byte, error) {
	file, err := root.OpenFile(
		filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open checksums.txt", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to inspect checksums.txt", err)
	}
	if !opened.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "checksums.txt is not a regular file")
	}
	if opened.Size() > defaults.MaxChecksumFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "checksums.txt exceeds the maximum allowed size")
	}

	data := make([]byte, 0, opened.Size())
	buffer := make([]byte, inventoryIOBufferSize)
	for {
		if ctxErr := contextErr(ctx, "checksums.txt read canceled"); ctxErr != nil {
			return nil, ctxErr
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			if int64(len(data)+read) > defaults.MaxChecksumFileBytes {
				return nil, errors.New(
					errors.ErrCodeInvalidRequest, "checksums.txt exceeds the maximum allowed size")
			}
			data = append(data, buffer[:read]...)
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				break
			}
			return nil, errors.Wrap(
				errors.ErrCodeInvalidRequest, "failed to read checksums.txt", readErr)
		}
	}
	after, err := root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, errors.Wrap(
			errors.ErrCodeInvalidRequest, "checksums.txt changed while reading", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return nil, errors.New(
			errors.ErrCodeInvalidRequest, "checksums.txt changed while reading")
	}
	return data, nil
}

func validateExactTree(
	ctx context.Context,
	root string,
	expectedFiles map[string]*Entry,
	expectedDirectories map[string]struct{},
) error {

	seenFiles := make(map[string]struct{}, len(expectedFiles))
	walkErr := filepath.WalkDir(root, func(fullPath string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to inspect bundle entry", walkErr)
		}
		if err := contextErr(ctx, "bundle inventory walk canceled"); err != nil {
			return err
		}
		if fullPath == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, fullPath)
		if relErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to resolve bundle entry path", relErr)
		}
		rel = filepath.ToSlash(rel)
		if validateErr := validateCanonicalPath(rel); validateErr != nil {
			return validateErr
		}
		info, lstatErr := os.Lstat(fullPath)
		if lstatErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to inspect bundle entry", lstatErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("bundle contains a symlink: %q", rel))
		}
		if info.IsDir() {
			if _, ok := expectedDirectories[rel]; !ok {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("bundle contains an unexpected directory: %q", rel))
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("bundle contains a special object: %q", rel))
		}
		if _, ok := expectedFiles[rel]; !ok {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("bundle contains an unexpected file: %q", rel))
		}
		seenFiles[rel] = struct{}{}
		return nil
	})
	if walkErr != nil {
		return errors.PropagateOrWrap(walkErr, errors.ErrCodeInvalidRequest, "failed to walk bundle inventory")
	}
	for rel := range expectedFiles {
		if _, ok := seenFiles[rel]; !ok {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("bundle is missing expected file: %q", rel))
		}
	}
	return nil
}

func requireBundleRoot(bundleDir string) (string, os.FileInfo, error) {
	if bundleDir == "" {
		return "", nil, errors.New(errors.ErrCodeInvalidRequest, "bundle directory is required")
	}
	root, err := filepath.Abs(bundleDir)
	if err != nil {
		return "", nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve bundle directory", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", nil, errors.Wrap(errors.ErrCodeInvalidRequest, "bundle directory is unavailable", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", nil, errors.New(errors.ErrCodeInvalidRequest,
			"bundle directory must be an actual directory, not a symlink or special object")
	}
	return root, info, nil
}

func bundlePath(root, rel string) (string, error) {
	fullPath, err := deployer.SafeJoin(root, filepath.FromSlash(rel))
	if err != nil {
		return "", errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid bundle path")
	}
	return fullPath, nil
}

func directoryAncestors(files map[string]*Entry) map[string]struct{} {
	directories := make(map[string]struct{})
	for rel := range files {
		for directory := path.Dir(rel); directory != "."; directory = path.Dir(directory) {
			directories[directory] = struct{}{}
		}
	}
	return directories
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, inventoryIOBufferSize)
	for {
		if err := contextErr(ctx, "bundle staging canceled"); err != nil {
			return err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			if writeErr != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to write staged bundle file", writeErr)
			}
			if written != read {
				return errors.Wrap(errors.ErrCodeInternal, "failed to write complete staged bundle file", io.ErrShortWrite)
			}
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				return nil
			}
			return errors.Wrap(errors.ErrCodeInternal, "failed to read verified bundle file", readErr)
		}
	}
}
