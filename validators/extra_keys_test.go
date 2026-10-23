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

package validators

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"maps"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/redact"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// extraKeySamples maps each ExtraKey* constant to a well-formed value.
var extraKeySamples = map[string]string{
	ExtraKeyNodesValidated:      "1",
	ExtraKeyNodesTotal:          "2",
	ExtraKeySkipReason:          SkipReasonNoConstraint,
	ExtraKeyRuntimeSource:       "delivered-artifact",
	ExtraKeyAchievedGbps:        FormatDecimal(305.44),
	ExtraKeyThresholdGbps:       FormatDecimal(300),
	ExtraKeyTolerancePct:        FormatDecimal(10),
	ExtraKeyPassMargin:          FormatSignedDecimal(1.8),
	ExtraKeyWorkerNodes:         "2",
	ExtraKeyGPUsInvolved:        "16",
	ExtraKeyTransport:           "nvls",
	ExtraKeyFabric:              "efa",
	ExtraKeyDRAReady:            "8",
	ExtraKeyDRADesired:          "8",
	ExtraKeyNodewrightComplete:  "3",
	ExtraKeyNodewrightExpected:  "3",
	ExtraKeyNamespacesActive:    "6",
	ExtraKeyNamespacesExpected:  "6",
	ExtraKeyThroughputTokPerSec: FormatDecimal(4120.53),
	ExtraKeyTTFTP99Ms:           FormatDecimal(182.3),
	ExtraKeyThroughputThreshold: FormatDecimal(5000),
	ExtraKeyTTFTThresholdMs:     FormatDecimal(200),
	ExtraKeyScaledThreshold:     FormatDecimal(2500),
	ExtraKeyGPUsBenchmarked:     "4",
	ExtraKeyGPUsPerNode:         "8",
	ExtraKeyConfiguredPolicy:    "dra-resource-claim",
	ExtraKeyResolvedMode:        "device-plugin-extended-resource",
	ExtraKeyNodeLocal:           FormatBool(true),
	ExtraKeyDetectedVersion:     "v25.3.0",
	ExtraKeyVersionSatisfied:    FormatBool(false),
}

// constsWithPrefix returns the string value of each constant in extra_keys.go
// whose name starts with prefix, keyed by name.
func constsWithPrefix(t *testing.T, prefix string) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "extra_keys.go", nil, 0)
	if err != nil {
		t.Fatalf("parse extra_keys.go: %v", err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, prefix) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				v, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					t.Fatalf("%s: %v", name.Name, uerr)
				}
				out[name.Name] = v
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s* constants found", prefix)
	}
	return out
}

func redactedExtra(extra map[string]string) map[string]string {
	out, _ := redact.CTRF(&ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{{Name: "t", Extra: extra}}}})
	return out.Results.Tests[0].Extra
}

func TestExtraKeysSurviveRedaction(t *testing.T) {
	for name, key := range constsWithPrefix(t, "ExtraKey") {
		t.Run(name, func(t *testing.T) {
			sample, ok := extraKeySamples[key]
			if !ok {
				t.Fatalf("%s (%q) has no entry in extraKeySamples", name, key)
			}
			if got := redactedExtra(map[string]string{key: sample})[key]; got != sample {
				t.Errorf("%s=%q dropped by redaction. Add it to redact.ctrfExtraAllowlist", key, sample)
			}
		})
	}
}

func TestSkipReasonsSurviveRedaction(t *testing.T) {
	for name, code := range constsWithPrefix(t, "SkipReason") {
		t.Run(name, func(t *testing.T) {
			if got := redactedExtra(map[string]string{ExtraKeySkipReason: code})[ExtraKeySkipReason]; got != code {
				t.Errorf("skip code %q dropped by redaction. Add it to redact.ctrfSkipReasons", code)
			}
		})
	}
}

func TestSkipWithReason(t *testing.T) {
	var buf bytes.Buffer
	orig := extrasOut
	extrasOut = &buf
	defer func() { extrasOut = orig }()

	err := SkipWithReason(SkipReasonNoConstraint, "no foo constraint in recipe")
	if !IsSkip(err) {
		t.Fatalf("SkipWithReason() = %v, want a skip", err)
	}
	if !strings.Contains(err.Error(), "no foo constraint in recipe") {
		t.Errorf("skip error %q lost its reason", err)
	}
	payload, ok := strings.CutPrefix(strings.TrimSuffix(buf.String(), "\n"), ctrf.ExtraLinePrefix)
	if !ok {
		t.Fatalf("output %q missing extra sentinel", buf.String())
	}
	var got map[string]string
	if jerr := json.Unmarshal([]byte(payload), &got); jerr != nil {
		t.Fatalf("payload %q: %v", payload, jerr)
	}
	if got[ExtraKeySkipReason] != SkipReasonNoConstraint || len(got) != 1 {
		t.Errorf("extra = %v, want only skipReason=%s", got, SkipReasonNoConstraint)
	}
}

func TestEmitExtraOrWarnSwallowsWriteError(t *testing.T) {
	orig := extrasOut
	extrasOut = errWriter{}
	defer func() { extrasOut = orig }()
	var logs bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(origLogger)

	EmitExtraOrWarn(map[string]string{ExtraKeyNodesTotal: "1"})
	if !strings.Contains(logs.String(), "failed to emit validator extra") {
		t.Errorf("write failure not logged, got %q", logs.String())
	}
}

func TestFormatDecimal(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{305.44, "305.4"},
		{305.45, "305.5"},
		{300, "300"},
		{0, "0"},
		{-0.04, "0"},
		{0.3, "0.3"},
		{-1, "-1"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "+Inf"},
	}
	for _, tt := range tests {
		if got := FormatDecimal(tt.in); got != tt.want {
			t.Errorf("FormatDecimal(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatSignedDecimal(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{1.8, "+1.8"},
		{-12.34, "-12.3"},
		{0, "+0"},
		{-0.04, "+0"},
		{100, "+100"},
	}
	for _, tt := range tests {
		if got := FormatSignedDecimal(tt.in); got != tt.want {
			t.Errorf("FormatSignedDecimal(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGPUAllocationPolicyExtra(t *testing.T) {
	got := GPUAllocationPolicyExtra(v1.GPUAllocationPolicyDRAResourceClaim)
	want := map[string]string{ExtraKeyConfiguredPolicy: "dra-resource-claim"}
	if !maps.Equal(got, want) {
		t.Errorf("GPUAllocationPolicyExtra() = %v, want %v", got, want)
	}
	if !maps.Equal(redactedExtra(got), want) {
		t.Errorf("redacted policy extra = %v, want %v", redactedExtra(got), want)
	}
}

func TestGPUAllocationResolvedExtra(t *testing.T) {
	tests := []struct {
		name      string
		dra       bool
		nodeLocal bool
		want      map[string]string
	}{
		{"node-local DRA wiring", true, true,
			map[string]string{ExtraKeyResolvedMode: "dra-resource-claim", ExtraKeyNodeLocal: "true"}},
		{"cluster-wide device plugin", false, false,
			map[string]string{ExtraKeyResolvedMode: "device-plugin-extended-resource", ExtraKeyNodeLocal: "false"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GPUAllocationResolvedExtra(tt.dra, tt.nodeLocal)
			if !maps.Equal(got, tt.want) {
				t.Errorf("GPUAllocationResolvedExtra() = %v, want %v", got, tt.want)
			}
			if !maps.Equal(redactedExtra(got), tt.want) {
				t.Errorf("redacted resolved extra = %v, want %v", redactedExtra(got), tt.want)
			}
		})
	}
}
