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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
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
	"../../demos/end-to-end-cli.md",
	"../../demos/recipe-data-architecture.md",
}

// docsExampleHost is the host the reference uses for a locally running aicrd.
// Only requests aimed at it are replayed; an example pointing at a registry or
// an external service is documentation about something else.
const docsExampleHost = "localhost:8080"

// docsExpectation pins a documented request whose published behavior depends on
// how the server was started, or which is documented as failing.
//
// The association cannot live in the page: the MDX safety gate rejects HTML
// comments outside code fences, and an MDX comment would render literally on
// GitHub. So it lives here, in the shape this repository already uses for
// acknowledged exceptions (pkg/client/v1/api-diff-exceptions.yaml) — including
// the part that makes such a list safe, which is that a stale entry fails.
//
// Without this, the allowlist section was covered in name only. The page tells
// the operator to start aicrd with AICR_ALLOWED_ACCELERATORS=h100,l40 and then
// shows accelerator=gb200 returning 400. Replayed against the default allow-all
// fixture that request returns 200, and a gate that only rejects 4xx reports the
// success as a pass — so the one status the page actually promises was the one
// nothing checked.
type docsExpectation struct {
	file       string
	target     string
	wantStatus int
	allow      *aicr.AllowLists
	why        string
}

var docsExpectations = []docsExpectation{
	{
		file:       "../../docs/user/api-reference.md",
		target:     "/v1/recipe?accelerator=gb200&service=eks",
		wantStatus: http.StatusBadRequest,
		allow: &aicr.AllowLists{
			Accelerators: []string{"h100", "l40"},
			Services:     []string{"eks"},
		},
		why: "documented under the H100/L40 allowlist configuration the same " +
			"section tells the operator to start the server with",
	},
}

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
	allow       *aicr.AllowLists
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

	servers := newDocsServerCache(t)

	for _, req := range requests {
		mux := servers.handler(t, req.allow)
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
				// One result per curl stage: a pipeline contributes each leg
				// separately, so none is dropped without a skip line.
				for _, result := range parseCurlRequests(command.text) {
					if !result.ok {
						if result.reason != "" {
							t.Logf("skipped %s:%d (%s)", source,
								block.startLine+command.offset, result.reason)
						}
						continue
					}
					req := result.req
					req.file = source
					req.line = block.startLine + command.offset
					applyDocsExpectation(&req, source)
					requests = append(requests, req)
				}
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
	text   string
	offset int // lines from the block's first content line
}

// shellCommands splits a block into logical commands, joining backslash
// continuations.
//
// A quoted body may span lines without a trailing backslash (curl -d 'a:\n b'),
// so a line is only terminal when quotes are balanced.
func shellCommands(block string) []shellCommand {
	var commands []shellCommand

	lines := strings.Split(block, "\n")

	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(raw)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		start := i
		text := raw
		for continuesCommand(text) && i+1 < len(lines) {
			i++
			text = strings.TrimSuffix(strings.TrimRight(text, " \t"), "\\") + "\n" + lines[i]
		}

		commands = append(commands, shellCommand{text: text, offset: start})
	}
	return commands
}

// continuesCommand reports whether a command continues onto the next line,
// either by a trailing backslash or by an unterminated quoted string.
//
// Both quote styles are tracked. Counting only single quotes left a
// double-quoted multi-line body looking complete, after which tokenizeShell
// reported an unterminated quote and the request was merely logged as skipped —
// coverage lost quietly, with the request floor still satisfied by its
// neighbors.
func continuesCommand(text string) bool {
	if strings.HasSuffix(strings.TrimRight(text, " \t"), "\\") {
		return true
	}

	var inSingle, inDouble bool
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			// A backslash escapes inside double quotes and outside quotes, but
			// is literal inside single quotes.
			if !inSingle && i+1 < len(runes) {
				i++
			}
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		}
	}
	return inSingle || inDouble
}

