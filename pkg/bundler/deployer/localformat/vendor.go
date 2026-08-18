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

// Bundle-time chart pulling for --vendor-charts.
//
// Strategy is hidden behind ChartPuller so the rest of the bundler does
// not care HOW upstream chart bytes were obtained — only that we have a
// .tgz and a provenance record. Today we shell out to `helm pull`
// (CLIChartPuller) because pulling the in-process Helm SDK transitively
// vendors github.com/cyphar/filepath-securejoin (MPL-2.0), which is not
// on the AICR license allowlist (Makefile: license-check). When legal
// approves the SDK we add an SDKChartPuller alongside CLIChartPuller and
// flip the default; nothing else in this package changes.

package localformat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"sigs.k8s.io/yaml"
)

// safeChartNameRE bounds the recipe-supplied identifiers that flow into
// `helm pull` as positional argv tokens. The leading character must be
// alphanumeric (rejects `--insecure-skip-tls-verify`, `-flag=value`, and
// any other helm-flag-shaped value) and the remainder is restricted to
// the lowest-common-denominator chart-name alphabet (alphanumerics, dot,
// underscore, hyphen). This is the boundary defense for
// helm-flag-injection — see validateForPull.
var safeChartNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// safeChartVersionRE is the same shape as safeChartNameRE but also
// allows `+` so semver build metadata (e.g. `1.2.3+build.7`) is not
// rejected. Versions also reach `helm pull --version <v>` as a separate
// argv token, so the leading-char rule applies for the same reason.
var safeChartVersionRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*$`)

// VendorRecord captures one entry of the bundle-time audit log emitted
// when --vendor-charts is set. Together the fields let an operator
// reconstruct provenance for a vendored chart and run yank-list lookups
// after the fact.
type VendorRecord struct {
	// Name is the recipe component name (folder NNN-<name>).
	Name string `json:"name"`
	// Chart is the upstream chart name as declared in the recipe.
	Chart string `json:"chart"`
	// Version is the chart version pulled.
	Version string `json:"version"`
	// Repository is the resolved upstream URL (HTTP(S) or oci://).
	Repository string `json:"repository"`
	// SHA256 is the hex-encoded digest of the .tgz bytes pulled.
	SHA256 string `json:"sha256"`
	// TarballName is the on-disk filename written under charts/.
	TarballName string `json:"tarballName"`
	// PullerVersion identifies the puller implementation that produced
	// this record (e.g. "helm-cli v3.20.2"). Audit-only; not used by
	// downstream code.
	PullerVersion string `json:"pullerVersion,omitempty"`
}

// ChartPuller fetches an upstream Helm chart and returns the raw .tgz
// bytes plus a provenance record. The implementation choice — shelling
// out to the helm CLI vs the in-process Helm SDK — is hidden behind this
// interface so the bundle-time path is identical either way.
//
// Implementations MUST honor ctx cancellation, MUST NOT mutate c, and
// SHOULD return one of the structured error codes from pkg/errors so
// callers can branch on intent rather than substring matching.
type ChartPuller interface {
	Pull(ctx context.Context, c Component) (tgz []byte, rec VendorRecord, tarball string, err error)
}

// CLIChartPuller shells out to `helm pull` to fetch upstream chart
// bytes. Used today while legal review of the in-process Helm SDK is
// pending; will be supplemented by an SDKChartPuller when approved.
//
// Auth for private repositories:
//   - HTTP(S): the upstream helm CLI does NOT read
//     HELM_REPOSITORY_USERNAME / HELM_REPOSITORY_PASSWORD for
//     `helm pull --repo <url>` (verified against helm v4.2.3: the
//     subprocess sends no Authorization header on either the index or
//     tarball GETs). Private HTTP repositories therefore require an
//     out-of-band mechanism today — either a `helm repo add --username
//     --password` performed before the subprocess runs, or the SDK
//     puller when licensing clears. The vendor path's own index.yaml
//     pre-check consumes these env vars only when EnvHelmRepositoryHost
//     gates the attachment — see attachHelmBasicAuth.
//   - OCI: standard docker config (~/.docker/config.json or
//     $DOCKER_CONFIG), exactly like `helm pull oci://...`. This IS
//     honored by helm's OCI code path.
//
// HelmBin overrides the binary lookup; empty falls back to "helm" on $PATH.
type CLIChartPuller struct {
	HelmBin string
}

// compile-time check
var _ ChartPuller = (*CLIChartPuller)(nil)

// helmBinary returns the configured override or the default "helm".
func (p *CLIChartPuller) helmBinary() string {
	if p.HelmBin != "" {
		return p.HelmBin
	}
	return "helm"
}

