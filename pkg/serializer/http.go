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
	"time"

	"github.com/NVIDIA/eidos/pkg/defaults"
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
	HttpReaderUserAgent = "Eidos-Serializer/1.0"
)

var (
	HttpReaderDefaultTimeout               = defaults.HTTPClientTimeout
	HttpReaderDefaultKeepAlive             = defaults.HTTPKeepAlive
	HttpReaderDefaultConnectTimeout        = defaults.HTTPConnectTimeout
	HttpReaderDefaultTLSHandshakeTimeout   = defaults.HTTPTLSHandshakeTimeout
	HttpReaderDefaultResponseHeaderTimeout = defaults.HTTPResponseHeaderTimeout
	HttpReaderDefaultIdleConnTimeout       = defaults.HTTPIdleConnTimeout
	HttpReaderDefaultMaxIdleConns          = 100
	HttpReaderDefaultMaxIdleConnsPerHost   = 10
	HttpReaderDefaultMaxConnsPerHost       = 0
)

// HttpReaderOption defines a configuration option for HttpReader.
type HttpReaderOption func(*HttpReader)

// HttpReader handles fetching data over HTTP with configurable options.
type HttpReader struct {
	UserAgent             string
	TotalTimeout          time.Duration
	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	MaxConnsPerHost       int
	InsecureSkipVerify    bool
	Client                *http.Client
	transport             *http.Transport

	// Track which knobs were explicitly set via options so we don't
	// accidentally override caller-provided *http.Client defaults.
	totalTimeoutSet          bool
	connectTimeoutSet        bool
	tlsHandshakeTimeoutSet   bool
	responseHeaderTimeoutSet bool
	idleConnTimeoutSet       bool
	maxIdleConnsSet          bool
	maxIdleConnsPerHostSet   bool
	maxConnsPerHostSet       bool
	insecureSkipVerifySet    bool
}

func WithUserAgent(userAgent string) HttpReaderOption {
	return func(r *HttpReader) {
		r.UserAgent = userAgent
	}
}

func WithTotalTimeout(timeout time.Duration) HttpReaderOption {
	return func(r *HttpReader) {
		r.TotalTimeout = timeout
		r.totalTimeoutSet = true
	}
}

func WithConnectTimeout(timeout time.Duration) HttpReaderOption {
	return func(r *HttpReader) {
		r.ConnectTimeout = timeout
		r.connectTimeoutSet = true
	}
}

func WithTLSHandshakeTimeout(timeout time.Duration) HttpReaderOption {
	return func(r *HttpReader) {
		r.TLSHandshakeTimeout = timeout
		r.tlsHandshakeTimeoutSet = true
	}
}

func WithResponseHeaderTimeout(timeout time.Duration) HttpReaderOption {
	return func(r *HttpReader) {
		r.ResponseHeaderTimeout = timeout
		r.responseHeaderTimeoutSet = true
	}
}

func WithIdleConnTimeout(timeout time.Duration) HttpReaderOption {
	return func(r *HttpReader) {
		r.IdleConnTimeout = timeout
		r.idleConnTimeoutSet = true
	}
}

func WithMaxIdleConns(max int) HttpReaderOption {
	return func(r *HttpReader) {
		r.MaxIdleConns = max
		r.maxIdleConnsSet = true
	}
}

func WithMaxIdleConnsPerHost(max int) HttpReaderOption {
	return func(r *HttpReader) {
		r.MaxIdleConnsPerHost = max
		r.maxIdleConnsPerHostSet = true
	}
}

func WithMaxConnsPerHost(max int) HttpReaderOption {
	return func(r *HttpReader) {
		r.MaxConnsPerHost = max
		r.maxConnsPerHostSet = true
	}
}

func WithInsecureSkipVerify(skip bool) HttpReaderOption {
	return func(r *HttpReader) {
		r.InsecureSkipVerify = skip
		r.insecureSkipVerifySet = true
	}
}

func WithClient(client *http.Client) HttpReaderOption {
	return func(r *HttpReader) {
		r.Client = client
	}
}

