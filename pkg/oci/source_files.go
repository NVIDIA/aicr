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

package oci

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/opencontainers/go-digest"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

const (
	workspacePrefix = "aicr-oci-source-"
	layoutPrefix    = "oci-layout-"
	copyBufferSize  = 128 * 1024
)

type rootedDirectory struct {
	path     string
	resolved string
	root     *os.Root
	info     fs.FileInfo
}

func openRootedDirectoryWithClose(
	ctx context.Context,
	dir string,
	code apperrors.ErrorCode,
	beforeOpen func(string) error,
	closeRoot func(*os.Root) error,
) (_ *rootedDirectory, retErr error) {

	if err := contextError(ctx, "directory validation canceled"); err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, apperrors.New(code, "directory path must not be empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, apperrors.Wrap(code, "failed to resolve directory path", err)
	}
	abs = filepath.Clean(abs)
	before, err := os.Lstat(abs)
	if err != nil {
		return nil, apperrors.Wrap(code, "failed to inspect directory", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, apperrors.New(code, "directory must be an existing real directory")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, apperrors.Wrap(code, "failed to resolve directory topology", err)
	}
	if beforeOpen != nil {
		if beforeOpenErr := beforeOpen(abs); beforeOpenErr != nil {
			return nil, apperrors.Wrap(code, "directory pre-open check failed", beforeOpenErr)
		}
	}
	if ctxErr := contextError(ctx, "directory validation canceled"); ctxErr != nil {
		return nil, ctxErr
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, apperrors.Wrap(code, "failed to open directory root", err)
	}
	keepRoot := false
	defer func() {
		if keepRoot {
			return
		}
		if closeErr := closeRoot(root); closeErr != nil {
			retErr = stderrors.Join(retErr,
				apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close rejected directory root", closeErr))
		}
	}()
	descriptorInfo, descriptorErr := root.Stat(".")
	after, afterErr := os.Lstat(abs)
	if descriptorErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, descriptorInfo) || !os.SameFile(before, after) {

		return nil, apperrors.Wrap(code, "directory identity changed during open",
			stderrors.Join(descriptorErr, afterErr))
	}
	resolvedInfo, resolvedErr := os.Lstat(resolved)
	if resolvedErr != nil || resolvedInfo.Mode()&os.ModeSymlink != 0 ||
		!resolvedInfo.IsDir() || !os.SameFile(descriptorInfo, resolvedInfo) {

		return nil, apperrors.Wrap(code, "canonical directory identity changed during open", resolvedErr)
	}
	keepRoot = true
	return &rootedDirectory{path: abs, resolved: resolved, root: root, info: descriptorInfo}, nil
}

func (r *rootedDirectory) validate(code apperrors.ErrorCode) error {
	if r == nil || r.root == nil {
		return apperrors.New(code, "directory root is unavailable")
	}
	held, heldErr := r.root.Stat(".")
	named, namedErr := os.Lstat(r.path)           //nolint:gosec // retained canonical root identity
	resolved, resolvedErr := os.Lstat(r.resolved) //nolint:gosec // retained canonical root identity
	if heldErr != nil || namedErr != nil || resolvedErr != nil ||
		named.Mode()&os.ModeSymlink != 0 || resolved.Mode()&os.ModeSymlink != 0 ||
		!named.IsDir() || !resolved.IsDir() ||
		!os.SameFile(r.info, held) || !os.SameFile(r.info, named) || !os.SameFile(r.info, resolved) {

		return apperrors.Wrap(code, "directory identity changed",
			stderrors.Join(heldErr, namedErr, resolvedErr))
	}
	return nil
}

func (r *rootedDirectory) close(
	label string,
	closeRoot func(*os.Root) error,
) error {

	if r == nil || r.root == nil {
		return nil
	}
	if err := closeRoot(r.root); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close "+label, err)
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	return sameOrBelow(a, b) || sameOrBelow(b, a)
}

func sameOrBelow(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	return sameOrBelowExact(child, parent) ||
		sameOrBelowExact(strings.ToLower(child), strings.ToLower(parent))
}

func sameOrBelowExact(child, parent string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type workspaceDependencies struct {
	beforeTempOpen        func(string) error
	beforeChildRevalidate func(parent, child string) error
	beforeRemove          func(parent, child string) error
	childLstat            func(*os.Root, string) (fs.FileInfo, error)
	openChild             func(*os.Root, string) (*os.Root, error)
	chmodChild            func(*os.Root) error
	removeAll             func(*os.Root, string) error
	closeRoot             func(*os.Root) error
}

func defaultWorkspaceDependencies() workspaceDependencies {
	return workspaceDependencies{
		childLstat: func(root *os.Root, name string) (fs.FileInfo, error) {
			return root.Lstat(name)
		},
		openChild: func(root *os.Root, name string) (*os.Root, error) {
			return root.OpenRoot(name)
		},
		chmodChild: func(root *os.Root) error { return root.Chmod(".", 0o700) },
		removeAll:  func(root *os.Root, name string) error { return root.RemoveAll(name) },
		closeRoot:  func(root *os.Root) error { return root.Close() },
	}
}

func normalizeWorkspaceDependencies(deps workspaceDependencies) workspaceDependencies {
	defaults := defaultWorkspaceDependencies()
	if deps.childLstat == nil {
		deps.childLstat = defaults.childLstat
	}
	if deps.openChild == nil {
		deps.openChild = defaults.openChild
	}
	if deps.chmodChild == nil {
		deps.chmodChild = defaults.chmodChild
	}
	if deps.removeAll == nil {
		deps.removeAll = defaults.removeAll
	}
	if deps.closeRoot == nil {
		deps.closeRoot = defaults.closeRoot
	}
	return deps
}

type childAllocationDependencies struct {
	childLstat func(*os.Root, string) (fs.FileInfo, error)
	openChild  func(*os.Root, string) (*os.Root, error)
	chmodChild func(*os.Root) error
	removeAll  func(*os.Root, string) error
	closeRoot  func(*os.Root) error
}

type anchoredChild struct {
	name string
	root *os.Root
	info fs.FileInfo
}

type anchoredChildAllocation struct {
	parent *rootedDirectory
	child  anchoredChild
	deps   childAllocationDependencies
	owned  bool
	opened bool
	label  string
}

func allocateAnchoredChild(
	ctx context.Context,
	parent *rootedDirectory,
	prefix, label string,
	beforeRevalidate func(parent, child string) error,
	deps childAllocationDependencies,
) (*anchoredChild, error) {

	if err := parent.validate(apperrors.ErrCodeInternal); err != nil {
		return nil, err
	}
	name, err := createUniqueDirectory(ctx, parent.root, prefix)
	if err != nil {
		return nil, err
	}
	allocation := &anchoredChildAllocation{
		parent: parent,
		child:  anchoredChild{name: name},
		deps:   deps,
		owned:  true,
		label:  label,
	}
	fail := func(primary error) (*anchoredChild, error) {
		return nil, allocation.fail(primary)
	}

	initial, err := parent.root.Lstat(name)
	if err != nil {
		return fail(apperrors.Wrap(apperrors.ErrCodeInternal, "failed to inspect "+label, err))
	}
	allocation.child.info = initial
	if !initial.IsDir() || initial.Mode()&os.ModeSymlink != 0 {
		return fail(apperrors.New(apperrors.ErrCodeInternal, label+" is not a real directory"))
	}
	observed, err := deps.childLstat(parent.root, name)
	if err != nil || !observed.IsDir() || observed.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(initial, observed) {

		return fail(apperrors.Wrap(
			apperrors.ErrCodeInternal, label+" changed during initial inspection", err))
	}
	childRoot, err := deps.openChild(parent.root, name)
	if err != nil {
		return fail(apperrors.Wrap(apperrors.ErrCodeInternal, "failed to open "+label, err))
	}
	allocation.child.root = childRoot
	allocation.opened = true
	held, err := childRoot.Stat(".")
	if err != nil || !held.IsDir() || !os.SameFile(initial, held) {
		return fail(apperrors.Wrap(apperrors.ErrCodeInternal, label+" changed while opening", err))
	}
	if err := deps.chmodChild(childRoot); err != nil {
		return fail(apperrors.Wrap(apperrors.ErrCodeInternal, "failed to secure "+label, err))
	}
	if beforeRevalidate != nil {
		if err := beforeRevalidate(parent.path, name); err != nil {
			return fail(apperrors.Wrap(apperrors.ErrCodeInternal, label+" revalidation hook failed", err))
		}
	}
	if err := contextError(ctx, label+" allocation canceled"); err != nil {
		return fail(err)
	}
	if err := parent.validate(apperrors.ErrCodeInternal); err != nil {
		return fail(err)
	}
	held, heldErr := childRoot.Stat(".")
	named, namedErr := deps.childLstat(parent.root, name)
	if heldErr != nil || namedErr != nil || !held.IsDir() || !named.IsDir() ||
		named.Mode()&os.ModeSymlink != 0 || held.Mode().Perm() != 0o700 ||
		!os.SameFile(initial, held) || !os.SameFile(initial, named) {

		return fail(apperrors.Wrap(apperrors.ErrCodeInternal, label+" identity changed after creation",
			stderrors.Join(heldErr, namedErr)))
	}
	allocation.owned = false
	allocation.opened = false
	result := allocation.child
	return &result, nil
}

func (a *anchoredChildAllocation) fail(primary error) error {
	var cleanupErrors []error
	if a.opened {
		if err := a.deps.closeRoot(a.child.root); err != nil {
			cleanupErrors = append(cleanupErrors,
				apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close rejected "+a.label, err))
		}
		a.opened = false
	}
	if a.owned {
		remove := a.child.info == nil
		if !remove {
			if err := a.parent.validate(apperrors.ErrCodeInternal); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			} else {
				named, err := a.deps.childLstat(a.parent.root, a.child.name)
				switch {
				case os.IsNotExist(err):
					a.owned = false
				case err != nil:
					cleanupErrors = append(cleanupErrors, apperrors.Wrap(
						apperrors.ErrCodeInternal, "failed to inspect rejected "+a.label, err))
				case !named.IsDir() || named.Mode()&os.ModeSymlink != 0 ||
					!os.SameFile(a.child.info, named):

					cleanupErrors = append(cleanupErrors, apperrors.New(
						apperrors.ErrCodeInternal, "rejected "+a.label+" identity changed before cleanup"))
				default:
					remove = true
				}
			}
		}
		if remove {
			if err := a.deps.removeAll(a.parent.root, a.child.name); err != nil {
				cleanupErrors = append(cleanupErrors,
					apperrors.Wrap(apperrors.ErrCodeInternal, "failed to remove rejected "+a.label, err))
			}
			a.owned = false
		}
	}
	return stderrors.Join(append([]error{primary}, cleanupErrors...)...)
}

// Workspace owns a unique private directory beneath the configured temporary
// root. Cleanup is anchored to the retained parent root and is checked.
type Workspace struct {
	path       string
	parentPath string
	parentReal string
	childName  string
	parent     *os.Root
	child      *os.Root
	parentInfo fs.FileInfo
	childInfo  fs.FileInfo
	deps       workspaceDependencies
	closeOnce  sync.Once
	closeErr   error
}

// NewPrivateWorkspace creates a unique 0700 workspace outside excludedRoots.
func NewPrivateWorkspace(ctx context.Context, prefix string, excludedRoots ...string) (*Workspace, error) {
	return newPrivateWorkspaceWithDependencies(ctx, prefix, defaultWorkspaceDependencies(), excludedRoots...)
}

func newPrivateWorkspaceWithDependencies(
	ctx context.Context,
	prefix string,
	deps workspaceDependencies,
	excludedRoots ...string,
) (*Workspace, error) {

	deps = normalizeWorkspaceDependencies(deps)

	if !validWorkspacePrefix(prefix) {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "workspace prefix is empty or unsafe")
	}
	excluded := make([]*rootedDirectory, 0, len(excludedRoots))
	for _, excludedPath := range excludedRoots {
		root, err := openRootedDirectoryWithClose(
			ctx, excludedPath, apperrors.ErrCodeInvalidRequest, nil, deps.closeRoot)
		if err != nil {
			return nil, stderrors.Join(err, closeRootedDirectories(excluded, deps.closeRoot))
		}
		excluded = append(excluded, root)
	}

	temp, tempOpenErr := openRootedDirectoryWithClose(
		ctx, os.TempDir(), apperrors.ErrCodeInternal, deps.beforeTempOpen, deps.closeRoot)
	if tempOpenErr != nil {
		return nil, stderrors.Join(tempOpenErr, closeRootedDirectories(excluded, deps.closeRoot))
	}
	for _, root := range excluded {
		if sameOrBelow(temp.path, root.path) || sameOrBelow(temp.resolved, root.resolved) || os.SameFile(temp.info, root.info) {
			primary := apperrors.New(apperrors.ErrCodeInternal,
				"configured temporary root overlaps an excluded root")
			return nil, stderrors.Join(primary,
				closeRootedDirectories(excluded, deps.closeRoot),
				temp.close("configured temporary root", deps.closeRoot))
		}
	}
	if validateErr := temp.validate(apperrors.ErrCodeInternal); validateErr != nil {
		return nil, stderrors.Join(validateErr,
			closeRootedDirectories(excluded, deps.closeRoot),
			temp.close("configured temporary root", deps.closeRoot))
	}
	if excludedCloseErr := closeRootedDirectories(excluded, deps.closeRoot); excludedCloseErr != nil {
		return nil, stderrors.Join(excludedCloseErr,
			temp.close("configured temporary root", deps.closeRoot))
	}
	allocated, allocationErr := allocateAnchoredChild(ctx, temp, prefix, "workspace", deps.beforeChildRevalidate,
		childAllocationDependencies{
			childLstat: deps.childLstat,
			openChild:  deps.openChild,
			chmodChild: deps.chmodChild,
			removeAll:  deps.removeAll,
			closeRoot:  deps.closeRoot,
		})
	if allocationErr != nil {
		return nil, stderrors.Join(allocationErr,
			temp.close("configured temporary root", deps.closeRoot))
	}
	return &Workspace{
		path:       filepath.Join(temp.path, allocated.name),
		parentPath: temp.path,
		parentReal: temp.resolved,
		childName:  allocated.name,
		parent:     temp.root,
		child:      allocated.root,
		parentInfo: temp.info,
		childInfo:  allocated.info,
		deps:       deps,
	}, nil
}

func validWorkspacePrefix(prefix string) bool {
	if prefix == "" || filepath.Base(prefix) != prefix || prefix == "." || prefix == ".." {
		return false
	}
	for _, r := range prefix {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func createUniqueDirectory(ctx context.Context, root *os.Root, prefix string) (string, error) {
	for range 128 {
		if err := contextError(ctx, "workspace creation canceled"); err != nil {
			return "", err
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to generate workspace name", err)
		}
		name := prefix + hex.EncodeToString(random[:])
		if err := root.Mkdir(name, 0o700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create workspace", err)
		}
		return name, nil
	}
	return "", apperrors.New(apperrors.ErrCodeInternal, "failed to allocate a unique workspace")
}

func closeRootedDirectories(
	roots []*rootedDirectory,
	closeRoot func(*os.Root) error,
) error {

	var errs []error
	for _, root := range roots {
		if err := root.close("excluded workspace root", closeRoot); err != nil {
			errs = append(errs, err)
		}
	}
	return stderrors.Join(errs...)
}

// Path returns the workspace path.
func (w *Workspace) Path() string { return w.path }

func (w *Workspace) validate() error {
	parentHeld, parentHeldErr := w.parent.Stat(".")
	parentNamed, parentNamedErr := os.Lstat(w.parentPath) //nolint:gosec // retained canonical parent identity
	parentReal, parentRealErr := os.Lstat(w.parentReal)   //nolint:gosec // retained canonical parent identity
	childHeld, childHeldErr := w.child.Stat(".")
	named, namedErr := w.parent.Lstat(w.childName)
	if parentHeldErr != nil || parentNamedErr != nil || parentRealErr != nil ||
		childHeldErr != nil || namedErr != nil || parentNamed.Mode()&os.ModeSymlink != 0 ||
		parentReal.Mode()&os.ModeSymlink != 0 || named.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(w.parentInfo, parentHeld) || !os.SameFile(w.parentInfo, parentNamed) ||
		!os.SameFile(w.parentInfo, parentReal) ||
		!os.SameFile(w.childInfo, childHeld) || !os.SameFile(w.childInfo, named) {

		return apperrors.Wrap(apperrors.ErrCodeInternal, "workspace identity changed",
			stderrors.Join(parentHeldErr, parentNamedErr, parentRealErr, childHeldErr, namedErr))
	}
	return nil
}

// Close removes the unchanged workspace and returns a cached checked result.
func (w *Workspace) Close() error {
	w.closeOnce.Do(func() {
		stable := true
		if err := w.validate(); err != nil {
			w.closeErr = stderrors.Join(w.closeErr, err)
			stable = false
		}
		if w.deps.beforeRemove != nil {
			w.closeErr = stderrors.Join(w.closeErr, w.deps.beforeRemove(w.parentPath, w.childName))
		}
		if err := w.validate(); err != nil {
			w.closeErr = stderrors.Join(w.closeErr, err)
			stable = false
		}
		w.closeErr = stderrors.Join(w.closeErr, w.deps.closeRoot(w.child))
		if stable {
			parentHeld, parentHeldErr := w.parent.Stat(".")
			parentNamed, parentNamedErr := os.Lstat(w.parentPath) //nolint:gosec // retained canonical parent identity
			named, namedErr := w.parent.Lstat(w.childName)
			if parentHeldErr != nil || parentNamedErr != nil || namedErr != nil ||
				!os.SameFile(w.parentInfo, parentHeld) || !os.SameFile(w.parentInfo, parentNamed) ||
				!os.SameFile(w.childInfo, named) {

				stable = false
				w.closeErr = stderrors.Join(w.closeErr, apperrors.Wrap(
					apperrors.ErrCodeInternal, "workspace identity changed before removal",
					stderrors.Join(parentHeldErr, parentNamedErr, namedErr)))
			}
		}
		if stable {
			w.closeErr = stderrors.Join(w.closeErr, w.deps.removeAll(w.parent, w.childName))
		}
		w.closeErr = stderrors.Join(w.closeErr, w.deps.closeRoot(w.parent))
		if w.closeErr != nil {
			w.closeErr = apperrors.PropagateOrWrap(w.closeErr, apperrors.ErrCodeInternal,
				"failed to close private workspace")
		}
	})
	return w.closeErr
}

type prepareSourceDependencies struct {
	beforeSourceOpen     func(string) error
	beforeOutputOpen     func(string) error
	beforeFileOpen       func(string) error
	beforeFileRevalidate func(string) error
	newWorkspace         func(context.Context, string, ...string) (*Workspace, error)
	openStageRoot        func(*os.Root) (*os.Root, error)
	copy                 func(context.Context, io.Writer, io.Reader) (int64, error)
	closeRoot            func(*os.Root) error
}

func defaultPrepareSourceDependencies() prepareSourceDependencies {
	return prepareSourceDependencies{
		newWorkspace:  NewPrivateWorkspace,
		openStageRoot: func(root *os.Root) (*os.Root, error) { return root.OpenRoot(".") },
		copy:          copyWithContext,
		closeRoot:     func(root *os.Root) error { return root.Close() },
	}
}

func normalizePrepareSourceDependencies(deps prepareSourceDependencies) prepareSourceDependencies {
	defaults := defaultPrepareSourceDependencies()
	if deps.newWorkspace == nil {
		deps.newWorkspace = defaults.newWorkspace
	}
	if deps.openStageRoot == nil {
		deps.openStageRoot = defaults.openStageRoot
	}
	if deps.copy == nil {
		deps.copy = defaults.copy
	}
	if deps.closeRoot == nil {
		deps.closeRoot = defaults.closeRoot
	}
	return deps
}

type preparedSource struct {
	workspace *Workspace
	auxiliary *Workspace
	source    *rootedDirectory
	output    *rootedDirectory
	dir       string
	files     []string
	metadata  map[string]stagedFileMetadata
	root      *os.Root
	rootInfo  fs.FileInfo
	closeRoot func(*os.Root) error
	closeOnce sync.Once
	closeErr  error
}

type stagedFileMetadata struct {
	digest digest.Digest
	size   int64
	mode   fs.FileMode
}

type verifiedStagedFileDependencies struct {
	newReader                func(context.Context, *os.File) *contextReadCloser
	beforeMetadataRevalidate func() error
}

func defaultVerifiedStagedFileDependencies() verifiedStagedFileDependencies {
	return verifiedStagedFileDependencies{
		newReader: func(ctx context.Context, file *os.File) *contextReadCloser {
			return newContextReadCloser(ctx, file)
		},
	}
}

type verifiedStagedFile struct {
	ctx      context.Context
	source   *preparedSource
	file     *os.File
	reader   *contextReadCloser
	rel      string
	identity fs.FileInfo
	info     fs.FileInfo
	metadata stagedFileMetadata
	digester digest.Digester
	deps     verifiedStagedFileDependencies

	mu              sync.Mutex
	size            int64
	verified        bool
	verificationErr error
	closed          bool
	closeErr        error
}

func (f *verifiedStagedFile) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, os.ErrClosed
	}
	return f.info, nil
}

func (f *verifiedStagedFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readLocked(p)
}

func (f *verifiedStagedFile) readLocked(p []byte) (int, error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	if f.verificationErr != nil {
		return 0, f.verificationErr
	}
	if err := contextError(f.ctx, "staged source read canceled"); err != nil {
		f.verificationErr = err
		return 0, err
	}
	n, readErr := f.reader.Read(p)
	if n > 0 {
		written, hashErr := f.digester.Hash().Write(p[:n])
		f.size += int64(written)
		if hashErr != nil || written != n {
			f.verificationErr = apperrors.Wrap(
				apperrors.ErrCodeInternal, "failed to hash consumed staged source bytes", hashErr)
			return n, f.verificationErr
		}
		if f.size > f.metadata.size {
			f.verificationErr = apperrors.New(
				apperrors.ErrCodeInternal, "consumed staged source size exceeds retained size")
			return n, f.verificationErr
		}
	}
	if err := contextError(f.ctx, "staged source read canceled"); err != nil {
		f.verificationErr = err
		return n, err
	}
	if stderrors.Is(readErr, io.EOF) {
		if err := f.verifyLocked(); err != nil {
			return n, err
		}
		return n, io.EOF
	}
	if readErr != nil {
		f.verificationErr = apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to read staged source file", readErr)
		return n, f.verificationErr
	}
	return n, nil
}

func (f *verifiedStagedFile) verifyLocked() error {
	if f.verified {
		return f.verificationErr
	}
	f.verified = true
	if err := contextError(f.ctx, "staged source verification canceled"); err != nil {
		f.verificationErr = err
		return err
	}
	if f.size != f.metadata.size || f.digester.Digest() != f.metadata.digest {
		f.verificationErr = apperrors.New(
			apperrors.ErrCodeInternal, "consumed staged source content does not match retained digest")
		return f.verificationErr
	}
	if f.deps.beforeMetadataRevalidate != nil {
		if err := f.deps.beforeMetadataRevalidate(); err != nil {
			if ctxErr := contextError(f.ctx, "staged source metadata revalidation canceled"); ctxErr != nil {
				f.verificationErr = ctxErr
				return ctxErr
			}
			f.verificationErr = apperrors.PropagateOrWrap(
				err, apperrors.ErrCodeInternal, "staged source metadata revalidation precondition failed")
			return f.verificationErr
		}
	}
	afterPath, afterPathErr := lstatComponents(f.source.root, f.rel)
	afterFD, afterFDErr := f.file.Stat()
	if err := contextError(f.ctx, "staged source metadata revalidation canceled"); err != nil {
		f.verificationErr = err
		return err
	}
	if afterPathErr != nil || afterFDErr != nil ||
		!os.SameFile(f.identity, afterPath) || !os.SameFile(f.identity, afterFD) ||
		!afterFD.Mode().IsRegular() || afterFD.Mode().Perm() != f.metadata.mode ||
		afterFD.Size() != f.metadata.size {

		f.verificationErr = apperrors.Wrap(
			apperrors.ErrCodeInternal, "staged source file changed while being consumed",
			stderrors.Join(afterPathErr, afterFDErr))
		return f.verificationErr
	}
	if err := f.source.validate(); err != nil {
		if ctxErr := contextError(f.ctx, "staged source verification canceled"); ctxErr != nil {
			f.verificationErr = ctxErr
			return ctxErr
		}
		f.verificationErr = err
		return err
	}
	if err := contextError(f.ctx, "staged source verification canceled"); err != nil {
		f.verificationErr = err
		return err
	}
	return nil
}

func (f *verifiedStagedFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.closeErr
	}
	if !f.verified && f.verificationErr == nil {
		if err := contextError(f.ctx, "staged source close canceled"); err != nil {
			f.verificationErr = err
		} else {
			f.verificationErr = apperrors.New(
				apperrors.ErrCodeInternal, "staged source file closed before EOF verification")
		}
	}
	closeErr := f.reader.Close()
	if closeErr != nil {
		closeErr = apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to close verified staged source file", closeErr)
	}
	f.closed = true
	f.closeErr = stderrors.Join(f.verificationErr, closeErr)
	return f.closeErr
}