// Pull invokes `helm pull` for c, reads the resulting .tgz from a
// temporary destination, computes its SHA256, and returns the bytes plus
// a VendorRecord. The temp directory is removed before returning even on
// error. ctx cancellation interrupts the helm subprocess via os.Interrupt
// (exec.CommandContext default).
func (p *CLIChartPuller) Pull(ctx context.Context, c Component) ([]byte, VendorRecord, string, error) {
	if err := validateForPull(c); err != nil {
		return nil, VendorRecord{}, "", err
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.HelmChartPullTimeout)
	defer cancel()

	// SSRF defense (two layers):
	//
	//   1. Reject the repository host if it resolves to loopback,
	//      link-local, private, multicast, unspecified, or a cloud-metadata
	//      IP before any network I/O.
	//   2. For HTTP(S) repos, fetch the repository index.yaml via a
	//      hardened HTTP client (bounded body, redirect-validated), extract
	//      the tarball URLs declared for the requested chart version, and
	//      run the same policy against each. Closes the "public index.yaml
	//      points at a private-network tarball" gap the naive c.Repository
	//      check leaves open.
	//
	// This is NOT complete SSRF closure. The following residuals remain and
	// are the operator's responsibility to close with a Kubernetes
	// NetworkPolicy or an egress firewall — see the deployment docs:
	//
	//   - DNS rebinding between the pre-check and helm's own re-resolution
	//   - helm's HTTP redirects during tarball fetch (subprocess is not
	//     intercepted)
	//   - OCI registry redirects and blob-GETs (protocol not intercepted)
	//   - Resolver divergence: selectChartURLs re-implements helm's semver
	//     constraint resolution. helm re-fetches the index and re-resolves
	//     independently at pull time, so the URL egress-validated here may
	//     differ from the one helm actually pulls if the index changes
	//     between calls or if the two resolvers pick differently under
	//     ambiguous inputs (defense-in-depth that can bit-rot). Note that
	//     safeChartVersionRE currently rejects `^`/`~`/`>=`/`<`/`*`/space
	//     before Version reaches Pull(), so only partial-pin forms like
	//     `1.2` or `1.2.x` actually take the constraint slow path today —
	//     the full-constraint divergence surface is regex-gated out of
	//     the production request path and only becomes reachable if the
	//     regex is loosened.
	//
	// A follow-up will move the tarball fetch in-process to close all four.
	if err := checkEgressPolicy(ctx, c.Repository); err != nil {
		slog.WarnContext(ctx, "vendor-charts: egress policy rejected repository URL",
			"component", c.Name, "repository", redactURL(c.Repository), "error", err)
		return nil, VendorRecord{}, "", err
	}
	if err := resolveAndValidateHTTPIndex(ctx, c); err != nil {
		return nil, VendorRecord{}, "", err
	}

	tmpDir, err := os.MkdirTemp("", "aicr-vendor-")
	if err != nil {
		return nil, VendorRecord{}, "", errors.Wrap(errors.ErrCodeInternal,
			"vendor-charts: create temp dir", err)
	}
	defer os.RemoveAll(tmpDir)

	chartName := c.ChartName
	if chartName == "" {
		chartName = c.Name
	}

	// Build argv. `helm pull` accepts:
	//   helm pull <chart> --repo <url> --version <ver> --destination <dir>     (HTTP(S))
	//   helm pull oci://<host>/<path>/<chart> --version <ver> --destination .. (OCI)
	args := []string{"pull"}
	if c.IsOCI {
		args = append(args, strings.TrimRight(c.Repository, "/")+"/"+chartName)
	} else {
		args = append(args, chartName, "--repo", c.Repository)
	}
	args = append(args, "--version", c.Version, "--destination", tmpDir)

	stderr := &bytes.Buffer{}
	cmd := exec.CommandContext(ctx, p.helmBinary(), args...) //nolint:gosec // helm binary path is config-controlled; chart args are validated upstream and passed as exec args (no shell expansion).
	cmd.Stderr = stderr
	cmd.Stdout = stderr // capture both streams in one place for error reporting

	if runErr := cmd.Run(); runErr != nil {
		return nil, VendorRecord{}, "", classifyHelmCLIError(c, runErr, stderr.String())
	}

	saved, readErr := readSingleTgz(tmpDir)
	if readErr != nil {
		return nil, VendorRecord{}, "", readErr
	}

	tgz, readErr := readBoundedTgz(saved, defaults.HelmChartArtifactLimit)
	if readErr != nil {
		return nil, VendorRecord{}, "", readErr
	}

	sum := sha256.Sum256(tgz)
	tarball := filepath.Base(saved)

	return tgz, VendorRecord{
		Name:          c.Name,
		Chart:         chartName,
		Version:       c.Version,
		Repository:    c.Repository,
		SHA256:        hex.EncodeToString(sum[:]),
		TarballName:   tarball,
		PullerVersion: p.detectVersion(ctx),
	}, tarball, nil
}

// detectVersion best-efforts capture of the helm CLI version for audit.
// Failure here is non-fatal — the field is informational and absence is
// preferable to failing the whole bundle build over a probe.
func (p *CLIChartPuller) detectVersion(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, p.helmBinary(), "version", "--short") //nolint:gosec // same justification as Pull above.
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return "helm-cli " + strings.TrimSpace(string(out))
}

// readBoundedTgz reads path with a hard upper bound. Uses Stat as a fast
// pre-check so the process never allocates for an over-cap artifact, then
// io.LimitReader as belt-and-braces against a Stat/Open race and against
// symlink chicanery inside our temp dir. limit is compared against the
// on-disk size — a caller-supplied cap prevents an untrusted URL from
// steering the vendor path at a multi-gigabyte tarball.
func readBoundedTgz(path string, limit int64) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("vendor-charts: stat pulled chart %s", path), err)
	}
	if fi.Size() > limit {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: pulled chart %s is %d bytes, exceeds limit of %d bytes",
				path, fi.Size(), limit))
	}
	f, err := os.Open(path) //nolint:gosec // path was discovered inside our own temp dir.
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("vendor-charts: open pulled chart %s", path), err)
	}
	defer f.Close() //nolint:errcheck // read-only file handle; close error is not actionable.
	// LimitReader is +1 so a race that grows the file between Stat and Open
	// is caught (we'd read exactly limit+1 bytes and reject) rather than
	// silently truncated.
	buf, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("vendor-charts: read pulled chart %s", path), err)
	}
	// This branch is intentional defense-in-depth for the Stat/Open TOCTOU
	// race and is not covered by TestReadBoundedTgz — every over-cap case
	// there is caught by the Stat pre-check before Open. Reaching this
	// branch requires a file to grow past `limit` between the Stat call
	// above and the ReadAll below (e.g., a concurrent writer on the same
	// tmpDir), which the helm-subprocess-produces-one-tgz contract does
	// not permit today. Keep the check anyway — if the temp-dir contract
	// ever weakens, the branch fails-closed instead of returning truncated
	// bytes.
	if int64(len(buf)) > limit {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: pulled chart %s exceeded limit of %d bytes during read",
				path, limit))
	}
	return buf, nil
}

