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
	tlog "github.com/transparency-dev/formats/log"
	"github.com/transparency-dev/merkle/proof"
)

// defaultTimeout bounds a single monitor pass (TUF fetch, shard discovery,
// consistency proof, and identity scan). The steady-state hourly window scans in
// minutes, but the identity scan is linear in the window size, and the window
// can balloon when the checkpoint has not advanced for a while: a multi-hour
// Sigstore/TUF outage, or a finding that a maintainer has not yet triaged (the
// cursor deliberately holds until then). A window of several hundred thousand
// entries is realistic in those cases, so this is sized to scan that backlog in
// one pass rather than timing out every run and never catching up. It still caps
// a genuinely stalled request rather than hanging the CI job (the workflow's
// job-level timeout-minutes is the outer backstop). Kept local so this
// standalone tool does not import the shared pkg/defaults for one constant.
const defaultTimeout = 45 * time.Minute

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
	for line := range strings.SplitSeq(string(data), "\n") {
		if tag := strings.TrimSpace(line); tag != "" {
			known[tag] = true
		}
	}
	return known, nil
}

// run performs one monitor pass: it wires up the checkpoint store and the
// (network-backed) monitor, then hands off to observe for the orchestration.
func run(ctx context.Context, opts options, w io.Writer) error {
	// The checkpoint artifact is restored once in realMain before the retry loop,
	// so this store reads whatever is already on disk (the seed on the first
	// attempt, or the failed attempt's saved progress on a retry).
	store := checkpointStore{path: opts.checkpointFile, restoreZip: opts.restoreZip}

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

	// Shard rotation (yearly, e.g. log2025-1 -> log2026-1): prev and cur are
	// different logs, so a size-based window is meaningless and the vendored
	// IdentitySearch only reads the latest shard. Re-baseline on the new shard
	// (resetting scan progress) and report the gap rather than silently skipping.
	if prev != nil && cur != nil && prev.Origin != cur.Origin {
		out.rotated = true
		out.abandonedProgress = abandonedForReport(store)
		out.report(w)
		return advanceCheckpoint(store, prev, cur)
	}

	// scanWindow returns exclusive-start bounds; the entries scanned are
	// (start, end]. ok is false on the first run (baseline) or an empty window;
	// either way there is nothing to scan, so advance (baseline) and reset.
	start, end, ok := scanWindow(prev, cur)
	if !mon.watchesIdentity() || !ok {
		out.report(w)
		return advanceCheckpoint(store, prev, cur)
	}

	// Resume the identity scan from persisted progress within (start, end]. Read
	// it only here, past the rotation/baseline/consistency-only early returns: each
	// of those advances via writeProgress(0), which overwrites a corrupt companion,
	// so reading above would turn a self-healing state into a wedge. A large window
	// is scanned across several runs rather than re-scanned (and timed out) from
	// scratch every pass.
	progress, err := store.readProgress()
	if err != nil {
		return err
	}
	scanFrom := start
	if progress == end {
		// The whole window was already scanned by a prior run whose checkpoint
		// advance did not persist (interrupted between saving progress and
		// advancing). Advance now rather than rescanning the whole window (which,
		// for a large backlog, would just time out and re-wedge). Covered by
		// TestObserveResumeAtWindowEnd.
		out.report(w)
		// This run scans nothing, so out.report prints only the consistency line;
		// say explicitly that the identity window was already covered, so the log
		// is not mistaken for "identity scanning did not apply".
		fmt.Fprintf(w, "identity scan [%d, %d]: already covered by a prior run; advancing checkpoint\n", start+1, end)
		return advanceCheckpoint(store, prev, cur)
	}
	if progress > end {
		// Impossible on an append-only log with a same-shard window: progress is
		// only ever persisted as a scanned chunk-end within (start, end]. Refuse to
		// advance -- advancing would leave (end, progress] worth of entries
		// unscanned yet report clean. Fail closed rather than fail open on an
		// ambiguous state (CLAUDE.md anti-pattern).
		return errors.New(errors.ErrCodeInternal, "scan progress exceeds window end; refusing to advance")
	}
	if progress > start {
		scanFrom = progress
	}

	// Bounded, resumable scan of (scanFrom, end] in chunks. The primary bound is
	// the soft time budget (scanDeadline): scan whatever fits before the pass
	// deadline nears, then save progress and resume next run. runEnd is the outer
	// per-run safety ceiling. After each clean chunk the progress cursor is
	// persisted, so a mid-run failure, the time budget, or the per-run cap resumes
	// here instead of restarting.
	if scanChunkSize <= 0 {
		// Guard the loop invariant: a non-positive chunk size makes chunkEnd <=
		// reached, so the scan never advances and burns the whole pass deadline.
		// Not reachable in production (package var), but fail fast if a test misconfigures it.
		return errors.New(errors.ErrCodeInternal, "scanChunkSize must be positive")
	}
	scanDeadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		scanDeadline = scanDeadline.Add(-scanBudgetHeadroom)
	}
	runEnd := end
	if capped := scanFrom + maxScanEntriesPerRun; capped < runEnd {
		runEnd = capped
	}
	reached := scanFrom
	for reached < runEnd {
		if cerr := ctx.Err(); cerr != nil {
			return errors.Wrap(errors.ErrCodeTimeout, "identity scan canceled", cerr)
		}
		chunkEnd := min(reached+scanChunkSize, runEnd)
		found, failed, scanErr := mon.scanIdentity(ctx, reached, chunkEnd)
		if scanErr != nil {
			// If the pass deadline expired inside this chunk but earlier chunks
			// landed, treat it as a clean partial rather than an operational failure:
			// every completed chunk's progress is already persisted, and the
			// finalization below is local file IO that does not need a live context.
			// Requiring at least one landed chunk (reached > scanFrom) means a
			// genuinely hung first chunk still errors operationally.
			if stderrors.Is(scanErr, context.DeadlineExceeded) && reached > scanFrom {
				break
			}
			return scanErr // progress up to `reached` is already persisted
		}
		found = filterKnownReleases(found, knownTags)
		if len(found) > 0 || len(failed) > 0 {
			// Finding in this chunk: alert and hold. Do NOT persist this chunk's
			// progress, so it is re-detected every run until a maintainer triages
			// it (advancing would let a later clean window auto-close the alert
			// without acknowledgement).
			out.scanned = true
			out.from, out.to = reached+1, chunkEnd
			out.found, out.failed = found, failed
			out.report(w)
			return out.findingError()
		}
		reached = chunkEnd
		if perr := store.writeProgress(reached); perr != nil {
			return perr
		}
		// Soft time budget spent: stop cleanly with progress saved. Checked after
		// persisting the chunk so a run always makes at least one chunk of forward
		// progress, and the partial-pass branch below leaves the checkpoint at prev
		// to resume here next run.
		if hasDeadline && time.Now().After(scanDeadline) {
			break
		}
	}

	out.scanned = true
	out.from, out.to = scanFrom+1, reached
	out.caughtUp = reached >= end
	if !out.caughtUp {
		out.remaining = end - reached
	}
	out.report(w)

	if !out.caughtUp {
		// Partial pass: progress is persisted; leave the signed checkpoint at prev
		// so the next run resumes from where this one stopped. A finding always
		// halts before this point, so a partial pass never coexists with an open
		// finding alert. recordCatchUpProgress tracks convergence and returns a
		// degraded error if the catch-up has stalled.
		return recordCatchUpProgress(store, out.remaining)
	}
	// Caught up with no findings: advance the signed checkpoint and reset scan
	// progress so the next run scans only newly-added entries.
	return advanceCheckpoint(store, prev, cur)
}

