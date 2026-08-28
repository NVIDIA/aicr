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

// Package deprecation implements the runtime half of the AICR deprecation
// channel. The policy half — what counts as breaking on each of the four
// frozen surfaces, and the notice a removal owes — lives in RELEASING.md; the
// user-facing register of active deprecations is docs/user/deprecations.md.
//
// Three of the four surfaces warn through this package:
//
//   - CLI and artifact loaders call Warn, which emits one slog warning per
//     distinct subject.
//   - REST handlers call SetHTTPHeaders, which sets the Deprecation (RFC 9745),
//     Sunset (RFC 8594), and Link response fields.
//
// The fourth, the Go SDK, needs no runtime support: a `// Deprecated:` godoc
// marker is reported to consumers by staticcheck at their build time, which is
// strictly better than a message they would have to run the code to see.
//
// A Notice is data, not a call site. Declaring one as a package-level var next
// to the thing it deprecates keeps the removal release visible in the code that
// must eventually delete it, rather than only in a changelog.
package deprecation

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DocsURL is the durable register of active deprecations. It is referenced from
// warning text and from the Link response header so a user who sees only the
// warning can still find the full entry.
//
// This points at the source on GitHub rather than the rendered page on
// docs.nvidia.com deliberately. The published URL is derived by Fern from the
// nav in docs/index.yml and is versioned per release, so it is neither stable
// across releases nor verifiable from this repository. A dead link inside a
// deprecation warning defeats the purpose of the warning.
const DocsURL = "https://github.com/NVIDIA/aicr/blob/main/docs/user/deprecations.md"

// Notice describes one deprecation.
type Notice struct {
	// Subject is the thing being deprecated, named as a user would refer to
	// it: "--legacy-flag", "/v1/recipe", "apiVersion aicr.run/v1alpha2".
	// It is also the deduplication key, so it must be stable across calls.
	Subject string

	// Replacement is what to use instead. Empty when the capability is going
	// away with no successor, which is worth saying explicitly rather than
	// leaving the user to guess.
	Replacement string

	// RemovedIn is the AICR release that removes Subject, e.g. "v0.23".
	RemovedIn string

	// Deprecated is when Subject became (or becomes) deprecated. This is the
	// value of the RFC 9745 Deprecation header and is distinct from Sunset:
	// deprecation is the announcement, sunset is the removal. Zero when no
	// date is recorded, in which case the header is omitted — RFC 9745 admits
	// only a date, so there is no honest way to say "deprecated, date unknown".
	Deprecated time.Time

	// Sunset is the calendar date of the removal, when one is committed. Zero
	// when the removal is pinned to a release but not yet to a date; the
	// Sunset response header is then omitted, since RFC 8594 has no way to
	// express "a release from now".
	Sunset time.Time
}

// Message renders the notice as a single line of user-facing text.
func (n Notice) Message() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is deprecated and will be removed in %s", n.Subject, n.RemovedIn)
	if n.Replacement != "" {
		fmt.Fprintf(&b, "; use %s instead", n.Replacement)
	}
	return b.String()
}

// Recorder emits each distinct Notice at most once. Callers that warn from a
// loop — a bundle loader walking components, a catalog scanner walking files —
// need this, or a single deprecated input produces one line per item and the
// signal is lost in its own repetition.
//
// The zero Recorder is ready to use.
type Recorder struct {
	mu   sync.Mutex
	seen map[string]bool
}

// Warn emits n once per distinct Subject for the life of this Recorder.
//
// It routes through slog rather than writing to stderr directly, which means it
// honors AICR_LOG_LEVEL, NO_COLOR, and TTY detection like every other AICR
// diagnostic (see pkg/logging). The tradeoff is real and deliberate: a user who
// has set AICR_LOG_LEVEL=error will not see deprecation warnings. Silencing
// warnings is an explicit opt-out, and the release notes plus docs/user/
// deprecations.md remain the channels that do not depend on log level.
func (r *Recorder) Warn(n Notice) {
	if n.Subject == "" {
		return
	}

	r.mu.Lock()
	if r.seen == nil {
		r.seen = make(map[string]bool)
	}
	already := r.seen[n.Subject]
	r.seen[n.Subject] = true
	r.mu.Unlock()

	if already {
		return
	}

	attrs := []any{
		"subject", n.Subject,
		"removedIn", n.RemovedIn,
		"details", DocsURL,
	}
	if n.Replacement != "" {
		attrs = append(attrs, "replacement", n.Replacement)
	}
	slog.Warn(n.Message(), attrs...)
}

// defaultRecorder backs the package-level Warn. Process-wide deduplication is
// the right default for a CLI invocation: the user needs to be told once per
// run, not once per call site.
var defaultRecorder = &Recorder{}

// Warn emits n through the process-wide Recorder. Tests that assert on
// repetition should construct their own Recorder instead, so one test's
// subjects do not suppress another's.
func Warn(n Notice) { defaultRecorder.Warn(n) }

// SetHTTPHeaders marks an HTTP response as coming from a deprecated resource.
//
// Two different standards with two different date formats, carrying two
// different meanings:
//
//   - Deprecation (RFC 9745 §2.1) is an Item Structured Header whose value MUST
//     be a Date per RFC 9651 §3.3.7 — "@" followed by Unix seconds, e.g.
//     "@1688169599". It carries the date the resource became deprecated.
//   - Sunset (RFC 8594) is an IMF-fixdate and carries the date the resource is
//     removed.
//
// Neither admits a placeholder, so a zero time omits its header rather than
// emitting something a conforming client would reject. An unparseable
// Deprecation is worse than an absent one: it tells the client nothing and
// costs it an error path.
//
// The deprecation link relation is RFC 9745 §3 and is always safe to send.
//
// Headers must be set before the response body is written; a handler that has
// already called WriteHeader will find these silently dropped.
func SetHTTPHeaders(h http.Header, n Notice) {
	if !n.Deprecated.IsZero() {
		h.Set("Deprecation", fmt.Sprintf("@%d", n.Deprecated.UTC().Unix()))
	}
	if !n.Sunset.IsZero() {
		h.Set("Sunset", n.Sunset.UTC().Format(http.TimeFormat))
	}
	// Add, not Set: Link is a multi-valued header, and clobbering a Link a
	// handler set for its own reasons would be a silent regression.
	h.Add("Link", fmt.Sprintf("<%s>; rel=\"deprecation\"", DocsURL))
}