func preparePackageSource(
	ctx context.Context,
	sourceDir, outputDir, subDir string,
	sourceFiles []string,
) (*preparedSource, error) {

	return preparePackageSourceWithDependencies(
		ctx, sourceDir, outputDir, subDir, sourceFiles, defaultPrepareSourceDependencies())
}

func preparePackageSourceWithDependencies(
	ctx context.Context,
	sourceDir, outputDir, subDir string,
	sourceFiles []string,
	deps prepareSourceDependencies,
) (_ *preparedSource, retErr error) {

	deps = normalizePrepareSourceDependencies(deps)

	if err := contextError(ctx, "source preparation canceled"); err != nil {
		return nil, err
	}
	if sourceFiles != nil && subDir != "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"SubDir is valid only when SourceFiles is nil")
	}
	if sourceFiles != nil && len(sourceFiles) == 0 {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "SourceFiles must not be empty")
	}

	source, sourceOpenErr := openRootedDirectoryWithClose(
		ctx, sourceDir, apperrors.ErrCodeInvalidRequest, deps.beforeSourceOpen, deps.closeRoot)
	if sourceOpenErr != nil {
		return nil, sourceOpenErr
	}
	output, outputOpenErr := openRootedDirectoryWithClose(
		ctx, outputDir, apperrors.ErrCodeInvalidRequest, deps.beforeOutputOpen, deps.closeRoot)
	if outputOpenErr != nil {
		return nil, stderrors.Join(outputOpenErr,
			source.close("source root after output validation failure", deps.closeRoot))
	}
	fail := func(primary error, workspace *Workspace, stagedRoot *os.Root) (*preparedSource, error) {
		var cleanupErrors []error
		if stagedRoot != nil {
			if closeErr := deps.closeRoot(stagedRoot); closeErr != nil {
				cleanupErrors = append(cleanupErrors, apperrors.Wrap(
					apperrors.ErrCodeInternal, "failed to close rejected staged source root", closeErr))
			}
		}
		if workspace != nil {
			if closeErr := workspace.Close(); closeErr != nil {
				cleanupErrors = append(cleanupErrors, closeErr)
			}
		}
		if closeErr := source.close("rejected source root", deps.closeRoot); closeErr != nil {
			cleanupErrors = append(cleanupErrors, closeErr)
		}
		if closeErr := output.close("rejected output root", deps.closeRoot); closeErr != nil {
			cleanupErrors = append(cleanupErrors, closeErr)
		}
		return nil, stderrors.Join(append([]error{primary}, cleanupErrors...)...)
	}
	if pathsOverlap(source.path, output.path) || pathsOverlap(source.resolved, output.resolved) || os.SameFile(source.info, output.info) {
		return fail(apperrors.New(apperrors.ErrCodeInvalidRequest,
			"source and output directories must not overlap"), nil, nil)
	}
	validateCallerRoots := func() error {
		if sourceErr := source.validate(apperrors.ErrCodeInvalidRequest); sourceErr != nil {
			return sourceErr
		}
		return output.validate(apperrors.ErrCodeInvalidRequest)
	}
	if err := validateCallerRoots(); err != nil {
		return fail(err, nil, nil)
	}

	selection, selectionErr := selectSourceFiles(ctx, source.root, subDir, sourceFiles)
	if selectionErr != nil {
		return fail(selectionErr, nil, nil)
	}
	if err := validateCallerRoots(); err != nil {
		return fail(err, nil, nil)
	}
	workspace, workspaceErr := deps.newWorkspace(ctx, workspacePrefix, source.path, output.path)
	if workspaceErr != nil {
		return fail(workspaceErr, nil, nil)
	}
	if err := validateCallerRoots(); err != nil {
		return fail(err, workspace, nil)
	}
	if err := workspace.validate(); err != nil {
		return fail(err, workspace, nil)
	}
	stagedRoot, stageOpenErr := deps.openStageRoot(workspace.child)
	if stageOpenErr != nil {
		return fail(apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to duplicate staged source root", stageOpenErr), workspace, nil)
	}
	stagedInfo, stageStatErr := stagedRoot.Stat(".")
	if stageStatErr != nil || !os.SameFile(workspace.childInfo, stagedInfo) {
		return fail(apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to verify staged source root", stageStatErr), workspace, stagedRoot)
	}

	metadata := make(map[string]stagedFileMetadata, len(selection))
	for _, rel := range selection {
		if err := validateCallerRoots(); err != nil {
			return fail(err, workspace, stagedRoot)
		}
		if err := workspace.validate(); err != nil {
			return fail(err, workspace, stagedRoot)
		}
		fileMetadata, copyErr := copySelectedFile(ctx, source.root, stagedRoot, rel, deps)
		if copyErr != nil {
			return fail(copyErr, workspace, stagedRoot)
		}
		metadata[rel] = fileMetadata
		if err := validateCallerRoots(); err != nil {
			return fail(err, workspace, stagedRoot)
		}
		if err := workspace.validate(); err != nil {
			return fail(err, workspace, stagedRoot)
		}
	}
	prepared := &preparedSource{
		workspace: workspace,
		source:    source,
		output:    output,
		dir:       workspace.Path(),
		files:     append([]string(nil), selection...),
		metadata:  metadata,
		root:      stagedRoot,
		rootInfo:  stagedInfo,
		closeRoot: deps.closeRoot,
	}
	if err := prepared.validate(); err != nil {
		return fail(err, workspace, stagedRoot)
	}
	return prepared, nil
}

func selectSourceFiles(ctx context.Context, root *os.Root, subDir string, sourceFiles []string) ([]string, error) {
	if sourceFiles != nil {
		selected := append([]string(nil), sourceFiles...)
		if err := validateSelectedPaths(selected); err != nil {
			return nil, err
		}
		sort.Strings(selected)
		return selected, nil
	}
	start := "."
	if subDir != "" {
		if err := validateSelectedPath(subDir); err != nil {
			return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest, "invalid SubDir", err)
		}
		info, err := lstatComponents(root, subDir)
		if err != nil || !info.IsDir() {
			return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
				"SubDir must name a real directory", err)
		}
		start = subDir
	}
	var files []string
	err := fs.WalkDir(root.FS(), start, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." || name == start {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return apperrors.New(apperrors.ErrCodeInvalidRequest, "source discovery found a symlink: "+name)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return apperrors.New(apperrors.ErrCodeInvalidRequest, "source discovery found a non-regular file: "+name)
		}
		files = append(files, filepath.ToSlash(name))
		return nil
	})
	if err != nil {
		if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
			return nil, contextError(ctx, "source discovery canceled")
		}
		return nil, apperrors.PropagateOrWrap(err, apperrors.ErrCodeInvalidRequest, "failed to discover source files")
	}
	sort.Strings(files)
	return files, nil
}

