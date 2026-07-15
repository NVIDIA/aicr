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
	"encoding/json"
	stderrors "errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	storeoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

func TestRootOCIStoreInitializesDeterministicLayout(t *testing.T) {
	t.Parallel()

	layouts := make([][]byte, 0, 2)
	indexes := make([][]byte, 0, 2)
	for range 2 {
		layout, store := newRootOCIStoreForTest(t)
		if store == nil {
			t.Fatal("newRootOCIStore() returned nil store")
		}
		layoutBytes, err := layout.child.ReadFile(ociv1.ImageLayoutFile)
		if err != nil {
			t.Fatal(err)
		}
		indexBytes, err := layout.child.ReadFile(ociv1.ImageIndexFile)
		if err != nil {
			t.Fatal(err)
		}
		layouts = append(layouts, layoutBytes)
		indexes = append(indexes, indexBytes)
	}

	if got, want := string(layouts[0]), `{"imageLayoutVersion":"1.0.0"}`; got != want {
		t.Fatalf("oci-layout = %q, want %q", got, want)
	}
	if got, want := string(indexes[0]), `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`; got != want {
		t.Fatalf("index.json = %q, want %q", got, want)
	}
	if !bytes.Equal(layouts[0], layouts[1]) || !bytes.Equal(indexes[0], indexes[1]) {
		t.Fatalf("metadata is not byte deterministic: layouts=%q indexes=%q", layouts, indexes)
	}
}

func TestRootOCIStoreExposesOnlyRequiredCapabilities(t *testing.T) {
	_, store := newRootOCIStoreForTest(t)
	var value any = store
	if _, ok := value.(content.Storage); !ok {
		t.Fatal("rootOCIStore must implement content.Storage")
	}
	if _, ok := value.(content.Tagger); !ok {
		t.Fatal("rootOCIStore must implement content.Tagger")
	}
	if _, ok := value.(content.Resolver); ok {
		t.Fatal("rootOCIStore unexpectedly exposes content.Resolver")
	}
	if _, ok := value.(oras.Target); ok {
		t.Fatal("rootOCIStore unexpectedly exposes oras.Target")
	}
}

func TestRootOCIStorePushValidatesDescriptorAndContent(t *testing.T) {
	t.Parallel()

	validBytes := []byte("verified blob")
	valid := content.NewDescriptorFromBytes("application/octet-stream", validBytes)
	invalidDigest := valid
	invalidDigest.Digest = "sha256:not-hex"
	negativeSize := valid
	negativeSize.Size = -1
	missingMediaType := valid
	missingMediaType.MediaType = ""

	tests := []struct {
		name     string
		desc     ociv1.Descriptor
		reader   func() io.Reader
		cancel   bool
		wantCode apperrors.ErrorCode
	}{
		{name: "valid", desc: valid, reader: func() io.Reader { return bytes.NewReader(validBytes) }},
		{name: "corrupt", desc: valid, reader: func() io.Reader { return bytes.NewReader([]byte("corrupt blob!")) }, wantCode: apperrors.ErrCodeInternal},
		{name: "short", desc: valid, reader: func() io.Reader { return bytes.NewReader(validBytes[:3]) }, wantCode: apperrors.ErrCodeInternal},
		{name: "trailing", desc: valid, reader: func() io.Reader { return bytes.NewReader(append(append([]byte(nil), validBytes...), 'x')) }, wantCode: apperrors.ErrCodeInternal},
		{name: "invalid digest", desc: invalidDigest, reader: func() io.Reader { return bytes.NewReader(validBytes) }, wantCode: apperrors.ErrCodeInternal},
		{name: "negative size", desc: negativeSize, reader: func() io.Reader { return bytes.NewReader(validBytes) }, wantCode: apperrors.ErrCodeInternal},
		{name: "missing media type", desc: missingMediaType, reader: func() io.Reader { return bytes.NewReader(validBytes) }, wantCode: apperrors.ErrCodeInternal},
		{name: "canceled", desc: valid, reader: func() io.Reader { return bytes.NewReader(validBytes) }, cancel: true, wantCode: apperrors.ErrCodeTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			layout, store := newRootOCIStoreForTest(t)
			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := store.Push(ctx, tt.desc, tt.reader())
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("Push() error = %v", err)
				}
				got, readErr := content.FetchAll(context.Background(), store, valid)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !bytes.Equal(got, validBytes) {
					t.Fatalf("FetchAll() = %q, want %q", got, validBytes)
				}
			} else {
				assertErrorCode(t, err, tt.wantCode)
			}
			assertRootIngestEmpty(t, layout.child)
		})
	}
}

