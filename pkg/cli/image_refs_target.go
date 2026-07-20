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

package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/NVIDIA/aicr/pkg/errors"
)

type imageRefsTarget struct {
	bundle     *bundleOutputTarget
	parentPath string
	baseName   string
	root       *os.Root
	parentInfo fs.FileInfo
	targetInfo fs.FileInfo
	deps       imageRefsTargetDependencies
	closeOnce  sync.Once
	closeErr   error
}

type imageRefsTargetDependencies struct {
	lstat                  func(string) (fs.FileInfo, error)
	openRoot               func(string) (*os.Root, error)
	rootLstat              func(*os.Root, string) (fs.FileInfo, error)
	rootOpenFile           func(*os.Root, string, int, fs.FileMode) (*os.File, error)
	fileStat               func(*os.File) (fs.FileInfo, error)
	fileWrite              func(*os.File, []byte) (int, error)
	fileChmod              func(*os.File, fs.FileMode) error
	closeFile              func(*os.File) error
	rootRemove             func(*os.Root, string) error
	rootRename             func(*os.Root, string, string) error
	closeRoot              func(*os.Root) error
	sameFile               func(fs.FileInfo, fs.FileInfo) bool
	randomName             func() (string, error)
	beforeParentOpen       func(string) error
	beforeTargetOpen       func(*imageRefsTarget) error
	beforeParentRevalidate func(*imageRefsTarget) error
	beforeTargetRevalidate func(*imageRefsTarget) error
	beforeTempRevalidate   func(*imageRefsTarget, string, fs.FileInfo) error
}

func defaultImageRefsTargetDependencies() imageRefsTargetDependencies {
	return imageRefsTargetDependencies{
		lstat:    os.Lstat,
		openRoot: os.OpenRoot,
		rootLstat: func(root *os.Root, name string) (fs.FileInfo, error) {
			return root.Lstat(name)
		},
		rootOpenFile: func(root *os.Root, name string, flag int, mode fs.FileMode) (*os.File, error) {
			return root.OpenFile(name, flag, mode)
		},
		fileStat:  func(file *os.File) (fs.FileInfo, error) { return file.Stat() },
		fileWrite: func(file *os.File, data []byte) (int, error) { return file.Write(data) },
		fileChmod: func(file *os.File, mode fs.FileMode) error { return file.Chmod(mode) },
		closeFile: func(file *os.File) error { return file.Close() },
		rootRemove: func(root *os.Root, name string) error {
			return root.Remove(name)
		},
		rootRename: func(root *os.Root, oldName, newName string) error {
			return root.Rename(oldName, newName)
		},
		closeRoot:  func(root *os.Root) error { return root.Close() },
		sameFile:   os.SameFile,
		randomName: randomImageRefsTempName,
		beforeParentOpen: func(string) error {
			return nil
		},
		beforeTargetOpen: func(*imageRefsTarget) error {
			return nil
		},
		beforeParentRevalidate: func(*imageRefsTarget) error {
			return nil
		},
		beforeTargetRevalidate: func(*imageRefsTarget) error {
			return nil
		},
		beforeTempRevalidate: func(*imageRefsTarget, string, fs.FileInfo) error {
			return nil
		},
	}
}