// curlSegments returns every pipeline stage that runs curl, in order.
//
// Non-curl stages are excluded because their flags collide with curl's. The
// reference pipes a header dump through `tr -d '\r'`, whose -d meant "data" to
// this parser: the example was replayed as a POST carrying a carriage return,
// and the gate reported a documented GET as broken. A false failure is worse
// than a missed one here, because it teaches the reader to distrust the gate.
//
// Every stage is returned, not just the first. This used to stop at the first
// curl, so `curl ... | curl -X POST ...` yielded only the GET: the POST was
// discarded before any skip check could see it, so it was neither replayed nor
// reported, and the gate claimed coverage it did not have (#2655). Silent
// under-coverage is worse than an honest skip, because the skip lines are what
// a maintainer reads to know what is not being checked.
func curlSegments(tokens []string) []curlStage {
	var stages []curlStage
	start := 0
	add := func(seg []string) {
		if kind := classifyStage(seg); kind != stageNotCurl {
			stages = append(stages, curlStage{tokens: seg, kind: kind})
		}
	}
	for i, token := range tokens {
		switch token {
		case "|", ";":
			add(tokens[start:i])
			start = i + 1
		}
	}
	add(tokens[start:])
	// A stage with no curl token at all yields nothing, silently. Returning it
	// instead would let `wget "http://..."` or a URL whose fragment happens to
	// read "#curl" be replayed as a GET, inflating the request count and
	// reporting coverage for text that is not a request at all.
	return stages
}

// curlStage is one pipeline stage that has something to do with curl.
type curlStage struct {
	tokens []string
	kind   stageKind
}

// stageKind says how a pipeline stage relates to curl, which decides whether it
// is replayed, reported as skipped, or ignored.
type stageKind int

const (
	// stageNotCurl names no curl at all. Ignored without a skip line: reporting
	// it would mean one for every kubectl, jq, and tr in the documentation.
	stageNotCurl stageKind = iota
	// stageCurl runs curl as its command word.
	stageCurl
	// stageIndirectCurl contains a bare curl token somewhere other than the
	// command word -- behind a wrapper such as `sudo curl` or
	// `kubectl exec … -- curl`, or as a plain argument, as in the package list
	// of `apt-get install … curl …`.
	//
	// These are reported rather than replayed. Replaying would be wrong: a
	// wrapper changes what a faithful replay means, and an argument is not an
	// invocation at all. But dropping them silently is the #2655 defect over
	// again, so they get a skip line and a maintainer decides.
	stageIndirectCurl
)

// shellPrefixWords are the tokens that may precede a command word without
// changing which command runs: POSIX reserved words, and the `!` negation.
//
// tokenizeShell already strips `$(`, so a substitution such as `x=$(curl …)`
// arrives as the assignment token followed by `curl`.
var shellPrefixWords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true,
	"while": true, "until": true, "do": true, "!": true,
}

// classifyStage decides how a pipeline stage relates to curl.
//
// Only the command word makes a stage replayable; treating a curl token
// anywhere as an invocation would readmit the false positives curlSegments
// exists to exclude -- a URL fragment reading "#curl", or a comment mentioning
// it. Leading assignments and reserved words are stepped over to find that
// command word: requiring curl at index 0 drops
// `if metrics=$(curl -fsS …/metrics …); then` in
// docs/integrator/kubernetes-deployment.md.
//
// A curl token found anywhere else makes the stage stageIndirectCurl rather
// than nothing, so it is reported instead of vanishing. The distinction between
// a wrapper and a mere argument needs to know what each command does with its
// operands, which is unbounded -- so this does not try, and hands the maintainer
// a skip line to judge instead.
func classifyStage(segment []string) stageKind {
	for _, token := range segment {
		if strings.Contains(token, "=") || shellPrefixWords[token] {
			continue
		}
		if token == "curl" {
			return stageCurl
		}
		break
	}
	if slices.Contains(segment, "curl") {
		return stageIndirectCurl
	}
	return stageNotCurl
}

// normalizeCurlTokens rewrites the option spellings curl accepts into the
// separated form the parser reads: --opt=value and clustered -Xvalue.
//
// Without this, `curl --request=POST ...` fell through as an unrecognized flag
// and the example was replayed as a GET. That is worse than skipping it: a
// passing GET would report coverage for a documented POST that may itself be
// broken.
func normalizeCurlTokens(tokens []string) []string {
	// Short options that take a value and may be written without a space.
	valueShorts := []string{"-X", "-H", "-d", "-o", "-w", "-u"}

	normalized := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if strings.HasPrefix(token, "--") {
			if name, value, found := strings.Cut(token, "="); found {
				normalized = append(normalized, name, value)
				continue
			}
			normalized = append(normalized, token)
			continue
		}

		if strings.HasPrefix(token, "-") && len(token) > 2 {
			var split bool
			for _, short := range valueShorts {
				if strings.HasPrefix(token, short) {
					normalized = append(normalized, short, token[len(short):])
					split = true
					break
				}
			}
			if split {
				continue
			}
		}
		normalized = append(normalized, token)
	}
	return normalized
}

