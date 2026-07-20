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

// Package testgrid maps a recipe's canonical coordinate to the AICR evidence
// dashboard (validation.aicr.run) and answers "does this coordinate have a
// dashboard presence?" — the two facts RQ1 (#1283) and RQ2 (#1284) share.
//
// It is deliberately thin and owns no mapping of its own: the recipe →
// coordinate mapping is pkg/recipe.CoordinateFor (the single shared function
// consumed by GP4/GP5/TG2/RQ1). testgrid only adds the dashboard host, the
// hash-routed deep-link scheme built around Coordinate.Path, and the committed
// presence manifest.
//
// Two presence sources, one spelling of a coordinate path:
//
//   - Committed (hermetic): presence.yaml, embedded here and read offline by
//     the recipe-health generator so link *construction* never touches the
//     network. LoadPresence exposes it as a Presence set.
//   - Live: the published data/index.json the dashboard renderer boots from.
//     LivePaths extracts the present coordinate paths from a parsed index so
//     the warning-only link-check bot can compare the committed links against
//     what the dashboard actually serves.
package testgrid