func normalizeImageRefsTargetDependencies(
	deps imageRefsTargetDependencies,
) imageRefsTargetDependencies {

	defaults := defaultImageRefsTargetDependencies()
	if deps.lstat == nil {
		deps.lstat = defaults.lstat
	}
	if deps.openRoot == nil {
		deps.openRoot = defaults.openRoot
	}
	if deps.rootLstat == nil {
		deps.rootLstat = defaults.rootLstat
	}
	if deps.rootOpenFile == nil {
		deps.rootOpenFile = defaults.rootOpenFile
	}
	if deps.fileStat == nil {
		deps.fileStat = defaults.fileStat
	}
	if deps.fileWrite == nil {
		deps.fileWrite = defaults.fileWrite
	}
	if deps.fileChmod == nil {
		deps.fileChmod = defaults.fileChmod
	}
	if deps.closeFile == nil {
		deps.closeFile = defaults.closeFile
	}
	if deps.rootRemove == nil {
		deps.rootRemove = defaults.rootRemove
	}
	if deps.rootRename == nil {
		deps.rootRename = defaults.rootRename
	}
	if deps.closeRoot == nil {
		deps.closeRoot = defaults.closeRoot
	}
	if deps.sameFile == nil {
		deps.sameFile = defaults.sameFile
	}
	if deps.randomName == nil {
		deps.randomName = defaults.randomName
	}
	if deps.beforeParentOpen == nil {
		deps.beforeParentOpen = defaults.beforeParentOpen
	}
	if deps.beforeTargetOpen == nil {
		deps.beforeTargetOpen = defaults.beforeTargetOpen
	}
	if deps.beforeParentRevalidate == nil {
		deps.beforeParentRevalidate = defaults.beforeParentRevalidate
	}
	if deps.beforeTargetRevalidate == nil {
		deps.beforeTargetRevalidate = defaults.beforeTargetRevalidate
	}
	if deps.beforeTempRevalidate == nil {
		deps.beforeTempRevalidate = defaults.beforeTempRevalidate
	}
	return deps
}

func prepareImageRefsTarget(
	ctx context.Context,
	bundle *bundleOutputTarget,
	targetPath string,
) (*imageRefsTarget, error) {

	return prepareImageRefsTargetWithDependencies(
		ctx, bundle, targetPath, defaultImageRefsTargetDependencies())
}