// docsParse is one curl stage's parse outcome. A command may contain several.
type docsParse struct {
	req    docsRequest
	ok     bool
	reason string
}

// parseCurlRequests converts every curl stage in a command into a replayable
// request, so a pipeline contributes one result per stage rather than one for
// the whole command. Each result is independently ok or skipped-with-a-reason;
// none is dropped (#2655).
func parseCurlRequests(command string) []docsParse {
	// A cheap reject before tokenizing. The authoritative check is the curl
	// token in classifyStage: this substring alone also matches prose and other
	// commands that merely mention curl.
	if !strings.Contains(command, "curl") {
		return nil
	}

	tokens, err := tokenizeShell(command)
	if err != nil {
		return []docsParse{{reason: "unparseable: " + err.Error()}}
	}

	stages := curlSegments(tokens)
	results := make([]docsParse, 0, len(stages))
	for _, stage := range stages {
		if stage.kind == stageIndirectCurl {
			results = append(results, docsParse{
				reason: "curl is not the command word",
			})
			continue
		}
		req, ok, reason := parseCurlSegment(stage.tokens)
		results = append(results, docsParse{req: req, ok: ok, reason: reason})
	}
	return results
}

// parseCurlRequest reports the first curl stage in a command. It exists for the
// single-invocation table tests; the gate itself uses parseCurlRequests so a
// pipeline's later stages are not discarded.
func parseCurlRequest(command string) (req docsRequest, ok bool, reason string) {
	results := parseCurlRequests(command)
	if len(results) == 0 {
		return req, false, ""
	}
	return results[0].req, results[0].ok, results[0].reason
}

