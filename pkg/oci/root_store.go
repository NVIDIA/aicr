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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	specs "github.com/opencontainers/image-spec/specs-go"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	storeoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

const rootStoreIngestDir = "ingest"

// localOCIStore is deliberately narrower than oras.Target. Generic packaging
// needs only content storage and tag persistence; publishing receives its
// read-only view separately. Its confinement relies on os.Root's rename-stable
// handles on AICR's supported native platforms; plan9 and js are not supported.
type localOCIStore interface {
	content.Storage
	content.Tagger
}

type rootOCIStoreDependencies struct {
	randomName         func() (string, error)
	openFile           func(*os.Root, string, int, fs.FileMode) (*os.File, error)
	closeFile          func(*os.File) error
	syncFile           func(*os.File) error
	chmodFile          func(*os.File, fs.FileMode) error
	mkdirAll           func(*os.Root, string, fs.FileMode) error
	lstat              func(*os.Root, string) (fs.FileInfo, error)
	link               func(*os.Root, string, string) error
	rename             func(*os.Root, string, string) error
	remove             func(*os.Root, string) error
	copy               func(context.Context, io.Writer, io.Reader) (int64, error)
	beforeBlobPromote  func() error
	beforeBlobVerify   func() error
	beforeIndexPromote func() error
}

func defaultRootOCIStoreDependencies() rootOCIStoreDependencies {
	return rootOCIStoreDependencies{
		randomName: func() (string, error) {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return "", err
			}
			return hex.EncodeToString(random[:]), nil
		},
		openFile: func(root *os.Root, name string, flag int, mode fs.FileMode) (*os.File, error) {
			return root.OpenFile(name, flag, mode)
		},
		closeFile: func(file *os.File) error { return file.Close() },
		syncFile:  func(file *os.File) error { return file.Sync() },
		chmodFile: func(file *os.File, mode fs.FileMode) error { return file.Chmod(mode) },
		mkdirAll:  func(root *os.Root, name string, mode fs.FileMode) error { return root.MkdirAll(name, mode) },
		lstat:     func(root *os.Root, name string) (fs.FileInfo, error) { return root.Lstat(name) },
		link:      func(root *os.Root, oldName, newName string) error { return root.Link(oldName, newName) },
		rename:    func(root *os.Root, oldName, newName string) error { return root.Rename(oldName, newName) },
		remove:    func(root *os.Root, name string) error { return root.Remove(name) },
		copy:      copyWithContext,
	}
}

type rootOCIStore struct {
	root     *os.Root
	readOnly content.ReadOnlyStorage
	validate func() error
	deps     rootOCIStoreDependencies

	mu   sync.RWMutex
	tags map[string]ociv1.Descriptor
}

var _ localOCIStore = (*rootOCIStore)(nil)

func newRootOCIStore(ctx context.Context, layout *ownedLayout) (*rootOCIStore, error) {
	return newRootOCIStoreWithDependencies(ctx, layout, defaultRootOCIStoreDependencies())
}

func newRootOCIStoreWithDependencies(
	ctx context.Context,
	layout *ownedLayout,
	deps rootOCIStoreDependencies,
) (*rootOCIStore, error) {

	if layout == nil || layout.child == nil {
		return nil, apperrors.New(apperrors.ErrCodeInternal, "OCI layout root is unavailable")
	}
	if err := contextError(ctx, "OCI store initialization canceled"); err != nil {
		return nil, err
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	store := &rootOCIStore{
		root:     layout.child,
		readOnly: storeoci.NewStorageFromFS(layout.child.FS()),
		validate: layout.validate,
		deps:     deps,
		tags:     make(map[string]ociv1.Descriptor),
	}
	if err := store.ensureDirectory(rootStoreIngestDir, 0o700); err != nil {
		return nil, err
	}
	if err := store.ensureDirectory(ociv1.ImageBlobsDir, 0o755); err != nil {
		return nil, err
	}
	layoutJSON, err := json.Marshal(ociv1.ImageLayout{Version: ociv1.ImageLayoutVersion})
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to marshal OCI layout metadata", err)
	}
	indexJSON, err := marshalRootStoreIndex(nil)
	if err != nil {
		return nil, err
	}
	if err := store.createMetadata(ctx, ociv1.ImageLayoutFile, layoutJSON); err != nil {
		return nil, err
	}
	if err := store.createMetadata(ctx, ociv1.ImageIndexFile, indexJSON); err != nil {
		return nil, stderrors.Join(err, store.removePublished(
			ociv1.ImageLayoutFile, "failed to remove partial OCI layout metadata"))
	}
	return store, nil
}

