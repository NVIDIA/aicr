// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// convertCTRF reads all phase CTRF reports from bundleDir/ctrf/ and
// produces a deterministic jUnit XML document.
//
// Phase names are used as test suite names so each phase is a
// separate <testsuite> element. Test names are prefixed with the
// phase to ensure global uniqueness across phases.
//
// CTRF status → jUnit mapping:
//
//	passed  → bare <testcase>  (PASS)
//	failed  → <testcase><failure>
//	other   → <testcase><error>
//	skipped → <testcase><skipped/>
//	pending → <testcase><skipped/>
func convertCTRF(bundleDir string) ([]byte, bool, error) {
	ctrfDir := filepath.Join(bundleDir, ctrfDirName)

	reports := make(map[attestation.Phase]*ctrf.Report, len(attestation.AllPhases))
	for _, phase := range attestation.AllPhases {
		path := filepath.Join(ctrfDir, string(phase)+".json")
		data, err := readBoundedFile(path, defaults.MaxBundlePOSTBytes)
		if err != nil {
			if os.IsNotExist(err) {
				// Phase not present in this bundle — skip silently.
				continue
			}
			return nil, false, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("read %s.json", phase), err)
		}
		var r ctrf.Report
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, false, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("parse %s.json", phase), err)
		}
		reports[phase] = &r
	}

	if len(reports) == 0 {
		return nil, false, errors.New(errors.ErrCodeInvalidRequest,
			"bundle contains no CTRF phase reports under ctrf/")
	}

	// Reject bundles where every phase report has zero tests — a crashed
	// test runner writes empty CTRF files, which would otherwise publish
	// as a green SUCCESS with zero testcases.
	totalTests := 0
	for _, r := range reports {
		totalTests += len(r.Results.Tests)
	}
	if totalTests == 0 {
		return nil, false, errors.New(errors.ErrCodeInvalidRequest,
			"bundle CTRF reports contain zero test results across all phases")
	}

	suites := jUnitTestSuites{}

	// Iterate phases in the canonical order from the attestation package
	// (deployment → performance → conformance) for determinism.
	for _, phase := range attestation.AllPhases {
		r, ok := reports[phase]
		if !ok {
			continue
		}
		suite := jUnitTestSuite{Name: string(phase)}

		// Sort tests by name within each phase for determinism.
		tests := make([]ctrf.TestResult, len(r.Results.Tests))
		copy(tests, r.Results.Tests)
		sort.Slice(tests, func(i, j int) bool {
			return tests[i].Name < tests[j].Name
		})
		// Derive Tests from the actual slice, not Summary.Tests, so the
		// attribute is accurate even when the CTRF producer miscounts.
		suite.Tests = len(tests)

		for _, t := range tests {
			dur := fmt.Sprintf("%.3f", float64(t.Duration)/1000.0) // ms → s
			tc := jUnitTestCase{
				Name:      string(phase) + "/" + t.Name,
				ClassName: string(phase),
				Time:      dur,
			}
			switch t.Status {
			case ctrf.StatusFailed:
				tc.Failure = &jUnitFailure{Message: t.Message, Text: t.Message}
				suite.Failures++
			case ctrf.StatusOther:
				tc.Error = &jUnitError{Message: t.Message, Text: t.Message}
				suite.Errors++
			case ctrf.StatusSkipped, ctrf.StatusPending:
				tc.Skipped = &jUnitSkipped{}
				suite.Skipped++
			case ctrf.StatusPassed:
				// bare testcase, no child element
			default:
				// Unknown status: treat as error so unrecognized results never
				// silently appear as passing testcases.
				tc.Error = &jUnitError{
					Message: "unrecognized CTRF status: " + t.Status,
					Text:    "unrecognized CTRF status: " + t.Status,
				}
				suite.Errors++
			}
			suite.TestCases = append(suite.TestCases, tc)
		}
		suites.TestSuites = append(suites.TestSuites, suite)
	}

	out, err := xml.MarshalIndent(suites, "", "  ")
	if err != nil {
		return nil, false, errors.Wrap(errors.ErrCodeInternal, "marshal junit xml", err)
	}

	// Derive allPassed from accumulated counts — avoids re-scanning marshaled XML.
	allPassed := true
	for _, s := range suites.TestSuites {
		if s.Failures > 0 || s.Errors > 0 {
			allPassed = false
			break
		}
	}
	return append([]byte(xml.Header), out...), allPassed, nil
}

// ctrfDirName is the bundle subdirectory holding CTRF phase reports.
// Mirrors attestation.ctrfDirName (unexported there).
const ctrfDirName = "ctrf"

// jUnit XML types ─────────────────────────────────────────────────────────────

type jUnitTestSuites struct {
	XMLName    xml.Name         `xml:"testsuites"`
	TestSuites []jUnitTestSuite `xml:"testsuite"`
}

type jUnitTestSuite struct {
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	Errors    int             `xml:"errors,attr"`
	Skipped   int             `xml:"skipped,attr"`
	TestCases []jUnitTestCase `xml:"testcase"`
}

type jUnitTestCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *jUnitFailure `xml:"failure,omitempty"`
	Error     *jUnitError   `xml:"error,omitempty"`
	Skipped   *jUnitSkipped `xml:"skipped,omitempty"`
}

type jUnitFailure struct {
	Message string `xml:"message,attr,omitempty"`
	Text    string `xml:",chardata"`
}

type jUnitError struct {
	Message string `xml:"message,attr,omitempty"`
	Text    string `xml:",chardata"`
}

type jUnitSkipped struct{}
