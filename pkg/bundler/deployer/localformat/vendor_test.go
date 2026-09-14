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

package localformat

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestValidateForPull(t *testing.T) {
	tests := []struct {
		name    string
		c       Component
		wantErr bool
		code    string
	}{
		{"missing repo", Component{Name: "x"}, true, string(errors.ErrCodeInvalidRequest)},
		{"missing version", Component{Name: "x", Repository: "https://r"}, true, string(errors.ErrCodeInvalidRequest)},
		{"valid http", Component{Name: "x", Repository: "http://r", Version: "1"}, false, ""},
		{"valid https", Component{Name: "x", Repository: "https://r", Version: "1"}, false, ""},
		{"valid oci", Component{Name: "x", Repository: "oci://r/c", Version: "1", IsOCI: true}, false, ""},
		{"bare hostname rejected", Component{Name: "x", Repository: "r.example.com", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"file scheme rejected", Component{Name: "x", Repository: "file:///tmp", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"oci scheme without IsOCI rejected", Component{Name: "x", Repository: "oci://r/c", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"IsOCI without oci scheme rejected", Component{Name: "x", Repository: "https://r/c", Version: "1", IsOCI: true}, true, string(errors.ErrCodeInvalidRequest)},
		{"flag-looking chart name rejected", Component{Name: "x", ChartName: "--insecure-skip-tls-verify", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"flag-looking component name rejected", Component{Name: "--ca-file=evil", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"flag-looking version rejected", Component{Name: "x", Repository: "https://r", Version: "-rce"}, true, string(errors.ErrCodeInvalidRequest)},
		// Regex defense-in-depth: rejects values that don't lead with `-`
		// but contain shell-meta or whitespace which could matter if the
		// invocation path ever changes (env-var, shell wrapper, etc.).
		{"chart name with space rejected", Component{Name: "x", ChartName: "foo bar", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"chart name with shell meta rejected", Component{Name: "x", ChartName: "foo;rm", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"chart name with leading dot rejected", Component{Name: "x", ChartName: ".hidden", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		// Legitimate inputs that the regex must NOT reject:
		{"chart name with hyphen accepted", Component{Name: "x", ChartName: "gpu-operator", Repository: "https://r", Version: "1.2.3"}, false, ""},
		{"chart name with dot accepted", Component{Name: "x", ChartName: "v8.21.runtime", Repository: "https://r", Version: "1"}, false, ""},
		{"semver version with build metadata accepted", Component{Name: "x", Repository: "https://r", Version: "1.2.3+build.7"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateForPull(tt.c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.code != "" {
				var se *errors.StructuredError
				if !stderrors.As(err, &se) {
					t.Fatalf("expected StructuredError, got: %T (%v)", err, err)
				}
				if string(se.Code) != tt.code {
					t.Errorf("Code = %s, want %s", se.Code, tt.code)
				}
			}
		})
	}
}

func TestShouldVendor(t *testing.T) {
	tests := []struct {
		name string
		c    Component
		want bool
	}{
		{"helm component", Component{Repository: "https://r"}, true},
		{"oci component", Component{Repository: "oci://r/c", IsOCI: true}, true},
		{"manifest only", Component{Repository: ""}, false},
		{"kustomize tag", Component{Repository: "https://git/repo", Tag: "v1"}, false},
		{"kustomize path", Component{Repository: "https://git/repo", Path: "p"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldVendor(tt.c); got != tt.want {
				t.Errorf("shouldVendor(%+v) = %v, want %v", tt.c, got, tt.want)
			}
		})
	}
}

func TestClassifyHelmCLIError(t *testing.T) {
	c := Component{Name: "x", ChartName: "foo", Version: "1.0", Repository: "https://r"}
	tests := []struct {
		name   string
		runErr error
		stderr string
		want   errors.ErrorCode
	}{
		// Substring-fallback path (runErr is a generic "exit status 1"):
		{"binary missing (stderr fallback)", stderrors.New("exit status 1"), "exec: \"helm\": executable file not found in $PATH", errors.ErrCodeUnavailable},
		{"chart not found", stderrors.New("exit status 1"), `Error: chart "foo" version "1.0" not found`, errors.ErrCodeNotFound},
		{"http 404", stderrors.New("exit status 1"), "Error: failed to fetch https://r/foo-1.0.tgz: 404 Not Found", errors.ErrCodeNotFound},
		{"unauthorized", stderrors.New("exit status 1"), "Error: failed to authorize: unauthorized: authentication required", errors.ErrCodeUnauthorized},
		{"forbidden 403", stderrors.New("exit status 1"), "Error: 403 Forbidden", errors.ErrCodeUnauthorized},
		{"context deadline", stderrors.New("exit status 1"), "Error: context deadline exceeded", errors.ErrCodeTimeout},
		{"signal killed", stderrors.New("exit status 1"), "signal: killed", errors.ErrCodeTimeout},
		{"dns failure", stderrors.New("exit status 1"), "Error: dial tcp: lookup r: no such host", errors.ErrCodeUnavailable},
		{"connection refused", stderrors.New("exit status 1"), "Error: dial tcp: connection refused", errors.ErrCodeUnavailable},
		{"generic", stderrors.New("exit status 1"), "Error: something bizarre and unexpected", errors.ErrCodeInternal},

		// Typed-sentinel path: the classifier checks errors.Is BEFORE the
		// stderr substring fallback. Both sentinels must classify as
		// ErrCodeUnavailable (binary missing) regardless of stderr.
		{"binary missing (exec.ErrNotFound sentinel)", exec.ErrNotFound, "", errors.ErrCodeUnavailable},
		{"binary missing (os.ErrNotExist sentinel)", os.ErrNotExist, "", errors.ErrCodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyHelmCLIError(c, tt.runErr, tt.stderr)
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("not a StructuredError: %v", err)
			}
			if se.Code != tt.want {
				t.Errorf("Code = %v, want %v\nrunErr: %v\nstderr: %s", se.Code, tt.want, tt.runErr, tt.stderr)
			}
		})
	}
}

// TestCLIChartPuller_SSRFFilterWired is the end-to-end proof that
// checkEgressPolicy runs INSIDE Pull() — not just as a standalone
// function that could accidentally be bypassed. A loopback URL must be
// rejected as ErrCodeInvalidRequest before helm-binary discovery, so
// even a machine with helm on PATH cannot be steered at the local
// network from the vendor path.
func TestCLIChartPuller_SSRFFilterWired(t *testing.T) {
	// HelmBin points at /bin/true so if the SSRF filter DIDN'T run, we'd
	// see ErrCodeInternal (helm exits 0, readSingleTgz fails on the empty
	// tmpDir with "helm pull produced no .tgz output") — a distinct
	// signal from what we want, which is the InvalidRequest short-circuit.
	p := &CLIChartPuller{HelmBin: "/bin/true"}
	c := Component{Name: "x", ChartName: "foo", Version: "1", Repository: "http://127.0.0.1/charts"}

	tgz, _, _, err := p.Pull(context.Background(), c) //nolint:dogsled // Pull's 4-value return is fixed by the ChartPuller interface.
	if err == nil {
		t.Fatalf("expected SSRF-policy rejection for loopback URL; got %d bytes", len(tgz))
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("not a StructuredError: %v", err)
	}
	if se.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("Code = %v, want %v (helm should never have been invoked)", se.Code, errors.ErrCodeInvalidRequest)
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error %q does not mention loopback", err.Error())
	}
}

func TestCLIChartPuller_NoBinary(t *testing.T) {
	// Point HelmBin at a path that definitely doesn't exist so the test
	// is hermetic regardless of whether `helm` is installed locally.
	// Repository uses a literal public IP (Google DNS) so the SSRF egress
	// filter passes without hitting the system resolver — the test
	// exercises the helm-binary-missing path, not DNS reachability.
	// Mock fetchIndexYAML so the new second-layer index pre-check does
	// not try a real HTTP fetch to 8.8.8.8; it returns a well-formed
	// index whose one URL is public, so the pre-check passes cleanly.
	withMockIndexFetcher(t, func(_ context.Context, _ string) ([]byte, error) {
		return []byte("apiVersion: v1\nentries:\n  foo:\n    - version: \"1\"\n      urls: [\"https://8.8.8.8/foo-1.tgz\"]\n"), nil
	})
	p := &CLIChartPuller{HelmBin: "/nonexistent/aicr-test/helm"}
	c := Component{Name: "x", ChartName: "foo", Version: "1", Repository: "https://8.8.8.8"}

	tgz, _, _, err := p.Pull(context.Background(), c)
	if err == nil {
		t.Fatalf("expected error from missing helm binary; got %d bytes", len(tgz))
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("not a StructuredError: %v", err)
	}
	if se.Code != errors.ErrCodeUnavailable {
		t.Errorf("Code = %v, want %v", se.Code, errors.ErrCodeUnavailable)
	}
}

// TestCLIChartPuller_IndexPointsAtPrivateTarball is the PR's headline
// SSRF vector, exercised end-to-end through Pull() (not just through
// resolveAndValidateHTTPIndex in isolation). Repository is a public IP
// so the layer-1 check on c.Repository passes; the mocked index lists a
// tarball URL on 169.254.169.254 (AWS metadata) so the layer-2 check on
// each resolved chart URL must reject BEFORE helm-binary discovery.
// Pins that the 3-line wiring `Pull → checkEgressPolicy → resolveAnd
// ValidateHTTPIndex → helm subprocess` cannot regress silently.
func TestCLIChartPuller_IndexPointsAtPrivateTarball(t *testing.T) {
	// Mock the index fetcher so the pre-check reads canned bytes
	// containing a cloud-metadata tarball URL.
	withMockIndexFetcher(t, func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`apiVersion: v1
entries:
  gpu-operator:
    - version: 1.0.0
      urls: [http://169.254.169.254/latest/meta-data/iam/security-credentials/steal.tgz]
`), nil
	})
	// HelmBin points at /bin/true so a regression that let control fall
	// past the pre-check would produce a distinct signal (ErrCodeInternal
	// from readSingleTgz on an empty tmpDir) rather than the expected
	// InvalidRequest from the tarball-URL egress rejection.
	p := &CLIChartPuller{HelmBin: "/bin/true"}
	c := Component{
		Name:       "gpu-operator",
		ChartName:  "gpu-operator",
		Version:    "1.0.0",
		Repository: "https://93.184.216.34/charts",
	}
	tgz, _, _, err := p.Pull(context.Background(), c) //nolint:dogsled // Pull's 4-value return is fixed by the ChartPuller interface.
	if err == nil {
		t.Fatalf("expected pre-check rejection for cloud-metadata tarball URL; got %d bytes", len(tgz))
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("not a StructuredError: %v", err)
	}
	if se.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("Code = %v, want %v (helm should never have been invoked)", se.Code, errors.ErrCodeInvalidRequest)
	}
	if !strings.Contains(err.Error(), "cloud-metadata") {
		t.Errorf("error %q does not mention cloud-metadata (wrong rejection path?)", err.Error())
	}
}

// mockResolverActive is set while a withMockResolver stub is installed
// so a nested call fails loudly instead of silently masking the outer
// stub. Guarded by mockResolverMu because Go's test runner may execute
// t.Parallel() subtests concurrently; the mu also blocks any race
// between install and the deferred restore.
var (
	mockResolverMu     sync.Mutex
	mockResolverActive bool
)

// withMockResolver swaps the package-level lookupIP for the duration of a
// single test. Fails the test via t.Fatal on double-install so a nested call
// cannot silently mask the outer stub — the failure surfaces as an
// obvious test error rather than an inscrutable resolver mismatch.
func withMockResolver(t *testing.T, fn func(ctx context.Context, host string) ([]net.IP, error)) {
	t.Helper()
	mockResolverMu.Lock()
	if mockResolverActive {
		mockResolverMu.Unlock()
		t.Fatal("withMockResolver: resolver stub already installed by an outer test; nested installs are not supported")
	}
	orig := lookupIP
	lookupIP = fn
	mockResolverActive = true
	mockResolverMu.Unlock()
	t.Cleanup(func() {
		mockResolverMu.Lock()
		lookupIP = orig
		mockResolverActive = false
		mockResolverMu.Unlock()
	})
}

// mockIndexFetcherActive / mockResolvedURLCheckerActive gate the
// package-level swappable hooks so a nested test install fails loudly
// instead of silently masking the outer stub. Same pattern as
// mockResolverActive on lookupIP — every test-time hook in this file
// is a package global, so tests that install one MUST NOT be run in
// parallel with each other via t.Parallel().
var (
	mockIndexFetcherMu           sync.Mutex
	mockIndexFetcherActive       bool
	mockResolvedURLCheckerMu     sync.Mutex
	mockResolvedURLCheckerActive bool
)

// withMockIndexFetcher swaps the package-level fetchIndexYAML so the
// index-pre-check exercises canned bytes instead of hitting the network.
// This is how the integration tests below drive resolveAndValidateHTTPIndex
// with realistic YAML payloads without depending on an httptest.Server
// (which would bind loopback and get rejected by our own egress policy).
// Fails the test via t.Fatal on double-install so a nested call cannot silently
// mask the outer stub — the failure surfaces as an obvious test error
// rather than an inscrutable fetch mismatch.
func withMockIndexFetcher(t *testing.T, fn func(ctx context.Context, indexURL string) ([]byte, error)) {
	t.Helper()
	mockIndexFetcherMu.Lock()
	if mockIndexFetcherActive {
		mockIndexFetcherMu.Unlock()
		t.Fatal("withMockIndexFetcher: fetcher stub already installed by an outer test; nested installs are not supported")
	}
	orig := fetchIndexYAML
	fetchIndexYAML = fn
	mockIndexFetcherActive = true
	mockIndexFetcherMu.Unlock()
	t.Cleanup(func() {
		mockIndexFetcherMu.Lock()
		fetchIndexYAML = orig
		mockIndexFetcherActive = false
		mockIndexFetcherMu.Unlock()
	})
}

// withMockResolvedURLChecker swaps checkResolvedChartURL, the per-tarball
// egress hook that resolveAndValidateHTTPIndex invokes on each resolved
// chart URL from the index. Tests use this to pin the EXACT URL string
// that the fixup produces (trailing-slash preservation, relative URL
// resolution against multi-segment repo paths, and so on), which cannot
// be inferred from the pass/fail outcome alone. Panics via t.Fatal on
// double-install for the same reason as withMockIndexFetcher above.
func withMockResolvedURLChecker(t *testing.T, fn func(ctx context.Context, resolvedURL string) error) {
	t.Helper()
	mockResolvedURLCheckerMu.Lock()
	if mockResolvedURLCheckerActive {
		mockResolvedURLCheckerMu.Unlock()
		t.Fatal("withMockResolvedURLChecker: checker stub already installed by an outer test; nested installs are not supported")
	}
	orig := checkResolvedChartURL
	checkResolvedChartURL = fn
	mockResolvedURLCheckerActive = true
	mockResolvedURLCheckerMu.Unlock()
	t.Cleanup(func() {
		mockResolvedURLCheckerMu.Lock()
		checkResolvedChartURL = orig
		mockResolvedURLCheckerActive = false
		mockResolvedURLCheckerMu.Unlock()
	})
}

func TestCheckEgressPolicy_LiteralIP(t *testing.T) {
	// Literal-IP URLs bypass the resolver entirely. This matrix asserts
	// that every disallowed class is rejected without any DNS activity.
	tests := []struct {
		name    string
		url     string
		wantErr bool
		reason  string // substring expected in the error message when wantErr
	}{
		{"loopback v4", "https://127.0.0.1/x", true, "loopback"},
		{"loopback v6", "https://[::1]/x", true, "loopback"},
		{"aws-metadata v4", "http://169.254.169.254/latest/meta-data/", true, "cloud-metadata"},
		{"alibaba-metadata v4", "http://100.100.100.200/latest/", true, "cloud-metadata"},
		// IPv6 metadata endpoints — pinned here so the cloud-metadata
		// classification wins over the more-generic private (fc00::/7) or
		// link-local (fe80::/10) that would otherwise mask them in logs.
		{"aws-metadata v6", "http://[fd00:ec2::254]/latest/meta-data/", true, "cloud-metadata"},
		{"link-local-metadata v6", "http://[fe80::a9fe:a9fe]/latest/", true, "cloud-metadata"},
		// IPv4-mapped IPv6: ::ffff:X.X.X.X normalizes to X.X.X.X via
		// net.IP.To4(). The disallowed classes must still fire; a bypass
		// here would be a real vulnerability.
		{"v4-mapped v6 loopback", "http://[::ffff:127.0.0.1]/x", true, "loopback"},
		{"v4-mapped v6 private", "http://[::ffff:10.0.0.1]/x", true, "private"},
		{"v4-mapped v6 aws-metadata", "http://[::ffff:169.254.169.254]/x", true, "cloud-metadata"},
		// NAT64 transition addresses — global-unicast IPv6 that decode
		// to a disallowed IPv4 once a NAT64 gateway forwards the packet.
		// disallowedIPReason recurses on the decoded v4 so the reason
		// label matches the underlying class.
		{"nat64 aws-metadata (64:ff9b::a9fe:a9fe)", "http://[64:ff9b::a9fe:a9fe]/latest/", true, "cloud-metadata"},
		{"nat64 rfc1918 (64:ff9b::a00:1)", "http://[64:ff9b::a00:1]/x", true, "private"},
		{"nat64 loopback (64:ff9b::7f00:1)", "http://[64:ff9b::7f00:1]/x", true, "loopback"},
		// 6to4 transition addresses — bytes 2-5 encode the embedded v4.
		{"6to4 rfc1918 (2002:0a00:0001::)", "http://[2002:0a00:0001::]/x", true, "private"},
		{"6to4 loopback (2002:7f00:0001::)", "http://[2002:7f00:0001::]/x", true, "loopback"},
		{"6to4 aws-metadata (2002:a9fe:a9fe::)", "http://[2002:a9fe:a9fe::]/x", true, "cloud-metadata"},
		{"link-local v4", "http://169.254.1.5/x", true, "link-local"},
		{"link-local v6", "http://[fe80::1]/x", true, "link-local"},
		{"rfc1918 10/8", "http://10.0.5.4:9200/x", true, "private"},
		{"rfc1918 172.16/12", "http://172.16.0.1/x", true, "private"},
		{"rfc1918 192.168/16", "http://192.168.1.5/x", true, "private"},
		{"cgnat 100.64/10", "http://100.64.0.1/x", true, "private"},
		{"ula fc00::/7", "http://[fc00::1]/x", true, "private"},
		{"unspecified v4", "http://0.0.0.0/x", true, "unspecified"},
		// Martian IPv4 — 0.x.y.z is Linux localhost alias, 240/4 is
		// class E reserved, 255.255.255.255 is limited broadcast. All
		// three must reject via the explicit CIDR check since Go's
		// stdlib classifiers don't cover them.
		{"martian 0/8 (Linux loopback alias)", "http://0.1.2.3/x", true, "martian"},
		{"martian 0/8 boundary", "http://0.255.255.255/x", true, "martian"},
		{"martian 240/4 class-E", "http://240.0.0.1/x", true, "martian"},
		{"martian 240/4 upper", "http://254.255.255.255/x", true, "martian"},
		{"martian 255.255.255.255 broadcast", "http://255.255.255.255/x", true, "martian"},
		{"multicast v4", "http://239.0.0.1/x", true, "multicast"},
		// oci scheme uses the same host — must be filtered too.
		{"oci loopback", "oci://127.0.0.1:5000/charts/foo", true, "loopback"},
		{"oci private", "oci://10.0.0.5/charts/foo", true, "private"},
		// Public IPs are allowed. The check never accepts a random public
		// address as "safe" — only that the class-based filter is not the
		// gate for them (real deployments should still front the server
		// with an authenticated ingress).
		{"public v4 (Google DNS)", "https://8.8.8.8/index.yaml", false, ""},
		{"public v4 (Cloudflare)", "https://1.1.1.1/index.yaml", false, ""},
	}
	// Belt-and-braces: install a resolver that panics so a test that
	// accidentally exercises the DNS path fails loudly rather than
	// silently hitting the system resolver.
	withMockResolver(t, func(_ context.Context, host string) ([]net.IP, error) {
		t.Fatalf("resolver invoked for literal-IP test with host=%q", host)
		return nil, nil
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkEgressPolicy(context.Background(), tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("not a StructuredError: %v", err)
			}
			if se.Code != errors.ErrCodeInvalidRequest {
				t.Errorf("Code = %v, want %v", se.Code, errors.ErrCodeInvalidRequest)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.reason)
			}
		})
	}
}

func TestCheckEgressPolicy_ResolvedHost(t *testing.T) {
	// Hostnames flow through lookupIP; the stub controls which IPs come back.
	tests := []struct {
		name     string
		host     string
		resolved []net.IP
		lookupOK bool
		wantErr  bool
		wantCode errors.ErrorCode
		reason   string
	}{
		{
			name:     "resolves to public",
			host:     "charts.example.com",
			resolved: []net.IP{net.IPv4(93, 184, 216, 34)},
			lookupOK: true,
			wantErr:  false,
		},
		{
			name:     "resolves to loopback",
			host:     "malicious.example.com",
			resolved: []net.IP{net.IPv4(127, 0, 0, 1)},
			lookupOK: true,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			reason:   "loopback",
		},
		{
			name:     "resolves to aws metadata",
			host:     "metadata.rebound.example",
			resolved: []net.IP{net.IPv4(169, 254, 169, 254)},
			lookupOK: true,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			reason:   "cloud-metadata",
		},
		{
			name: "split-horizon: public + private both returned",
			host: "split.example.com",
			// A hostname that resolves to BOTH a public and a private IP
			// must be rejected — the private answer would otherwise be
			// reachable via connection-reuse or DNS-cache poisoning.
			resolved: []net.IP{net.IPv4(93, 184, 216, 34), net.IPv4(10, 0, 0, 5)},
			lookupOK: true,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			reason:   "private",
		},
		{
			name:     "empty resolution",
			host:     "void.example.com",
			resolved: nil,
			lookupOK: true,
			wantErr:  true,
			wantCode: errors.ErrCodeUnavailable,
			reason:   "no addresses",
		},
		{
			name:     "resolver error",
			host:     "nx.example.com",
			resolved: nil,
			lookupOK: false,
			wantErr:  true,
			wantCode: errors.ErrCodeUnavailable,
			reason:   "cannot resolve",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMockResolver(t, func(_ context.Context, host string) ([]net.IP, error) {
				if host != tt.host {
					t.Fatalf("unexpected host: got %q, want %q", host, tt.host)
				}
				if !tt.lookupOK {
					return nil, stderrors.New("mock DNS failure")
				}
				return tt.resolved, nil
			})
			err := checkEgressPolicy(context.Background(), "https://"+tt.host+"/index.yaml")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("not a StructuredError: %v", err)
			}
			if se.Code != tt.wantCode {
				t.Errorf("Code = %v, want %v", se.Code, tt.wantCode)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.reason)
			}
		})
	}
}

