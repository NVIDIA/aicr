// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// RespondJSON writes a JSON response with the given status code and data.
// It buffers the JSON encoding before writing headers to prevent partial responses.
func RespondJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")

	// Serialize first to detect errors before writing headers
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(data); err != nil {
		slog.Error("json encoding failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(statusCode)
	if _, err := w.Write(buf.Bytes()); err != nil {
		// Connection is broken, log but can't recover
		slog.Warn("response write failed", "error", err)
	}
}

const (
	HTTPReaderUserAgent = "AICR-Serializer/1.0"
)

var (
	HTTPReaderDefaultTimeout               = defaults.HTTPClientTimeout
	HTTPReaderDefaultKeepAlive             = defaults.HTTPKeepAlive
	HTTPReaderDefaultConnectTimeout        = defaults.HTTPConnectTimeout
	HTTPReaderDefaultTLSHandshakeTimeout   = defaults.HTTPTLSHandshakeTimeout
	HTTPReaderDefaultResponseHeaderTimeout = defaults.HTTPResponseHeaderTimeout
	HTTPReaderDefaultIdleConnTimeout       = defaults.HTTPIdleConnTimeout
	HTTPReaderDefaultMaxIdleConns          = 100
	HTTPReaderDefaultMaxIdleConnsPerHost   = 10
	HTTPReaderDefaultMaxConnsPerHost       = 0
)

// HTTPReaderOption defines a configuration option for HTTPReader.
type HTTPReaderOption func(*HTTPReader)

// ptr is a helper to create a pointer to a value.
func ptr[T any](v T) *T { return &v }

// HTTPReader handles fetching data over HTTP with configurable options.
type HTTPReader struct {
	UserAgent             string
	TotalTimeout          *time.Duration
	ConnectTimeout        *time.Duration
	TLSHandshakeTimeout   *time.Duration
	ResponseHeaderTimeout *time.Duration
	IdleConnTimeout       *time.Duration
	MaxIdleConns          *int
	MaxIdleConnsPerHost   *int
	MaxConnsPerHost       *int
	InsecureSkipVerify    *bool
	Client                *http.Client
	transport             *http.Transport
}

func WithUserAgent(userAgent string) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.UserAgent = userAgent
	}
}

func WithTotalTimeout(timeout time.Duration) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.TotalTimeout = &timeout
	}
}

func WithConnectTimeout(timeout time.Duration) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.ConnectTimeout = &timeout
	}
}

func WithTLSHandshakeTimeout(timeout time.Duration) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.TLSHandshakeTimeout = &timeout
	}
}

func WithResponseHeaderTimeout(timeout time.Duration) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.ResponseHeaderTimeout = &timeout
	}
}

func WithIdleConnTimeout(timeout time.Duration) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.IdleConnTimeout = &timeout
	}
}

func WithMaxIdleConns(max int) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.MaxIdleConns = &max
	}
}

func WithMaxIdleConnsPerHost(max int) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.MaxIdleConnsPerHost = &max
	}
}

func WithMaxConnsPerHost(max int) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.MaxConnsPerHost = &max
	}
}

func WithInsecureSkipVerify(skip bool) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.InsecureSkipVerify = &skip
	}
}

func WithClient(client *http.Client) HTTPReaderOption {
	return func(r *HTTPReader) {
		r.Client = client
	}
}

// NewHTTPReader creates a new HTTPReader with the specified options.
func NewHTTPReader(options ...HTTPReaderOption) *HTTPReader {
	t := newDefaultHTTPTransport()

	r := &HTTPReader{
		UserAgent: HTTPReaderUserAgent,
		transport: t,
		Client: &http.Client{
			Timeout:   HTTPReaderDefaultTimeout,
			Transport: t,
		},
	}

	// Apply options
	for _, opt := range options {
		opt(r)
	}

	// Apply config to the underlying client/transport.
	// Note: if a custom client is supplied via WithClient, transport-related
	// options are best-effort and may be ignored depending on client.Transport.
	r.apply()
	return r
}

