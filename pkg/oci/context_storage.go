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
	stderrors "errors"
	"io"
	"log/slog"
	"sync"

	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

type contextReadCloser struct {
	boundCtx  context.Context
	methodCtx context.Context
	reader    io.ReadCloser
	cleanup   func()

	stop        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	cleanupOnce sync.Once
	closeOnce   sync.Once
	closeErr    error
}

func newContextReadCloser(ctx context.Context, r io.ReadCloser) *contextReadCloser {
	return newContextReadCloserForContexts(ctx, ctx, r, nil)
}

func newContextReadCloserForContexts(
	boundCtx context.Context,
	methodCtx context.Context,
	r io.ReadCloser,
	cleanup func(),
) *contextReadCloser {

	reader := &contextReadCloser{
		boundCtx:  boundCtx,
		methodCtx: methodCtx,
		reader:    r,
		cleanup:   cleanup,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go reader.watchCancellation()
	return reader
}

func (r *contextReadCloser) watchCancellation() {
	defer close(r.done)
	select {
	case <-r.boundCtx.Done():
		r.closeUnderlying()
	case <-r.methodCtx.Done():
		r.closeUnderlying()
	case <-r.stop:
	}
}

func (r *contextReadCloser) Read(p []byte) (int, error) {
	if err := contextOperationError(r.boundCtx, r.methodCtx); err != nil {
		return 0, err
	}

	n, err := r.reader.Read(p)
	if ctxErr := contextOperationError(r.boundCtx, r.methodCtx); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func (r *contextReadCloser) Close() error {
	r.stopOnce.Do(func() { close(r.stop) })
	r.closeUnderlying()
	<-r.done
	r.cleanupOnce.Do(func() {
		if r.cleanup != nil {
			r.cleanup()
		}
	})
	return r.closeErr
}

func (r *contextReadCloser) closeUnderlying() {
	r.closeOnce.Do(func() {
		r.closeErr = r.reader.Close()
	})
}

type attemptReadOnlyStorage struct {
	ctx    context.Context
	source content.ReadOnlyStorage

	mu        sync.Mutex
	inFlight  sync.WaitGroup
	readers   []*contextReadCloser
	closing   bool
	closeOnce sync.Once
	closeErr  error
}

var _ content.ReadOnlyStorage = (*attemptReadOnlyStorage)(nil)

func newAttemptReadOnlyStorage(
	ctx context.Context,
	source content.ReadOnlyStorage,
) *attemptReadOnlyStorage {

	return &attemptReadOnlyStorage{ctx: ctx, source: source}
}

func (s *attemptReadOnlyStorage) Exists(
	ctx context.Context,
	target ociv1.Descriptor,
) (bool, error) {

	if err := s.begin(); err != nil {
		return false, err
	}
	defer s.inFlight.Done()

	if err := contextOperationError(s.ctx, ctx); err != nil {
		return false, err
	}
	callCtx, cleanup := combineContexts(s.ctx, ctx)
	exists, err := s.source.Exists(callCtx, target)
	cleanup()
	if ctxErr := contextOperationError(s.ctx, ctx); ctxErr != nil {
		return false, ctxErr
	}
	return exists, err
}

func (s *attemptReadOnlyStorage) Fetch(
	ctx context.Context,
	target ociv1.Descriptor,
) (io.ReadCloser, error) {

	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.inFlight.Done()

	if err := contextOperationError(s.ctx, ctx); err != nil {
		return nil, err
	}
	callCtx, cleanup := combineContexts(s.ctx, ctx)
	reader, err := s.source.Fetch(callCtx, target)
	var tracked *contextReadCloser
	if reader == nil {
		cleanup()
	} else {
		tracked = newContextReadCloserForContexts(s.ctx, ctx, reader, cleanup)
		s.mu.Lock()
		s.readers = append(s.readers, tracked)
		s.mu.Unlock()
	}

	if ctxErr := contextOperationError(s.ctx, ctx); ctxErr != nil {
		return tracked, ctxErr
	}
	if err != nil {
		return tracked, err
	}
	if tracked == nil {
		return nil, apperrors.New(apperrors.ErrCodeInternal, "OCI source returned a nil reader")
	}
	return tracked, nil
}

func (s *attemptReadOnlyStorage) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()

		s.inFlight.Wait()

		s.mu.Lock()
		readers := append([]*contextReadCloser(nil), s.readers...)
		s.mu.Unlock()

		closeErrors := make([]error, 0, len(readers))
		for _, reader := range readers {
			if err := reader.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
		s.closeErr = stderrors.Join(closeErrors...)
	})
	return s.closeErr
}

