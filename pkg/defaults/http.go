// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package defaults

import (
	"net"
	"net/http"
	"time"
)

// NewHTTPTransport returns an *http.Transport configured with the standard
// timeout constants from this package.
func NewHTTPTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   HTTPConnectTimeout,
			KeepAlive: HTTPKeepAlive,
		}).DialContext,
		TLSHandshakeTimeout:   HTTPTLSHandshakeTimeout,
		ResponseHeaderTimeout: HTTPResponseHeaderTimeout,
		IdleConnTimeout:       HTTPIdleConnTimeout,
		ExpectContinueTimeout: HTTPExpectContinueTimeout,
	}
}

// NewHTTPClient returns an *http.Client with a standard transport and the
// given timeout. If timeout is zero, HTTPClientTimeout is used.
func NewHTTPClient(timeout time.Duration) *http.Client {
	if timeout == 0 {
		timeout = HTTPClientTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: NewHTTPTransport(),
	}
}
