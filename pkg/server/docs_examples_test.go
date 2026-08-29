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

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// REST is one of the four surfaces ROADMAP §1 freezes at v1, and the curl
// examples in docs/user/api-reference.md are the form most integrators actually
// copy. Nothing derived them from the server.
//
// #2464 proved the cost. Collapsing the REST families removed the legacy
// RecipeCriteria POST body, and the reference kept presenting it as the
// required body in prose, a schema block, and four curl examples — every one of
// which returns 400. A full `make qualify` passed. The route-conformance tests
// added in #2461 compare the spec's paths to the mux and never look inside an
// operation; the docs-claims gate from #2462 parses `aicr <cmd> --flag`
// invocations and knows nothing about REST bodies. The published request shape
// had no gate at all.
//
// This closes that (issue #2112, scope item 7) by replaying each documented
// request against a real handler in process. No cluster, no network, no server
// binary: it belongs in `make test`, because the failing doc example was
// invisible to everything except a running server, and E2E only exercises two
// of these requests.
//
// Scope: status codes only. Whether a response *body* matches the documented
// example is the schema gate's job, not this one.

// documentedAPISources are every tracked document that shows a request against
// a locally running aicrd. api-reference.md is the contract page, but the same
// examples are copied into the integrator and development guides, and those
// went stale in #2464 too — gating only the reference would leave the copies
// free to rot.
//
// A new document with aicrd examples must be added here.
// TestDocumentedAPISourcesAreComplete fails when one is missing, so this list
// cannot silently fall behind the docs tree.
var documentedAPISources = []string{
	"../../docs/user/api-reference.md",
	"../../docs/integrator/automation.md",
	"../../docs/integrator/kubernetes-deployment.md",
	"../../DEVELOPMENT.md",
	"../../tests/e2e/README.md",
	"../../demos/private-signing.md",
}

// docsExampleHost is the host the reference uses for a locally running aicrd.
// Only requests aimed at it are replayed; an example pointing at a registry or
// an external service is documentation about something else.
const docsExampleHost = "localhost:8080"

// expectStatusDirective lets a fenced block document a request that is supposed
// to fail, so an error example stays an error example instead of being quietly
// exempted. Written as a shell comment on the line before the request:
//
//	# expect-status: 400
//	curl "http://localhost:8080/v1/recipe"
const expectStatusDirective = "# expect-status:"

// minDocumentedRequests guards against the gate silently becoming inert.
//
// If a refactor breaks extraction — a fence label changes, curl gains a flag the
// tokenizer mishandles — every assertion below would pass by iterating nothing.
// The floor is deliberately well under the current count so ordinary doc edits
// do not trip it, while wholesale extraction failure does. This counts requests
// actually replayed, not lines matched: #2462 shipped a guard that counted
// substrings and could not fire.
const minDocumentedRequests = 25

// docsRequest is one replayable request recovered from the reference.
type docsRequest struct {
	method      string
	target      string // path plus query, host stripped
	contentType string
	body        string
	wantStatus  int // 0 means "any non-error status"
	file        string
	line        int
	source      string
}

func TestDocumentedAPIExamplesAreAccepted(t *testing.T) {
	requests := documentedAPIRequests(t)

	if len(requests) < minDocumentedRequests {
		t.Fatalf("recovered only %d replayable requests from %v, want at least %d; "+
			"extraction is probably broken and every assertion below would pass "+
			"vacuously", len(requests), documentedAPISources, minDocumentedRequests)
	}

	// The reference documents a running server, and Serve marks itself ready
	// once startup completes. Without this, GET /ready answers 503 and the
	// example would look like a documentation error rather than a harness that
	// never finished starting.
	server := newSpecTestServer(t)
	server.setReady(true)
	mux := server.httpServer.Handler

	for _, req := range requests {
		name := fmt.Sprintf("%s_L%d_%s_%s", filepath.Base(req.file), req.line, req.method, req.target)
		t.Run(name, func(t *testing.T) {
			var httpReq *http.Request
			if req.body != "" {
				httpReq = httptest.NewRequest(req.method, req.target, strings.NewReader(req.body))
			} else {
				httpReq = httptest.NewRequest(req.method, req.target, nil)
			}
			if req.contentType != "" {
				httpReq.Header.Set("Content-Type", req.contentType)
			}

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httpReq)

			if req.wantStatus != 0 {
				if rec.Code != req.wantStatus {
					t.Errorf("%s:%d documents %s %s as returning %d, got %d\n"+
						"request: %s\nresponse: %s",
						req.file, req.line, req.method, req.target,
						req.wantStatus, rec.Code, req.source,
						truncateForFailure(rec.Body.String()))
				}
				return
			}

			if rec.Code >= http.StatusBadRequest {
				t.Errorf("%s:%d documents %s %s, but the server rejects it with %d; "+
					"an integrator copying this example gets an error\n"+
					"request: %s\nresponse: %s",
					req.file, req.line, req.method, req.target, rec.Code,
					req.source, truncateForFailure(rec.Body.String()))
			}
		})
	}
}

