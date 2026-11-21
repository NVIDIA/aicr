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

// module-rename rewrites a Go module path across the tracked files of a
// checkout. It rewrites only unambiguous Go package paths and then reports every
// remaining mention of the old path that is not a repository URL, so a missed
// reference fails loudly instead of quietly matching nothing. With -go-only it
// rewrites go.mod and Go files alone; tools/api-diff uses that mode to align a
// pre-rename release with the current module path. With -check it writes
// nothing and reports every line that still names the old path, which is how
// make lint keeps the old path from coming back.
//
// Usage: module-rename -from OLD -to NEW [-dir DIR] [-go-only] [-skip FILE]
//
//	module-rename -check -from OLD [-dir DIR] [-skip FILE]
//
// Exit status: 0 success, 1 error, 2 leftovers found (the rewrite is still
// written). Leftovers print to stdout as "path:line: text".
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	exitOK = iota
	exitError
	exitLeftovers
)

// gitTimeout bounds "git ls-files" on a large checkout.
const gitTimeout = 2 * time.Minute

// defaultSkip is relative to -dir; a missing default list means skip nothing.
const defaultSkip = "tools/module-rename/skip.txt"

type options struct {
	dir, from, to, skip string
	goOnly, check       bool
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "module-rename: %v\n", err)
		os.Exit(exitError)
	}
	os.Exit(run(context.Background(), o, os.Stdout, os.Stderr))
}

func parseFlags(args []string) (options, error) {
	var o options
	flags := flag.NewFlagSet("module-rename", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&o.dir, "dir", ".", "checkout to rewrite")
	flags.StringVar(&o.from, "from", "", "module path to replace")
	flags.StringVar(&o.to, "to", "", "replacement module path")
	flags.StringVar(&o.skip, "skip", "", "skip list (default <dir>/"+defaultSkip+")")
	flags.BoolVar(&o.goOnly, "go-only", false, "rewrite only go.mod and Go files, without the leftover check")
	flags.BoolVar(&o.check, "check", false, "write nothing; report every line that still names -from")
	if err := flags.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	switch {
	case o.check && o.goOnly:
		return options{}, errors.New("-check and -go-only are mutually exclusive")
	case o.from == "":
		return options{}, errors.New("-from is required")
	case o.to == "" && !o.check:
		return options{}, errors.New("-to is required unless -check is set")
	}
	return o, nil
}

func run(ctx context.Context, o options, stdout, stderr io.Writer) int {
	found, err := rename(ctx, o, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "module-rename: %v\n", err)
		return exitError
	}
	if found {
		return exitLeftovers
	}
	return exitOK
}

func rename(ctx context.Context, o options, stdout, stderr io.Writer) (bool, error) {
	if o.from == o.to {
		return false, fmt.Errorf("-from and -to are both %q", o.from)
	}
	files, err := trackedFiles(ctx, o.dir)
	if err != nil {
		return false, err
	}
	var skip []string
	if !o.goOnly {
		if skip, err = loadSkip(o); err != nil {
			return false, err
		}
	}
	goDirs := goTopDirs(files)
	occurrences, changed, flagged := 0, 0, 0
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("rename canceled: %w", err)
		}
		if o.goOnly && !isGoSource(name) || !o.goOnly && skipped(name, skip) {
			continue
		}
		n, content, err := rewriteFile(o.dir, name, o, goDirs, stderr)
		if err != nil {
			return false, err
		}
		if n > 0 {
			occurrences += n
			changed++
		}
		if o.goOnly || content == nil {
			continue
		}
		lines := bytes.Split(content, []byte("\n"))
		for _, line := range leftovers(content, o.from) {
			flagged++
			fmt.Fprintf(stdout, "%s:%d: %s\n", name, line, strings.TrimRight(string(lines[line-1]), "\r"))
		}
	}
	if o.check {
		fmt.Fprintf(stderr, "found %d lines naming %s\n", flagged, o.from)
	} else {
		fmt.Fprintf(stderr, "rewrote %d occurrences in %d files\n", occurrences, changed)
	}
	return flagged > 0, nil
}

func isGoSource(name string) bool {
	return filepath.Base(name) == "go.mod" || strings.HasSuffix(name, ".go")
}

// rewriteFile returns the replacement count and the file's resulting content,
// or nil content for a file that is not a regular text file. name must stay
// inside dir.
func rewriteFile(dir, name string, o options, goDirs []string, stderr io.Writer) (int, []byte, error) {
	if !filepath.IsLocal(name) {
		return 0, nil, fmt.Errorf("refusing path outside the checkout: %s", name)
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("stat %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return 0, nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, fmt.Errorf("read %s: %w", name, err)
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return 0, nil, nil
	}
	if o.check {
		return 0, content, nil
	}
	updated, n := rewrite(content, o.from, o.to, goDirs)
	if n == 0 {
		return 0, content, nil
	}
	if strings.HasSuffix(name, ".go") {
		updated = formatGo(name, content, updated, stderr)
	}
	if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil { //nolint:gosec // G703 -- name passed filepath.IsLocal and is a regular file, not a symlink
		return 0, nil, fmt.Errorf("write %s: %w", name, err)
	}
	return n, updated, nil
}

// formatGo re-sorts the imports of a rewritten Go file only when the original
// was already gofmt-clean, so the rename never carries unrelated formatting
// changes (line endings, final newline, spacing). Any other file keeps the
// plain path swap and gets a warning.
func formatGo(name string, original, updated []byte, stderr io.Writer) []byte {
	canonical, err := format.Source(original)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "warning: %s: not formatted: %v\n", name, err)
		return updated
	case !bytes.Equal(canonical, original):
		fmt.Fprintf(stderr, "warning: %s: not formatted: source was not gofmt-clean\n", name)
		return updated
	}
	formatted, err := format.Source(updated)
	if err != nil {
		fmt.Fprintf(stderr, "warning: %s: not formatted: %v\n", name, err)
		return updated
	}
	return formatted
}

func trackedFiles(ctx context.Context, dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "ls-files", "-z").Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git ls-files in %s: %s", dir, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("git ls-files in %s: %w", dir, err)
	}
	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

func loadSkip(o options) ([]string, error) {
	path := o.skip
	if path == "" {
		path = filepath.Join(o.dir, defaultSkip)
	}
	f, err := os.Open(path)
	if o.skip == "" && errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open skip list: %w", err)
	}
	defer f.Close()
	return parseSkip(f)
}
