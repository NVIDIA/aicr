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

package cncf

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// Artifact file names the upstream validator parses by name
// (canonicalArtifacts in scripts/validate.go), and the evidence pages'
// directory inside the submission.
const (
	junitFile       = "junit.xml"
	e2eLogFile      = "e2e.log"
	productFile     = "PRODUCT.yaml"
	readmeFile      = "README.md"
	evidenceDirName = "evidence"
)

// exportedPhase is the validation phase whose results the artifacts carry,
// as the CTRF builder records it in TestResult.Suite.
const exportedPhase = "conformance"

// exportedStatus is the status a check's testcase carries in junit.xml and
// e2e.log. It is the CTRF status except where the export rule applies.
type exportedStatus string

const (
	exportPassed  exportedStatus = "passed"
	exportFailed  exportedStatus = "failed"
	exportSkipped exportedStatus = "skipped"
)

// exportedTest is one check as the submission artifacts carry it: its name
// (the junit testcase name and the e2e.log result name, which PRODUCT.yaml
// references by fragment), its exported status, and the message that keeps
// the original verdict when the status was rewritten.
type exportedTest struct {
	Name     string
	Status   exportedStatus
	Message  string
	Duration time.Duration

	// Original is the CTRF status; it differs from Status only when the
	// export rule rewrote a non-passing result to skipped.
	Original string

	// Rewritten is true when the export rule applied.
	Rewritten bool
}

// Export rule for the submission artifacts (ADR-007 amendment, decision 6).
// The raw CTRF report keeps every real verdict; the artifacts the upstream
// validator parses must not carry a failure the validator would hold against
// the submission when the requirement behind the check does not hold it
// against the run. The validator fails a referenced artifact on any failing
// testcase anywhere in it, before it resolves a fragment, so the rewrite is
// what lets a Not Implemented SHOULD or an N/A conditional MUST coexist with
// the MUSTs' evidence in one junit.xml.
//
// A non-passing result (failed, other, pending) is exported as skipped, with
// the original verdict and reason in the message, when its check evidences
//
//   - a SHOULD, which renders Not Implemented and never fails the run;
//   - a requirement declared not applicable, whose justification is quoted;
//   - supporting evidence (a requirement the checklist version does not
//     list, dra_support from v1.36 on), which no row cites.
//
// It stays a failure when its check evidences an applicable MUST, which
// fails the run before a directory is declared complete, and when its check
// evidences no requirement at all (platform-health and the Slinky health
// checks): such a failure is a cluster that is not ready to submit, and the
// validator rejecting the artifact says so. A skipped result is exported
// skipped with its reason code; a passed result passes.
//
// Only conformance-phase results are exported: a `--phase all` run's merged
// report also carries deployment and performance checks, which evidence no
// CNCF requirement and would make a failure the validator holds against the
// artifact. A result with no recorded phase (a report assembled outside the
// validator) is exported.
func exportTests(report *ctrf.Report, agg *Aggregate) []exportedTest {
	if report == nil {
		return nil
	}
	out := make([]exportedTest, 0, len(report.Results.Tests))
	for _, t := range report.Results.Tests {
		if len(t.Suite) > 0 && t.Suite[0] != exportedPhase {
			continue
		}
		e := exportedTest{
			Name:     t.Name,
			Original: t.Status,
			Duration: time.Duration(t.Duration) * time.Millisecond,
		}
		switch t.Status {
		case ctrf.StatusPassed:
			e.Status = exportPassed
		case ctrf.StatusSkipped:
			e.Status = exportSkipped
			e.Message = skipMessage(t)
		default:
			e.Status = exportFailed
			e.Message = verdictMessage(t)
			if reason, ok := exportAsSkipped(t.Name, agg); ok {
				e.Status = exportSkipped
				e.Rewritten = true
				e.Message = reason + "; original verdict " + verdictMessage(t)
			}
		}
		out = append(out, e)
	}
	return out
}