// checkResolvedChartURL is the egress check applied to each tarball URL
// resolved from the index.yaml. Swappable at the package level so tests
// can capture the exact URL the resolver produced (needed to pin the
// trailing-slash / base-URL resolution shape) without depending on the
// full checkEgressPolicy path or the system resolver. Production calls
// checkEgressPolicy unchanged.
var checkResolvedChartURL = checkEgressPolicy

// checkFetchTargetURL is the egress check applied to every URL the
// index-fetch client would GET — the initial indexURL (fail-closed
// re-check in case the caller skipped it) and each HTTP redirect hop
// the client follows. Swappable at the package level so tests can
// exercise the hop-count cap and the fail-closed guard without also
// tripping the egress rejection on the (necessarily loopback)
// httptest.Server URL. Production calls checkEgressPolicy unchanged.
var checkFetchTargetURL = checkEgressPolicy

// lookupIP is the resolver used by checkEgressPolicy. Swappable at the
// package level so tests can drive resolution deterministically without
// touching the system resolver. Production callers get the standard
// resolver bound to the request context so cancellation propagates.
var lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, nil
}

// cloudMetadataIPs enumerates the well-known instance-metadata endpoints
// across the major clouds. These are link-local addresses (already covered
// by IsLinkLocalUnicast for IPv4 169.254.0.0/16) but listed explicitly so
// the intent is grep-visible and so future providers whose metadata IP
// falls outside the RFC-classified ranges have an obvious extension point.
var cloudMetadataIPs = []net.IP{
	net.IPv4(169, 254, 169, 254),   // AWS, Azure, GCP, DigitalOcean, OpenStack
	net.IPv4(100, 100, 100, 200),   // Alibaba Cloud
	net.ParseIP("fd00:ec2::254"),   // AWS IMDS over IPv6
	net.ParseIP("fe80::a9fe:a9fe"), // Some IPv6 metadata endpoints
}

// checkEgressPolicy rejects repository URLs whose host resolves to any
// address a bundle-generating server has no business fetching from:
// loopback, link-local, unspecified, multicast, RFC1918 / CGNAT / ULA
// private ranges, or a well-known cloud-metadata IP. Applies to http,
// https, and oci:// schemes uniformly.
//
// The check is a fail-safe pre-filter, not a complete SSRF mitigation.
// DNS rebinding between this resolution and `helm pull`'s re-resolution
// remains an open surface until the fetch is moved in-process and the
// resolved IP is pinned into the request. Operators exposing aicrd
// beyond a trusted network should also gate the vendor path behind an
// explicit opt-in (see server config: AllowVendorCharts).
func checkEgressPolicy(ctx context.Context, repoURL string) error {
	u, err := url.Parse(repoURL)
	if err != nil {
		// Do NOT wrap err here: net/url.Parse embeds the raw URL in its
		// error message, so wrapping would re-emit the caller-supplied
		// URL (potentially with credentials) via StructuredError.Cause
		// even though redactURL already replaced the visible instance
		// with a placeholder. The parse error's detail is not otherwise
		// informative — "invalid URL syntax" is what the caller needs.
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: cannot parse repository URL %q", redactURL(repoURL)))
	}
	host := u.Hostname()
	if host == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: repository URL %q has no host", redactURL(repoURL)))
	}

	// A literal IP in the URL bypasses DNS entirely — check it directly
	// so an attacker cannot skip the resolver hook by supplying an IP
	// (and so tests do not need to stub resolution for the direct-IP
	// matrix).
	if ip := net.ParseIP(host); ip != nil {
		if reason := disallowedIPReason(ip); reason != "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("vendor-charts: repository host %q resolves to disallowed address %s (%s)",
					host, ip, reason))
		}
		return nil
	}

	ips, err := lookupIP(ctx, host)
	if err != nil {
		// Resolution failure is not a security decision — surface as
		// Unavailable so operators can distinguish DNS outage from
		// policy rejection. helm would fail the same way downstream.
		return errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("vendor-charts: cannot resolve repository host %q", host), err)
	}
	if len(ips) == 0 {
		return errors.New(errors.ErrCodeUnavailable,
			fmt.Sprintf("vendor-charts: repository host %q resolved to no addresses", host))
	}
	// Fail on ANY disallowed resolution — a hostname with a public A and a
	// private A (DNS split-horizon leak, misconfigured internal record) must
	// not be usable to smuggle a request into the private range.
	for _, ip := range ips {
		if reason := disallowedIPReason(ip); reason != "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("vendor-charts: repository host %q resolves to disallowed address %s (%s)",
					host, ip, reason))
		}
	}
	return nil
}

// unparseableURLPlaceholder is the fixed marker redactURL returns when
// url.Parse rejects the input. Returning the raw string in that case
// would leak credentials whenever the input embeds userinfo AND fails
// to parse (control chars, invalid escapes, unclosed IPv6 brackets,
// etc.) — all of which Go's url.Parse rejects while helm CLI's own
// error messages sometimes carry the raw URL. The placeholder favors
// safety over diagnostic value; operators wanting the raw string can
// consult the request-body debug log path.
const unparseableURLPlaceholder = "[redacted: unparseable url]"

// redactURL masks userinfo (password) in caller-supplied URLs before they
// reach any log line or client-visible error message. A caller could
// submit `Repository: "https://user:secret@repo.example/charts"` via
// POST /v1/bundle; without this shim the secret would appear verbatim
// in stderr logs and in the JSON error body returned to the client.
//
// url.URL.Redacted returns the URL with the password replaced by
// "xxxxx" — the standard net/url masking. On parse failure the input is
// replaced by unparseableURLPlaceholder rather than returned raw so an
// invalid-URL error path cannot leak credentials that happened to
// survive into the input. Callers still pass the redacted value through
// %s / %q as usual.
func redactURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return unparseableURLPlaceholder
	}
	return u.Redacted()
}

