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
	"bufio"
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
)

// keepMarker exempts a line from the leftover check.
const keepMarker = "module-rename:keep"

// webRoutes are GitHub page routes; a path followed by one of them is a
// repository URL, not a Go package path.
var webRoutes = []string{
	"blob", "tree", "releases", "issues", "pull", "actions", "security",
	"discussions", "wiki", "commit", "compare", "raw", "archive",
	"attestations", "pkgs",
}

// rewrite replaces from with to wherever from is unambiguously a Go package
// path: followed by "/<dir>" for a top-level directory holding Go code, by a
// closing quote, or as the whole tail of a "module" directive line. Any other
// form is left for leftovers to report.
func rewrite(content []byte, from, to string, goDirs []string) ([]byte, int) {
	var out bytes.Buffer
	count, last := 0, 0
	for i := indexFrom(content, from, 0); i >= 0; i = indexFrom(content, from, i+len(from)) {
		if !precededByBoundary(content, i) || !isPackagePath(content, i, len(from), goDirs) {
			continue
		}
		out.Write(content[last:i])
		out.WriteString(to)
		last = i + len(from)
		count++
	}
	if count == 0 {
		return content, 0
	}
	out.Write(content[last:])
	return out.Bytes(), count
}

// leftovers returns the 1-based numbers of lines that still name from in a
// form that is neither a repository URL nor marked with keepMarker.
func leftovers(content []byte, from string) []int {
	var lines []int
	for n, line := range bytes.Split(content, []byte("\n")) {
		if bytes.Contains(line, []byte(keepMarker)) {
			continue
		}
		for i := indexFrom(line, from, 0); i >= 0; i = indexFrom(line, from, i+len(from)) {
			if isLeftover(line, i, len(from)) {
				lines = append(lines, n+1)
				break
			}
		}
	}
	return lines
}

// goTopDirs returns the sorted top-level directories that contain Go files.
func goTopDirs(paths []string) []string {
	var dirs []string
	for _, p := range paths {
		top, rest, nested := strings.Cut(p, "/")
		if nested && strings.HasSuffix(rest, ".go") && !slices.Contains(dirs, top) {
			dirs = append(dirs, top)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// parseSkip reads one repository-relative path per line; "#" starts a comment.
// An entry ending in "/" matches every path under it.
func parseSkip(r io.Reader) ([]string, error) {
	var entries []string
	scanner := bufio.NewScanner(r)
	for n := 1; scanner.Scan(); n++ {
		line, _, _ := strings.Cut(scanner.Text(), "#")
		fields := strings.Fields(line)
		switch len(fields) {
		case 0:
		case 1:
			entries = append(entries, fields[0])
		default:
			return nil, fmt.Errorf("skip list line %d: want one path, got %q", n, strings.TrimSpace(line))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read skip list: %w", err)
	}
	return entries, nil
}

func skipped(path string, skip []string) bool {
	for _, entry := range skip {
		if strings.HasSuffix(entry, "/") && strings.HasPrefix(path, entry) || path == entry {
			return true
		}
	}
	return false
}

func indexFrom(content []byte, from string, start int) int {
	if start >= len(content) {
		return -1
	}
	i := bytes.Index(content[start:], []byte(from))
	if i < 0 {
		return -1
	}
	return start + i
}

func isWordByte(b byte) bool {
	return b == '_' || b == '-' || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// precededByBoundary rejects matches inside a URL, another path, an email-like
// token, or a longer word. A key=value pair is allowed: the value is matched
// only when a Go directory follows, which a URL query carrying the repository
// path does not have.
func precededByBoundary(content []byte, i int) bool {
	if i == 0 {
		return true
	}
	prev := content[i-1]
	return !isWordByte(prev) && !strings.ContainsRune("/.@", rune(prev))
}

func isPackagePath(content []byte, i, n int, goDirs []string) bool {
	end := i + n
	if end == len(content) || content[end] == '\r' || content[end] == '\n' || content[end] == ' ' || content[end] == '\t' {
		return isModuleDirective(content, i, end)
	}
	switch content[end] {
	case '"':
		return true
	case '/':
		return slices.Contains(goDirs, string(segmentAt(content, end+1)))
	}
	return false
}

// isModuleDirective reports whether content[i:end] is the whole argument of a
// "module" line, allowing surrounding whitespace.
func isModuleDirective(content []byte, i, end int) bool {
	lineStart := bytes.LastIndexByte(content[:i], '\n') + 1
	head := strings.TrimLeft(string(content[lineStart:i]), " \t")
	keyword, gap, ok := strings.Cut(head, "module")
	if !ok || keyword != "" || gap == "" || strings.TrimLeft(gap, " \t") != "" {
		return false
	}
	lineEnd := bytes.IndexByte(content[end:], '\n')
	if lineEnd < 0 {
		lineEnd = len(content) - end
	}
	return strings.TrimRight(string(content[end:end+lineEnd]), " \t\r") == ""
}

func segmentAt(content []byte, start int) []byte {
	end := start
	for end < len(content) && isWordByte(content[end]) {
		end++
	}
	return content[start:end]
}

func isLeftover(line []byte, i, n int) bool {
	if i >= 3 && string(line[i-3:i]) == "://" {
		return false
	}
	rest := line[i+n:]
	switch {
	case len(rest) > 0 && isWordByte(rest[0]):
		return false
	case bytes.HasPrefix(rest, []byte(".git")) && (len(rest) == 4 || !isWordByte(rest[4])):
		return false
	case bytes.HasPrefix(rest, []byte("/.github")):
		return false
	case len(rest) > 0 && rest[0] == '/':
		return !slices.Contains(webRoutes, string(segmentAt(rest, 1)))
	}
	return true
}
