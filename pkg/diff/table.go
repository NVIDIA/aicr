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
)

// WriteTable writes the diff result as a human-readable table.
func WriteTable(w io.Writer, result *Result) error {
	if len(result.Changes) == 0 {
		fmt.Fprintln(w, "NO CHANGES")
		return nil
	}

	fmt.Fprintf(w, "CHANGES (%d added, %d removed, %d modified)\n",
		result.Summary.Added, result.Summary.Removed, result.Summary.Modified)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tPATH\tBASELINE\tTARGET")
	fmt.Fprintln(tw, "----\t----\t--------\t------")

	for _, c := range result.Changes {
		baseline := c.Baseline
		target := c.Target
		if baseline == "" {
			baseline = "-"
		}
		if target == "" {
			target = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			strings.ToUpper(string(c.Kind)), c.Path, baseline, target)
	}

	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	if result.HasDrift() {
		fmt.Fprintln(w, "DRIFT DETECTED")
	} else {
		fmt.Fprintln(w, "NO DRIFT")
	}

	return nil
}
