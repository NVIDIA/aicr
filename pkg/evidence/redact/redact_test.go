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

package redact_test

import (
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/redact"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// fullSnapshot builds a snapshot exercising every collector measurement type
// and the sensitive fields the minimal policy must remove.
func fullSnapshot() *snapshotter.Snapshot {
	s := snapshotter.NewSnapshot()
	s.Kind = header.KindSnapshot
	s.APIVersion = header.GroupVersion
	s.Metadata = map[string]string{
		"timestamp":   "2026-06-22T00:00:00Z",
		"version":     "0.11.1",
		"source-node": "ip-10-0-248-107.ec2.internal",
	}
	s.Measurements = []*measurement.Measurement{
		measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("server").
				SetString("version", "v1.33.1")).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("node").
				SetString("source-node", "ip-10-0-248-107.ec2.internal").
				SetString("provider-id", "aws:///us-west-2a/i-0123456789abcdef0").
				SetString("provider", "eks").
				SetString("container-runtime-id", "containerd://abc123").
				SetString("container-runtime-name", "containerd").
				SetString("container-runtime-version", "1.7.0").
				SetString("kubelet-version", "v1.33.1").
				SetString("kernel-version", "5.10.0-26").
				SetString("operating-system", "linux").
				SetString("os-image", "Amazon Linux 2")).
			Build(),
		measurement.NewMeasurement(measurement.TypeGPU).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("hardware").
				SetBool("gpu-present", true).
				SetInt("gpu-count", 8).
				SetString("model", "h100")).
			Build(),
		measurement.NewMeasurement(measurement.TypeOS).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("release").
				SetString("ID", "ubuntu").
				SetString("VERSION_ID", "22.04")).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("grub").
				SetString("hugepagesz", "1G")).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("sysctl").
				SetString("/proc/sys/kernel/hostname", "secret-host")).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("kmod").
				SetBool("nvidia", true)).
			Build(),
		measurement.NewMeasurement(measurement.TypeSystemD).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("containerd.service").
				SetString("ActiveState", "active").
				SetString("ControlGroup", "/system.slice/containerd.service")).
			Build(),
		measurement.NewMeasurement(measurement.TypeNodeTopology).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("summary").
				SetInt("node-count", 4)).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("label").
				SetString("nvidia.com/gpu.product", "NVIDIA-H100|node1,node2")).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("taint").
				SetString("nvidia.com/gpu.NoSchedule", "present|node1")).
			Build(),
	}
	return s
}

// findSubtype returns the named subtype of the named measurement type, or nil.
func findSubtype(s *snapshotter.Snapshot, t measurement.Type, sub string) *measurement.Subtype {
	for _, m := range s.Measurements {
		if m.Type == t {
			return m.GetSubtype(sub)
		}
	}
	return nil
}

func findMeasurement(s *snapshotter.Snapshot, t measurement.Type) *measurement.Measurement {
	for _, m := range s.Measurements {
		if m.Type == t {
			return m
		}
	}
	return nil
}

func TestSnapshotKeepsAllowlistedSubtypesAndKeys(t *testing.T) {
	out, _ := redact.Snapshot(fullSnapshot())

	// K8s server kept entirely.
	if st := findSubtype(out, measurement.TypeK8s, "server"); st == nil || !st.Has("version") {
		t.Fatalf("K8s.server.version must be kept")
	}

	// K8s node: only allowlisted keys survive.
	node := findSubtype(out, measurement.TypeK8s, "node")
	if node == nil {
		t.Fatalf("K8s.node subtype must be kept")
	}
	for _, dropped := range []string{"source-node", "provider-id", "container-runtime-id"} {
		if node.Has(dropped) {
			t.Errorf("K8s.node.%s must be dropped", dropped)
		}
	}
	for _, kept := range []string{"provider", "kubelet-version", "kernel-version", "operating-system", "os-image", "container-runtime-name", "container-runtime-version"} {
		if !node.Has(kept) {
			t.Errorf("K8s.node.%s must be kept", kept)
		}
	}

	// GPU hardware kept entirely.
	if st := findSubtype(out, measurement.TypeGPU, "hardware"); st == nil || !st.Has("model") {
		t.Errorf("GPU.hardware must be kept")
	}

	// OS release kept; grub/sysctl/kmod dropped.
	if st := findSubtype(out, measurement.TypeOS, "release"); st == nil || !st.Has("ID") {
		t.Errorf("OS.release must be kept")
	}
	for _, sub := range []string{"grub", "sysctl", "kmod"} {
		if findSubtype(out, measurement.TypeOS, sub) != nil {
			t.Errorf("OS.%s subtype must be dropped", sub)
		}
	}

	// NodeTopology: summary kept; label/taint dropped.
	if st := findSubtype(out, measurement.TypeNodeTopology, "summary"); st == nil || !st.Has("node-count") {
		t.Errorf("NodeTopology.summary must be kept")
	}
	for _, sub := range []string{"label", "taint"} {
		if findSubtype(out, measurement.TypeNodeTopology, sub) != nil {
			t.Errorf("NodeTopology.%s subtype must be dropped", sub)
		}
	}

	// SystemD measurement dropped entirely.
	if findMeasurement(out, measurement.TypeSystemD) != nil {
		t.Errorf("SystemD measurement must be dropped entirely")
	}
}