// abandonedForReport reads how far a prior-shard scan reached, for the rotation
// report only. This is purely a human-readable number, so a corrupt companion
// must never fail the pass -- and must not block the re-baseline whose
// writeProgress(0) is exactly what overwrites the bad file. Returns -1 (unknown)
// on a read error rather than propagating it.
func abandonedForReport(store checkpointStore) int64 {
	abandoned, err := store.readProgress()
	if err != nil {
		slog.Warn("could not read scan-progress for the rotation report", "error", err)
		return -1
	}
	return abandoned
}

// recordCatchUpProgress persists this partial pass's convergence trend and
// returns a degraded error when the catch-up is not making adequate ground. Two
// independent triggers, both auto-recovering (the trend resets on any advance):
//   - stall: `remaining` failed to beat its best-ever low-water mark for
//     maxCatchUpStallRuns consecutive passes. Comparing against the watermark, not
//     the immediately-prior pass, means an oscillating divergence (down one pass,
//     up the next) cannot keep resetting the counter to evade detection.
//   - passes: the window has taken maxCatchUpTotalRuns partial passes without ever
//     catching up. This absolute bound trips a glacial-but-monotone catch-up that
//     inches `remaining` down forever without the stall count ever firing.
func recordCatchUpProgress(store checkpointStore, remaining int64) error {
	t, err := store.readScanTrend()
	if err != nil {
		return err
	}
	t.passes++
	if t.bestRemaining == 0 || remaining < t.bestRemaining {
		// First partial pass in this window, or a new low-water mark: genuine net
		// progress, so the stall count resets. (remaining is always > 0 here -- a
		// caught-up pass never reaches this function.)
		t.bestRemaining, t.stall = remaining, 0
	} else {
		t.stall++
	}
	if err := store.writeScanTrend(t); err != nil {
		return err
	}
	switch {
	case t.stall >= maxCatchUpStallRuns:
		return &catchUpStalledError{remaining: remaining,
			reason: fmt.Sprintf("no new low-water mark for %d consecutive passes", t.stall)}
	case t.passes >= maxCatchUpTotalRuns:
		return &catchUpStalledError{remaining: remaining,
			reason: fmt.Sprintf("still behind after %d partial passes", t.passes)}
	}
	return nil
}

