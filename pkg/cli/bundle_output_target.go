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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/errors"
)

const bundleGenerationCopyBufferSize = 32 * 1024

type bundleOutputTarget struct {
	path          string
	ancestorPath  string
	relativePath  string
	ancestor      *os.Root
	ancestorInfo  fs.FileInfo
	generated     *os.Root
	generatedInfo fs.FileInfo
	deps          bundleOutputDependencies
	closeOnce     sync.Once
	closeErr      error
}

type bundleOutputDependencies struct {
	lstat               func(string) (fs.FileInfo, error)
	openRoot            func(string) (*os.Root, error)
	randomName          func() (string, error)
	rootMkdir           func(*os.Root, string, fs.FileMode) error
	rootLstat           func(*os.Root, string) (fs.FileInfo, error)
	rootOpenRoot        func(*os.Root, string) (*os.Root, error)
	rootRemoveAll       func(*os.Root, string) error
	rootRename          func(*os.Root, string, string) error
	closeRoot           func(*os.Root) error
	beforeAncestorOpen  func(string) error
	beforeGeneratedOpen func(*bundleOutputTarget) error
}

func defaultBundleOutputDependencies() bundleOutputDependencies {
	return bundleOutputDependencies{
		lstat:      os.Lstat,
		openRoot:   os.OpenRoot,
		randomName: randomBundleGenerationName,
		rootMkdir: func(root *os.Root, name string, mode fs.FileMode) error {
			return root.Mkdir(name, mode)
		},
		rootLstat: func(root *os.Root, name string) (fs.FileInfo, error) {
			return root.Lstat(name)
		},
		rootOpenRoot: func(root *os.Root, name string) (*os.Root, error) {
			return root.OpenRoot(name)
		},
		rootRemoveAll: func(root *os.Root, name string) error {
			return root.RemoveAll(name)
		},
		rootRename: func(root *os.Root, oldName, newName string) error {
			return root.Rename(oldName, newName)
		},
		closeRoot: func(root *os.Root) error { return root.Close() },
		beforeAncestorOpen: func(string) error {
			return nil
		},
		beforeGeneratedOpen: func(*bundleOutputTarget) error {
			return nil
		},
	}
}

func normalizeBundleOutputDependencies(deps bundleOutputDependencies) bundleOutputDependencies {
	defaults := defaultBundleOutputDependencies()
	if deps.lstat == nil {
		deps.lstat = defaults.lstat
	}
	if deps.openRoot == nil {
		deps.openRoot = defaults.openRoot
	}
	if deps.randomName == nil {
		deps.randomName = defaults.randomName
	}
	if deps.rootMkdir == nil {
		deps.rootMkdir = defaults.rootMkdir
	}
	if deps.rootLstat == nil {
		deps.rootLstat = defaults.rootLstat
	}
	if deps.rootOpenRoot == nil {
		deps.rootOpenRoot = defaults.rootOpenRoot
	}
	if deps.rootRemoveAll == nil {
		deps.rootRemoveAll = defaults.rootRemoveAll
	}
	if deps.rootRename == nil {
		deps.rootRename = defaults.rootRename
	}
	if deps.closeRoot == nil {
		deps.closeRoot = defaults.closeRoot
	}
	if deps.beforeAncestorOpen == nil {
		deps.beforeAncestorOpen = defaults.beforeAncestorOpen
	}
	if deps.beforeGeneratedOpen == nil {
		deps.beforeGeneratedOpen = defaults.beforeGeneratedOpen
	}
	return deps
}

type bundleGenerationStage struct {
	target    *bundleOutputTarget
	name      string
	path      string
	root      *os.Root
	info      fs.FileInfo
	closeOnce sync.Once
	closeErr  error
}

func bundleFilesystemError(message string, causes ...error) error {
	cause := stderrors.Join(causes...)
	if cause == nil {
		return errors.New(errors.ErrCodeInternal, message)
	}
	return errors.Wrap(errors.ErrCodeInternal, message, cause)
}

func (t *bundleOutputTarget) validateRetainedDirectoryName(
	name string,
	expected fs.FileInfo,
	message string,
) error {

	if t == nil || t.ancestor == nil || expected == nil {
		return errors.New(errors.ErrCodeInternal, message)
	}
	named, err := t.deps.rootLstat(t.ancestor, name)
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !named.IsDir() ||
		!os.SameFile(expected, named) {

		return bundleFilesystemError(message, err)
	}
	return nil
}

