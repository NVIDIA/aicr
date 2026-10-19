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

package recipes

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"testing"
	"text/template"

	k8syaml "sigs.k8s.io/yaml"
)

// drainTemplateAction mirrors the protobuf enum node-drainer passes, whose
// String method the template calls.
type drainTemplateAction int

func (drainTemplateAction) String() string { return "RESTART_BM" }

type drainTemplateEntity struct {
	EntityType  string
	EntityValue string
}

type drainTemplateEvent struct {
	NodeName          string
	CheckName         string
	RecommendedAction drainTemplateAction
	ErrorCode         []string
	EntitiesImpacted  []drainTemplateEntity
	Message           string
}

// drainTemplateData has the field names of node-drainer v1.28.0's
// customdrain.TemplateData.
type drainTemplateData struct {
	HealthEvent *drainTemplateEvent
	EventID     string
	PodsToDrain map[string][]string
}

// TestSlinkyDrainTemplateParsesFaultMessages renders the drain template the
// way node-drainer v1.28.0's customdrain.Client does (text/template with no
// extra functions, then sigs.k8s.io/yaml) with the messages NVSentinel's
// fatal-event monitors produce, and checks they reach the DrainRequest
// unchanged. The messages are taken from NVSentinel v1.28.0's health monitors:
// their format strings and the kernel lines in their test data.
func TestSlinkyDrainTemplateParsesFaultMessages(t *testing.T) {
	_, docs := renderSlinkyDrainer(t, nil)
	configMap := slinkyDrainerObject(t, docs, "ConfigMap")
	binaryData, _ := configMap["binaryData"].(map[string]any)
	encoded, _ := binaryData["drain-template.yaml"].(string)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode drain-template.yaml: %v", err)
	}
	tmpl, err := template.New("drain-template").Parse(string(raw))
	if err != nil {
		t.Fatalf("parse drain template: %v", err)
	}

	texts := []struct {
		name string
		text string
	}{
		{"xid with single-quoted pid", "NVRM: Xid (PCI:0000:b5:00): 13, pid='<unknown>', name=<unknown>, " +
			"Graphics SM Warp Exception on (GPC 1, TPC 3, SM 0): Out Of Range Address"},
		{"xid with process", "NVRM: Xid (PCI:0000:00:08.0): 79, pid=12345, name=test-process"},
		{"xid with register dump", "NVRM: Xid (PCI:0000:00:08.0): 145, RLW_REMAP Nonfatal XC1 i0 Link 10 " +
			"(0x00000082 0x00000040 0x00000000 0x00000000 0x00000000 0x00000000)"},
		{"dcgm power brake", "GPU 0 hardware power brake asserted for 3 consecutive poll(s) (clocks event reasons mask 0x8)"},
		{"dcgm thermal margin", "GPU 0 thermal margin 2°C below HW slowdown T.Limit (slowdown=90°C)"},
		{"nic soft lockup", "kernel soft lockup: CPU#3 stuck for 22s in mlx5 NAPI poll loop " +
			"(evidence: watchdog: BUG: soft lockup - CPU#3 stuck for 22s! [ksoftirqd/3:30])"},
		{"cancellation", "Cancelled by SysLogsXIDError error code 79"},
	}
	for _, tt := range texts {
		t.Run(tt.name, func(t *testing.T) {
			event := &drainTemplateEvent{
				NodeName:          "gpu-node-1",
				CheckName:         "GpuXidError",
				ErrorCode:         []string{"79"},
				EntitiesImpacted:  []drainTemplateEntity{{EntityType: "PCI", EntityValue: "0000:b5:00"}},
				Message:           tt.text,
				RecommendedAction: 1,
			}
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, drainTemplateData{
				HealthEvent: event,
				EventID:     "evt-1",
				PodsToDrain: map[string][]string{"slurm": {"slurm-worker-0"}},
			}); err != nil {
				t.Fatalf("execute drain template: %v", err)
			}
			var obj map[string]any
			if err := k8syaml.Unmarshal(buf.Bytes(), &obj); err != nil {
				t.Fatalf("node-drainer would fail to parse the rendered DrainRequest: %v\n%s", err, buf.String())
			}
			spec, _ := obj["spec"].(map[string]any)
			want := map[string]any{
				"nodeName":          "gpu-node-1",
				"checkName":         "GpuXidError",
				"recommendedAction": "RESTART_BM",
				"errorCode":         []any{"79"},
				"healthEventID":     "evt-1",
				"entitiesImpacted":  []any{map[string]any{"type": "PCI", "value": "0000:b5:00"}},
				"reason":            tt.text,
				"podsToDrain":       map[string]any{"slurm": []any{"slurm-worker-0"}},
			}
			if !reflect.DeepEqual(spec, want) {
				t.Errorf("rendered spec = %#v\nwant %#v", spec, want)
			}
		})
	}
}
