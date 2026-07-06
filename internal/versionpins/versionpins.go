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

// Package versionpins loads the repository's declarative version-pin
// exemption table (recipes/version-pin-exemptions.yaml): componentRefs whose
// overlay/mixin version pin is INTENTIONALLY different from the component's
// registry defaultVersion/defaultTag.
//
// The file is repository-only policy data — deliberately NOT embedded into
// the aicr binary (see recipes/data.go) and not part of the runtime recipe
// data model, which is why this package lives under internal/ rather than
// pkg/: it is dev/CI tooling, not public Go API. It is consumed from a
// repository checkout by the version-pin guard test
// (pkg/recipe/version_pin_guard_test.go) and, once #1611 wires it, by the
// BOM generator (tools/bom) — which must read this policy and
// recipes/registry.yaml from the same checkout (tools/bom -repo-root). The
// single shared file keeps the guard and the BOM from drifting on which
// divergences are blessed.
//
// Load fails closed: an unreadable, oversized, mis-identified, or
// semantically invalid file is an error, never an empty exemption list — a
// malformed policy file must not silently un-bless every divergence (which
// would fail the guard) or, worse, silently bless none for a consumer that
// treats absence as "no variants" (tools/bom).
package versionpins

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
)

// FileName is the exemption file's name under the repository's recipes/
// directory.
const FileName = "version-pin-exemptions.yaml"

// expectedKind is the document identity Load requires (alongside
// header.GroupVersion for apiVersion), so an unrelated YAML file passed by
// mistake cannot masquerade as the exemption table.
const expectedKind = "VersionPinExemptions"

// Exemption documents a componentRef whose overlay/mixin version pin is
// INTENTIONALLY different from the component's registry defaultVersion.
//
// ExpectedPin/ExpectedDefault bind the exemption to ONE specific divergence:
// if either the recipe's pin or the registry default later moves, the guard
// test fails so the divergence is re-reviewed and re-justified rather than a
// new, unvetted version silently inheriting the old blessing.
type Exemption struct {
	// Source is the overlay/mixin metadata.name that declares the pin.
	Source string `yaml:"source"`
	// Component is the componentRef name.
	Component string `yaml:"component"`
	// ExpectedPin is the exact divergent version/tag this exemption blesses.
	ExpectedPin string `yaml:"expectedPin"`
	// ExpectedDefault is the registry default at the time the exemption was
	// written.
	ExpectedDefault string `yaml:"expectedDefault"`
	// Reason records why the divergence is intentional (cite an issue/PR).
	Reason string `yaml:"reason"`
}

// document is the on-disk file shape. Exemptions is a pointer so an omitted
// (or explicit null) key is distinguishable from a declared empty list: a
// truncated file must fail closed, not read as "no exemptions".
type document struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Exemptions *[]Exemption `yaml:"exemptions"`
}

// Test seams for openRegularBounded's blocking syscalls, so tests can
// deterministically simulate a hung open(2)/fstat(2) without a pathological
// filesystem. Production code never reassigns them; tests swap and restore
// them around a Load call (no test runs Load concurrently with a swap).
var (
	osOpen   = os.Open
	fileStat = (*os.File).Stat
)

// Load reads and validates the exemption file at path, typically
// filepath.Join(repoRoot, "recipes", FileName). Open, the regular-file check
// (fstat on the opened handle — no stat-to-open race), and the read all run
// inside context-bounded workers under defaults.FileReadTimeout (tightening
// any caller deadline), so a FIFO blocking in open(2) or a hung network/FUSE
// mount cannot stall the caller past the deadline. Size is bounded by
// defaults.MaxVersionPinExemptionsBytes.
//
// An explicitly declared empty list (`exemptions: []`) is valid — every pin
// matches its registry default. Every other defect — missing file, non-regular
// file, oversize, unknown fields, wrong apiVersion/kind, an omitted or null
// exemptions key, missing or whitespace-only entry fields, duplicate
// (source, component) keys, or an entry whose expectedPin equals
// expectedDefault — is an error.
func Load(ctx context.Context, path string) ([]Exemption, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()

	// Deterministic entry check: an already-expired context must fail here
	// rather than race the (possibly instant) open/read selects below.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout,
			"reading version-pin exemptions file "+path+" timed out", ctxErr)
	}

	// The helpers classify their own failures as structured errors, so both
	// propagate as-is (re-wrapping would overwrite their codes).
	f, err := openRegularBounded(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close() // read-only handle; Close error carries no data loss

	data, err := readAllBounded(ctx, f, defaults.MaxVersionPinExemptionsBytes+1, path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > defaults.MaxVersionPinExemptionsBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"version-pin exemptions file %s exceeds %d bytes",
			path, defaults.MaxVersionPinExemptionsBytes))
	}
	return parse(data, path)
}

