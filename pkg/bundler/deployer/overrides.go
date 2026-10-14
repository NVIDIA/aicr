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

package deployer

// ComponentOverride carries the per-component helm flag overrides a deployer
// must apply on top of its cluster-wide defaults.
//
// Wait is the value to use for this component, not a toggle: an entry exists
// only because the component needs something other than the default, so the
// field is read directly.
type ComponentOverride struct {
	// Wait is whether to block on the release's workloads.
	Wait bool
	// TimeoutSeconds overrides the deployer's default wait timeout. Zero
	// means the default applies.
	TimeoutSeconds int
}

// componentOverrides mirrors the special cases hardcoded in
// pkg/bundler/deployer/helm/templates/deploy.sh.tmpl (ASYNC_COMPONENTS and the
// COMPONENT_HELM_TIMEOUT case block). deploy.sh and this map must be updated
// together; promoting these into the recipe schema is tracked as a follow-up
// to issue #632.
//
// Shared rather than per-deployer: this was the helmfile deployer's private
// table until the terraform deployer needed the same facts, and a third copy
// of a list that already has to be kept in sync with a shell template is how
// one of the copies silently stops matching.
var componentOverrides = map[string]ComponentOverride{
	// helm --wait times out on kai-scheduler's custom-resource readiness
	// even though every pod started.
	"kai-scheduler": {Wait: false, TimeoutSeconds: 20 * 60},
}

// ComponentOverrideFor returns the overrides for a component, and whether any
// exist. Callers key by the PARENT component so a primary release and its
// injected -pre / -post / -readiness folders inherit the same override.
func ComponentOverrideFor(component string) (ComponentOverride, bool) {
	ov, ok := componentOverrides[component]
	return ov, ok
}
