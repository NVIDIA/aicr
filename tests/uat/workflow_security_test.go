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

package uat

import (
	_ "crypto/sha256" // Register SHA-256 for github.com/opencontainers/go-digest.
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	"gopkg.in/yaml.v3"
)

const (
	workflowsDir = "../../.github/workflows"
	actuatorRoot = "ghcr.io/mchmarny/cluster/"
	pinnedGKERef = "${{ env.GKE_ACTUATOR_IMAGE }}"
)

type actuatorExpectation struct {
	name       string
	file       string
	job        string
	envVar     string
	repository string
}

var actuatorExpectations = []actuatorExpectation{
	{"AWS", "uat-aws.yaml", "uat-aws", "EKS_IMAGE", "ghcr.io/mchmarny/cluster/eks"},
	{"Azure", "uat-azure.yaml", "uat-azure", "AKS_ACTUATOR_IMAGE", "ghcr.io/mchmarny/cluster/aks"},
	{"GCP", "uat-gcp.yaml", "uat-gcp", "GKE_ACTUATOR_IMAGE", "ghcr.io/mchmarny/cluster/gke"},
}

type credentialApplyExpectation struct {
	name       string
	authOutput string
}

var credentialApplyExpectations = []credentialApplyExpectation{
	{"bringup", "${{ steps.auth.outputs.credentials_file_path }}"},
	{"teardown", "${{ steps.auth_teardown.outputs.credentials_file_path }}"},
}

var actuatorStepNames = []string{"Bringup Infra", "Destroy Cluster"}

type workflowDocument struct {
	Env      map[string]string      `yaml:"env"`
	Defaults workflowDefaults       `yaml:"defaults"`
	Jobs     map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Env      map[string]string `yaml:"env"`
	Defaults workflowDefaults  `yaml:"defaults"`
	Steps    []workflowStep    `yaml:"steps"`
}

type workflowDefaults struct {
	Run workflowRunDefaults `yaml:"run"`
}

type workflowRunDefaults struct {
	Shell string `yaml:"shell"`
}

type workflowStep struct {
	Name  string            `yaml:"name"`
	Run   string            `yaml:"run"`
	Shell string            `yaml:"shell"`
	Env   map[string]string `yaml:"env"`
}

type dockerInvocation struct {
	image     string
	arguments []string
}

func TestUATActuatorInvocationsArePinnedApplyCommands(t *testing.T) {
	for _, tt := range actuatorExpectations {
		t.Run(tt.name, func(t *testing.T) {
			node := loadWorkflow(t, tt.file)
			var workflow workflowDocument
			if err := node.Decode(&workflow); err != nil {
				t.Fatalf("decode %s: %v", tt.file, err)
			}

			job, ok := workflow.Jobs[tt.job]
			if !ok {
				t.Fatalf("%s: missing job %q", tt.file, tt.job)
			}
			ref, ok := job.Env[tt.envVar]
			if !ok || ref == "" {
				t.Fatalf("%s: job %q missing %s", tt.file, tt.job, tt.envVar)
			}

			if err := parsePinnedActuatorReference(ref, tt.repository); err != nil {
				t.Errorf("%s must be an immutable %s SHA-256 reference: %v", tt.envVar, tt.repository, err)
			}
			for _, step := range job.Steps {
				if _, overrides := step.Env[tt.envVar]; overrides {
					t.Errorf("step %q must not override job-level %s", step.Name, tt.envVar)
				}
			}

			expectedImage := fmt.Sprintf("${{ env.%s }}", tt.envVar)
			for _, stepName := range actuatorStepNames {
				step := uniqueStepNamed(t, job.Steps, stepName)
				invocations, err := parseDockerInvocations(step.Run)
				if err != nil {
					t.Fatalf("parse Docker runs in step %q: %v", step.Name, err)
				}
				if len(invocations) != 1 {
					t.Fatalf("step %q contains %d Docker runs, want exactly one", step.Name, len(invocations))
				}
				invocation := invocations[0]
				if invocation.image != expectedImage {
					t.Errorf("step %q uses Docker image %q, want pinned job image %q", step.Name, invocation.image, expectedImage)
				}
				if !slices.Equal(invocation.arguments, []string{"apply"}) {
					t.Errorf("step %q uses actuator arguments %q, want exactly [apply]", step.Name, invocation.arguments)
				}
			}
		})
	}
}