func TestCheckEgressPolicy_MalformedURL(t *testing.T) {
	// URL parse failures and host-less URLs are rejected as InvalidRequest,
	// not surfaced as resolver errors — a caller who supplies "not a url"
	// gets a coherent 400, not a spurious Unavailable.
	tests := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"no host", "https:///path"},
		{"invalid scheme character", "ht!tp://foo"},
	}
	withMockResolver(t, func(_ context.Context, host string) ([]net.IP, error) {
		t.Fatalf("resolver invoked for malformed-URL test with host=%q", host)
		return nil, nil
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkEgressPolicy(context.Background(), tt.url)
			if err == nil {
				t.Fatalf("expected error for %q", tt.url)
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("not a StructuredError: %v", err)
			}
			if se.Code != errors.ErrCodeInvalidRequest {
				t.Errorf("Code = %v, want %v", se.Code, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

func TestReadBoundedTgz(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name      string
		body      []byte
		limit     int64
		wantErr   bool
		wantCode  errors.ErrorCode
		wantBytes int
	}{
		{"under limit", []byte("payload"), 1024, false, "", 7},
		{"exactly at limit", make([]byte, 128), 128, false, "", 128},
		{"one over limit rejected", make([]byte, 129), 128, true, errors.ErrCodeInvalidRequest, 0},
		{"far over limit rejected", make([]byte, 4096), 128, true, errors.ErrCodeInvalidRequest, 0},
		{"empty file accepted", []byte{}, 128, false, "", 0},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, "chart-"+string(rune('a'+i))+".tgz")
			if writeErr := os.WriteFile(path, tt.body, 0o600); writeErr != nil {
				t.Fatalf("WriteFile: %v", writeErr)
			}
			got, err := readBoundedTgz(path, tt.limit)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var se *errors.StructuredError
				if !stderrors.As(err, &se) {
					t.Fatalf("not a StructuredError: %v", err)
				}
				if se.Code != tt.wantCode {
					t.Errorf("Code = %v, want %v", se.Code, tt.wantCode)
				}
				return
			}
			if len(got) != tt.wantBytes {
				t.Errorf("len = %d, want %d", len(got), tt.wantBytes)
			}
		})
	}
}