// cgnat100 is RFC6598's Carrier-Grade NAT range (100.64.0.0/10). Go's
// net.IP.IsPrivate() only covers RFC1918 + RFC4193 ULA; CGNAT is neither
// but is equally unroutable and equally unsafe as an egress target.
var cgnat100 = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// martianIPv4Ranges enumerates reserved IPv4 blocks Go's stdlib
// classifiers do not cover but which must not be reachable via the
// vendor path:
//   - 0.0.0.0/8      RFC 1122 "this network" — on Linux any 0.x.y.z
//     address in a URL routes to LOCALHOST via the
//     kernel's SIOCGIFADDR fallback, which would defeat
//     the loopback (127.0.0.0/8) protection without an
//     explicit CIDR check here. IsUnspecified only
//     catches the exact 0.0.0.0 address.
//   - 240.0.0.0/4    RFC 1112 class E reserved — unroutable on the
//     public Internet; egress to this range indicates
//     misconfiguration or probing.
//   - 255.255.255.255/32 RFC 919 limited broadcast — same posture.
var martianIPv4Ranges = []net.IPNet{
	{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(8, 32)},
	{IP: net.IPv4(240, 0, 0, 0), Mask: net.CIDRMask(4, 32)},
	{IP: net.IPv4(255, 255, 255, 255), Mask: net.CIDRMask(32, 32)},
}

// nat64Prefix is the well-known RFC 6052 NAT64/DNS64 well-known prefix
// (64:ff9b::/96). Any IPv6 address in this range is a NAT64-translated
// IPv4 packed into the low 32 bits, so a global-unicast IPv6 like
// `64:ff9b::a9fe:a9fe` is really 169.254.169.254 (AWS metadata) once
// the gateway forwards it. Go's stdlib does not decode this — the
// address looks like ordinary global unicast and passes every
// IsLoopback / IsPrivate / IsLinkLocal check.
var nat64Prefix = net.IPNet{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)}

// sixToFourPrefix is the RFC 3056 6to4 automatic-tunneling prefix
// (2002::/16). Bytes 2-5 of the address encode the embedded IPv4;
// e.g. `2002:0a00:0001::` is 10.0.0.1. Same bypass shape as NAT64:
// global-unicast IPv6 that decodes to a disallowed IPv4.
var sixToFourPrefix = net.IPNet{IP: net.ParseIP("2002::"), Mask: net.CIDRMask(16, 128)}

// decodeIPv6TransitionV4 extracts the embedded IPv4 address from a
// NAT64 (64:ff9b::/96) or 6to4 (2002::/16) IPv6 address. Returns nil
// for any other input, including nil, IPv4, or non-transition IPv6.
// Called from disallowedIPReason to catch the "IPv6 that decodes to a
// disallowed IPv4" bypass class.
func decodeIPv6TransitionV4(ip net.IP) net.IP {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return nil
	}
	if nat64Prefix.Contains(v6) {
		// Low 32 bits carry the v4.
		return net.IPv4(v6[12], v6[13], v6[14], v6[15]).To4()
	}
	if sixToFourPrefix.Contains(v6) {
		// Bytes 2-5 carry the v4.
		return net.IPv4(v6[2], v6[3], v6[4], v6[5]).To4()
	}
	return nil
}

// disallowedIPReason returns a non-empty reason string when ip is in a
// range aicrd must not initiate egress to from a request-driven code path.
// The empty string means "allowed."
//
// Cloud-metadata IPs are checked FIRST — 169.254.169.254 also matches
// IsLinkLocalUnicast, but the metadata-specific reason is what an
// operator needs to see in logs to recognize a credential-theft attempt.
func disallowedIPReason(ip net.IP) string {
	if ip == nil {
		return "nil address"
	}
	for _, meta := range cloudMetadataIPs {
		if meta != nil && ip.Equal(meta) {
			return "cloud-metadata"
		}
	}
	switch {
	case ip.IsLoopback():
		return "loopback"
	case ip.IsLinkLocalUnicast():
		return "link-local"
	case ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "multicast"
	case ip.IsUnspecified():
		return "unspecified"
	case ip.IsPrivate():
		// Covers RFC1918 (10/8, 172.16/12, 192.168/16) and RFC4193 ULA
		// (fc00::/7). RFC6598 CGNAT is handled below because
		// net.IP.IsPrivate does not include it.
		return "private"
	case cgnat100.Contains(ip):
		return "private"
	}
	// Martian IPv4 (0.0.0.0/8, 240.0.0.0/4, 255.255.255.255) — the
	// stdlib classifiers do not treat these as private/loopback but
	// egress to any of them is either unroutable or (in the 0.0.0.0/8
	// case on Linux) an alias for localhost that would sidestep the
	// loopback check above. Only matches IPv4 forms via To4().
	if v4 := ip.To4(); v4 != nil {
		for i := range martianIPv4Ranges {
			if martianIPv4Ranges[i].Contains(v4) {
				return "martian"
			}
		}
	}
	// IPv6 transition addresses (NAT64 64:ff9b::/96, 6to4 2002::/16)
	// encode an IPv4 in the low bits. Decode and recurse so a NAT64
	// gateway forwarding `64:ff9b::a9fe:a9fe` (= 169.254.169.254) is
	// rejected as cloud-metadata, not silently allowed as global-
	// unicast IPv6. The recursion depth is bounded by construction:
	// the decoded address is an IPv4 that cannot re-decode.
	if v4 := decodeIPv6TransitionV4(ip); v4 != nil {
		if reason := disallowedIPReason(v4); reason != "" {
			return reason
		}
	}
	return ""
}