func TestRootOCIStoreExistingBlobMustVerifyBeforeAlreadyExists(t *testing.T) {
	t.Parallel()

	layout, store := newRootOCIStoreForTest(t)
	blob := []byte("existing")
	desc := content.NewDescriptorFromBytes("application/octet-stream", blob)
	if err := store.Push(context.Background(), desc, bytes.NewReader(blob)); err != nil {
		t.Fatal(err)
	}
	if err := store.Push(context.Background(), desc, bytes.NewReader(blob)); !stderrors.Is(err, errdef.ErrAlreadyExists) {
		t.Fatalf("second Push() error = %v, want ErrAlreadyExists", err)
	}

	blobName := rootStoreBlobPath(desc)
	if err := layout.child.Chmod(blobName, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := layout.child.WriteFile(blobName, []byte("corrupt!"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := store.Push(context.Background(), desc, bytes.NewReader(blob))
	if err == nil || stderrors.Is(err, errdef.ErrAlreadyExists) {
		t.Fatalf("Push() error = %v, want corrupt existing blob rejection", err)
	}
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	assertRootIngestEmpty(t, layout.child)
}

func TestRootOCIStoreReaderFailureCleansIngest(t *testing.T) {
	t.Parallel()

	layout, store := newRootOCIStoreForTest(t)
	blob := []byte("reader failure")
	desc := content.NewDescriptorFromBytes("application/octet-stream", blob)
	injected := stderrors.New("source reader failed")
	err := store.Push(context.Background(), desc, rootStoreFailingReader{err: injected})
	if !stderrors.Is(err, injected) {
		t.Fatalf("Push() error = %v, want %v", err, injected)
	}
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if _, statErr := layout.child.Lstat(rootStoreBlobPath(desc)); !os.IsNotExist(statErr) {
		t.Fatalf("reader failure published blob: %v", statErr)
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestRootOCIStorePushFailuresCleanIngest(t *testing.T) {
	t.Parallel()

	injected := stderrors.New("injected store failure")
	blob := []byte("blob")
	desc := content.NewDescriptorFromBytes("application/octet-stream", blob)
	tests := []struct {
		name   string
		inject func(*rootOCIStoreDependencies)
	}{
		{name: "write", inject: func(deps *rootOCIStoreDependencies) {
			deps.copy = func(context.Context, io.Writer, io.Reader) (int64, error) { return 0, injected }
		}},
		{name: "sync", inject: func(deps *rootOCIStoreDependencies) {
			deps.syncFile = func(*os.File) error { return injected }
		}},
		{name: "close", inject: func(deps *rootOCIStoreDependencies) {
			deps.closeFile = func(file *os.File) error { return stderrors.Join(file.Close(), injected) }
		}},
		{name: "promotion", inject: func(deps *rootOCIStoreDependencies) {
			deps.link = func(*os.Root, string, string) error { return injected }
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			layout, store := newRootOCIStoreForTest(t)
			deps := defaultRootOCIStoreDependencies()
			tt.inject(&deps)
			store.deps = deps
			err := store.Push(context.Background(), desc, bytes.NewReader(blob))
			if !stderrors.Is(err, injected) {
				t.Fatalf("Push() error = %v, want %v", err, injected)
			}
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
			if _, statErr := layout.child.Lstat(rootStoreBlobPath(desc)); !os.IsNotExist(statErr) {
				t.Fatalf("failed Push() published blob: %v", statErr)
			}
			assertRootIngestEmpty(t, layout.child)
		})
	}
}

func TestRootOCIStoreCleanupFailureRollsBackPromotedBlob(t *testing.T) {
	t.Parallel()

	layout, store := newRootOCIStoreForTest(t)
	blob := []byte("rollback")
	desc := content.NewDescriptorFromBytes("application/octet-stream", blob)
	injected := stderrors.New("temporary unlink failed")
	defaultRemove := store.deps.remove
	var calls int
	store.deps.remove = func(root *os.Root, name string) error {
		calls++
		if calls == 1 {
			return injected
		}
		return defaultRemove(root, name)
	}
	err := store.Push(context.Background(), desc, bytes.NewReader(blob))
	if !stderrors.Is(err, injected) {
		t.Fatalf("Push() error = %v, want %v", err, injected)
	}
	if _, statErr := layout.child.Lstat(rootStoreBlobPath(desc)); !os.IsNotExist(statErr) {
		t.Fatalf("cleanup failure left promoted blob: %v", statErr)
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestRootOCIStoreCancellationBeforePromotionDoesNotMutate(t *testing.T) {
	t.Parallel()

	layout, store := newRootOCIStoreForTest(t)
	blob := []byte(`{"schemaVersion":2}`)
	desc := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, blob)
	ctx, cancel := context.WithCancel(context.Background())
	store.deps.beforeBlobPromote = func() error {
		cancel()
		return nil
	}
	err := store.Push(ctx, desc, bytes.NewReader(blob))
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if _, statErr := layout.child.Lstat(rootStoreBlobPath(desc)); !os.IsNotExist(statErr) {
		t.Fatalf("canceled Push() published blob: %v", statErr)
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestRootOCIStoreCreateMetadataCancellationBeforeLinkDoesNotPublish(t *testing.T) {
	t.Parallel()

	layout, store := newRootOCIStoreForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closeFile := store.deps.closeFile
	store.deps.closeFile = func(file *os.File) error {
		err := closeFile(file)
		cancel()
		return err
	}

	const name = "canceled-metadata.json"
	err := store.createMetadata(ctx, name, []byte(`{"canceled":true}`))
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if _, statErr := layout.child.Lstat(name); !os.IsNotExist(statErr) {
		t.Fatalf("canceled metadata creation published %q: %v", name, statErr)
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestRootOCIStoreCancellationBeforeIndexPromotionPreservesIndex(t *testing.T) {
	t.Parallel()

	layout, store := newRootOCIStoreForTest(t)
	blob := []byte(`{"schemaVersion":2}`)
	desc := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, blob)
	if err := store.Push(context.Background(), desc, bytes.NewReader(blob)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(context.Background(), desc, "v1"); err != nil {
		t.Fatal(err)
	}
	before, err := layout.child.ReadFile(ociv1.ImageIndexFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	store.deps.beforeIndexPromote = func() error {
		cancel()
		return nil
	}
	err = store.Tag(ctx, desc, "v2")
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	after, err := layout.child.ReadFile(ociv1.ImageIndexFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("canceled Tag() changed index:\nbefore=%s\nafter=%s", before, after)
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestStageGenericOCIManifestMidVerificationCancellationPreservesState(t *testing.T) {
	t.Parallel()

	layout, store := newRootOCIStoreForTest(t)
	layerBytes := []byte("layer")
	layer := content.NewDescriptorFromBytes(ociv1.MediaTypeImageLayerGzip, layerBytes)
	if err := store.Push(context.Background(), layer, bytes.NewReader(layerBytes)); err != nil {
		t.Fatal(err)
	}
	indexBefore, err := layout.child.ReadFile(ociv1.ImageIndexFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx := newCancelAfterChecksContext()
	var stateAtCancellation []string
	store.deps.beforeBlobVerify = func() error {
		stateAtCancellation = snapshotRootStoreTree(t, layout.child)
		ctx.arm(4)
		return nil
	}
	manifest, err := stageGenericOCIManifest(ctx, store, "", layer, PackageOptions{Tag: "v1"})
	if !reflect.DeepEqual(manifest, ociv1.Descriptor{}) {
		t.Fatalf("canceled stage returned manifest: %+v", manifest)
	}
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if len(stateAtCancellation) == 0 {
		t.Fatal("manifest verification hook was not reached")
	}
	if got := snapshotRootStoreTree(t, layout.child); !reflect.DeepEqual(got, stateAtCancellation) {
		t.Fatalf("canceled manifest verification mutated store:\nbefore=%v\nafter=%v", stateAtCancellation, got)
	}
	indexAfter, err := layout.child.ReadFile(ociv1.ImageIndexFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexAfter, indexBefore) {
		t.Fatalf("canceled manifest verification changed index:\nbefore=%s\nafter=%s", indexBefore, indexAfter)
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestStageGenericOCIManifestMapsPlainFailuresToInternal(t *testing.T) {
	t.Parallel()

	injected := stderrors.New("plain local store failure")
	layer := content.NewDescriptorFromBytes(ociv1.MediaTypeImageLayerGzip, []byte("layer"))
	for _, tt := range []struct {
		name    string
		pushErr error
		tagErr  error
	}{
		{name: "pack manifest", pushErr: injected},
		{name: "tag manifest", tagErr: injected},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &plainFailureLocalStore{pushErr: tt.pushErr, tagErr: tt.tagErr}
			manifest, err := stageGenericOCIManifest(
				context.Background(), store, "", layer, PackageOptions{Tag: "v1"})
			if !reflect.DeepEqual(manifest, ociv1.Descriptor{}) {
				t.Fatalf("failed stage returned manifest: %+v", manifest)
			}
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
			if !stderrors.Is(err, injected) {
				t.Fatalf("stage error = %v, want %v", err, injected)
			}
		})
	}
}

func TestRootOCIStoreMetadataFailuresCleanIngestAndPreserveIndex(t *testing.T) {
	t.Parallel()

	injected := stderrors.New("injected metadata failure")
	for _, tt := range []struct {
		name   string
		inject func(*rootOCIStoreDependencies)
	}{
		{name: "write", inject: func(deps *rootOCIStoreDependencies) {
			deps.copy = func(context.Context, io.Writer, io.Reader) (int64, error) { return 0, injected }
		}},
		{name: "sync", inject: func(deps *rootOCIStoreDependencies) {
			deps.syncFile = func(*os.File) error { return injected }
		}},
		{name: "close", inject: func(deps *rootOCIStoreDependencies) {
			deps.closeFile = func(file *os.File) error { return stderrors.Join(file.Close(), injected) }
		}},
		{name: "promotion", inject: func(deps *rootOCIStoreDependencies) {
			deps.rename = func(*os.Root, string, string) error { return injected }
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			layout, store := newRootOCIStoreForTest(t)
			firstBytes := []byte(`{"schemaVersion":2,"first":true}`)
			secondBytes := []byte(`{"schemaVersion":2,"second":true}`)
			first := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, firstBytes)
			second := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, secondBytes)
			for _, item := range []struct {
				desc ociv1.Descriptor
				blob []byte
			}{{first, firstBytes}, {second, secondBytes}} {
				if err := store.Push(context.Background(), item.desc, bytes.NewReader(item.blob)); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Tag(context.Background(), first, "v1"); err != nil {
				t.Fatal(err)
			}
			before, err := layout.child.ReadFile(ociv1.ImageIndexFile)
			if err != nil {
				t.Fatal(err)
			}
			deps := defaultRootOCIStoreDependencies()
			tt.inject(&deps)
			store.deps = deps
			err = store.Tag(context.Background(), second, "v1")
			if !stderrors.Is(err, injected) {
				t.Fatalf("Tag() error = %v, want %v", err, injected)
			}
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
			after, err := layout.child.ReadFile(ociv1.ImageIndexFile)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("failed Tag() changed index:\nbefore=%s\nafter=%s", before, after)
			}
			assertRootIngestEmpty(t, layout.child)
		})
	}
}

func TestRootOCIStoreInitializationFailureRemovesPartialMetadata(t *testing.T) {
	t.Parallel()

	layout, err := newOwnedLayout(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := layout.Close(); closeErr != nil {
			t.Errorf("layout Close() error = %v", closeErr)
		}
	})
	injected := stderrors.New("index creation failed")
	deps := defaultRootOCIStoreDependencies()
	var links int
	defaultLink := deps.link
	deps.link = func(root *os.Root, oldName, newName string) error {
		links++
		if links == 2 {
			return injected
		}
		return defaultLink(root, oldName, newName)
	}
	store, err := newRootOCIStoreWithDependencies(context.Background(), layout, deps)
	if store != nil {
		t.Fatalf("failed initialization returned store: %+v", store)
	}
	if !stderrors.Is(err, injected) {
		t.Fatalf("newRootOCIStore() error = %v, want %v", err, injected)
	}
	for _, name := range []string{ociv1.ImageLayoutFile, ociv1.ImageIndexFile} {
		if _, statErr := layout.child.Lstat(name); !os.IsNotExist(statErr) {
			t.Fatalf("partial metadata %q remains: %v", name, statErr)
		}
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestRootOCIStoreInitializationCleanupFailureRollsBackMetadata(t *testing.T) {
	t.Parallel()

	layout, err := newOwnedLayout(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := layout.Close(); closeErr != nil {
			t.Errorf("layout Close() error = %v", closeErr)
		}
	})
	injected := stderrors.New("metadata temporary unlink failed")
	deps := defaultRootOCIStoreDependencies()
	defaultRemove := deps.remove
	var calls int
	deps.remove = func(root *os.Root, name string) error {
		calls++
		if calls == 1 {
			return injected
		}
		return defaultRemove(root, name)
	}
	store, err := newRootOCIStoreWithDependencies(context.Background(), layout, deps)
	if store != nil {
		t.Fatalf("failed initialization returned store: %+v", store)
	}
	if !stderrors.Is(err, injected) {
		t.Fatalf("newRootOCIStore() error = %v, want %v", err, injected)
	}
	for _, name := range []string{ociv1.ImageLayoutFile, ociv1.ImageIndexFile} {
		if _, statErr := layout.child.Lstat(name); !os.IsNotExist(statErr) {
			t.Fatalf("cleanup failure left metadata %q: %v", name, statErr)
		}
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestRootOCIStoreTagIsDeterministicAndTransactional(t *testing.T) {
	t.Parallel()

	layout, store := newRootOCIStoreForTest(t)
	firstBytes := []byte(`{"schemaVersion":2,"first":true}`)
	secondBytes := []byte(`{"schemaVersion":2,"second":true}`)
	first := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, firstBytes)
	second := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, secondBytes)
	for _, item := range []struct {
		desc ociv1.Descriptor
		blob []byte
	}{{first, firstBytes}, {second, secondBytes}} {
		if err := store.Push(context.Background(), item.desc, bytes.NewReader(item.blob)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Tag(context.Background(), first, "z"); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(context.Background(), first, "a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(context.Background(), second, "z"); err != nil {
		t.Fatal(err)
	}

	indexBytes, err := layout.child.ReadFile(ociv1.ImageIndexFile)
	if err != nil {
		t.Fatal(err)
	}
	var index ociv1.Index
	if unmarshalErr := json.Unmarshal(indexBytes, &index); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if got := []string{
		index.Manifests[0].Annotations[ociv1.AnnotationRefName],
		index.Manifests[1].Annotations[ociv1.AnnotationRefName],
	}; !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("index tag order = %v, want [a z]", got)
	}
	if !content.Equal(index.Manifests[0], first) || !content.Equal(index.Manifests[1], second) {
		t.Fatalf("index manifests = %+v, want a=first z=second", index.Manifests)
	}

	before := append([]byte(nil), indexBytes...)
	injected := stderrors.New("index promotion failed")
	store.deps.rename = func(*os.Root, string, string) error { return injected }
	if tagErr := store.Tag(context.Background(), first, "z"); !stderrors.Is(tagErr, injected) {
		t.Fatalf("Tag() error = %v, want %v", tagErr, injected)
	}
	after, err := layout.child.ReadFile(ociv1.ImageIndexFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("failed Tag() changed index:\nbefore=%s\nafter=%s", before, after)
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestRootOCIStoreRejectsInvalidTagsWithoutMutation(t *testing.T) {
	t.Parallel()

	layout, store := newRootOCIStoreForTest(t)
	blob := []byte(`{"schemaVersion":2}`)
	desc := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, blob)
	if err := store.Push(context.Background(), desc, bytes.NewReader(blob)); err != nil {
		t.Fatal(err)
	}
	before, err := layout.child.ReadFile(ociv1.ImageIndexFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{
		"",
		"bad tag",
		"bad/tag",
		"bad\x00tag",
		strings.Repeat("a", maximumDistributionTagBytes+1),
	} {
		tagErr := store.Tag(context.Background(), desc, tag)
		assertErrorCode(t, tagErr, apperrors.ErrCodeInvalidRequest)
		after, readErr := layout.child.ReadFile(ociv1.ImageIndexFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(after, before) {
			t.Fatalf("Tag(%q) changed index:\nbefore=%s\nafter=%s", tag, before, after)
		}
		assertRootIngestEmpty(t, layout.child)
	}
	boundary := "a" + strings.Repeat("b", maximumDistributionTagBytes-1)
	if err := store.Tag(context.Background(), desc, boundary); err != nil {
		t.Fatalf("Tag(%d-byte boundary) error = %v", len(boundary), err)
	}
}

func TestRootOCIStorePathSwapCannotRedirectBlobOrIndex(t *testing.T) {
	for _, phase := range []string{"blob promotion", "index promotion"} {
		t.Run(phase, func(t *testing.T) {
			layout, store := newRootOCIStoreForTest(t)
			blob := []byte(`{"schemaVersion":2}`)
			desc := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, blob)
			moved := layout.Path() + "-moved"
			replacement := layout.Path()
			swap := func() error {
				if err := os.Rename(layout.Path(), moved); err != nil {
					return err
				}
				if err := os.Mkdir(replacement, 0o700); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("replacement"), 0o600)
			}
			if phase == "blob promotion" {
				store.deps.beforeBlobPromote = swap
				if err := store.Push(context.Background(), desc, bytes.NewReader(blob)); err != nil {
					t.Fatalf("anchored Push() error = %v", err)
				}
			} else {
				if err := store.Push(context.Background(), desc, bytes.NewReader(blob)); err != nil {
					t.Fatal(err)
				}
				store.deps.beforeIndexPromote = swap
				if err := store.Tag(context.Background(), desc, "v1"); err != nil {
					t.Fatalf("anchored Tag() error = %v", err)
				}
			}
			if got := directoryEntryNames(t, replacement); !reflect.DeepEqual(got, []string{"sentinel"}) {
				t.Fatalf("replacement mutated: %v", got)
			}
			if _, err := os.Stat(filepath.Join(moved, rootStoreBlobPath(desc))); err != nil {
				t.Fatalf("retained root missing blob: %v", err)
			}
			if phase == "index promotion" {
				indexBytes, err := os.ReadFile(filepath.Join(moved, ociv1.ImageIndexFile))
				if err != nil || !bytes.Contains(indexBytes, []byte(`"v1"`)) {
					t.Fatalf("retained root index = %q, %v", indexBytes, err)
				}
			}

			// The test deliberately changed the visible identity. Close must not
			// mutate the replacement; clean both names explicitly afterwards.
			layout.finishOnce.Do(func() {})
			if err := layout.child.Close(); err != nil {
				t.Error(err)
			}
			if err := layout.parent.Close(); err != nil {
				t.Error(err)
			}
			if err := layout.backup.Close(); err != nil {
				t.Error(err)
			}
			if err := os.RemoveAll(replacement); err != nil {
				t.Error(err)
			}
			if err := os.RemoveAll(moved); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestRootOCIStorePathSwapDuringPackManifestCannotReachReplacement(t *testing.T) {
	layout, store := newRootOCIStoreForTest(t)
	layerBytes := []byte("layer")
	layer := content.NewDescriptorFromBytes("application/octet-stream", layerBytes)
	if err := store.Push(context.Background(), layer, bytes.NewReader(layerBytes)); err != nil {
		t.Fatal(err)
	}
	moved := layout.Path() + "-moved"
	replacement := layout.Path()
	var swapOnce sync.Once
	store.deps.beforeBlobPromote = func() error {
		var err error
		swapOnce.Do(func() {
			if renameErr := os.Rename(layout.Path(), moved); renameErr != nil {
				err = renameErr
				return
			}
			if mkdirErr := os.Mkdir(replacement, 0o700); mkdirErr != nil {
				err = mkdirErr
				return
			}
			err = os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("replacement"), 0o600)
		})
		return err
	}
	_, err := oras.PackManifest(context.Background(), store, oras.PackManifestVersion1_1,
		artifactType, oras.PackManifestOptions{
			Layers:              []ociv1.Descriptor{layer},
			ManifestAnnotations: map[string]string{ociv1.AnnotationCreated: reproducibleTimestamp},
		})
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if got := directoryEntryNames(t, replacement); !reflect.DeepEqual(got, []string{"sentinel"}) {
		t.Fatalf("replacement mutated: %v", got)
	}

	layout.finishOnce.Do(func() {})
	if err := layout.child.Close(); err != nil {
		t.Error(err)
	}
	if err := layout.parent.Close(); err != nil {
		t.Error(err)
	}
	if err := layout.backup.Close(); err != nil {
		t.Error(err)
	}
	if err := os.RemoveAll(replacement); err != nil {
		t.Error(err)
	}
	if err := os.RemoveAll(moved); err != nil {
		t.Error(err)
	}
}

func TestRootOCIStoreConcurrentPushAndTag(t *testing.T) {
	layout, store := newRootOCIStoreForTest(t)

	const count = 24
	descriptors := make([]ociv1.Descriptor, count)
	blobs := make([][]byte, count)
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := range count {
		blobs[i] = []byte{byte(i), byte(i + 1), byte(i + 2)}
		descriptors[i] = content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, blobs[i])
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := store.Push(context.Background(), descriptors[i], bytes.NewReader(blobs[i])); err != nil {
				errs <- err
				return
			}
			if err := store.Tag(context.Background(), descriptors[i], "tag-"+descriptors[i].Digest.Encoded()[:8]); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent operation error = %v", err)
	}
	indexBytes, err := layout.child.ReadFile(ociv1.ImageIndexFile)
	if err != nil {
		t.Fatal(err)
	}
	var index ociv1.Index
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != count {
		t.Fatalf("index manifests = %d, want %d", len(index.Manifests), count)
	}
	tags := make([]string, len(index.Manifests))
	for i := range index.Manifests {
		tags[i] = index.Manifests[i].Annotations[ociv1.AnnotationRefName]
	}
	if !sort.StringsAreSorted(tags) {
		t.Fatalf("index tags are not sorted: %v", tags)
	}
	assertRootIngestEmpty(t, layout.child)
}

func TestPackageReleasedRootStoreReopensAndCopiesExactDescriptor(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	result, err := Package(context.Background(), PackageOptions{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := storeoci.NewFromFS(context.Background(), os.DirFS(result.StorePath))
	if err != nil {
		t.Fatalf("reopen released layout: %v", err)
	}
	resolved, err := reopened.Resolve(context.Background(), "v1")
	if err != nil {
		t.Fatalf("Resolve(v1): %v", err)
	}
	if !content.Equal(resolved, result.Descriptor) {
		t.Fatalf("resolved = %+v, want exact %+v", resolved, result.Descriptor)
	}
	target := newTestPublicationTarget()
	if err := oras.CopyGraph(context.Background(), reopened, target, result.Descriptor, oras.DefaultCopyGraphOptions); err != nil {
		t.Fatalf("CopyGraph() error = %v", err)
	}
	if _, ok := target.blobs[result.Descriptor.Digest]; !ok {
		t.Fatal("CopyGraph() did not copy released root manifest")
	}
}

func TestPackageOperationReleaseRootCloseFailureReturnsNoResult(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	op, err := packageGenericOperation(context.Background(), PackageOptions{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	layoutPath := op.layout.Path()
	injected := stderrors.New("retained root close failed")
	defaultClose := op.layout.deps.closeRoot
	var closeOnce sync.Once
	op.layout.deps.closeRoot = func(root *os.Root) error {
		closeErr := defaultClose(root)
		closeOnce.Do(func() { closeErr = stderrors.Join(closeErr, injected) })
		return closeErr
	}
	result, err := op.release()
	if result != nil {
		t.Fatalf("release root close failure returned result: %+v", result)
	}
	if !stderrors.Is(err, injected) {
		t.Fatalf("release() error = %v, want %v", err, injected)
	}
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if _, statErr := os.Lstat(layoutPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed release left layout: %v", statErr)
	}
}

func newRootOCIStoreForTest(t *testing.T) (*ownedLayout, *rootOCIStore) {
	t.Helper()
	layout, err := newOwnedLayout(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := newRootOCIStore(context.Background(), layout)
	if err != nil {
		_ = layout.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !layout.released {
			if err := layout.Close(); err != nil {
				t.Errorf("layout Close() error = %v", err)
			}
		}
	})
	return layout, store
}

func assertRootIngestEmpty(t *testing.T, root *os.Root) {
	t.Helper()
	entries, err := fs.ReadDir(root.FS(), rootStoreIngestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s entries = %v, want empty", rootStoreIngestDir, entries)
	}
}

type rootStoreFailingReader struct {
	err error
}

func (r rootStoreFailingReader) Read([]byte) (int, error) {
	return 0, r.err
}

func snapshotRootStoreTree(t *testing.T, root *os.Root) []string {
	t.Helper()
	var snapshot []string
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := name + "|" + info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := fs.ReadFile(root.FS(), name)
			if err != nil {
				return err
			}
			item += "|" + string(data)
		}
		snapshot = append(snapshot, item)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(snapshot)
	return snapshot
}

type plainFailureLocalStore struct {
	pushErr error
	tagErr  error
}

func (s *plainFailureLocalStore) Fetch(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
	return nil, stderrors.New("unexpected fetch")
}

func (s *plainFailureLocalStore) Exists(context.Context, ociv1.Descriptor) (bool, error) {
	return false, nil
}

func (s *plainFailureLocalStore) Push(context.Context, ociv1.Descriptor, io.Reader) error {
	return s.pushErr
}

func (s *plainFailureLocalStore) Tag(context.Context, ociv1.Descriptor, string) error {
	return s.tagErr
}
