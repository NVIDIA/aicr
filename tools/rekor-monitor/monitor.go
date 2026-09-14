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
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sigstore/rekor-monitor/pkg/identity"
	rekorv2 "github.com/sigstore/rekor-monitor/pkg/rekor/v2"
	"github.com/sigstore/rekor-monitor/pkg/tiles"
	rmutil "github.com/sigstore/rekor-monitor/pkg/util"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	tlog "github.com/transparency-dev/formats/log"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/trust"
)

// monitorChecks is the behavior observe() needs from a monitor. *monitor
// implements it (via network-backed methods); tests substitute a fake so the
// orchestration is covered without contacting TUF or Rekor.
type monitorChecks interface {
	watchesIdentity() bool
	checkConsistency(ctx context.Context, prev *tlog.Checkpoint) (*tlog.Checkpoint, error)
	scanIdentity(ctx context.Context, start, end int64) ([]identity.MonitoredIdentity, []identity.FailedLogEntry, error)
}

// monitor holds the resolved Rekor v2 shard set and the identity to watch for.
// It exposes the two checks a pass performs; wiring lives in newMonitor.
type monitor struct {
	shards      map[string]rekorv2.ShardInfo
	shardOrigin string
	identities  identity.MonitoredValues
	// Fulcio CA root/intermediate PEM files materialized from the trusted root.
	// Without them the vendored IdentitySearch skips certificate-chain
	// validation, so a self-signed cert bearing the monitored SAN/issuer would
	// register as a (false) match.
	caRoots         string
	caIntermediates string
	cleanup         func() // removes the temp CA files; safe to call once
}

// newMonitor resolves everything a pass needs: AICR's v2 signing config (the
// piece the upstream tool cannot see), the trusted root, the v2 shard set
// discovered from that config, and the monitored identity.
func newMonitor(ctx context.Context, opts options) (*monitor, error) {
	identities, err := buildMonitoredValues(opts.certSubject, opts.certIssuer)
	if err != nil {
		return nil, err
	}

	// The v2 signing config AICR signs against: the piece upstream cannot see.
	signingConfig, err := trust.ResolveSigningConfig(ctx)
	if err != nil {
		return nil, err // already a coded pkg/errors error
	}

	tufClient, err := tuf.New(tuf.DefaultOptions())
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to initialize TUF client", err)
	}
	trustedRoot, err := root.GetTrustedRoot(tufClient)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to fetch trusted root", err)
	}

	// GetRekorShards filters the services to v2, so passing the full list (which
	// also carries the legacy v1 entry) is fine and future-proof across rotation.
	shards, shardOrigin, err := rekorv2.GetRekorShards(ctx, trustedRoot, signingConfig.RekorLogURLs(), opts.userAgent, "")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to resolve Rekor v2 shards", err)
	}

	// Materialize the Fulcio CA roots so IdentitySearch actually validates the
	// certificate chain (empty paths make it skip chain validation).
	caRoots, caIntermediates, cleanup, err := rmutil.ConfigureTrustedCAs("", "", trustedRoot)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to materialize Fulcio CA roots", err)
	}

	return &monitor{
		shards:          shards,
		shardOrigin:     shardOrigin,
		identities:      identities,
		caRoots:         caRoots,
		caIntermediates: caIntermediates,
		cleanup:         cleanup,
	}, nil
}

// watchesIdentity reports whether an identity scan is configured (vs
// consistency-only).
func (m *monitor) watchesIdentity() bool { return len(m.identities) > 0 }

// checkConsistency proves the log is append-only from prev to the current head
// and returns the current checkpoint. A nil prev is the first run (baseline: no
// proof to run yet).
func (m *monitor) checkConsistency(ctx context.Context, prev *tlog.Checkpoint) (*tlog.Checkpoint, error) {
	cur, err := tiles.VerifyConsistencyWithCheckpoint(ctx, m.shards, m.shardOrigin, prev)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "consistency verification failed", err)
	}
	return cur, nil
}

