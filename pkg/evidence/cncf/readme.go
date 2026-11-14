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

package cncf

import (
	"bytes"
	"text/template"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf/upstreamvalidator"
	"github.com/NVIDIA/aicr/pkg/evidence/redact"
)

// Platform is what the submission README records about the cluster and the
// recipe the checks ran against: the facts PRODUCT.yaml notes and a reviewer
// need, taken from the snapshot's fingerprint and the resolved recipe, not
// typed. It replaces the hand-written platform-configuration page of
// earlier submissions.
type Platform struct {
	// KubernetesVersion is the server version the snapshot recorded.
	KubernetesVersion string

	// Service, Accelerator, GPUModel, OS and OSVersion are the snapshot's
	// fingerprint dimensions; empty when not captured.
	Service     string
	Accelerator string
	GPUModel    string
	OS          string
	OSVersion   string

	// NodeCount and GPUNodeCount are the cluster's node counts; zero when
	// not captured.
	NodeCount    int
	GPUNodeCount int

	// NodeNames are the cluster's node names. They are not rendered; the
	// redaction pass aliases them wherever check output mentions one.
	NodeNames []string

	// Recipe names the resolved recipe the checks were selected from.
	Recipe RecipeInfo

	// Components are the recipe's enabled components with the versions it
	// pins. Installed versions are not collected.
	Components []Component
}

// RecipeInfo identifies the recipe of a submission run.
type RecipeInfo struct {
	Name    string
	Version string
}

// Component is one recipe component and its pinned version (chart version,
// or tag for a Kustomize component).
type Component struct {
	Name    string
	Version string
}

// Tool identifies the AICR build that produced the submission.
type Tool struct {
	Version string
	Commit  string
}

// readmeInputs is what README.md is rendered from, with every run-derived
// string already scrubbed; the vendor and platform facts are rendered as
// given.
type readmeInputs struct {
	in         SubmissionInput
	agg        *Aggregate
	rows       []productRow
	supporting []RequirementResult
	exported   []exportedTest
	redaction  []string
}

// validationState is the upstream validator outcome README.md records.
type validationState struct {
	// Outcome is one of validationAccepted, validationRejected or
	// validationNotRun.
	Outcome string
	// Detail says why for a rejected or not-run outcome.
	Detail string
}

// Validator outcomes README.md can state.
const (
	validationAccepted = "accepted"
	validationRejected = "rejected"
	validationNotRun   = "not run"
)

// readmeData is the README.md template input.
type readmeData struct {
	Vendor          Vendor
	Version         Version
	VersionExplicit bool
	Platform        Platform
	Tool            Tool
	GeneratedAt     time.Time
	Rows            []productRow
	Supporting      []RequirementResult
	Rewritten       []exportedTest
	HasArtifacts    bool
	Validator       struct {
		Repository string
		Path       string
		Commit     string
	}
	Validation validationState
	Redaction  struct {
		Policy  string
		Version string
		Rules   []string
		Report  []string
	}
}

var parsedReadmeTemplate = template.Must(template.New("readme").Funcs(templateFuncs()).Parse(readmeTemplate))

// renderReadme renders README.md, the index of the submission directory:
// the platform facts PRODUCT.yaml notes cite, the requirement table with the
// status each row carries and the result it came from, the export rule's
// rewrites, the pinned upstream validator and its outcome, and the redaction
// applied.
func renderReadme(r *readmeInputs, state validationState) ([]byte, error) {
	data := readmeData{
		Vendor:          r.in.Vendor,
		Version:         r.agg.Version,
		VersionExplicit: r.in.VersionExplicit,
		Platform:        r.in.Platform,
		Tool:            r.in.Tool,
		GeneratedAt:     r.in.Now,
		Rows:            r.rows,
		Supporting:      r.supporting,
		HasArtifacts:    len(r.exported) > 0,
		Validation:      state,
	}
	for _, e := range r.exported {
		if e.Rewritten {
			data.Rewritten = append(data.Rewritten, e)
		}
	}
	data.Validator.Repository = upstreamvalidator.UpstreamRepository
	data.Validator.Path = upstreamvalidator.UpstreamPath
	data.Validator.Commit = upstreamvalidator.UpstreamCommit
	data.Redaction.Policy = redact.PolicyName
	data.Redaction.Version = redact.PolicyVersion
	data.Redaction.Rules = (&redact.TextScrubber{}).AppliedRules()
	data.Redaction.Report = r.redaction

	var buf bytes.Buffer
	if err := parsedReadmeTemplate.Execute(&buf, data); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "render submission README", err)
	}
	return buf.Bytes(), nil
}

