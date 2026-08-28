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

package deprecation

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNoticeMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		notice Notice
		want   string
	}{
		{
			name: "with replacement",
			notice: Notice{
				Subject:     "--legacy-flag",
				Replacement: "--new-flag",
				RemovedIn:   "v0.25",
			},
			want: "--legacy-flag is deprecated and will be removed in v0.25; use --new-flag instead",
		},
		{
			// A capability going away with no successor is the case a user most
			// needs stated outright, so the message must not imply one exists.
			name: "without replacement",
			notice: Notice{
				Subject:   "apiVersion aicr.run/v1alpha2",
				RemovedIn: "v0.23",
			},
			want: "apiVersion aicr.run/v1alpha2 is deprecated and will be removed in v0.23",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.notice.Message(); got != tt.want {
				t.Errorf("Message() = %q, want %q", got, tt.want)
			}
		})
	}
}

// captureLogs redirects slog for the duration of a test and returns the buffer.
// slog's default logger is process-global, so these tests cannot run in
// parallel with each other.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestRecorderWarnsOncePerSubject is the property that makes this usable from a
// loader loop. Without it a catalog of 100 deprecated overlays emits 100
// identical lines and the user learns nothing they did not know after the
// first.
func TestRecorderWarnsOncePerSubject(t *testing.T) {
	buf := captureLogs(t)

	var r Recorder
	notice := Notice{Subject: "--legacy-flag", Replacement: "--new-flag", RemovedIn: "v0.25"}
	for range 5 {
		r.Warn(notice)
	}
	r.Warn(Notice{Subject: "--other-flag", RemovedIn: "v0.25"})

	if got := strings.Count(buf.String(), "--legacy-flag is deprecated"); got != 1 {
		t.Errorf("repeated subject logged %d times, want 1", got)
	}
	if got := strings.Count(buf.String(), "--other-flag is deprecated"); got != 1 {
		t.Errorf("distinct subject logged %d times, want 1", got)
	}
}

// TestRecorderWarnEmitsStructuredContext pins the fields a consumer of the log
// stream can rely on. The message alone is prose; these are what a log
// aggregator can alert on.
func TestRecorderWarnEmitsStructuredContext(t *testing.T) {
	buf := captureLogs(t)

	var r Recorder
	r.Warn(Notice{Subject: "/v1/recipe", Replacement: "/v2/recipe", RemovedIn: "v0.25"})

	out := buf.String()
	for _, want := range []string{
		`subject=/v1/recipe`,
		`removedIn=v0.25`,
		`replacement=/v2/recipe`,
		DocsURL,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\ngot: %s", want, out)
		}
	}
}

// TestRecorderIgnoresEmptySubject guards the deduplication key. An empty
// subject would collapse every unrelated notice into one bucket, silently
// suppressing real warnings after the first.
func TestRecorderIgnoresEmptySubject(t *testing.T) {
	buf := captureLogs(t)

	var r Recorder
	r.Warn(Notice{RemovedIn: "v0.25"})

	// Assert on the empty-subject call in isolation, before anything else has
	// logged. An earlier version of this test made both calls first and then
	// looked for the malformed line "and no --real-flag line" — but the second
	// call always logs --real-flag, so that condition was never true and the
	// assertion could not fail. It also anchored on a newline immediately after
	// the message, which slog's text handler never emits because the structured
	// attributes follow on the same line.
	if got := buf.String(); got != "" {
		t.Errorf("empty-subject notice was logged, want it dropped; got: %s", got)
	}

	r.Warn(Notice{Subject: "--real-flag", RemovedIn: "v0.25"})

	if !strings.Contains(buf.String(), "--real-flag is deprecated") {
		t.Error("a real notice was suppressed by the empty-subject call")
	}
}

func TestRecorderWarnIsConcurrencySafe(t *testing.T) {
	captureLogs(t)

	var r Recorder
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Warn(Notice{Subject: "shared", RemovedIn: "v0.25"})
			r.Warn(Notice{Subject: string(rune('a' + i%8)), RemovedIn: "v0.25"})
		}()
	}
	wg.Wait()
}