func validateSelectedPaths(paths []string) error {
	seen := make(map[string]struct{}, len(paths))
	for _, rel := range paths {
		if err := validateSelectedPath(rel); err != nil {
			return err
		}
		key := strings.ToLower(rel)
		if _, ok := seen[key]; ok {
			return apperrors.New(apperrors.ErrCodeInvalidRequest, "SourceFiles contains a duplicate path: "+rel)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateSelectedPath(rel string) error {
	if rel == "" || rel == "." || !fs.ValidPath(rel) || path.Clean(rel) != rel ||
		filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || strings.Contains(rel, "\\") {

		return apperrors.New(apperrors.ErrCodeInvalidRequest, "source path is not canonical slash-relative: "+rel)
	}
	for _, r := range rel {
		if unicode.IsControl(r) {
			return apperrors.New(apperrors.ErrCodeInvalidRequest, "source path contains a control character")
		}
	}
	return nil
}

func lstatComponents(root *os.Root, rel string) (fs.FileInfo, error) {
	parts := strings.Split(rel, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "source path contains a symlink: "+name)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "source path parent is not a directory: "+name)
		}
		if i == len(parts)-1 {
			return info, nil
		}
	}
	return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "source path is empty")
}

func copySelectedFile(
	ctx context.Context,
	source, staged *os.Root,
	rel string,
	deps prepareSourceDependencies,
) (_ stagedFileMetadata, retErr error) {

	if err := contextError(ctx, "source copy canceled"); err != nil {
		return stagedFileMetadata{}, err
	}
	before, err := lstatComponents(source, rel)
	if err != nil || !before.Mode().IsRegular() {
		return stagedFileMetadata{}, apperrors.Wrap(
			apperrors.ErrCodeInvalidRequest, "selected source is not a regular file", err)
	}
	if deps.beforeFileOpen != nil {
		if beforeOpenErr := deps.beforeFileOpen(rel); beforeOpenErr != nil {
			return stagedFileMetadata{}, apperrors.Wrap(
				apperrors.ErrCodeInvalidRequest, "source file pre-open check failed", beforeOpenErr)
		}
	}
	if ctxErr := contextError(ctx, "source copy canceled"); ctxErr != nil {
		return stagedFileMetadata{}, ctxErr
	}
	in, err := source.Open(rel)
	if err != nil {
		return stagedFileMetadata{}, apperrors.Wrap(
			apperrors.ErrCodeInvalidRequest, "failed to open selected source", err)
	}
	defer func() {
		if closeErr := in.Close(); closeErr != nil {
			wrapped := apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close selected source", closeErr)
			retErr = stderrors.Join(retErr, wrapped)
		}
	}()
	opened, err := in.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return stagedFileMetadata{}, apperrors.Wrap(
			apperrors.ErrCodeInvalidRequest, "selected source identity changed during open", err)
	}

	parent := path.Dir(rel)
	if parent != "." {
		if mkdirErr := staged.MkdirAll(parent, 0o755); mkdirErr != nil {
			return stagedFileMetadata{}, apperrors.Wrap(
				apperrors.ErrCodeInternal, "failed to create staged directory", mkdirErr)
		}
		for _, dir := range parentDirectories(parent) {
			if chmodErr := staged.Chmod(dir, 0o755); chmodErr != nil {
				return stagedFileMetadata{}, apperrors.Wrap(
					apperrors.ErrCodeInternal, "failed to set staged directory mode", chmodErr)
			}
		}
	}
	out, err := staged.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return stagedFileMetadata{}, apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to create staged file", err)
	}
	firstHash := digest.SHA256.Digester()
	size, copyErr := deps.copy(ctx, io.MultiWriter(out, firstHash.Hash()), in)
	chmodErr := out.Chmod(opened.Mode().Perm())
	closeErr := out.Close()
	if copyErr != nil || chmodErr != nil || closeErr != nil {
		return stagedFileMetadata{}, wrapContextualIOFailure(ctx, stderrors.Join(copyErr, chmodErr, closeErr),
			apperrors.ErrCodeInternal, "failed to stage source file")
	}
	firstDigest := firstHash.Digest()

	if deps.beforeFileRevalidate != nil {
		if revalidateErr := deps.beforeFileRevalidate(rel); revalidateErr != nil {
			return stagedFileMetadata{}, apperrors.Wrap(
				apperrors.ErrCodeInvalidRequest, "source file revalidation hook failed", revalidateErr)
		}
	}
	if ctxErr := contextError(ctx, "source revalidation canceled"); ctxErr != nil {
		return stagedFileMetadata{}, ctxErr
	}
	if _, seekErr := in.Seek(0, io.SeekStart); seekErr != nil {
		return stagedFileMetadata{}, apperrors.Wrap(
			apperrors.ErrCodeInvalidRequest, "failed to rewind selected source", seekErr)
	}
	secondHash := digest.SHA256.Digester()
	secondSize, err := deps.copy(ctx, secondHash.Hash(), in)
	if err != nil {
		return stagedFileMetadata{}, wrapContextualIOFailure(ctx, err,
			apperrors.ErrCodeInvalidRequest, "failed to re-read selected source")
	}
	afterPath, err := lstatComponents(source, rel)
	afterFD, statErr := in.Stat()
	if err != nil || statErr != nil || !os.SameFile(before, afterPath) || !os.SameFile(before, afterFD) ||
		opened.Mode() != afterFD.Mode() || size != secondSize || firstDigest != secondHash.Digest() {

		return stagedFileMetadata{}, apperrors.Wrap(
			apperrors.ErrCodeInvalidRequest, "selected source changed while staging",
			stderrors.Join(err, statErr))
	}
	metadata, err := verifyStagedFile(
		ctx, staged, rel, firstDigest, size, opened.Mode().Perm(), deps.copy)
	if err != nil {
		return stagedFileMetadata{}, err
	}
	return metadata, nil
}

