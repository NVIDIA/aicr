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

// Command check-docs-links verifies that every link on a page Fern publishes
// reaches something a reader can open. It exits 1 when a link does not, and 2
// when the published pages or a link target cannot be read.
//
// The published pages are the paths in docs/index.yml, Fern's navigation
// manifest. A relative link must resolve to one of them, because Fern renders
// a link to any other file as a site URL that serves "Page Not Found" with
// HTTP 200, so neither the file existing nor an online status check catches
// it. A https://github.com/NVIDIA/aicr/blob/main/<path> URL must name a path
// that exists in the repository.
//
// Usage:
//
//	go run ./tools/check-docs-links
package main

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const navPath = "docs/index.yml"

// maxFileBytes bounds every page and nav read, far above the largest page.
const maxFileBytes = 8 << 20

func main() {
	violations, err := run(context.Background(), ".", os.Stdout)
	if err != nil {
		slog.Error("docs link check could not run", "error", err)
		os.Exit(2)
	}
	if violations > 0 {
		os.Exit(1)
	}
}

// linkKind is how a link is checked. Only relative links and links to this
// repository's main branch on GitHub are checked.
type linkKind int

const (
	kindIgnored linkKind = iota
	kindRelative
	kindGitHub
)

// link is one link destination and the 1-based source line it appears on.
type link struct {
	url  string
	line int
}

// site holds the repository root and its published pages, as slash-separated
// paths relative to that root.
type site struct {
	root      string
	published map[string]bool
}

// exists reports whether repoPath names a file or directory inside the root.
// A path through a file fails with ENOTDIR and counts as missing. Any other
// stat failure is returned, so it is not reported as a broken link.
func (s site) exists(repoPath string) (bool, error) {
	if !filepath.IsLocal(filepath.FromSlash(repoPath)) {
		return false, nil
	}
	_, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(repoPath)))
	switch {
	case err == nil:
		return true, nil
	case stderrors.Is(err, fs.ErrNotExist), stderrors.Is(err, syscall.ENOTDIR):
		return false, nil
	default:
		return false, errors.Wrap(errors.ErrCodeInternal, "stat "+repoPath, err)
	}
}

// run checks every published page under root, writes one line per unreachable
// link and a summary to out, and returns the number of unreachable links.
func run(ctx context.Context, root string, out io.Writer) (int, error) {
	nav, err := readBounded(filepath.Join(root, filepath.FromSlash(navPath)))
	if err != nil {
		return 0, err
	}
	pages, err := publishedPages(nav)
	if err != nil {
		return 0, err
	}
	s := site{root: root, published: make(map[string]bool, len(pages))}
	for _, p := range pages {
		s.published[p] = true
	}

	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	var relative, github, violations int
	for _, page := range pages {
		if err := ctx.Err(); err != nil {
			return 0, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "docs link check interrupted")
		}
		src, err := readBounded(filepath.Join(root, filepath.FromSlash(page)))
		if err != nil {
			return 0, errors.Wrap(errors.ErrCodeNotFound, navPath+" references "+page+", which cannot be read", err)
		}
		for _, l := range extractLinks(md, src) {
			kind, reason, err := s.classify(page, l.url)
			if err != nil {
				return 0, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("check link %s on %s:%d", l.url, page, l.line), err)
			}
			switch kind {
			case kindRelative:
				relative++
			case kindGitHub:
				github++
			case kindIgnored:
				continue
			}
			if reason != "" {
				fmt.Fprintf(out, "DOC-LINK: %s:%d: %s: %s\n", page, l.line, l.url, reason)
				violations++
			}
		}
	}

	if relative == 0 {
		return 0, errors.New(errors.ErrCodeNotFound,
			"no relative links were found on any published page, so the link check verified nothing")
	}
	if violations > 0 {
		fmt.Fprintf(out, "\nERROR: %d link(s) on published pages do not reach a published page or a repository path.\n", violations)
		fmt.Fprintf(out, "A relative link must name a page listed in %s. For a file Fern does\n", navPath)
		fmt.Fprintln(out, "not publish, link https://github.com/NVIDIA/aicr/blob/main/<path> instead.")
		return violations, nil
	}
	fmt.Fprintf(out, "OK: %d relative link(s) and %d GitHub main link(s) on %d published page(s) resolve\n",
		relative, github, len(pages))
	return 0, nil
}

// readBounded reads name, failing rather than allocating past maxFileBytes.
func readBounded(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "open "+name, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read "+name, err)
	}
	if len(data) > maxFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("%s exceeds %d bytes", name, maxFileBytes))
	}
	return data, nil
}