// fetchIndexYAML is swappable at the package level for tests. Production
// uses a hardened HTTP client (bounded body, redirect-validated) with
// helm's own basic-auth env vars if set. Tests inject canned bytes so the
// URL-validation logic can exercise realistic index.yaml payloads without
// depending on the developer's system resolver or network reachability.
var fetchIndexYAML = defaultFetchIndexYAML

// defaultFetchIndexYAML performs the production HTTP GET. Every redirect
// hop is passed back through checkEgressPolicy so a public repo cannot
// redirect the index-fetch itself into the private range. Response body
// is capped at defaults.HelmChartIndexBodyLimit.
func defaultFetchIndexYAML(ctx context.Context, indexURL string) ([]byte, error) {
	// Fail-closed egress check on the initial URL. Idempotent: safe to
	// re-run for URLs the caller already validated. Today the only
	// production caller (resolveAndValidateHTTPIndex) runs
	// checkEgressPolicy(c.Repository) upstream and derives indexURL from
	// the same host, so this call is a no-op in the happy path. It
	// exists so a future refactor that reorders Pull() — or a new caller
	// of defaultFetchIndexYAML that skips the upstream check — cannot
	// silently open an SSRF hole here.
	if err := checkFetchTargetURL(ctx, indexURL); err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: defaults.NewHTTPTransport(),
		Timeout:   defaults.HelmChartIndexPreCheckTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= defaults.HelmChartIndexMaxRedirects {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("vendor-charts: index pre-check exceeded %d redirects",
						defaults.HelmChartIndexMaxRedirects))
			}
			if err := checkFetchTargetURL(req.Context(), req.URL.String()); err != nil {
				slog.WarnContext(req.Context(), "vendor-charts: egress policy rejected index redirect target",
					"redirect_target", redactURL(req.URL.String()),
					"hop", len(via), "error", err)
				return err
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("vendor-charts: build index pre-check request for %s", redactURL(indexURL)), err)
	}
	// Basic auth is gated on operator opt-in via AICR_HELM_REPOSITORY_HOST.
	// Sending HELM_REPOSITORY_USERNAME/PASSWORD indiscriminately would leak
	// the operator's helm credentials to any caller-supplied repository
	// URL. Attach only when scheme is HTTPS AND request host equals the
	// configured allowed host. Go's http.Client strips Authorization on
	// cross-origin redirects, so this env-var gate is the initial-URL
	// control. See defaults.EnvHelmRepositoryHost.
	attachHelmBasicAuth(req)
	resp, err := client.Do(req)
	if err != nil {
		// A policy-rejected redirect surfaces here as a wrapped
		// StructuredError with InvalidRequest — preserve its code so
		// callers can distinguish "someone tried to smuggle us into a
		// private range" from "upstream network is flaky." Anything else
		// is a genuine transport/dial failure → Unavailable.
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeUnavailable,
			fmt.Sprintf("vendor-charts: index pre-check GET %s failed", redactURL(indexURL)))
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; close error is not actionable.
	if resp.StatusCode != http.StatusOK {
		// Split HTTP status into the closest structured code so
		// orchestrators branching on the returned code get the right
		// retryability signal:
		//   - 404             -> NotFound          (chart repo does not exist)
		//   - 401 / 403       -> Unauthorized      (caller must fix creds; retry same request won't help)
		//   - 408 / 429       -> Unavailable       (retryable — request timeout / rate-limited)
		//   - other 4xx       -> InvalidRequest    (caller-shaped problem)
		//   - 5xx / everything else -> Unavailable (transient upstream)
		code := errors.ErrCodeUnavailable
		switch {
		case resp.StatusCode == http.StatusNotFound:
			code = errors.ErrCodeNotFound
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			code = errors.ErrCodeUnauthorized
		case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
			code = errors.ErrCodeUnavailable
		case resp.StatusCode >= http.StatusBadRequest && resp.StatusCode < http.StatusInternalServerError:
			code = errors.ErrCodeInvalidRequest
		}
		return nil, errors.New(code,
			fmt.Sprintf("vendor-charts: index pre-check GET %s returned HTTP %d", redactURL(indexURL), resp.StatusCode))
	}
	limited := io.LimitReader(resp.Body, defaults.HelmChartIndexBodyLimit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("vendor-charts: read index pre-check body from %s", redactURL(indexURL)), err)
	}
	if int64(len(body)) > defaults.HelmChartIndexBodyLimit {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: index at %s exceeds %d-byte cap",
				redactURL(indexURL), defaults.HelmChartIndexBodyLimit))
	}
	return body, nil
}

// attachHelmBasicAuth sets EnvHelmRepositoryUsername /
// EnvHelmRepositoryPassword on req only when: (a) the operator has opted
// in by setting EnvHelmRepositoryHost, (b) the request scheme is HTTPS
// (never leak creds in cleartext), and (c) the request host equals the
// configured allowed host. Any mismatch suppresses the credentials
// silently — a WARN log would be too chatty on the redirect path where
// suppression is expected. Go's stdlib http.Client separately strips
// Authorization on cross-origin redirects; this function is the
// initial-URL gate that stops the leak at the source.
func attachHelmBasicAuth(req *http.Request) {
	user := os.Getenv(defaults.EnvHelmRepositoryUsername)
	if user == "" {
		return
	}
	allowed := os.Getenv(defaults.EnvHelmRepositoryHost)
	if allowed == "" {
		return
	}
	if req.URL.Scheme != "https" {
		return
	}
	if !strings.EqualFold(req.URL.Host, allowed) {
		return
	}
	req.SetBasicAuth(user, os.Getenv(defaults.EnvHelmRepositoryPassword))
}

// helmIndexEntry captures just the fields the index pre-check reads
// from each entry in a Helm repository index.yaml. Declared once and
// referenced from both helmIndex.Entries and the selectChartURLs
// signature so a JSON-tag edit cannot silently drift the two shapes
// apart (which used to be possible when Entries was an anonymous
// struct and helmIndexEntry was a hand-copied alias).
type helmIndexEntry struct {
	Version string   `json:"version"`
	URLs    []string `json:"urls"`
}