func TestSnapshotKeyConstrainsAllKeptSubtypes(t *testing.T) {
	// Each formerly keep-all subtype now carries an extra unknown/sensitive
	// key alongside its known keys; the unknown key must be dropped.
	s := snapshotter.NewSnapshot()
	s.Measurements = []*measurement.Measurement{
		measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("server").
				SetString("version", "v1.33.1").
				SetString("future-leak", "secret")).
			Build(),
		measurement.NewMeasurement(measurement.TypeGPU).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("hardware").
				SetString("model", "h100").
				SetString("serial-number", "GPU-SENSITIVE-123")).
			Build(),
		measurement.NewMeasurement(measurement.TypeOS).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("release").
				SetString("ID", "ubuntu").
				SetString("VERSION_ID", "22.04").
				// non-standard / operator-influenced os-release keys
				SetString("HOME_URL", "https://internal.example.com").
				SetString("CUSTOM_TENANT", "acct-123")).
			Build(),
		measurement.NewMeasurement(measurement.TypeNodeTopology).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("summary").
				SetInt("node-count", 4).
				SetString("internal-detail", "leak")).
			Build(),
	}
	out, _ := redact.Snapshot(s)

	cases := []struct {
		typ        measurement.Type
		sub        string
		keptKey    string
		droppedKey string
	}{
		{measurement.TypeK8s, "server", "version", "future-leak"},
		{measurement.TypeGPU, "hardware", "model", "serial-number"},
		{measurement.TypeOS, "release", "ID", "HOME_URL"},
		{measurement.TypeOS, "release", "VERSION_ID", "CUSTOM_TENANT"},
		{measurement.TypeNodeTopology, "summary", "node-count", "internal-detail"},
	}
	for _, c := range cases {
		st := findSubtype(out, c.typ, c.sub)
		if st == nil {
			t.Errorf("%s.%s must be kept", c.typ, c.sub)
			continue
		}
		if !st.Has(c.keptKey) {
			t.Errorf("%s.%s.%s must be kept", c.typ, c.sub, c.keptKey)
		}
		if st.Has(c.droppedKey) {
			t.Errorf("%s.%s.%s must be dropped (key-level fail-closed)", c.typ, c.sub, c.droppedKey)
		}
	}
}

func TestSnapshotHeaderMetadataIsAllowlisted(t *testing.T) {
	in := fullSnapshot()
	// A future/unknown metadata key must be dropped (fail-closed), not just
	// the known-sensitive source-node.
	in.Metadata["future-sensitive-key"] = "tenant-acct-123"
	out, _ := redact.Snapshot(in)
	for _, dropped := range []string{"source-node", "future-sensitive-key"} {
		if _, ok := out.Metadata[dropped]; ok {
			t.Errorf("Header metadata %q must be dropped (fail-closed allowlist)", dropped)
		}
	}
	for _, kept := range []string{"timestamp", "version"} {
		if _, ok := out.Metadata[kept]; !ok {
			t.Errorf("Header metadata %s must be kept", kept)
		}
	}
}

func TestSnapshotKeepsAdvisoryFingerprint(t *testing.T) {
	in := fullSnapshot()
	in.Fingerprint = &fingerprint.Fingerprint{}
	in.Fingerprint.Accelerator = fingerprint.Dimension{Value: "h100"}
	out, _ := redact.Snapshot(in)
	if out.Fingerprint == nil || out.Fingerprint.Accelerator.Value != "h100" {
		t.Errorf("advisory fingerprint must be preserved")
	}
}

