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
	stderrors "errors"
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// WithRetry returns a Renderer that renders with r and retries a render that
// fails with ErrCodeInternal, as a transient chart pull does. It makes up to
// defaults.HelmRenderAttempts attempts, each bounded by
// defaults.HelmTemplateTimeout, and returns the last error if all fail. It
// does not retry an attempt that timed out or a render that failed with any
// other code.
func WithRetry(r Renderer) Renderer {
	return &retryRenderer{
		r:        r,
		attempts: defaults.HelmRenderAttempts,
		timeout:  defaults.HelmTemplateTimeout,
		backoff:  defaults.HelmRenderRetryBackoff,
	}
}

type retryRenderer struct {
	r        Renderer
	attempts int
	timeout  time.Duration
	backoff  time.Duration
}

func (rr *retryRenderer) Render(ctx context.Context, input ChartInput) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		out, timedOut, err := rr.renderOnce(ctx, input)
		// Only helm exiting non-zero can succeed on a retry. A missing helm
		// binary or an unconfigured chart will not fix itself, and a chart
		// that hung once would cost another full timeout.
		if err == nil || timedOut || attempt >= rr.attempts ||
			!stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {

			return out, err
		}
		slog.Warn("chart render failed, retrying", "component", input.Name, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(time.Duration(attempt) * rr.backoff):
		}
	}
}

// renderOnce renders input once and reports whether the attempt timed out.
func (rr *retryRenderer) renderOnce(ctx context.Context, input ChartInput) (out []byte, timedOut bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, rr.timeout)
	defer cancel()
	out, err = rr.r.Render(ctx, input)
	return out, ctx.Err() != nil, err
}