func parentDirectories(parent string) []string {
	parts := strings.Split(parent, "/")
	result := make([]string, 0, len(parts))
	for i := range parts {
		result = append(result, strings.Join(parts[:i+1], "/"))
	}
	return result
}

func verifyStagedFile(
	ctx context.Context,
	root *os.Root,
	rel string,
	wantDigest digest.Digest,
	wantSize int64,
	wantMode fs.FileMode,
	copyFn func(context.Context, io.Writer, io.Reader) (int64, error),
) (_ stagedFileMetadata, retErr error) {

	f, err := root.Open(rel)
	if err != nil {
		return stagedFileMetadata{}, apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to reopen staged file", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			retErr = stderrors.Join(retErr,
				apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close staged file verification", closeErr))
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != wantMode {
		return stagedFileMetadata{}, apperrors.Wrap(
			apperrors.ErrCodeInternal, "staged file metadata mismatch", err)
	}
	digester := digest.SHA256.Digester()
	size, err := copyFn(ctx, digester.Hash(), f)
	if err != nil {
		return stagedFileMetadata{}, wrapContextualIOFailure(ctx, err,
			apperrors.ErrCodeInternal, "failed to verify staged file")
	}
	if size != wantSize || digester.Digest() != wantDigest {
		return stagedFileMetadata{}, apperrors.New(
			apperrors.ErrCodeInternal, "staged file content mismatch")
	}
	return stagedFileMetadata{digest: wantDigest, size: wantSize, mode: wantMode}, nil
}

func wrapContextualIOFailure(
	ctx context.Context,
	err error,
	defaultCode apperrors.ErrorCode,
	message string,
) error {

	if err == nil {
		return nil
	}
	if ctx.Err() != nil || stderrors.Is(err, context.Canceled) ||
		stderrors.Is(err, context.DeadlineExceeded) ||
		stderrors.Is(err, apperrors.New(apperrors.ErrCodeTimeout, "")) {

		return apperrors.PropagateOrWrap(err, apperrors.ErrCodeTimeout, message)
	}
	return apperrors.PropagateOrWrap(err, defaultCode, message)
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, copyBufferSize)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			m, writeErr := dst.Write(buffer[:n])
			written += int64(m)
			if writeErr != nil {
				return written, writeErr
			}
			if m != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func (s *preparedSource) relativeFiles() []string {
	return append([]string(nil), s.files...)
}

func (s *preparedSource) validate() error {
	if s == nil || s.workspace == nil || s.source == nil || s.output == nil || s.root == nil {
		return apperrors.New(apperrors.ErrCodeInternal, "prepared source ownership is incomplete")
	}
	if len(s.files) != len(s.metadata) {
		return apperrors.New(apperrors.ErrCodeInternal, "prepared source metadata is incomplete")
	}
	for _, rel := range s.files {
		if _, ok := s.metadata[rel]; !ok {
			return apperrors.New(apperrors.ErrCodeInternal, "prepared source file metadata is missing")
		}
	}
	if err := s.source.validate(apperrors.ErrCodeInvalidRequest); err != nil {
		return err
	}
	if err := s.output.validate(apperrors.ErrCodeInvalidRequest); err != nil {
		return err
	}
	if err := s.workspace.validate(); err != nil {
		return err
	}
	if s.auxiliary != nil {
		if err := s.auxiliary.validate(); err != nil {
			return err
		}
	}
	held, heldErr := s.root.Stat(".")
	workspaceHeld, workspaceHeldErr := s.workspace.child.Stat(".")
	if heldErr != nil || workspaceHeldErr != nil ||
		!os.SameFile(s.rootInfo, held) || !os.SameFile(s.rootInfo, workspaceHeld) ||
		!os.SameFile(s.workspace.childInfo, held) {

		return apperrors.Wrap(apperrors.ErrCodeInternal, "staged source identity changed",
			stderrors.Join(heldErr, workspaceHeldErr))
	}
	return nil
}

func (s *preparedSource) open(ctx context.Context, rel string) (_ *verifiedStagedFile, retErr error) {
	return s.openWithDependencies(ctx, rel, defaultVerifiedStagedFileDependencies())
}

func (s *preparedSource) openWithDependencies(
	ctx context.Context,
	rel string,
	deps verifiedStagedFileDependencies,
) (_ *verifiedStagedFile, retErr error) {

	if deps.newReader == nil {
		deps.newReader = defaultVerifiedStagedFileDependencies().newReader
	}
	if err := contextError(ctx, "staged source open canceled"); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	metadata, ok := s.metadata[rel]
	if !ok {
		return nil, apperrors.New(
			apperrors.ErrCodeInternal, "staged source file metadata is unavailable")
	}
	before, beforeErr := lstatComponents(s.root, rel)
	if beforeErr != nil || !before.Mode().IsRegular() ||
		before.Mode().Perm() != metadata.mode || before.Size() != metadata.size {

		return nil, apperrors.Wrap(
			apperrors.ErrCodeInternal, "staged source file metadata changed", beforeErr)
	}
	f, err := s.root.Open(rel)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to open staged source file", err)
	}
	keepOpen := false
	defer func() {
		if keepOpen {
			return
		}
		if closeErr := f.Close(); closeErr != nil {
			retErr = stderrors.Join(retErr,
				apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close rejected staged source file", closeErr))
		}
	}()
	opened, statErr := f.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) ||
		opened.Mode().Perm() != metadata.mode || opened.Size() != metadata.size {

		return nil, apperrors.Wrap(
			apperrors.ErrCodeInternal, "staged source file identity changed during open", statErr)
	}
	if err := contextError(ctx, "staged source verification canceled"); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	keepOpen = true
	return &verifiedStagedFile{
		ctx:      ctx,
		source:   s,
		file:     f,
		reader:   deps.newReader(ctx, f),
		rel:      rel,
		identity: opened,
		info:     opened,
		metadata: metadata,
		digester: digest.SHA256.Digester(),
		deps:     deps,
	}, nil
}