func TestReadBoundedTgz_Missing(t *testing.T) {
	// A missing file surfaces as ErrCodeInternal (Stat failure), NOT as an
	// InvalidRequest — the caller's bug is a lost pull-output tarball, not
	// a caller-supplied over-cap artifact.
	_, err := readBoundedTgz(filepath.Join(t.TempDir(), "does-not-exist.tgz"), 128)
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("not a StructuredError: %v", err)
	}
	if se.Code != errors.ErrCodeInternal {
		t.Errorf("Code = %v, want %v", se.Code, errors.ErrCodeInternal)
	}
}

// TestResolveAndValidateHTTPIndex is the integration test the review
// asked for: an index.yaml served over HTTP whose entries point at
// blocked chart hosts must be rejected BEFORE `helm pull` is invoked.
// Uses a mock fetcher instead of an httptest.Server because
// httptest.Server binds loopback, and our own layer-1 egress check
// would reject the initial c.Repository = "https://127.0.0.1/..." fetch
// before this second-layer code ever ran.
func TestResolveAndValidateHTTPIndex(t *testing.T) {
	// Common inputs. Repository uses a literal public IP so the outer
	// checkEgressPolicy the production Pull() would run first is a no-op
	// here; this test drives only the second-layer index pre-check.
	const chart = "victim-chart"
	const version = "1.2.3"
	const repo = "https://93.184.216.34/charts"

	tests := []struct {
		name         string
		component    Component
		indexBody    string
		fetchErr     error
		wantErr      bool
		wantCode     errors.ErrorCode
		wantContains string
	}{
		{
			name:      "all public urls pass",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls:
        - https://93.184.216.34/charts/victim-chart-1.2.3.tgz
`,
			wantErr: false,
		},
		{
			name:      "relative url resolves against repo (allowed)",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls: [victim-chart-1.2.3.tgz]
`,
			wantErr: false,
		},
		{
			name:      "public index points at cloud-metadata tarball rejected",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			// The classic SSRF vector: public index.yaml owned by attacker,
			// urls point at the instance metadata endpoint. Pre-check must
			// reject BEFORE helm follows the URL.
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls:
        - http://169.254.169.254/latest/meta-data/iam/security-credentials/
`,
			wantErr:      true,
			wantCode:     errors.ErrCodeInvalidRequest,
			wantContains: "cloud-metadata",
		},
		{
			name:      "public index points at private-network tarball rejected",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls:
        - http://10.0.5.4:8080/leak.tgz
`,
			wantErr:      true,
			wantCode:     errors.ErrCodeInvalidRequest,
			wantContains: "private",
		},
		{
			name:      "any private url in a mixed list rejects the whole entry",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			// A version entry with two mirror URLs where one is public and
			// one is private must be rejected on the private one — a helm
			// pull might pick either mirror.
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls:
        - https://93.184.216.34/charts/victim-chart-1.2.3.tgz
        - http://127.0.0.1/mirror/victim-chart-1.2.3.tgz
`,
			wantErr:      true,
			wantCode:     errors.ErrCodeInvalidRequest,
			wantContains: "loopback",
		},
		{
			name:      "chart entry missing from index",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			indexBody: `apiVersion: v1
entries:
  other-chart:
    - version: 1.0.0
      urls: [https://93.184.216.34/x.tgz]
`,
			wantErr:      true,
			wantCode:     errors.ErrCodeNotFound,
			wantContains: "no entry for chart",
		},
		{
			name:      "chart present but requested version missing",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 9.9.9
      urls: [https://93.184.216.34/other.tgz]
`,
			wantErr:      true,
			wantCode:     errors.ErrCodeNotFound,
			wantContains: "no index entry matching pinned version",
		},
		{
			name:      "constraint 1.2.x resolves to highest matching entry",
			component: Component{Name: chart, ChartName: chart, Version: "1.2.x", Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.1.5
      urls: [https://93.184.216.34/victim-chart-1.1.5.tgz]
    - version: 1.2.7
      urls: [https://93.184.216.34/victim-chart-1.2.7.tgz]
    - version: 1.2.9
      urls: [https://93.184.216.34/victim-chart-1.2.9.tgz]
    - version: 1.3.0
      urls: [https://93.184.216.34/victim-chart-1.3.0.tgz]
`,
			// Picks 1.2.9 (highest 1.2.x, matches helm's own resolution).
			// The URL is public so the whole chain passes.
			wantErr: false,
		},
		{
			name:      "constraint resolves and enforces egress on the resolved entry",
			component: Component{Name: chart, ChartName: chart, Version: "^1.2", Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls: [https://93.184.216.34/ok.tgz]
    - version: 1.5.0
      urls: [http://169.254.169.254/creds]
`,
			// ^1.2 → highest 1.x — the 1.5.0 entry (with cloud-metadata
			// URL) wins the constraint AND then fails the egress check.
			// A byte-identical policy: constraint resolution feeds into
			// egress check exactly like an exact match would.
			wantErr:      true,
			wantCode:     errors.ErrCodeInvalidRequest,
			wantContains: "cloud-metadata",
		},
		{
			name:      "constraint with no matching entry",
			component: Component{Name: chart, ChartName: chart, Version: "^5.0", Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.0.0
      urls: [https://93.184.216.34/x.tgz]
`,
			wantErr:      true,
			wantCode:     errors.ErrCodeNotFound,
			wantContains: "no index entry satisfying constraint",
		},
		{
			name:      "version neither pinned nor constraint",
			component: Component{Name: chart, ChartName: chart, Version: "not-a-version", Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.0.0
      urls: [https://93.184.216.34/x.tgz]
`,
			wantErr:      true,
			wantCode:     errors.ErrCodeInvalidRequest,
			wantContains: "neither a valid pinned semver nor a valid semver constraint",
		},
		{
			name:      "v-prefix canonicalization: v1.2.3 matches 1.2.3 entry",
			component: Component{Name: chart, ChartName: chart, Version: "v1.2.3", Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls: [https://93.184.216.34/victim-1.2.3.tgz]
`,
			// helm and masterminds/semver both strip the leading v; the
			// exact-string fast path misses but the semver-equality fallback
			// catches this so we don't spuriously reject.
			wantErr: false,
		},
		{
			name:         "malformed yaml rejected",
			component:    Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			indexBody:    "this is not: valid: yaml: at all\n  - nope\n",
			wantErr:      true,
			wantCode:     errors.ErrCodeInvalidRequest,
			wantContains: "not valid YAML",
		},
		{
			name:      "OCI component skipped (no pre-check possible)",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: "oci://ghcr.io/nvidia/x", IsOCI: true},
			// If the pre-check RAN for an OCI component and used the mock,
			// the mock's poisoned body would reject. Instead the function
			// must return nil without consulting the fetcher at all.
			indexBody: `entries: { victim-chart: [ { version: 1.2.3, urls: [http://127.0.0.1/leak.tgz] } ] }`,
			wantErr:   false,
		},
		{
			// Relative URLs are resolved against the repo directory: for
			// `Repository: "https://93.184.216.34/charts"` a relative URL
			// `victim-chart-1.2.3.tgz` must resolve to
			// `https://93.184.216.34/charts/victim-chart-1.2.3.tgz` (the
			// URL helm actually fetches), NOT
			// `https://93.184.216.34/victim-chart-1.2.3.tgz` (which is
			// what raw RFC 3986 does without a trailing-slash fixup).
			// Landing on a public URL means the egress check passes; the
			// nil error here is only reachable if the trailing-slash fix
			// worked. Without it the resolved URL would still be public
			// too (so no coverage from the egress path) — the anchor is
			// the exact URL passed to checkEgressPolicy, which we assert
			// via the mock resolver's expectation.
			name:      "multi-segment repo with relative url keeps repo directory",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls: [victim-chart-1.2.3.tgz]
`,
			wantErr: false,
		},
		{
			name:      "matched entry with empty urls list rejected",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls: []
`,
			wantErr:      true,
			wantCode:     errors.ErrCodeNotFound,
			wantContains: "no tarball URLs",
		},
		{
			// F17 defense at the boundary: two index entries with the
			// same version, first public, second private. The prior
			// selectChartURLs returned only the first entry's URLs, so
			// the private one bypassed egress; the current impl unions
			// URLs across all matching entries and this test locks it in.
			name:      "duplicate same-version entries — private URL in second row still egress-rejected",
			component: Component{Name: chart, ChartName: chart, Version: version, Repository: repo},
			indexBody: `apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls: [https://93.184.216.34/charts/victim-chart-1.2.3.tgz]
    - version: 1.2.3
      urls: [http://169.254.169.254/latest/meta-data/steal.tgz]
`,
			wantErr:      true,
			wantCode:     errors.ErrCodeInvalidRequest,
			wantContains: "cloud-metadata",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetcherCalled := false
			withMockIndexFetcher(t, func(_ context.Context, indexURL string) ([]byte, error) {
				fetcherCalled = true
				if !tt.component.IsOCI {
					wantURL := strings.TrimRight(tt.component.Repository, "/") + "/index.yaml"
					if indexURL != wantURL {
						t.Errorf("indexURL = %q, want %q", indexURL, wantURL)
					}
				}
				if tt.fetchErr != nil {
					return nil, tt.fetchErr
				}
				return []byte(tt.indexBody), nil
			})
			err := resolveAndValidateHTTPIndex(context.Background(), tt.component)
			if tt.component.IsOCI && fetcherCalled {
				t.Fatal("OCI component: fetcher should not have been consulted")
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("not a StructuredError: %v", err)
			}
			if se.Code != tt.wantCode {
				t.Errorf("Code = %v, want %v", se.Code, tt.wantCode)
			}
			if tt.wantContains != "" && !strings.Contains(err.Error(), tt.wantContains) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantContains)
			}
		})
	}
}

// TestDefaultFetchIndexYAML covers the production HTTP fetcher end-to-end
// against an httptest.Server. httptest binds loopback, which the
// production checkFetchTargetURL (defaults to checkEgressPolicy) would
// reject on the initial URL fail-closed guard AND on every redirect
// hop. Stubbing to always-pass at the parent-test level focuses each
// subtest on the HTTP-layer behavior under test (status handling, body
// cap, redirect count). The redirect-to-private-IP subtest re-installs
// the real checkEgressPolicy locally to exercise that branch.
func TestDefaultFetchIndexYAML(t *testing.T) {
	origCheck := checkFetchTargetURL
	checkFetchTargetURL = func(_ context.Context, _ string) error { return nil }
	t.Cleanup(func() { checkFetchTargetURL = origCheck })

	t.Run("happy path 200 with body", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/index.yaml" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, "apiVersion: v1\nentries: {}\n")
		}))
		defer ts.Close()
		body, err := defaultFetchIndexYAML(context.Background(), ts.URL+"/index.yaml")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if !strings.Contains(string(body), "apiVersion") {
			t.Errorf("body = %q, want to contain apiVersion", body)
		}
	})

	t.Run("non-200 surfaces as Unavailable", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer ts.Close()
		// Use doFetchIndexYAMLAttempt directly to test status classification
		// without triggering retry logic.
		_, err := doFetchIndexYAMLAttempt(context.Background(), ts.URL+"/index.yaml")
		if err == nil {
			t.Fatal("expected error for 500")
		}
		var se *errors.StructuredError
		if !stderrors.As(err, &se) || se.Code != errors.ErrCodeUnavailable {
			t.Errorf("Code = %v (err=%v), want %v", codeOf(err), err, errors.ErrCodeUnavailable)
		}
	})

	t.Run("oversized body rejected as InvalidRequest", func(t *testing.T) {
		// Stream HelmChartIndexBodyLimit+1 bytes in bounded chunks so the
		// test itself does not allocate a full-cap buffer just to prove
		// the cap enforcement works. The handler flushes each chunk so
		// the client-side LimitReader hits the +1 byte the same way it
		// would for a real oversize payload.
		const chunk = 64 * 1024
		buf := make([]byte, chunk)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			flusher, _ := w.(http.Flusher)
			remaining := defaults.HelmChartIndexBodyLimit + 1
			for remaining > 0 {
				n := min(remaining, int64(chunk))
				if _, err := w.Write(buf[:n]); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
				remaining -= n
			}
		}))
		defer ts.Close()
		_, err := defaultFetchIndexYAML(context.Background(), ts.URL+"/index.yaml")
		if err == nil {
			t.Fatal("expected size-cap rejection")
		}
		var se *errors.StructuredError
		if !stderrors.As(err, &se) || se.Code != errors.ErrCodeInvalidRequest {
			t.Errorf("Code = %v, want %v", codeOf(err), errors.ErrCodeInvalidRequest)
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("error %q does not mention exceeds", err.Error())
		}
	})

	t.Run("redirect to private IP rejected by CheckRedirect", func(t *testing.T) {
		// The parent test stubs checkFetchTargetURL to always-pass so
		// the httptest.Server loopback URL is reachable. This subtest
		// restores the real checkEgressPolicy locally to exercise the
		// redirect-hop rejection branch. The initial-URL guard is
		// still stubbed away for this subtest so we reach the redirect
		// step — the reviewer's N4 concern is covered by
		// TestDefaultFetchIndexYAML_InitialURLFailClosed below.
		origCheck := checkFetchTargetURL
		checkFetchTargetURL = func(ctx context.Context, u string) error {
			// Pass the initial URL, apply the real policy to any
			// redirect target. Simple heuristic: initial URL contains
			// the test server's port literal; redirect target is
			// 127.0.0.1:1 (a different port constant).
			if strings.Contains(u, "127.0.0.1:1/") {
				return checkEgressPolicy(ctx, u)
			}
			return nil
		}
		t.Cleanup(func() { checkFetchTargetURL = origCheck })

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Redirect target is another loopback URL — the redirect check
			// treats loopback (127/8) as disallowed regardless of DNS.
			http.Redirect(w, r, "http://127.0.0.1:1/malicious", http.StatusFound)
		}))
		defer ts.Close()
		_, err := defaultFetchIndexYAML(context.Background(), ts.URL+"/index.yaml")
		if err == nil {
			t.Fatal("expected redirect rejection")
		}
		// Redirect errors wrap the underlying policy error; the underlying
		// StructuredError code must be preserved as InvalidRequest.
		var se *errors.StructuredError
		if !stderrors.As(err, &se) || se.Code != errors.ErrCodeInvalidRequest {
			t.Errorf("Code = %v (err=%v), want %v", codeOf(err), err, errors.ErrCodeInvalidRequest)
		}
	})

	t.Run("redirect hop count cap enforced", func(t *testing.T) {
		// Chain of self-redirects to prove the hop-count guard fires
		// when a public host returns an endless redirect loop. The
		// parent test's stub already suppresses egress, so every hop
		// passes and only the len(via) >= HelmChartIndexMaxRedirects
		// branch can stop the loop.

		var ts *httptest.Server
		hops := 0
		ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hops++
			http.Redirect(w, r, ts.URL+"/index.yaml", http.StatusFound)
		}))
		defer ts.Close()
		_, err := defaultFetchIndexYAML(context.Background(), ts.URL+"/index.yaml")
		if err == nil {
			t.Fatal("expected redirect-count rejection")
		}
		var se *errors.StructuredError
		if !stderrors.As(err, &se) || se.Code != errors.ErrCodeInvalidRequest {
			t.Errorf("Code = %v (err=%v), want %v", codeOf(err), err, errors.ErrCodeInvalidRequest)
		}
		if !strings.Contains(err.Error(), "exceeded") {
			t.Errorf("error %q does not mention exceeded redirects", err.Error())
		}
		// Sanity: the handler was called at least HelmChartIndexMaxRedirects
		// times (initial GET + N redirects that CheckRedirect passed
		// through). The Nth redirect is what trips the cap.
		if hops < defaults.HelmChartIndexMaxRedirects {
			t.Errorf("handler served %d hops, want at least %d before cap fires",
				hops, defaults.HelmChartIndexMaxRedirects)
		}
	})
}

