// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package evidence

// indexTemplate renders the submission evidence index.
const indexTemplate = `# CNCF AI Conformance Evidence

**Generated:** {{ .GeneratedAt }}
**Run ID:** {{ .RunID }}

## Results

| # | Requirement | Feature | Result | Evidence |
|---|-------------|---------|--------|----------|
{{- range $i, $e := .Entries }}
| {{ inc $i }} | ` + "`{{ $e.RequirementID }}`" + ` | {{ $e.Title }} | {{ upper $e.Status }} | [{{ $e.Filename }}]({{ $e.Filename }}) |
{{- end }}
`

// evidenceTemplate renders a single evidence document.
const evidenceTemplate = `# {{ .Title }}

**Generated:** {{ .GeneratedAt }}
**Requirement:** ` + "`{{ .RequirementID }}`" + `
**Result:** {{ upper .Status }}

---

{{ .Description }}

## Checks
{{ range .Checks }}
### {{ .Name }}

- **Status:** {{ upper .Status }}
{{- if .Duration }}
- **Duration:** {{ .Duration }}
{{- end }}
{{- if .Reason }}

` + "```" + `
{{ .Reason }}
` + "```" + `
{{- end }}
{{ end }}
`