// parseCurlSegment converts one curl invocation into a replayable request.
//
// It returns ok=false with a reason for anything that cannot be replayed
// faithfully. Skipping is deliberate and reported rather than silent: a request
// this gate cannot model is not a request it should claim to cover.
func parseCurlSegment(segment []string) (req docsRequest, ok bool, reason string) {
	tokens := normalizeCurlTokens(segment)

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

	// Build the target the way curl would put it on the wire. Splitting on the
	// host string instead would carry a fragment into the request line, and a
	// fragment is never sent — the replayed target would differ from the one
	// the documented command actually issues.
	parsed, parseErr := url.Parse(rawURL)
	if parseErr != nil {
		return req, false, "unparseable URL: " + parseErr.Error()
	}
	if parsed.Host != docsExampleHost {
		return req, false, ""
	}
	req.target = parsed.EscapedPath()
	if req.target == "" {
		req.target = "/"
	}
	if parsed.RawQuery != "" {
		req.target += "?" + parsed.RawQuery
	}
	// The stage's own tokens, not the whole command: for a pipeline this names
	// the leg that produced the request rather than repeating the pipeline for
	// each of them.
	req.source = strings.Join(segment, " ")
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
		case '#':
			// An unquoted # that starts a word begins a comment. Without this,
			// the word "curl" in a trailing comment made any command look like
			// a curl stage — `wget "http://..." # like curl` was replayed as a
			// GET. Mid-word # is literal in shell (and is a URL fragment here).
			if !inWord {
				for i < len(runes) && runes[i] != '\n' {
					i++
				}
				continue
			}
			current.WriteRune(c)
			inWord = true
		case '|', ';':
			// Unquoted pipeline separator. Emitted as its own token so the
			// caller can stop reading arguments at the end of this command.
			flush()
			tokens = append(tokens, string(c))
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

// TestContinuesCommand covers line joining for both quote styles.
//
// The double-quote cases are the regression: tracking only single quotes let a
// double-quoted multi-line body look like a finished command, and the request
// was then dropped with a skip log rather than replayed.
func TestContinuesCommand(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"complete command", `curl "http://localhost:8080/v1/recipe"`, false},
		{"trailing backslash", `curl -X POST \`, true},
		{"open single quote", `curl -d 'criteria:`, true},
		{"closed single quote", `curl -d 'criteria: x'`, false},
		{"open double quote", `curl -d "{`, true},
		{"closed double quote", `curl -d "{}"`, false},
		{"double quote inside single", `curl -d 'say "hi"'`, false},
		{"single quote inside double", `curl -d "it's fine"`, false},
		{"escaped double quote stays open", `curl -d "a \" b`, true},
		{"apostrophe inside double quotes", `curl -w "Time: %{time}s"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := continuesCommand(tt.text); got != tt.want {
				t.Errorf("continuesCommand(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// TestParseCurlRequest covers the option spellings curl accepts.
//
// The --request=POST and -XPOST cases are the regression: an unrecognized
// spelling used to fall through as a plain flag and the example was replayed as
// a GET. A passing GET would then report coverage for a documented POST, which
// is worse than skipping it, because the gate reads as green.
func TestParseCurlRequest(t *testing.T) {
	tests := []struct {
		name            string
		command         string
		wantOK          bool
		wantMethod      string
		wantTarget      string
		wantBody        string
		wantContentType string
	}{
		{
			name:       "plain GET",
			command:    `curl "http://localhost:8080/v1/recipe?service=eks"`,
			wantOK:     true,
			wantMethod: http.MethodGet,
			wantTarget: "/v1/recipe?service=eks",
		},
		{
			name:       "separated --request",
			command:    `curl --request POST "http://localhost:8080/v1/recipe"`,
			wantOK:     true,
			wantMethod: http.MethodPost,
			wantTarget: "/v1/recipe",
		},
		{
			name:       "inline --request=POST",
			command:    `curl --request=POST "http://localhost:8080/v1/recipe"`,
			wantOK:     true,
			wantMethod: http.MethodPost,
			wantTarget: "/v1/recipe",
		},
		{
			name:       "clustered -XPOST",
			command:    `curl -XPOST "http://localhost:8080/v1/recipe"`,
			wantOK:     true,
			wantMethod: http.MethodPost,
			wantTarget: "/v1/recipe",
		},
		{
			name:       "head via -I",
			command:    `curl -I "http://localhost:8080/v1/recipe"`,
			wantOK:     true,
			wantMethod: http.MethodHead,
			wantTarget: "/v1/recipe",
		},
		{
			name:       "head via clustered -sI",
			command:    `curl -sI "http://localhost:8080/v1/recipe"`,
			wantOK:     true,
			wantMethod: http.MethodHead,
			wantTarget: "/v1/recipe",
		},
		{
			name: "body implies POST and carries content type",
			command: `curl "http://localhost:8080/v1/recipe" ` +
				`-H "Content-Type: application/json" -d '{"criteria":{}}'`,
			wantOK:          true,
			wantMethod:      http.MethodPost,
			wantTarget:      "/v1/recipe",
			wantBody:        `{"criteria":{}}`,
			wantContentType: "application/json",
		},
		{
			name: "multi-line double-quoted body",
			command: "curl -X POST \"http://localhost:8080/v1/recipe\" \\\n" +
				"  -H \"Content-Type: application/json\" \\\n" +
				"  -d \"{\n  \\\"criteria\\\": {}\n}\"",
			wantOK:          true,
			wantMethod:      http.MethodPost,
			wantTarget:      "/v1/recipe",
			wantBody:        "{\n  \"criteria\": {}\n}",
			wantContentType: "application/json",
		},
		{
			name:       "command substitution unwrapped",
			command:    `response=$(curl -s "http://localhost:8080/v1/recipe?service=eks")`,
			wantOK:     true,
			wantMethod: http.MethodGet,
			wantTarget: "/v1/recipe?service=eks",
		},
		{
			name:       "output flag value is not the URL",
			command:    `curl -s -o recipe.json "http://localhost:8080/v1/recipe?service=eks"`,
			wantOK:     true,
			wantMethod: http.MethodGet,
			wantTarget: "/v1/recipe?service=eks",
		},
		{
			// tr's -d is not curl's -d. Reading past the pipe turned a
			// documented GET into a POST carrying a carriage return, and the
			// gate reported the example as broken.
			name: "later pipeline stage flags are not curl's",
			command: `curl -sD - -o /dev/null "http://localhost:8080/v1/recipe?service=eks"` +
				` | grep -i "Retry-After" | awk '{print $2}' | tr -d '\r'`,
			wantOK:     true,
			wantMethod: http.MethodGet,
			wantTarget: "/v1/recipe?service=eks",
		},
		{
			name:    "body from file is skipped",
			command: `curl -X POST "http://localhost:8080/v1/bundle" -d @recipe.json`,
			wantOK:  false,
		},
		{
			name:    "placeholder URL is skipped",
			command: `curl "http://localhost:8080/v1/recipe?service=<service>"`,
			wantOK:  false,
		},
		{
			// Without a curl stage the segment is empty. Returning every token
			// instead would replay this as a GET and count it as covered.
			name:    "url fragment mentioning curl is not a request",
			command: `echo "http://localhost:8080/v1/recipe#curl"`,
			wantOK:  false,
		},
		{
			name:    "another command with curl in a trailing comment",
			command: `wget "http://localhost:8080/v1/recipe?service=eks" # like curl`,
			wantOK:  false,
		},
		{
			// Two rules meet here. A quoted # is a URL fragment, not the
			// start of a comment, so the command still parses. But a fragment
			// is never sent on the wire, so it must not reach the replayed
			// target — the first version of this case asserted otherwise and
			// pinned the wrong behavior.
			name:       "quoted fragment parses but is not replayed",
			command:    `curl "http://localhost:8080/v1/recipe?service=eks#frag"`,
			wantOK:     true,
			wantMethod: http.MethodGet,
			wantTarget: "/v1/recipe?service=eks",
		},
		{
			name:    "other host is skipped",
			command: `curl "https://registry.example.com/v2/_catalog"`,
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, ok, reason := parseCurlRequest(tt.command)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v (reason %q), want %v", ok, reason, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if req.method != tt.wantMethod {
				t.Errorf("method = %q, want %q", req.method, tt.wantMethod)
			}
			if req.target != tt.wantTarget {
				t.Errorf("target = %q, want %q", req.target, tt.wantTarget)
			}
			if req.body != tt.wantBody {
				t.Errorf("body = %q, want %q", req.body, tt.wantBody)
			}
			if req.contentType != tt.wantContentType {
				t.Errorf("contentType = %q, want %q", req.contentType, tt.wantContentType)
			}
		})
	}
}