// TestDefaultFetchIndexYAML_InitialURLFailClosed pins the fail-closed
// guard at the top of defaultFetchIndexYAML: a caller that calls the
// fetcher directly with a private-range URL (bypassing the upstream
// Pull() → checkEgressPolicy(c.Repository) ordering) must get an
// InvalidRequest before any network I/O happens. This test does NOT
// stub checkFetchTargetURL so the production egress check fires.
func TestDefaultFetchIndexYAML_InitialURLFailClosed(t *testing.T) {
	// No stubbing: the production checkEgressPolicy runs. Passing a
	// literal loopback URL must be rejected before the fetcher opens
	// a socket.
	_, err := defaultFetchIndexYAML(context.Background(), "http://127.0.0.1/index.yaml")
	if err == nil {
		t.Fatal("expected fail-closed rejection of loopback initial URL")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) || se.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("Code = %v (err=%v), want %v", codeOf(err), err, errors.ErrCodeInvalidRequest)
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error %q does not mention loopback", err.Error())
	}
}

// TestAttachHelmBasicAuth pins the credential-leak defense: even if the
// operator has set HELM_REPOSITORY_USERNAME/PASSWORD, credentials must
// only be attached to requests that (a) are HTTPS and (b) target the
// exact host named in AICR_HELM_REPOSITORY_HOST. Everything else — no
// opt-in, http-scheme, mismatched host — must send an unauthenticated
// request so a caller-supplied repository URL cannot exfiltrate the
// operator's helm credentials to an attacker-controlled endpoint.
func TestAttachHelmBasicAuth(t *testing.T) {
	tests := []struct {
		name             string
		user, pass, host string // env values; empty means unset
		url              string
		wantAuthHeader   bool
	}{
		{
			name: "no username set — no auth ever",
			url:  "https://allowed.example/index.yaml",
			host: "allowed.example",
		},
		{
			name: "username set but no host allowlist — no auth",
			user: "u", pass: "p",
			url: "https://allowed.example/index.yaml",
		},
		{
			name: "http scheme rejected even when host matches",
			user: "u", pass: "p",
			host: "allowed.example",
			url:  "http://allowed.example/index.yaml",
		},
		{
			name: "host mismatch — no auth",
			user: "u", pass: "p",
			host: "allowed.example",
			url:  "https://attacker.example/index.yaml",
		},
		{
			name: "https + exact host match — auth attached",
			user: "u", pass: "p",
			host:           "allowed.example",
			url:            "https://allowed.example/index.yaml",
			wantAuthHeader: true,
		},
		{
			name: "case-insensitive host match still attaches",
			user: "u", pass: "p",
			host:           "Allowed.Example",
			url:            "https://ALLOWED.example/index.yaml",
			wantAuthHeader: true,
		},
		{
			name: "port in URL is part of the host — mismatch suppresses",
			user: "u", pass: "p",
			host: "allowed.example",
			url:  "https://allowed.example:8443/index.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv doesn't unset — use os.Unsetenv guard for the
			// negative cases so we exercise "truly missing," not "empty."
			for _, k := range []string{"HELM_REPOSITORY_USERNAME", "HELM_REPOSITORY_PASSWORD", defaults.EnvHelmRepositoryHost} {
				prev, wasSet := os.LookupEnv(k)
				os.Unsetenv(k)
				t.Cleanup(func() {
					if wasSet {
						os.Setenv(k, prev)
					}
				})
			}
			if tt.user != "" {
				t.Setenv("HELM_REPOSITORY_USERNAME", tt.user)
			}
			if tt.pass != "" {
				t.Setenv("HELM_REPOSITORY_PASSWORD", tt.pass)
			}
			if tt.host != "" {
				t.Setenv(defaults.EnvHelmRepositoryHost, tt.host)
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tt.url, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			attachHelmBasicAuth(req)
			gotUser, gotPass, hasAuth := req.BasicAuth()
			if hasAuth != tt.wantAuthHeader {
				t.Fatalf("BasicAuth set = %v, want %v (url=%q host-env=%q)",
					hasAuth, tt.wantAuthHeader, tt.url, tt.host)
			}
			if tt.wantAuthHeader {
				if gotUser != tt.user || gotPass != tt.pass {
					t.Errorf("BasicAuth = (%q,%q), want (%q,%q)",
						gotUser, gotPass, tt.user, tt.pass)
				}
			}
		})
	}
}