func (s *attemptReadOnlyStorage) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return apperrors.New(apperrors.ErrCodeInternal, "OCI source is already finalized")
	}
	s.inFlight.Add(1)
	return nil
}

type copyGraphStorage struct {
	target content.Storage
}

type publicationTargetCore interface {
	content.Storage
	content.TagResolver
}

type publicationTarget interface {
	publicationTargetCore
	CloseIdleConnections()
}

var _ content.Storage = (*copyGraphStorage)(nil)

func newCopyGraphStorage(target content.Storage) *copyGraphStorage {
	return &copyGraphStorage{target: target}
}

func (s *copyGraphStorage) Exists(
	ctx context.Context,
	target ociv1.Descriptor,
) (bool, error) {

	return s.target.Exists(ctx, target)
}

func (s *copyGraphStorage) Fetch(
	ctx context.Context,
	target ociv1.Descriptor,
) (io.ReadCloser, error) {

	return s.target.Fetch(ctx, target)
}

func (s *copyGraphStorage) Push(
	ctx context.Context,
	expected ociv1.Descriptor,
	r io.Reader,
) error {

	return s.target.Push(ctx, expected, r)
}

type copyGraphFunc func(
	ctx context.Context,
	src content.ReadOnlyStorage,
	dst content.Storage,
	root ociv1.Descriptor,
	opts oras.CopyGraphOptions,
) error

func copyGraphWithTrackedSource(
	ctx context.Context,
	src content.ReadOnlyStorage,
	dst content.Storage,
	root ociv1.Descriptor,
	sourceFailureCode apperrors.ErrorCode,
	opts oras.CopyGraphOptions,
	copyGraph copyGraphFunc,
) error {

	attempt := newAttemptReadOnlyStorage(ctx, src)

	rootReader, fetchErr := attempt.Fetch(ctx, root)
	if fetchErr != nil {
		primary := mapSourceFailure(fetchErr, sourceFailureCode, "failed to fetch local OCI root")
		return finalizeAttempt(primary, attempt.Close())
	}

	verifyReader := content.NewVerifyReader(rootReader, root)
	_, readErr := io.Copy(io.Discard, verifyReader)
	if readErr == nil {
		readErr = verifyReader.Verify()
	}
	rootCloseErr := rootReader.Close()
	if readErr != nil {
		primary := mapSourceFailure(readErr, sourceFailureCode, "failed to verify local OCI root")
		return finalizeAttempt(primary, attempt.Close())
	}
	if ctxErr := contextOperationError(ctx); ctxErr != nil {
		return finalizeAttempt(ctxErr, attempt.Close())
	}
	if rootCloseErr != nil {
		closeErr := attempt.Close()
		if closeErr == nil {
			closeErr = rootCloseErr
		}
		return finalizeAttempt(nil, closeErr)
	}
	graphErr := copyGraph(ctx, attempt, newCopyGraphStorage(dst), root, opts)
	if ctxErr := contextOperationError(ctx); ctxErr != nil {
		graphErr = ctxErr
	}
	primary := mapGraphFailure(graphErr, sourceFailureCode)
	return finalizeAttempt(primary, attempt.Close())
}

func combineContexts(boundCtx, methodCtx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(boundCtx)
	stop := context.AfterFunc(methodCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func contextOperationError(contexts ...context.Context) error {
	for _, ctx := range contexts {
		if err := ctx.Err(); err != nil {
			return apperrors.Wrap(apperrors.ErrCodeTimeout, "OCI content operation canceled", err)
		}
	}
	return nil
}

func mapSourceFailure(err error, code apperrors.ErrorCode, message string) error {
	if err == nil {
		return nil
	}
	if apperrors.IsTransient(err) {
		return apperrors.Wrap(apperrors.ErrCodeTimeout, message, err)
	}
	return apperrors.Wrap(code, message, err)
}

func mapGraphFailure(err error, sourceFailureCode apperrors.ErrorCode) error {
	if err == nil {
		return nil
	}
	if apperrors.IsTransient(err) {
		return apperrors.Wrap(apperrors.ErrCodeTimeout, "OCI graph copy canceled", err)
	}
	var copyErr *oras.CopyError
	if stderrors.As(err, &copyErr) && copyErr.Origin == oras.CopyErrorOriginSource {
		return apperrors.Wrap(sourceFailureCode, "failed to copy local OCI content", err)
	}
	return err
}

func finalizeAttempt(primary, closeErr error) error {
	if primary != nil {
		if closeErr != nil {
			slog.Warn("failed to close OCI copy source after primary error", "error", closeErr, "primary", primary)
		}
		return primary
	}
	if closeErr != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close OCI copy source", closeErr)
	}
	return nil
}