// publishedPages returns the sorted, repo-relative path of every page nav
// lists. Pages nest under sections at any depth, so every mapping with a
// path key counts.
func publishedPages(nav []byte) ([]string, error) {
	var doc any
	if err := yaml.Unmarshal(nav, &doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse "+navPath, err)
	}
	var pages []string
	var walk func(v any) error
	walk = func(v any) error {
		switch v := v.(type) {
		case map[string]any:
			if p, ok := v["path"]; ok {
				s, isString := p.(string)
				if !isString || s == "" {
					return errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("%s has a path that is not a non-empty string: %v", navPath, p))
				}
				pages = append(pages, path.Join("docs", s))
			}
			for _, child := range v {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range v {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(doc); err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, errors.New(errors.ErrCodeNotFound, "no page paths found in "+navPath)
	}
	slices.Sort(pages)
	return slices.Compact(pages), nil
}

var (
	schemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	githubMain    = regexp.MustCompile(`(?i)^https?://(?:www\.)?github\.com/nvidia/aicr/blob/main`)
)

// classify reports how rawURL on page is checked and, when it reaches nothing
// a reader can open, why. An empty reason means the link resolves. An error
// means a target could not be checked.
func (s site) classify(page, rawURL string) (linkKind, string, error) {
	if repoPath, reason, ok := githubMainPath(rawURL); ok {
		if reason != "" {
			return kindGitHub, reason, nil
		}
		found, err := s.exists(repoPath)
		if err != nil {
			return kindGitHub, "", err
		}
		if !found {
			reason = fmt.Sprintf("names %s, which does not exist in the repository", repoPath)
		}
		return kindGitHub, reason, nil
	}
	if schemePattern.MatchString(rawURL) || strings.HasPrefix(rawURL, "//") {
		return kindIgnored, "", nil
	}

	target := rawURL
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return kindIgnored, "", nil
	}
	if strings.HasPrefix(target, "/") {
		return kindRelative, "a root-relative link cannot be resolved offline. Link to the page by relative path", nil
	}
	decoded, err := url.PathUnescape(target)
	if err != nil {
		return kindRelative, "malformed percent-encoding", nil
	}
	resolved := path.Join(path.Dir(page), decoded)
	if s.published[resolved] {
		return kindRelative, "", nil
	}
	found, err := s.exists(resolved)
	if err != nil {
		return kindRelative, "", err
	}
	if found {
		return kindRelative, fmt.Sprintf("resolves to %s, which %s does not publish", resolved, navPath), nil
	}
	return kindRelative, fmt.Sprintf("resolves to %s, which does not exist", resolved), nil
}

// githubMainPath reports whether rawURL is a blob/main URL into this
// repository and, if so, the repository path it names, with a reason when
// that path is malformed or not canonical.
func githubMainPath(rawURL string) (repoPath, reason string, ok bool) {
	prefix := githubMain.FindString(rawURL)
	if prefix == "" {
		return "", "", false
	}
	rest := rawURL[len(prefix):]
	if rest != "" && !strings.ContainsAny(rest[:1], "/?#") {
		return "", "", false
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return rest, "malformed percent-encoding", true
	}
	repoPath = strings.TrimSuffix(strings.TrimPrefix(decoded, "/"), "/")
	if repoPath == "" {
		return repoPath, "does not name a repository path", true
	}
	if path.Clean(repoPath) != repoPath {
		return repoPath, fmt.Sprintf("names %s, which is not a canonical repository path", repoPath), true
	}
	return repoPath, "", true
}

// extractLinks returns every Markdown link destination in src outside code,
// covering inline links, reference definitions, autolinks, and bare URLs. A
// reference link is covered by its definition.
func extractLinks(md goldmark.Markdown, src []byte) []link {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	lineAt := func(offset int) int {
		if offset < 0 {
			return 1
		}
		i, found := slices.BinarySearch(starts, offset)
		if found {
			i++
		}
		return i
	}
	var links []link
	doc := md.Parser().Parse(text.NewReader(src))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link:
			if n.Reference == nil {
				links = append(links, link{url: string(n.Destination), line: lineAt(n.Pos())})
			}
		case *ast.LinkReferenceDefinition:
			links = append(links, link{url: string(n.Destination), line: lineAt(n.Pos())})
		case *ast.AutoLink:
			if n.AutoLinkType != ast.AutoLinkEmail {
				links = append(links, link{url: string(n.URL(src)), line: lineAt(n.Pos())})
			}
		}
		return ast.WalkContinue, nil
	})
	return links
}