// TestSelectChartURLs exercises the constraint / exact-version resolver
// as a pure function, independent of the fetcher, so the semver policy
// is pinned even if the higher-level flow changes. Mirrors helm's own
// resolution behavior: exact string match wins first (for non-semver
// aliases like "latest"), then a semver-equal fallback (v-prefix
// canonicalization), then constraint-highest-match.
func TestSelectChartURLs(t *testing.T) {
	entries := []helmIndexEntry{
		{Version: "1.0.0", URLs: []string{"u-100"}},
		{Version: "1.2.3", URLs: []string{"u-123"}},
		{Version: "1.2.7", URLs: []string{"u-127"}},
		{Version: "1.5.0", URLs: []string{"u-150"}},
		{Version: "2.0.0-beta.1", URLs: []string{"u-200b1"}},
		{Version: "latest", URLs: []string{"u-latest"}},
	}
	tests := []struct {
		name        string
		spec        string
		wantErr     bool
		wantCode    errors.ErrorCode
		wantURLs    []string
		wantVersion string
	}{
		{"exact pinned hits", "1.2.3", false, "", []string{"u-123"}, "1.2.3"},
		{"non-semver alias hits by exact match", "latest", false, "", []string{"u-latest"}, "latest"},
		{"v-prefix canonicalizes", "v1.2.3", false, "", []string{"u-123"}, "1.2.3"},
		{"constraint 1.2.x picks 1.2.7", "1.2.x", false, "", []string{"u-127"}, "1.2.7"},
		// Two-segment spec "1.2" — helm treats this as ">=1.2.0 <1.3.0".
		// StrictNewVersion rejects "1.2" as pinned, so it falls through to
		// the constraint path and resolves to 1.2.7. NewVersion would have
		// coerced it to "1.2.0" and returned NotFound (bug the fix closes).
		{"partial spec 1.2 routes to constraint", "1.2", false, "", []string{"u-127"}, "1.2.7"},
		{"partial spec 1 routes to constraint", "1", false, "", []string{"u-150"}, "1.5.0"},
		{"caret ^1 picks 1.5.0", "^1", false, "", []string{"u-150"}, "1.5.0"},
		{"tilde ~1.2 picks 1.2.7", "~1.2", false, "", []string{"u-127"}, "1.2.7"},
		{"pinned miss reports NotFound", "3.0.0", true, errors.ErrCodeNotFound, nil, ""},
		{"constraint miss reports NotFound", "^5", true, errors.ErrCodeNotFound, nil, ""},
		{"unparseable spec is InvalidRequest", "not-a-thing", true, errors.ErrCodeInvalidRequest, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURLs, gotVersion, err := selectChartURLs(entries, "chartX", tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if codeOf(err) != tt.wantCode {
					t.Errorf("Code = %v, want %v (err=%v)", codeOf(err), tt.wantCode, err)
				}
				if !strings.Contains(err.Error(), "chartX") {
					t.Errorf("error %q does not name the chart", err.Error())
				}
				return
			}
			if gotVersion != tt.wantVersion {
				t.Errorf("resolvedVersion = %q, want %q", gotVersion, tt.wantVersion)
			}
			if len(gotURLs) != len(tt.wantURLs) || (len(gotURLs) > 0 && gotURLs[0] != tt.wantURLs[0]) {
				t.Errorf("urls = %v, want %v", gotURLs, tt.wantURLs)
			}
		})
	}
}