// advanceCheckpoint resets the scan-progress cursor and then writes cur as the
// new signed checkpoint (the consistency anchor for the next run), since the new
// window starts fresh. Progress is reset FIRST on purpose: the two files are
// non-atomic and uploaded together, so if the second write fails the artifact
// pairs the OLD checkpoint with progress 0 -- a harmless full re-scan -- never a
// NEW checkpoint with stale old-window progress (which could skip entries, e.g.
// a shard rotation whose small new Size sits below a six-figure old-shard index).
// Safe because advanceCheckpoint is only reached once the window is fully scanned
// (or has nothing to scan), so resetting progress loses no unscanned coverage.
func advanceCheckpoint(store checkpointStore, prev, cur *tlog.Checkpoint) error {
	if err := store.writeProgress(0); err != nil {
		return err
	}
	// A fresh window has no convergence history; clear the trend so the next
	// catch-up starts its stall count from zero.
	if err := store.resetScanTrend(); err != nil {
		return err
	}
	return store.write(prev, cur)
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
	// classDegraded is a deterministic "monitor cannot keep up" signal (the
	// identity catch-up is not converging). Unlike classOperational it is NOT
	// retried within a pass -- re-running would just re-scan and reach the same
	// conclusion -- but it is still a non-security failure, so the workflow's
	// degraded-issue path (which fires on any non-tamper/non-identity failure)
	// opens a tracking issue after the usual consecutive-failure streak.
	classDegraded classification = "degraded"
)

// catchUpStalledError signals that the identity catch-up scan is not converging:
// `remaining` failed to decrease across maxCatchUpStallRuns consecutive partial
// passes, so the log is outpacing the scan. It is a distinct type so classify can
// map it to classDegraded (deterministic, not retried) rather than a retryable
// operational failure.
type catchUpStalledError struct {
	remaining int64
	reason    string
}

func (e *catchUpStalledError) Error() string {
	return fmt.Sprintf("identity catch-up not converging: %d entries remaining (%s)", e.remaining, e.reason)
}

// classify maps a non-nil terminal error to its classification. Only the two
// unambiguous compromise signals are security-classified: a consistency break
// (tamper) and a positive finding via ErrCodeConflict (identity match or an
// entry that failed verification). Everything else (transport, setup, timeout,
// and even a malformed-but-not-mismatch consistency proof) is operational, so an
// infrastructure blip never pages maintainers. A real log rewrite manifests as
// proof.RootMismatchError, which stays reachable through the wrap chain via
// Unwrap.
func classify(err error) classification {
	if _, ok := stderrors.AsType[proof.RootMismatchError](err); ok {
		return classTamper
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeConflict, "")) {
		return classIdentity
	}
	if _, ok := stderrors.AsType[*catchUpStalledError](err); ok {
		return classDegraded
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
// 0 clean, 1 security (tamper/identity), 3 operational or degraded, 2 bad args. It prints a
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

	// Restore the checkpoint (and its progress companion) ONCE, before the retry
	// loop. Doing it here rather than inside each run() attempt means an
	// operational retry resumes from the progress the failed attempt saved on
	// disk, instead of re-extracting the artifact and reverting to the archived
	// (older) progress -- which would re-scan already-covered entries.
	store := checkpointStore{path: opts.checkpointFile, restoreZip: opts.restoreZip}
	if err := store.restore(ctx); err != nil {
		return exitFor(w, err)
	}

	runErr := runWithRetry(ctx, opts, w, runFn)
	if runErr == nil {
		fmt.Fprintf(w, "CLASSIFICATION=%s\n", classClean)
		slog.Info("rekor v2 monitor completed cleanly")
		return 0
	}
	return exitFor(w, runErr)
}

// exitFor classifies a terminal error, prints the machine-readable
// CLASSIFICATION line, and maps it to the process exit code: 3 operational, 1
// security (tamper/identity).
func exitFor(w io.Writer, err error) int {
	class := classify(err)
	fmt.Fprintf(w, "CLASSIFICATION=%s\n", class)
	slog.Error("rekor v2 monitor detected an issue or failed", "classification", class, "error", err)
	// Operational and degraded are both non-security failures (exit 3); only the
	// two compromise signals (tamper/identity) return the security exit code 1.
	if class == classOperational || class == classDegraded {
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
// signed checkpoint solely on a fully clean pass; a failed attempt leaves only
// its saved chunk progress on disk, and (because restore happens once in realMain
// before this loop) the retry resumes from that progress rather than re-scanning
// from the artifact. The last error is returned for the caller to classify.
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
		// Do not retry if too little of the pass budget remains for a meaningful
		// attempt. With the soft scan budget already spent, a retry would scan a
		// single chunk, break, and report a clean partial pass -- masking this
		// operational failure and handing the stall detector a spurious "made
		// progress" reset. Report the failure instead.
		if d, ok := ctx.Deadline(); ok && time.Until(d) < scanBudgetHeadroom {
			slog.Warn("insufficient pass budget remaining to retry; reporting the failure",
				"attempt", attempt, "error", lastErr)
			return lastErr
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