// scanIdentity searches the [start, end] entry-index window for the monitored
// identity, returning matching entries and entries that failed to parse.
func (m *monitor) scanIdentity(ctx context.Context, start, end int64) ([]identity.MonitoredIdentity, []identity.FailedLogEntry, error) {
	found, failed, err := rekorv2.IdentitySearch(ctx, m.shards, m.shardOrigin, m.identities, start, end,
		identity.WithCARootsFile(m.caRoots),
		identity.WithCAIntermediatesFile(m.caIntermediates))
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, "identity search failed", err)
	}
	return found, failed, nil
}

// buildMonitoredValues converts the identity flags into rekor-monitor's
// MonitoredValues. It returns the zero value (no identities) when certSubject is
// empty, which the caller treats as consistency-only.
func buildMonitoredValues(certSubject, certIssuer string) (identity.MonitoredValues, error) {
	if certSubject == "" {
		// An issuer without a subject would silently run consistency-only while
		// looking configured, disabling the identity check. parseFlags already
		// rejects this at the CLI; guard here too so the function is safe in
		// isolation.
		if certIssuer != "" {
			return identity.MonitoredValues{}, errors.New(errors.ErrCodeInvalidRequest, "cert issuer set without cert subject")
		}
		return identity.MonitoredValues{}, nil
	}
	var issuers []string
	if certIssuer != "" {
		issuers = []string{certIssuer}
	}
	certID := identity.CertIdentityValue{CertSubject: certSubject, Issuers: issuers}
	if err := certID.Verify(); err != nil {
		return identity.MonitoredValues{}, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid monitored identity", err)
	}
	return identity.MonitoredValues{certID}, nil
}

// tagRefMarker separates the workflow-identity prefix of a release-signer SAN
// from the git tag it signed, e.g.
// ".../on-tag.yaml@refs/tags/v0.18.0-rc1" -> "v0.18.0-rc1".
const tagRefMarker = "@refs/tags/"

// extractTag returns the tag ref embedded in a release-signer certificate SAN.
// ok is false when the SAN has no @refs/tags/ segment OR the tag is empty — both
// unexpected shapes the caller must NOT suppress.
func extractTag(certSubject string) (tag string, ok bool) {
	_, after, ok := strings.Cut(certSubject, tagRefMarker)
	if !ok {
		return "", false
	}
	tag = after
	if tag == "" {
		return "", false // empty tag ref is an unexpected shape; do not suppress
	}
	return tag, true
}

// filterKnownReleases drops identity-scan matches that correspond to a known
// release tag, returning only the unexpected residual matches that warrant an
// alert. An entry whose tag is not in known — or whose SAN has no @refs/tags/
// segment — is kept (fail closed). A MonitoredIdentity left with no entries is
// omitted. An empty known set is a pass-through (suppression disabled), so a
// missing allowlist can never silently hide a real finding. See #1887.
func filterKnownReleases(found []identity.MonitoredIdentity, known map[string]bool) []identity.MonitoredIdentity {
	if len(known) == 0 {
		return found
	}
	var residual []identity.MonitoredIdentity
	for _, mi := range found {
		var kept []identity.LogEntry
		for _, e := range mi.FoundIdentityEntries {
			if tag, ok := extractTag(e.CertSubject); ok && known[tag] {
				continue // expected release signature
			}
			kept = append(kept, e)
		}
		if len(kept) > 0 {
			mi.FoundIdentityEntries = kept
			residual = append(residual, mi)
		}
	}
	return residual
}

