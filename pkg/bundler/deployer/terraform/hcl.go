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

package terraform

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// hclString renders s as an HCL quoted string literal.
//
// Beyond the usual backslash and double-quote escaping, HCL quoted strings
// interpret ${...} as a template interpolation and %{...} as a template
// directive. A chart version or repository URL containing either would
// otherwise become an evaluation error at plan time — or, worse, evaluate.
// Both are escaped by doubling the introducer, which is HCL's own rule.
func hclString(s string) string {
	return `"` + escapeHCL(s) + `"`
}

// escapeHCL is hclString's body without the quotes: what a value needs so
// that it cannot end the quoted string it is placed in, nor start an
// interpolation. Callers that build a string around a live ${path.module}
// reference use it on the variable part only.
func escapeHCL(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
		"${", `$${`,
		"%{", `%%{`,
	)
	return r.Replace(s)
}

// modulePath renders a path under the bundle root as a quoted string that
// keeps the ${path.module} interpolation live and escapes the rest, so a
// folder name cannot close the string or open a second interpolation.
// path.module keeps the reference relative to the bundle root, so the bundle
// stays relocatable and works as a module inside a larger configuration.
func modulePath(rel string) string {
	return `"${path.module}/` + escapeHCL(rel) + `"`
}

// localChartExpr renders the chart argument for a folder that carries its own
// Chart.yaml.
func localChartExpr(dir string) string {
	return modulePath(dir)
}

// commentText makes s safe to emit after a `#` on one line: every control
// character, newlines included, becomes a space. A newline in a value would
// otherwise end the comment and hand the rest of the value to the HCL parser
// as top-level configuration.
func commentText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// hasControl reports whether s contains a control character (C0, C1 or DEL).
func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// errIdentifierCollision reports two release names that sanitize to the same
// Terraform module label. Module labels are a namespace this package invents,
// so a collision is caught here rather than surfacing as a duplicate-block
// error from terraform init on a bundle AICR claimed to have generated
// successfully.
func errIdentifierCollision(first, second, label string) error {
	return errors.New(errors.ErrCodeInvalidRequest,
		fmt.Sprintf("releases %q and %q both map to Terraform module label %q; "+
			"rename one of the components", first, second, label))
}
