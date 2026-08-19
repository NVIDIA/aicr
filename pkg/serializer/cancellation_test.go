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

package serializer

import (
	"bytes"
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// wantCanceledAndNotTransient asserts the abort contract every read path in
// this package must satisfy: an operator cancellation is coded ErrCodeCanceled
// and reports non-transient.
//
// Non-transience is the half that matters operationally. errors.IsTransient
// returns true for ErrCodeTimeout, for ErrCodeUnavailable wrapping a context
// error, and for a bare context.Canceled — so a wrong code here lets a
// caller's retry loop re-enter on an abort the operator explicitly requested.
func wantCanceledAndNotTransient(t *testing.T, err error, source string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error for a canceled context", source)
	}
	if !stderrors.Is(err, context.Canceled) {
		t.Errorf("%s: error = %v, want one wrapping context.Canceled", source, err)
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) || se.Code != errors.ErrCodeCanceled {
		t.Errorf("%s: error = %v, want code %v", source, err, errors.ErrCodeCanceled)
	}
	if errors.IsTransient(err) {
		t.Errorf("%s: canceled read reports transient; a retry loop would re-enter on an operator abort", source)
	}
}

// TestCancellation_LocalFile covers the local-file read path.
func TestCancellation_LocalFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "doc.yaml")
	if err := os.WriteFile(path, []byte("kind: Snapshot\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewFileReaderWithContext(ctx, FormatYAML, path)
	wantCanceledAndNotTransient(t, err, "local file")
}

// TestCancellation_HTTP covers the HTTP(S) read path.
//
// The context error surfaces from Client.Do wrapped in a *url.Error, which the
// transport path would otherwise class as ErrCodeUnavailable — a code that is
// still transient, so the abort would be retried.
func TestCancellation_HTTP(t *testing.T) {
	t.Parallel()

	// Block until the client gives up so the cancellation, not the response,
	// ends the request.
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
	}))
	t.Cleanup(func() {
		close(released)
		srv.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	t.Cleanup(cancel)

	_, err := NewHTTPReader().ReadWithContext(ctx, srv.URL)
	wantCanceledAndNotTransient(t, err, "http")
}

// TestCancellation_ConfigMap covers the cm:// read path's classifier.
//
// Exercised directly rather than through a fake clientset because the
// classification is the whole behavior under test: the Get error arrives from
// client-go, and the bug was that this function grouped context.Canceled with
// the deadline and apiserver-timeout cases.
func TestCancellation_ConfigMap(t *testing.T) {
	t.Parallel()

	err := classifyConfigMapGetError("aicr", "snapshot", context.Canceled)
	wantCanceledAndNotTransient(t, err, "configmap")
}

// TestCancellation_DeadlineStaysTimeout guards the other direction: only a
// deliberate abort becomes ErrCodeCanceled. An environmental deadline is a
// retryable condition and must keep reporting as one, or the fix above would
// have suppressed legitimate retries.
func TestCancellation_DeadlineStaysTimeout(t *testing.T) {
	t.Parallel()

	err := classifyConfigMapGetError("aicr", "snapshot", context.DeadlineExceeded)
	if err == nil {
		t.Fatal("expected an error for a deadline")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) || se.Code != errors.ErrCodeTimeout {
		t.Errorf("error = %v, want code %v", err, errors.ErrCodeTimeout)
	}
	if !errors.IsTransient(err) {
		t.Error("a deadline must stay transient so legitimate retries still happen")
	}
}

// TestCancellation_HTTPDeadlineStaysTimeout pins the other half of the HTTP
// mapping. Only an abort becomes ErrCodeCanceled; a deadline is an
// environmental fault and must stay Timeout AND transient, or the fix above
// would have suppressed legitimate retries on a slow server.
func TestCancellation_HTTPDeadlineStaysTimeout(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
	}))
	t.Cleanup(func() {
		close(released)
		srv.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)

	_, err := NewHTTPReader().ReadWithContext(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected an error for an expired deadline")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) || se.Code != errors.ErrCodeTimeout {
		t.Errorf("error = %v, want code %v", err, errors.ErrCodeTimeout)
	}
	if !errors.IsTransient(err) {
		t.Error("an HTTP deadline must stay transient so legitimate retries still happen")
	}
}

// TestCancellation_HTTPTemplate covers the template loader's HTTP boundary.
//
// readTemplateContent wraps whatever ReadWithContext returns. Wrapping with a
// fixed ErrCodeUnavailable silently undid the classification one call down,
// making an abort transient again — the classification has to survive every
// layer between the cancellation and the caller, not just the one that
// observes it.
func TestCancellation_HTTPTemplate(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
	}))
	t.Cleanup(func() {
		close(released)
		srv.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	t.Cleanup(cancel)

	var buf bytes.Buffer
	err := NewTemplateWriter(srv.URL, &buf).Serialize(ctx, struct{}{})
	wantCanceledAndNotTransient(t, err, "http template")
}

// blockingBody is a response body that never yields data until the context is
// done, then reports why. It makes the body-read cancellation deterministic:
// a real server races the client's header processing, so a cancel fired from
// the handler can land while Client.Do is still returning and never reach the
// read at all.
type blockingBody struct{ ctx context.Context } //nolint:containedctx // the body's whole job is to block on it

func (b blockingBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b blockingBody) Close() error { return nil }

// blockingTransport returns headers immediately with a body that blocks, so
// Client.Do succeeds and the failure can only come from io.ReadAll.
type blockingTransport struct{}

func (blockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       blockingBody{ctx: req.Context()},
		Request:    req,
	}, nil
}

// TestCancellation_HTTPDuringBodyRead exercises the io.ReadAll branch, which
// TestCancellation_HTTP does not reach.
//
// That test cancels before response headers arrive, so it fails inside
// Client.Do and never enters the body read — the body-read classification had
// no coverage at all. Verified by mutation: deleting that branch left the
// whole package green.
//
// The message assertion pins this to the intended branch. Without it a timing
// shift could satisfy the test through the Do path, which is exactly how the
// gap went unnoticed.
func TestCancellation_HTTPDuringBodyRead(t *testing.T) {
	t.Parallel()

	reader := NewHTTPReader()
	reader.Client = &http.Client{Transport: blockingTransport{}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	t.Cleanup(cancel)

	_, err := reader.ReadWithContext(ctx, "http://body-read.test/snapshot.yaml")
	wantCanceledAndNotTransient(t, err, "http body read")
	if err != nil && !strings.Contains(err.Error(), "body read") {
		t.Errorf("error = %v, want it to come from the body-read branch", err)
	}
}