// scanChunkSize, maxScanEntriesPerRun, and scanBudgetHeadroom bound the
// resumable identity scan. They are package vars (not consts) only so tests can
// shrink them (via withScanBounds); production never reassigns them. Because that
// mutation is process-global, tests in this package must not call t.Parallel() --
// with -race a parallel test would data-race on these.
var (
	// scanChunkSize bounds a single IdentitySearch call so one chunk cannot
	// approach the pass deadline, and gives the resumable scan a save point: the
	// persisted progress cursor advances one chunk at a time. It also caps the
	// overrun past the soft budget, since the budget is only checked between
	// chunks (one in-flight chunk may finish after the budget is spent).
	scanChunkSize int64 = 50_000
	// scanBudgetHeadroom is the PRIMARY bound: the scan stops once the pass
	// deadline is within this margin, saving progress to resume next run. This
	// makes catch-up adaptive to scan speed -- a slow run covers fewer entries,
	// a fast run more, and neither overruns the deadline. The margin only needs to
	// cover the one in-flight chunk running when the deadline trips (the budget is
	// checked between chunks): a completed catch-up run measured ~950k entries in
	// ~40min (~24k/min), so a 50k chunk is ~2min, call it ~3-4min on a slow-network
	// day. 5m is ~1.5x that worst case, and finalizing a partial pass is
	// near-instant (progress is already persisted per chunk; the checkpoint stays
	// at prev). If a trailing chunk still overruns the 45m pass deadline, ctx
	// cancellation degrades that run gracefully (operational classification,
	// retryable) and the next run resumes from saved progress -- it never surfaces
	// a false security finding.
	scanBudgetHeadroom = 5 * time.Minute
	// maxScanEntriesPerRun is the outer SAFETY ceiling on one run's scan, in case
	// an unexpectedly fast run (or a broken clock) would otherwise scan an
	// unbounded slice. On normal infra the soft budget above stops the scan first
	// (a completed run covered ~950k entries in the ~40min budget), so this rarely
	// binds; it exists so a single run can never run away. The log grows ~70k/hr,
	// well under a per-run pass, so catch-up converges. (Throughput is measured
	// from completed catch-up runs, not derived from the 2026-07-24 wedge, which
	// only bounds the window size, not completed work.)
	maxScanEntriesPerRun int64 = 2_000_000
	// maxCatchUpStallRuns is how many consecutive partial passes may fail to beat
	// the best-ever `remaining` (see scanTrend) before observe declares the
	// catch-up diverging and returns a degraded error. Combined with the workflow's
	// own consecutive-failure streak before it opens a degraded issue, this is
	// deliberately slow so a single transient slow pass never pages.
	maxCatchUpStallRuns = 3
	// maxCatchUpTotalRuns is an absolute bound: how many partial passes a single
	// window may take before observe declares it degraded, even if `remaining` is
	// still inching down (which keeps the stall count above at 0). It catches a
	// glacial catch-up the watermark never trips. Must exceed the passes a
	// legitimate large backlog needs -- the ~6.5M-entry 2026-07 wedge caught up in
	// well under ten passes at ~1M/pass -- so 72 (~3 days hourly) leaves ample room.
	maxCatchUpTotalRuns = 72
)

// scanWindow computes the [start, end] entry-index window to scan between the
// previous checkpoint and the current head. ok is false when there is no prior
// checkpoint (first run) or the window is empty/degenerate, so the caller only
// baselines.
func scanWindow(prev, cur *tlog.Checkpoint) (start, end int64, ok bool) {
	if prev == nil || cur == nil {
		return 0, 0, false
	}
	// cur.Size <= prev.Size: nothing new to scan. prev.Size == 0 is defensive:
	// WriteCheckpointRekorV2 skips size-0 writes and the checkpoint format will
	// not parse a size-0 entry, so a persisted prev always has Size >= 1; the
	// guard also keeps start (below) >= 0.
	if prev.Size == 0 || cur.Size <= prev.Size {
		return 0, 0, false
	}
	// IdentitySearch delegates to tiles.GetEntriesByIndexRange, which is
	// EXCLUSIVE of start: it scans (start, end]. Pass prev.Size-1 so the first
	// new entry (index prev.Size) is included, through the current last index.
	// prev.Size >= 1 is guaranteed above, so start >= 0. Passing prev.Size here
	// (as if start were inclusive) would skip index prev.Size and scan nothing
	// when exactly one entry was added.
	start = int64(prev.Size) - 1 //nolint:gosec // G115: log size never approaches int64 max
	end = int64(cur.Size) - 1    //nolint:gosec // G115: log size never approaches int64 max
	return start, end, true
}

