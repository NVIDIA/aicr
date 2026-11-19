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

package recipe

import "testing"

const (
	snapshotComponent  = "snapshot"
	snapshotValuesFile = "components/snapshot/values.yaml"
)

// dynamoDeclaredSnapshot maps each pinned dynamo-platform chart version to the
// Snapshot version its Chart.yaml declares under dependencies. When you bump
// dynamo-platform, add a row from the new chart and re-pin snapshot in the same
// change.
var dynamoDeclaredSnapshot = map[string]string{
	"1.6.2": "0.2.0",
}

// TestSnapshotPinFollowsDynamoPlatform fails when either pin moves without the
// other. Dynamo 1.6.2 restore breaks on Snapshot 0.3.0, and nothing else ties
// the two pins together.
func TestSnapshotPinFollowsDynamoPlatform(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	dynamo := registry.Get("dynamo-platform")
	snapshot := registry.Get(snapshotComponent)
	if dynamo == nil || snapshot == nil {
		t.Fatal("dynamo-platform and snapshot must both be in recipes/registry.yaml")
	}

	want, ok := dynamoDeclaredSnapshot[dynamo.Helm.DefaultVersion]
	if !ok {
		t.Fatalf("dynamo-platform is pinned at %s, which dynamoDeclaredSnapshot has no row for", dynamo.Helm.DefaultVersion)
	}
	if snapshot.Helm.DefaultVersion != want {
		t.Errorf("snapshot is pinned at %s, but dynamo-platform %s declares %s",
			snapshot.Helm.DefaultVersion, dynamo.Helm.DefaultVersion, want)
	}
}
