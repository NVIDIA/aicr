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

## Results

| # | Requirement | Level | Feature | Result | Evidence |
|---|-------------|-------|---------|--------|----------|
{{- range $i, $e := .Entries }}
| {{ add $i 1 }} | ` + "`{{ $e.RequirementID }}`" + ` | {{ $e.Level }} | {{ $e.Title }} | {{ upper $e.Status }} | [{{ $e.Filename }}]({{ $e.Filename }}) |
{{- end }}
{{- if .Supporting }}

## Supporting Evidence

Not requirements of the {{ .Version }} checklist; recorded because the checks ran.

| Capability | Feature | Result | Evidence |
|------------|---------|--------|----------|
{{- range $e := .Supporting }}
| ` + "`{{ $e.RequirementID }}`" + ` | {{ $e.Title }} | {{ upper $e.Status }} | [{{ $e.Filename }}]({{ $e.Filename }}) |
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
**Result:** {{ upper .Status }}

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