func (s *preparedSource) Close() error {
	s.closeOnce.Do(func() {
		if s.root != nil {
			if err := s.closeRoot(s.root); err != nil {
				s.closeErr = stderrors.Join(s.closeErr,
					apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close staged source root", err))
			}
		}
		if s.workspace != nil {
			s.closeErr = stderrors.Join(s.closeErr, s.workspace.Close())
		}
		if s.source != nil {
			s.closeErr = stderrors.Join(s.closeErr, s.source.close(
				"prepared caller source root", s.closeRoot))
		}
		if s.output != nil {
			s.closeErr = stderrors.Join(s.closeErr, s.output.close(
				"prepared caller output root", s.closeRoot))
		}
		if s.auxiliary != nil {
			s.closeErr = stderrors.Join(s.closeErr, s.auxiliary.Close())
		}
		if s.closeErr != nil {
			s.closeErr = apperrors.PropagateOrWrap(s.closeErr, apperrors.ErrCodeInternal,
				"failed to close prepared source")
		}
	})
	return s.closeErr
}

type ownedLayoutDependencies struct {
	beforeOutputOpen      func(string) error
	beforeChildRevalidate func(parent, child string) error
	beforeRemove          func(parent, child string) error
	childLstat            func(*os.Root, string) (fs.FileInfo, error)
	openChild             func(*os.Root, string) (*os.Root, error)
	chmodChild            func(*os.Root) error
	openParentCopy        func(*os.Root) (*os.Root, error)
	removeAll             func(*os.Root, string) error
	closeRoot             func(*os.Root) error
}

func defaultOwnedLayoutDependencies() ownedLayoutDependencies {
	return ownedLayoutDependencies{
		childLstat: func(root *os.Root, name string) (fs.FileInfo, error) {
			return root.Lstat(name)
		},
		openChild: func(root *os.Root, name string) (*os.Root, error) {
			return root.OpenRoot(name)
		},
		chmodChild:     func(root *os.Root) error { return root.Chmod(".", 0o700) },
		openParentCopy: func(root *os.Root) (*os.Root, error) { return root.OpenRoot(".") },
		removeAll:      func(root *os.Root, name string) error { return root.RemoveAll(name) },
		closeRoot:      func(root *os.Root) error { return root.Close() },
	}
}

func normalizeOwnedLayoutDependencies(deps ownedLayoutDependencies) ownedLayoutDependencies {
	defaults := defaultOwnedLayoutDependencies()
	if deps.childLstat == nil {
		deps.childLstat = defaults.childLstat
	}
	if deps.openChild == nil {
		deps.openChild = defaults.openChild
	}
	if deps.chmodChild == nil {
		deps.chmodChild = defaults.chmodChild
	}
	if deps.openParentCopy == nil {
		deps.openParentCopy = defaults.openParentCopy
	}
	if deps.removeAll == nil {
		deps.removeAll = defaults.removeAll
	}
	if deps.closeRoot == nil {
		deps.closeRoot = defaults.closeRoot
	}
	return deps
}

type ownedLayout struct {
	path       string
	parentPath string
	parentReal string
	childName  string
	parent     *os.Root
	backup     *os.Root
	child      *os.Root
	parentInfo fs.FileInfo
	childInfo  fs.FileInfo
	deps       ownedLayoutDependencies
	finishOnce sync.Once
	finishErr  error
	released   bool
}

func newOwnedLayout(ctx context.Context, outputDir string) (*ownedLayout, error) {
	return newOwnedLayoutWithDependencies(ctx, outputDir, defaultOwnedLayoutDependencies())
}

func newOwnedLayoutWithDependencies(
	ctx context.Context,
	outputDir string,
	deps ownedLayoutDependencies,
) (*ownedLayout, error) {

	deps = normalizeOwnedLayoutDependencies(deps)

	output, err := openRootedDirectoryWithClose(
		ctx, outputDir, apperrors.ErrCodeInvalidRequest, deps.beforeOutputOpen, deps.closeRoot)
	if err != nil {
		return nil, err
	}
	backup, err := deps.openParentCopy(output.root)
	if err != nil {
		return nil, stderrors.Join(
			apperrors.Wrap(apperrors.ErrCodeInternal, "failed to retain OCI layout cleanup root", err),
			output.close("OCI layout output root", deps.closeRoot))
	}
	backupInfo, err := backup.Stat(".")
	if err != nil || !os.SameFile(output.info, backupInfo) {
		return nil, stderrors.Join(
			apperrors.Wrap(apperrors.ErrCodeInternal, "failed to verify OCI layout cleanup root", err),
			apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close rejected OCI layout cleanup root",
				deps.closeRoot(backup)),
			output.close("OCI layout output root", deps.closeRoot))
	}
	allocated, err := allocateAnchoredChild(ctx, output, layoutPrefix, "OCI layout",
		deps.beforeChildRevalidate, childAllocationDependencies{
			childLstat: deps.childLstat,
			openChild:  deps.openChild,
			chmodChild: deps.chmodChild,
			removeAll:  deps.removeAll,
			closeRoot:  deps.closeRoot,
		})
	if err != nil {
		return nil, stderrors.Join(err,
			apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close rejected OCI layout cleanup root",
				deps.closeRoot(backup)),
			output.close("OCI layout output root", deps.closeRoot))
	}
	return &ownedLayout{
		path:       filepath.Join(output.path, allocated.name),
		parentPath: output.path,
		parentReal: output.resolved,
		childName:  allocated.name,
		parent:     output.root,
		backup:     backup,
		child:      allocated.root,
		parentInfo: output.info,
		childInfo:  allocated.info,
		deps:       deps,
	}, nil
}

