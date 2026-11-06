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

package upstreamvalidator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// TestVendoredValidatorMatchesPin holds the generated validate.go to the
// digest in provenance.go, so an edit to the vendored copy, or a refresh to
// another upstream revision, is a deliberate change of both.
func TestVendoredValidatorMatchesPin(t *testing.T) {
	data, err := os.ReadFile("validate.go")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != vendoredDigest {
		t.Errorf("validate.go digest = %s, pinned %s (run tools/vendor-cncf-validator and update the pin together)", got, vendoredDigest)
	}
	if len(upstreamCommit) != 40 || len(upstreamDigest) != 64 {
		t.Errorf("upstreamCommit %q / upstreamDigest %q are not a full SHA and a sha256", upstreamCommit, upstreamDigest)
	}
	head := string(data[:600])
	for _, want := range []string{"Code generated", "DO NOT EDIT", upstreamCommit, upstreamDigest} {
		if !strings.Contains(head, want) {
			t.Errorf("validate.go header lacks %q", want)
		}
	}
}

// fakeTransport answers every request in memory: the CNCF landscape with
// the members it is given, and any other URL with 200, so the vendored
// validator's network checks run without a listener. Installed on
// http.DefaultTransport, which the validator's zero-Transport clients use.
type fakeTransport struct {
	members         []string
	landscapeStatus int

	mu       sync.Mutex
	requests []string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req.URL.String())
	f.mu.Unlock()
	status, body := http.StatusOK, ""
	if req.URL.String() == landscapeURL {
		status = f.landscapeStatus
		var sb strings.Builder
		sb.WriteString("landscape:\n  - name: CNCF Members\n    subcategories:\n      - name: Platinum\n        items:\n")
		for _, m := range f.members {
			sb.WriteString("          - name: " + m + " (member)\n")
		}
		body = sb.String()
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func installTransport(t *testing.T, f *fakeTransport) *fakeTransport {
	t.Helper()
	if f.landscapeStatus == 0 {
		f.landscapeStatus = http.StatusOK
	}
	prev := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = prev })
	return f
}

