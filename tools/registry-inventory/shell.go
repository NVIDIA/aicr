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

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// shellFiles are the installer scripts scanned for tool-download egress. This
// pass is best-effort by nature — shell URLs are assembled from variables and
// appear in comments and log lines — so it (a) matches on a command *verb*, not
// a bare URL, to avoid advice/comment false positives, and (b) emits a warning
// for any download-looking line it cannot resolve, so the inventory never reads
// as more complete than it is.
var shellFiles = []string{
	filepath.Join("tools", "setup-tools"),
}

var (
	shGoInstallRe = regexp.MustCompile(`\bgo install\s+(\S+?)@(\S+)`)
	// The install verbs capture everything after `install` so multi-package
	// lines (`apt-get install a b c`) and trailing tokens (`... 2>/dev/null \`)
	// are handled by the tokenizer rather than a brittle end-anchor.
	shPipRe      = regexp.MustCompile(`\bpip install\s+(.+)$`)
	shAptRe      = regexp.MustCompile(`\b(?:apt-get|apt)\s+install\s+(.+)$`)
	shBrewRe     = regexp.MustCompile(`\bbrew install\s+(.+)$`)
	shURLRe      = regexp.MustCompile("https?://[A-Za-z0-9._-]+(?:/[^\\s\"'`)]*)?")
	shVersionSeg = regexp.MustCompile(`/v?\d+\.\d+`)
	shDownloadRe = regexp.MustCompile(`\b(curl|wget)\b`)
	// A line whose *command* (first word, after control keywords like sudo/if)
	// is a logger/echo is advice or output, not an install — skip it so
	// `log_warning "run: pip install foo==X"` is not mined. Matching only the
	// command position (not "contains echo") means `curl … && echo done` is
	// still processed for the curl.
	shLoggerStartRe = regexp.MustCompile(`^(?:sudo\s+|if\s+|then\s+|elif\s+|else\s+|while\s+|do\s+|!\s*|\{\s+)*(?:log_[a-z_]+|echo|printf)\b`)
)

// extractShellInstallers walks the curated installer scripts and records the
// tool-download hosts they reach. A missing/unreadable required script is a
// sourceError (the gate should notice a source dropped out), not a soft warning.
func extractShellInstallers(repoRoot string) extractResult {
	var recs []Record
	var warnings, sourceErrors []string
	for _, f := range shellFiles {
		path := filepath.Join(repoRoot, f)
		data, err := os.ReadFile(path) //nolint:gosec // fixed in-repo path
		if err != nil {
			sourceErrors = append(sourceErrors, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		src := rel(repoRoot, path)
		r, w, se := scanShellData(src, data)
		recs = append(recs, r...)
		warnings = append(warnings, w...)
		sourceErrors = append(sourceErrors, se...)
	}
	return extractResult{records: recs, warnings: warnings, sourceErrors: sourceErrors}
}

// scanShellData classifies every line of an installer script. A scanner failure
// (e.g. an oversized line exceeding the 1 MiB token limit) is a sourceError, not
// a soft warning — it means records were dropped, and the gate must not stay
// green on incomplete source data.
func scanShellData(src string, data []byte) (recs []Record, warnings, sourceErrors []string) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		r, w := shellLineRecords(src, sc.Text())
		recs = append(recs, r...)
		warnings = append(warnings, w...)
	}
	if err := sc.Err(); err != nil {
		sourceErrors = append(sourceErrors, fmt.Sprintf("%s: scan: %v", src, err))
	}
	return recs, warnings, sourceErrors
}

// shSegRe splits a shell line on command separators so each command is
// classified on its own. `;` is intentionally excluded — it frequently appears
// inside quoted advice strings (`"a; b"`) and splitting on it would mine them.
var shSegRe = regexp.MustCompile(`&&|\|\||\|`)

// shellLineRecords classifies a single shell line by splitting it into
// command segments and processing each. A logger-position segment is skipped
// (advice/output), but a download *later* in the line (e.g.
// `log_info "x" && curl …`) is still processed — a logger prefix no longer
// masks the rest of the line.
func shellLineRecords(src, line string) (recs []Record, warnings []string) {
	if trim := strings.TrimSpace(line); trim == "" || strings.HasPrefix(trim, "#") {
		return nil, nil
	}
	for _, seg := range shSegRe.Split(line, -1) {
		st := strings.TrimSpace(seg)
		if st == "" || shLoggerStartRe.MatchString(st) {
			continue
		}
		r, w := shellSegmentRecords(src, seg, st)
		recs = append(recs, r...)
		warnings = append(warnings, w...)
	}
	return recs, warnings
}