func (l *ownedLayout) Path() string { return l.path }

func (l *ownedLayout) validate() error {
	parentHeld, parentHeldErr := l.parent.Stat(".")
	backupHeld, backupHeldErr := l.backup.Stat(".")
	parentNamed, parentNamedErr := os.Lstat(l.parentPath) //nolint:gosec // retained canonical parent identity
	parentReal, parentRealErr := os.Lstat(l.parentReal)   //nolint:gosec // retained canonical parent identity
	childHeld, childHeldErr := l.child.Stat(".")
	named, namedErr := l.parent.Lstat(l.childName)
	if parentHeldErr != nil || backupHeldErr != nil || parentNamedErr != nil ||
		parentRealErr != nil || childHeldErr != nil || namedErr != nil ||
		parentNamed.Mode()&os.ModeSymlink != 0 || parentReal.Mode()&os.ModeSymlink != 0 ||
		named.Mode()&os.ModeSymlink != 0 || !os.SameFile(l.parentInfo, parentHeld) ||
		!os.SameFile(l.parentInfo, backupHeld) || !os.SameFile(l.parentInfo, parentNamed) ||
		!os.SameFile(l.parentInfo, parentReal) ||
		!os.SameFile(l.childInfo, childHeld) || !os.SameFile(l.childInfo, named) {

		return apperrors.Wrap(apperrors.ErrCodeInternal, "OCI layout identity changed",
			stderrors.Join(parentHeldErr, backupHeldErr, parentNamedErr,
				parentRealErr, childHeldErr, namedErr))
	}
	return nil
}

