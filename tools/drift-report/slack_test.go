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
	"strings"
	"testing"
)

func payloadText(t *testing.T, r Report) string {
	t.Helper()
	b, err := SlackPayload(r)
	if err != nil {
		t.Fatalf("SlackPayload: %v", err)
	}
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	return p.Text
}

func TestSlackPayloadDrift(t *testing.T) {
	r := Report{
		SchemaVersion: schemaVersion,
		GeneratedAt:   "2026-09-21T07:00:11Z",
		RunURL:        "https://example/run/1",
		Summary:       Summary{Tracked: 34, Behind: 2, Unresolved: 1},
		Drift: []Row{
			{Components: []string{"nvsentinel"}, Chart: "nvsentinel", Current: "v1.20.0", Latest: "v1.23.0", UpdateType: "minor"},
			{Components: []string{"prometheus-adapter", "prometheus-adapter-ocp"},
				Chart: "prometheus-community/prometheus-adapter", Current: "5.3.0", Latest: "5.4.0", UpdateType: "minor"},
		},
		Unresolved: []Unresolved{{Components: []string{"dynamo-platform"}, Chart: "dynamo-platform", Reason: "lookup failed"}},
	}
	text := payloadText(t, r)
	for _, want := range []string{
		"34 pins, 2 charts behind, 1 charts unresolved",
		"https://example/run/1",
		"Review with /aicr-reviewing-component-drift (Codex: $aicr-reviewing-component-drift)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("message missing %q:\n%s", want, text)
		}
	}

	// Per-chart rows belong to drift-report.json, not the channel post.
	for _, unwanted := range []string{"nvsentinel", "prometheus-adapter", "dynamo-platform", "v1.20.0", "->", "•"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("message must not carry per-chart detail %q:\n%s", unwanted, text)
		}
	}
	if lines := strings.Count(text, "\n") + 1; lines != 3 {
		t.Errorf("digest should be header + link + call to action, got %d lines:\n%s", lines, text)
	}

	raw, err := SlackPayload(r)
	if err != nil {
		t.Fatalf("SlackPayload: %v", err)
	}
	if !strings.Contains(string(raw), `Review with /aicr-reviewing-component-drift (Codex: $aicr-reviewing-component-drift)`) {
		t.Errorf("$ form did not survive JSON encoding literally:\n%s", raw)
	}
}

func TestSlackPayloadCleanWeek(t *testing.T) {
	r := Report{
		SchemaVersion: schemaVersion,
		GeneratedAt:   "2026-09-28T07:00:09Z",
		Summary:       Summary{Tracked: 34},
	}
	text := payloadText(t, r)
	if !strings.Contains(text, "all 34 pins current") {
		t.Errorf("clean-week heartbeat missing:\n%s", text)
	}
	if strings.Contains(text, "aicr-reviewing-component-drift") {
		t.Errorf("clean-week heartbeat must not include a call to action:\n%s", text)
	}
}

func TestSlackPayloadDriftWithoutRunURL(t *testing.T) {
	r := Report{
		SchemaVersion: schemaVersion,
		GeneratedAt:   "2026-09-21T07:00:11Z",
		Summary:       Summary{Tracked: 34, Behind: 2},
		Drift: []Row{
			{Components: []string{"nvsentinel"}, Chart: "nvsentinel", Current: "v1.20.0", Latest: "v1.23.0", UpdateType: "minor"},
		},
	}
	text := payloadText(t, r)
	if !strings.Contains(text, "Review with /aicr-reviewing-component-drift (Codex: $aicr-reviewing-component-drift)") {
		t.Errorf("call to action must not depend on RunURL:\n%s", text)
	}
}

func TestSlackPayloadUnresolvedOnly(t *testing.T) {
	r := Report{
		SchemaVersion: schemaVersion,
		GeneratedAt:   "2026-09-21T07:00:11Z",
		RunURL:        "https://example/run/1",
		Summary:       Summary{Tracked: 34, Unresolved: 1},
		Unresolved:    []Unresolved{{Components: []string{"dynamo-platform"}, Chart: "dynamo-platform", Reason: "lookup failed"}},
	}
	text := payloadText(t, r)
	if !strings.Contains(text, "Review with /aicr-reviewing-component-drift (Codex: $aicr-reviewing-component-drift)") {
		t.Errorf("unresolved-only report missing call to action:\n%s", text)
	}
}
