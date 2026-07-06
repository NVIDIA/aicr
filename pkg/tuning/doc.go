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

// Package tuning computes the nodewright tuning-status matrix: for each
// (service, accelerator) the catalog resolves, which tuning profile is applied
// and the pinned versions of the nodewright nvidia-setup / nvidia-tuned /
// nvidia-tuning-gke packages. It is hermetic and offline — every input is a pure
// read of the embedded recipe catalog and component manifests (no network, no
// cluster). tools/tuning renders the Report as Markdown; make tuning-docs
// splices it into docs/integrator/components/nodewright.md.
package tuning
