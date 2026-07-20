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

package main

import (
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

const sampleIndex = `{
  "schema": "aicr-corroboration/v1",
  "groups": [
    {
      "service": "eks",
      "dashboards": [
        {
          "accelerator": "h100",
          "os": "ubuntu",
          "tabs": [
            {"recipe": "h100-eks-ubuntu-training-kubeflow",
             "coord": {"service":"eks","accelerator":"h100","os":"ubuntu","intent":"training","platform":"kubeflow"}}
          ]
        }
      ]
    }
  ]
}`

func TestOriginHost(t *testing.T) {
	if got := originHost(); got != "validation.aicr.run" {
		t.Errorf("originHost() = %q, want validation.aicr.run", got)
	}
}

func TestCheckRedirectRefusesOffOrigin(t *testing.T) {
	client := newPinnedClient()
	if client.CheckRedirect == nil {
		t.Fatal("pinned client has no CheckRedirect guard")
	}

	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{"same-origin redirect allowed", "https://validation.aicr.run/data/other.json", false},
		{"off-origin redirect refused", "https://evil.example.com/data/index.json", true},
		{"look-alike host refused", "https://validation.aicr.run.evil.com/x", true},
		{"scheme downgrade to http refused", "http://validation.aicr.run/data/index.json", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tt.rawURL, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			err = client.CheckRedirect(req, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckRedirect(%q) error = %v, wantErr %v", tt.rawURL, err, tt.wantErr)
			}
		})
	}
}

func TestDecodeIndex(t *testing.T) {
	oversized := strings.Repeat("a", int(defaults.HTTPResponseBodyLimit)+1)
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"valid", sampleIndex, false},
		{"oversized body rejected", oversized, true},
		{"malformed json rejected", "{not json", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, err := decodeIndex(strings.NewReader(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeIndex() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (len(idx.Groups) != 1 || idx.Groups[0].Service != "eks") {
				t.Errorf("decodeIndex() did not parse groups: %+v", idx.Groups)
			}
		})
	}
}

func TestReadIndexFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "index.json")
	if err := os.WriteFile(good, []byte(sampleIndex), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid file", good, false},
		{"missing file", filepath.Join(dir, "nope.json"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, err := readIndexFile(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("readIndexFile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(idx.Groups) != 1 {
				t.Errorf("readIndexFile() parsed %d groups, want 1", len(idx.Groups))
			}
		})
	}
}

// TestRunOfflineDryRun exercises the full run() over a local fixture and
// asserts it writes a deterministic report and returns no error (exit 0).
func TestRunOfflineDryRun(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
	if err := os.WriteFile(indexPath, []byte(sampleIndex), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	reportPath := filepath.Join(dir, "report.md")

	if err := run(context.Background(), reportPath, indexPath); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	report, err := os.ReadFile(reportPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	got := string(report)
	// The committed eks training-kubeflow link resolves against the fixture; the
	// other committed links (not in the fixture) are dead-link warnings.
	if !strings.Contains(got, "`eks/h100-ubuntu/training-kubeflow` | ✅ resolved") {
		t.Errorf("expected resolved row, got:\n%s", got)
	}
	if !strings.Contains(got, "dead link(s)") {
		t.Errorf("expected dead-link warnings for uncovered committed links, got:\n%s", got)
	}
}

// TestRunFetchFailureExitsZero asserts an unreadable live source produces a
// fetch-failure report and no error (warning-only: exit 0).
func TestRunFetchFailureExitsZero(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.md")

	err := run(context.Background(), reportPath, filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("run() with unreadable live source must not error (warning-only), got %v", err)
	}
	report, err := os.ReadFile(reportPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(report), "Could not load live dashboard data") {
		t.Errorf("expected fetch-failure note, got:\n%s", report)
	}
}

func TestFetchIndex(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{
			name:    "2xx with body",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(sampleIndex)) },
			wantErr: false,
		},
		{
			name:    "non-2xx rejected",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			idx, err := fetchIndex(context.Background(), srv.Client(), srv.URL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("fetchIndex() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(idx.Groups) != 1 {
				t.Errorf("fetchIndex() parsed %d groups, want 1", len(idx.Groups))
			}
		})
	}
}

func TestWriteReportToStdout(t *testing.T) {
	// Empty reportOut renders to stdout; assert the branch runs without error.
	err := writeReport("", func(w io.Writer) error {
		_, wErr := w.Write([]byte("ok"))
		return wErr
	})
	if err != nil {
		t.Errorf("writeReport(stdout) error = %v", err)
	}
}

func TestStateLabel(t *testing.T) {
	tests := map[state]string{
		stateResolved:          "resolved",
		stateMissingButPresent: "missing-but-present",
		stateNotYetLinked:      "not-yet-linked",
		state("unknown"):       "unknown",
	}
	for st, want := range tests {
		if got := stateLabel(st); !strings.Contains(got, want) {
			t.Errorf("stateLabel(%q) = %q, want to contain %q", st, got, want)
		}
	}
}

func TestRealMainOffline(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
	if err := os.WriteFile(indexPath, []byte(sampleIndex), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	reportPath := filepath.Join(dir, "report.md")

	withArgs(t, "testgrid-link-check", "-index-file", indexPath, "-report-out", reportPath)
	if code := realMain(); code != 0 {
		t.Errorf("realMain() = %d, want 0 (warning-only)", code)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Errorf("realMain() did not write the report: %v", err)
	}
}

// withArgs swaps os.Args and resets the global flag set for one realMain call,
// restoring both when the test ends.
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	orig := os.Args
	origFlags := flag.CommandLine
	t.Cleanup(func() {
		os.Args = orig
		flag.CommandLine = origFlags
	})
	os.Args = args
	flag.CommandLine = flag.NewFlagSet(args[0], flag.ContinueOnError)
}