// shellSegmentRecords classifies one command segment.
func shellSegmentRecords(src, line, trim string) (recs []Record, warnings []string) {
	if m := shGoInstallRe.FindStringSubmatch(line); m != nil {
		ref := strings.Trim(m[2], `"'`)
		pt := goInstallPin(m[2])
		// `go install` contacts both the module proxy and the checksum db.
		recs = append(recs,
			Record{
				Host: hostGoProxy, PackageType: PkgGoModule, Direction: DirPull,
				Consumer: ConsumerCIRunner, PinType: pt, Pin: cleanVar(ref), Detail: m[1], Source: src,
			},
			Record{
				Host: hostGoSum, PackageType: PkgGoModule, Direction: DirPull,
				Consumer: ConsumerCIRunner, PinType: pt, Pin: cleanVar(ref), Detail: m[1], Source: src,
			})
	}
	if m := shPipRe.FindStringSubmatch(line); m != nil {
		for _, pkg := range installTokens(m[1]) {
			name, spec, _ := strings.Cut(pkg, "==")
			if spec != "" {
				spec = "==" + spec
			}
			pt, pin := pipPin(spec)
			recs = append(recs, Record{
				Host: hostPyPI, PackageType: PkgPyPI, Direction: DirPull,
				Consumer: ConsumerCIRunner, PinType: pt, Pin: pin, Detail: name, Source: src,
			})
		}
	}
	if m := shAptRe.FindStringSubmatch(line); m != nil {
		for _, pkg := range installTokens(m[1]) {
			recs = append(recs, Record{
				Host: hostApt, PackageType: PkgApt, Direction: DirPull,
				Consumer: ConsumerCIRunner, PinType: PinNone, Detail: pkg, Source: src,
			})
		}
	}
	if m := shBrewRe.FindStringSubmatch(line); m != nil {
		for _, pkg := range installTokens(m[1]) {
			recs = append(recs, Record{
				Host: hostHomebrew, PackageType: PkgBrew, Direction: DirPull,
				Consumer: ConsumerCIRunner, PinType: PinNone, Detail: pkg, Source: src,
			})
		}
	}

	for _, u := range shURLRe.FindAllString(line, -1) {
		host := urlHost(u)
		if host == "" {
			continue
		}
		pkg := PkgBinaryRelease
		if strings.Contains(u, "install.sh") || strings.HasSuffix(u, ".sh") {
			pkg = PkgInstallScript
		}
		recs = append(recs, Record{
			Host: host, PackageType: pkg, Direction: DirPull,
			Consumer: ConsumerCIRunner, PinType: shellURLPin(u), Detail: u, Source: src,
		})
	}

	// Honesty: a download command that yielded no literal URL almost always
	// built the URL from a shell variable — surface it rather than drop it.
	if len(recs) == 0 && shDownloadRe.MatchString(line) {
		warnings = append(warnings, fmt.Sprintf(
			"%s: unresolved download line (URL likely built from a shell var): %s", src, trim))
	}
	return recs, warnings
}

// installTokens splits the argument tail of an install command into package
// tokens: it truncates at the first shell control/redirect operator, drops
// flags, and strips wrapping quotes. This fixes both the trailing-token drop
// (`... 2>/dev/null \`) and the multi-package case (`install a b c`).
func installTokens(rest string) []string {
	// Truncate at shell separators / redirects / comments / line-continuation.
	for _, cut := range []string{"&&", "||", ";", "|", "#", " 2>", " &>", " 1>", " >", " <"} {
		if i := strings.Index(rest, cut); i >= 0 {
			rest = rest[:i]
		}
	}
	rest = strings.TrimRight(rest, "\\ \t")

	var pkgs []string
	for tok := range strings.FieldsSeq(rest) {
		if strings.HasPrefix(tok, "-") {
			continue // flag (-y, --user, ...)
		}
		tok = strings.Trim(tok, `"'`)
		if tok == "" {
			continue
		}
		pkgs = append(pkgs, tok)
	}
	return pkgs
}

// goInstallPin classifies a `go install module@ref` pin. Any `$` means the
// version is a shell variable (`@$TOOL_VERSION`, `@${V}`, `@$(cmd)`) — a mutable
// pin, not a tag.
func goInstallPin(ref string) string {
	if strings.Contains(ref, "$") {
		return PinVar
	}
	r := strings.Trim(ref, `"'`)
	switch {
	case r == "latest":
		return PinLatest
	case isSHA40(r):
		return PinSHA
	default:
		return PinTag
	}
}

// pipPin classifies a pip version spec (the `==...` capture, possibly empty).
func pipPin(spec string) (pinType, pin string) {
	if spec == "" {
		return PinNone, ""
	}
	v := strings.TrimPrefix(spec, "==")
	if strings.Contains(v, "${") || strings.Contains(v, "$(") {
		return PinVar, ""
	}
	return PinTag, v
}

// shellURLPin classifies a download URL's pin. A literal /main//master/ segment
// is a genuine floating ref; a ${VAR} in the path is pinned-via-var; a literal
// version segment is a tag.
func shellURLPin(u string) string {
	switch {
	case strings.Contains(u, "/main/") || strings.Contains(u, "/master/"):
		return PinBranch
	case strings.Contains(u, "${") || strings.Contains(u, "$("):
		return PinVar
	case shVersionSeg.MatchString(u):
		return PinTag
	default:
		return PinNone
	}
}

// cleanVar trims wrapping quotes for readability in the Detail/Pin fields while
// preserving the ${VAR} tokens (they document the settings key in play).
func cleanVar(s string) string {
	return strings.Trim(s, `"'`)
}
