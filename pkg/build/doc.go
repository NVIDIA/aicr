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

// Package build defines the BuildSpec schema and the load / validate /
// write-back primitives used by the build pipeline. A BuildSpec is the
// Kubernetes-style document (apiVersion aicr.nvidia.com/v1beta1, kind
// AICRRuntime) the runtime controller consumes: spec.* captures inputs
// (recipe, version, target, registry) and status.images is populated with
// the per-image registry/repository/tag/digest after a build.
//
// LoadSpec reads a spec file from disk under a bounded size cap, Validate
// enforces required fields, and WriteBack rewrites the file with updated
// status using a temp-file + rename so partially-written specs are never
// observable. The package itself does not push images; consumers compose
// it with pkg/oci to do the actual builds.
package build
