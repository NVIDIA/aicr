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
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/errors"
)

//go:embed templates/install-upstream-helm.sh.tmpl
var upstreamHelmTemplates embed.FS

// shellSingleQuote wraps s in single quotes for safe inclusion in a shell
// `source`-able file (e.g. KEY='value'). Embedded single quotes are escaped
// with the canonical close-escape-reopen sequence `'\”`.
//
// Single quotes (vs double quotes) are required: inside double quotes,
// `$()`, backticks, and `$VAR` still expand, so a malicious or
// pathological value could execute when sourced.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var upstreamHelmTmpl = template.Must(
	template.ParseFS(upstreamHelmTemplates, "templates/install-upstream-helm.sh.tmpl"),
)

// writeUpstreamHelmFolder writes values.yaml + cluster-values.yaml + upstream.env + install.sh
// into outputDir/dir. Returns the Folder manifest (Files are all relative to outputDir).
func writeUpstreamHelmFolder(outputDir, dir string, idx int, c Component) (Folder, error) {
	folderDir, err := deployer.SafeJoin(outputDir, dir)
	if err != nil {
		return Folder{}, errors.Wrap(errors.ErrCodeInvalidRequest, "folder path unsafe", err)
	}
	if err = os.MkdirAll(folderDir, 0o755); err != nil {
		return Folder{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("create folder %s", dir), err)
	}

	if err = writeValueFiles(folderDir, c); err != nil {
		return Folder{}, err
	}

	// For OCI charts, helm wants the full URI as the chart argument (no --repo).
	// For HTTP/HTTPS charts, helm wants the chart name + a separate --repo flag.
	// Encode that into upstream.env so install.sh can be branch-free.
	chart, repo := c.ChartName, c.Repository
	if c.IsOCI {
		chart = strings.TrimRight(c.Repository, "/") + "/" + c.ChartName
		repo = ""
	}
	// install.sh sources upstream.env; values must be shell-safe so a value
	// containing a single quote, $(...), or backticks can't escape into
	// command execution. shellSingleQuote wraps the value in single quotes
	// and replaces any embedded single quote with the four-character
	// sequence '\'' — a closed quote, an escaped quote, then a re-opened
	// one — which is the only POSIX-safe escape inside single quotes.
	envContent := fmt.Sprintf("CHART=%s\nREPO=%s\nVERSION=%s\n",
		shellSingleQuote(chart), shellSingleQuote(repo), shellSingleQuote(c.Version))
	envPath, err := deployer.SafeJoin(folderDir, "upstream.env")
	if err != nil {
		return Folder{}, errors.Wrap(errors.ErrCodeInvalidRequest, "upstream.env path unsafe", err)
	}
	if err = writeFile(envPath, []byte(envContent), 0o600); err != nil {
		return Folder{}, err
	}

	installData := struct {
		Name      string
		Namespace string
	}{c.Name, c.Namespace}
	if err = renderTemplateToFile(upstreamHelmTmpl, installData, folderDir, "install.sh", 0o755); err != nil {
		return Folder{}, err
	}

	return Folder{
		Index:  idx,
		Dir:    dir,
		Kind:   KindUpstreamHelm,
		Name:   c.Name,
		Parent: c.Name,
		Upstream: &Upstream{
			Chart:   c.ChartName,
			Repo:    c.Repository,
			Version: c.Version,
		},
		Files: []string{
			filepath.Join(dir, "values.yaml"),
			filepath.Join(dir, "cluster-values.yaml"),
			filepath.Join(dir, "upstream.env"),
			filepath.Join(dir, "install.sh"),
		},
	}, nil
}