// openRegularBounded opens path and verifies it is a regular file, entirely
// inside a context-bounded worker: a FIFO with no writer blocks in open(2)
// itself, and a hung network/FUSE mount can block open(2) or the fstat(2)
// after it — no post-open context check helps with any of those, so every
// potentially blocking call lives in the worker.
//
// Handle lifecycle: delivery over the unbuffered channel is a rendezvous, so
// the worker always learns whether the caller took the result. On every
// non-delivery path — open/fstat error, non-regular type, or the caller's
// deadline firing first — the worker closes the handle itself; there is no
// drainer goroutine to pin alongside it. If the underlying syscall never
// returns (a permanently hung mount), exactly one goroutine — this worker —
// stays pinned until it does; Go cannot cancel a blocked syscall, and
// unblocking the caller while bounding cleanup is the same trade
// readAllBounded (and pkg/serializer) makes for reads.
//
// Failures are classified here, once, as structured errors: ErrCodeNotFound
// (fs.ErrNotExist), ErrCodeInvalidRequest (non-regular file), ErrCodeTimeout
// (deadline), ErrCodeInternal (any other open/stat failure).
func openRegularBounded(ctx context.Context, path string) (*os.File, error) {
	type result struct {
		f   *os.File
		err error
	}
	// Capture the seams before spawning so the worker never reads the
	// package vars concurrently with a test swapping them.
	open, stat := osOpen, fileStat
	ch := make(chan result) // unbuffered: send is a rendezvous with the caller
	deliver := func(res result) {
		select {
		case ch <- res:
		case <-ctx.Done():
			// The caller is gone; it will never receive this result.
			if res.f != nil {
				_ = res.f.Close()
			}
		}
	}
	go func() {
		f, err := open(path)
		if err != nil {
			if stderrors.Is(err, fs.ErrNotExist) {
				deliver(result{err: errors.Wrap(errors.ErrCodeNotFound,
					"version-pin exemptions file not found at "+path, err)})
				return
			}
			deliver(result{err: errors.Wrap(errors.ErrCodeInternal,
				"failed to open version-pin exemptions file "+path, err)})
			return
		}
		info, err := stat(f)
		if err != nil {
			_ = f.Close()
			deliver(result{err: errors.Wrap(errors.ErrCodeInternal,
				"failed to stat version-pin exemptions file "+path, err)})
			return
		}
		if !info.Mode().IsRegular() {
			_ = f.Close()
			deliver(result{err: errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"version-pin exemptions file %s is not a regular file (mode %s)",
				path, info.Mode()))})
			return
		}
		deliver(result{f: f})
	}()
	select {
	case <-ctx.Done():
		return nil, errors.Wrap(errors.ErrCodeTimeout,
			"opening version-pin exemptions file "+path+" timed out", ctx.Err())
	case res := <-ch:
		return res.f, res.err
	}
}

// readAllBounded reads up to limit bytes from r, returning early if ctx is
// canceled or its deadline fires. The read runs in a goroutine so a hung
// filesystem read (network mount, FUSE) cannot stall the caller past the
// deadline: the caller is unblocked and the goroutine ends when the
// underlying Read eventually returns (the buffered channel lets its send
// always succeed; byte data needs no cleanup, so no rendezvous is required).
// Mirrors pkg/serializer.readAllBounded. Failures are classified here, once:
// ErrCodeTimeout (deadline), ErrCodeInternal (read failure).
func readAllBounded(ctx context.Context, r io.Reader, limit int64, path string) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1) // buffered so the goroutine never leaks on send
	go func() {
		data, err := io.ReadAll(io.LimitReader(r, limit))
		ch <- result{data: data, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, errors.Wrap(errors.ErrCodeTimeout,
			"reading version-pin exemptions file "+path+" timed out", ctx.Err())
	case res := <-ch:
		if res.err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"failed to read version-pin exemptions file "+path, res.err)
		}
		return res.data, nil
	}
}

// parse strictly decodes and validates the exemption document, aggregating
// every problem into one error so the author sees all defects at once.
func parse(data []byte, path string) ([]Exemption, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc document
	if err := dec.Decode(&doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to parse version-pin exemptions file "+path, err)
	}
	// A trailing second YAML document would be silently dropped by a single
	// Decode; reject it so no exemption can hide in an unread document.
	if err := dec.Decode(new(document)); !stderrors.Is(err, io.EOF) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"version-pin exemptions file "+path+" must contain exactly one YAML document")
	}

	var problems []string
	if doc.APIVersion != header.GroupVersion {
		problems = append(problems, fmt.Sprintf(
			"apiVersion %q, want %q", doc.APIVersion, header.GroupVersion))
	}
	if doc.Kind != expectedKind {
		problems = append(problems, fmt.Sprintf(
			"kind %q, want %q", doc.Kind, expectedKind))
	}
	if doc.Exemptions == nil {
		problems = append(problems, "exemptions key is missing or null; declare "+
			"an explicit empty list ([]) when no divergence is blessed — a "+
			"truncated file must not read as \"no exemptions\"")
	}

	var exemptions []Exemption
	if doc.Exemptions != nil {
		exemptions = *doc.Exemptions
	}
	type key struct{ source, component string }
	seen := make(map[key]struct{}, len(exemptions))
	for i, e := range exemptions {
		entry := fmt.Sprintf("exemptions[%d] (source=%q component=%q)",
			i, e.Source, e.Component)
		// Whitespace-only values are as useless as empty ones — reject both.
		if strings.TrimSpace(e.Source) == "" {
			problems = append(problems, entry+" has no source")
		}
		if strings.TrimSpace(e.Component) == "" {
			problems = append(problems, entry+" has no component")
		}
		if strings.TrimSpace(e.ExpectedPin) == "" || strings.TrimSpace(e.ExpectedDefault) == "" {
			problems = append(problems, entry+" must set both expectedPin and "+
				"expectedDefault so drift within the exemption is caught")
		}
		if strings.TrimSpace(e.ExpectedPin) != "" && e.ExpectedPin == e.ExpectedDefault {
			problems = append(problems, fmt.Sprintf(
				"%s has expectedPin == expectedDefault (%q); an exemption documents "+
					"a DIVERGENCE — delete it instead", entry, e.ExpectedPin))
		}
		if strings.TrimSpace(e.Reason) == "" {
			problems = append(problems, entry+" has no reason")
		}
		k := key{source: e.Source, component: e.Component}
		if _, dup := seen[k]; dup {
			problems = append(problems, entry+" duplicates an earlier entry")
		}
		seen[k] = struct{}{}
	}

	if len(problems) > 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"invalid version-pin exemptions file "+path+": "+strings.Join(problems, "; "))
	}
	return exemptions, nil
}