// TestParseCurlRequestsPipeline pins the multi-stage behavior separately from
// the table above, which only ever inspects one request.
//
// Before #2655 the parser stopped at the first curl, so a `curl … | curl -X
// POST …` example yielded only the GET. The POST was discarded before any skip
// check ran, so it was neither replayed nor logged and the gate reported
// coverage it did not have. A silently-passing gate is the dangerous direction.
func TestParseCurlRequestsPipeline(t *testing.T) {
	// wantMethod is the replayed method, or "" when the stage is expected to be
	// skipped. A skipped stage must still carry a reason: the invariant is that
	// every stage is accounted for, not that every stage is replayable.
	tests := []struct {
		name        string
		command     string
		wantMethods []string
	}{
		{
			// The bundle example from demos/end-to-end-cli.md. The POST leg
			// reads its body from stdin, so it is correctly skipped -- but it
			// must be *seen* and reported, which is what previously did not
			// happen.
			name: "piped curl to curl accounts for both stages",
			command: `curl -s "http://localhost:8080/v1/recipe?service=eks" | ` +
				`curl -X POST "http://localhost:8080/v1/bundle?deployer=argocd" -d @-`,
			wantMethods: []string{http.MethodGet, ""},
		},
		{
			// The case curlSegments was originally written for: a later stage
			// whose flags collide with curl's must not be parsed as curl.
			// `-I` makes the first stage a HEAD.
			name:        "non-curl pipeline stage is not a request",
			command:     `curl -sI "http://localhost:8080/v1/recipe" | tr -d '\r'`,
			wantMethods: []string{http.MethodHead},
		},
		{
			name:        "single curl is unchanged",
			command:     `curl "http://localhost:8080/v1/recipe?service=eks"`,
			wantMethods: []string{http.MethodGet},
		},
		{
			// docs/integrator/kubernetes-deployment.md's readiness probe.
			// curl is not the first token: tokenizeShell strips `$(`, leaving
			// `if`, `metrics=`, `curl`. An earlier draft of isCurlStage looked
			// only at index 0 and dropped this one silently -- the same class
			// of defect this test exists to prevent.
			name: "curl behind a keyword and an assignment is found",
			command: `if metrics=$(curl -fsS http://localhost:8080/metrics 2>/dev/null); ` +
				`then ready=1; break; fi`,
			wantMethods: []string{http.MethodGet},
		},
		{
			// The other side of that allowance: stepping over reserved words
			// must not turn a non-curl stage into a request.
			name:        "keyword-led non-curl stage is not a request",
			command:     `if kubectl get pods; then echo ok; fi`,
			wantMethods: nil,
		},
		{
			// A wrapper runs curl as a child, so replaying the stage would not
			// be faithful -- but it must still be reported. Dropping it is the
			// same silent under-coverage this test exists to prevent.
			name:        "wrapper-invoked curl is reported, not dropped",
			command:     `sudo curl "http://localhost:8080/health"`,
			wantMethods: []string{""},
		},
		{
			name:        "curl reached through kubectl exec is reported",
			command:     `kubectl exec -it aicrd-0 -- curl "http://localhost:8080/health"`,
			wantMethods: []string{""},
		},
		{
			// curl as a plain operand, not an invocation: DEVELOPMENT.md's
			// prerequisites line. Reported for the same reason -- telling a
			// wrapper from an argument needs per-command knowledge this gate
			// does not have.
			name:        "curl as a package name is reported",
			command:     `sudo apt-get install -y make git curl pipx`,
			wantMethods: []string{""},
		},
		{
			name:        "no curl stage yields nothing",
			command:     `wget "http://localhost:8080/v1/recipe" # like curl`,
			wantMethods: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := parseCurlRequests(tt.command)
			if len(results) != len(tt.wantMethods) {
				t.Fatalf("got %d stage(s), want %d -- a stage that is neither "+
					"replayed nor skipped has been dropped",
					len(results), len(tt.wantMethods))
			}
			for i, want := range tt.wantMethods {
				switch {
				case want == "":
					if results[i].ok {
						t.Errorf("stage %d replayed, want skipped", i)
					}
					if results[i].reason == "" {
						t.Errorf("stage %d skipped with no reason; a silent "+
							"skip is indistinguishable from a dropped stage", i)
					}
				case !results[i].ok:
					t.Errorf("stage %d skipped (%q), want method %q",
						i, results[i].reason, want)
				case results[i].req.method != want:
					t.Errorf("stage %d method = %q, want %q",
						i, results[i].req.method, want)
				}
			}
		})
	}
}