func (t *bundleOutputTarget) renameRetainedDirectory(
	oldName string,
	expected fs.FileInfo,
	newName string,
	message string,
) error {

	if err := t.validateRetainedDirectoryName(oldName, expected, message); err != nil {
		return err
	}
	if _, err := t.deps.rootLstat(t.ancestor, newName); err == nil {
		return errors.New(errors.ErrCodeInternal, message+": destination appeared")
	} else if !os.IsNotExist(err) {
		return errors.Wrap(errors.ErrCodeInternal, message+": failed to inspect destination", err)
	}
	if err := t.deps.rootRename(t.ancestor, oldName, newName); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, message, err)
	}
	return nil
}

func (t *bundleOutputTarget) removeRetainedDirectory(
	name string,
	expected fs.FileInfo,
	message string,
) error {

	if err := t.validateRetainedDirectoryName(name, expected, message); err != nil {
		return err
	}
	if err := t.deps.rootRemoveAll(t.ancestor, name); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, message, err)
	}
	return nil
}

func randomBundleGenerationName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal,
			"failed to generate private bundle stage name", err)
	}
	return ".aicr-bundle-generation-" + hex.EncodeToString(random[:]), nil
}

func prepareBundleOutputTarget(ctx context.Context, path string) (*bundleOutputTarget, error) {
	return prepareBundleOutputTargetWithDependencies(ctx, path, defaultBundleOutputDependencies())
}

func prepareBundleOutputTargetWithDependencies(
	ctx context.Context,
	path string,
	deps bundleOutputDependencies,
) (*bundleOutputTarget, error) {

	deps = normalizeBundleOutputDependencies(deps)
	if err := cliFilesystemContextError(ctx, "bundle output preflight canceled"); err != nil {
		return nil, err
	}
	absPath, absErr := filepath.Abs(path)
	if absErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to resolve bundle output path", absErr)
	}
	absPath = filepath.Clean(absPath)
	if filepath.Dir(absPath) == absPath {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle output may not be the filesystem root")
	}

	ancestorPath, relativePath, outputInfo, inspectErr := inspectPlannedDirectory(ctx, absPath, deps.lstat)
	if inspectErr != nil {
		return nil, inspectErr
	}
	// An existing target must be retained from its parent so a completed
	// private generation stage can replace the target through the parent's
	// anchored Root. Retaining the target itself would make relativePath "."
	// and leave no directory entry that can be atomically renamed.
	if outputInfo != nil && relativePath == "." {
		ancestorPath = filepath.Dir(absPath)
		relativePath = filepath.Base(absPath)
	}
	plannedAncestorInfo, ancestorStatErr := deps.lstat(ancestorPath)
	if ancestorStatErr != nil || !plannedAncestorInfo.IsDir() ||
		plannedAncestorInfo.Mode()&os.ModeSymlink != 0 {

		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to retain planned bundle output ancestor identity", ancestorStatErr)
	}
	if err := deps.beforeAncestorOpen(ancestorPath); err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"bundle output ancestor changed before open")
	}
	ancestor, openErr := deps.openRoot(ancestorPath)
	if openErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to retain bundle output ancestor", openErr)
	}
	ancestorInfo, statErr := ancestor.Stat(".")
	namedAncestor, lstatErr := deps.lstat(ancestorPath)
	if statErr != nil || lstatErr != nil || !ancestorInfo.IsDir() ||
		namedAncestor.Mode()&os.ModeSymlink != 0 || !os.SameFile(ancestorInfo, namedAncestor) {

		primary := errors.Wrap(errors.ErrCodeInternal, "bundle output ancestor changed during open",
			stderrors.Join(statErr, lstatErr))
		return nil, joinInternalCleanup(primary, deps.closeRoot(ancestor), "bundle output ancestor")
	}
	if !os.SameFile(plannedAncestorInfo, ancestorInfo) {
		primary := errors.New(errors.ErrCodeInternal,
			"bundle output ancestor identity changed before open")
		return nil, joinInternalCleanup(primary, deps.closeRoot(ancestor), "bundle output ancestor")
	}

	target := &bundleOutputTarget{
		path:         absPath,
		ancestorPath: ancestorPath,
		relativePath: relativePath,
		ancestor:     ancestor,
		ancestorInfo: plannedAncestorInfo,
		deps:         deps,
	}
	if outputInfo == nil {
		return target, nil
	}
	generated, openErr := deps.rootOpenRoot(ancestor, relativePath)
	if openErr != nil {
		return nil, joinInternalCleanup(
			errors.Wrap(errors.ErrCodeInternal, "failed to retain existing bundle output", openErr),
			deps.closeRoot(ancestor), "bundle output ancestor")
	}
	generatedInfo, generatedStatErr := generated.Stat(".")
	if generatedStatErr != nil || !generatedInfo.IsDir() || !os.SameFile(outputInfo, generatedInfo) {
		var primary error = errors.Wrap(errors.ErrCodeInternal,
			"bundle output changed during preflight", generatedStatErr)
		primary = joinInternalCleanup(primary, deps.closeRoot(generated), "generated bundle output")
		return nil, joinInternalCleanup(primary, deps.closeRoot(ancestor), "bundle output ancestor")
	}
	target.generated = generated
	target.generatedInfo = generatedInfo
	if validateErr := target.validate(ctx); validateErr != nil {
		return nil, joinInternalCleanup(validateErr, target.close(), "bundle output target")
	}
	return target, nil
}