func prepareImageRefsTargetWithDependencies(
	ctx context.Context,
	bundle *bundleOutputTarget,
	targetPath string,
	deps imageRefsTargetDependencies,
) (*imageRefsTarget, error) {

	deps = normalizeImageRefsTargetDependencies(deps)
	if bundle == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle output target is required")
	}
	if err := bundle.validateAncestor(ctx); err != nil {
		return nil, err
	}
	absTarget, absErr := filepath.Abs(targetPath)
	if absErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to resolve image-reference target", absErr)
	}
	absTarget = filepath.Clean(absTarget)
	parentPath := filepath.Dir(absTarget)
	baseName := filepath.Base(absTarget)
	if baseName == "." || baseName == string(filepath.Separator) || baseName == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "image-reference target must name a file")
	}
	if pathsConflict(absTarget, bundle.path) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"image-reference target must be outside the bundle output")
	}
	resolvedParent, parentResolveErr := filepath.EvalSymlinks(parentPath)
	if parentResolveErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"image-reference target parent is unavailable", parentResolveErr)
	}
	resolvedAncestor, ancestorResolveErr := filepath.EvalSymlinks(bundle.ancestorPath)
	if ancestorResolveErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"bundle output ancestor cannot be resolved", ancestorResolveErr)
	}
	resolvedBundle := filepath.Join(resolvedAncestor, bundle.relativePath)
	if pathsConflict(filepath.Join(resolvedParent, baseName), resolvedBundle) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"image-reference target aliases the bundle output")
	}

	parentAncestor, parentRelative, parentOutputInfo, inspectErr :=
		inspectPlannedDirectory(ctx, parentPath, deps.lstat)
	if inspectErr != nil {
		return nil, inspectErr
	}
	if parentOutputInfo == nil || parentAncestor != parentPath || parentRelative != "." {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"image-reference target parent must be an existing real directory")
	}
	if err := deps.beforeParentOpen(parentPath); err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"image-reference target parent changed before open")
	}
	root, openErr := deps.openRoot(parentPath)
	if openErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to retain image-reference target parent", openErr)
	}
	parentInfo, statErr := root.Stat(".")
	parentNamed, parentLstatErr := deps.lstat(parentPath)
	if statErr != nil || parentLstatErr != nil || !parentInfo.IsDir() ||
		parentNamed.Mode()&os.ModeSymlink != 0 || !deps.sameFile(parentInfo, parentNamed) {

		primary := errors.Wrap(errors.ErrCodeInternal,
			"image-reference target parent changed during open",
			stderrors.Join(statErr, parentLstatErr))
		return nil, joinInternalCleanup(primary, deps.closeRoot(root), "image-reference target parent")
	}
	if !deps.sameFile(parentOutputInfo, parentInfo) {
		primary := errors.New(errors.ErrCodeInternal,
			"image-reference target parent identity changed before open")
		return nil, joinInternalCleanup(primary, deps.closeRoot(root), "image-reference target parent")
	}
	target := &imageRefsTarget{
		bundle:     bundle,
		parentPath: parentPath,
		baseName:   baseName,
		root:       root,
		parentInfo: parentOutputInfo,
		deps:       deps,
	}
	initialInfo, initialErr := deps.rootLstat(root, baseName)
	if initialErr != nil && !os.IsNotExist(initialErr) {
		return nil, joinInternalCleanup(errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to inspect image-reference target", initialErr), target.close(), "image-reference target")
	}
	initiallyAbsent := os.IsNotExist(initialErr)
	if !initiallyAbsent && (initialInfo.Mode()&os.ModeSymlink != 0 || !initialInfo.Mode().IsRegular()) {
		return nil, joinInternalCleanup(errors.New(errors.ErrCodeInvalidRequest,
			"image-reference target must be absent or a regular file"), target.close(), "image-reference target")
	}
	if err := deps.beforeTargetOpen(target); err != nil {
		return nil, joinInternalCleanup(errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"image-reference target changed before open"), target.close(), "image-reference target")
	}
	info, lstatErr := deps.rootLstat(root, baseName)
	if lstatErr != nil {
		if !initiallyAbsent || !os.IsNotExist(lstatErr) {
			return nil, joinInternalCleanup(errors.Wrap(errors.ErrCodeInternal,
				"image-reference target changed before retained open", lstatErr), target.close(), "image-reference target")
		}
	} else {
		if initiallyAbsent || !deps.sameFile(initialInfo, info) {
			return nil, joinInternalCleanup(errors.New(errors.ErrCodeInternal,
				"image-reference target identity changed before retained open"), target.close(), "image-reference target")
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, joinInternalCleanup(errors.New(errors.ErrCodeInvalidRequest,
				"image-reference target must be absent or a regular file"), target.close(), "image-reference target")
		}
		target.targetInfo = initialInfo
	}
	if err := target.validateParent(ctx); err != nil {
		return nil, joinInternalCleanup(err, target.close(), "image-reference target")
	}
	if err := target.validateTarget(ctx); err != nil {
		return nil, joinInternalCleanup(err, target.close(), "image-reference target")
	}
	if bundle.generated != nil {
		if err := target.validateBundleAliases(ctx); err != nil {
			return nil, joinInternalCleanup(err, target.close(), "image-reference target")
		}
	}
	return target, nil
}

func pathsConflict(candidate, bundle string) bool {
	return sameOrBelowPath(candidate, bundle) ||
		sameOrBelowPath(strings.ToLower(candidate), strings.ToLower(bundle))
}

