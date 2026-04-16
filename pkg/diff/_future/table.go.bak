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
	if result.Mode == "recipe-vs-snapshot" {
		return writeRecipeTable(w, result)
	}
	return writeSnapshotTable(w, result)
}

func writeRecipeTable(w io.Writer, result *Result) error {
	// Constraints table
	if len(result.ConstraintResults) > 0 {
		fmt.Fprintf(w, "CONSTRAINTS (%d passed, %d failed, %d errors)\n",
			result.Summary.ConstraintsPassed, result.Summary.ConstraintsFailed, result.Summary.ConstraintsError)

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "STATUS\tSEVERITY\tCONSTRAINT\tEXPECTED\tACTUAL\tREMEDIATION")
		fmt.Fprintln(tw, "------\t--------\t----------\t--------\t------\t-----------")

		for _, cr := range result.ConstraintResults {
			status := "PASS"
			if cr.Error != "" {
				status = "ERR"
			} else if !cr.Passed {
				status = "FAIL"
			}

			remediation := cr.Remediation
			if len(remediation) > 60 {
				remediation = remediation[:57] + "..."
			}

			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				status, cr.Severity, cr.Name, cr.Expected, cr.Actual, remediation)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}

	// Components table
	if len(result.ComponentDrifts) > 0 {
		fmt.Fprintf(w, "COMPONENTS (%d ok, %d drifted)\n",
			result.Summary.ComponentsOK, result.Summary.ComponentsDrifted)

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "STATUS\tCOMPONENT\tEXPECTED\tACTUAL\tNAMESPACE")
		fmt.Fprintln(tw, "------\t---------\t--------\t------\t---------")

		for _, cd := range result.ComponentDrifts {
			status := strings.ToUpper(cd.Status)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				status, cd.Name, cd.ExpectedVersion, cd.ActualVersion, cd.Namespace)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}

	// Validation phases table
	if len(result.ValidationPhases) > 0 {
		fmt.Fprintln(w, "VALIDATION PHASES")

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PHASE\tCHECKS\tCONSTRAINTS")
		fmt.Fprintln(tw, "-----\t------\t-----------")

		for _, vp := range result.ValidationPhases {
			checks := strings.Join(vp.Checks, ", ")
			constraintCount := len(vp.ConstraintResults)
			failedCount := 0
			for _, cr := range vp.ConstraintResults {
				if !cr.Passed {
					failedCount++
				}
			}
			constraintSummary := fmt.Sprintf("%d total", constraintCount)
			if failedCount > 0 {
				constraintSummary = fmt.Sprintf("%d total (%d failed)", constraintCount, failedCount)
			}
			if constraintCount == 0 {
				constraintSummary = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", vp.Phase, checks, constraintSummary)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}

	// Summary
	if result.HasDrift() {
		fmt.Fprintln(w, "DRIFT DETECTED")
	} else {
		fmt.Fprintln(w, "NO DRIFT")
	}

	return nil
}

func writeSnapshotTable(w io.Writer, result *Result) error {
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

	return tw.Flush()
}
