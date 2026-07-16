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

// Command uat-broker is the day/night UAT broker helper (#1274, DC1). It
// reads the reservation registry (infra/uat/reservations.yaml) and expands
// the nightly version-matrix schedule. It holds no credentials and performs
// no network or git I/O — the calling workflow feeds it the registry path
// and the raw `git tag` list on stdin.
//
// Subcommands:
//
//	uat-broker reservations --name <name> [--file <registry>]
//	    Resolve one reservation row and print its fields as
//	    GITHUB_OUTPUT-style key=value lines (redirect to "$GITHUB_OUTPUT").
//
//	uat-broker reservations --list [--file <registry>]
//	    Print every reservation name, one per line.
//
//	uat-broker reservations --daytime [--file <registry>]
//	    Print the daytime human-access rotation (#1281, DC8) as a JSON array
//	    of {reservation, intent} — one entry per row with a non-empty
//	    daytime-intent — for the daytime scheduler's dispatch matrix.
//
//	uat-broker schedule [--file <registry>] [--reservations a,b] \
//	    [--previous-n N] [--include-main] < tags
//	    Read candidate tags from stdin and print the ordered nightly
//	    version-matrix schedule as JSON: main-first, then the previous N
//	    stable releases in descending semver order, per reservation.
package main

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/uatbroker"
)

// defaultRegistryPath is the registry location relative to the repo root,
// where the workflows invoke the tool.
const defaultRegistryPath = "infra/uat/reservations.yaml"

// maxTagsBytes bounds the stdin tag-list read so an oversized stream cannot
// OOM the process.
const maxTagsBytes int64 = 1 << 20 // 1 MiB

const usage = `uat-broker — UAT reservation registry + nightly schedule helper (#1274)

Usage:
  uat-broker reservations --name <name> [--file <registry>]   resolve one row as key=value lines
  uat-broker reservations --list [--file <registry>]          list reservation names
  uat-broker reservations --daytime [--file <registry>]       print the daytime rotation as JSON [{reservation,intent}]
  uat-broker schedule [--file <registry>] [--reservations a,b] [--previous-n N] [--include-main]
                                                              expand the nightly version matrix (tags on stdin) as JSON`

func main() {
	os.Exit(realMain())
}

// realMain runs the CLI and returns the process exit code. It is split from
// main so the signal-context teardown (defer stop()) runs before os.Exit,
// which would otherwise skip deferred functions.
func realMain() int {
	// Cancel on Ctrl-C / SIGTERM (CI cancellation) rather than imposing an
	// arbitrary deadline: the tool only does local file + stdin I/O.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return parseAndRun(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// parseAndRun dispatches the subcommand and returns a process exit code,
// preserving the coded pkg/errors exit-code contract (2=INVALID_REQUEST,
// 3=NOT_FOUND, etc.) rather than collapsing every failure into 1.
func parseAndRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return errors.ExitInvalidInput
	}

	sub, rest := args[0], args[1:]
	var err error
	switch sub {
	case "reservations":
		err = runReservations(rest, stdout, stderr)
	case "schedule":
		err = runSchedule(ctx, rest, stdin, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "uat-broker: unknown subcommand %q\n%s\n", sub, usage)
		return errors.ExitInvalidInput
	}

	if err != nil {
		fmt.Fprintln(stderr, "uat-broker:", err)
		return errors.ExitCodeFromError(err)
	}
	return 0
}