func sameOrBelowPath(candidate, root string) bool {
	candidate = filepath.Clean(candidate)
	root = filepath.Clean(root)
	if candidate == root {
		return true
	}
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (t *imageRefsTarget) validateParent(ctx context.Context) error {
	if t == nil || t.root == nil {
		return errors.New(errors.ErrCodeInternal, "image-reference target is not retained")
	}
	if err := cliFilesystemContextError(ctx, "image-reference target validation canceled"); err != nil {
		return err
	}
	held, heldErr := t.root.Stat(".")
	named, namedErr := t.deps.lstat(t.parentPath)
	if heldErr != nil || namedErr != nil || !held.IsDir() || !named.IsDir() ||
		named.Mode()&os.ModeSymlink != 0 || !t.deps.sameFile(t.parentInfo, held) ||
		!t.deps.sameFile(t.parentInfo, named) {

		return errors.Wrap(errors.ErrCodeInternal,
			"image-reference target parent identity changed", stderrors.Join(heldErr, namedErr))
	}
	return nil
}

func (t *imageRefsTarget) validateTarget(ctx context.Context) error {
	if err := cliFilesystemContextError(ctx, "image-reference target validation canceled"); err != nil {
		return err
	}
	named, err := t.deps.rootLstat(t.root, t.baseName)
	if t.targetInfo == nil {
		if err == nil {
			return errors.New(errors.ErrCodeInternal,
				"absent image-reference target appeared during publication")
		}
		if !os.IsNotExist(err) {
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to revalidate absent image-reference target", err)
		}
		return nil
	}
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !named.Mode().IsRegular() ||
		!t.deps.sameFile(t.targetInfo, named) {

		return errors.Wrap(errors.ErrCodeInternal,
			"image-reference target identity changed", err)
	}
	file, openErr := t.deps.rootOpenFile(t.root, t.baseName, os.O_RDONLY, 0)
	if openErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to retain image-reference target", openErr)
	}
	opened, statErr := t.deps.fileStat(file)
	after, afterErr := t.deps.rootLstat(t.root, t.baseName)
	closeErr := t.deps.closeFile(file)
	if statErr != nil || afterErr != nil || closeErr != nil || !opened.Mode().IsRegular() ||
		!t.deps.sameFile(t.targetInfo, opened) || !t.deps.sameFile(t.targetInfo, after) {

		return errors.Wrap(errors.ErrCodeInternal, "image-reference target changed during validation",
			stderrors.Join(statErr, afterErr, closeErr))
	}
	return nil
}

func (t *imageRefsTarget) validateBundleAliases(ctx context.Context) error {
	bundleInfos, err := t.bundle.entryInfos(ctx)
	if err != nil {
		return err
	}
	for _, directoryInfo := range bundleInfos.directories {
		if t.deps.sameFile(t.parentInfo, directoryInfo) {
			return errors.New(errors.ErrCodeInvalidRequest,
				"image-reference target parent aliases a generated bundle directory")
		}
	}
	targetInfo, err := t.deps.rootLstat(t.root, t.baseName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(errors.ErrCodeInternal, "failed to inspect image-reference target", err)
	}
	if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
		return errors.New(errors.ErrCodeInternal, "image-reference target became unsafe")
	}
	for _, bundleInfo := range bundleInfos.regularFiles {
		if t.deps.sameFile(targetInfo, bundleInfo) {
			return errors.New(errors.ErrCodeInvalidRequest,
				"image-reference target is hardlinked to a generated bundle file")
		}
	}
	return nil
}

func randomImageRefsTempName() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal,
			"failed to allocate image-reference temporary name", err)
	}
	return ".aicr-image-refs-" + hex.EncodeToString(bytes[:]) + ".tmp", nil
}

func validImageRefsTempName(name string) bool {
	return filepath.Base(name) == name && strings.HasPrefix(name, ".aicr-image-refs-") &&
		strings.HasSuffix(name, ".tmp")
}