func (t *bundleOutputTarget) newGenerationStage(ctx context.Context) (*bundleGenerationStage, error) {
	if t == nil || t.ancestor == nil {
		return nil, errors.New(errors.ErrCodeInternal, "bundle output target is unavailable")
	}
	if t.relativePath == "." {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"OCI bundle output must be below its retained ancestor")
	}
	if err := t.validateAncestor(ctx); err != nil {
		return nil, err
	}
	name, err := t.deps.randomName()
	if err != nil {
		return nil, errors.PropagateOrWrap(
			err, errors.ErrCodeInternal, "failed to allocate bundle generation stage name")
	}
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, errors.New(errors.ErrCodeInternal, "generated bundle stage name is unsafe")
	}
	stage := &bundleGenerationStage{
		target: t,
		name:   name,
		path:   filepath.Join(t.ancestorPath, name),
	}
	if mkdirErr := t.deps.rootMkdir(t.ancestor, name, 0o700); mkdirErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to create private bundle generation stage", mkdirErr)
	}
	fail := func(primary error) (*bundleGenerationStage, error) {
		return nil, joinInternalCleanup(primary, stage.close(), "bundle generation stage")
	}
	named, err := t.deps.rootLstat(t.ancestor, name)
	if err != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 {
		return fail(bundleFilesystemError(
			"private bundle generation stage changed during creation", err))
	}
	stage.info = named
	stage.root, err = t.deps.rootOpenRoot(t.ancestor, name)
	if err != nil {
		return fail(errors.Wrap(errors.ErrCodeInternal,
			"failed to retain private bundle generation stage", err))
	}
	held, err := stage.root.Stat(".")
	if err != nil || !held.IsDir() || !os.SameFile(named, held) {
		return fail(bundleFilesystemError(
			"private bundle generation stage changed during open", err))
	}
	stage.info = held
	if err := stage.validate(ctx); err != nil {
		return fail(err)
	}
	return stage, nil
}

func (s *bundleGenerationStage) validate(ctx context.Context) error {
	if s == nil || s.target == nil || s.root == nil || s.info == nil || s.name == "" {
		return errors.New(errors.ErrCodeInternal, "bundle generation stage is unavailable")
	}
	if err := s.target.validateAncestor(ctx); err != nil {
		return err
	}
	held, heldErr := s.root.Stat(".")
	named, namedErr := s.target.deps.rootLstat(s.target.ancestor, s.name)
	absolute, absoluteErr := s.target.deps.lstat(s.path)
	if heldErr != nil || namedErr != nil || absoluteErr != nil ||
		named.Mode()&os.ModeSymlink != 0 || absolute.Mode()&os.ModeSymlink != 0 ||
		!held.IsDir() || !named.IsDir() || !absolute.IsDir() ||
		!os.SameFile(s.info, held) || !os.SameFile(s.info, named) || !os.SameFile(s.info, absolute) {

		return bundleFilesystemError(
			"bundle generation stage identity changed", heldErr, namedErr, absoluteErr)
	}
	return cliFilesystemContextError(ctx, "bundle generation stage validation canceled")
}

