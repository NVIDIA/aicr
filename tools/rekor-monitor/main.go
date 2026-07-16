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

package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// defaultTimeout bounds a single monitor pass (TUF fetch, shard discovery,
// consistency proof, and identity scan). It is generous relative to the observed
// few-seconds runtime so a real scan never trips it, while still capping a
// stalled network request rather than hanging the CI job. Kept local so this
// standalone tool does not import the shared pkg/defaults for one constant.
const defaultTimeout = 15 * time.Minute

// options are the tool's inputs. The monitored identity is passed by the caller
// (the workflow) rather than hardcoded so the workflow stays the auditable
// source of truth for what is watched.
type options struct {
	// checkpointFile is the path to the persisted v2 checkpoint (the cursor).
	// Empty content / missing file means "first run": establish a baseline at
	// the current head and skip the identity scan.
	checkpointFile string
	// certSubject is a regex matched against the certificate SAN of each scanned
	// entry. Empty disables the identity scan (consistency-only).
	certSubject string
	// certIssuer is a regex matched against the certificate issuer. Only used
	// when certSubject is set.
	certIssuer string
	userAgent  string
	// timeout bounds the whole monitor pass so a stalled network request cannot
	// hang the job.
	timeout time.Duration
	// restoreZip, when set, is a GitHub-artifact zip to extract the prior
	// checkpoint from into checkpointFile before monitoring. A missing zip file
	// means "first run" (no prior artifact); a present-but-unusable zip is an
	// error, so we never silently reset the cursor and stop scanning identity.
	restoreZip string
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("rekor-monitor", flag.ContinueOnError)
	var opts options
	fs.StringVar(&opts.checkpointFile, "file", "checkpoint_v2.txt", "path to the persisted Rekor v2 checkpoint (the cursor)")
	fs.StringVar(&opts.certSubject, "cert-subject", "", "regex for the monitored certificate SAN; empty runs consistency-only")
	fs.StringVar(&opts.certIssuer, "cert-issuer", "", "regex for the monitored certificate issuer")
	fs.StringVar(&opts.userAgent, "user-agent", "aicr-rekor-v2-monitor", "User-Agent for requests to the log")
	fs.DurationVar(&opts.timeout, "timeout", defaultTimeout, "maximum duration for the whole monitor pass")
	fs.StringVar(&opts.restoreZip, "restore-zip", "", "path to a GitHub-artifact zip to extract the prior checkpoint from before monitoring (missing file = first run)")
	if err := fs.Parse(args); err != nil {
		return options{}, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse flags", err)
	}
	if opts.certSubject == "" && opts.certIssuer != "" {
		return options{}, errors.New(errors.ErrCodeInvalidRequest, "--cert-issuer requires --cert-subject")
	}
	if opts.timeout <= 0 {
		// A zero/negative deadline yields an already-expired context, which would
		// surface as an operational failure rather than a clear argument error.
		return options{}, errors.New(errors.ErrCodeInvalidRequest, "--timeout must be positive")
	}
	return opts, nil
}

// run performs one monitor pass: it wires up the checkpoint store and the
// (network-backed) monitor, then hands off to observe for the orchestration.
func run(ctx context.Context, opts options, w io.Writer) error {
	store := checkpointStore{path: opts.checkpointFile, restoreZip: opts.restoreZip}
	if err := store.restore(ctx); err != nil {
		return err
	}

	mon, err := newMonitor(ctx, opts)
	if err != nil {
		return err
	}
	defer mon.cleanup() // remove the temp Fulcio CA files

	return observe(ctx, mon, store, w)
}

// observe runs one consistency + identity pass and persists the cursor. It takes
// the monitor behind the monitorChecks interface so the orchestration (the
// baseline/scan/finding branching and checkpoint-advance ordering) is
// unit-testable without network access. It returns a non-nil error on a
// consistency break, a scan error, or an identity finding, so the caller exits
// non-zero and the workflow alerts.
func observe(ctx context.Context, mon monitorChecks, store checkpointStore, w io.Writer) error {
	prev, err := store.read()
	if err != nil {
		return err
	}

	// Consistency: prove append-only from prev to the current head. This anchors
	// the identity scan below (guarantees the window was not rewritten) and is
	// the standard tamper check; a failure returns before advancing the cursor.
	cur, err := mon.checkConsistency(ctx, prev)
	if err != nil {
		return err
	}

	out := outcome{prev: prev, cur: cur}
	switch {
	case prev != nil && prev.Origin != cur.Origin:
		// Shard rotation (yearly, e.g. log2025-1 -> log2026-1): prev and cur are
		// different logs, so a size-based window is meaningless and the vendored
		// IdentitySearch only reads the latest shard. Re-baseline on the new shard
		// and report the gap (see out.report) rather than silently skipping.
		out.rotated = true
	case mon.watchesIdentity():
		// Identity: scan only the window of entries added since the last
		// checkpoint. scanWindow returns exclusive-start bounds; the entries
		// actually scanned are (start, end], i.e. [start+1, end].
		if start, end, ok := scanWindow(prev, cur); ok {
			found, failed, scanErr := mon.scanIdentity(ctx, start, end)
			if scanErr != nil {
				// The window is unverified; return before advancing.
				return scanErr
			}
			out.scanned = true
			out.from, out.to = start+1, end // inclusive range actually covered
			out.found, out.failed = found, failed
		}
	}

	out.report(w)
	if out.hasFindings() {
		// Do NOT advance the cursor on a finding: it must be re-detected every
		// run (keeping the alert issue open) until a maintainer triages it.
		// Advancing would sweep past a possible compromise and let a later clean
		// window auto-close the alert without acknowledgement.
		return out.findingError()
	}

	// Clean pass: advance the cursor so the next run scans only newly-added
	// entries (consistency proof done, identity window clear).
	if err := store.write(prev, cur); err != nil {
		return err
	}
	return nil
}

// runFunc is the signature of the monitor pass; injectable so realMain's
// flag/timeout/exit-code handling is testable without the network-backed run.
type runFunc func(context.Context, options, io.Writer) error

func main() {
	os.Exit(realMain(os.Args[1:], run))
}

// realMain parses args, bounds the pass with a timeout, runs it, and returns the
// process exit code. Keeping os.Exit out of the body lets the deferred cancel()
// run (os.Exit would skip it); taking runFn makes the wrapper testable.
func realMain(args []string, runFn runFunc) int {
	opts, err := parseFlags(args)
	if err != nil {
		slog.Error("invalid arguments", "error", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	if err := runFn(ctx, opts, os.Stdout); err != nil {
		slog.Error("rekor v2 monitor detected an issue or failed", "error", err)
		return 1
	}
	slog.Info("rekor v2 monitor completed cleanly")
	return 0
}
