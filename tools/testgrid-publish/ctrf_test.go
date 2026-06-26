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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func TestConvertCTRF(t *testing.T) {
	tests := []struct {
		name        string
		phases      map[string]ctrf.Report
		wantPassed  bool
		wantContain []string // substrings expected in the XML
		wantAbsent  []string // substrings that must NOT appear
		wantErr     bool
	}{
		{
			name: "all passed",
			phases: map[string]ctrf.Report{
				"deployment": makeReport("deployment", []ctrf.TestResult{
					{Name: "health-check", Status: ctrf.StatusPassed, Duration: 1000},
				}),
			},
			wantPassed:  true,
			wantContain: []string{"deployment/health-check", `classname="deployment"`},
			wantAbsent:  []string{"<failure", "<error"},
		},
		{
			name: "one failed test",
			phases: map[string]ctrf.Report{
				"deployment": makeReport("deployment", []ctrf.TestResult{
					{Name: "health-check", Status: ctrf.StatusPassed, Duration: 500},
					{Name: "cpu-check", Status: ctrf.StatusFailed, Duration: 200, Message: "CPU over 90%"},
				}),
			},
			wantPassed:  false,
			wantContain: []string{"<failure", "CPU over 90%", "deployment/cpu-check"},
		},
		{
			name: "skipped test",
			phases: map[string]ctrf.Report{
				"performance": makeReport("performance", []ctrf.TestResult{
					{Name: "gpu-benchmark", Status: ctrf.StatusSkipped},
				}),
			},
			wantPassed:  true,
			wantContain: []string{"<skipped"},
		},
		{
			name: "other status becomes error",
			phases: map[string]ctrf.Report{
				"conformance": makeReport("conformance", []ctrf.TestResult{
					{Name: "sonobuoy", Status: ctrf.StatusOther, Message: "OOM killed"},
				}),
			},
			wantPassed:  false,
			wantContain: []string{"<error", "OOM killed"},
		},
		{
			name:    "no ctrf files",
			phases:  nil,
			wantErr: true,
		},
		{
			name: "all phases present but zero tests",
			phases: map[string]ctrf.Report{
				"deployment": makeReport("deployment", nil),
			},
			wantErr: true, // zero total tests → error, not silent SUCCESS
		},
		{
			name: "pending test produces skipped element",
			phases: map[string]ctrf.Report{
				"deployment": makeReport("deployment", []ctrf.TestResult{
					{Name: "pending-check", Status: ctrf.StatusPending},
				}),
			},
			wantPassed:  true,
			wantContain: []string{"<skipped"},
		},
		{
			name: "unrecognized status produces error element",
			phases: map[string]ctrf.Report{
				"deployment": makeReport("deployment", []ctrf.TestResult{
					{Name: "bogus-check", Status: "bogus", Message: "unknown status"},
				}),
			},
			wantPassed:  false,
			wantContain: []string{"<error", "unrecognized CTRF status: bogus"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundleDir := writeFakeCTRF(t, tt.phases)

			xmlBytes, passed, err := convertCTRF(bundleDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("convertCTRF() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			xmlStr := string(xmlBytes)
			for _, want := range tt.wantContain {
				if !strings.Contains(xmlStr, want) {
					t.Errorf("XML missing %q\nXML:\n%s", want, xmlStr)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(xmlStr, absent) {
					t.Errorf("XML unexpectedly contains %q\nXML:\n%s", absent, xmlStr)
				}
			}

			if passed != tt.wantPassed {
				t.Errorf("allPassed = %v, want %v\nXML:\n%s", passed, tt.wantPassed, xmlStr)
			}
		})
	}
}

func TestConvertCTRFDeterminism(t *testing.T) {
	phases := map[string]ctrf.Report{
		"deployment": makeReport("deployment", []ctrf.TestResult{
			{Name: "z-test", Status: ctrf.StatusPassed, Duration: 100},
			{Name: "a-test", Status: ctrf.StatusFailed, Duration: 200},
			{Name: "m-test", Status: ctrf.StatusSkipped, Duration: 0},
		}),
	}
	bundleDir := writeFakeCTRF(t, phases)

	first, _, err := convertCTRF(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := convertCTRF(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("convertCTRF not deterministic:\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	// Verify tests are sorted alphabetically within the suite.
	xml := string(first)
	aIdx := strings.Index(xml, "a-test")
	mIdx := strings.Index(xml, "m-test")
	zIdx := strings.Index(xml, "z-test")
	if aIdx < 0 || mIdx < 0 || zIdx < 0 {
		t.Fatal("one or more test names not found in XML")
	}
	if aIdx >= mIdx || mIdx >= zIdx {
		t.Errorf("tests not sorted: a=%d m=%d z=%d", aIdx, mIdx, zIdx)
	}
}

// writeFakeCTRF creates a bundle dir with ctrf/<phase>.json files.
func writeFakeCTRF(t *testing.T, phases map[string]ctrf.Report) string {
	t.Helper()
	dir := t.TempDir()
	ctrfDir := filepath.Join(dir, ctrfDirName)
	if err := os.MkdirAll(ctrfDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for phase, report := range phases {
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ctrfDir, phase+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func makeReport(tool string, tests []ctrf.TestResult) ctrf.Report {
	s := ctrf.Summary{Tests: len(tests)}
	for _, t := range tests {
		switch t.Status {
		case ctrf.StatusPassed:
			s.Passed++
		case ctrf.StatusFailed:
			s.Failed++
		case ctrf.StatusSkipped:
			s.Skipped++
		case ctrf.StatusOther:
			s.Other++
		}
	}
	return ctrf.Report{
		ReportFormat: ctrf.ReportFormatCTRF,
		SpecVersion:  ctrf.SpecVersion,
		Results: ctrf.Results{
			Tool:    ctrf.Tool{Name: tool},
			Summary: s,
			Tests:   tests,
		},
	}
}