//nolint:funlen // ordered identity checks must remain adjacent to the atomic write seams
func (t *imageRefsTarget) writeAtomic(ctx context.Context, data []byte) (retErr error) {
	if t == nil {
		return errors.New(errors.ErrCodeInternal, "image-reference target is required")
	}
	if err := t.validateParent(ctx); err != nil {
		return err
	}
	if err := t.validateTarget(ctx); err != nil {
		return err
	}
	if err := t.validateBundleAliases(ctx); err != nil {
		return err
	}

	tempName, nameErr := t.deps.randomName()
	if nameErr != nil {
		return errors.PropagateOrWrap(nameErr, errors.ErrCodeInternal,
			"failed to allocate image-reference temporary name")
	}
	if !validImageRefsTempName(tempName) || tempName == t.baseName {
		return errors.New(errors.ErrCodeInternal,
			"image-reference temporary name is unsafe")
	}
	temp, openErr := t.deps.rootOpenFile(t.root, tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if openErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to create image-reference temporary file", openErr)
	}
	tempOpen := true
	tempOwned := true
	var tempInfo fs.FileInfo
	defer func() {
		if tempOpen {
			closeErr := t.deps.closeFile(temp)
			retErr = joinInternalCleanup(retErr, closeErr, "image-reference temporary file")
		}
		if tempOwned {
			removeErr := t.removeOwnedTemp(tempName, tempInfo)
			retErr = joinInternalCleanup(retErr, removeErr, "image-reference temporary file")
		}
	}()

	var statErr error
	tempInfo, statErr = t.deps.fileStat(temp)
	namedTemp, namedErr := t.deps.rootLstat(t.root, tempName)
	if statErr != nil || namedErr != nil || !tempInfo.Mode().IsRegular() ||
		namedTemp.Mode()&os.ModeSymlink != 0 || !t.deps.sameFile(tempInfo, namedTemp) {

		return errors.Wrap(errors.ErrCodeInternal,
			"image-reference temporary file changed during creation", stderrors.Join(statErr, namedErr))
	}
	if err := cliFilesystemContextError(ctx, "image-reference write canceled"); err != nil {
		return err
	}
	written, writeErr := t.deps.fileWrite(temp, data)
	if writeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write image references", writeErr)
	}
	if written != len(data) {
		return errors.New(errors.ErrCodeInternal, "short write while writing image references")
	}
	if err := t.deps.fileChmod(temp, 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to set image-reference temporary file mode", err)
	}
	updatedInfo, updatedStatErr := t.deps.fileStat(temp)
	updatedNamed, namedErr := t.deps.rootLstat(t.root, tempName)
	if updatedStatErr != nil || namedErr != nil || !updatedInfo.Mode().IsRegular() ||
		updatedInfo.Mode().Perm() != 0o600 || updatedInfo.Size() != int64(len(data)) ||
		!t.deps.sameFile(tempInfo, updatedInfo) || !t.deps.sameFile(tempInfo, updatedNamed) {

		return errors.Wrap(errors.ErrCodeInternal,
			"image-reference temporary file failed revalidation",
			stderrors.Join(updatedStatErr, namedErr))
	}

	if err := t.deps.beforeParentRevalidate(t); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"image-reference parent changed before final validation")
	}
	if err := t.validateParent(ctx); err != nil {
		return err
	}
	if err := t.deps.beforeTargetRevalidate(t); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"image-reference target changed before final validation")
	}
	if err := t.validateTarget(ctx); err != nil {
		return err
	}
	if err := t.validateBundleAliases(ctx); err != nil {
		return err
	}
	if err := t.deps.beforeTempRevalidate(t, tempName, tempInfo); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"image-reference temporary file changed before final validation")
	}
	if err := t.validateTemp(ctx, tempName, tempInfo, int64(len(data))); err != nil {
		return err
	}

	if closeErr := t.deps.closeFile(temp); closeErr != nil {
		tempOpen = false
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to close image-reference temporary file", closeErr)
	}
	tempOpen = false

	// These are the last portable identity checks before rename. Go does not
	// provide an identity-conditional rename, so callers must not concurrently
	// mutate this directory after the checks complete.
	if err := t.validateParent(ctx); err != nil {
		return err
	}
	if err := t.validateTarget(ctx); err != nil {
		return err
	}
	if err := t.validateTemp(ctx, tempName, tempInfo, int64(len(data))); err != nil {
		return err
	}
	if err := t.validateBundleAliases(ctx); err != nil {
		return err
	}
	if err := cliFilesystemContextError(ctx, "image-reference write canceled"); err != nil {
		return err
	}
	renameErr := t.deps.rootRename(t.root, tempName, t.baseName)
	if renameErr != nil {
		renameErr = errors.Wrap(errors.ErrCodeInternal, "failed to publish image references", renameErr)
	}
	if err := authoritativeFilesystemContextError(
		ctx, "image-reference write canceled after publish", renameErr); err != nil {
		return err
	}
	tempOwned = false
	published, publishedErr := t.deps.rootLstat(t.root, t.baseName)
	if err := cliFilesystemContextError(ctx, "image-reference write canceled after validation"); err != nil {
		return err
	}
	if publishedErr != nil || !published.Mode().IsRegular() || published.Mode().Perm() != 0o600 ||
		published.Size() != int64(len(data)) || !t.deps.sameFile(tempInfo, published) {

		return errors.Wrap(errors.ErrCodeInternal,
			"published image-reference target failed revalidation", publishedErr)
	}
	t.targetInfo = published
	return cliFilesystemContextError(ctx, "image-reference write canceled after validation")
}

