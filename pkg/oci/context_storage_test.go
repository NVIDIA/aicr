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
	stderrors "errors"
	"io"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

type testReadOnlyStorage struct {
	exists func(context.Context, ociv1.Descriptor) (bool, error)
	fetch  func(context.Context, ociv1.Descriptor) (io.ReadCloser, error)
}

func (s *testReadOnlyStorage) Exists(ctx context.Context, desc ociv1.Descriptor) (bool, error) {
	if s.exists == nil {
		return false, nil
	}
	return s.exists(ctx, desc)
}

func (s *testReadOnlyStorage) Fetch(ctx context.Context, desc ociv1.Descriptor) (io.ReadCloser, error) {
	if s.fetch == nil {
		return nil, stderrors.New("unexpected fetch")
	}
	return s.fetch(ctx, desc)
}

type testContentStorage struct {
	testReadOnlyStorage
	push func(context.Context, ociv1.Descriptor, io.Reader) error
}

func (s *testContentStorage) Push(ctx context.Context, desc ociv1.Descriptor, r io.Reader) error {
	if s.push == nil {
		return nil
	}
	return s.push(ctx, desc, r)
}

type closeUnblockedReader struct {
	data      []byte
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	closeErr  error
}

func newCloseUnblockedReader(data []byte) *closeUnblockedReader {
	return &closeUnblockedReader{
		data:    append([]byte(nil), data...),
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (r *closeUnblockedReader) Read(p []byte) (int, error) {
	r.startOnce.Do(func() { close(r.started) })
	<-r.closed
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *closeUnblockedReader) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return r.closeErr
}

type finiteCloseErrorReader struct {
	*bytes.Reader
	closeErr error
	closed   atomic.Int32
}

func (r *finiteCloseErrorReader) Close() error {
	r.closed.Add(1)
	return r.closeErr
}

func TestAttemptReadOnlyStorageCapabilityNarrowing(t *testing.T) {
	var value any = newAttemptReadOnlyStorage(context.Background(), &testReadOnlyStorage{})
	if _, ok := value.(content.ReadOnlyStorage); !ok {
		t.Fatal("attemptReadOnlyStorage must implement content.ReadOnlyStorage")
	}
	if _, ok := value.(content.Resolver); ok {
		t.Fatal("attemptReadOnlyStorage unexpectedly implements content.Resolver")
	}
	if _, ok := value.(content.Tagger); ok {
		t.Fatal("attemptReadOnlyStorage unexpectedly implements content.Tagger")
	}
	if _, ok := value.(oras.ReadOnlyTarget); ok {
		t.Fatal("attemptReadOnlyStorage unexpectedly implements oras.ReadOnlyTarget")
	}
	if _, ok := value.(content.ReadOnlyGraphStorage); ok {
		t.Fatal("attemptReadOnlyStorage unexpectedly implements content.ReadOnlyGraphStorage")
	}
	if _, ok := value.(registry.ReferenceFetcher); ok {
		t.Fatal("attemptReadOnlyStorage unexpectedly implements registry.ReferenceFetcher")
	}

	var destination any = newCopyGraphStorage(&testContentStorage{})
	if _, ok := destination.(content.Storage); !ok {
		t.Fatal("copyGraphStorage must implement content.Storage")
	}
	if _, ok := destination.(content.Resolver); ok {
		t.Fatal("copyGraphStorage unexpectedly implements content.Resolver")
	}
	if _, ok := destination.(content.Tagger); ok {
		t.Fatal("copyGraphStorage unexpectedly implements content.Tagger")
	}
	if _, ok := destination.(oras.Target); ok {
		t.Fatal("copyGraphStorage unexpectedly implements oras.Target")
	}
	if _, ok := destination.(content.Deleter); ok {
		t.Fatal("copyGraphStorage unexpectedly implements content.Deleter")
	}
	if _, ok := destination.(content.GraphStorage); ok {
		t.Fatal("copyGraphStorage unexpectedly implements content.GraphStorage")
	}
	if _, ok := destination.(registry.Mounter); ok {
		t.Fatal("copyGraphStorage unexpectedly implements registry.Mounter")
	}
	if _, ok := destination.(registry.ReferencePusher); ok {
		t.Fatal("copyGraphStorage unexpectedly implements registry.ReferencePusher")
	}
}

func TestAttemptReadOnlyStorageExistsCancellationChecks(t *testing.T) {
	t.Run("bound context canceled before delegate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var called atomic.Bool
		storage := newAttemptReadOnlyStorage(ctx, &testReadOnlyStorage{
			exists: func(context.Context, ociv1.Descriptor) (bool, error) {
				called.Store(true)
				return true, nil
			},
		})
		_, err := storage.Exists(context.Background(), ociv1.Descriptor{})
		assertErrorCode(t, err, apperrors.ErrCodeTimeout)
		if called.Load() {
			t.Fatal("Exists delegated after bound context cancellation")
		}
		if err := storage.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	t.Run("method context canceled before delegate", func(t *testing.T) {
		methodCtx, cancel := context.WithCancel(context.Background())
		cancel()
		var called atomic.Bool
		storage := newAttemptReadOnlyStorage(context.Background(), &testReadOnlyStorage{
			exists: func(context.Context, ociv1.Descriptor) (bool, error) {
				called.Store(true)
				return true, nil
			},
		})
		_, err := storage.Exists(methodCtx, ociv1.Descriptor{})
		assertErrorCode(t, err, apperrors.ErrCodeTimeout)
		if called.Load() {
			t.Fatal("Exists delegated after method context cancellation")
		}
		if err := storage.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	t.Run("bound context canceled by delegate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		storage := newAttemptReadOnlyStorage(ctx, &testReadOnlyStorage{
			exists: func(context.Context, ociv1.Descriptor) (bool, error) {
				cancel()
				return true, nil
			},
		})
		_, err := storage.Exists(context.Background(), ociv1.Descriptor{})
		assertErrorCode(t, err, apperrors.ErrCodeTimeout)
		if err := storage.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	t.Run("method context canceled by delegate", func(t *testing.T) {
		methodCtx, cancel := context.WithCancel(context.Background())
		storage := newAttemptReadOnlyStorage(context.Background(), &testReadOnlyStorage{
			exists: func(context.Context, ociv1.Descriptor) (bool, error) {
				cancel()
				return true, nil
			},
		})
		_, err := storage.Exists(methodCtx, ociv1.Descriptor{})
		assertErrorCode(t, err, apperrors.ErrCodeTimeout)
		if err := storage.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
}

func TestAttemptReadOnlyStorageFetchTracksReaderOnCancellationAndError(t *testing.T) {
	closeErr := stderrors.New("tracked close failed")
	boundCtx, cancel := context.WithCancel(context.Background())
	reader := &finiteCloseErrorReader{Reader: bytes.NewReader([]byte("payload")), closeErr: closeErr}
	storage := newAttemptReadOnlyStorage(boundCtx, &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
			cancel()
			return reader, stderrors.New("fetch also failed")
		},
	})

	returned, err := storage.Fetch(context.Background(), content.NewDescriptorFromBytes("test/data", []byte("payload")))
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if returned == nil {
		t.Fatal("Fetch() discarded a non-nil reader returned with cancellation/error")
	}
	closeResult := storage.Close()
	if !stderrors.Is(closeResult, closeErr) {
		t.Fatalf("Close() error = %v, want tracked %v", closeResult, closeErr)
	}
	if got := reader.closed.Load(); got != 1 {
		t.Fatalf("underlying Close calls = %d, want 1", got)
	}
	if again := storage.Close(); !stderrors.Is(again, closeErr) || again.Error() != closeResult.Error() {
		t.Fatalf("repeated Close() = %v, want cached %v", again, closeResult)
	}
}

func TestAttemptReadOnlyStorageRejectsNilReaderWithoutError(t *testing.T) {
	storage := newAttemptReadOnlyStorage(context.Background(), &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
			return nil, nil //nolint:nilnil // Exercise rejection of an invalid storage implementation.
		},
	})
	reader, err := storage.Fetch(context.Background(), validTestManifestDescriptor())
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if reader != nil {
		t.Fatalf("Fetch() reader = %T, want nil", reader)
	}
	if closeErr := storage.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
}

func TestAttemptReadOnlyStorageCancelUnblocksReturnedReader(t *testing.T) {
	for _, mediaType := range []string{ociv1.MediaTypeImageManifest, ociv1.MediaTypeImageConfig} {
		t.Run(mediaType, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			underlying := newCloseUnblockedReader(nil)
			storage := newAttemptReadOnlyStorage(ctx, &testReadOnlyStorage{
				fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
					return underlying, nil
				},
			})
			desc := content.NewDescriptorFromBytes(mediaType, nil)
			reader, err := storage.Fetch(context.Background(), desc)
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			readDone := make(chan error, 1)
			go func() {
				_, readErr := reader.Read(make([]byte, 1))
				readDone <- readErr
			}()
			<-underlying.started
			cancel()
			readErr := <-readDone
			assertErrorCode(t, readErr, apperrors.ErrCodeTimeout)
			select {
			case <-underlying.closed:
			default:
				t.Fatal("context cancellation did not close the underlying reader")
			}
			wrapped, ok := reader.(*contextReadCloser)
			if !ok {
				t.Fatalf("Fetch() reader type = %T, want *contextReadCloser", reader)
			}
			if closeErr := storage.Close(); closeErr != nil {
				t.Fatalf("Close() error = %v", closeErr)
			}
			select {
			case <-wrapped.done:
			default:
				t.Fatal("reader cancellation watcher was not joined")
			}
		})
	}
}

