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

// Command image-unpack unpacks one platform of a release image from a local
// OCI layout into a directory tree that file scanners can read.
//
// Each layer is unpacked into its own directory, so a file that a later layer
// deletes is still on disk to scan. The raw index, manifest and image config
// (environment, labels, build history) are written beside the layers. Secret
// scanners skip binary files, so each executable built from the AICR module
// also gets a file of its printable strings.
//
// Every blob is checked against its digest, every layer's uncompressed stream
// against the config's diff_ids, and the layout's index against the expected
// digest, so the tree is exactly the bytes being released. A coverage report
// records what was unpacked and what was not, so a scan summary can state its
// scope.
//
// Usage:
//
//	image-unpack -layout dist/candidate/image -platform linux/amd64 \
//	  -index sha256:<64 hex> -out scan/linux-amd64 \
//	  -coverage scan/linux-amd64.coverage.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"aicr.run/pkg/errors"
)

// options is the parsed command line.
type options struct {
	layout   string
	platform string
	index    string
	out      string
	coverage string
}

func main() {
	var o options
	flag.StringVar(&o.layout, "layout", "", "OCI image layout holding one image index (required)")
	flag.StringVar(&o.platform, "platform", "", "platform to unpack, os/arch[/variant] (required)")
	flag.StringVar(&o.index, "index", "", "digest the layout's image index must have, sha256:<64 hex> (required)")
	flag.StringVar(&o.out, "out", "", "directory to unpack into; must not exist (required)")
	flag.StringVar(&o.coverage, "coverage", "", "path to write the coverage report, outside -out (required)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, o, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "image-unpack: %v\n", err)
		os.Exit(1)
	}
}

// run unpacks the image and writes the coverage report. It prints a one-line
// summary so the release log records what each scan covers.
func run(ctx context.Context, o options, stdout io.Writer) error {
	for _, required := range []struct{ flag, value string }{
		{"-layout", o.layout}, {"-platform", o.platform}, {"-index", o.index},
		{"-out", o.out}, {"-coverage", o.coverage},
	} {
		if required.value == "" {
			return errors.New(errors.ErrCodeInvalidRequest, required.flag+" is required")
		}
	}
	// The scanners read everything under -out, so a report written there would
	// be scanned as image content. The paths are compared as written and as
	// the filesystem resolves them, so a symlink cannot route the report there.
	if inside, err := within(o.out, o.coverage); err != nil || inside {
		return errors.New(errors.ErrCodeInvalidRequest, "-coverage must be outside -out")
	}
	realOut, err := resolve(o.out)
	if err != nil {
		return err
	}
	realCoverage, err := resolve(o.coverage)
	if err != nil {
		return err
	}
	inside, err := within(realOut, realCoverage)
	if err != nil || inside {
		return errors.New(errors.ErrCodeInvalidRequest, "-coverage must be outside -out")
	}
	coverage, err := Unpack(ctx, Options{Layout: o.layout, Platform: o.platform, Index: o.index, Out: o.out})
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(coverage, "", "  ")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to encode the coverage report", err)
	}
	if err = os.WriteFile(o.coverage, append(data, '\n'), 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to write %s", o.coverage), err)
	}
	t := coverage.Totals
	fmt.Fprintf(stdout, "unpacked %s %s: %d layers, %d files, %d AICR executables, %d other binaries\n",
		coverage.Platform, coverage.Manifest, len(coverage.Layers), t.Files, t.AICRExecutables,
		t.Binaries-t.AICRExecutables)
	return nil
}

// resolve returns p with the symlinks of its directory resolved. That
// directory must exist; p itself may not exist yet, but must not be a symlink.
// p is never cleaned: the kernel applies ".." after following a symlink, and
// cleaning would apply it before, so the checked path could differ from the
// one the caller opens.
func resolve(p string) (string, error) {
	trimmed := strings.TrimRight(p, string(filepath.Separator))
	parent, base := ".", trimmed
	if i := strings.LastIndexByte(trimmed, filepath.Separator); i >= 0 {
		parent, base = trimmed[:i], trimmed[i+1:]
		if parent == "" {
			parent = string(filepath.Separator)
		}
	}
	if base == "" || base == "." || base == ".." {
		return "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("%s must end in a file or directory name", p))
	}
	if info, err := os.Lstat(trimmed); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("%s must not be a symlink", p))
	}
	dir, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to resolve the directory of %s", p), err)
	}
	return filepath.Join(dir, base), nil
}

// within reports whether path is dir or lies under it.
func within(dir, path string) (bool, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false, err
	}
	return rel == "." || filepath.IsLocal(rel), nil
}
