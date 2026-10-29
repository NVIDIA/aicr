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

package helm

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// flakyRenderer fails the first failures calls with err, then returns out.
type flakyRenderer struct {
	failures int
	err      error
	out      []byte
	calls    int
}

func (f *flakyRenderer) Render(context.Context, ChartInput) ([]byte, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, f.err
	}
	return f.out, nil
}

func TestWithRetry(t *testing.T) {
	rr, ok := WithRetry(&flakyRenderer{}).(*retryRenderer)
	if !ok || rr.attempts != defaults.HelmRenderAttempts || rr.timeout != defaults.HelmTemplateTimeout ||
		rr.backoff != defaults.HelmRenderRetryBackoff {

		t.Fatalf("WithRetry() = %+v, want a retryRenderer with the defaults", rr)
	}

	internal := errors.New(errors.ErrCodeInternal, "pull failed")
	tests := []struct {
		name      string
		failures  int
		err       error
		wantCalls int
		wantCode  errors.ErrorCode
	}{
		{"transient failure is absorbed", 2, internal, 3, ""},
		{"persistent failure surfaces after all attempts", 99, internal, 3, errors.ErrCodeInternal},
		{"non-internal failure is not retried", 99, errors.New(errors.ErrCodeNotFound, "no helm"), 1,
			errors.ErrCodeNotFound},
		{"non-internal failure wrapping an internal one is not retried", 99,
			errors.Wrap(errors.ErrCodeNotFound, "no chart", internal), 1, errors.ErrCodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &flakyRenderer{failures: tt.failures, err: tt.err, out: []byte("kind: ConfigMap\n")}
			r := &retryRenderer{r: f, attempts: 3, timeout: time.Minute}
			out, err := r.Render(t.Context(), ChartInput{Name: "x"})
			if f.calls != tt.wantCalls {
				t.Errorf("Render() calls = %d, want %d", f.calls, tt.wantCalls)
			}
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("Render() error = %v, want nil", err)
			}
			if string(out) != "kind: ConfigMap\n" {
				t.Errorf("Render() = %q, want the rendered output", out)
			}
		})
	}
}

// hangingRenderer blocks until its context is done, then fails.
type hangingRenderer struct{ calls int }

func (h *hangingRenderer) Render(ctx context.Context, _ ChartInput) ([]byte, error) {
	h.calls++
	<-ctx.Done()
	return nil, errors.New(errors.ErrCodeInternal, "signal: killed")
}

func TestWithRetryDoesNotRetryTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &hangingRenderer{}
		r := &retryRenderer{r: h, attempts: 3, timeout: time.Minute}
		if _, err := r.Render(t.Context(), ChartInput{Name: "x"}); err == nil {
			t.Fatal("Render() error = nil, want the timeout failure")
		}
		if h.calls != 1 {
			t.Errorf("calls = %d, want 1 (timeouts are not retried)", h.calls)
		}
	})
}

func TestWithRetryStopsOnContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f := &flakyRenderer{failures: 99, err: errors.New(errors.ErrCodeInternal, "pull failed")}
		r := &retryRenderer{r: f, attempts: 3, timeout: time.Minute, backoff: time.Hour}
		errc := make(chan error, 1)
		go func() {
			_, err := r.Render(ctx, ChartInput{Name: "x"})
			errc <- err
		}()
		// Render is now blocked in its backoff after the first failure.
		synctest.Wait()
		cancel()
		if err := <-errc; err == nil {
			t.Fatal("Render() error = nil, want the render failure")
		}
		if f.calls != 1 {
			t.Errorf("calls = %d, want 1 (no retry after cancel)", f.calls)
		}
	})
}