func TestAttemptReadOnlyStorageConcurrentCloseIsCheckedAndCached(t *testing.T) {
	closeErr := stderrors.New("close failed")
	reader := &finiteCloseErrorReader{Reader: bytes.NewReader([]byte("payload")), closeErr: closeErr}
	storage := newAttemptReadOnlyStorage(context.Background(), &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) { return reader, nil },
	})
	if _, err := storage.Fetch(context.Background(), content.NewDescriptorFromBytes("test/data", []byte("payload"))); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	const callers = 12
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- storage.Close()
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if !stderrors.Is(err, closeErr) {
			t.Errorf("Close() error = %v, want %v", err, closeErr)
		}
	}
	if got := reader.closed.Load(); got != 1 {
		t.Fatalf("underlying Close calls = %d, want 1", got)
	}
	if _, err := storage.Exists(context.Background(), ociv1.Descriptor{}); err == nil {
		t.Fatal("Exists() after Close() unexpectedly succeeded")
	}
	if reader, err := storage.Fetch(context.Background(), ociv1.Descriptor{}); err == nil || reader != nil {
		t.Fatalf("Fetch() after Close() = (%T, %v), want nil,error", reader, err)
	}
}

func TestCopyGraphStorageFetchDelegates(t *testing.T) {
	fetchErr := stderrors.New("fetch failed")
	for _, tt := range []struct {
		name    string
		wantErr error
	}{
		{name: "success"},
		{name: "error", wantErr: fetchErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			desc := content.NewDescriptorFromBytes("test/data", []byte("payload"))
			desc.Annotations = map[string]string{"test": tt.name}
			wantReader := &finiteCloseErrorReader{Reader: bytes.NewReader([]byte("payload"))}
			target := &testContentStorage{testReadOnlyStorage: testReadOnlyStorage{
				fetch: func(gotCtx context.Context, gotDesc ociv1.Descriptor) (io.ReadCloser, error) {
					if gotCtx != ctx {
						t.Errorf("Fetch() context = %v, want original context", gotCtx)
					}
					if !reflect.DeepEqual(gotDesc, desc) {
						t.Errorf("Fetch() descriptor = %#v, want %#v", gotDesc, desc)
					}
					return wantReader, tt.wantErr
				},
			}}

			gotReader, err := newCopyGraphStorage(target).Fetch(ctx, desc)
			if gotReader != wantReader {
				t.Fatalf("Fetch() reader = %T, want delegated reader %T", gotReader, wantReader)
			}
			if !stderrors.Is(err, tt.wantErr) {
				t.Fatalf("Fetch() error = %v, want %v", err, tt.wantErr)
			}
			if closeErr := gotReader.Close(); closeErr != nil {
				t.Fatalf("Fetch() reader Close() error = %v", closeErr)
			}
		})
	}
}

