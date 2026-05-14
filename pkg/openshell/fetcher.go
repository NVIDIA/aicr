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
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// cacheEntry holds a cached policy document with its expiry metadata.
type cacheEntry struct {
	doc       *PolicyDocument
	fetchedAt time.Time
	ttl       time.Duration
}

// expired returns true if the cache entry has passed its TTL.
func (e cacheEntry) expired() bool {
	return time.Since(e.fetchedAt) >= e.ttl
}

// maxCacheEntries is the maximum number of policy documents kept in cache.
// When exceeded, all expired entries are evicted; if still over, the oldest
// entry is removed. This prevents unbounded memory growth in clusters with
// many agents.
const maxCacheEntries = 1024

// Fetcher retrieves and caches policy documents from target agents' policy URIs.
// It implements SSRF protection, size limits, and TTL-based caching.
// Concurrent fetches for the same URI are coalesced via singleflight.
type Fetcher struct {
	client       *http.Client
	cacheTTL     time.Duration
	maxBytes     int64
	allowPrivate bool // for testing only: skip SSRF private IP check

	mu    sync.RWMutex
	cache map[string]cacheEntry
	group singleflight.Group
}

// NewFetcher creates a Fetcher with functional options.
//
// The default HTTP client uses a Transport with a safeDialContext that rejects
// connections to private/loopback addresses even when the URL hostname resolves
// to one (closing the SSRF bypass via DNS). Redirects are followed only when
// the redirect target is HTTPS; cross-scheme redirects are rejected. Tests that
// need to reach localhost via httptest must pass WithHTTPClient or
// WithAllowPrivate(true).
func NewFetcher(opts ...FetcherOption) *Fetcher {
	f := &Fetcher{
		cacheTTL: defaults.PolicyCacheTTL,
		maxBytes: defaults.PolicyMaxBytes,
		cache:    make(map[string]cacheEntry),
	}
	for _, opt := range opts {
		opt(f)
	}
	if f.client == nil {
		f.client = &http.Client{
			Timeout:       defaults.PolicyFetchTimeout,
			Transport:     newSafeTransport(f.allowPrivate),
			CheckRedirect: checkRedirect,
		}
	}
	return f
}

// checkRedirect re-validates each redirect target so a 30x response cannot
// downgrade the connection from HTTPS or steer it to a private IP that would
// be missed by the up-front URL validation. The dial-time SSRF guard still
// runs, but rejecting non-HTTPS redirects up front gives a clearer error.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New(errors.ErrCodePolicyFetch, "too many redirects")
	}
	if req.URL.Scheme != "https" {
		return errors.New(errors.ErrCodePolicyFetch,
			"refusing to follow non-HTTPS redirect: "+req.URL.Scheme)
	}
	return nil
}