// truncateForFailure keeps a failure message readable when a handler returns a
// full recipe document.
func truncateForFailure(body string) string {
	const limit = 300
	body = strings.TrimSpace(body)
	if len(body) <= limit {
		return body
	}
	return body[:limit] + "…"
}

// documentedAPIRequests recovers every replayable aicrd request from the
// documented sources.
func documentedAPIRequests(t *testing.T) []docsRequest {
	t.Helper()

	var requests []docsRequest
	for _, source := range documentedAPISources {
		data, err := os.ReadFile(filepath.Clean(source))
		if err != nil {
			t.Fatalf("read %q: %v", source, err)
		}

		for _, block := range shellFencedBlocks(string(data)) {
			for _, command := range shellCommands(block.content) {
				req, ok, reason := parseCurlRequest(command.text)
				if !ok {
					if reason != "" {
						t.Logf("skipped %s:%d (%s)", source,
							block.startLine+command.offset, reason)
					}
					continue
				}
				req.file = source
				req.line = block.startLine + command.offset
				req.wantStatus = command.expectStatus
				requests = append(requests, req)
			}
		}
	}
	return requests
}

// fencedBlock is a fenced code block plus the file line its content starts on.
type fencedBlock struct {
	content   string
	startLine int
}

// shellFencedBlocks returns the shell-ish fenced blocks in a markdown document.
func shellFencedBlocks(doc string) []fencedBlock {
	var blocks []fencedBlock

	lines := strings.Split(doc, "\n")
	shellLangs := map[string]bool{"shell": true, "bash": true, "console": true, "sh": true}

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		lang := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "```")))
		start := i + 1
		end := start
		for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "```") {
			end++
		}
		if shellLangs[lang] {
			blocks = append(blocks, fencedBlock{
				content:   strings.Join(lines[start:end], "\n"),
				startLine: start + 1, // 1-indexed file line of the first content line
			})
		}
		i = end
	}
	return blocks
}

// shellCommand is one logical command within a fenced block.
type shellCommand struct {
	text         string
	offset       int // lines from the block's first content line
	expectStatus int
}

// shellCommands splits a block into logical commands, joining backslash
// continuations and carrying any expect-status directive onto the next command.
//
// A quoted body may span lines without a trailing backslash (curl -d 'a:\n b'),
// so a line is only terminal when quotes are balanced.
func shellCommands(block string) []shellCommand {
	var commands []shellCommand

	lines := strings.Split(block, "\n")
	pendingStatus := 0

	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(raw)

		if strings.HasPrefix(trimmed, expectStatusDirective) {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, expectStatusDirective))
			if code, err := strconv.Atoi(value); err == nil {
				pendingStatus = code
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		start := i
		text := raw
		for continuesCommand(text) && i+1 < len(lines) {
			i++
			text = strings.TrimSuffix(strings.TrimRight(text, " \t"), "\\") + "\n" + lines[i]
		}

		commands = append(commands, shellCommand{
			text:         text,
			offset:       start,
			expectStatus: pendingStatus,
		})
		pendingStatus = 0
	}
	return commands
}

// continuesCommand reports whether a command continues onto the next line,
// either by a trailing backslash or by an unterminated single-quoted string.
func continuesCommand(text string) bool {
	if strings.HasSuffix(strings.TrimRight(text, " \t"), "\\") {
		return true
	}
	return strings.Count(text, "'")%2 == 1
}

// parseCurlRequest converts a curl invocation into a replayable request.
//
// It returns ok=false with a reason for anything that cannot be replayed
// faithfully. Skipping is deliberate and reported rather than silent: a request
// this gate cannot model is not a request it should claim to cover.
func parseCurlRequest(command string) (req docsRequest, ok bool, reason string) {
	if !strings.Contains(command, "curl") {
		return req, false, ""
	}
	// A pipeline's later stages consume the previous command's output rather
	// than being standalone requests.
	if idx := strings.Index(command, "curl"); idx > 0 {
		before := strings.TrimSpace(command[:idx])
		if strings.HasSuffix(before, "|") {
			return req, false, "piped input"
		}
	}

	tokens, err := tokenizeShell(command)
	if err != nil {
		return req, false, "unparseable: " + err.Error()
	}

	req.method = http.MethodGet
	var rawURL string

	for i := 0; i < len(tokens); i++ {
		switch tokens[i] {
		case "-X", "--request":
			if i+1 < len(tokens) {
				i++
				req.method = strings.ToUpper(tokens[i])
			}
		case "-H", "--header":
			if i+1 < len(tokens) {
				i++
				name, value, found := strings.Cut(tokens[i], ":")
				if found && strings.EqualFold(strings.TrimSpace(name), "Content-Type") {
					req.contentType = strings.TrimSpace(value)
				}
			}
		case "-d", "--data", "--data-raw", "--data-binary", "--data-ascii":
			if i+1 < len(tokens) {
				i++
				req.body = tokens[i]
			}
		case "-o", "--output", "-w", "--write-out", "-u", "--user":
			i++ // consume the flag's value so it is not mistaken for the URL
		case "-I", "--head":
			req.method = http.MethodHead
		default:
			if strings.HasPrefix(tokens[i], "-") {
				// Clustered short flags, e.g. -sI. Only -I changes the request.
				if !strings.HasPrefix(tokens[i], "--") &&
					strings.ContainsRune(tokens[i], 'I') {

					req.method = http.MethodHead
				}
				continue
			}
			if tokens[i] == "curl" {
				continue
			}
			if rawURL == "" && strings.Contains(tokens[i], "://") {
				rawURL = tokens[i]
			}
		}
	}

	if rawURL == "" {
		return req, false, "no URL"
	}
	if !strings.Contains(rawURL, docsExampleHost) {
		return req, false, ""
	}
	if strings.ContainsAny(rawURL, "<>${}") {
		return req, false, "URL contains a placeholder"
	}
	if strings.HasPrefix(req.body, "@") {
		return req, false, "body reads a file"
	}
	if strings.ContainsAny(req.body, "<>$") {
		return req, false, "body contains a placeholder"
	}

	_, after, found := strings.Cut(rawURL, docsExampleHost)
	if !found {
		return req, false, "could not split host"
	}
	if after == "" {
		after = "/"
	}
	req.target = after
	req.source = strings.Join(strings.Fields(command), " ")
	if len(req.source) > 200 {
		req.source = req.source[:200] + "…"
	}

	// A body with no method is curl's implicit POST.
	if req.body != "" && req.method == http.MethodGet {
		req.method = http.MethodPost
	}
	return req, true, ""
}