// exportAsSkipped decides whether a non-passing result of the named check is
// exported as skipped, and with which reason prefix.
func exportAsSkipped(check string, agg *Aggregate) (string, bool) {
	page := pageForCheck(check)
	if page == nil || agg == nil {
		return "", false
	}
	if r := agg.Requirement(page.RequirementID); r != nil {
		switch {
		case r.Status == StatusNotApplicable:
			return "not applicable: " + r.Reason, true
		case r.Level == LevelShould:
			return fmt.Sprintf("not implemented: %s is a SHOULD at %s and is not claimed", r.ID, agg.Version), true
		}
		return "", false
	}
	// Not a requirement of this version: supporting evidence.
	return fmt.Sprintf("supporting evidence: %s is not a requirement of the %s checklist", page.RequirementID, agg.Version), true
}

func skipMessage(t ctrf.TestResult) string {
	if code := t.Extra[extraKeySkipReason]; code != "" {
		return "skipped: " + code
	}
	if t.Message != "" {
		return "skipped: " + t.Message
	}
	return "skipped"
}

func verdictMessage(t ctrf.TestResult) string {
	if t.Message == "" {
		return t.Status
	}
	return t.Status + ": " + t.Message
}

// junit.xml shape: a <testsuites> root with one <testsuite> of AICR
// conformance checks, as gotestsum and go-junit-report emit for a Go
// package, which is what upstream's parser is written against.
type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Skipped   int         `xml:"skipped,attr"`
	Time      string      `xml:"time,attr"`
	Timestamp string      `xml:"timestamp,attr"`
	Cases     []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

// junitSuiteName is the testsuite and classname: the Go package the
// conformance checks would be, were they `go test` tests.
const junitSuiteName = "aicr/conformance"

// renderJUnit renders the exported tests as junit.xml.
func renderJUnit(tests []exportedTest, now time.Time) ([]byte, error) {
	suite := junitSuite{Name: junitSuiteName, Timestamp: now.UTC().Format("2006-01-02T15:04:05")}
	var total time.Duration
	for _, t := range tests {
		c := junitCase{Name: t.Name, Classname: junitSuiteName, Time: seconds(t.Duration)}
		switch t.Status {
		case exportFailed:
			suite.Failures++
			c.Failure = &junitMessage{Message: t.Message, Body: t.Message}
		case exportSkipped:
			suite.Skipped++
			c.Skipped = &junitMessage{Message: t.Message}
		case exportPassed:
		}
		suite.Tests++
		total += t.Duration
		suite.Cases = append(suite.Cases, c)
	}
	suite.Time = seconds(total)
	doc := junitSuites{
		Name:     junitSuiteName,
		Tests:    suite.Tests,
		Failures: suite.Failures,
		Skipped:  suite.Skipped,
		Time:     suite.Time,
		Suites:   []junitSuite{suite},
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "\t")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// renderE2ELog renders the exported tests in `go test -v` shape: a RUN line
// and a "--- PASS|FAIL|SKIP: <check> (<seconds>s)" result line per check,
// the message indented beneath a failed or skipped one, then the package
// verdict. Upstream parses only the result lines, by the regular expression
// `^\s*--- (PASS|FAIL|SKIP): (\S+)`, so the check name is the token the
// fragment of a PRODUCT.yaml reference must match.
func renderE2ELog(tests []exportedTest, now time.Time) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# AICR conformance checks, %s; exported from the CTRF report of this run\n", now.UTC().Format(time.RFC3339))
	var total time.Duration
	failed := false
	for _, t := range tests {
		total += t.Duration
		fmt.Fprintf(&sb, "=== RUN   %s\n", t.Name)
		var verdict string
		switch t.Status {
		case exportPassed:
			verdict = "PASS"
		case exportFailed:
			verdict = "FAIL"
			failed = true
		case exportSkipped:
			verdict = "SKIP"
		}
		fmt.Fprintf(&sb, "--- %s: %s (%ss)\n", verdict, t.Name, seconds(t.Duration))
		if t.Message != "" {
			for _, line := range strings.Split(t.Message, "\n") {
				fmt.Fprintf(&sb, "    %s\n", line)
			}
		}
	}
	if failed {
		fmt.Fprintf(&sb, "FAIL\nFAIL\t%s\t%ss\n", junitSuiteName, seconds(total))
	} else {
		fmt.Fprintf(&sb, "PASS\nok  \t%s\t%ss\n", junitSuiteName, seconds(total))
	}
	return []byte(sb.String())
}

func seconds(d time.Duration) string {
	return fmt.Sprintf("%.3f", d.Seconds())
}