func (s *bundleGenerationStage) copyInventory(ctx context.Context, inventory *checksum.Inventory) error {
	if inventory == nil {
		return errors.New(errors.ErrCodeInternal, "verified generated bundle inventory is required")
	}
	if err := s.validate(ctx); err != nil {
		return err
	}
	for _, rel := range inventory.RelativeDirectories() {
		if err := cliFilesystemContextError(ctx, "bundle generation copy canceled"); err != nil {
			return err
		}
		if err := s.root.MkdirAll(filepath.FromSlash(rel), 0o755); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to create private bundle generation directory", err)
		}
	}
	for _, rel := range inventory.RelativeFiles() {
		if err := cliFilesystemContextError(ctx, "bundle generation copy canceled"); err != nil {
			return err
		}
		source, err := inventory.Open(ctx, rel)
		if err != nil {
			return errors.PropagateOrWrap(
				err, errors.ErrCodeInternal, "failed to open verified generated bundle file")
		}
		info, statErr := source.Stat()
		if statErr != nil {
			_ = source.Close()
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to inspect verified generated bundle file", statErr)
		}
		if !info.Mode().IsRegular() {
			_ = source.Close()
			return errors.New(errors.ErrCodeInternal,
				"verified generated bundle entry is not a regular file")
		}
		destinationRel := filepath.FromSlash(rel)
		if err := s.root.MkdirAll(filepath.Dir(destinationRel), 0o755); err != nil {
			_ = source.Close()
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to create private bundle generation parent", err)
		}
		destination, createErr := s.root.OpenFile(
			destinationRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if createErr != nil {
			_ = source.Close()
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to create private bundle generation file", createErr)
		}
		copyErr := copyBundleGenerationFile(ctx, destination, source)
		sourceCloseErr := source.Close()
		destinationCloseErr := destination.Close()
		if copyErr != nil {
			return errors.PropagateOrWrap(
				copyErr, errors.ErrCodeInternal, "failed to copy verified generated bundle file")
		}
		if sourceCloseErr != nil || destinationCloseErr != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to close generated bundle file after copy",
				stderrors.Join(sourceCloseErr, destinationCloseErr))
		}
	}
	return s.validate(ctx)
}

func copyBundleGenerationFile(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, bundleGenerationCopyBufferSize)
	for {
		if err := cliFilesystemContextError(ctx, "bundle generation copy canceled"); err != nil {
			return err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			if writeErr != nil {
				return errors.Wrap(errors.ErrCodeInternal,
					"failed to write private bundle generation file", writeErr)
			}
			if written != read {
				return errors.New(errors.ErrCodeInternal,
					"short write while copying private bundle generation file")
			}
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				return nil
			}
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to read verified generated bundle file", readErr)
		}
	}
}