const readmeTemplate = `# {{ .Vendor.PlatformName }} {{ .Vendor.PlatformVersion }}: CNCF AI Conformance {{ .Version }}

This directory is a [CNCF Kubernetes AI Conformance](https://github.com/cncf/k8s-ai-conformance)
submission for {{ .Vendor.PlatformName }} by {{ .Vendor.Name }}, generated by
[NVIDIA AI Cluster Runtime (AICR)](https://github.com/NVIDIA/aicr) from one
` + "`aicr validate --phase conformance --cncf-submission`" + ` run. Every status in
[PRODUCT.yaml](PRODUCT.yaml) comes from that run's check results or from a
declaration in the validate config (a conditional MUST declared not
applicable, or public documentation attesting a requirement the run only
observed); none was typed into this file.

- **Generated:** {{ .GeneratedAt.Format "2006-01-02T15:04:05Z" }}
- **Checklist:** {{ .Version }} (` + "`docs/AIConformance-{{ trimV .Version }}.yaml`" + ` in cncf/k8s-ai-conformance){{ if .VersionExplicit }}, selected explicitly by the submitter rather than derived from the cluster's Kubernetes minor{{ end }}
- **AICR:** {{ .Tool.Version }}{{ if .Tool.Commit }} (commit ` + "`{{ .Tool.Commit }}`" + `){{ end }}
- **Recipe:** ` + "`{{ .Platform.Recipe.Name }}`" + `{{ if .Platform.Recipe.Version }} (recipe data {{ .Platform.Recipe.Version }}){{ end }}
- **Product:** [{{ .Vendor.WebsiteURL }}]({{ .Vendor.WebsiteURL }}), documentation [{{ .Vendor.DocumentationURL }}]({{ .Vendor.DocumentationURL }})
- **Kubernetes conformance:** [{{ .Vendor.K8sConformanceURL }}]({{ .Vendor.K8sConformanceURL }})

## Platform

Facts recorded by the AICR snapshot the checks ran against; PRODUCT.yaml notes
that cite a version or a count refer to these.

| Fact | Value |
| --- | --- |
| Kubernetes version | {{ or .Platform.KubernetesVersion "not captured" }} |
| Kubernetes service | {{ or .Platform.Service "not captured" }} |
| Node operating system | {{ or .Platform.OS "not captured" }}{{ if .Platform.OSVersion }} {{ .Platform.OSVersion }}{{ end }} |
| Nodes | {{ if .Platform.NodeCount }}{{ .Platform.NodeCount }}{{ else }}not captured{{ end }} |
| GPU nodes | {{ if .Platform.GPUNodeCount }}{{ .Platform.GPUNodeCount }}{{ else }}not captured{{ end }} |
| Accelerator | {{ or .Platform.Accelerator "not captured" }}{{ if and .Platform.GPUModel (ne .Platform.GPUModel .Platform.Accelerator) }} ({{ .Platform.GPUModel }}){{ end }} |
{{- if .Platform.Components }}

### Components

The recipe's components at the versions it pins, which is what AICR deploys.
Installed versions are not collected.

| Component | Version |
| --- | --- |
{{- range .Platform.Components }}
| ` + "`{{ .Name }}`" + ` | {{ or .Version "unpinned" }} |
{{- end }}
{{- end }}

## Requirements

Status is the PRODUCT.yaml status; result is the aggregate verdict of the
AICR checks expected to evidence the requirement, from which the status was
derived unless a config declaration (N/A, or documentation for an OBSERVED
result) set it (see [Requirement status](https://github.com/NVIDIA/aicr/blob/main/docs/conformance/cncf/index.md#requirement-status)).

| Requirement | Level | Status | Result | Evidence |
| --- | --- | --- | --- | --- |
{{- range .Rows }}
| ` + "`{{ .Requirement.ID }}`" + ` | {{ .Requirement.Level }} | {{ .Status }} | {{ result .Requirement.Status (cell .Requirement.Reason) }} | {{ links .Evidence }} |
{{- end }}
{{- if .Supporting }}

### Supporting evidence

Not requirements of the {{ .Version }} checklist; recorded because the checks ran.
No PRODUCT.yaml row cites them.

| Capability | Result | Evidence |
| --- | --- | --- |
{{- range .Supporting }}
| ` + "`{{ .ID }}`" + ` | {{ result .Status (cell .Reason) }} | {{ if .Page }}[evidence/{{ .Page }}](evidence/{{ .Page }}){{ end }} |
{{- end }}
{{- end }}

## Test artifacts

` + "`junit.xml`" + ` and ` + "`e2e.log`" + ` carry one testcase per AICR conformance check of this
run, named by check, exported from the same CTRF report. PRODUCT.yaml references
them as ` + "`junit.xml#<check>`" + ` and ` + "`e2e.log#<check>`" + `, so the upstream
validator resolves the named testcase rather than its upstream-test-name map.
{{- if .Rewritten }}

The raw CTRF report keeps every real verdict. In these artifacts a non-passing
check whose requirement does not hold against the run is exported as skipped,
with the original verdict in the message, and no PRODUCT.yaml row references it:

| Testcase | Original verdict | Exported as | Reason |
| --- | --- | --- | --- |
{{- range .Rewritten }}
| ` + "`{{ .Name }}`" + ` | {{ .Original }} | skipped | {{ cell .Message }} |
{{- end }}
{{- end }}

## Validation

The upstream submission validator is ` + "`{{ .Validator.Path }}`" + ` of
[{{ .Validator.Repository }}](https://{{ .Validator.Repository }}) at commit
[` + "`{{ .Validator.Commit }}`" + `](https://{{ .Validator.Repository }}/blob/{{ .Validator.Commit }}/{{ .Validator.Path }}),
run as the last step of the submission run; only a directory it accepts is
written to the submission path.
{{- if eq .Validation.Outcome "accepted" }}

**Outcome:** accepted. This directory passed it without edits.
{{- else if eq .Validation.Outcome "rejected" }}

**Outcome:** rejected; {{ .Validation.Detail }}.
{{- else }}

**Outcome:** not run; {{ .Validation.Detail }}.
{{- end }}

## Redaction

Check output is kept and the identifiers inside it are aliased, consistently
across every file of this directory, by AICR's redaction policy
` + "`{{ .Redaction.Policy }}`" + ` {{ .Redaction.Version }} (rules: {{ join .Redaction.Rules ", " }}).
{{- if .Redaction.Report }}
Applied to this directory:
{{ range .Redaction.Report }}
- {{ . }}
{{- end }}
{{- else }}
Nothing in this directory matched a rule.
{{- end }}

Node names are aliased from the snapshot's node list; a node absent from a
partial or redacted snapshot, and any other free-form identifier, can remain
in check output. Review this directory before sharing or submitting it.
`
