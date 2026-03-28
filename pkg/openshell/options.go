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

package openshell

import (
	"net/http"
	"time"
)

// GuardOption configures a Guard.
type GuardOption func(*Guard)

// WithMode sets the enforcement mode.
func WithMode(mode Mode) GuardOption {
	return func(g *Guard) {
		g.mode = mode
	}
}

// WithCallerID sets the calling agent's identity for policy context.
func WithCallerID(id string) GuardOption {
	return func(g *Guard) {
		g.callerID = id
	}
}

// WithCallerDomain sets the calling agent's domain for policy context.
func WithCallerDomain(domain string) GuardOption {
	return func(g *Guard) {
		g.callerDomain = domain
	}
}

// WithFetcher overrides the default policy fetcher.
func WithFetcher(f *Fetcher) GuardOption {
	return func(g *Guard) {
		g.fetcher = f
	}
}

// FetcherOption configures a Fetcher.
type FetcherOption func(*Fetcher)

// WithCacheTTL sets the cache time-to-live for fetched policy documents.
func WithCacheTTL(ttl time.Duration) FetcherOption {
	return func(f *Fetcher) {
		f.cacheTTL = ttl
	}
}

// WithHTTPClient overrides the HTTP client used for policy fetches.
func WithHTTPClient(client *http.Client) FetcherOption {
	return func(f *Fetcher) {
		f.client = client
	}
}

// WithMaxBytes sets the maximum policy document size in bytes.
func WithMaxBytes(n int64) FetcherOption {
	return func(f *Fetcher) {
		f.maxBytes = n
	}
}

// WithAllowPrivate disables SSRF private/loopback IP checks.
// This must only be used in tests.
func WithAllowPrivate(allow bool) FetcherOption {
	return func(f *Fetcher) {
		f.allowPrivate = allow
	}
}