func (s *rootOCIStore) Fetch(
	ctx context.Context,
	desc ociv1.Descriptor,
) (io.ReadCloser, error) {

	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := contextError(ctx, "local OCI fetch canceled"); err != nil {
		return nil, err
	}
	if err := s.validateDescriptor(desc); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	reader, err := s.readOnly.Fetch(ctx, desc)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to fetch local OCI blob", err)
	}
	return reader, nil
}

func (s *rootOCIStore) Exists(ctx context.Context, desc ociv1.Descriptor) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := contextError(ctx, "local OCI existence check canceled"); err != nil {
		return false, err
	}
	if err := s.validateDescriptor(desc); err != nil {
		return false, err
	}
	if err := s.validate(); err != nil {
		return false, err
	}
	exists, err := s.readOnly.Exists(ctx, desc)
	if err != nil {
		return false, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to inspect local OCI blob", err)
	}
	return exists, nil
}

func (s *rootOCIStore) Push(ctx context.Context, desc ociv1.Descriptor, reader io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := contextError(ctx, "local OCI blob push canceled"); err != nil {
		return err
	}
	if err := s.validateDescriptor(desc); err != nil {
		return err
	}
	if reader == nil {
		return apperrors.New(apperrors.ErrCodeInternal, "local OCI blob reader is nil")
	}
	if err := s.validate(); err != nil {
		return err
	}
	blobName := rootStoreBlobPath(desc)
	if _, err := s.deps.lstat(s.root, blobName); err == nil {
		return s.verifyAlreadyExists(ctx, desc)
	} else if !stderrors.Is(err, fs.ErrNotExist) {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to inspect local OCI blob", err)
	}
	if err := s.ensureDirectory(path.Dir(blobName), 0o755); err != nil {
		return err
	}

	verifyReader := content.NewVerifyReader(reader, desc)
	tempName, err := s.writeTemp(ctx, verifyReader, 0o444)
	if err != nil {
		return err
	}
	if err := verifyReader.Verify(); err != nil {
		return s.failTemp(tempName, apperrors.Wrap(
			apperrors.ErrCodeInternal, "local OCI blob content does not match descriptor", err))
	}
	if s.deps.beforeBlobPromote != nil {
		if err := s.deps.beforeBlobPromote(); err != nil {
			return s.failTemp(tempName, apperrors.Wrap(
				apperrors.ErrCodeInternal, "local OCI blob promotion precondition failed", err))
		}
	}
	if err := contextError(ctx, "local OCI blob promotion canceled"); err != nil {
		return s.failTemp(tempName, err)
	}
	if err := s.deps.link(s.root, tempName, blobName); err != nil {
		cleanupErr := s.removeTemp(tempName)
		if stderrors.Is(err, fs.ErrExist) {
			if cleanupErr != nil {
				return cleanupErr
			}
			return s.verifyAlreadyExists(ctx, desc)
		}
		return stderrors.Join(
			apperrors.Wrap(apperrors.ErrCodeInternal, "failed to promote local OCI blob", err),
			cleanupErr,
		)
	}
	if cleanupErr := s.removeTemp(tempName); cleanupErr != nil {
		return stderrors.Join(cleanupErr, s.removePublished(
			blobName, "failed to roll back local OCI blob after cleanup failure"))
	}
	return nil
}

func (s *rootOCIStore) Tag(
	ctx context.Context,
	desc ociv1.Descriptor,
	reference string,
) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextError(ctx, "local OCI tag canceled"); err != nil {
		return err
	}
	if err := validateDistributionTag(reference); err != nil {
		return err
	}
	if err := s.validateDescriptor(desc); err != nil {
		return err
	}
	if err := s.validate(); err != nil {
		return err
	}
	if err := s.verifyBlob(ctx, desc); err != nil {
		return err
	}

	candidate := make(map[string]ociv1.Descriptor, len(s.tags)+1)
	for tag, tagged := range s.tags {
		candidate[tag] = cloneRootStoreDescriptor(tagged)
	}
	candidate[reference] = cloneRootStoreDescriptor(desc)
	indexJSON, err := marshalRootStoreIndex(candidate)
	if err != nil {
		return err
	}
	if err := s.replaceIndex(ctx, indexJSON); err != nil {
		return err
	}
	s.tags = candidate
	return nil
}

func (s *rootOCIStore) validateDescriptor(desc ociv1.Descriptor) error {
	if desc.MediaType == "" {
		return apperrors.New(apperrors.ErrCodeInternal, "local OCI descriptor media type is empty")
	}
	if desc.Size < 0 {
		return apperrors.Wrap(apperrors.ErrCodeInternal,
			"local OCI descriptor size is negative", content.ErrInvalidDescriptorSize)
	}
	if err := desc.Digest.Validate(); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "local OCI descriptor digest is invalid", err)
	}
	return nil
}