func (t *imageRefsTarget) validateTemp(
	ctx context.Context,
	name string,
	expected fs.FileInfo,
	expectedSize int64,
) error {

	if err := cliFilesystemContextError(ctx, "image-reference temporary validation canceled"); err != nil {
		return err
	}
	named, err := t.deps.rootLstat(t.root, name)
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !named.Mode().IsRegular() ||
		named.Mode().Perm() != 0o600 || named.Size() != expectedSize ||
		!t.deps.sameFile(expected, named) {

		return errors.Wrap(errors.ErrCodeInternal,
			"image-reference temporary file identity changed", err)
	}
	return nil
}

func (t *imageRefsTarget) removeOwnedTemp(name string, expected fs.FileInfo) error {
	if expected == nil {
		return errors.New(errors.ErrCodeInternal,
			"cannot prove image-reference temporary file identity for cleanup")
	}
	named, err := t.deps.rootLstat(t.root, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to inspect image-reference temporary file for cleanup", err)
	}
	if named.Mode()&os.ModeSymlink != 0 || !named.Mode().IsRegular() ||
		!t.deps.sameFile(expected, named) {

		return errors.New(errors.ErrCodeInternal,
			"image-reference temporary file changed; refusing unsafe cleanup")
	}
	if err := t.deps.rootRemove(t.root, name); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to remove image-reference temporary file", err)
	}
	return nil
}

func (t *imageRefsTarget) close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		if t.root != nil {
			t.closeErr = t.deps.closeRoot(t.root)
		}
		if t.closeErr != nil {
			t.closeErr = errors.PropagateOrWrap(t.closeErr, errors.ErrCodeInternal,
				"failed to close image-reference target")
		}
	})
	return t.closeErr
}

func cliFilesystemContextError(ctx context.Context, message string) error {
	if ctx == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "context is required")
	}
	if err := ctx.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeTimeout, message, err)
	}
	return nil
}

func authoritativeFilesystemContextError(ctx context.Context, message string, operationErr error) error {
	if ctxErr := cliFilesystemContextError(ctx, message); ctxErr != nil {
		return ctxErr
	}
	return operationErr
}

func joinInternalCleanup(primary, cleanup error, label string) error {
	if cleanup == nil {
		return primary
	}
	wrapped := errors.PropagateOrWrap(cleanup, errors.ErrCodeInternal,
		"failed to close "+label)
	if primary == nil {
		return wrapped
	}
	return stderrors.Join(primary, wrapped)
}

func preservePrimaryCloseError(primary, closeErr error, label string) error {
	if closeErr == nil {
		return primary
	}
	wrapped := errors.PropagateOrWrap(closeErr, errors.ErrCodeInternal,
		"failed to close "+label)
	if primary == nil {
		return wrapped
	}
	slog.Warn("cleanup failed after an earlier error", "owner", label, "error", wrapped)
	return primary
}