// TestSelectChartURLs_DuplicateEntriesUnion pins the F17 defense:
// selectChartURLs must return the UNION of URLs across every entry
// matching the resolved version, not just the first. If the pre-check
// only validated the first entry, a poisoned index with two same-version
// rows (public first, private second) could let helm's own re-resolution
// pick the private URL bypassing egress.
func TestSelectChartURLs_DuplicateEntriesUnion(t *testing.T) {
	tests := []struct {
		name     string
		entries  []helmIndexEntry
		spec     string
		wantURLs []string
	}{
		{
			name: "exact-string duplicates union",
			// Two entries with identical version strings — a poisoned or
			// misconfigured index. Both URLs must reach the egress check.
			entries: []helmIndexEntry{
				{Version: "1.2.3", URLs: []string{"https://public.example/foo.tgz"}},
				{Version: "1.2.3", URLs: []string{"http://169.254.169.254/steal.tgz"}},
			},
			spec:     "1.2.3",
			wantURLs: []string{"https://public.example/foo.tgz", "http://169.254.169.254/steal.tgz"},
		},
		{
			name: "semver-equivalent duplicates union under pinned path",
			// Different string encodings of the same semver — "v1.2.3"
			// and "1.2.3" both parse to 1.2.3. Both must be egress-checked.
			entries: []helmIndexEntry{
				{Version: "v1.2.3", URLs: []string{"https://public.example/a.tgz"}},
				{Version: "1.2.3", URLs: []string{"http://10.0.0.1/b.tgz"}},
			},
			spec:     "1.2.3",
			wantURLs: []string{"https://public.example/a.tgz", "http://10.0.0.1/b.tgz"},
		},
		{
			name: "constraint path duplicates union at best version",
			// Constraint 1.2.x resolves to 1.2.7. If the index has two
			// 1.2.7 entries, both URLs must be returned.
			entries: []helmIndexEntry{
				{Version: "1.2.5", URLs: []string{"u-125"}},
				{Version: "1.2.7", URLs: []string{"u-127-primary"}},
				{Version: "1.2.7", URLs: []string{"u-127-mirror"}},
			},
			spec:     "1.2.x",
			wantURLs: []string{"u-127-primary", "u-127-mirror"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := selectChartURLs(tt.entries, "chartX", tt.spec)
			if err != nil {
				t.Fatalf("selectChartURLs: %v", err)
			}
			if len(got) != len(tt.wantURLs) {
				t.Fatalf("urls = %v, want %v", got, tt.wantURLs)
			}
			for i := range got {
				if got[i] != tt.wantURLs[i] {
					t.Errorf("urls[%d] = %q, want %q", i, got[i], tt.wantURLs[i])
				}
			}
		})
	}
}

// codeOf extracts a StructuredError code for concise assertion messages;
// returns "" when err is not a StructuredError.
func codeOf(err error) errors.ErrorCode {
	if se, ok := stderrors.AsType[*errors.StructuredError](err); ok {
		return se.Code
	}
	return ""
}

