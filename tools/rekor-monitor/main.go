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
	stderrors "errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/transparency-dev/merkle/proof"
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
	// knownTagsFile, when set, is a file of newline-separated release tags that
	// legitimately signed under the monitored identity. Identity-scan matches
	// whose SAN tag is in this set are expected releases and are suppressed;
	// everything else still alerts. Empty disables suppression (all matches
	// alert). See #1887.
	knownTagsFile string
	// knownTags is the resolved allowlist (loaded from knownTagsFile in realMain,
	// before the retry loop). Nil/empty disables suppression.
	knownTags map[string]bool
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
	fs.StringVar(&opts.knownTagsFile, "known-tags-file", "", "path to a newline-separated file of known release tags; identity matches for these tags are suppressed as expected releases (empty disables suppression)")
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

// maxKnownTagsFileBytes bounds the known-tags file read. Even a decade of
// releases is a few KiB; this simply prevents a pathological or
// attacker-influenced path from ballooning memory.
const maxKnownTagsFileBytes = 1 << 20 // 1 MiB

// loadKnownTags reads a newline-separated file of release tags into a set. An
// empty path returns an empty (non-nil) set, disabling suppression. Blank lines
// and surrounding whitespace are ignored. A set path that cannot be read is an
// error: the workflow always writes the file, so a missing one is a real
// failure, not a silent "suppress nothing".
func loadKnownTags(path string) (map[string]bool, error) {
	known := map[string]bool{}
	if path == "" {
		return known, nil
	}
	f, err := os.Open(path) //nolint:gosec // path is a workflow-controlled argument
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open known-tags file", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxKnownTagsFileBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read known-tags file", err)
	}
	if int64(len(data)) > maxKnownTagsFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "known-tags file exceeds size limit")
	}
	for _, line := range strings.Split(string(data), "\n") {
		if tag := strings.TrimSpace(line); tag != "" {
			known[tag] = true
		}
	}
	return known, nil
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

	return observe(ctx, mon, store, opts.knownTags, w)
}

// observe runs one consistency + identity pass and persists the cursor. It takes
// the monitor behind the monitorChecks interface so the orchestration (the
// baseline/scan/finding branching and checkpoint-advance ordering) is
// unit-testable without network access. It returns a non-nil error on a
// consistency break, a scan error, or an identity finding, so the caller exits
// non-zero and the workflow alerts.
func observe(ctx context.Context, mon monitorChecks, store checkpointStore, knownTags map[string]bool, w io.Writer) error {
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
			found = filterKnownReleases(found, knownTags)
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

// classification labels a terminal monitor error by whether it is a genuine
// security signal (tamper or a positive finding) or an operational failure that
// a retry could clear. The workflow branches its alerting on this.
type classification string

const (
	classClean       classification = "clean"
	classTamper      classification = "tamper"
	classIdentity    classification = "identity"
	classOperational classification = "operational"
)

// classify maps a non-nil terminal error to its classification. Only the two
// unambiguous compromise signals are security-classified: a consistency break
// (tamper) and a positive finding via ErrCodeConflict (identity match or an
// entry that failed verification). Everything else (transport, setup, timeout,
// and even a malformed-but-not-mismatch consistency proof) is operational, so an
// infrastructure blip never pages maintainers. A real log rewrite manifests as
// proof.RootMismatchError, which stays reachable through the wrap chain via
// Unwrap.
func classify(err error) classification {
	var mismatch proof.RootMismatchError
	if stderrors.As(err, &mismatch) {
		return classTamper
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeConflict, "")) {
		return classIdentity
	}
	return classOperational
}

// runFunc is the signature of the monitor pass; injectable so realMain's
// flag/timeout/exit-code handling is testable without the network-backed run.
type runFunc func(context.Context, options, io.Writer) error

func main() {
	os.Exit(realMain(os.Args[1:], run, os.Stdout))
}

// realMain parses args, bounds the pass with a timeout, runs it (retrying
// operational failures via runWithRetry), and returns the process exit code:
// 0 clean, 1 security (tamper/identity), 3 operational, 2 bad args. It prints a
// machine-readable CLASSIFICATION=<value> line to w so the workflow can branch
// its alerting without re-deriving intent from a stack trace.
func realMain(args []string, runFn runFunc, w io.Writer) int {
	opts, err := parseFlags(args)
	if err != nil {
		slog.Error("invalid arguments", "error", err)
		return 2
	}
	tags, err := loadKnownTags(opts.knownTagsFile)
	if err != nil {
		slog.Error("invalid known-tags file", "error", err)
		return 2
	}
	opts.knownTags = tags
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	runErr := runWithRetry(ctx, opts, w, runFn)
	if runErr == nil {
		fmt.Fprintf(w, "CLASSIFICATION=%s\n", classClean)
		slog.Info("rekor v2 monitor completed cleanly")
		return 0
	}
	class := classify(runErr)
	fmt.Fprintf(w, "CLASSIFICATION=%s\n", class)
	slog.Error("rekor v2 monitor detected an issue or failed", "classification", class, "error", runErr)
	if class == classOperational {
		return 3
	}
	return 1
}

const (
	// maxPassAttempts bounds how many times a single monitor invocation retries
	// an operational (transient) failure before giving up. A security
	// classification is never retried — it is deterministic, and retrying only
	// delays the maintainer page.
	maxPassAttempts = 3
	// retryBackoff is the fixed delay between operational retries. Kept modest
	// relative to the pass timeout so three attempts cannot approach the
	// deadline; a stalled network call is bounded by the context, not this sleep.
	retryBackoff = 100 * time.Millisecond
)

// runWithRetry runs the monitor pass, retrying only operational failures up to
// maxPassAttempts. It is safe to re-run a pass because observe() advances the
// checkpoint cursor solely on a fully clean pass, so a failed attempt leaves no
// partial state. The last error is returned for the caller to classify.
func runWithRetry(ctx context.Context, opts options, w io.Writer, runFn runFunc) error {
	var lastErr error
	for attempt := 1; attempt <= maxPassAttempts; attempt++ {
		lastErr = runFn(ctx, opts, w)
		if lastErr == nil || classify(lastErr) != classOperational {
			return lastErr
		}
		if attempt == maxPassAttempts {
			break
		}
		slog.Warn("operational failure; retrying monitor pass",
			"attempt", attempt, "maxAttempts", maxPassAttempts, "error", lastErr)
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "monitor retry cancelled", ctx.Err())
		case <-time.After(retryBackoff):
		}
	}
	return lastErr
}