func (s *rootOCIStore) verifyAlreadyExists(ctx context.Context, desc ociv1.Descriptor) error {
	if err := s.verifyBlob(ctx, desc); err != nil {
		return err
	}
	return apperrors.Wrap(apperrors.ErrCodeInternal, "local OCI blob already exists", errdef.ErrAlreadyExists)
}

func (s *rootOCIStore) verifyBlob(ctx context.Context, desc ociv1.Descriptor) error {
	if err := contextError(ctx, "local OCI blob verification canceled"); err != nil {
		return err
	}
	if s.deps.beforeBlobVerify != nil {
		if err := s.deps.beforeBlobVerify(); err != nil {
			return apperrors.PropagateOrWrap(
				err, apperrors.ErrCodeInternal, "local OCI blob verification precondition failed")
		}
	}
	if err := contextError(ctx, "local OCI blob verification canceled"); err != nil {
		return err
	}
	name := rootStoreBlobPath(desc)
	info, err := s.deps.lstat(s.root, name)
	if err != nil {
		if stderrors.Is(err, fs.ErrNotExist) {
			return apperrors.Wrap(apperrors.ErrCodeInternal, "local OCI blob was not found", errdef.ErrNotFound)
		}
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to inspect local OCI blob", err)
	}
	if !info.Mode().IsRegular() {
		return apperrors.New(apperrors.ErrCodeInternal, "local OCI blob is not a regular file")
	}
	file, err := s.root.Open(name)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to open local OCI blob", err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		var identityErr error = apperrors.Wrap(
			apperrors.ErrCodeInternal, "local OCI blob identity changed during open", statErr)
		if closeErr := file.Close(); closeErr != nil {
			identityErr = stderrors.Join(identityErr, apperrors.Wrap(
				apperrors.ErrCodeInternal, "failed to close rejected local OCI blob", closeErr))
		}
		return identityErr
	}
	reader := newContextReadCloser(ctx, file)
	_, readErr := content.ReadAll(reader, desc)
	current, currentErr := s.deps.lstat(s.root, name)
	if currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		readErr = stderrors.Join(readErr, apperrors.Wrap(
			apperrors.ErrCodeInternal, "local OCI blob identity changed during verification", currentErr))
	}
	closeErr := reader.Close()
	if readErr != nil {
		readErr = rootStoreOperationError(ctx, "local OCI blob does not match descriptor", readErr)
	}
	if closeErr != nil {
		closeErr = apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close local OCI blob", closeErr)
	}
	return stderrors.Join(readErr, closeErr)
}

func (s *rootOCIStore) ensureDirectory(name string, mode fs.FileMode) error {
	if err := s.deps.mkdirAll(s.root, name, mode); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create local OCI directory", err)
	}
	current := ""
	for _, component := range strings.Split(name, "/") {
		current = path.Join(current, component)
		info, err := s.deps.lstat(s.root, current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return apperrors.Wrap(apperrors.ErrCodeInternal, "local OCI directory is invalid", err)
		}
	}
	return nil
}

func (s *rootOCIStore) writeTemp(
	ctx context.Context,
	reader io.Reader,
	mode fs.FileMode,
) (name string, retErr error) {

	if err := contextError(ctx, "local OCI temporary write canceled"); err != nil {
		return "", err
	}
	randomName, err := s.deps.randomName()
	if err != nil {
		return "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to generate local OCI temporary name", err)
	}
	tempName := path.Join(rootStoreIngestDir, ".aicr-"+randomName)
	name = tempName
	file, err := s.deps.openFile(s.root, tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create local OCI temporary file", err)
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := s.deps.closeFile(file); closeErr != nil {
				retErr = stderrors.Join(retErr, apperrors.Wrap(
					apperrors.ErrCodeInternal, "failed to close local OCI temporary file", closeErr))
			}
		}
		if retErr != nil {
			retErr = stderrors.Join(retErr, s.removeTemp(tempName))
			name = ""
		}
	}()
	if _, err := s.deps.copy(ctx, file, reader); err != nil {
		return "", rootStoreOperationError(ctx, "failed to write local OCI temporary file", err)
	}
	if err := contextError(ctx, "local OCI temporary write canceled"); err != nil {
		return "", err
	}
	if err := s.deps.syncFile(file); err != nil {
		return "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to sync local OCI temporary file", err)
	}
	if err := s.deps.chmodFile(file, mode); err != nil {
		return "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to set local OCI temporary file mode", err)
	}
	if err := s.deps.closeFile(file); err != nil {
		closed = true
		return "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close local OCI temporary file", err)
	}
	closed = true
	return name, nil
}

