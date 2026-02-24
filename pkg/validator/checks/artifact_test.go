// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package checks

import (
	"strings"
	"sync"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

func TestArtifact_EncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		label string
		data  string
	}{
		{"simple", "Pod Status", "NAME  READY  STATUS\nnginx  1/1  Running"},
		{"empty data", "Empty", ""},
		{"unicode", "Metrics \u00b5s", "latency: 42\u00b5s"},
		{"large payload", "Big Data", strings.Repeat("x", 4096)},
		{"special chars", "JSON Output", `{"key": "value", "nested": {"a": 1}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := Artifact{Label: tt.label, Data: tt.data}

			encoded, err := original.Encode()
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}

			decoded, err := DecodeArtifact(encoded)
			if err != nil {
				t.Fatalf("DecodeArtifact() error = %v", err)
			}

			if decoded.Label != original.Label {
				t.Errorf("Label = %q, want %q", decoded.Label, original.Label)
			}
			if decoded.Data != original.Data {
				t.Errorf("Data = %q, want %q", decoded.Data, original.Data)
			}
		})
	}
}

func TestDecodeArtifact_InvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"invalid base64", "not-valid-base64!!!", "failed to decode artifact base64"},
		{"valid base64 invalid json", "aGVsbG8=", "failed to unmarshal artifact JSON"},
		{"empty string", "", "failed to unmarshal artifact JSON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeArtifact(tt.input)
			if err == nil {
				t.Fatal("DecodeArtifact() should return error for invalid input")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, should contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestArtifactCollector_RecordAndDrain(t *testing.T) {
	c := NewArtifactCollector()

	// Record two artifacts.
	if err := c.Record("first", "data-1"); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := c.Record("second", "data-2"); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	// Drain should return both.
	arts := c.Drain()
	if len(arts) != 2 {
		t.Fatalf("Drain() returned %d artifacts, want 2", len(arts))
	}
	if arts[0].Label != "first" || arts[0].Data != "data-1" {
		t.Errorf("arts[0] = %+v, want {Label:first Data:data-1}", arts[0])
	}
	if arts[1].Label != "second" || arts[1].Data != "data-2" {
		t.Errorf("arts[1] = %+v, want {Label:second Data:data-2}", arts[1])
	}

	// Second drain should return nil (reset).
	arts2 := c.Drain()
	if arts2 != nil {
		t.Errorf("second Drain() = %v, want nil", arts2)
	}
}

func TestArtifactCollector_DrainEmpty(t *testing.T) {
	c := NewArtifactCollector()
	arts := c.Drain()
	if arts != nil {
		t.Errorf("Drain() on empty collector = %v, want nil", arts)
	}
}

func TestArtifactCollector_DataTruncation(t *testing.T) {
	c := NewArtifactCollector()

	largeData := strings.Repeat("a", defaults.ArtifactMaxDataSize+1000)
	if err := c.Record("large", largeData); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	arts := c.Drain()
	if len(arts) != 1 {
		t.Fatalf("Drain() returned %d artifacts, want 1", len(arts))
	}

	// Data should be truncated to defaults.ArtifactMaxDataSize + truncation marker.
	if !strings.HasSuffix(arts[0].Data, "\n... [truncated]") {
		t.Error("truncated artifact should end with '\\n... [truncated]'")
	}

	// The prefix should be exactly defaults.ArtifactMaxDataSize bytes of the original.
	prefix := arts[0].Data[:defaults.ArtifactMaxDataSize]
	if prefix != strings.Repeat("a", defaults.ArtifactMaxDataSize) {
		t.Error("truncated artifact prefix should be original data up to defaults.ArtifactMaxDataSize")
	}
}

func TestArtifactCollector_DataAtExactLimit(t *testing.T) {
	c := NewArtifactCollector()

	exactData := strings.Repeat("b", defaults.ArtifactMaxDataSize)
	if err := c.Record("exact", exactData); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	arts := c.Drain()
	if arts[0].Data != exactData {
		t.Error("data at exact limit should not be truncated")
	}
}

func TestArtifactCollector_CountLimit(t *testing.T) {
	c := NewArtifactCollector()

	// Fill to capacity.
	for i := range defaults.ArtifactMaxPerCheck {
		if err := c.Record("item", "data"); err != nil {
			t.Fatalf("Record() #%d error = %v", i, err)
		}
	}

	// One more should fail.
	err := c.Record("overflow", "data")
	if err == nil {
		t.Fatal("Record() beyond limit should return error")
	}
	if !strings.Contains(err.Error(), "artifact limit reached") {
		t.Errorf("error = %v, should contain 'artifact limit reached'", err)
	}

	// Drain should return exactly defaults.ArtifactMaxPerCheck.
	arts := c.Drain()
	if len(arts) != defaults.ArtifactMaxPerCheck {
		t.Errorf("Drain() returned %d artifacts, want %d", len(arts), defaults.ArtifactMaxPerCheck)
	}
}

func TestArtifactCollector_ThreadSafety(t *testing.T) {
	c := NewArtifactCollector()

	var wg sync.WaitGroup
	goroutines := 10
	perGoroutine := 2 // 10 * 2 = 20 = defaults.ArtifactMaxPerCheck

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				_ = c.Record("concurrent", "data")
			}
		}()
	}

	wg.Wait()

	arts := c.Drain()
	if len(arts) != defaults.ArtifactMaxPerCheck {
		t.Errorf("concurrent Drain() returned %d artifacts, want %d", len(arts), defaults.ArtifactMaxPerCheck)
	}
}

func TestTestRunner_CancelWithArtifacts(t *testing.T) {
	mockT := &mockTestingT{}
	collector := NewArtifactCollector()
	if err := collector.Record("test-label", "test-data"); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	runner := &TestRunner{
		t: mockT,
		ctx: &ValidationContext{
			Artifacts: collector,
		},
	}

	runner.Cancel()

	// Verify artifact was emitted via t.Logf.
	if len(mockT.logMessages) == 0 {
		t.Fatal("Cancel() should emit artifact via t.Logf")
	}

	found := false
	for _, msg := range mockT.logMessages {
		if strings.HasPrefix(msg, "ARTIFACT:") {
			found = true
			// Verify round-trip: decode the emitted artifact.
			encoded := strings.TrimPrefix(msg, "ARTIFACT:")
			decoded, err := DecodeArtifact(encoded)
			if err != nil {
				t.Fatalf("failed to decode emitted artifact: %v", err)
			}
			if decoded.Label != "test-label" {
				t.Errorf("decoded Label = %q, want %q", decoded.Label, "test-label")
			}
			if decoded.Data != "test-data" {
				t.Errorf("decoded Data = %q, want %q", decoded.Data, "test-data")
			}
		}
	}
	if !found {
		t.Error("Cancel() should emit ARTIFACT: prefixed log message")
	}
}

func TestTestRunner_CancelWithNilArtifacts(t *testing.T) {
	mockT := &mockTestingT{}
	runner := &TestRunner{
		t: mockT,
		ctx: &ValidationContext{
			Artifacts: nil,
		},
	}

	// Should not panic.
	runner.Cancel()

	// No artifact messages should be emitted.
	for _, msg := range mockT.logMessages {
		if strings.HasPrefix(msg, "ARTIFACT:") {
			t.Error("Cancel() with nil Artifacts should not emit artifact logs")
		}
	}
}
