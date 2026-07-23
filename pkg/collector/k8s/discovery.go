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

package k8s

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// collectorDiscovery returns a discovery client whose requests are canceled
// with ctx. client-go discovery methods issue requests with context.TODO(), so
// the collector context must be joined at the HTTP transport boundary.
func collectorDiscovery(
	ctx context.Context,
	base discovery.DiscoveryInterface,
	config *rest.Config,
) (discovery.DiscoveryInterface, error) {

	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "Kubernetes discovery context cancelled", err)
	}
	if base == nil {
		return nil, errors.New(errors.ErrCodeInternal, "Kubernetes discovery client is nil")
	}

	baseREST := base.RESTClient()
	if baseREST == nil {
		// Fake discovery clients are in-memory and expose no REST client.
		return base, nil
	}
	if config == nil {
		return nil, errors.New(errors.ErrCodeInternal, "Kubernetes REST config is nil")
	}

	restClient, ok := baseREST.(*rest.RESTClient)
	if !ok {
		return nil, errors.New(errors.ErrCodeInternal, "unsupported Kubernetes discovery REST client")
	}

	baseTransport := http.DefaultTransport
	httpClient := &http.Client{}
	if restClient.Client != nil {
		if restClient.Client.Transport != nil {
			baseTransport = restClient.Client.Transport
		}
		httpClient.CheckRedirect = restClient.Client.CheckRedirect
		httpClient.Jar = restClient.Client.Jar
		httpClient.Timeout = restClient.Client.Timeout
	}

	timeout, err := discoveryTimeout(ctx, httpClient.Timeout)
	if err != nil {
		return nil, err
	}
	httpClient.Transport = &collectorContextTransport{
		ctx:  ctx,
		base: baseTransport,
	}
	httpClient.Timeout = timeout

	client, err := discovery.NewDiscoveryClientForConfigAndClient(
		rest.CopyConfig(config),
		httpClient,
	)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create bounded Kubernetes discovery client", err)
	}
	return client, nil
}

func discoveryTimeout(ctx context.Context, configured time.Duration) (time.Duration, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		if configured > 0 {
			return configured, nil
		}
		return defaults.CollectorK8sTimeout, nil
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, errors.Wrap(
			errors.ErrCodeTimeout,
			"Kubernetes discovery context deadline exceeded",
			context.DeadlineExceeded,
		)
	}
	if configured <= 0 || remaining < configured {
		return remaining, nil
	}
	return configured, nil
}

// collectorContextTransport preserves the request's own timeout while also
// canceling in-flight discovery requests when the collector context ends.
type collectorContextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t *collectorContextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}

	requestCtx, cancel := context.WithCancel(req.Context())
	// This intentionally merges the request and collector contexts rather
	// than deriving one from the other.
	stop := context.AfterFunc(t.ctx, cancel) //nolint:contextcheck
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			stop()
			cancel()
		})
	}

	response, err := t.base.RoundTrip(req.Clone(requestCtx))
	if err != nil {
		cleanup()
		return nil, err
	}
	if response.Body == nil {
		cleanup()
		return response, nil
	}
	// RoundTrip returns after receiving response headers, before client-go has
	// consumed the body. Keep the merged context alive until Body.Close so a
	// slow or large discovery response is not interrupted with context.Canceled.
	response.Body = &contextCleanupBody{
		ReadCloser: response.Body,
		cleanup:    cleanup,
	}
	return response, nil
}

type contextCleanupBody struct {
	io.ReadCloser
	cleanup func()
}

func (b *contextCleanupBody) Close() error {
	err := b.ReadCloser.Close()
	b.cleanup()
	return err
}
