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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
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
// Auth flows through helm's own conventions:
//   - HTTP(S): HELM_REPOSITORY_USERNAME / HELM_REPOSITORY_PASSWORD env vars.
//   - OCI:     standard docker config (~/.docker/config.json or
//     $DOCKER_CONFIG), exactly like `helm pull oci://...`.
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

	tgz, readErr := os.ReadFile(saved) //nolint:gosec // saved was discovered inside our own temp dir.
	if readErr != nil {
		return nil, VendorRecord{}, "", errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("vendor-charts: read pulled chart %s", saved), readErr)
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
				c.ChartName, c.Version, c.Repository, strings.TrimSpace(stderrText)),
			runErr)
	case strings.Contains(combined, "401") ||
		strings.Contains(combined, "403") ||
		strings.Contains(combined, "unauthorized") ||
		strings.Contains(combined, "forbidden"):
		return errors.Wrap(errors.ErrCodeUnauthorized,
			fmt.Sprintf("vendor-charts: authentication failed pulling %q from %q (set HELM_REPOSITORY_USERNAME/HELM_REPOSITORY_PASSWORD or docker login for OCI): %s",
				c.ChartName, c.Repository, strings.TrimSpace(stderrText)),
			runErr)
	case strings.Contains(combined, "context deadline") ||
		strings.Contains(combined, "context canceled") ||
		strings.Contains(combined, "signal: killed"):
		return errors.Wrap(errors.ErrCodeTimeout,
			fmt.Sprintf("vendor-charts: pull timed out for %q from %q",
				c.ChartName, c.Repository),
			runErr)
	case strings.Contains(combined, "no such host") ||
		strings.Contains(combined, "connection refused") ||
		strings.Contains(combined, "dial tcp"):
		return errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("vendor-charts: cannot reach repository %q: %s",
				c.Repository, strings.TrimSpace(stderrText)),
			runErr)
	default:
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("vendor-charts: helm pull %q@%q from %q failed: %s",
				c.ChartName, c.Version, c.Repository, strings.TrimSpace(stderrText)),
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
					c.Repository, c.Name))
		}
		return nil
	}
	// Sanity-check the URL so we fail fast on a typo'd repo. url.Parse
	// on its own is too permissive — it accepts bare strings as a
	// relative reference — so also require an http(s) scheme.
	u, perr := url.Parse(c.Repository)
	if perr != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: invalid repository URL %q for %q",
				c.Repository, c.Name), perr)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("vendor-charts: repository URL %q for %q must use http or https scheme (got %q); use IsOCI for oci:// repos",
				c.Repository, c.Name, u.Scheme))
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
