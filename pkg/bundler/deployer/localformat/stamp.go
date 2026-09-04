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

package localformat

import (
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/header"
)

// Annotations stamped into every generated wrapper Chart.yaml. Exported so
// consumers that read a Helm release back out of a cluster share one
// spelling with the templates that write them, and derived from header.Domain
// per ADR-013 so an API-domain migration cannot leave them behind.
//
// The rule for a reader is one sentence with two branches: use
// AnnotationComponentVersion when it is present, otherwise use the release's
// own chart version. Its presence is exactly the signal that the chart
// version describes the wrapper rather than the payload — an upstream chart
// installed directly carries neither annotation, and its release version IS
// the payload version (ADR-021 Decision 7).
//
// The Chart.yaml templates spell both keys literally, because a templated key
// reads far worse than the value it labels. The tests that look annotations up
// by these constants are what holds the two in sync: a domain change that
// misses the templates turns every lookup into a miss and fails them.
const (
	// AnnotationComponentVersion carries the free-form version of the
	// payload the wrapper contains.
	AnnotationComponentVersion = header.Domain + "/component-version"

	// AnnotationGeneratedBy carries the AICR build version that produced
	// the wrapper. Mirrors Chart.yaml `version:`.
	AnnotationGeneratedBy = header.Domain + "/generated-by"
)

// chartStamp carries the two versions written into every generated wrapper
// Chart.yaml.
//
// A generated wrapper's single `version:` field was being asked two
// questions at once — "what produced this artifact" and "what is inside
// it" — and answered neither, reporting a hardcoded 0.1.0 for both. ADR-021
// Decision 7 splits them.
//
// AICRVersion answers the first and lands in `version:` plus the
// aicr.run/generated-by annotation. It is Helm-valid by construction:
// deployer.NormalizeChartVersion folds an unstamped "dev" build to
// defaults.DevChartVersion, because Helm refuses to load a chart whose
// version is not SemVer 2.
//
// PayloadVersion answers the second and lands in `appVersion:` plus the
// aicr.run/component-version annotation. It is deliberately NOT normalized:
// Helm treats both of those as free-form strings, so a Kustomize ref such as
// "release-1.4" does not have to masquerade as SemVer to be reported.
type chartStamp struct {
	AICRVersion    string
	PayloadVersion string
}

// stampFor derives the Chart.yaml stamp for a generated wrapper that carries
// component c's content and was produced by AICR build aicrVersion.
//
// The payload version is the upstream chart version for Helm-typed
// components and the git ref for Kustomize-typed ones. A component with
// neither ships only AICR-authored recipe content — a manifest-only
// component, or an injected -pre / -post / -readiness folder whose parent
// has no upstream pin — so AICR's own version is the honest payload version
// there. That fallback is also why the annotation is emitted
// unconditionally: a consumer reading aicr.run/component-version never has
// to distinguish "absent" from "empty".
func stampFor(c Component, aicrVersion string) chartStamp {
	s := chartStamp{AICRVersion: deployer.NormalizeChartVersion(aicrVersion)}
	switch {
	case c.Version != "":
		s.PayloadVersion = c.Version
	case c.Tag != "":
		s.PayloadVersion = c.Tag
	default:
		s.PayloadVersion = s.AICRVersion
	}
	return s
}