func TestCopyGraphWithTrackedSourceVerifiesRootBeforeDestination(t *testing.T) {
	payload := []byte("manifest")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, payload)
	var destinationCalls atomic.Int32
	destination := &testContentStorage{
		testReadOnlyStorage: testReadOnlyStorage{
			exists: func(context.Context, ociv1.Descriptor) (bool, error) {
				destinationCalls.Add(1)
				return false, nil
			},
		},
		push: func(context.Context, ociv1.Descriptor, io.Reader) error {
			destinationCalls.Add(1)
			return nil
		},
	}
	source := &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte("corrupt"))), nil
		},
	}
	copyCalled := atomic.Bool{}
	err := copyGraphWithTrackedSource(
		context.Background(), source, destination, root, apperrors.ErrCodeInvalidRequest,
		oras.DefaultCopyGraphOptions,
		func(context.Context, content.ReadOnlyStorage, content.Storage, ociv1.Descriptor, oras.CopyGraphOptions) error {
			copyCalled.Store(true)
			return nil
		},
	)
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
	if copyCalled.Load() || destinationCalls.Load() != 0 {
		t.Fatalf("copy/destination called before exact local-root verification: copy=%v dst=%d",
			copyCalled.Load(), destinationCalls.Load())
	}
}

func TestCopyGraphWithTrackedSourceCapabilityOrderAndClose(t *testing.T) {
	rootBytes := []byte("manifest")
	configBytes := []byte("config")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	config := content.NewDescriptorFromBytes(ociv1.MediaTypeImageConfig, configBytes)
	var mu sync.Mutex
	var order []string
	appendOrder := func(value string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, value)
	}
	configReader := &orderedReadCloser{
		Reader: bytes.NewReader(configBytes),
		close:  func() error { appendOrder("tracked-source Close"); return nil },
	}
	source := &testReadOnlyStorage{
		fetch: func(_ context.Context, desc ociv1.Descriptor) (io.ReadCloser, error) {
			switch desc.Digest {
			case root.Digest:
				return &orderedReadCloser{Reader: bytes.NewReader(rootBytes), close: func() error {
					appendOrder("local root close")
					return nil
				}}, nil
			case config.Digest:
				return configReader, nil
			default:
				return nil, stderrors.New("unknown descriptor")
			}
		},
	}
	destination := &testContentStorage{}
	err := copyGraphWithTrackedSource(
		context.Background(), source, destination, root, apperrors.ErrCodeInternal,
		oras.DefaultCopyGraphOptions,
		func(copyCtx context.Context, src content.ReadOnlyStorage, dst content.Storage, _ ociv1.Descriptor, _ oras.CopyGraphOptions) error {
			if _, ok := any(src).(content.Resolver); ok {
				t.Error("copy source exposes content.Resolver")
			}
			if _, ok := any(dst).(content.Tagger); ok {
				t.Error("copy destination exposes content.Tagger")
			}
			appendOrder("CopyGraph")
			r, fetchErr := src.Fetch(copyCtx, config)
			if fetchErr != nil {
				return fetchErr
			}
			_, readErr := io.Copy(io.Discard, r)
			// Deliberately omit r.Close(), matching vendored CopyGraph's ignored defer error.
			return readErr
		},
	)
	if err != nil {
		t.Fatalf("copyGraphWithTrackedSource() error = %v", err)
	}
	want := []string{"local root close", "CopyGraph", "tracked-source Close"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestCopyGraphWithTrackedSourceCancellationJoinsBlockedReader(t *testing.T) {
	rootBytes := []byte("manifest")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	ctx, cancel := context.WithCancel(context.Background())
	reader := newCloseUnblockedReader(rootBytes)
	source := &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) { return reader, nil },
	}
	copyCalled := atomic.Bool{}
	done := make(chan error, 1)
	go func() {
		done <- copyGraphWithTrackedSource(
			ctx, source, &testContentStorage{}, root, apperrors.ErrCodeInternal,
			oras.DefaultCopyGraphOptions,
			func(context.Context, content.ReadOnlyStorage, content.Storage, ociv1.Descriptor, oras.CopyGraphOptions) error {
				copyCalled.Store(true)
				return nil
			},
		)
	}()
	<-reader.started
	cancel()
	err := <-done
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if copyCalled.Load() {
		t.Fatal("CopyGraph called after local-root read cancellation")
	}
	select {
	case <-reader.closed:
	default:
		t.Fatal("blocked local-root reader was not closed")
	}
}

