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

package diff

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// errWriter wraps an io.Writer and retains the first write error encountered.
// Subsequent writes become no-ops after an error so callers can check err once.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) printf(format string, args ...any) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintf(ew.w, format, args...)
}

func (ew *errWriter) println(s string) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintln(ew.w, s)
}

// WriteTable writes the diff result as a human-readable table.
// Returns a structured error wrapping the first write failure encountered
// (useful for broken-pipe or full-disk scenarios on the target writer).
func WriteTable(w io.Writer, result *Result) error {
	ew := &errWriter{w: w}

	if len(result.Changes) == 0 {
		ew.println("NO CHANGES")
		return wrapTableErr(ew.err)
	}

	ew.printf("CHANGES (%d added, %d removed, %d modified)\n",
		result.Summary.Added, result.Summary.Removed, result.Summary.Modified)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "KIND\tPATH\tBASELINE\tTARGET"); err != nil {
		return wrapTableErr(err)
	}
	if _, err := fmt.Fprintln(tw, "----\t----\t--------\t------"); err != nil {
		return wrapTableErr(err)
	}

	for _, c := range result.Changes {
		baseline := c.Baseline
		target := c.Target
		if baseline == "" {
			baseline = "-"
		}
		if target == "" {
			target = "-"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			strings.ToUpper(string(c.Kind)), c.Path, baseline, target); err != nil {
			return wrapTableErr(err)
		}
	}

	if err := tw.Flush(); err != nil {
		return wrapTableErr(err)
	}

	ew.println("")
	ew.println("DRIFT DETECTED")

	return wrapTableErr(ew.err)
}

func wrapTableErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.Wrap(errors.ErrCodeInternal, "failed to write diff table output", err)
}