// newSafeTransport returns an http.Transport whose DialContext rejects
// connections to private, loopback, or link-local addresses post-resolution.
// When allowPrivate is true (test mode only), the check is skipped.
//
// Implementation note: to avoid a TOCTOU between the IP-allowlist check and
// the dialer's own DNS resolution, we pin the dial to the first allowed IP
// from our lookup. Hostname is preserved for TLS SNI/cert verification by
// leaving http.Transport's TLS config untouched (it uses URL.Host, not addr).
func newSafeTransport(allowPrivate bool) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{
		Timeout:   defaults.HTTPConnectTimeout,
		KeepAlive: defaults.HTTPKeepAlive,
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if allowPrivate {
			return dialer.DialContext(ctx, network, addr)
		}
		// If the host is already an IP literal, validate and dial directly.
		if ip := net.ParseIP(host); ip != nil {
			if isBlockedIP(ip) {
				return nil, errors.New(errors.ErrCodePolicyFetch,
					"refusing to connect to private/loopback address: "+ip.String())
			}
			return dialer.DialContext(ctx, network, addr)
		}
		// Resolve once, validate every result, then dial the first allowed IP.
		// Pinning the IP closes the TOCTOU window where a second resolution
		// (inside the stdlib dialer) could return a different, blocked address.
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New(errors.ErrCodePolicyFetch,
				"no IPs resolved for "+host)
		}
		for _, ipa := range ips {
			if isBlockedIP(ipa.IP) {
				return nil, errors.New(errors.ErrCodePolicyFetch,
					"refusing to connect to private/loopback address: "+ipa.IP.String())
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
	return t
}

// isBlockedIP reports whether the IP is in a range that should not be reachable
// from a policy fetcher (loopback, private, link-local, unspecified).
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// Fetch retrieves a policy document from the given URI. Results are cached
// for the configured TTL. Concurrent fetches for the same URI are coalesced.
// Returns an error if the URI is invalid, the fetch fails, or the document
// exceeds the size limit.
func (f *Fetcher) Fetch(ctx context.Context, policyURI string) (*PolicyDocument, error) {
	// Fast path: check cache under read lock
	f.mu.RLock()
	if entry, ok := f.cache[policyURI]; ok && !entry.expired() {
		f.mu.RUnlock()
		return entry.doc, nil
	}
	f.mu.RUnlock()

	// Slow path: coalesce concurrent fetches for the same URI via singleflight.
	// singleflight deduplicates in-flight requests; only one goroutine actually
	// performs the HTTP fetch, and the result is shared with all waiters.
	//
	// Caveat: the ctx passed into the fetch closure is the context of the FIRST
	// caller. If that caller cancels mid-flight, all coalesced waiters receive
	// the same error even if their own contexts are still valid. With the 3s
	// PolicyFetchTimeout this is an acceptable trade-off — a fetch that takes
	// longer would have timed out for everyone anyway.
	result, err, _ := f.group.Do(policyURI, func() (any, error) {
		// Double-check cache after acquiring singleflight slot
		f.mu.RLock()
		if entry, ok := f.cache[policyURI]; ok && !entry.expired() {
			f.mu.RUnlock()
			return entry.doc, nil
		}
		f.mu.RUnlock()

		return f.fetchAndCache(ctx, policyURI)
	})
	if err != nil {
		return nil, err
	}
	return result.(*PolicyDocument), nil
}

// fetchAndCache performs the actual HTTP fetch, validates the response, and
// caches the result. Called within singleflight to coalesce concurrent requests.
func (f *Fetcher) fetchAndCache(ctx context.Context, policyURI string) (*PolicyDocument, error) {
	// Validate URL before fetching (skip private IP check in test mode)
	if err := f.validateURL(policyURI); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, policyURI, nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodePolicyFetch, "failed to create policy request", err)
	}
	req.Header.Set("Accept", "application/json")

	// SSRF mitigations applied at three layers before this call:
	//   1. validateURL — pre-flight: rejects non-HTTPS schemes and any
	//      IP literal in the URL that resolves to a private/loopback range.
	//   2. newSafeTransport (HTTP transport) — post-resolution: rejects any
	//      dial target whose resolved IP is private/loopback/link-local;
	//      pins the dial to the validated IP to close the TOCTOU window
	//      between LookupIPAddr and dialer.DialContext.
	//   3. checkRedirect — re-applies the HTTPS-only check on every 3xx
	//      hop and caps redirect chains at 10.
	// Regression test: TestFetcherSSRFDialBlocksPrivateIP.
	resp, err := f.client.Do(req) //nolint:gosec // G704: SSRF mitigated above
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodePolicyFetch, "failed to fetch policy document", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(errors.ErrCodePolicyFetch,
			"policy URI returned non-200 status: "+resp.Status)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		return nil, errors.New(errors.ErrCodePolicyFetch,
			"policy URI returned unexpected content-type: "+ct)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodePolicyFetch, "failed to read policy document body", err)
	}
	if int64(len(body)) > f.maxBytes {
		return nil, errors.New(errors.ErrCodePolicyFetch,
			"policy document exceeds maximum size")
	}

	var doc PolicyDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodePolicyFetch, "failed to parse policy document JSON", err)
	}

	// Cache under write lock, with bounded eviction.
	f.mu.Lock()
	f.cache[policyURI] = cacheEntry{
		doc:       &doc,
		fetchedAt: time.Now(),
		ttl:       f.cacheTTL,
	}
	if len(f.cache) > maxCacheEntries {
		f.evictLocked()
	}
	f.mu.Unlock()

	return &doc, nil
}

// evictLocked removes expired entries from the cache. If still over
// maxCacheEntries, removes the oldest entry. Must be called with f.mu held.
func (f *Fetcher) evictLocked() {
	// First pass: remove all expired entries.
	for k, e := range f.cache {
		if e.expired() {
			delete(f.cache, k)
		}
	}
	// Second pass: if still over limit, remove oldest entry.
	for len(f.cache) > maxCacheEntries {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, e := range f.cache {
			if first || e.fetchedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = e.fetchedAt
				first = false
			}
		}
		delete(f.cache, oldestKey)
	}
}

// validateURL checks that the policy URI is safe to fetch.
// Delegates to validateFetchURL with the fetcher's allowPrivate setting.
func (f *Fetcher) validateURL(rawURL string) error {
	return validateFetchURL(rawURL, f.allowPrivate)
}

// validateFetchURL checks that the policy URI is safe to fetch.
// Rejects non-HTTPS schemes and private/loopback IPs in the URL itself.
// Hostnames that resolve to private addresses are rejected at dial time
// by the transport's safeDialContext (see newSafeTransport).
// If allowPrivate is true, private/loopback IP checks are skipped (testing only).
func validateFetchURL(rawURL string, allowPrivate bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.Wrap(errors.ErrCodePolicyFetch, "invalid policy URI", err)
	}

	if u.Scheme != "https" {
		return errors.New(errors.ErrCodePolicyFetch,
			"policy URI must use HTTPS scheme, got: "+u.Scheme)
	}

	hostname := u.Hostname()
	ip := net.ParseIP(hostname)
	if ip == nil {
		// Not a raw IP — DNS resolution and the post-resolution IP check
		// happen at dial time in safeDialContext.
		return nil
	}

	if !allowPrivate && isBlockedIP(ip) {
		return errors.New(errors.ErrCodePolicyFetch,
			"policy URI must not point to private/loopback address")
	}

	return nil
}
