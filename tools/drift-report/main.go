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
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func main() {
	repoRoot := flag.String("repo-root", ".", "repository root containing recipes/registry.yaml")
	renovateReport := flag.String("renovate-report", "drift-report-raw.json", "Renovate report written with RENOVATE_REPORT_TYPE=file")
	out := flag.String("out", "drift-report.json", "normalized drift report to write")
	slackOut := flag.String("slack-out", "", "Slack webhook payload to write (optional)")
	runURL := flag.String("run-url", "", "workflow run URL to link from the digest")
	commit := flag.String("commit", "", "commit the report was generated from")
	generatedAt := flag.String("generated-at", "", "RFC3339 timestamp; omitted when empty for reproducible output")
	flag.Parse()

	if err := run(*repoRoot, *renovateReport, *out, *slackOut, Meta{
		GeneratedAt: *generatedAt, Commit: *commit, RunURL: *runURL,
	}); err != nil {
		slog.Error("drift-report failed", "error", err)
		os.Exit(1)
	}
}

func run(repoRoot, renovateReport, out, slackOut string, meta Meta) error {
	pins, err := LoadPins(repoRoot)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(renovateReport) //nolint:gosec // operator-supplied path
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound,
			"read Renovate report (is RENOVATE_REPORT_PATH readable from the runner?)", err)
	}
	lookups, err := ParseRenovateReport(raw)
	if err != nil {
		return err
	}
	report, err := BuildReport(pins, lookups, meta)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "encode drift report", err)
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "write drift report", err)
	}
	if slackOut != "" {
		payload, err := SlackPayload(report)
		if err != nil {
			return err
		}
		if err := os.WriteFile(slackOut, append(payload, '\n'), 0o600); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "write Slack payload", err)
		}
	}
	fmt.Fprintf(os.Stdout, "tracked=%d behind=%d unresolved=%d\n",
		report.Summary.Tracked, report.Summary.Behind, report.Summary.Unresolved)
	return nil
}