func (l *ownedLayout) Close() error {
	l.finish(false)
	return l.finishErr
}

func (l *ownedLayout) release() (string, error) {
	l.finish(true)
	if l.finishErr != nil {
		return "", l.finishErr
	}
	return l.path, nil
}

func (l *ownedLayout) finish(release bool) {
	l.finishOnce.Do(func() {
		if release {
			l.finishErr = l.finishRelease()
		} else {
			l.finishErr = l.finishRemove(nil)
		}
		if l.finishErr != nil {
			l.finishErr = apperrors.PropagateOrWrap(l.finishErr, apperrors.ErrCodeInternal,
				"failed to finalize OCI layout")
		}
	})
}

func (l *ownedLayout) finishRelease() error {
	if err := l.validate(); err != nil {
		return l.finishRemove(err)
	}
	childCloseErr := l.deps.closeRoot(l.child)
	parentCloseErr := l.deps.closeRoot(l.parent)
	primary := stderrors.Join(childCloseErr, parentCloseErr)
	if primary != nil {
		cleanupErr := l.removeWithRoot(l.backup)
		backupCloseErr := l.deps.closeRoot(l.backup)
		return stderrors.Join(primary, cleanupErr, backupCloseErr)
	}
	if err := l.deps.closeRoot(l.backup); err != nil {
		return stderrors.Join(err, l.recoverReleasedLayout())
	}
	l.released = true
	return nil
}

