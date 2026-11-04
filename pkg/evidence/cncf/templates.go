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

const indexTemplate = `# CNCF AI Conformance Evidence

**Generated:** {{ .GeneratedAt.Format "2006-01-02T15:04:05Z" }}
**Checklist:** {{ .Version }}

**Applicable MUSTs:** {{ if .Passed }}PASSED{{ else }}FAILED{{ end }}

## Results

| # | Requirement | Level | Feature | Result | Evidence |
|---|-------------|-------|---------|--------|----------|
{{- range $i, $r := .Rows }}
| {{ add $i 1 }} | ` + "`{{ $r.RequirementID }}`" + ` | {{ $r.Level }} | {{ if $r.Title }}{{ $r.Title }} {{ end }}| {{ result $r.Status $r.Reason }} | {{ if $r.Filename }}[{{ $r.Filename }}]({{ $r.Filename }}) {{ end }}|
{{- end }}
{{- if .Supporting }}

## Supporting Evidence

Not requirements of the {{ .Version }} checklist; recorded because the checks ran.

| Capability | Feature | Result | Evidence |
|------------|---------|--------|----------|
{{- range $e := .Supporting }}
| ` + "`{{ $e.RequirementID }}`" + ` | {{ $e.Title }} | {{ result $e.Status $e.Reason }} | [{{ $e.Filename }}]({{ $e.Filename }}) |
{{- end }}
{{- end }}
`

const evidenceTemplate = `# {{ .Title }}

**Generated:** {{ .GeneratedAt.Format "2006-01-02T15:04:05Z" }}
**Checklist:** {{ .Version }}
{{- if .Supporting }}
**Supporting evidence:** ` + "`{{ .RequirementID }}`" + ` is not a requirement of the {{ .Version }} checklist
{{- else }}
**Requirement:** ` + "`{{ .RequirementID }}`" + ` ({{ .Level }})
{{- end }}
**Result:** {{ result .Status .Reason }}
{{- if .Proof }}
**Proof:** {{ join .Proof ", " }}
{{- end }}

---

{{ .Description }}

## Checks
{{ range .Checks }}
### {{ .Name }}

- **Status:** {{ upper .Status }}
- **Duration:** {{ .Duration }}ms
{{- if .Message }}

**Error:**
` + "```" + `
{{ .Message }}
` + "```" + `
{{- end }}
{{- if .Stdout }}

**Evidence Output:**
` + "```" + `
{{ join .Stdout "\n" }}
` + "```" + `
{{- end }}
{{ end }}
`