// runReservations resolves one reservation row (--name) into key=value lines
// or lists every reservation name (--list).
func runReservations(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("reservations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		file    string
		name    string
		list    bool
		daytime bool
	)
	fs.StringVar(&file, "file", defaultRegistryPath, "path to the reservation registry")
	fs.StringVar(&name, "name", "", "reservation name to resolve")
	fs.BoolVar(&list, "list", false, "list all reservation names, one per line")
	fs.BoolVar(&daytime, "daytime", false, "print the daytime human-access rotation as JSON: [{\"reservation\",\"intent\"}]")
	if err := fs.Parse(args); err != nil {
		return flagParseErr(err, "reservations")
	}
	if fs.NArg() != 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"reservations: unexpected positional arguments: "+strings.Join(fs.Args(), " "))
	}
	// The three output modes are mutually exclusive: --name resolves one row,
	// --list prints names, --daytime prints the daytime rotation.
	if selected := boolCount(list, daytime, name != ""); selected > 1 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"reservations: --name, --list, and --daytime are mutually exclusive")
	}

	reg, err := uatbroker.LoadRegistryFile(file)
	if err != nil {
		return err
	}

	// The daytime rotation is JSON (a dispatch matrix), written directly by the
	// encoder; the other two modes are line-oriented. Handle JSON first so its
	// output path is unambiguous.
	if daytime {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reg.DaytimeAssignments()); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "encode daytime assignments", err)
		}
		return nil
	}

	// Build the output first, then write it in one checked call so a broken
	// pipe or an unwritable $GITHUB_OUTPUT surfaces as a failure instead of a
	// silent exit-0 that leaves downstream jobs without their inputs. Writes to
	// the strings.Builder cannot fail.
	var b strings.Builder
	switch {
	case list:
		for _, n := range reg.Names() {
			fmt.Fprintln(&b, n)
		}
	case name == "":
		return errors.New(errors.ErrCodeInvalidRequest, "reservations: --name is required (or pass --list or --daytime)")
	default:
		res, lookupErr := reg.Lookup(name)
		if lookupErr != nil {
			return lookupErr
		}
		// GITHUB_OUTPUT-style key=value lines; every value is single-line.
		fmt.Fprintf(&b, "cloud=%s\n", res.Cloud)
		fmt.Fprintf(&b, "reservation-id=%s\n", res.ReservationID)
		fmt.Fprintf(&b, "accelerator=%s\n", res.Accelerator)
		fmt.Fprintf(&b, "gpu-count=%d\n", res.GPUCount)
		fmt.Fprintf(&b, "cluster-config-path=%s\n", res.ClusterConfigPath)
		fmt.Fprintf(&b, "test-config-dir=%s\n", res.TestConfigDir)
		// The nightly batch's intents for this reservation, comma-joined and
		// resolved (not raw) so an un-annotated reservation reports the training
		// default rather than an empty value the caller must re-default. The
		// nightly controller splits on comma and dispatches one cell per intent.
		fmt.Fprintf(&b, "nightly-intents=%s\n", strings.Join(res.NightlyIntentsOrDefault(), ","))
		// Empty when the reservation is not in the daytime rotation; callers
		// that don't consume this key simply ignore the line.
		fmt.Fprintf(&b, "daytime-intent=%s\n", res.DaytimeIntent)
	}

	if _, err := io.WriteString(stdout, b.String()); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "write reservations output", err)
	}
	return nil
}

// runSchedule reads candidate tags from stdin and prints the ordered nightly
// version-matrix schedule as JSON.
func runSchedule(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		file        string
		reservCSV   string
		previousN   int
		includeMain bool
	)
	fs.StringVar(&file, "file", defaultRegistryPath, "registry path (always loaded — supplies each cell's eligible nightly intents)")
	fs.StringVar(&reservCSV, "reservations", "", "comma-separated reservation names to schedule, each of which must exist in --file (default: every row)")
	fs.IntVar(&previousN, "previous-n", 2, "number of previous stable releases below main to include")
	fs.BoolVar(&includeMain, "include-main", true, "include the tip-of-main cell first")
	if err := fs.Parse(args); err != nil {
		return flagParseErr(err, "schedule")
	}
	if fs.NArg() != 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"schedule: unexpected positional arguments: "+strings.Join(fs.Args(), " "))
	}

	reservations, err := resolveReservations(file, reservCSV)
	if err != nil {
		return err
	}
	tags, err := readTags(ctx, stdin)
	if err != nil {
		return err
	}

	schedule := uatbroker.ExpandSchedule(reservations, tags, includeMain, previousN)

	// Warn loudly (but do not fail) when the matrix degrades, so a shallow
	// checkout with no tag history doesn't silently skip the release rows. The
	// two degraded states are distinct: main still runs, vs nothing runs.
	switch {
	case previousN > 0 && includeMain && scheduleHasNoReleases(schedule):
		fmt.Fprintln(stderr,
			"uat-broker: warning: no stable release tags on stdin — schedule is main-only "+
				"(shallow checkout? fetch full tag history)")
	case scheduleIsEmpty(schedule):
		fmt.Fprintln(stderr,
			"uat-broker: warning: empty schedule — --include-main=false and no stable release tags, nothing to run")
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(schedule); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "encode schedule", err)
	}
	return nil
}