// docsServerCache hands out a handler per allowlist configuration.
//
// Most examples run against the default allow-all server, but the allowlist
// section documents a specific configuration and the 400 that follows from it.
// Replaying that example against allow-all returns 200, which is a real
// response to a different question. Servers are cached because building one
// resolves the embedded catalog.
type docsServerCache struct {
	handlers map[string]http.Handler
}

func newDocsServerCache(t *testing.T) *docsServerCache {
	t.Helper()
	return &docsServerCache{handlers: map[string]http.Handler{}}
}

func (c *docsServerCache) handler(t *testing.T, allow *aicr.AllowLists) http.Handler {
	t.Helper()

	key := allowListKey(allow)
	if handler, ok := c.handlers[key]; ok {
		return handler
	}

	cfg := parseConfig()
	cfg.Handlers = newRoutes(newTestHandler(t, allow), newTestBundleHandler(t))
	cfg.RateLimit = 1e6
	cfg.RateLimitBurst = 1e6

	server := New(withConfig(cfg))
	// The docs describe a running server, and Serve marks itself ready once
	// startup completes. Without this GET /ready answers 503 and the example
	// looks like a documentation error rather than a harness that never
	// finished starting.
	server.setReady(true)

	c.handlers[key] = server.httpServer.Handler
	return c.handlers[key]
}

// allowListKey canonicalizes an allowlist so two directives requesting the same
// configuration share one server.
func allowListKey(allow *aicr.AllowLists) string {
	if allow == nil {
		return "default"
	}
	return fmt.Sprintf("a=%v|s=%v|i=%v|o=%v",
		allow.Accelerators, allow.Services, allow.Intents, allow.OSTypes)
}

// applyDocsExpectation attaches a pinned expectation to a matching request.
func applyDocsExpectation(req *docsRequest, source string) {
	for _, want := range docsExpectations {
		if filepath.Clean(want.file) == filepath.Clean(source) && want.target == req.target {
			req.wantStatus = want.wantStatus
			req.allow = want.allow
			return
		}
	}
}

// TestDocsExpectationsAreLive asserts every pinned expectation still matches a
// documented request.
//
// This is the half that makes an out-of-band list safe. An entry whose example
// was reworded or deleted would otherwise sit here forever, describing a
// contract nothing exercises, and the replay gate would quietly go back to
// asserting only "not 4xx" for the case that motivated the entry. Same reason
// tools/api-diff fails on a stale acknowledgement instead of ignoring it.
func TestDocsExpectationsAreLive(t *testing.T) {
	requests := documentedAPIRequests(t)
	if len(requests) == 0 {
		t.Fatal("no documented requests recovered; this assertion would pass vacuously")
	}

	for _, want := range docsExpectations {
		var found bool
		for _, req := range requests {
			if filepath.Clean(req.file) == filepath.Clean(want.file) && req.target == want.target {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docsExpectations pins %s %q (%s) but no documented request "+
				"matches it; the example moved or was removed, so this entry no "+
				"longer gates anything — update or delete it",
				want.file, want.target, want.why)
		}
	}
}