func (s *bundleGenerationStage) publish(ctx context.Context) error {
	if err := s.validate(ctx); err != nil {
		return err
	}
	t := s.target
	if err := t.ensureOutputParent(ctx); err != nil {
		return err
	}
	if t.generated == nil {
		if _, err := t.deps.rootLstat(t.ancestor, t.relativePath); err == nil {
			return errors.New(errors.ErrCodeInternal,
				"bundle output appeared after generation preflight")
		} else if !os.IsNotExist(err) {
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to revalidate absent bundle output", err)
		}
	} else if err := t.validate(ctx); err != nil {
		return err
	}

	backupName := ""
	var backupInfo fs.FileInfo
	if t.generated != nil {
		var err error
		backupName, err = t.deps.randomName()
		if err != nil {
			return errors.PropagateOrWrap(
				err, errors.ErrCodeInternal, "failed to allocate bundle output backup name")
		}
		if backupName == "" || filepath.Base(backupName) != backupName ||
			backupName == "." || backupName == ".." || backupName == s.name {

			return errors.New(errors.ErrCodeInternal, "generated bundle backup name is unsafe")
		}
		if _, inspectErr := t.deps.rootLstat(t.ancestor, backupName); inspectErr == nil {
			return errors.New(errors.ErrCodeInternal, "generated bundle backup name already exists")
		} else if !os.IsNotExist(inspectErr) {
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to inspect generated bundle backup path", inspectErr)
		}
		if renameErr := t.deps.rootRename(t.ancestor, t.relativePath, backupName); renameErr != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to retain previous bundle output for replacement", renameErr)
		}
		backupInfo, err = t.deps.rootLstat(t.ancestor, backupName)
		if err != nil || !os.SameFile(t.generatedInfo, backupInfo) {
			rollbackErr := t.renameRetainedDirectory(
				backupName, t.generatedInfo, t.relativePath,
				"failed to restore previous bundle output")
			primary := bundleFilesystemError(
				"previous bundle output changed during replacement", err)
			return stderrors.Join(primary, rollbackErr)
		}
	}

	if err := t.deps.rootRename(t.ancestor, s.name, t.relativePath); err != nil {
		var rollbackErr error
		if backupName != "" {
			rollbackErr = t.renameRetainedDirectory(
				backupName, backupInfo, t.relativePath,
				"failed to restore previous bundle output")
		}
		return stderrors.Join(errors.Wrap(errors.ErrCodeInternal,
			"failed to publish generated bundle through retained root", err), rollbackErr)
	}
	rollback := func(primary error) error {
		stageErr := t.renameRetainedDirectory(
			t.relativePath, s.info, s.name,
			"failed to restore private bundle generation stage")
		if stageErr != nil {
			return stderrors.Join(primary, stageErr)
		}
		var backupErr error
		if backupName != "" {
			backupErr = t.renameRetainedDirectory(
				backupName, backupInfo, t.relativePath,
				"failed to restore previous bundle output")
		}
		return stderrors.Join(primary, stageErr, backupErr)
	}
	named, namedErr := t.deps.rootLstat(t.ancestor, t.relativePath)
	absolute, absoluteErr := t.deps.lstat(t.path)
	held, heldErr := s.root.Stat(".")
	if namedErr != nil || absoluteErr != nil || heldErr != nil ||
		named.Mode()&os.ModeSymlink != 0 || absolute.Mode()&os.ModeSymlink != 0 ||
		!named.IsDir() || !absolute.IsDir() || !held.IsDir() ||
		!os.SameFile(s.info, named) || !os.SameFile(s.info, absolute) || !os.SameFile(s.info, held) {

		primary := bundleFilesystemError(
			"generated bundle identity changed during publication",
			namedErr, absoluteErr, heldErr)
		return rollback(primary)
	}
	if backupName != "" {
		if err := t.validateRetainedDirectoryName(
			backupName, backupInfo,
			"previous bundle output changed before publication commit"); err != nil {
			return rollback(err)
		}
	}
	if err := cliFilesystemContextError(ctx, "bundle output publication canceled"); err != nil {
		return rollback(err)
	}

	previous := t.generated
	t.generated = s.root
	t.generatedInfo = s.info
	s.root = nil
	s.name = ""
	s.path = ""

	var cleanupErr error
	if previous != nil {
		cleanupErr = stderrors.Join(cleanupErr, t.deps.closeRoot(previous))
	}
	if backupName != "" {
		cleanupErr = stderrors.Join(cleanupErr, t.removeRetainedDirectory(
			backupName, backupInfo, "failed to remove unchanged previous bundle output"))
	}
	if cleanupErr != nil {
		return errors.PropagateOrWrap(cleanupErr, errors.ErrCodeInternal,
			"failed to remove replaced bundle output")
	}
	return nil
}

func (t *bundleOutputTarget) ensureOutputParent(ctx context.Context) error {
	parent := filepath.Dir(t.relativePath)
	if parent == "." {
		return t.validateAncestor(ctx)
	}
	if err := t.ancestor.MkdirAll(parent, 0o755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to create retained bundle output parent", err)
	}
	current := ""
	for _, component := range strings.Split(parent, string(filepath.Separator)) {
		if err := cliFilesystemContextError(ctx, "bundle output parent creation canceled"); err != nil {
			return err
		}
		current = filepath.Join(current, component)
		info, err := t.deps.rootLstat(t.ancestor, current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return bundleFilesystemError(
				"retained bundle output parent is unsafe", err)
		}
	}
	return t.validateAncestor(ctx)
}

func (s *bundleGenerationStage) close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.root != nil {
			held, err := s.root.Stat(".")
			if err != nil || s.info == nil || !held.IsDir() || !os.SameFile(s.info, held) {
				s.closeErr = stderrors.Join(s.closeErr, bundleFilesystemError(
					"private bundle generation stage retained identity changed", err))
			}
			s.closeErr = stderrors.Join(s.closeErr, s.target.deps.closeRoot(s.root))
			s.root = nil
		}
		if s.name != "" {
			s.closeErr = stderrors.Join(s.closeErr, s.target.removeRetainedDirectory(
				s.name, s.info, "private bundle generation stage name changed before cleanup"))
			s.name = ""
		}
		if s.closeErr != nil {
			s.closeErr = errors.PropagateOrWrap(s.closeErr, errors.ErrCodeInternal,
				"failed to clean private bundle generation stage")
		}
	})
	return s.closeErr
}