func (l *ownedLayout) finishRemove(primary error) error {
	stable := true
	if err := l.validate(); err != nil {
		primary = stderrors.Join(primary, err)
		stable = false
	}
	childCloseErr := l.deps.closeRoot(l.child)
	var removeErr error
	if stable {
		removeErr = l.removeWithRoot(l.parent)
	}
	parentCloseErr := l.deps.closeRoot(l.parent)
	backupCloseErr := l.deps.closeRoot(l.backup)
	return stderrors.Join(primary, childCloseErr, removeErr, parentCloseErr, backupCloseErr)
}

func (l *ownedLayout) removeWithRoot(root *os.Root) error {
	var errs []error
	if l.deps.beforeRemove != nil {
		if err := l.deps.beforeRemove(l.parentPath, l.childName); err != nil {
			errs = append(errs,
				apperrors.Wrap(apperrors.ErrCodeInternal, "OCI layout pre-removal hook failed", err))
		}
	}
	if err := l.validateNamedWithRoot(root); err != nil {
		return stderrors.Join(append(errs, err)...)
	}
	if err := l.deps.removeAll(root, l.childName); err != nil {
		errs = append(errs,
			apperrors.Wrap(apperrors.ErrCodeInternal, "failed to remove OCI layout", err))
	}
	return stderrors.Join(errs...)
}

func (l *ownedLayout) validateNamedWithRoot(root *os.Root) error {
	parentHeld, parentHeldErr := root.Stat(".")
	parentNamed, parentNamedErr := os.Lstat(l.parentPath) //nolint:gosec // retained canonical parent identity
	parentReal, parentRealErr := os.Lstat(l.parentReal)   //nolint:gosec // retained canonical parent identity
	childNamed, childNamedErr := root.Lstat(l.childName)
	if parentHeldErr != nil || parentNamedErr != nil || parentRealErr != nil || childNamedErr != nil ||
		parentNamed.Mode()&os.ModeSymlink != 0 || parentReal.Mode()&os.ModeSymlink != 0 ||
		childNamed.Mode()&os.ModeSymlink != 0 || !os.SameFile(l.parentInfo, parentHeld) ||
		!os.SameFile(l.parentInfo, parentNamed) || !os.SameFile(l.parentInfo, parentReal) ||
		!os.SameFile(l.childInfo, childNamed) {

		return apperrors.Wrap(apperrors.ErrCodeInternal, "OCI layout identity changed before removal",
			stderrors.Join(parentHeldErr, parentNamedErr, parentRealErr, childNamedErr))
	}
	return nil
}

func (l *ownedLayout) recoverReleasedLayout() error {
	before, err := os.Lstat(l.parentPath) //nolint:gosec // retained canonical parent identity
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to inspect OCI layout parent during release recovery", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || !os.SameFile(l.parentInfo, before) {
		return apperrors.New(apperrors.ErrCodeInternal,
			"OCI layout parent changed while recovering failed release")
	}
	root, err := os.OpenRoot(l.parentPath)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to reopen OCI layout parent during release recovery", err)
	}
	held, heldErr := root.Stat(".")
	after, afterErr := os.Lstat(l.parentPath) //nolint:gosec // retained canonical parent identity
	var retErr error
	if heldErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(l.parentInfo, held) || !os.SameFile(l.parentInfo, after) {

		retErr = apperrors.Wrap(apperrors.ErrCodeInternal,
			"OCI layout parent changed while reopening for release recovery",
			stderrors.Join(heldErr, afterErr))
	} else {
		retErr = l.removeWithRoot(root)
	}
	if closeErr := l.deps.closeRoot(root); closeErr != nil {
		retErr = stderrors.Join(retErr, apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to close recovered OCI layout parent", closeErr))
	}
	return retErr
}

func contextError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeTimeout, message, err)
	}
	return nil
}
