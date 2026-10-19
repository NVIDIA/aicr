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

package deployer

import (
	_ "embed"
	"strings"
)

// kubeConnection is the shell prologue that turns the bundle's connection
// environment (KUBE_CONTEXT, KUBECONFIG, and the deprecated KUBECONFIG_FLAG)
// into the two flag arrays HELM_CONN and KUBECTL_CONN.
//
// It is rendered into every generated script that talks to a cluster rather
// than sourced from a shared file, because each of those scripts is also a
// documented standalone entry point: install.sh is run directly by the e2e
// harnesses and by the command deploy.sh prints for each component, so it
// cannot depend on a sibling file having been written by whichever deployer
// assembled the bundle around it.
//
//go:embed templates/kube-connection.sh
var kubeConnection string

// licenseHeaderEnd is the last line of the Apache header the repo's license
// gate requires on every source file, including the embedded fragment.
const licenseHeaderEnd = "# limitations under the License.\n"

// KubeConnection returns the connection prologue with a trailing newline, for
// templates to interpolate via the `kubeConn` function.
//
// The fragment's own license header is dropped: every script it renders into
// already carries one, and a second Apache block partway down a generated file
// reads as the start of a new file.
func KubeConnection() string {
	body := kubeConnection
	if i := strings.Index(body, licenseHeaderEnd); i >= 0 {
		body = body[i+len(licenseHeaderEnd):]
	}
	return strings.TrimLeft(strings.TrimRight(body, "\n"), "\n") + "\n"
}