func TestSnapshotDoesNotMutateInput(t *testing.T) {
	in := fullSnapshot()
	before, err := serializer.MarshalYAMLDeterministic(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _ = redact.Snapshot(in)
	after, err := serializer.MarshalYAMLDeterministic(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("Snapshot must not mutate its input")
	}
}

func TestSnapshotDeterministic(t *testing.T) {
	a, _ := redact.Snapshot(fullSnapshot())
	b, _ := redact.Snapshot(fullSnapshot())
	ab, err := serializer.MarshalYAMLDeterministic(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bb, err := serializer.MarshalYAMLDeterministic(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(ab) != string(bb) {
		t.Errorf("redaction must be deterministic")
	}
}

func TestSnapshotAppliedRulesSortedNonEmpty(t *testing.T) {
	_, rules := redact.Snapshot(fullSnapshot())
	if len(rules) == 0 {
		t.Fatalf("expected applied rules")
	}
	if !sortedUnique(rules) {
		t.Errorf("applied rules must be sorted and unique: %v", rules)
	}
}

func TestSnapshotDropsSubtypeWithNoAllowlistedKeys(t *testing.T) {
	s := snapshotter.NewSnapshot()
	s.Measurements = []*measurement.Measurement{
		// node subtype carries ONLY non-allowlisted keys.
		measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("node").
				SetString("source-node", "n1").
				SetString("provider-id", "p1")).
			Build(),
	}
	out, _ := redact.Snapshot(s)
	// Subtype retains nothing → dropped → measurement (no surviving subtype) dropped.
	if findMeasurement(out, measurement.TypeK8s) != nil {
		t.Errorf("K8s measurement with only non-allowlisted node keys must be dropped, not shipped with empty data")
	}
}

func TestSnapshotDropsSubtypeContext(t *testing.T) {
	s := snapshotter.NewSnapshot()
	st := measurement.NewSubtypeBuilder("server").SetString("version", "v1.33").Build()
	st.Context = map[string]string{"secret-ctx": "leak-me"}
	s.Measurements = []*measurement.Measurement{
		{Type: measurement.TypeK8s, Subtypes: []measurement.Subtype{st}},
	}
	out, _ := redact.Snapshot(s)
	got := findSubtype(out, measurement.TypeK8s, "server")
	if got == nil {
		t.Fatalf("server subtype must be kept")
	}
	if got.Context != nil {
		t.Errorf("subtype Context must be dropped (fail-closed), got %v", got.Context)
	}
}

func TestSnapshotNilReturnsNil(t *testing.T) {
	out, rules := redact.Snapshot(nil)
	if out != nil || rules != nil {
		t.Errorf("nil snapshot should yield nil, nil; got %v %v", out, rules)
	}
}

func sampleReport() *ctrf.Report {
	return &ctrf.Report{
		ReportFormat: ctrf.ReportFormatCTRF,
		SpecVersion:  ctrf.SpecVersion,
		GeneratedBy:  "aicr",
		Results: ctrf.Results{
			Tool:    ctrf.Tool{Name: "chainsaw", Version: "0.2.0"},
			Summary: ctrf.Summary{Tests: 2, Passed: 1, Failed: 1},
			Tests: []ctrf.TestResult{
				{Name: "t1", Status: ctrf.StatusPassed, Duration: 10, Suite: []string{"deploy"},
					Stdout: []string{"connected to 10.0.0.5", "https://internal.example.com"}},
				{Name: "t2", Status: ctrf.StatusFailed, Duration: 20,
					Message: "cert secret my-tls-secret not found", Stdout: []string{"line"}},
			},
		},
	}
}

func TestCTRFOmitsStdoutAndMessage(t *testing.T) {
	out, _ := redact.CTRF(sampleReport())
	for i, tr := range out.Results.Tests {
		if tr.Stdout != nil {
			t.Errorf("test[%d] stdout must be omitted, got %v", i, tr.Stdout)
		}
		if tr.Message != "" {
			t.Errorf("test[%d] message must be omitted, got %q", i, tr.Message)
		}
	}
}

func TestCTRFPreservesSignal(t *testing.T) {
	in := sampleReport()
	out, _ := redact.CTRF(in)
	if !reflect.DeepEqual(out.Results.Summary, in.Results.Summary) {
		t.Errorf("summary counts must be preserved")
	}
	if len(out.Results.Tests) != 2 {
		t.Fatalf("test count must be preserved")
	}
	if out.Results.Tests[0].Name != "t1" || out.Results.Tests[0].Status != ctrf.StatusPassed {
		t.Errorf("test name/status must be preserved")
	}
	if out.Results.Tests[1].Status != ctrf.StatusFailed {
		t.Errorf("failed status must be preserved")
	}
	if !reflect.DeepEqual(out.Results.Tests[0].Suite, []string{"deploy"}) {
		t.Errorf("suite must be preserved")
	}
}

func TestCTRFDoesNotMutateInput(t *testing.T) {
	in := sampleReport()
	_, _ = redact.CTRF(in)
	if in.Results.Tests[0].Stdout == nil || in.Results.Tests[1].Message == "" {
		t.Errorf("CTRF must not mutate its input")
	}
}

func TestCTRFAppliedRulesSortedNonEmpty(t *testing.T) {
	_, rules := redact.CTRF(sampleReport())
	if len(rules) == 0 {
		t.Fatalf("expected applied rules")
	}
	if !sortedUnique(rules) {
		t.Errorf("applied rules must be sorted and unique: %v", rules)
	}
}

func TestCTRFNilReturnsNil(t *testing.T) {
	out, rules := redact.CTRF(nil)
	if out != nil || rules != nil {
		t.Errorf("nil report should yield nil, nil")
	}
}

func sortedUnique(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] >= s[i] {
			return false
		}
	}
	return true
}