// helmIndex mirrors the subset of a Helm repository index.yaml this
// pre-check needs. Extra fields are ignored; sigs.k8s.io/yaml is lenient.
type helmIndex struct {
	Entries map[string][]helmIndexEntry `json:"entries"`
}

// resolveAndValidateHTTPIndex is the second SSRF layer for the HTTP(S)
// vendor path. It fetches the repository's index.yaml via fetchIndexYAML,
// finds the entry matching the requested chart+version, resolves any
// relative URLs against the repository, and runs checkEgressPolicy on
// each. Any disallowed URL fails the request as InvalidRequest.
//
// No-op for OCI components (c.IsOCI): OCI resolution goes through the
// OCI distribution protocol (manifest → blob GETs) which this pre-check
// does not intercept. The operational control for OCI is a Kubernetes
// NetworkPolicy or an egress firewall on the aicrd pod; see the
// deployment docs.
func resolveAndValidateHTTPIndex(ctx context.Context, c Component) error {
	if c.IsOCI || c.Repository == "" {
		return nil
	}
	chartName := c.ChartName
	if chartName == "" {
		chartName = c.Name
	}
	indexURL := strings.TrimRight(c.Repository, "/") + "/index.yaml"
	body, err := fetchIndexYAML(ctx, indexURL)
	if err != nil {
		return err
	}
	var idx helmIndex
	if unmarshalErr := yaml.Unmarshal(body, &idx); unmarshalErr != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: index at %s is not valid YAML", redactURL(indexURL)), unmarshalErr)
	}
	entries, ok := idx.Entries[chartName]
	if !ok {
		// A missing entry could be legitimate (chart name typo, misconfigured
		// repo) or an attacker probing. Return NotFound with the exact
		// coordinates so operators can distinguish it from the egress
		// rejection cases below.
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("vendor-charts: repo %s has no entry for chart %q",
				redactURL(c.Repository), chartName))
	}
	matched, resolvedVersion, err := selectChartURLs(entries, chartName, c.Version)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
		// A matched entry with an empty urls list is a malformed index —
		// helm would then fail with no tarball to fetch. Reject up-front
		// with the same NotFound class as "no matching entry" so the
		// operator distinguishes broken index from wrong version.
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("vendor-charts: chart %q@%q resolved to an index entry with no tarball URLs",
				chartName, resolvedVersion))
	}
	// Force a trailing slash on the repository URL so RFC 3986
	// ResolveReference treats it as a directory rather than dropping the
	// last path segment as a filename. Matches helm's own
	// repo.ResolveReferenceURL semantics — without it a relative index
	// URL under a multi-segment repo path like "https://host/charts"
	// resolves to "https://host/foo.tgz" (validating a URL helm never
	// fetches) instead of "https://host/charts/foo.tgz".
	base, err := url.Parse(strings.TrimSuffix(c.Repository, "/") + "/")
	if err != nil {
		// Drop the parse err from the cause chain: net/url.Parse embeds
		// the raw URL in its error, which would re-emit any credentials
		// caller-supplied Repository carries. See checkEgressPolicy.
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: cannot parse repository URL %q", redactURL(c.Repository)))
	}
	for _, raw := range matched {
		ref, parseErr := url.Parse(raw)
		if parseErr != nil {
			// Drop the parse err from the cause chain: net/url.Parse
			// embeds the raw URL (from the index entry) in its error,
			// which is caller-influenced when the repo is caller-supplied.
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("vendor-charts: chart %q@%q has malformed URL %q in index",
					chartName, resolvedVersion, redactURL(raw)))
		}
		resolved := base.ResolveReference(ref).String()
		if egressErr := checkResolvedChartURL(ctx, resolved); egressErr != nil {
			// Server-visible signal so an operator has evidence someone
			// tried to smuggle a private URL through vendor-charts.
			// Warn-level: a legitimate request never triggers this.
			slog.WarnContext(ctx, "vendor-charts: egress policy rejected chart tarball URL",
				"component", c.Name, "chart", chartName, "version", resolvedVersion,
				"repository", redactURL(c.Repository), "rejected_url", redactURL(resolved),
				"error", egressErr)
			// PropagateOrWrap preserves the inner code so a DNS-lookup
			// failure inside checkEgressPolicy stays as Unavailable, while
			// an actual policy rejection (already coded InvalidRequest)
			// stays as InvalidRequest. Wrap-with-InvalidRequest would
			// mis-classify transient upstream failure as a client error.
			return errors.PropagateOrWrap(egressErr, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("vendor-charts: chart %q@%q index at %s lists disallowed tarball URL %s",
					chartName, resolvedVersion, redactURL(indexURL), redactURL(resolved)))
		}
	}
	return nil
}

// anyExactMatch reports whether at least one entry's Version string
// literally equals spec. Used by selectChartURLs to decide whether the
// fast (exact-string / semver-equal aggregation) path should run for a
// non-strict-semver spec like "latest".
func anyExactMatch(entries []helmIndexEntry, spec string) bool {
	for _, e := range entries {
		if e.Version == spec {
			return true
		}
	}
	return false
}