func (s *rootOCIStore) createMetadata(ctx context.Context, name string, data []byte) error {
	if _, err := s.deps.lstat(s.root, name); err == nil {
		return apperrors.New(apperrors.ErrCodeInternal, "local OCI metadata already exists")
	} else if !stderrors.Is(err, fs.ErrNotExist) {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to inspect local OCI metadata", err)
	}
	tempName, err := s.writeTemp(ctx, bytes.NewReader(data), 0o644)
	if err != nil {
		return err
	}
	if err := contextError(ctx, "local OCI metadata promotion canceled"); err != nil {
		return s.failTemp(tempName, err)
	}
	if err := s.deps.link(s.root, tempName, name); err != nil {
		return s.failTemp(tempName, apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to create local OCI metadata", err))
	}
	if cleanupErr := s.removeTemp(tempName); cleanupErr != nil {
		return stderrors.Join(cleanupErr, s.removePublished(
			name, "failed to roll back local OCI metadata after cleanup failure"))
	}
	return nil
}

func (s *rootOCIStore) replaceIndex(ctx context.Context, data []byte) error {
	tempName, err := s.writeTemp(ctx, bytes.NewReader(data), 0o644)
	if err != nil {
		return err
	}
	if s.deps.beforeIndexPromote != nil {
		if err := s.deps.beforeIndexPromote(); err != nil {
			return s.failTemp(tempName, apperrors.Wrap(
				apperrors.ErrCodeInternal, "local OCI index promotion precondition failed", err))
		}
	}
	if err := contextError(ctx, "local OCI index promotion canceled"); err != nil {
		return s.failTemp(tempName, err)
	}
	if err := s.deps.rename(s.root, tempName, ociv1.ImageIndexFile); err != nil {
		return s.failTemp(tempName, apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to replace local OCI index", err))
	}
	return nil
}

func (s *rootOCIStore) failTemp(name string, primary error) error {
	return stderrors.Join(primary, s.removeTemp(name))
}

func (s *rootOCIStore) removeTemp(name string) error {
	return s.removeWithRetry(name, "failed to remove local OCI temporary file")
}

func (s *rootOCIStore) removePublished(name, message string) error {
	return s.removeWithRetry(name, message)
}

func (s *rootOCIStore) removeWithRetry(name, message string) error {
	if name == "" {
		return nil
	}
	firstErr := s.deps.remove(s.root, name)
	if firstErr == nil || stderrors.Is(firstErr, fs.ErrNotExist) {
		return nil
	}
	secondErr := s.deps.remove(s.root, name)
	if secondErr == nil || stderrors.Is(secondErr, fs.ErrNotExist) {
		return apperrors.Wrap(apperrors.ErrCodeInternal, message, firstErr)
	}
	return stderrors.Join(
		apperrors.Wrap(apperrors.ErrCodeInternal, message, firstErr),
		apperrors.Wrap(apperrors.ErrCodeInternal, message+" after retry", secondErr),
	)
}

func marshalRootStoreIndex(tags map[string]ociv1.Descriptor) ([]byte, error) {
	references := make([]string, 0, len(tags))
	for reference := range tags {
		references = append(references, reference)
	}
	sort.Strings(references)
	manifests := make([]ociv1.Descriptor, 0, len(references))
	for _, reference := range references {
		desc := cloneRootStoreDescriptor(tags[reference])
		if desc.Annotations == nil {
			desc.Annotations = make(map[string]string, 1)
		}
		desc.Annotations[ociv1.AnnotationRefName] = reference
		manifests = append(manifests, desc)
	}
	index := ociv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ociv1.MediaTypeImageIndex,
		Manifests: manifests,
	}
	data, err := json.Marshal(index)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to marshal local OCI index", err)
	}
	return data, nil
}

func cloneRootStoreDescriptor(desc ociv1.Descriptor) ociv1.Descriptor {
	clone := desc
	if desc.Annotations != nil {
		clone.Annotations = make(map[string]string, len(desc.Annotations))
		for key, value := range desc.Annotations {
			clone.Annotations[key] = value
		}
	}
	if desc.URLs != nil {
		clone.URLs = append([]string(nil), desc.URLs...)
	}
	if desc.Data != nil {
		clone.Data = append([]byte(nil), desc.Data...)
	}
	return clone
}

func rootStoreBlobPath(desc ociv1.Descriptor) string {
	return path.Join(ociv1.ImageBlobsDir, desc.Digest.Algorithm().String(), desc.Digest.Encoded())
}

func rootStoreOperationError(ctx context.Context, message string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil || stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return apperrors.PropagateOrWrap(err, apperrors.ErrCodeTimeout, message)
	}
	return apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal, message)
}