// tokenizeShell splits a command into words, honoring single quotes, double
// quotes and backslash-newline continuations. It covers the subset of shell
// syntax the reference's examples use.
func tokenizeShell(command string) ([]string, error) {
	var (
		tokens  []string
		current strings.Builder
		inWord  bool
	)

	flush := func() {
		if inWord {
			tokens = append(tokens, current.String())
			current.Reset()
			inWord = false
		}
	}

	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case '\'':
			inWord = true
			i++
			for i < len(runes) && runes[i] != '\'' {
				current.WriteRune(runes[i])
				i++
			}
			if i >= len(runes) {
				return nil, fmt.Errorf("unterminated single quote")
			}
		case '"':
			inWord = true
			i++
			for i < len(runes) && runes[i] != '"' {
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
				}
				current.WriteRune(runes[i])
				i++
			}
			if i >= len(runes) {
				return nil, fmt.Errorf("unterminated double quote")
			}
		case '\\':
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
				continue
			}
			if i+1 < len(runes) {
				i++
				current.WriteRune(runes[i])
				inWord = true
			}
		case '$':
			// Command substitution: `response=$(curl ...)`. Drop the wrapper so
			// the inner request is recovered instead of the URL keeping a
			// trailing paren.
			if i+1 < len(runes) && runes[i+1] == '(' {
				i++
				flush()
				continue
			}
			current.WriteRune(c)
			inWord = true
		case ')', '(':
			// Unquoted, so it is substitution or grouping syntax, never part of
			// a URL. Quoted parens never reach here.
			flush()
		case ' ', '\t', '\n':
			flush()
		default:
			current.WriteRune(c)
			inWord = true
		}
	}
	flush()
	return tokens, nil
}

// TestDocumentedAPISourcesAreComplete asserts documentedAPISources names every
// tracked document that shows a request against a locally running aicrd.
//
// Without this, the replay gate above degrades silently: a new page with
// examples, or examples moved into a page not on the list, would simply never
// be checked, and the suite would stay green while the coverage shrank. The
// discovery walks tracked files so an untracked scratch file cannot fail it.
func TestDocumentedAPISourcesAreComplete(t *testing.T) {
	listed := make(map[string]bool, len(documentedAPISources))
	for _, source := range documentedAPISources {
		listed[filepath.Clean(source)] = true
	}

	out, err := exec.Command("git", "-C", "../..", "ls-files", "*.md").Output()
	if err != nil {
		t.Fatalf("list tracked markdown: %v", err)
	}

	tracked := strings.Fields(string(out))
	if len(tracked) == 0 {
		t.Fatal("git ls-files returned no markdown; discovery is broken and this " +
			"assertion would pass vacuously")
	}

	var checked int
	for _, rel := range tracked {
		path := filepath.Join("..", "..", rel)
		data, readErr := os.ReadFile(filepath.Clean(path))
		if readErr != nil {
			continue
		}
		checked++
		if !strings.Contains(string(data), docsExampleHost) {
			continue
		}
		// ADRs under docs/design are historical records of what a release
		// decided, not instructions to run, so their examples are deliberately
		// not held to current behavior.
		if strings.HasPrefix(rel, "docs/design/") {
			continue
		}
		if !listed[filepath.Clean(path)] {
			t.Errorf("%s contains %s examples but is absent from "+
				"documentedAPISources, so its requests are never replayed; add it "+
				"there", rel, docsExampleHost)
		}
	}

	if checked == 0 {
		t.Fatal("read no markdown files; discovery is broken")
	}
}