// outcome is the result of one monitor pass. Keeping it separate from the
// monitoring mechanics lets run() decide reporting and exit status in one place.
type outcome struct {
	prev, cur *tlog.Checkpoint // prev is nil on the first run (baseline)
	rotated   bool             // prev and cur are different shards (yearly rotation)
	// abandonedProgress is the prior-shard scan cursor at a rotation: when > 0, an
	// in-progress catch-up is being abandoned across the rotation boundary and its
	// remaining entries will never be identity-scanned (reported loudly).
	abandonedProgress int64
	scanned           bool  // whether an identity scan ran this pass
	from, to          int64 // the inclusive range actually scanned (valid when scanned)
	found             []identity.MonitoredIdentity
	failed            []identity.FailedLogEntry
	// caughtUp reports whether this pass finished scanning the whole window up to
	// the current head. When false, the scan was bounded (soft budget / per-run
	// entry cap) and persisted its progress; a later run resumes and continues.
	caughtUp bool
	// remaining is the number of window entries still unscanned after this pass
	// (valid when scanned and !caughtUp), for the human-readable catch-up line.
	remaining int64
}

// hasFindings reports whether the scan surfaced anything alert-worthy: a matched
// identity or an entry that failed to parse.
func (o outcome) hasFindings() bool { return len(o.found) > 0 || len(o.failed) > 0 }

// findingError returns the error surfaced when the scan found something,
// distinguishing identity matches from entries that failed verification so the
// run log and alert issue are accurately labeled (the cursor has already
// advanced past these indices, so triage wording matters).
func (o outcome) findingError() error {
	return errors.New(errors.ErrCodeConflict,
		fmt.Sprintf("scanned window has %d identity match(es) and %d entr(y/ies) that failed verification",
			len(o.found), len(o.failed)))
}

// report writes a human-readable summary of the pass to w.
func (o outcome) report(w io.Writer) {
	switch {
	case o.prev == nil:
		fmt.Fprintf(w, "baseline established at tree size %d (first run; identity scan skipped)\n", o.cur.Size)
		return
	case o.rotated:
		// prev and cur are different logs; there is no meaningful cross-shard
		// window, so we re-baseline on the new shard and flag the gap loudly.
		fmt.Fprintf(w, "shard rotation detected: %q -> %q; re-baselining on the new shard at size %d. "+
			"Entries around the rotation boundary are not identity-scanned; see the shard-rotation note in "+
			"docs/contributor/maintaining.md.\n", o.prev.Origin, o.cur.Origin, o.cur.Size)
		// A catch-up scan mid-window when the shard rotated: the vendored
		// IdentitySearch only reads the latest shard, so the prior shard's unscanned
		// tail is abandoned. Surface it instead of the pre-catch-up assumption that
		// the gap was at most one hour of entries. A negative value means the
		// progress companion could not be read (corrupt); report that, not a count.
		switch {
		case o.abandonedProgress > 0:
			fmt.Fprintf(w, "WARNING: an in-progress prior-shard identity scan had reached index %d; "+
				"its remaining entries are abandoned across the rotation and never scanned.\n", o.abandonedProgress)
		case o.abandonedProgress < 0:
			fmt.Fprintf(w, "WARNING: prior-shard scan progress could not be read (corrupt companion); "+
				"any in-progress prior-shard scan is abandoned across the rotation.\n")
		}
		return
	}
	fmt.Fprintf(w, "consistency verified: %d -> %d\n", o.prev.Size, o.cur.Size)
	if !o.scanned {
		return
	}
	if !o.hasFindings() {
		if o.scanned && !o.caughtUp {
			fmt.Fprintf(w, "identity scan [%d, %d]: no matching entries; catching up, %d entr(y/ies) remaining\n",
				o.from, o.to, o.remaining)
			return
		}
		fmt.Fprintf(w, "identity scan [%d, %d]: no matching entries\n", o.from, o.to)
		return
	}
	fmt.Fprintf(w, "identity scan [%d, %d]: ALERT: %d matching identit(y/ies), %d failed entr(y/ies)\n",
		o.from, o.to, len(o.found), len(o.failed))
	for _, f := range o.found {
		fmt.Fprintf(w, "  MATCH: %+v\n", f)
	}
	for _, f := range o.failed {
		fmt.Fprintf(w, "  FAILED: %+v\n", f)
	}
}