func TestCopyGraphWithTrackedSourceCancellationWinsRootCloseError(t *testing.T) {
	rootBytes := []byte("manifest")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closeErr := stderrors.New("root close failed after cancellation")
	var closeCalls atomic.Int32
	source := &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
			return &orderedReadCloser{Reader: bytes.NewReader(rootBytes), close: func() error {
				closeCalls.Add(1)
				cancel()
				return closeErr
			}}, nil
		},
	}
	copyCalled := atomic.Bool{}
	err := copyGraphWithTrackedSource(
		ctx, source, &testContentStorage{}, root, apperrors.ErrCodeInternal,
		oras.DefaultCopyGraphOptions,
		func(context.Context, content.ReadOnlyStorage, content.Storage, ociv1.Descriptor, oras.CopyGraphOptions) error {
			copyCalled.Store(true)
			return nil
		},
	)
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if copyCalled.Load() {
		t.Fatal("CopyGraph called after root-close cancellation")
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("root close calls = %d, want 1", got)
	}
}

func TestCopyGraphWithTrackedSourceReaderCloseErrors(t *testing.T) {
	rootBytes := []byte("manifest")
	configBytes := []byte("config")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	config := content.NewDescriptorFromBytes(ociv1.MediaTypeImageConfig, configBytes)
	closeErr := stderrors.New("ignored reader close failed")
	primary := apperrors.New(apperrors.ErrCodeInvalidRequest, "graph rejected")

	for _, tt := range []struct {
		name      string
		graphErr  error
		wantCode  apperrors.ErrorCode
		wantCause error
	}{
		{name: "close-only promoted", wantCode: apperrors.ErrCodeInternal, wantCause: closeErr},
		{name: "primary preserved", graphErr: primary, wantCode: apperrors.ErrCodeInvalidRequest, wantCause: primary},
	} {
		t.Run(tt.name, func(t *testing.T) {
			closeReader := &finiteCloseErrorReader{Reader: bytes.NewReader(configBytes), closeErr: closeErr}
			source := &testReadOnlyStorage{
				fetch: func(_ context.Context, desc ociv1.Descriptor) (io.ReadCloser, error) {
					if desc.Digest == root.Digest {
						return io.NopCloser(bytes.NewReader(rootBytes)), nil
					}
					return closeReader, nil
				},
			}
			err := copyGraphWithTrackedSource(
				context.Background(), source, &testContentStorage{}, root, apperrors.ErrCodeInternal,
				oras.DefaultCopyGraphOptions,
				func(copyCtx context.Context, src content.ReadOnlyStorage, _ content.Storage, _ ociv1.Descriptor, _ oras.CopyGraphOptions) error {
					r, fetchErr := src.Fetch(copyCtx, config)
					if fetchErr != nil {
						return fetchErr
					}
					_, _ = io.Copy(io.Discard, r)
					return tt.graphErr
				},
			)
			assertErrorCode(t, err, tt.wantCode)
			if !stderrors.Is(err, tt.wantCause) {
				t.Fatalf("error = %v, want cause %v", err, tt.wantCause)
			}
			if got := closeReader.closed.Load(); got != 1 {
				t.Fatalf("reader Close calls = %d, want 1", got)
			}
		})
	}
}

func TestCopyGraphWithTrackedSourceMapsChildSourceFailure(t *testing.T) {
	rootBytes := []byte("manifest")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	source := &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rootBytes)), nil
		},
	}
	for _, code := range []apperrors.ErrorCode{apperrors.ErrCodeInternal, apperrors.ErrCodeInvalidRequest} {
		t.Run(string(code), func(t *testing.T) {
			err := copyGraphWithTrackedSource(
				context.Background(), source, &testContentStorage{}, root, code,
				oras.DefaultCopyGraphOptions,
				func(context.Context, content.ReadOnlyStorage, content.Storage, ociv1.Descriptor, oras.CopyGraphOptions) error {
					return &oras.CopyError{Op: "Fetch", Origin: oras.CopyErrorOriginSource, Err: os.ErrNotExist}
				},
			)
			assertErrorCode(t, err, code)
		})
	}
}

type orderedReadCloser struct {
	*bytes.Reader
	close func() error
	once  sync.Once
	err   error
}

func (r *orderedReadCloser) Close() error {
	r.once.Do(func() { r.err = r.close() })
	return r.err
}

func assertErrorCode(t *testing.T, err error, code apperrors.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", code)
	}
	if !stderrors.Is(err, apperrors.New(code, "")) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}