func inspectPlannedDirectory(
	ctx context.Context,
	absPath string,
	lstat func(string) (fs.FileInfo, error),
) (ancestorPath, relativePath string, outputInfo fs.FileInfo, retErr error) {

	prefixes, prefixErr := absolutePathPrefixes(absPath)
	if prefixErr != nil {
		return "", "", nil, prefixErr
	}
	ancestorPath = prefixes[0]
	for index, prefix := range prefixes {
		if err := cliFilesystemContextError(ctx, "bundle output preflight canceled"); err != nil {
			return "", "", nil, err
		}
		info, lstatErr := lstat(prefix)
		if lstatErr != nil {
			if os.IsNotExist(lstatErr) {
				break
			}
			return "", "", nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"failed to inspect bundle output path", lstatErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", nil, errors.New(errors.ErrCodeInvalidRequest,
				"bundle output path contains a symbolic link")
		}
		if !info.IsDir() {
			return "", "", nil, errors.New(errors.ErrCodeInvalidRequest,
				"bundle output path contains a non-directory component")
		}
		ancestorPath = prefix
		if index == len(prefixes)-1 {
			outputInfo = info
		}
	}
	var relErr error
	relativePath, relErr = filepath.Rel(ancestorPath, absPath)
	if relErr != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return "", "", nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to anchor bundle output path", relErr)
	}
	if relativePath == "" {
		relativePath = "."
	}
	return ancestorPath, relativePath, outputInfo, nil
}

func absolutePathPrefixes(absPath string) ([]string, error) {
	if !filepath.IsAbs(absPath) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "path must be absolute")
	}
	clean := filepath.Clean(absPath)
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	remainder = strings.TrimLeft(remainder, string(filepath.Separator))
	root := volume + string(filepath.Separator)
	if root == "" {
		root = string(filepath.Separator)
	}
	prefixes := []string{root}
	if remainder == "" {
		return prefixes, nil
	}
	current := root
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "path contains an unsafe component")
		}
		current = filepath.Join(current, component)
		prefixes = append(prefixes, current)
	}
	return prefixes, nil
}

func (t *bundleOutputTarget) captureGenerated(ctx context.Context, outDir string) error {
	if t == nil {
		return errors.New(errors.ErrCodeInternal, "bundle output target is required")
	}
	absOut, absErr := filepath.Abs(outDir)
	if absErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to resolve generated bundle output", absErr)
	}
	if filepath.Clean(absOut) != t.path {
		return errors.New(errors.ErrCodeInternal, "generated bundle output differs from planned output")
	}
	if err := t.validateAncestor(ctx); err != nil {
		return err
	}
	namedBefore, beforeErr := t.validateRelativePath(ctx)
	if beforeErr != nil {
		return beforeErr
	}
	if err := t.deps.beforeGeneratedOpen(t); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"generated bundle output changed before open")
	}
	named, pathErr := t.validateRelativePath(ctx)
	if pathErr != nil {
		return pathErr
	}
	if !os.SameFile(namedBefore, named) {
		return errors.New(errors.ErrCodeInternal,
			"generated bundle output changed before retained open")
	}
	if t.generated != nil {
		if !os.SameFile(t.generatedInfo, named) {
			return errors.New(errors.ErrCodeInternal, "generated bundle output identity changed")
		}
		return t.validate(ctx)
	}
	generated, openErr := t.deps.rootOpenRoot(t.ancestor, t.relativePath)
	if openErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to retain generated bundle output", openErr)
	}
	info, statErr := generated.Stat(".")
	if statErr != nil || !info.IsDir() || !os.SameFile(info, named) {
		primary := errors.Wrap(errors.ErrCodeInternal, "generated bundle output changed during open", statErr)
		return joinInternalCleanup(primary, t.deps.closeRoot(generated), "generated bundle output")
	}
	t.generated = generated
	t.generatedInfo = info
	return t.validate(ctx)
}