// selectChartURLs picks the entries whose Version satisfies versionSpec
// and returns the UNION of their URL lists (so every candidate mirror
// is egress-validated by the caller). Preserves exact-string match
// resolution first for non-semver aliases like "latest". If versionSpec
// is not a fully-qualified pinned semver (e.g. "1.2" or a constraint
// like "^1.2" / "~1.2" / "1.2.x"), treats it as a Helm-compatible
// semver constraint and resolves to the highest matching entry version
// (matching helm's own `helm pull --version <constraint>` policy) plus
// every duplicate row for that same resolved version.
//
// Union over duplicate same-version entries is the F17 defense: an
// index with two rows for version X (public URL first, private second)
// would previously return only the first, letting helm's independent
// re-resolution potentially pick the second bypassing egress. Now every
// candidate URL runs through the egress check.
//
// Returns the aggregate URL list, the resolved concrete version string
// (equal to versionSpec for exact matches; the index entry's version
// for constraints — useful for downstream error messages), and an error.
// Errors carry a StructuredError code so callers can PropagateOrWrap.
func selectChartURLs(entries []helmIndexEntry, chartName, versionSpec string) ([]string, string, error) {
	// Fast path: versionSpec is either a fully-qualified pinned semver
	// (major.minor.patch, with optional prerelease/build) or a non-
	// semver alias like "latest". Aggregate URLs from EVERY entry that
	// either exact-string-matches OR semver-equals the spec, so
	// duplicate rows (F17) and v-prefix aliases don't hide a URL from
	// the egress check.
	//
	// Uses StrictNewVersion (not NewVersion) so partial specs like "1.2"
	// or "1" — which Masterminds NewVersion silently coerces to
	// "1.2.0" / "1.0.0" — fall through to the constraint slow path
	// instead. Helm treats `helm pull --version 1.2` as the constraint
	// ">=1.2.0 <1.3.0", so a caller passing "1.2" should get 1.2.<max>
	// resolved from the index, not NOT_FOUND on the missing "1.2.0"
	// exact entry.
	var pinnedTarget *semver.Version
	if v, verErr := semver.StrictNewVersion(strings.TrimPrefix(versionSpec, "v")); verErr == nil {
		pinnedTarget = v
	}
	if pinnedTarget != nil || anyExactMatch(entries, versionSpec) {
		var matchedURLs []string
		var matchedOriginal string
		anyMatched := false
		for _, e := range entries {
			eq := false
			if e.Version == versionSpec {
				eq = true
			} else if pinnedTarget != nil {
				if ev, evErr := semver.NewVersion(e.Version); evErr == nil && ev.Equal(pinnedTarget) {
					eq = true
				}
			}
			if !eq {
				continue
			}
			anyMatched = true
			matchedURLs = append(matchedURLs, e.URLs...)
			if matchedOriginal == "" {
				matchedOriginal = e.Version
			}
		}
		if anyMatched && len(matchedURLs) == 0 {
			// Entry matched but declared no urls — helm would fail on the
			// missing tarball. Distinct signal from "no matching entry".
			return nil, "", errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("vendor-charts: chart %q@%q has no tarball URLs in matched index entries",
					chartName, matchedOriginal))
		}
		if anyMatched {
			return matchedURLs, matchedOriginal, nil
		}
		if pinnedTarget != nil {
			return nil, "", errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("vendor-charts: chart %q has no index entry matching pinned version %q",
					chartName, versionSpec))
		}
	}
	// Slow path: versionSpec is a semver constraint. Pick the highest
	// entry version that satisfies it — same policy as helm's own
	// resolution. Entries whose Version does not parse as semver are
	// skipped (helm does the same for non-semver entries under a
	// constraint match). Aggregate URLs across every entry equal to the
	// resolved best version (again for F17 duplicate-entry defense).
	con, conErr := semver.NewConstraint(versionSpec)
	if conErr != nil {
		return nil, "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: chart %q version %q is neither a valid pinned semver nor a valid semver constraint",
				chartName, versionSpec))
	}
	var bestVer *semver.Version
	var bestOriginal string
	for _, e := range entries {
		ev, evErr := semver.NewVersion(e.Version)
		if evErr != nil {
			continue
		}
		if !con.Check(ev) {
			continue
		}
		if bestVer == nil || ev.GreaterThan(bestVer) {
			bestVer = ev
			bestOriginal = e.Version
		}
	}
	if bestVer == nil {
		return nil, "", errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("vendor-charts: chart %q has no index entry satisfying constraint %q",
				chartName, versionSpec))
	}
	// Second pass: gather URLs from every entry equal to bestVer.
	var bestURLs []string
	for _, e := range entries {
		ev, evErr := semver.NewVersion(e.Version)
		if evErr != nil {
			continue
		}
		if ev.Equal(bestVer) {
			bestURLs = append(bestURLs, e.URLs...)
		}
	}
	return bestURLs, bestOriginal, nil
}

// readSingleTgz returns the path of the single .tgz file inside dir.
// `helm pull` writes exactly one tarball per invocation; if we see zero
// or multiple something has gone unexpectedly wrong.
func readSingleTgz(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal,
			"vendor-charts: read pull output dir", err)
	}
	var found string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".tgz") {
			continue
		}
		if found != "" {
			return "", errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("vendor-charts: multiple .tgz files in pull dir (%q and %q)",
					found, e.Name()))
		}
		found = e.Name()
	}
	if found == "" {
		return "", errors.New(errors.ErrCodeInternal,
			"vendor-charts: helm pull produced no .tgz output")
	}
	return filepath.Join(dir, found), nil
}