func TestParsePinnedActuatorReference(t *testing.T) {
	const repository = "ghcr.io/mchmarny/cluster/gke"
	const hash = "f586ffa14dccb867c81fb8a12484f6e31d3adad93242292b7c9cc00c93af2367"
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"digest only", repository + "@sha256:" + hash, false},
		{"tag and digest", repository + ":v0.5.16@sha256:" + hash, false},
		{"tag only", repository + ":v0.5.16", true},
		{"short digest", repository + "@sha256:abcd", true},
		{"uppercase digest", repository + "@sha256:" + strings.ToUpper(hash), true},
		{"wrong repository", "ghcr.io/mchmarny/cluster/eks@sha256:" + hash, true},
		{"trailing suffix", repository + "@sha256:" + hash + "-extra", true},
		{"trailing reference", repository + "@sha256:" + hash + " " + repository + ":latest", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parsePinnedActuatorReference(tt.value, repository)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePinnedActuatorReference(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestUATWorkflowsContainNoMutableActuatorReferences(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(workflowsDir, "uat-*.yaml"))
	if err != nil {
		t.Fatalf("glob UAT workflows: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no UAT workflows found")
	}

	for _, path := range paths {
		file := filepath.Base(path)
		node := loadWorkflow(t, file)
		walkStringScalars(node, func(value string) {
			refs, parseErr := actuatorReferenceTokens(value)
			if parseErr != nil {
				t.Errorf("%s contains an unparseable actuator reference command: %v", file, parseErr)
				return
			}
			for _, raw := range refs {
				named, err := reference.ParseNormalizedNamed(raw)
				if err != nil {
					t.Errorf("%s contains malformed actuator reference %q: %v", file, raw, err)
					continue
				}
				if err := parsePinnedActuatorReference(raw, named.Name()); err != nil {
					t.Errorf("%s contains mutable actuator reference %q: %v", file, raw, err)
				}
			}
		})
	}
}

func TestActuatorReferenceTokens(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"plain reference", `docker run ghcr.io/mchmarny/cluster/gke:latest apply`, []string{"ghcr.io/mchmarny/cluster/gke:latest"}},
		{"adjacent quoted fragments", `docker run ghcr.io/mchmarny/clu''ster/gke:latest apply`, []string{"ghcr.io/mchmarny/cluster/gke:latest"}},
		{"split registry owner", `docker run ghcr.io/mch''marny/cluster/gke:latest apply`, []string{"ghcr.io/mchmarny/cluster/gke:latest"}},
		{"ANSI-C encoded dot", `docker run ghcr$'\x2e'io/mchmarny/cluster/gke:latest apply`, []string{"ghcr.io/mchmarny/cluster/gke:latest"}},
		{"ANSI-C encoded slash", `docker run ghcr.io$'\x2f'mchmarny/cluster/gke:latest apply`, []string{"ghcr.io/mchmarny/cluster/gke:latest"}},
		{"ANSI-C octal punctuation", `docker run ghcr$'\056'io$'\057'mchmarny/cluster/gke:latest apply`, []string{"ghcr.io/mchmarny/cluster/gke:latest"}},
		{"ANSI-C octal width", `docker run ghcr$'\0056'io/mchmarny/cluster/gke:latest apply`, nil},
		{"ANSI-C Unicode punctuation", `docker run ghcr$'\u002e'io$'\u002f'mchmarny/cluster/gke:latest apply`, []string{"ghcr.io/mchmarny/cluster/gke:latest"}},
		{"fully ANSI-C encoded reference", `docker run $'\x67\x68\x63\x72\x2e\x69\x6f\x2f\x6d\x63\x68\x6d\x61\x72\x6e\x79\x2f\x63\x6c\x75\x73\x74\x65\x72\x2f\x67\x6b\x65\x3a\x6c\x61\x74\x65\x73\x74' apply`, []string{"ghcr.io/mchmarny/cluster/gke:latest"}},
		{"commented reference", `# docker run ghcr.io/mchmarny/cluster/gke:latest apply`, nil},
		{"heredoc reference", "cat <<EOF\ndocker run ghcr.io/mchmarny/cluster/gke:latest apply\nEOF", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := actuatorReferenceTokens(tt.text)
			if err != nil {
				t.Fatalf("actuatorReferenceTokens() error = %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("actuatorReferenceTokens() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestActuatorReferenceTokensRejectsContinuedToken(t *testing.T) {
	value := "docker run ghcr.io/mchmar\\\nny/cluster/gke:latest apply"
	if _, err := actuatorReferenceTokens(value); err == nil {
		t.Fatal("actuatorReferenceTokens() error = nil, want unsupported continued token error")
	}
}

func TestActuatorReferenceTokensRejectsMalformedANSIQuoting(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"malformed hex escape", `docker run ghcr$'\x'io/mchmarny/cluster/gke:latest apply`},
		{"unsupported escape", `docker run ghcr$'\q'io/mchmarny/cluster/gke:latest apply`},
		{"NUL escape", `docker run ghcr$'\x00'io/mchmarny/cluster/gke:latest apply`},
		{"Unicode surrogate", `docker run ghcr$'\uD800'io/mchmarny/cluster/gke:latest apply`},
		{"unterminated quote", `docker run ghcr$'\x2eio/mchmarny/cluster/gke:latest apply`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := actuatorReferenceTokens(tt.value); err == nil {
				t.Fatal("actuatorReferenceTokens() error = nil, want malformed ANSI-C quote error")
			}
		})
	}
}

func TestActuatorReferenceTokensIgnoresUnrelatedANSIQuoting(t *testing.T) {
	value := `s=${s//$'\r'/'%0D'}; s=${s//$'\n'/'%0A'}`
	refs, err := actuatorReferenceTokens(value)
	if err != nil {
		t.Fatalf("actuatorReferenceTokens() error = %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("actuatorReferenceTokens() = %v, want no references", refs)
	}
}

func TestGKECredentialApplyStepsUsePinnedImageAndExpectedAuth(t *testing.T) {
	workflow := decodeWorkflow(t, "uat-gcp.yaml")
	job, ok := workflow.Jobs["uat-gcp"]
	if !ok {
		t.Fatal("uat-gcp.yaml: missing uat-gcp job")
	}
	credentialSteps := stepsWithDockerRun(t, job.Steps)
	if len(credentialSteps) != len(credentialApplyExpectations) {
		t.Fatalf("found %d credential-bearing steps, want %d", len(credentialSteps), len(credentialApplyExpectations))
	}
	seen := make(map[string]bool, len(credentialApplyExpectations))
	for _, step := range credentialSteps {
		matched := make([]credentialApplyExpectation, 0, 1)
		for _, expected := range credentialApplyExpectations {
			environment := `KEY_CONTENT="$(base64 < ` + expected.authOutput + `)"`
			if activeDockerRunMatches(step.Run, pinnedGKERef+" apply", environment) {
				matched = append(matched, expected)
			}
		}
		if len(matched) != 1 {
			t.Fatalf("step %q matched %d expected auth outputs, want exactly one", step.Name, len(matched))
		}
		expected := matched[0]
		if seen[expected.name] {
			t.Fatalf("auth output for %s is used by more than one credential step", expected.name)
		}
		seen[expected.name] = true
	}
	for _, expected := range credentialApplyExpectations {
		if !seen[expected.name] {
			t.Errorf("missing credential apply step for %s", expected.name)
		}
	}
}

func TestCredentialBearingUATStepsDisableXtrace(t *testing.T) {
	tests := []struct {
		name        string
		file        string
		job         string
		selectSteps func(*testing.T, []workflowStep) []workflowStep
	}{
		{
			"AWS", "uat-aws.yaml", "uat-aws",
			func(t *testing.T, steps []workflowStep) []workflowStep {
				selected := make([]workflowStep, 0, len(actuatorStepNames))
				for _, stepName := range actuatorStepNames {
					selected = append(selected, uniqueStepNamed(t, steps, stepName))
				}
				return selected
			},
		},
		{
			"GCP", "uat-gcp.yaml", "uat-gcp",
			func(t *testing.T, steps []workflowStep) []workflowStep {
				selected := make([]workflowStep, 0, len(credentialApplyExpectations))
				for _, expected := range credentialApplyExpectations {
					selected = append(selected, uniqueStepUsing(t, steps, expected.authOutput))
				}
				return selected
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflow := decodeWorkflow(t, tt.file)
			job, ok := workflow.Jobs[tt.job]
			if !ok {
				t.Fatalf("%s: missing %s job", tt.file, tt.job)
			}
			if shellEnvironmentEnablesXtrace(workflow.Env) || shellEnvironmentEnablesXtrace(job.Env) {
				t.Fatal("workflow or job environment can enable xtrace for credential-bearing steps")
			}
			if shellEnablesXtrace(workflow.Defaults.Run.Shell) || shellEnablesXtrace(job.Defaults.Run.Shell) {
				t.Fatal("workflow or job default shell enables xtrace for credential-bearing steps")
			}
			for _, step := range tt.selectSteps(t, job.Steps) {
				if shellEnablesXtrace(step.Run) || shellEnablesXtrace(step.Shell) || shellEnvironmentEnablesXtrace(step.Env) {
					t.Errorf("step %q enables xtrace while handling credentials", step.Name)
				}
			}
		})
	}
}

func TestShellEnvironmentEnablesXtrace(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"empty", nil, false},
		{"ordinary values", map[string]string{"CONFIG": "test"}, false},
		{"shell options", map[string]string{"SHELLOPTS": "errexit:xtrace"}, true},
		{"bash environment", map[string]string{"BASH_ENV": "/tmp/enable-xtrace"}, true},
		{"POSIX environment", map[string]string{"ENV": "/tmp/enable-xtrace"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellEnvironmentEnablesXtrace(tt.env); got != tt.want {
				t.Fatalf("shellEnvironmentEnablesXtrace() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShellEnablesXtrace(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"strict without xtrace", "set -euo pipefail", false},
		{"explicit disable", "set +x", false},
		{"short", "set -x", true},
		{"bundled", "set -euox pipefail", true},
		{"long", "set -o xtrace", true},
		{"long after another option", "set -o errexit -o xtrace", true},
		{"mid command", "echo ready; set -x; docker run", true},
		{"later AWS xtrace", "set -euo pipefail\nfor attempt in 1 2 3; do\nset -x\n/usr/bin/docker run image apply\ndone", true},
		{"bash short", "bash -x {0}", true},
		{"bash bundled", "bash --noprofile -euxo pipefail {0}", true},
		{"bash command with xtrace", `bash -xc 'echo ready'`, true},
		{"bash long", "bash --noprofile --xtrace {0}", true},
		{"nested bash command", `bash -c 'set -x; docker run'`, true},
		{"comment", "# set -x", false},
		{"inline comment", "echo ready # set -x", false},
		{"quoted keyword concatenation", `s""et -x`, true},
		{"ANSI-C quoted keyword", `$'set' -x`, true},
		{"dynamic keyword", `s$(:)et -x`, true},
		{"dynamic option variable", "flag=-x\nset \"$flag\"", true},
		{"dynamic command variable", "cmd=set\n\"$cmd\" -x", true},
		{"dynamic command after separator", "cmd=set\necho ready; \"$cmd\" -x", true},
		{"dynamic command after adjacent separator", "cmd=set\necho ready;\"$cmd\" -x", true},
		{"dynamic command after time prefix", "cmd=set\ntime \"$cmd\" -x", true},
		{"dynamic command after time option", "cmd=set\ntime -p \"$cmd\" -x", true},
		{"dynamic command through builtin", "cmd=set\nbuiltin \"$cmd\" -x", true},
		{"dynamic command after attached redirection", "cmd=set\n>/tmp/xtrace.log \"$cmd\" -x", true},
		{"dynamic command after separate redirection", "cmd=set\n> /tmp/xtrace.log \"$cmd\" -x", true},
		{"glob expanded option", "touch -- -x\nset *", true},
		{"glob expanded command", `/bin/ba?? -xc 'echo ready'`, true},
		{"quoted separator argument", `echo "ready; $value"`, false},
		{"literal command words as arguments", `echo set -x`, false},
		{"escaped option", `set -\x`, true},
		{"command substitution", `KEY_CONTENT="$(set -x; base64 < secret)"`, true},
		{"eval command", `eval 'set -x'`, true},
		{"DEBUG trap", `trap 'set -x' DEBUG`, true},
		{"split keyword", "se\\\nt -x", true},
		{"heredoc body", "cat <<EOF\nset -x\nEOF\nset -euo pipefail", false},
		{"quoted heredoc body", "cat <<'EOF'\nset -x\nEOF\nset -euo pipefail", false},
		{"active after heredoc", "cat <<-EOF\n\tset -e\n\tEOF\nset -x", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellEnablesXtrace(tt.text); got != tt.want {
				t.Fatalf("shellEnablesXtrace(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestParseDockerInvocations(t *testing.T) {
	tests := []struct {
		name    string
		run     string
		want    []dockerInvocation
		wantErr bool
	}{
		{"quoted environment", `/usr/bin/docker run -e AUTO_APPROVE=true "${EKS_IMAGE}" apply`, []dockerInvocation{{"${EKS_IMAGE}", []string{"apply"}}}, false},
		{"GitHub environment", `/usr/bin/docker run ${{ env.GKE_ACTUATOR_IMAGE }} apply`, []dockerInvocation{{pinnedGKERef, []string{"apply"}}}, false},
		{"conditional with volume", `if /usr/bin/docker run -v "$AZ_MOUNT:/data" "${AKS_ACTUATOR_IMAGE}" apply; then`, []dockerInvocation{{"${AKS_ACTUATOR_IMAGE}", []string{"apply"}}}, false},
		{"adjacent quoted mutable image", `/usr/bin/docker run ghcr.io/mchmarny/clu''ster/gke:latest apply`, []dockerInvocation{{"ghcr.io/mchmarny/cluster/gke:latest", []string{"apply"}}}, false},
		{"container run mutable image", `/usr/bin/docker container run ghcr.io/mchmarny/cluster/gke:latest apply`, []dockerInvocation{{"ghcr.io/mchmarny/cluster/gke:latest", []string{"apply"}}}, false},
		{"destroy command", `/usr/bin/docker run "${EKS_IMAGE}" destroy`, []dockerInvocation{{"${EKS_IMAGE}", []string{"destroy"}}}, false},
		{"missing command", `/usr/bin/docker run "${EKS_IMAGE}"`, []dockerInvocation{{"${EKS_IMAGE}", nil}}, false},
		{"extra command argument", `/usr/bin/docker run "${EKS_IMAGE}" apply unexpected`, []dockerInvocation{{"${EKS_IMAGE}", []string{"apply", "unexpected"}}}, false},
		{"PATH-resolved Docker", `docker run "${EKS_IMAGE}" apply`, nil, true},
		{"commented run", `# /usr/bin/docker run ghcr.io/mchmarny/cluster/gke:latest apply`, nil, false},
		{"unsupported option", `/usr/bin/docker run --privileged "${EKS_IMAGE}" apply`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDockerInvocations(tt.run)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDockerInvocations() error = %v, wantErr %v", err, tt.wantErr)
			}
			matches := slices.EqualFunc(got, tt.want, func(got, want dockerInvocation) bool {
				return got.image == want.image && slices.Equal(got.arguments, want.arguments)
			})
			if !matches {
				t.Fatalf("parseDockerInvocations() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExecutableShellLinesRejectUnterminatedHeredoc(t *testing.T) {
	if _, err := executableShellLines("cat <<EOF\nset -x"); err == nil {
		t.Fatal("executableShellLines() error = nil, want unterminated heredoc error")
	}
}

func TestCredentialRunMatching(t *testing.T) {
	authOutput := credentialApplyExpectations[0].authOutput
	tests := []struct {
		name      string
		run       string
		wantAuth  bool
		wantApply bool
	}{
		{"active lines", "-e KEY_CONTENT=\"$(base64 < " + authOutput + ")\" \\\n" + pinnedGKERef + " apply", true, true},
		{"conditional continuation", pinnedGKERef + " apply; then", false, true},
		{"conditional apply", "if " + pinnedGKERef + " apply; then", false, true},
		{"commented markers", "# KEY_CONTENT=" + authOutput + "\n# " + pinnedGKERef + " apply", false, false},
		{"inline commented markers", "echo ready # KEY_CONTENT=" + authOutput + "\necho ready # " + pinnedGKERef + " apply", false, false},
		{"heredoc markers", "cat <<'EOF'\nKEY_CONTENT=" + authOutput + "\n" + pinnedGKERef + " apply\nEOF", false, false},
		{"active after heredoc", "cat <<EOF\nKEY_CONTENT=wrong\nmutable apply\nEOF\nKEY_CONTENT=" + authOutput + "\n" + pinnedGKERef + " apply", true, true},
		{"auth marker on another line", "KEY_CONTENT=wrong\necho " + authOutput + "\n" + pinnedGKERef + " apply", false, true},
		{"echoed apply marker", "KEY_CONTENT=" + authOutput + "\necho " + pinnedGKERef + " apply", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := activeRunLineContainsAll(tt.run, "KEY_CONTENT=", authOutput); got != tt.wantAuth {
				t.Errorf("auth match = %v, want %v", got, tt.wantAuth)
			}
			if got := activeRunInvokesExactCommand(tt.run, pinnedGKERef+" apply"); got != tt.wantApply {
				t.Errorf("apply match = %v, want %v", got, tt.wantApply)
			}
		})
	}
}

func TestActiveDockerRunMatches(t *testing.T) {
	authOutput := credentialApplyExpectations[0].authOutput
	environment := `KEY_CONTENT="$(base64 < ` + authOutput + `)"`
	validRun := "/usr/bin/docker run \\\n  -e " + environment + " \\\n  " + pinnedGKERef + " apply"
	tests := []struct {
		name string
		run  string
		want bool
	}{
		{"continued command", validRun, true},
		{"inline conditional", "if " + validRun + "; then\n  echo ready\nfi", true},
		{"auth on another command", "echo " + environment + "\ndocker run " + pinnedGKERef + " apply", false},
		{"pinned marker on another command", "docker run -e " + environment + " mutable.example/gke:latest apply\necho " + pinnedGKERef + " apply", false},
		{"pinned marker in environment", "docker run -e " + environment + " -e NOTE='" + pinnedGKERef + " apply' mutable.example/gke:latest apply", false},
		{"pinned marker after mutable image", "docker run mutable.example/gke:latest -e " + environment + " " + pinnedGKERef + " apply", false},
		{"safe decoy after mutable run", "docker run -e " + environment + " mutable.example/gke:latest apply\n" + validRun, false},
		{"unsafe run after separator", validRun + "\necho ready; docker run -e " + environment + " mutable.example/gke:latest apply", false},
		{"split docker keyword", validRun + "\ndock\\\ner run -e " + environment + " mutable.example/gke:latest apply", false},
		{"split heredoc operator", "cat <\\\n<EOF\n" + validRun + "\nEOF", false},
		{"arithmetic shift decoy", "echo=0\n" + validRun + "\n(( x = 1 << echo ))\ndocker run -e " + environment + " mutable.example/gke:latest apply\necho", false},
		{"continued arithmetic shift decoy", "echo=0\n" + validRun + "\n(( x = 1 \\\n<< echo ))\ndocker run -e " + environment + " mutable.example/gke:latest apply\necho", false},
		{"equals environment option", "docker run -e=" + environment + " mutable.example/gke:latest apply\n" + validRun, false},
		{"duplicate credential environment", "docker run -e " + environment + " -e KEY_CONTENT=wrong " + pinnedGKERef + " apply", false},
		{"inherited credential environment", "docker run -e KEY_CONTENT mutable.example/gke:latest apply\n" + validRun, false},
		{"constructed credential name", "docker run -e 'KEY_'CONTENT=wrong mutable.example/gke:latest apply\n" + validRun, false},
		{"literal backslash in credential name", "docker run -e \"KEY\\_CONTENT=$(base64 < " + authOutput + ")\" " + pinnedGKERef + " apply", false},
		{"dynamic credential and image", "docker run -e KEY_$(:)CONTENT=\"$(base64 < " + authOutput + ")\" ghcr.io/mchmarny/clu''ster/gke:latest apply\n" + validRun, false},
		{"dynamic docker command", "$(:)docker r$(:)un -e KEY_$(:)CONTENT=\"$(base64 < " + authOutput + ")\" ghcr.io/mchmarny/clu''ster/gke:latest apply\n" + validRun, false},
		{"parameter-expanded docker command", "X=\n${X}docker run -e KEY_${X}CONTENT=wrong ghcr.io/mchmarny/clu''ster/gke:latest apply\n" + validRun, false},
		{"wrapped mutable run", "command docker run -e " + environment + " mutable.example/gke:latest apply\n" + validRun, false},
		{"backslash with trailing spaces", "docker run \\   \n  -e " + environment + " " + pinnedGKERef + " apply", false},
		{"backslash before comment", "docker run \\   # no continuation\n  -e " + environment + " \\\n  " + pinnedGKERef + " apply", false},
		{"markers in heredoc", "docker run mutable.example/gke:latest apply\ncat <<EOF\n" + validRun + "\nEOF", false},
		{"backslash in quoted delimiter", validRun + "\ncat <<\"E\\OF\"\nignored\nE\\OF\ndocker run -e " + environment + " mutable.example/gke:latest apply\nEOF", false},
		{"markers in inline comment", "docker run mutable.example/gke:latest apply # -e " + environment + " " + pinnedGKERef + " apply", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := activeDockerRunMatches(tt.run, pinnedGKERef+" apply", environment); got != tt.want {
				t.Fatalf("activeDockerRunMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func parsePinnedActuatorReference(raw, expectedRepository string) error {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsFunc(raw, unicode.IsSpace) {
		return fmt.Errorf("reference must be one complete token")
	}
	named, err := reference.ParseNormalizedNamed(raw)
	if err != nil {
		return fmt.Errorf("parse complete reference: %w", err)
	}
	if named.Name() != expectedRepository {
		return fmt.Errorf("repository %q, want %q", named.Name(), expectedRepository)
	}
	digested, ok := named.(reference.Digested)
	if !ok {
		return fmt.Errorf("reference is tag-only")
	}
	d := digested.Digest()
	if d.Algorithm() != digest.SHA256 || d.Validate() != nil ||
		len(d.Encoded()) != 64 || d.Encoded() != strings.ToLower(d.Encoded()) {

		return fmt.Errorf("digest must be one lowercase SHA-256")
	}
	if named.String() != raw {
		return fmt.Errorf("reference contains non-canonical or trailing data")
	}
	return nil
}

func actuatorReferenceTokens(value string) ([]string, error) {
	if !containsActuatorReferenceCandidate(value) {
		return nil, nil
	}
	commands, err := logicalShellLines(value)
	if err != nil {
		return nil, fmt.Errorf("parse shell commands: %w", err)
	}
	refs := make([]string, 0)
	for _, command := range commands {
		words, parseErr := shellWords(command)
		if parseErr != nil {
			return nil, fmt.Errorf("parse shell words: %w", parseErr)
		}
		for _, word := range words {
			if index := strings.Index(word, actuatorRoot); index >= 0 {
				refs = append(refs, word[index:])
			}
		}
	}
	return refs, nil
}

func containsActuatorReferenceCandidate(value string) bool {
	candidateValue := strings.ReplaceAll(value, "\\\n", "")
	for _, field := range strings.Fields(candidateValue) {
		if !looksLikeRegistryReference(field) && !strings.Contains(field, "$'") {
			continue
		}
		words, err := shellWords(field)
		if err != nil {
			return true
		}
		for _, word := range words {
			if looksLikeRegistryReference(word) {
				return true
			}
		}
	}
	return false
}

func looksLikeRegistryReference(value string) bool {
	firstSlash := strings.IndexByte(value, '/')
	firstDot := strings.IndexByte(value, '.')
	return strings.Count(value, "/") >= 3 && firstDot > 0 && firstDot < firstSlash
}

func shellEnablesXtrace(text string) bool {
	lines, err := logicalShellLines(text)
	if err != nil {
		return true
	}
	for _, line := range lines {
		lineWords, parseErr := shellWords(line)
		if parseErr != nil {
			return true
		}
		if hasUnsupportedDynamicShellWord(lineWords, true) {
			return true
		}
		if hasUnsupportedShellGlob(lineWords) {
			return true
		}
		segments, segmentErr := shellControlSegments(line)
		if segmentErr != nil {
			return true
		}
		for _, segment := range segments {
			if shellSegmentEnablesXtrace(segment) {
				return true
			}
		}
		for _, word := range lineWords {
			if !strings.Contains(word, "$(") && !strings.ContainsRune(word, '`') {
				continue
			}
			nestedSegments, nestedErr := shellControlSegments(word)
			if nestedErr != nil {
				return true
			}
			for _, segment := range nestedSegments {
				if shellSegmentEnablesXtrace(segment) {
					return true
				}
			}
		}
	}
	return false
}

func shellSegmentEnablesXtrace(segment string) bool {
	words, err := shellWords(segment)
	if err != nil || hasUnsupportedDynamicShellWord(words, true) ||
		hasUnsupportedShellGlob(words) || hasUnsupportedDynamicCommand(words) {

		return true
	}
	commandIndex := shellCommandIndex(words)
	if commandIndex < 0 {
		return false
	}
	command := filepath.Base(words[commandIndex])
	if command != "set" && command != "bash" && command != "sh" {
		return false
	}
	for next := commandIndex + 1; next < len(words); next++ {
		option := words[next]
		if strings.ContainsAny(option, "$`") {
			return true
		}
		if option == "--" {
			return false
		}
		if option == "-o" {
			if next+1 < len(words) && words[next+1] == "xtrace" {
				return true
			}
			next++
			continue
		}
		if option == "--xtrace" ||
			(strings.HasPrefix(option, "-") && !strings.HasPrefix(option, "--") && strings.Contains(option[1:], "x")) {

			return true
		}
		if command != "set" && (option == "-c" ||
			(strings.HasPrefix(option, "-") && !strings.HasPrefix(option, "--") && strings.Contains(option[1:], "c"))) {

			return next+1 < len(words) && shellEnablesXtrace(words[next+1])
		}
		if !strings.HasPrefix(option, "-") {
			return false
		}
	}
	return false
}

func shellEnvironmentEnablesXtrace(environment map[string]string) bool {
	for name := range environment {
		switch name {
		case "BASH_ENV", "ENV", "SHELLOPTS":
			return true
		}
	}
	return false
}

func shellControlSegments(line string) ([]string, error) {
	segments := make([]string, 0, 1)
	var segment strings.Builder
	var quote byte
	escaped := false
	flush := func() {
		value := strings.TrimSpace(segment.String())
		if value != "" {
			segments = append(segments, value)
		}
		segment.Reset()
	}

	for index := 0; index < len(line); index++ {
		character := line[index]
		if escaped {
			segment.WriteByte(character)
			escaped = false
			continue
		}
		if quote != 0 {
			segment.WriteByte(character)
			if character == quote {
				quote = 0
			} else if quote == '"' && character == '\\' {
				escaped = true
			}
			continue
		}
		if strings.HasPrefix(line[index:], "${{") {
			end := strings.Index(line[index+3:], "}}")
			if end < 0 {
				return nil, fmt.Errorf("unterminated GitHub expression")
			}
			end += index + 5
			segment.WriteString(line[index:end])
			index = end - 1
			continue
		}

		switch {
		case character == '\'' || character == '"':
			quote = character
			segment.WriteByte(character)
		case character == '\\':
			escaped = true
			segment.WriteByte(character)
		case character == ';' || character == '|' || character == '(' || character == ')':
			flush()
		case character == '&' && (index == 0 || line[index-1] != '>' && line[index-1] != '<'):
			flush()
		default:
			segment.WriteByte(character)
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated shell segment")
	}
	flush()
	return segments, nil
}

func decodeWorkflow(t *testing.T, file string) workflowDocument {
	t.Helper()
	node := loadWorkflow(t, file)
	var workflow workflowDocument
	if err := node.Decode(&workflow); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	return workflow
}

func uniqueStepUsing(t *testing.T, steps []workflowStep, marker string) workflowStep {
	t.Helper()
	matches := make([]workflowStep, 0, 1)
	for _, step := range steps {
		contains, err := shellScriptContainsMarker(step.Run, marker)
		if err != nil {
			t.Fatalf("parse shell for step %q: %v", step.Name, err)
		}
		if contains {
			matches = append(matches, step)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("found %d steps using %q, want exactly one", len(matches), marker)
	}
	return matches[0]
}

func uniqueStepNamed(t *testing.T, steps []workflowStep, name string) workflowStep {
	t.Helper()
	matches := make([]workflowStep, 0, 1)
	for _, step := range steps {
		if step.Name == name {
			matches = append(matches, step)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("found %d steps named %q, want exactly one", len(matches), name)
	}
	return matches[0]
}

func stepsWithDockerRun(t *testing.T, steps []workflowStep) []workflowStep {
	t.Helper()
	matches := make([]workflowStep, 0)
	for _, step := range steps {
		contains, err := shellScriptContainsDockerRun(step.Run)
		if err != nil {
			t.Fatalf("parse shell for step %q: %v", step.Name, err)
		}
		if contains {
			matches = append(matches, step)
		}
	}
	return matches
}

func parseDockerInvocations(script string) ([]dockerInvocation, error) {
	if !strings.Contains(script, "dock") {
		return nil, nil
	}
	commands, err := logicalShellLines(script)
	if err != nil {
		return nil, fmt.Errorf("parse logical shell lines: %w", err)
	}
	invocations := make([]dockerInvocation, 0)
	for _, command := range commands {
		command = strings.TrimSpace(strings.TrimSuffix(command, "; then"))
		command = strings.TrimSpace(strings.TrimPrefix(command, "if "))
		arguments, parseErr := shellWords(command)
		if parseErr != nil {
			return nil, fmt.Errorf("parse shell arguments: %w", parseErr)
		}

		if hasUnsupportedDynamicCommand(arguments) {
			return nil, fmt.Errorf("dynamic shell command is unsupported")
		}

		dockerIndex := -1
		imageIndex := -1
		for index := 0; index < len(arguments); index++ {
			if filepath.Base(arguments[index]) != "docker" {
				continue
			}
			if dockerIndex >= 0 {
				return nil, fmt.Errorf("multiple Docker runs on one shell command are unsupported")
			}
			dockerIndex = index
			if arguments[index] != "/usr/bin/docker" {
				return nil, fmt.Errorf("Docker must be invoked by its absolute runner path")
			}
			switch {
			case index+1 < len(arguments) && arguments[index+1] == "run":
				imageIndex = index + 2
			case index+2 < len(arguments) && arguments[index+1] == "container" && arguments[index+2] == "run":
				imageIndex = index + 3
			default:
				return nil, fmt.Errorf("unsupported Docker command")
			}
		}
		if dockerIndex < 0 {
			continue
		}
		if dockerIndex != 0 {
			return nil, fmt.Errorf("wrapped Docker run is unsupported")
		}
		if hasUnsupportedDynamicShellWord(arguments, true) {
			return nil, fmt.Errorf("dynamic shell word outside an assignment is unsupported")
		}

		index := imageIndex
		for index < len(arguments) && strings.HasPrefix(arguments[index], "-") {
			option := arguments[index]
			switch {
			case option == "-e" || option == "--env" || option == "-v" || option == "--volume":
				if index+1 >= len(arguments) {
					return nil, fmt.Errorf("Docker option %q is missing its value", option)
				}
				index += 2
			case strings.HasPrefix(option, "--env=") || strings.HasPrefix(option, "--volume=") ||
				strings.HasPrefix(option, "-e=") || strings.HasPrefix(option, "-v="):

				index++
			default:
				return nil, fmt.Errorf("Docker option %q is unsupported", option)
			}
		}
		if index >= len(arguments) {
			return nil, fmt.Errorf("Docker run is missing an image")
		}
		invocations = append(invocations, dockerInvocation{
			image:     arguments[index],
			arguments: slices.Clone(arguments[index+1:]),
		})
	}
	return invocations, nil
}

func shellScriptContainsDockerRun(script string) (bool, error) {
	commands, err := logicalShellLines(script)
	if err != nil {
		return false, fmt.Errorf("parse logical shell lines: %w", err)
	}
	for _, command := range commands {
		arguments, parseErr := shellWords(command)
		if parseErr != nil {
			return false, fmt.Errorf("parse shell arguments: %w", parseErr)
		}
		if hasUnsupportedDynamicShellWord(arguments, false) {
			return false, fmt.Errorf("dynamic shell word outside an assignment is unsupported")
		}
		if hasUnsupportedDynamicCommand(arguments) {
			return false, fmt.Errorf("dynamic shell command is unsupported")
		}
		if shellArgumentsContainDockerRun(arguments) {
			return true, nil
		}
	}
	return false, nil
}

func shellScriptContainsMarker(script, marker string) (bool, error) {
	commands, err := logicalShellLines(script)
	if err != nil {
		return false, fmt.Errorf("parse logical shell lines: %w", err)
	}
	for _, command := range commands {
		arguments, parseErr := shellWords(command)
		if parseErr != nil {
			return false, fmt.Errorf("parse shell arguments: %w", parseErr)
		}
		if hasUnsupportedDynamicShellWord(arguments, false) {
			return false, fmt.Errorf("dynamic shell word outside an assignment is unsupported")
		}
		if hasUnsupportedDynamicCommand(arguments) {
			return false, fmt.Errorf("dynamic shell command is unsupported")
		}
		if shellArgumentsContain(arguments, marker) {
			return true, nil
		}
	}
	return false, nil
}

func shellArgumentsContain(arguments []string, marker string) bool {
	for _, argument := range arguments {
		if strings.Contains(argument, marker) {
			return true
		}
	}
	return false
}

func shellArgumentsContainDockerRun(arguments []string) bool {
	for index, argument := range arguments {
		if strings.Contains(argument, "docker run") {
			return true
		}
		if filepath.Base(argument) == "docker" && index+1 < len(arguments) && arguments[index+1] == "run" {
			return true
		}
	}
	return false
}

func hasUnsupportedDynamicShellWord(arguments []string, rejectBackticks bool) bool {
	for _, argument := range arguments {
		if rejectBackticks && strings.ContainsRune(argument, '`') {
			return true
		}
		if strings.Contains(argument, "$(") && !isShellAssignment(argument) {
			return true
		}
	}
	return false
}

func hasUnsupportedShellGlob(arguments []string) bool {
	for _, argument := range arguments {
		if isShellAssignment(argument) {
			continue
		}
		if strings.ContainsAny(argument, "*?") ||
			argument != "[" && argument != "]" && strings.Contains(argument, "[") && strings.Contains(argument, "]") {

			return true
		}
	}
	return false
}

func isShellAssignment(word string) bool {
	separator := strings.IndexByte(word, '=')
	if separator < 1 {
		return false
	}
	for index, character := range word[:separator] {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && character != '_' &&
			(index == 0 || character < '0' || character > '9') {

			return false
		}
	}
	return true
}

func hasUnsupportedDynamicCommand(arguments []string) bool {
	timePrefix := false
	skipRedirectionTarget := false
	for _, argument := range arguments {
		if skipRedirectionTarget {
			skipRedirectionTarget = false
			continue
		}
		switch argument {
		case "", "!", "{", "}", "if", "then", "elif", "else", "for", "while", "until", "do":
			continue
		case "time":
			timePrefix = true
			continue
		case "builtin", "command", "env", "eval", "exec", "source", ".", "trap":
			return true
		}
		if isShellAssignment(argument) {
			continue
		}
		if isShellRedirection(argument) {
			skipRedirectionTarget = shellRedirectionNeedsTarget(argument)
			continue
		}
		if timePrefix && strings.HasPrefix(argument, "-") {
			continue
		}
		return strings.ContainsAny(argument, "$`")
	}
	return false
}

func shellCommandIndex(arguments []string) int {
	timePrefix := false
	skipRedirectionTarget := false
	for index, argument := range arguments {
		if skipRedirectionTarget {
			skipRedirectionTarget = false
			continue
		}
		switch argument {
		case "", "!", "{", "}", "if", "then", "elif", "else", "for", "while", "until", "do":
			continue
		case "time":
			timePrefix = true
			continue
		}
		if isShellAssignment(argument) {
			continue
		}
		if isShellRedirection(argument) {
			skipRedirectionTarget = shellRedirectionNeedsTarget(argument)
			continue
		}
		if timePrefix && strings.HasPrefix(argument, "-") {
			continue
		}
		return index
	}
	return -1
}

func isShellRedirection(word string) bool {
	operator := strings.TrimLeft(word, "0123456789")
	return strings.HasPrefix(operator, "<") || strings.HasPrefix(operator, ">") ||
		strings.HasPrefix(operator, "&>")
}

func shellRedirectionNeedsTarget(word string) bool {
	operator := strings.TrimLeft(word, "0123456789")
	switch operator {
	case "<", ">", "<<", "<<-", ">>", "<<<", "<>", ">|", "<&", ">&", "&>", "&>>":
		return true
	default:
		return false
	}
}

func activeRunLineContainsAll(run string, markers ...string) bool {
	lines, err := executableShellLines(run)
	if err != nil {
		return false
	}
	return executableLinesContainAll(lines, markers...)
}

func executableLinesContainAll(lines []string, markers ...string) bool {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		matches := true
		for _, marker := range markers {
			if !strings.Contains(line, marker) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func activeRunInvokesExactCommand(run, expected string) bool {
	lines, err := executableShellLines(run)
	if err != nil {
		return false
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimSuffix(line, "; then"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "if "))
		if line == expected {
			return true
		}
	}
	return false
}

func activeDockerRunMatches(run, expectedCommand, expectedEnvironment string) bool {
	commands, err := logicalShellLines(run)
	if err != nil {
		return false
	}
	expectedArguments, err := shellWords(expectedCommand)
	if err != nil || len(expectedArguments) == 0 {
		return false
	}
	environments, err := shellWords(expectedEnvironment)
	if err != nil || len(environments) != 1 {
		return false
	}
	dockerRuns := 0
	allMatch := true
	for _, command := range commands {
		command = strings.TrimSpace(strings.TrimSuffix(command, "; then"))
		command = strings.TrimSpace(strings.TrimPrefix(command, "if "))
		arguments, parseErr := shellWords(command)
		if parseErr != nil {
			if strings.Contains(command, "docker run") {
				dockerRuns++
				allMatch = false
			}
			continue
		}
		if hasUnsupportedDynamicShellWord(arguments, true) {
			allMatch = false
			continue
		}
		if hasUnsupportedDynamicCommand(arguments) {
			allMatch = false
			continue
		}
		containsDockerRun := shellArgumentsContainDockerRun(arguments)
		containsCredential := shellArgumentsContain(arguments, "KEY_CONTENT")
		if !containsDockerRun && !containsCredential {
			continue
		}
		if !containsDockerRun {
			allMatch = false
			continue
		}
		dockerRuns++
		if len(arguments) < 3 || arguments[0] != "/usr/bin/docker" || arguments[1] != "run" {
			allMatch = false
			continue
		}

		credentialEnvironments := 0
		environmentMatches := true
		checkEnvironment := func(value string) {
			if value != "KEY_CONTENT" && !strings.HasPrefix(value, "KEY_CONTENT=") {
				return
			}
			credentialEnvironments++
			environmentMatches = environmentMatches && value == environments[0]
		}
		index := 2
		validOptions := true
		for index < len(arguments) && strings.HasPrefix(arguments[index], "-") {
			option := arguments[index]
			switch {
			case option == "-e" || option == "--env":
				if index+1 >= len(arguments) {
					validOptions = false
					index++
					continue
				}
				checkEnvironment(arguments[index+1])
				index += 2
			case strings.HasPrefix(option, "--env="):
				checkEnvironment(strings.TrimPrefix(option, "--env="))
				index++
			case strings.HasPrefix(option, "-e="):
				checkEnvironment(strings.TrimPrefix(option, "-e="))
				index++
			case strings.HasPrefix(option, "-e") && len(option) > len("-e"):
				checkEnvironment(strings.TrimPrefix(option, "-e"))
				index++
			default:
				validOptions = false
				index++
			}
		}
		if !validOptions || credentialEnvironments != 1 || !environmentMatches ||
			len(arguments)-index != len(expectedArguments) {

			allMatch = false
			continue
		}
		matches := true
		for offset, expected := range expectedArguments {
			if arguments[index+offset] != expected {
				matches = false
				break
			}
		}
		if !matches {
			allMatch = false
		}
	}
	return dockerRuns == 1 && allMatch
}

func shellWords(command string) ([]string, error) {
	words := make([]string, 0)
	var word strings.Builder
	var quote byte
	escaped := false
	started := false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}

	for index := 0; index < len(command); index++ {
		character := command[index]
		if escaped {
			word.WriteByte(character)
			escaped = false
			started = true
			continue
		}
		if quote == '\'' {
			if character == '\'' {
				quote = 0
			} else {
				word.WriteByte(character)
			}
			started = true
			continue
		}
		if quote == '"' {
			switch character {
			case '"':
				quote = 0
			case '\\':
				if index+1 < len(command) && strings.ContainsRune("$`\"\\\n", rune(command[index+1])) {
					escaped = true
				} else {
					word.WriteByte(character)
				}
			default:
				word.WriteByte(character)
			}
			started = true
			continue
		}
		if strings.HasPrefix(command[index:], "${{") {
			end := strings.Index(command[index+3:], "}}")
			if end < 0 {
				return nil, fmt.Errorf("unterminated GitHub expression")
			}
			end += index + 5
			word.WriteString(command[index:end])
			started = true
			index = end - 1
			continue
		}
		if strings.HasPrefix(command[index:], "$'") {
			decoded, next, err := decodeANSICQuoted(command, index)
			if err != nil {
				return nil, fmt.Errorf("parse ANSI-C quote: %w", err)
			}
			word.WriteString(decoded)
			started = true
			index = next - 1
			continue
		}
		if strings.HasPrefix(command[index:], `$"`) {
			return nil, fmt.Errorf("locale shell quoting is unsupported")
		}

		switch {
		case character == '\'' || character == '"':
			quote = character
			started = true
		case character == '\\':
			escaped = true
			started = true
		case unicode.IsSpace(rune(character)):
			flush()
		default:
			word.WriteByte(character)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated shell word")
	}
	flush()
	return words, nil
}

func decodeANSICQuoted(command string, start int) (string, int, error) {
	var decoded strings.Builder
	for index := start + 2; index < len(command); index++ {
		character := command[index]
		if character == '\'' {
			return decoded.String(), index + 1, nil
		}
		if character != '\\' {
			decoded.WriteByte(character)
			continue
		}

		index++
		if index >= len(command) {
			return "", 0, fmt.Errorf("unterminated escape")
		}
		escape := command[index]
		switch escape {
		case 'a':
			decoded.WriteByte('\a')
		case 'b':
			decoded.WriteByte('\b')
		case 'e', 'E':
			decoded.WriteByte(0x1b)
		case 'f':
			decoded.WriteByte('\f')
		case 'n':
			decoded.WriteByte('\n')
		case 'r':
			decoded.WriteByte('\r')
		case 't':
			decoded.WriteByte('\t')
		case 'v':
			decoded.WriteByte('\v')
		case '\\', '\'', '"', '?':
			decoded.WriteByte(escape)
		case '\n':
			continue
		case 'x':
			value, consumed := parseANSIDigits(command[index+1:], 16, 2)
			if consumed == 0 {
				return "", 0, fmt.Errorf("hex escape requires one or two digits")
			}
			if value == 0 {
				return "", 0, fmt.Errorf("NUL escape is unsupported")
			}
			decoded.WriteByte(byte(value))
			index += consumed
		case 'u', 'U':
			maximumDigits := 4
			if escape == 'U' {
				maximumDigits = 8
			}
			value, consumed := parseANSIDigits(command[index+1:], 16, maximumDigits)
			if consumed == 0 {
				return "", 0, fmt.Errorf("Unicode escape requires hexadecimal digits")
			}
			if value == 0 || value > 0x10ffff || value >= 0xd800 && value <= 0xdfff {
				return "", 0, fmt.Errorf("Unicode escape is not a valid non-NUL scalar")
			}
			decoded.WriteRune(rune(value))
			index += consumed
		case '0', '1', '2', '3', '4', '5', '6', '7':
			value, consumed := parseANSIDigits(command[index:], 8, 3)
			if value == 0 || value > 0xff {
				return "", 0, fmt.Errorf("octal escape is not a valid non-NUL byte")
			}
			decoded.WriteByte(byte(value))
			index += consumed - 1
		case 'c':
			if index+1 >= len(command) || command[index+1] == '\'' {
				return "", 0, fmt.Errorf("control escape requires one ASCII character")
			}
			index++
			control := command[index]
			if control > 0x7f {
				return "", 0, fmt.Errorf("control escape requires one ASCII character")
			}
			value := control & 0x1f
			if control == '?' {
				value = 0x7f
			}
			if value == 0 {
				return "", 0, fmt.Errorf("NUL escape is unsupported")
			}
			decoded.WriteByte(value)
		default:
			return "", 0, fmt.Errorf("unsupported escape \\%c", escape)
		}
	}
	return "", 0, fmt.Errorf("unterminated quote")
}

func parseANSIDigits(value string, base, maximum int) (int, int) {
	result := 0
	consumed := 0
	for consumed < len(value) && consumed < maximum {
		digit := ansiDigitValue(value[consumed])
		if digit < 0 || digit >= base {
			break
		}
		result = result*base + digit
		consumed++
	}
	return result, consumed
}

func ansiDigitValue(value byte) int {
	switch {
	case value >= '0' && value <= '9':
		return int(value - '0')
	case value >= 'a' && value <= 'f':
		return int(value-'a') + 10
	case value >= 'A' && value <= 'F':
		return int(value-'A') + 10
	default:
		return -1
	}
}

type shellHeredoc struct {
	delimiter string
	stripTabs bool
}

func executableShellLines(script string) ([]string, error) {
	lines := make([]string, 0)
	pending := make([]shellHeredoc, 0)
	for lineNumber, raw := range strings.Split(script, "\n") {
		if len(pending) > 0 {
			candidate := raw
			if pending[0].stripTabs {
				candidate = strings.TrimLeft(candidate, "\t")
			}
			if candidate == pending[0].delimiter {
				pending = pending[1:]
			}
			continue
		}

		line, err := stripShellComment(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		if shellLineContinues(line) && (len(line) < 2 || !unicode.IsSpace(rune(line[len(line)-2]))) {
			return nil, fmt.Errorf("line %d: continuation inside a shell token is unsupported", lineNumber+1)
		}
		if strings.Contains(line, "((") {
			return nil, fmt.Errorf("line %d: shell arithmetic is unsupported", lineNumber+1)
		}
		heredocs := shellHeredocs(line)
		if len(heredocs) > 0 && shellLineContinues(line) {
			return nil, fmt.Errorf("line %d: heredoc with line continuation is unsupported", lineNumber+1)
		}
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
		pending = append(pending, heredocs...)
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("unterminated heredoc %q", pending[0].delimiter)
	}
	return lines, nil
}

func stripShellComment(line string) (string, error) {
	var quote byte
	escaped := false
	wordStart := true
	for index := 0; index < len(line); index++ {
		character := line[index]
		if escaped {
			escaped = false
			wordStart = false
			continue
		}
		if quote == '\'' {
			if character == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			switch character {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			}
			continue
		}

		switch {
		case character == '\\':
			escaped = true
			wordStart = false
		case character == '\'' || character == '"':
			quote = character
			wordStart = false
		case character == '#' && wordStart:
			return line[:index], nil
		default:
			wordStart = unicode.IsSpace(rune(character)) || strings.ContainsRune(`;|&()<>`, rune(character))
		}
	}
	if quote != 0 {
		return "", fmt.Errorf("unterminated shell quote")
	}
	return line, nil
}

func shellHeredocs(line string) []shellHeredoc {
	heredocs := make([]shellHeredoc, 0)
	var quote byte
	escaped := false
	for index := 0; index < len(line); index++ {
		character := line[index]
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else if quote == '"' && character == '\\' {
				escaped = true
			}
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character != '<' || index+1 >= len(line) || line[index+1] != '<' {
			continue
		}
		if index+2 < len(line) && line[index+2] == '<' {
			index += 2
			continue
		}

		cursor := index + 2
		stripTabs := false
		if cursor < len(line) && line[cursor] == '-' {
			stripTabs = true
			cursor++
		}
		for cursor < len(line) && (line[cursor] == ' ' || line[cursor] == '\t') {
			cursor++
		}
		delimiter, next, ok := readHeredocDelimiter(line, cursor)
		if ok {
			heredocs = append(heredocs, shellHeredoc{delimiter: delimiter, stripTabs: stripTabs})
			index = next - 1
		}
	}
	return heredocs
}

func readHeredocDelimiter(line string, start int) (string, int, bool) {
	var delimiter strings.Builder
	var quote byte
	escaped := false
	started := false
	index := start
	for ; index < len(line); index++ {
		character := line[index]
		if escaped {
			delimiter.WriteByte(character)
			escaped = false
			started = true
			continue
		}
		if quote != 0 {
			switch {
			case character == quote:
				quote = 0
			case quote == '"' && character == '\\':
				if index+1 < len(line) && strings.ContainsRune("$`\"\\\n", rune(line[index+1])) {
					escaped = true
				} else {
					delimiter.WriteByte(character)
				}
			default:
				delimiter.WriteByte(character)
			}
			started = true
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			started = true
			continue
		}
		if character == '\\' {
			escaped = true
			started = true
			continue
		}
		if unicode.IsSpace(rune(character)) || strings.ContainsRune(`;|&()<>`, rune(character)) {
			break
		}
		delimiter.WriteByte(character)
		started = true
	}
	return delimiter.String(), index, started
}

func shellLineContinues(line string) bool {
	if len(line) == 0 || line[len(line)-1] != '\\' {
		return false
	}
	backslashes := 0
	for index := len(line) - 1; index >= 0 && line[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func logicalShellLines(script string) ([]string, error) {
	lines, err := executableShellLines(script)
	if err != nil {
		return nil, fmt.Errorf("parse executable shell lines: %w", err)
	}
	commands := make([]string, 0)
	var logical strings.Builder
	for _, line := range lines {
		continued := shellLineContinues(line)
		line = strings.TrimSpace(line)
		if continued {
			line = strings.TrimSpace(strings.TrimSuffix(line, `\`))
		}
		if logical.Len() > 0 && line != "" {
			logical.WriteByte(' ')
		}
		logical.WriteString(line)
		if continued {
			continue
		}
		commands = append(commands, logical.String())
		logical.Reset()
	}
	if logical.Len() > 0 {
		return nil, fmt.Errorf("unterminated shell line continuation")
	}
	return commands, nil
}

func loadWorkflow(t *testing.T, file string) *yaml.Node {
	t.Helper()
	path := filepath.Clean(filepath.Join(workflowsDir, file))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return &node
}

func walkStringScalars(node *yaml.Node, visit func(string)) {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		visit(node.Value)
	}
	for _, child := range node.Content {
		walkStringScalars(child, visit)
	}
}
