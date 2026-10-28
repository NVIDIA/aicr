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
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
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
		wantErr   bool
	}{
		{"transient failure is absorbed", 2, internal, 3, false},
		{"persistent failure surfaces after all attempts", 99, internal, 3, true},
		{"non-internal failure is not retried", 99, errors.New(errors.ErrCodeNotFound, "no helm"), 1, true},
		{"non-internal failure wrapping an internal one is not retried", 99,
			errors.Wrap(errors.ErrCodeNotFound, "no chart", internal), 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &flakyRenderer{failures: tt.failures, err: tt.err, out: []byte("kind: ConfigMap\n")}
			r := &retryRenderer{r: f, attempts: 3, timeout: time.Minute}
			out, err := r.Render(t.Context(), ChartInput{Name: "x"})
			if (err != nil) != tt.wantErr || f.calls != tt.wantCalls {
				t.Errorf("Render() err = %v, calls = %d; want err %v, calls %d", err, f.calls, tt.wantErr, tt.wantCalls)
			}
			if !tt.wantErr && string(out) != "kind: ConfigMap\n" {
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
	h := &hangingRenderer{}
	r := &retryRenderer{r: h, attempts: 3, timeout: 10 * time.Millisecond}
	if _, err := r.Render(t.Context(), ChartInput{Name: "x"}); err == nil {
		t.Fatal("Render() error = nil, want the timeout failure")
	}
	if h.calls != 1 {
		t.Errorf("calls = %d, want 1 (timeouts are not retried)", h.calls)
	}
}

// cancelingRenderer cancels the caller's context on its first call, then fails.
type cancelingRenderer struct {
	cancel context.CancelFunc
	calls  int
}

func (c *cancelingRenderer) Render(context.Context, ChartInput) ([]byte, error) {
	c.calls++
	c.cancel()
	return nil, errors.New(errors.ErrCodeInternal, "pull failed")
}

func TestWithRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := &cancelingRenderer{cancel: cancel}
	r := &retryRenderer{r: c, attempts: 3, timeout: time.Minute, backoff: time.Hour}
	if _, err := r.Render(ctx, ChartInput{Name: "x"}); err == nil {
		t.Fatal("Render() error = nil, want the render failure")
	}
	if c.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry after cancel)", c.calls)
	}
}