func newDefaultHTTPTransport() *http.Transport {
	return &http.Transport{
		// Connection pooling
		MaxIdleConns:        HTTPReaderDefaultMaxIdleConns,
		MaxIdleConnsPerHost: HTTPReaderDefaultMaxIdleConnsPerHost,
		MaxConnsPerHost:     HTTPReaderDefaultMaxConnsPerHost,

		// Timeouts
		DialContext: (&net.Dialer{
			Timeout:   HTTPReaderDefaultConnectTimeout,
			KeepAlive: HTTPReaderDefaultKeepAlive,
		}).DialContext,
		TLSHandshakeTimeout:   HTTPReaderDefaultTLSHandshakeTimeout,
		ResponseHeaderTimeout: HTTPReaderDefaultResponseHeaderTimeout,
		ExpectContinueTimeout: defaults.HTTPExpectContinueTimeout,

		// Connection reuse
		IdleConnTimeout:    HTTPReaderDefaultIdleConnTimeout,
		DisableKeepAlives:  false,
		DisableCompression: false,
		ForceAttemptHTTP2:  true,

		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
}

func (r *HTTPReader) apply() {
	if r == nil {
		return
	}

	if r.UserAgent == "" {
		r.UserAgent = HTTPReaderUserAgent
	}

	// Track whether TotalTimeout was explicitly set
	totalTimeoutWasSet := r.TotalTimeout != nil

	// Apply defaults for unset options
	if r.TotalTimeout == nil {
		r.TotalTimeout = ptr(HTTPReaderDefaultTimeout)
	}
	if r.ConnectTimeout == nil {
		r.ConnectTimeout = ptr(HTTPReaderDefaultConnectTimeout)
	}
	if r.TLSHandshakeTimeout == nil {
		r.TLSHandshakeTimeout = ptr(HTTPReaderDefaultTLSHandshakeTimeout)
	}
	if r.ResponseHeaderTimeout == nil {
		r.ResponseHeaderTimeout = ptr(HTTPReaderDefaultResponseHeaderTimeout)
	}
	if r.IdleConnTimeout == nil {
		r.IdleConnTimeout = ptr(HTTPReaderDefaultIdleConnTimeout)
	}
	if r.MaxIdleConns == nil {
		r.MaxIdleConns = ptr(HTTPReaderDefaultMaxIdleConns)
	}
	if r.MaxIdleConnsPerHost == nil {
		r.MaxIdleConnsPerHost = ptr(HTTPReaderDefaultMaxIdleConnsPerHost)
	}
	if r.MaxConnsPerHost == nil {
		r.MaxConnsPerHost = ptr(HTTPReaderDefaultMaxConnsPerHost)
	}
	if r.InsecureSkipVerify == nil {
		r.InsecureSkipVerify = ptr(false)
	}

	if r.Client == nil {
		// Preserve behavior: if caller nils out the client, recreate a safe default.
		t := newDefaultHTTPTransport()
		r.transport = t
		r.Client = &http.Client{Timeout: *r.TotalTimeout, Transport: t}
	} else if totalTimeoutWasSet {
		// Only override client timeout if TotalTimeout was explicitly set
		r.Client.Timeout = *r.TotalTimeout
	}

	// If caller supplied a custom client, we can only apply transport-related options
	// when the transport is the default *http.Transport.
	tr, ok := r.Client.Transport.(*http.Transport)
	if !ok || tr == nil {
		return
	}

	// Pooling
	if *r.MaxIdleConns > 0 {
		tr.MaxIdleConns = *r.MaxIdleConns
	}
	if *r.MaxIdleConnsPerHost > 0 {
		tr.MaxIdleConnsPerHost = *r.MaxIdleConnsPerHost
	}
	if *r.MaxConnsPerHost > 0 {
		tr.MaxConnsPerHost = *r.MaxConnsPerHost
	}

	// Timeouts
	if *r.ConnectTimeout > 0 {
		tr.DialContext = (&net.Dialer{
			Timeout:   *r.ConnectTimeout,
			KeepAlive: HTTPReaderDefaultKeepAlive,
		}).DialContext
	}
	if *r.TLSHandshakeTimeout > 0 {
		tr.TLSHandshakeTimeout = *r.TLSHandshakeTimeout
	}
	if *r.ResponseHeaderTimeout > 0 {
		tr.ResponseHeaderTimeout = *r.ResponseHeaderTimeout
	}
	if *r.IdleConnTimeout > 0 {
		tr.IdleConnTimeout = *r.IdleConnTimeout
	}

	// TLS
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.MinVersion = tls.VersionTLS12
	tr.TLSClientConfig.InsecureSkipVerify = *r.InsecureSkipVerify
}

// Read fetches data from the specified URL and returns it as a byte slice.
// The request is bounded by the HTTPReader's TotalTimeout.
// Use ReadWithContext for caller-controlled cancellation.
func (r *HTTPReader) Read(url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), *r.TotalTimeout)
	defer cancel()
	return r.ReadWithContext(ctx, url)
}

// ReadWithContext fetches data from the specified URL and returns it as a byte slice.
// The request is bound to the provided context for cancellation and deadlines.
// Callers must provide a non-nil context.
func (r *HTTPReader) ReadWithContext(ctx context.Context, url string) ([]byte, error) {
	if url == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "url is empty")
	}

	if r.Client == nil {
		return nil, errors.New(errors.ErrCodeInternal, "http client is nil")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create request for url %s", url), err)
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}

	resp, err := r.Client.Do(req) //nolint:gosec // G704 -- URL is validated by http.NewRequestWithContext
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, fmt.Sprintf("http request failed for url %s", url), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(errors.ErrCodeUnavailable, fmt.Sprintf("failed to fetch data: status %s", resp.Status))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read response body", err)
	}

	return data, nil
}

// Download reads data from the specified URL and writes it to the given file path.
// The request is bounded by the HTTPReader's TotalTimeout.
// Use DownloadWithContext for caller-controlled cancellation.
func (r *HTTPReader) Download(url, filePath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), *r.TotalTimeout)
	defer cancel()
	return r.DownloadWithContext(ctx, url, filePath)
}

// DownloadWithContext reads data from the specified URL and writes it to the given file path.
// The request is bound to the provided context for cancellation and deadlines.
func (r *HTTPReader) DownloadWithContext(ctx context.Context, url, filePath string) error {
	data, err := r.ReadWithContext(ctx, url)
	if err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable, fmt.Sprintf("failed to read from url %s", url), err)
	}

	if err := os.WriteFile(filepath.Clean(filePath), data, 0600); err != nil { //nolint:gosec // G703 -- path from caller-provided download target
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to write file %s", filePath), err)
	}

	return nil
}