// NewHttpReader creates a new HttpReader with the specified options.
func NewHttpReader(options ...HttpReaderOption) *HttpReader {
	t := newDefaultHTTPTransport()

	r := &HttpReader{
		UserAgent:             HttpReaderUserAgent,
		TotalTimeout:          HttpReaderDefaultTimeout,
		ConnectTimeout:        HttpReaderDefaultConnectTimeout,
		TLSHandshakeTimeout:   HttpReaderDefaultTLSHandshakeTimeout,
		ResponseHeaderTimeout: HttpReaderDefaultResponseHeaderTimeout,
		IdleConnTimeout:       HttpReaderDefaultIdleConnTimeout,
		MaxIdleConns:          HttpReaderDefaultMaxIdleConns,
		MaxIdleConnsPerHost:   HttpReaderDefaultMaxIdleConnsPerHost,
		MaxConnsPerHost:       HttpReaderDefaultMaxConnsPerHost,
		InsecureSkipVerify:    false,
		transport:             t,
		Client: &http.Client{
			Timeout:   HttpReaderDefaultTimeout,
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
		MaxIdleConns:        HttpReaderDefaultMaxIdleConns,
		MaxIdleConnsPerHost: HttpReaderDefaultMaxIdleConnsPerHost,
		MaxConnsPerHost:     HttpReaderDefaultMaxConnsPerHost,

		// Timeouts
		DialContext: (&net.Dialer{
			Timeout:   HttpReaderDefaultConnectTimeout,
			KeepAlive: HttpReaderDefaultKeepAlive,
		}).DialContext,
		TLSHandshakeTimeout:   HttpReaderDefaultTLSHandshakeTimeout,
		ResponseHeaderTimeout: HttpReaderDefaultResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,

		// Connection reuse
		IdleConnTimeout:    HttpReaderDefaultIdleConnTimeout,
		DisableKeepAlives:  false,
		DisableCompression: false,
		ForceAttemptHTTP2:  true,

		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
}

func (r *HttpReader) apply() {
	if r == nil {
		return
	}

	if r.UserAgent == "" {
		r.UserAgent = HttpReaderUserAgent
	}

	if r.Client == nil {
		// Preserve behavior: if caller nils out the client, recreate a safe default.
		t := newDefaultHTTPTransport()
		r.transport = t
		r.Client = &http.Client{Timeout: HttpReaderDefaultTimeout, Transport: t}
	}

	// Apply client timeout only if explicitly set.
	if r.totalTimeoutSet && r.TotalTimeout > 0 {
		r.Client.Timeout = r.TotalTimeout
	}

	// If caller supplied a custom client, we can only apply transport-related options
	// when the transport is the default *http.Transport.
	tr, ok := r.Client.Transport.(*http.Transport)
	if !ok || tr == nil {
		return
	}

	// Pooling
	if r.maxIdleConnsSet && r.MaxIdleConns > 0 {
		tr.MaxIdleConns = r.MaxIdleConns
	}
	if r.maxIdleConnsPerHostSet && r.MaxIdleConnsPerHost > 0 {
		tr.MaxIdleConnsPerHost = r.MaxIdleConnsPerHost
	}
	if r.maxConnsPerHostSet && r.MaxConnsPerHost > 0 {
		tr.MaxConnsPerHost = r.MaxConnsPerHost
	}

	// Timeouts
	if r.connectTimeoutSet && r.ConnectTimeout > 0 {
		tr.DialContext = (&net.Dialer{
			Timeout:   r.ConnectTimeout,
			KeepAlive: HttpReaderDefaultKeepAlive,
		}).DialContext
	}
	if r.tlsHandshakeTimeoutSet && r.TLSHandshakeTimeout > 0 {
		tr.TLSHandshakeTimeout = r.TLSHandshakeTimeout
	}
	if r.responseHeaderTimeoutSet && r.ResponseHeaderTimeout > 0 {
		tr.ResponseHeaderTimeout = r.ResponseHeaderTimeout
	}
	if r.idleConnTimeoutSet && r.IdleConnTimeout > 0 {
		tr.IdleConnTimeout = r.IdleConnTimeout
	}

	// TLS
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.MinVersion = tls.VersionTLS12
	if r.insecureSkipVerifySet {
		tr.TLSClientConfig.InsecureSkipVerify = r.InsecureSkipVerify
	}
}

// Read fetches data from the specified URL and returns it as a byte slice.
func (r *HttpReader) Read(url string) ([]byte, error) {
	return r.ReadWithContext(context.Background(), url)
}

// ReadWithContext fetches data from the specified URL and returns it as a byte slice.
// The request is bound to the provided context for cancellation and deadlines.
// Callers must provide a non-nil context.
func (r *HttpReader) ReadWithContext(ctx context.Context, url string) ([]byte, error) {
	if url == "" {
		return nil, fmt.Errorf("url is empty")
	}

	if r.Client == nil {
		return nil, fmt.Errorf("http client is nil")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for url %s: %w", url, err)
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed for url %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch data: status %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// Download reads data from the specified URL and writes it to the given file path.
func (r *HttpReader) Download(url, filePath string) error {
	return r.DownloadWithContext(context.Background(), url, filePath)
}

// DownloadWithContext reads data from the specified URL and writes it to the given file path.
// The request is bound to the provided context for cancellation and deadlines.
func (r *HttpReader) DownloadWithContext(ctx context.Context, url, filePath string) error {
	data, err := r.ReadWithContext(ctx, url)
	if err != nil {
		return fmt.Errorf("failed to read from url %s: %w", url, err)
	}

	if err := os.WriteFile(filePath, data, 0600); err != nil {
		return fmt.Errorf("failed to write file %s: %w", filePath, err)
	}

	return nil
}
