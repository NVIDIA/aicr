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

package releasepolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// main-red-notify.yaml finds the workflows it watches by name, so a renamed
// workflow stops paging with no error anywhere, and a watched workflow that
// runs on a schedule pages only when the notify condition admits that event.
func TestMainRedNotifyWatchesNamedWorkflows(t *testing.T) {
	t.Parallel()
	notify := loadYAML(t, ".github/workflows/main-red-notify.yaml")
	watched := stringSlice(mapValue(t, mapValue(t, notify, "on"), "workflow_run")["workflows"])
	if len(watched) == 0 {
		t.Fatal("main-red-notify.yaml watches no workflow")
	}
	condition := stringValue(t, mapValue(t, mapValue(t, notify, "jobs"), "notify"), "if")

	entries, err := os.ReadDir(filepath.Join(repositoryRoot(t), ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, entry := range entries {
		if ext := filepath.Ext(entry.Name()); ext != ".yaml" && ext != ".yml" {
			continue
		}
		doc := loadYAML(t, filepath.Join(".github", "workflows", entry.Name()))
		if name, ok := doc["name"].(string); ok {
			byName[name] = doc
		}
	}
	for _, name := range watched {
		doc, ok := byName[name]
		if !ok {
			t.Errorf("main-red-notify.yaml watches %q, which no workflow is named", name)
			continue
		}
		triggers, _ := doc["on"].(map[string]any)
		if _, scheduled := triggers["schedule"]; scheduled && !strings.Contains(condition, "event == 'schedule'") {
			t.Errorf("%q runs on a schedule, but main-red-notify.yaml pages on no schedule event: %s", name, condition)
		}
	}
}