// writeProduct lays out <root>/docs/AIConformance-1.37.yaml from the sibling
// cncf package's embedded copy and <root>/v1.37/test/PRODUCT.yaml with every
// requirement at the given status, and returns the product path and the
// schema directory.
func writeProduct(t *testing.T, vendorName, status string) (product, schemas string) {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("..", "schemas", "AIConformance-1.37.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metadata map[string]string `yaml:"metadata"`
		Spec     map[string][]struct {
			ID          string `yaml:"id"`
			Description string `yaml:"description"`
			Level       string `yaml:"level"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(schema, &doc); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	schemas = filepath.Join(root, "docs")
	if err := os.MkdirAll(schemas, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(schemas, "AIConformance-1.37.yaml"), schema, 0o644); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("metadata:\n")
	for k, v := range map[string]string{
		"kubernetesVersion":   "v1.37",
		"platformName":        "Test Platform",
		"platformVersion":     "1.0.0",
		"vendorName":          vendorName,
		"websiteUrl":          "https://vendor.example/platform",
		"documentationUrl":    "https://vendor.example/docs",
		"productLogoUrl":      "https://vendor.example/logo.svg",
		"description":         "A test platform.",
		"contactEmailAddress": "owner@vendor.example",
		"k8sConformanceUrl":   "https://github.com/cncf/k8s-conformance/tree/master/v1.37/test",
	} {
		sb.WriteString("  " + k + ": " + v + "\n")
	}
	sb.WriteString("spec:\n")
	for cat, reqs := range doc.Spec {
		sb.WriteString("  " + cat + ":\n")
		for _, r := range reqs {
			sb.WriteString("    - id: " + r.ID + "\n      level: " + r.Level + "\n      status: " + status + "\n      evidence: []\n      notes: test\n")
		}
	}
	productDir := filepath.Join(root, "v1.37", "test")
	if err := os.MkdirAll(productDir, 0o755); err != nil {
		t.Fatal(err)
	}
	product = filepath.Join(productDir, "PRODUCT.yaml")
	if err := os.WriteFile(product, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return product, schemas
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		transport  *fakeTransport
		vendor     string
		status     string
		wantCode   errors.ErrorCode
		wantOutput []string
	}{
		{
			name:       "accepted",
			transport:  &fakeTransport{members: []string{"Vendor"}},
			vendor:     "Vendor",
			status:     "Implemented",
			wantOutput: []string{"Validation successful"},
		},
		{
			name:       "rejected when the vendor is not a CNCF member",
			transport:  &fakeTransport{members: []string{"Someone Else"}},
			vendor:     "Vendor",
			status:     "Implemented",
			wantCode:   errors.ErrCodeInternal,
			wantOutput: []string{"Validation failed:", "does not match any CNCF member"},
		},
		{
			name:       "rejected when a MUST is not implemented",
			transport:  &fakeTransport{members: []string{"Vendor"}},
			vendor:     "Vendor",
			status:     "Not Implemented",
			wantCode:   errors.ErrCodeInternal,
			wantOutput: []string{"is MUST level but status is 'Not Implemented'"},
		},
		{
			name:       "unavailable when the landscape cannot be fetched",
			transport:  &fakeTransport{members: []string{"Vendor"}, landscapeStatus: http.StatusBadGateway},
			vendor:     "Vendor",
			status:     "Implemented",
			wantCode:   errors.ErrCodeUnavailable,
			wantOutput: []string{"failed to fetch CNCF landscape"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installTransport(t, tt.transport)
			product, schemas := writeProduct(t, tt.vendor, tt.status)
			output, err := Run(context.Background(), product, schemas)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("Run() error = %v\n%s", err, output)
				}
			} else {
				errorstest.WantReportedCode(t, err, tt.wantCode)
			}
			for _, want := range tt.wantOutput {
				if !strings.Contains(output, want) {
					t.Errorf("output lacks %q:\n%s", want, output)
				}
			}
			if tt.wantCode == errors.ErrCodeInternal && !strings.Contains(err.Error(), tt.wantOutput[len(tt.wantOutput)-1]) {
				t.Errorf("error %q does not carry the validator's line", err.Error())
			}
		})
	}
}

func TestRunRequiresPaths(t *testing.T) {
	_, err := Run(context.Background(), "", "docs")
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	_, err = Run(context.Background(), "v1.37/x/PRODUCT.yaml", "")
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
}

func TestRunCanceled(t *testing.T) {
	tr := installTransport(t, &fakeTransport{members: []string{"Vendor"}})
	product, schemas := writeProduct(t, "Vendor", "Implemented")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, product, schemas)
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	// A context that is already dead starts nothing.
	tr.mu.Lock()
	started := len(tr.requests)
	tr.mu.Unlock()
	if started != 0 {
		t.Errorf("validator made %d request(s) under a canceled context", started)
	}
	// The background run must release the lock so the next call proceeds.
	if _, err := Run(context.Background(), product, schemas); err != nil {
		t.Fatalf("Run() after cancellation: %v", err)
	}
}

// While another validation holds the slot, a caller whose context ends
// returns at once with the context's code and never enters the validator;
// the slot is released afterwards for the next caller.
func TestRunWaitsForSlotUnderContext(t *testing.T) {
	tr := installTransport(t, &fakeTransport{members: []string{"Vendor"}})
	product, schemas := writeProduct(t, "Vendor", "Implemented")

	slot <- struct{}{} // another run is in progress
	released := false
	defer func() {
		if !released {
			<-slot
		}
	}()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(canceled, product, schemas)
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)

	deadline, cancelDeadline := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelDeadline()
	start := time.Now()
	_, err = Run(deadline, product, schemas)
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
	if time.Since(start) > 5*time.Second {
		t.Errorf("Run() waited %s for the slot instead of returning on the deadline", time.Since(start))
	}
	tr.mu.Lock()
	started := len(tr.requests)
	tr.mu.Unlock()
	if started != 0 {
		t.Errorf("validator made %d request(s) for callers that never acquired the slot", started)
	}

	<-slot // the other run finishes
	released = true
	if _, err := Run(context.Background(), product, schemas); err != nil {
		t.Fatalf("Run() after the slot was released: %v", err)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   errors.ErrorCode
	}{
		{"landscape fetch", "Fetching CNCF member list from landscape...\nError: failed to fetch CNCF landscape: HTTP 502\n", errors.ErrCodeUnavailable},
		{"landscape shape", "Fetching...\nError: could not find 'CNCF Members' category in landscape data\n", errors.ErrCodeUnavailable},
		{"validation", "Validating x...\nValidation failed:\n  - Missing requirement\n", errors.ErrCodeInternal},
		{"version from path", "Validating x...\nError: Could not determine version from path x\n", errors.ErrCodeInternal},
		{"usage", "Usage: go run -tags validate scripts/validate.go <path_to_product.yaml> ...\n", errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errorstest.WantReportedCode(t, classify(tt.output), tt.want)
		})
	}
}