func TestSetHTTPHeaders(t *testing.T) {
	t.Parallel()

	// RFC 9745 §2.1's own worked example: Friday, 30 June 2023 23:59:59 UTC is
	// "@1688169599". Using the spec's literal pair pins the encoding against
	// the standard rather than against our own arithmetic.
	deprecated := time.Date(2023, time.June, 30, 23, 59, 59, 0, time.UTC)
	sunset := time.Date(2026, time.November, 11, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		notice  Notice
		want    map[string]string
		absent  []string
		linkSub string
	}{
		{
			// Deprecation and Sunset are different dates with different
			// meanings and different encodings. Asserting both at once is what
			// catches a future edit that reuses one for the other.
			name: "both dates committed",
			notice: Notice{
				Subject:    "/v1/recipe",
				RemovedIn:  "v0.25",
				Deprecated: deprecated,
				Sunset:     sunset,
			},
			want: map[string]string{
				"Deprecation": "@1688169599",
				"Sunset":      "Wed, 11 Nov 2026 00:00:00 GMT",
			},
			linkSub: `rel="deprecation"`,
		},
		{
			// RFC 8594 has no way to say "two releases from now", so Sunset is
			// omitted rather than invented.
			name: "deprecated but no removal date",
			notice: Notice{
				Subject:    "/v1/query",
				RemovedIn:  "v0.25",
				Deprecated: deprecated,
			},
			want:    map[string]string{"Deprecation": "@1688169599"},
			absent:  []string{"Sunset"},
			linkSub: `rel="deprecation"`,
		},
		{
			// RFC 9745 admits only a Date, so there is no honest placeholder.
			// Omitting beats emitting something a conforming client rejects.
			name: "no dates at all still links to the docs",
			notice: Notice{
				Subject:   "/v1/bundle",
				RemovedIn: "v0.25",
			},
			want:    map[string]string{},
			absent:  []string{"Deprecation", "Sunset"},
			linkSub: `rel="deprecation"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := http.Header{}
			SetHTTPHeaders(h, tt.notice)

			for key, want := range tt.want {
				if got := h.Get(key); got != want {
					t.Errorf("%s header = %q, want %q", key, got, want)
				}
			}
			for _, key := range tt.absent {
				if got := h.Get(key); got != "" {
					t.Errorf("%s header = %q, want unset", key, got)
				}
			}
			if link := h.Get("Link"); !strings.Contains(link, tt.linkSub) ||
				!strings.Contains(link, DocsURL) {

				t.Errorf("Link header = %q, want it to carry %q and %q",
					link, tt.linkSub, DocsURL)
			}
		})
	}
}

// TestPackageWarn covers the package-level entry point, which most callers use.
// It shares one process-wide Recorder, so this test picks a subject no other
// test uses — a collision would suppress the log line and the assertion would
// fail for the wrong reason.
func TestPackageWarn(t *testing.T) {
	buf := captureLogs(t)

	Warn(Notice{
		Subject:     "TestPackageWarn-subject",
		Replacement: "the replacement",
		RemovedIn:   "v9.99",
	})

	if got := buf.String(); !strings.Contains(got, "TestPackageWarn-subject is deprecated") {
		t.Errorf("package Warn did not log the notice\ngot: %s", got)
	}
}

// TestSetHTTPHeadersPreservesExistingLink guards the multi-valued Link header.
// A handler that set its own Link for an unrelated reason must keep it; using
// Set here would drop it silently.
func TestSetHTTPHeadersPreservesExistingLink(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	h.Add("Link", `<https://example.com/schema>; rel="describedby"`)

	SetHTTPHeaders(h, Notice{Subject: "/v1/recipe", RemovedIn: "v0.25"})

	links := h.Values("Link")
	if len(links) != 2 {
		t.Fatalf("Link values = %d (%q), want 2", len(links), links)
	}
	if !strings.Contains(links[0], `rel="describedby"`) {
		t.Errorf("pre-existing Link was not preserved: %q", links[0])
	}
	if !strings.Contains(links[1], `rel="deprecation"`) {
		t.Errorf("deprecation Link missing: %q", links[1])
	}
}