func (t *bundleOutputTarget) validateAncestor(ctx context.Context) error {
	if err := cliFilesystemContextError(ctx, "bundle output validation canceled"); err != nil {
		return err
	}
	held, heldErr := t.ancestor.Stat(".")
	named, namedErr := t.deps.lstat(t.ancestorPath)
	if heldErr != nil || namedErr != nil || !held.IsDir() ||
		named.Mode()&os.ModeSymlink != 0 || !os.SameFile(t.ancestorInfo, held) ||
		!os.SameFile(t.ancestorInfo, named) {

		return errors.Wrap(errors.ErrCodeInternal, "bundle output ancestor identity changed",
			stderrors.Join(heldErr, namedErr))
	}
	return nil
}

func (t *bundleOutputTarget) validateRelativePath(ctx context.Context) (fs.FileInfo, error) {
	if t.relativePath == "." {
		if err := cliFilesystemContextError(ctx, "bundle output validation canceled"); err != nil {
			return nil, err
		}
		info, err := t.deps.rootLstat(t.ancestor, ".")
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"generated bundle output is unavailable", err)
		}
		return info, nil
	}
	current := ""
	var currentInfo fs.FileInfo
	components := strings.Split(t.relativePath, string(filepath.Separator))
	for _, component := range components {
		if err := cliFilesystemContextError(ctx, "bundle output validation canceled"); err != nil {
			return nil, err
		}
		current = filepath.Join(current, component)
		info, err := t.deps.rootLstat(t.ancestor, current)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "generated bundle output is unavailable", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New(errors.ErrCodeInternal,
				"generated bundle output contains a replaced or unsafe path component")
		}
		currentInfo = info
	}
	return currentInfo, nil
}

func (t *bundleOutputTarget) validate(ctx context.Context) error {
	if t == nil || t.ancestor == nil || t.generated == nil || t.generatedInfo == nil {
		return errors.New(errors.ErrCodeInternal, "generated bundle output has not been retained")
	}
	if err := t.validateAncestor(ctx); err != nil {
		return err
	}
	named, err := t.validateRelativePath(ctx)
	if err != nil {
		return err
	}
	held, heldErr := t.generated.Stat(".")
	absolute, absoluteErr := t.deps.lstat(t.path)
	if heldErr != nil || absoluteErr != nil || named.Mode()&os.ModeSymlink != 0 ||
		absolute.Mode()&os.ModeSymlink != 0 || !held.IsDir() || !named.IsDir() || !absolute.IsDir() ||
		!os.SameFile(t.generatedInfo, held) || !os.SameFile(t.generatedInfo, named) ||
		!os.SameFile(t.generatedInfo, absolute) {

		return errors.Wrap(errors.ErrCodeInternal, "generated bundle output identity changed",
			stderrors.Join(heldErr, absoluteErr))
	}
	return cliFilesystemContextError(ctx, "bundle output validation canceled")
}

type bundleEntryInfos struct {
	directories  []fs.FileInfo
	regularFiles []fs.FileInfo
}

func (t *bundleOutputTarget) entryInfos(ctx context.Context) (*bundleEntryInfos, error) {
	if err := t.validate(ctx); err != nil {
		return nil, err
	}
	infos := &bundleEntryInfos{}
	err := fs.WalkDir(t.generated.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to inspect generated bundle", walkErr)
		}
		if err := cliFilesystemContextError(ctx, "generated bundle scan canceled"); err != nil {
			return err
		}
		info, err := t.deps.rootLstat(t.generated, path)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to inspect generated bundle entry", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New(errors.ErrCodeInternal, "generated bundle contains a symlink or special object")
		}
		if info.IsDir() {
			infos.directories = append(infos.directories, info)
		} else {
			infos.regularFiles = append(infos.regularFiles, info)
		}
		return nil
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to scan generated bundle")
	}
	if err := t.validate(ctx); err != nil {
		return nil, err
	}
	return infos, nil
}

func (t *bundleOutputTarget) regularFileInfos(ctx context.Context) ([]fs.FileInfo, error) {
	infos, err := t.entryInfos(ctx)
	if err != nil {
		return nil, err
	}
	return infos.regularFiles, nil
}

func (t *bundleOutputTarget) close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		if t.generated != nil {
			t.closeErr = stderrors.Join(t.closeErr, t.deps.closeRoot(t.generated))
		}
		if t.ancestor != nil {
			t.closeErr = stderrors.Join(t.closeErr, t.deps.closeRoot(t.ancestor))
		}
		if t.closeErr != nil {
			t.closeErr = errors.PropagateOrWrap(t.closeErr, errors.ErrCodeInternal,
				"failed to close bundle output target")
		}
	})
	return t.closeErr
}
