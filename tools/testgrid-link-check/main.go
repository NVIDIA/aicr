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

// Command testgrid-link-check is the warning-only, weekly bot (RQ2 / #1284)
// that verifies the Evidence deep-links the recipe-health matrix emits (RQ1 /
// #1283) still resolve against the live evidence dashboard.
//
// It reads the committed presence manifest (the set of coordinates RQ1 links),
// fetches the dashboard's published data (data/index.json) with a bounded HTTP
// client pinned to the dashboard origin (off-origin redirects refused), and
// reports any linked coordinate the live data no longer serves as a dead-link
// warning. A coordinate the dashboard serves but RQ1 has not linked yet is
// expected, not a warning. The report is Markdown for $GITHUB_STEP_SUMMARY.
//
// It is advisory: it never blocks a merge and never edits the doc, so it always
// exits 0 — a fetch failure or a dead link is reported, not fatal. Structurally
// a clone of .github/workflows/bom-refresh.yaml minus the create-pull-request
// step (report-only, contents: read).
//
// Usage: testgrid-link-check [-report-out <path>] [-index-file <path>]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/NVIDIA/aicr/pkg/corroborate"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/testgrid"
)

func main() {
	os.Exit(realMain())
}

// realMain runs the check and returns a process exit code. It returns non-zero
// only for an internal failure (a broken embedded manifest, an unwritable
// report path) — never for a dead link or a failed dashboard fetch, which are
// reported and exit 0 to preserve the warning-only contract.
func realMain() int {
	var (
		reportOut string
		indexFile string
	)
	flag.StringVar(&reportOut, "report-out", "", "path to write the Markdown report (default: stdout)")
	flag.StringVar(&indexFile, "index-file", "", "read live dashboard data from a local file instead of fetching (offline dry-run)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, reportOut, indexFile); err != nil {
		slog.Error("testgrid-link-check failed", "error", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, reportOut, indexFile string) error {
	presence, err := testgrid.LoadPresence()
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "load testgrid presence")
	}
	committed := presence.Paths()

	live, fetchErr := loadLive(ctx, indexFile)
	if fetchErr != nil {
		// A failed fetch is reported, not fatal: without live data the bot cannot
		// classify links, but it must not fail the (warning-only) job. Emit a note
		// and exit 0.
		slog.Warn("could not load live dashboard data; skipping link classification", "error", fetchErr)
		return writeReport(reportOut, func(w io.Writer) error {
			return renderFetchFailure(w, fetchErr)
		})
	}

	results := classify(committed, live)
	if n := warnCount(results); n > 0 {
		slog.Warn("dead Evidence links found (advisory)", "count", n)
	} else {
		slog.Info("all Evidence links resolve", "linked", len(committed))
	}
	return writeReport(reportOut, func(w io.Writer) error {
		return renderReport(w, results)
	})
}

// loadLive returns the set of coordinate paths the live dashboard serves,
// either from a local file (offline dry-run) or by fetching the pinned origin.
func loadLive(ctx context.Context, indexFile string) (map[string]struct{}, error) {
	var idx *corroborate.Index
	var err error
	if indexFile != "" {
		idx, err = readIndexFile(indexFile)
	} else {
		idx, err = fetchIndex(ctx, newPinnedClient(), testgrid.DataURL)
	}
	if err != nil {
		return nil, err
	}
	return testgrid.LivePaths(idx), nil
}

// readIndexFile decodes a dashboard index from a local file, bounding the read
// so an attacker-influenced path cannot OOM the process.
func readIndexFile(path string) (*corroborate.Index, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied dry-run path
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "open index file", err)
	}
	defer func() { _ = f.Close() }()
	return decodeIndex(io.LimitReader(f, defaults.HTTPResponseBodyLimit+1))
}

// fetchIndex fetches and decodes the dashboard index from dataURL using the
// supplied bounded, redirect-hardened client. The URL and client are injected
// so the pinned production origin is set by the sole caller (loadLive) and
// tests can drive the same path against a local server.
func fetchIndex(ctx context.Context, client *http.Client, dataURL string) (*corroborate.Index, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dataURL, nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "build index request", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "fetch dashboard index", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New(errors.ErrCodeUnavailable,
			fmt.Sprintf("dashboard index returned HTTP %d", resp.StatusCode))
	}
	return decodeIndex(io.LimitReader(resp.Body, defaults.HTTPResponseBodyLimit+1))
}

// decodeIndex reads a bounded body and decodes it as a dashboard index,
// rejecting a body that exceeds the limit rather than trusting a hostile size.
func decodeIndex(r io.Reader) (*corroborate.Index, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read dashboard index", err)
	}
	if int64(len(data)) > defaults.HTTPResponseBodyLimit {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "dashboard index exceeds size limit")
	}
	var idx corroborate.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "decode dashboard index", err)
	}
	return &idx, nil
}

// newPinnedClient returns an HTTP client bounded by the repo's HTTP timeout and
// hardened against SSRF/redirect steering: it refuses any redirect that leaves
// the pinned dashboard origin — a different host or a scheme downgrade to plain
// HTTP — so even if a link target ever became influenced by recipe/overlay
// content the weekly bot cannot be walked off-origin or downgraded to plaintext.
func newPinnedClient() *http.Client {
	return &http.Client{
		Timeout: defaults.HTTPClientTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New(errors.ErrCodeInvalidRequest,
					"refusing scheme-downgrade redirect to "+req.URL.Scheme)
			}
			if req.URL.Host != originHost() {
				return errors.New(errors.ErrCodeInvalidRequest,
					"refusing off-origin redirect to "+req.URL.Host)
			}
			return nil
		},
	}
}

// originHost is the host component of the pinned dashboard origin. Parsing the
// shared constant keeps the allowlist in lockstep with the link scheme.
func originHost() string {
	u, err := url.Parse(testgrid.Origin)
	if err != nil {
		return ""
	}
	return u.Host
}

// writeReport renders the report to reportOut (stdout when empty). The file is
// opened for append, not truncate: the workflow points reportOut at
// $GITHUB_STEP_SUMMARY, whose contract is append (matching tools/health), so
// the bot never clobbers output another step may have written. A writable
// file's Close error is captured so a flush failure is not silently dropped.
func writeReport(reportOut string, render func(io.Writer) error) (err error) {
	if reportOut == "" {
		return render(os.Stdout)
	}
	f, openErr := os.OpenFile(reportOut, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec // operator-supplied output path
	if openErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "open report file", openErr)
	}
	defer func() {
		closeErr := f.Close()
		if err == nil && closeErr != nil {
			err = errors.Wrap(errors.ErrCodeInternal, "close report file", closeErr)
		}
	}()
	return render(f)
}

// renderFetchFailure emits a report noting the live data could not be loaded,
// so the (warning-only) run still leaves an audit trail and exits 0.
func renderFetchFailure(w io.Writer, cause error) error {
	sw := &stickyWriter{w: w}
	fmt.Fprintf(sw, "## TestGrid link check\n\n")
	fmt.Fprintf(sw, "⚠️ Could not load live dashboard data from [%s](%s): `%s`.\n\n",
		testgrid.Origin, testgrid.DataURL, cause)
	fmt.Fprintf(sw, "Link classification was skipped this run. This is advisory only — no merge is blocked.\n\n")
	if sw.err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "write fetch-failure report", sw.err)
	}
	return nil
}