// resolveReservations returns the reservation ROWS to schedule: the
// --reservations subset when given, otherwise every row in the registry. The
// registry is always loaded — the schedule attaches each cell's eligible
// nightly intents (nightly-intents gated by nightly-intent-min-versions),
// which only the registry row carries; a --reservations name absent from the
// registry therefore fails loudly rather than scheduling an intent-less row.
func resolveReservations(file, csv string) ([]uatbroker.Reservation, error) {
	reg, err := uatbroker.LoadRegistryFile(file)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(csv) == "" {
		return reg.Reservations, nil
	}
	names := splitCSV(csv)
	if len(names) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "schedule: --reservations is empty after trimming")
	}
	// Reject duplicates so the input fails loudly rather than silently
	// collapsing via the schedule's reservation-keyed output map — mirrors
	// the registry's own name-uniqueness invariant.
	seen := make(map[string]bool, len(names))
	out := make([]uatbroker.Reservation, 0, len(names))
	for _, n := range names {
		if seen[n] {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "schedule: duplicate reservation name "+n+" in --reservations")
		}
		seen[n] = true
		res, lookupErr := reg.Lookup(n)
		if lookupErr != nil {
			return nil, lookupErr
		}
		out = append(out, *res)
	}
	return out, nil
}

// readTags reads the newline-separated candidate tag list from stdin,
// size-bounded, returning the non-empty trimmed lines. The read runs in a
// goroutine so a SIGINT/SIGTERM (ctx cancellation) interrupts a blocking
// stdin read: an interactive invocation with no pipe, or a CI cancellation
// mid-read, returns promptly instead of hanging until SIGKILL. The reader
// goroutine is released when the process exits (or when stdin closes).
func readTags(ctx context.Context, stdin io.Reader) ([]string, error) {
	type readResult struct {
		data []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(stdin, maxTagsBytes+1))
		ch <- readResult{data, err}
	}()

	var data []byte
	select {
	case <-ctx.Done():
		return nil, errors.Wrap(errors.ErrCodeTimeout, "interrupted while reading tags from stdin", ctx.Err())
	case r := <-ch:
		if r.err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "read tags from stdin", r.err)
		}
		data = r.data
	}
	if int64(len(data)) > maxTagsBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "tag list on stdin exceeds size limit")
	}
	var tags []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tags = append(tags, line)
		}
	}
	return tags, nil
}

// scheduleHasNoReleases reports whether the schedule contains only
// tip-of-main cells (no release rows).
func scheduleHasNoReleases(schedule map[string][]uatbroker.Cell) bool {
	for _, cells := range schedule {
		for i := range cells {
			if !cells[i].IsMain {
				return false
			}
		}
	}
	return true
}

// scheduleIsEmpty reports whether the schedule has no cells at all (neither
// main nor any release) — i.e. --include-main=false with no stable tags.
func scheduleIsEmpty(schedule map[string][]uatbroker.Cell) bool {
	for _, cells := range schedule {
		if len(cells) > 0 {
			return false
		}
	}
	return true
}

// boolCount returns how many of the given flags are true — used to enforce
// mutual exclusivity of a set of mode flags.
func boolCount(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// splitCSV splits a comma-separated list, trimming whitespace and dropping
// empties.
func splitCSV(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// flagParseErr maps a FlagSet parse error to the coded exit contract: -h is
// not an error (exit 0), anything else is an invalid-request (exit 2). The
// FlagSet already wrote the message to stderr.
func flagParseErr(err error, sub string) error {
	if stderrors.Is(err, flag.ErrHelp) {
		return nil
	}
	return errors.Wrap(errors.ErrCodeInvalidRequest, "parse "+sub+" flags", err)
}
