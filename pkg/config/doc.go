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

// Package config defines the AICRConfig file schema accepted by the
// aicr CLI's --config flag on the recipe and bundle commands.
//
// AICRConfig is a Kubernetes-style envelope (kind / apiVersion / metadata / spec)
// that lets users capture flag values for both commands in a single YAML or
// JSON document. CLI flags always override values loaded from a config file;
// for slice/map flags, presence on the command line replaces the file's
// value rather than appending.
//
// Sources are restricted to local file paths and HTTP/HTTPS URLs.
// ConfigMap (cm://) URIs are intentionally rejected: extract the data with
// kubectl and pass the resulting file instead.
//
// Secrets (notably the cosign identity token) are not part of the schema;
// they must be supplied via environment variables or dedicated CLI flags.
package config
