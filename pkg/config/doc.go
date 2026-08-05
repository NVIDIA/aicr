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
// aicr CLI's --config flag on the snapshot, recipe, bundle, validate, and
// verify commands.
//
// AICRConfig is a Kubernetes-style envelope (kind / apiVersion / metadata / spec)
// that lets users capture flag values for these commands in a single YAML or
// JSON document. Each per-command section under spec (snapshot, recipe,
// bundle, validate, verify) is optional, so a config file may populate just
// one section or any combination for end-to-end workflows. CLI flags always
// override values loaded from a config file; for slice/map flags, presence
// on the command line replaces the file's value rather than appending.
//
// The first four sections are the producer pipeline; spec.verify is the
// consumer side, so one committed document can describe both how an artifact
// is built and the trust floor a downstream consumer enforces against it.
//
// Sources are restricted to local file paths and HTTP/HTTPS URLs.
// ConfigMap (cm://) URIs are intentionally rejected: extract the data with
// kubectl and pass the resulting file instead.
//
// Secrets (notably the cosign identity token) are not part of the schema;
// they must be supplied via environment variables or dedicated CLI flags.
// Durable, non-secret references are in scope by contrast: the private
// Sigstore endpoints (spec.bundle.attestation.fulcioURL / rekorURL), the
// KMS signing-key reference (spec.bundle.attestation.signingKey), and their
// verify-side counterparts (spec.verify.trust.key / trustRoot) all belong in
// version-controlled config even though they configure signing or its
// verification.
package config