// classifyHelmCLIError maps `helm pull` failures to AICR error codes.
// helm's stderr is unstructured but stable enough that substring
// matching covers the cases that matter (404s, auth, network, missing
// binary). When we migrate to the SDK these become typed-error checks.
func classifyHelmCLIError(c Component, runErr error, stderrText string) error {
	combined := strings.ToLower(stderrText + " " + runErr.Error())

	// Missing binary surfaces as exec.ErrNotFound from PATH lookup, or as
	// os.ErrNotExist when an absolute HelmBin override points at a path
	// that doesn't exist. Match those typed sentinels first; the substring
	// fallback covers exec.Run flavors that don't preserve the sentinel
	// (e.g. helm 2/3 wrapper scripts whose own exec failure surfaces as
	// "no such file or directory" inside stderr text).
	if stderrors.Is(runErr, exec.ErrNotFound) || stderrors.Is(runErr, os.ErrNotExist) ||
		strings.Contains(combined, "executable file not found") ||
		strings.Contains(combined, "no such file or directory") {

		return errors.Wrap(errors.ErrCodeUnavailable,
			"vendor-charts: helm CLI not found on PATH (install helm or unset --vendor-charts)",
			runErr)
	}

	switch {
	case strings.Contains(combined, "not found") ||
		strings.Contains(combined, "404") ||
		strings.Contains(combined, "no chart version") ||
		strings.Contains(combined, "no chart name"):
		return errors.Wrap(errors.ErrCodeNotFound,
			fmt.Sprintf("vendor-charts: chart %q version %q not found at %q: %s",
				c.ChartName, c.Version, redactURL(c.Repository), strings.TrimSpace(stderrText)),
			runErr)
	case strings.Contains(combined, "401") ||
		strings.Contains(combined, "403") ||
		strings.Contains(combined, "unauthorized") ||
		strings.Contains(combined, "forbidden"):
		return errors.Wrap(errors.ErrCodeUnauthorized,
			fmt.Sprintf("vendor-charts: authentication failed pulling %q from %q (see chart repository authentication in the deployment docs): %s",
				c.ChartName, redactURL(c.Repository), strings.TrimSpace(stderrText)),
			runErr)
	case strings.Contains(combined, "context deadline") ||
		strings.Contains(combined, "context canceled") ||
		strings.Contains(combined, "signal: killed"):
		return errors.Wrap(errors.ErrCodeTimeout,
			fmt.Sprintf("vendor-charts: pull timed out for %q from %q",
				c.ChartName, redactURL(c.Repository)),
			runErr)
	case strings.Contains(combined, "no such host") ||
		strings.Contains(combined, "connection refused") ||
		strings.Contains(combined, "dial tcp"):
		return errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("vendor-charts: cannot reach repository %q: %s",
				redactURL(c.Repository), strings.TrimSpace(stderrText)),
			runErr)
	default:
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("vendor-charts: helm pull %q@%q from %q failed: %s",
				c.ChartName, c.Version, redactURL(c.Repository), strings.TrimSpace(stderrText)),
			runErr)
	}
}

// validateForPull rejects component shapes the puller cannot handle
// before any subprocess work. Caller is responsible for routing
// non-Helm-typed components (Kustomize, manifest-only) past this.
func validateForPull(c Component) error {
	if c.Repository == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: component %q has no repository", c.Name))
	}
	if c.Version == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: component %q has no chart version", c.Name))
	}
	// Reject argv-flag-shaped or otherwise weird values for fields that
	// flow into `helm pull` as positional argv tokens. exec.CommandContext
	// is shell-free, so OS-level shell injection isn't reachable, but
	// `helm pull <chartName> --repo <url>` treats a leading `-` as a helm
	// flag — e.g. a `chartName: --insecure-skip-tls-verify` would weaken
	// TLS verification without ever appearing in repo state.
	//
	// We apply the full allowlist regex (not just a leading-`-` check) as
	// defense-in-depth: future changes in how the chart-name reaches helm
	// (env var, shell wrapper, etc.) could re-open the injection surface
	// for values that pass a narrower check. The OCI path is safe today
	// because the chart token is concatenated into a single URL, but the
	// rule is applied uniformly to keep the contract simple and the test
	// matrix small.
	chartName := c.ChartName
	if chartName == "" {
		chartName = c.Name
	}
	if !safeChartNameRE.MatchString(chartName) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: chart name %q for component %q must match %s",
				chartName, c.Name, safeChartNameRE.String()))
	}
	if !safeChartNameRE.MatchString(c.Name) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: component name %q must match %s",
				c.Name, safeChartNameRE.String()))
	}
	if !safeChartVersionRE.MatchString(c.Version) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: version %q for component %q must match %s",
				c.Version, c.Name, safeChartVersionRE.String()))
	}
	if c.IsOCI {
		// IsOCI is a recipe-declared flag; cross-check that the
		// repository URL actually carries the oci:// scheme to catch
		// recipes where the flag and URL drifted out of sync.
		if !strings.HasPrefix(c.Repository, "oci://") {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("vendor-charts: repository %q for %q is marked IsOCI but does not start with oci://",
					redactURL(c.Repository), c.Name))
		}
		return nil
	}
	// Sanity-check the URL so we fail fast on a typo'd repo. url.Parse
	// on its own is too permissive — it accepts bare strings as a
	// relative reference — so also require an http(s) scheme.
	u, perr := url.Parse(c.Repository)
	if perr != nil {
		// Drop the parse err from the cause chain: net/url.Parse embeds
		// the raw URL in its error, which would re-emit any credentials
		// the caller-supplied Repository carries. See checkEgressPolicy.
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: invalid repository URL %q for %q",
				redactURL(c.Repository), c.Name))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: repository URL %q for %q must use http or https scheme (got %q); use IsOCI for oci:// repos",
				redactURL(c.Repository), c.Name, u.Scheme))
	}
	return nil
}

// ShouldVendor reports whether c should be routed through the vendor
// path when --vendor-charts is on. Returns false (without error) for
// shapes that are already local after #662 (Kustomize, manifest-only)
// — callers fall through to the existing classify() path for those.
// Exported for deployers that build their own vendored folder layout
// (e.g., flux) and need the same predicate without duplicating it.
func ShouldVendor(c Component) bool {
	return shouldVendor(c)
}

// shouldVendor is the unexported implementation used by Write().
func shouldVendor(c Component) bool {
	if c.Repository == "" {
		return false
	}
	if c.Tag != "" || c.Path != "" {
		// Kustomize-typed: leave to the existing classify() path.
		return false
	}
	return true
}