// TestResolveAndValidateHTTPIndex_RelativeURLResolution anchors the
// trailing-slash fixup on the repository URL. When Repository is
// "https://host/nested/path/charts" (no trailing slash), RFC 3986
// ResolveReference treats "charts" as a filename and drops it, so a
// relative index entry "victim-1.2.3.tgz" would resolve to
// "https://host/nested/path/victim-1.2.3.tgz" instead of the correct
// "https://host/nested/path/charts/victim-1.2.3.tgz" that helm actually
// fetches.
//
// This test pins the exact URL string passed to the resolved-URL egress
// check via withMockResolvedURLChecker, so a regression in the trailing-
// slash normalization is caught by string equality, not by a downstream
// egress-verdict proxy (both the buggy and correct URLs would land on
// the same public host and pass egress alone).
func TestResolveAndValidateHTTPIndex_RelativeURLResolution(t *testing.T) {
	withMockIndexFetcher(t, func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls: [victim-chart-1.2.3.tgz]
`), nil
	})
	var captured []string
	withMockResolvedURLChecker(t, func(_ context.Context, u string) error {
		captured = append(captured, u)
		return nil
	})
	err := resolveAndValidateHTTPIndex(context.Background(), Component{
		Name:       "victim-chart",
		ChartName:  "victim-chart",
		Version:    "1.2.3",
		Repository: "https://93.184.216.34/nested/path/charts",
	})
	if err != nil {
		t.Fatalf("resolveAndValidateHTTPIndex: %v", err)
	}
	want := "https://93.184.216.34/nested/path/charts/victim-chart-1.2.3.tgz"
	if len(captured) != 1 {
		t.Fatalf("expected exactly one resolved-URL egress call, got %d: %v", len(captured), captured)
	}
	if captured[0] != want {
		t.Errorf("resolved URL = %q, want %q\n(a mismatch here means the trailing-slash fixup regressed and dropped the repo directory segment)",
			captured[0], want)
	}
}

// TestResolveAndValidateHTTPIndex_AbsoluteURLKeepsHost asserts the
// captured URL for an absolute chart URL is passed through unchanged —
// the trailing-slash fixup applies only to the RESOLVE step for
// relative URLs, not to the absolute case where the entry provides a
// full URL. Pins that the fixup is not accidentally reshaping absolute
// URLs (which would break real charts whose index lists absolute URLs).
func TestResolveAndValidateHTTPIndex_AbsoluteURLKeepsHost(t *testing.T) {
	withMockIndexFetcher(t, func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`apiVersion: v1
entries:
  victim-chart:
    - version: 1.2.3
      urls: [https://cdn.example/mirrors/victim-chart-1.2.3.tgz]
`), nil
	})
	var captured []string
	withMockResolvedURLChecker(t, func(_ context.Context, u string) error {
		captured = append(captured, u)
		return nil
	})
	err := resolveAndValidateHTTPIndex(context.Background(), Component{
		Name:       "victim-chart",
		ChartName:  "victim-chart",
		Version:    "1.2.3",
		Repository: "https://93.184.216.34/nested/path/charts",
	})
	if err != nil {
		t.Fatalf("resolveAndValidateHTTPIndex: %v", err)
	}
	want := "https://cdn.example/mirrors/victim-chart-1.2.3.tgz"
	if len(captured) != 1 || captured[0] != want {
		t.Errorf("captured = %v, want [%q]", captured, want)
	}
}

// TestDefaultFetchIndexYAML_4xx pins the per-status classification the
// pre-check applies to a non-200 index fetch. Orchestrators branching on
// the returned code need distinct retryability signals:
//   - 404             -> NotFound          (chart repo does not exist)
//   - 401 / 403       -> Unauthorized      (credentials issue)
//   - other 4xx       -> InvalidRequest    (caller-shaped problem)
//   - 5xx / other     -> Unavailable       (transient upstream, retryable)
func TestDefaultFetchIndexYAML_4xx(t *testing.T) {
	// Stub the initial-URL fail-closed guard so the httptest.Server
	// loopback URL is reachable. This test focuses on the status-code
	// classification, not the egress check (which has its own tests).
	origCheck := checkFetchTargetURL
	checkFetchTargetURL = func(_ context.Context, _ string) error { return nil }
	t.Cleanup(func() { checkFetchTargetURL = origCheck })

	tests := []struct {
		name       string
		status     int
		wantCode   errors.ErrorCode
		wantReason string
	}{
		{"404 not found", http.StatusNotFound, errors.ErrCodeNotFound, "404"},
		{"401 unauthorized", http.StatusUnauthorized, errors.ErrCodeUnauthorized, "401"},
		{"403 forbidden", http.StatusForbidden, errors.ErrCodeUnauthorized, "403"},
		// 408 / 429 are the retryable 4xx — an orchestrator that branches
		// on the returned code needs to distinguish them from the non-
		// retryable client-error class below.
		{"408 request timeout", http.StatusRequestTimeout, errors.ErrCodeUnavailable, "408"},
		{"429 too many requests", http.StatusTooManyRequests, errors.ErrCodeUnavailable, "429"},
		{"400 bad request", http.StatusBadRequest, errors.ErrCodeInvalidRequest, "400"},
		{"409 conflict", http.StatusConflict, errors.ErrCodeInvalidRequest, "409"},
		{"499 client-closed", 499, errors.ErrCodeInvalidRequest, "499"},
		// 5xx stays Unavailable — retryable class.
		{"500 internal", http.StatusInternalServerError, errors.ErrCodeUnavailable, "500"},
		{"502 bad gateway", http.StatusBadGateway, errors.ErrCodeUnavailable, "502"},
		{"503 unavailable", http.StatusServiceUnavailable, errors.ErrCodeUnavailable, "503"},
		{"504 gateway timeout", http.StatusGatewayTimeout, errors.ErrCodeUnavailable, "504"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", tt.status)
			}))
			defer ts.Close()
			// Use doFetchIndexYAMLAttempt directly to test status classification
			// without triggering retry logic.
			_, err := doFetchIndexYAMLAttempt(context.Background(), ts.URL+"/index.yaml")
			if err == nil {
				t.Fatalf("expected error for HTTP %d", tt.status)
			}
			if codeOf(err) != tt.wantCode {
				t.Errorf("Code = %v, want %v (err=%v)", codeOf(err), tt.wantCode, err)
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantReason)
			}
		})
	}
}

// TestAttachHelmBasicAuth_ConstantsUsed verifies the attachment path
// reads through the named env-var constants (defaults.EnvHelmRepositoryUsername
// / EnvHelmRepositoryPassword) rather than raw string literals — a regression
// here would silently break credential attachment for anyone consulting
// only the constants.
func TestAttachHelmBasicAuth_ConstantsUsed(t *testing.T) {
	if defaults.EnvHelmRepositoryUsername != "HELM_REPOSITORY_USERNAME" {
		t.Errorf("EnvHelmRepositoryUsername = %q, want HELM_REPOSITORY_USERNAME", defaults.EnvHelmRepositoryUsername)
	}
	if defaults.EnvHelmRepositoryPassword != "HELM_REPOSITORY_PASSWORD" {
		t.Errorf("EnvHelmRepositoryPassword = %q, want HELM_REPOSITORY_PASSWORD", defaults.EnvHelmRepositoryPassword)
	}
}

// TestResolveAndValidateHTTPIndex_FetchError verifies a fetch-layer error
// (network unreachable, TLS failure, DNS outage) is surfaced as
// ErrCodeUnavailable so operators can distinguish transient upstream
// failure from a policy rejection (which is InvalidRequest).
func TestResolveAndValidateHTTPIndex_FetchError(t *testing.T) {
	withMockIndexFetcher(t, func(_ context.Context, _ string) ([]byte, error) {
		return nil, errors.New(errors.ErrCodeUnavailable, "connection refused")
	})
	err := resolveAndValidateHTTPIndex(context.Background(), Component{
		Name: "x", ChartName: "x", Version: "1", Repository: "https://93.184.216.34",
	})
	if err == nil {
		t.Fatal("expected fetch error to surface")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("not a StructuredError: %v", err)
	}
	if se.Code != errors.ErrCodeUnavailable {
		t.Errorf("Code = %v, want %v", se.Code, errors.ErrCodeUnavailable)
	}
}

// TestRedactURL is the unit-level contract for the shared redactor. Every
// caller-visible log / error message that embeds a caller-supplied URL
// must route through this helper so a Repository like
// "https://user:secret@repo.example/charts" cannot leak the password.
func TestRedactURL(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{"no userinfo unchanged", "https://charts.jetstack.io/index.yaml", "https://charts.jetstack.io/index.yaml"},
		{"password masked", "https://user:secret@repo.example/charts", "https://user:xxxxx@repo.example/charts"},
		{"user only unchanged", "https://user@repo.example/charts", "https://user@repo.example/charts"},
		{"oci with creds masked", "oci://user:secret@registry.example/foo", "oci://user:xxxxx@registry.example/foo"},
		{"resolved absolute chart URL with creds masked",
			"https://user:secret@cdn.example/charts/foo-1.2.3.tgz",
			"https://user:xxxxx@cdn.example/charts/foo-1.2.3.tgz"},
		// Parse-failure cases: a credential-bearing URL that url.Parse
		// rejects must NOT return the raw string (that would leak the
		// secret). Instead the fixed placeholder is returned.
		// url.Parse rejects control characters in the host, invalid
		// percent escapes, and unclosed IPv6 brackets — real inputs an
		// attacker or bug could produce.
		{"control char in host + creds returns placeholder",
			"https://user:secret@repo\x00.example/x", unparseableURLPlaceholder},
		{"unclosed ipv6 bracket + creds returns placeholder",
			"https://user:secret@[::1/x", unparseableURLPlaceholder},
		{"invalid percent escape + creds returns placeholder",
			"https://user:secret@repo.example/%GH", unparseableURLPlaceholder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactURL(tt.in)
			if got != tt.want {
				t.Errorf("redactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// Belt-and-braces: the literal string "secret" must never
			// survive the redactor for ANY input in this table.
			if strings.Contains(got, "secret") {
				t.Errorf("password leaked through redactor: got %q", got)
			}
		})
	}
}

// TestVendorErrorPathsRedactCredentials drives real error paths with a
// URL that carries a distinctive secret and asserts the secret never
// appears in the error surfaced to the caller. Rather than enumerate
// every call site (which drifts as code evolves), this test exercises
// the observable behavior at the boundary that matters.
func TestVendorErrorPathsRedactCredentials(t *testing.T) {
	const secret = "SECRET_TOKEN_XYZ_9K3P"
	repoWithCreds := "https://user:" + secret + "@charts.example/repo"

	tests := []struct {
		name string
		run  func(t *testing.T) error
	}{
		{
			name: "checkEgressPolicy no-host path",
			run: func(_ *testing.T) error {
				// Userinfo-shaped URL that parses but has an empty host —
				// exercises the "no host" error path with credentials that
				// URL.Redacted must mask.
				return checkEgressPolicy(context.Background(), "https://user:"+secret+"@/path")
			},
		},
		{
			name: "validateForPull IsOCI scheme mismatch",
			run: func(_ *testing.T) error {
				return validateForPull(Component{
					Name: "x", ChartName: "foo", Version: "1", IsOCI: true,
					Repository: repoWithCreds,
				})
			},
		},
		{
			name: "resolveAndValidateHTTPIndex missing entry error",
			// subT here is the subtest's *testing.T so withMockIndexFetcher's
			// t.Cleanup fires between iterations rather than at the parent's
			// exit — otherwise the second iteration installs while the first
			// stub is still active and the double-install t.Fatal fires.
			run: func(subT *testing.T) error {
				withMockIndexFetcher(subT, func(_ context.Context, _ string) ([]byte, error) {
					return []byte("apiVersion: v1\nentries: {}\n"), nil
				})
				return resolveAndValidateHTTPIndex(context.Background(), Component{
					Name: "x", ChartName: "foo", Version: "1.2.3",
					Repository: repoWithCreds,
				})
			},
		},
		{
			name: "resolveAndValidateHTTPIndex malformed YAML error",
			run: func(subT *testing.T) error {
				withMockIndexFetcher(subT, func(_ context.Context, _ string) ([]byte, error) {
					return []byte("this is not: valid: yaml: at all\n  - nope\n"), nil
				})
				return resolveAndValidateHTTPIndex(context.Background(), Component{
					Name: "x", ChartName: "foo", Version: "1.2.3",
					Repository: repoWithCreds,
				})
			},
		},
		{
			name: "classifyHelmCLIError chart not found",
			run: func(_ *testing.T) error {
				return classifyHelmCLIError(
					Component{Name: "x", ChartName: "foo", Version: "1", Repository: repoWithCreds},
					stderrors.New("exit status 1"),
					"Error: 404 chart not found",
				)
			},
		},
		{
			// Parse-failure path: net/url.Parse rejects the input AND
			// includes the raw URL in its own error message. Without the
			// belt-and-braces "drop the cause" defense, StructuredError
			// would re-emit the secret via %v on the cause chain even
			// though redactURL swapped the visible instance with a
			// placeholder.
			name: "checkEgressPolicy parse-fail path (control char in host)",
			run: func(_ *testing.T) error {
				return checkEgressPolicy(context.Background(),
					"https://user:"+secret+"@repo\x00.example/x")
			},
		},
		{
			name: "validateForPull parse-fail path",
			run: func(_ *testing.T) error {
				return validateForPull(Component{
					Name: "x", ChartName: "foo", Version: "1",
					Repository: "https://user:" + secret + "@repo\x00.example/x",
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(t)
			if err == nil {
				t.Fatal("expected an error to inspect for credential leak")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("caller-supplied credential leaked into error message: %v", err)
			}
		})
	}
}

// TestFetchIndexYAMLRetry verifies the retry policy for transient failures.
// Transient errors (transport failures, 5xx, 408, 429) retry up to the budget;
// permanent errors (404, 401/403, other 4xx) fail on first attempt; policy
// rejections are never retried.
func TestFetchIndexYAMLRetry(t *testing.T) {
	tests := []struct {
		name        string
		errorType   string
		statusCode  int
		succeedAt   int
		wantErr     bool
		wantCode    errors.ErrorCode
		wantAttempt int
	}{
		{
			name:        "transport error retries and succeeds",
			errorType:   "transport",
			succeedAt:   2,
			wantErr:     false,
			wantAttempt: 2,
		},
		{
			name:        "503 retries and succeeds",
			errorType:   "status",
			statusCode:  http.StatusServiceUnavailable,
			succeedAt:   2,
			wantErr:     false,
			wantAttempt: 2,
		},
		{
			name:        "429 retries and succeeds",
			errorType:   "status",
			statusCode:  http.StatusTooManyRequests,
			succeedAt:   2,
			wantErr:     false,
			wantAttempt: 2,
		},
		{
			name:        "408 retries and succeeds",
			errorType:   "status",
			statusCode:  http.StatusRequestTimeout,
			succeedAt:   2,
			wantErr:     false,
			wantAttempt: 2,
		},
		{
			name:        "404 never retried",
			errorType:   "status",
			statusCode:  http.StatusNotFound,
			succeedAt:   100,
			wantErr:     true,
			wantCode:    errors.ErrCodeNotFound,
			wantAttempt: 1,
		},
		{
			name:        "401 never retried",
			errorType:   "status",
			statusCode:  http.StatusUnauthorized,
			succeedAt:   100,
			wantErr:     true,
			wantCode:    errors.ErrCodeUnauthorized,
			wantAttempt: 1,
		},
		{
			name:        "403 never retried",
			errorType:   "status",
			statusCode:  http.StatusForbidden,
			succeedAt:   100,
			wantErr:     true,
			wantCode:    errors.ErrCodeUnauthorized,
			wantAttempt: 1,
		},
		{
			name:        "policy rejection never retried",
			errorType:   "policy",
			succeedAt:   100,
			wantErr:     true,
			wantCode:    errors.ErrCodeInvalidRequest,
			wantAttempt: 1,
		},
		{
			name:        "transient error exhausts budget",
			errorType:   "transport",
			succeedAt:   100,
			wantErr:     true,
			wantCode:    errors.ErrCodeUnavailable,
			wantAttempt: defaults.HelmChartIndexRetryBudget,
		},
		{
			name:        "validation error (policy rejection) never retried",
			errorType:   "validation",
			succeedAt:   100,
			wantErr:     true,
			wantCode:    errors.ErrCodeInvalidRequest,
			wantAttempt: 0,
		},
		{
			name:        "transient validation error (DNS timeout) retries and succeeds",
			errorType:   "validation-transient",
			succeedAt:   2,
			wantErr:     false,
			wantAttempt: 1,
		},
		{
			name:        "transient validation error exhausts budget",
			errorType:   "validation-transient",
			succeedAt:   100,
			wantErr:     true,
			wantCode:    errors.ErrCodeUnavailable,
			wantAttempt: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := 0
			// Mock the per-attempt fetch function to control success/failure
			origAttempt := fetchIndexYAMLAttempt
			t.Cleanup(func() { fetchIndexYAMLAttempt = origAttempt })

			// Stub egress check to avoid DNS resolution
			origCheckTarget := checkFetchTargetURL
			t.Cleanup(func() { checkFetchTargetURL = origCheckTarget })
			validationAttempt := 0
			checkFetchTargetURL = func(ctx context.Context, url string) error {
				validationAttempt++
				switch tt.errorType {
				case "validation":
					if validationAttempt < tt.succeedAt {
						return errors.New(errors.ErrCodeInvalidRequest,
							"egress policy rejected target URL")
					}
				case "validation-transient":
					if validationAttempt < tt.succeedAt {
						return errors.PropagateOrWrap(
							stderrors.New("DNS timeout during address resolution"),
							errors.ErrCodeUnavailable,
							"transient validation failure")
					}
				}
				return nil
			}

			// Stub timer to avoid production sleep times
			origTimer := newBackoffTimer
			t.Cleanup(func() { newBackoffTimer = origTimer })
			newBackoffTimer = func(d time.Duration) *time.Timer {
				// Return a timer that fires immediately
				return time.NewTimer(1 * time.Nanosecond)
			}

			fetchIndexYAMLAttempt = func(ctx context.Context, indexURL string) ([]byte, error) {
				attempt++
				if attempt < tt.succeedAt {
					switch tt.errorType {
					case "transport":
						return nil, errors.PropagateOrWrap(
							stderrors.New("read: connection reset by peer"),
							errors.ErrCodeUnavailable,
							"index fetch failed")
					case "status":
						// Simulate what doFetchIndexYAMLAttempt does: parse HTTP status
						code := errors.ErrCodeUnavailable
						switch tt.statusCode {
						case http.StatusNotFound:
							code = errors.ErrCodeNotFound
						case http.StatusUnauthorized, http.StatusForbidden:
							code = errors.ErrCodeUnauthorized
						}
						return nil, errors.New(code,
							fmt.Sprintf("vendor-charts: index pre-check GET https://example.com/index.yaml returned HTTP %d", tt.statusCode))
					case "policy":
						return nil, errors.New(errors.ErrCodeInvalidRequest,
							"egress policy rejected redirect target")
					}
				}
				return []byte("index-yaml"), nil
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, err := defaultFetchIndexYAML(ctx, "https://example.com/index.yaml")

			if (err != nil) != tt.wantErr {
				t.Errorf("wantErr=%v, got err=%v", tt.wantErr, err)
			}
			if tt.wantErr && err != nil {
				if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
					t.Errorf("wantCode=%s, got error %v", tt.wantCode, err)
				}
			}
			if attempt != tt.wantAttempt {
				t.Errorf("wantAttempt=%d, got attempt=%d", tt.wantAttempt, attempt)
			}
		})
	}
}

// TestFetchIndexYAMLRetryEndToEnd drives defaultFetchIndexYAML against a real
// httptest.Server so the production HTTP-status classifier in
// doFetchIndexYAMLAttempt is exercised through the retry wrapper, rather than
// the hand-rolled classifier the table test stubs in.
func TestFetchIndexYAMLRetryEndToEnd(t *testing.T) {
	origCheck := checkFetchTargetURL
	checkFetchTargetURL = func(_ context.Context, _ string) error { return nil }
	t.Cleanup(func() { checkFetchTargetURL = origCheck })

	origTimer := newBackoffTimer
	t.Cleanup(func() { newBackoffTimer = origTimer })
	newBackoffTimer = func(time.Duration) *time.Timer {
		return time.NewTimer(1 * time.Nanosecond)
	}

	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits <= 2 {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "apiVersion: v1\nentries: {}\n")
	}))
	defer ts.Close()

	body, err := defaultFetchIndexYAML(context.Background(), ts.URL+"/index.yaml")
	if err != nil {
		t.Fatalf("fetch after 503 retries: %v", err)
	}
	if !strings.Contains(string(body), "apiVersion") {
		t.Errorf("body = %q, want to contain apiVersion", body)
	}
	if hits != 3 {
		t.Errorf("server hits = %d, want 3 (two 503s then 200)", hits)
	}
}

// TestFetchIndexYAMLContextCancellation verifies that context cancellation
// during backoff is honored and does not continue retrying.
func TestFetchIndexYAMLContextCancellation(t *testing.T) {
	t.Run("context canceled during backoff", func(t *testing.T) {
		attempt := 0
		origAttempt := fetchIndexYAMLAttempt
		t.Cleanup(func() { fetchIndexYAMLAttempt = origAttempt })

		// Stub egress check to avoid DNS resolution
		origCheckTarget := checkFetchTargetURL
		t.Cleanup(func() { checkFetchTargetURL = origCheckTarget })
		checkFetchTargetURL = func(ctx context.Context, url string) error { return nil }

		// Signal when backoff begins so we can cancel deterministically
		backoffStarted := make(chan struct{})
		origTimer := newBackoffTimer
		t.Cleanup(func() { newBackoffTimer = origTimer })
		newBackoffTimer = func(d time.Duration) *time.Timer {
			// Signal that backoff has begun, then return a long timer
			// so cancellation can interrupt it
			close(backoffStarted)
			return time.NewTimer(10 * time.Second)
		}

		fetchIndexYAMLAttempt = func(ctx context.Context, indexURL string) ([]byte, error) {
			attempt++
			if attempt == 1 {
				// Return transient error to trigger backoff
				return nil, errors.New(errors.ErrCodeUnavailable, "transient error")
			}
			// If we get here, context should have been canceled during backoff
			// and we should not have made a second attempt
			t.Error("unexpected second attempt after context cancellation")
			return nil, stderrors.New("should not reach here")
		}

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel after backoff begins to ensure cancellation happens during sleep
		go func() {
			<-backoffStarted // Wait for backoff to start
			cancel()
		}()

		_, err := defaultFetchIndexYAML(ctx, "https://example.com/index.yaml")
		if err == nil {
			t.Error("expected error from canceled context")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeCanceled, "")) {
			t.Errorf("expected ErrCodeCanceled, got error %v", err)
		}
		if !stderrors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("context should be canceled, got %v", ctx.Err())
		}
		if attempt != 1 {
			t.Errorf("expected exactly 1 attempt before cancellation, got %d", attempt)
		}
	})
}
