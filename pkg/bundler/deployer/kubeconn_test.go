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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubBin records the argv it was called with, one argument per line, so the
// assertions below compare what each binary actually received rather than a
// string the harness reassembled. The distinction is the point of the two
// arrays: a single joined string cannot show whether an empty context produced
// a stray empty argument.
//
// PATH holds only the stub directory, so the shebang is absolute and the script
// name comes from parameter expansion rather than basename(1).
const stubBin = `#!/bin/sh
printf '%s\n' "$@" > "${ARGV_DIR}/${0##*/}.argv"
`

// runKubeConn renders the connection prologue into a script that calls helm and
// kubectl through stubs, runs it with env, and returns each binary's argv.
func runKubeConn(t *testing.T, env []string) (helmArgv, kubectlArgv []string, stderr string, err error) {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	argvDir := filepath.Join(dir, "argv")
	for _, d := range []string{binDir, argvDir} {
		if mkErr := os.MkdirAll(d, 0o750); mkErr != nil {
			t.Fatalf("mkdir %s: %v", d, mkErr)
		}
	}
	for _, name := range []string{"helm", "kubectl"} {
		if wErr := os.WriteFile(filepath.Join(binDir, name), []byte(stubBin), 0o700); wErr != nil {
			t.Fatalf("write stub %s: %v", name, wErr)
		}
	}

	script := "#!/usr/bin/env bash\nset -euo pipefail\n\n" + KubeConnection() + `
helm ${HELM_CONN[@]+"${HELM_CONN[@]}"} list
kubectl ${KUBECTL_CONN[@]+"${KUBECTL_CONN[@]}"} get crd
`
	scriptPath := filepath.Join(dir, "harness.sh")
	if wErr := os.WriteFile(scriptPath, []byte(script), 0o700); wErr != nil {
		t.Fatalf("write harness: %v", wErr)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append([]string{
		"PATH=" + binDir,
		"ARGV_DIR=" + argvDir,
	}, env...)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	err = cmd.Run()

	read := func(name string) []string {
		b, rErr := os.ReadFile(filepath.Join(argvDir, name+".argv"))
		if rErr != nil {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
	return read("helm"), read("kubectl"), errBuf.String(), err
}

func TestKubeConnection_ResolvesContext(t *testing.T) {
	tests := []struct {
		name        string
		env         []string
		wantHelm    []string
		wantKubectl []string
		wantWarn    bool
	}{
		{
			name:        "no connection environment leaves both calls bare",
			env:         nil,
			wantHelm:    []string{"list"},
			wantKubectl: []string{"get", "crd"},
		},
		{
			name:        "KUBE_CONTEXT renders each binary's own spelling",
			env:         []string{"KUBE_CONTEXT=kind-aicr"},
			wantHelm:    []string{"--kube-context", "kind-aicr", "list"},
			wantKubectl: []string{"--context", "kind-aicr", "get", "crd"},
		},
		{
			name:        "deprecated KUBECONFIG_FLAG is translated, not forwarded",
			env:         []string{"KUBECONFIG_FLAG=--kube-context kind-aicr"},
			wantHelm:    []string{"--kube-context", "kind-aicr", "list"},
			wantKubectl: []string{"--context", "kind-aicr", "get", "crd"},
			wantWarn:    true,
		},
		{
			name:        "deprecated KUBECONFIG_FLAG accepts the --flag=value spelling",
			env:         []string{"KUBECONFIG_FLAG=--kube-context=kind-aicr"},
			wantHelm:    []string{"--kube-context", "kind-aicr", "list"},
			wantKubectl: []string{"--context", "kind-aicr", "get", "crd"},
			wantWarn:    true,
		},
		{
			name:        "agreeing KUBE_CONTEXT and KUBECONFIG_FLAG are not a conflict",
			env:         []string{"KUBE_CONTEXT=kind-aicr", "KUBECONFIG_FLAG=--kube-context kind-aicr"},
			wantHelm:    []string{"--kube-context", "kind-aicr", "list"},
			wantKubectl: []string{"--context", "kind-aicr", "get", "crd"},
			wantWarn:    true,
		},
		{
			// --kubeconfig is spelled the same by both binaries, so it is
			// forwarded rather than translated.
			name:        "deprecated --kubeconfig reaches both binaries",
			env:         []string{"KUBECONFIG_FLAG=--kubeconfig /tmp/kc"},
			wantHelm:    []string{"--kubeconfig", "/tmp/kc", "list"},
			wantKubectl: []string{"--kubeconfig", "/tmp/kc", "get", "crd"},
			wantWarn:    true,
		},
		{
			name:        "deprecated --kubeconfig keeps the joined spelling",
			env:         []string{"KUBECONFIG_FLAG=--kubeconfig=/tmp/kc"},
			wantHelm:    []string{"--kubeconfig=/tmp/kc", "list"},
			wantKubectl: []string{"--kubeconfig=/tmp/kc", "get", "crd"},
			wantWarn:    true,
		},
		{
			name:        "both options translate into each binary's spelling",
			env:         []string{"KUBECONFIG_FLAG=--kubeconfig=/tmp/kc --kube-context kind-aicr"},
			wantHelm:    []string{"--kubeconfig=/tmp/kc", "--kube-context", "kind-aicr", "list"},
			wantKubectl: []string{"--kubeconfig=/tmp/kc", "--context", "kind-aicr", "get", "crd"},
			wantWarn:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helmArgv, kubectlArgv, stderr, err := runKubeConn(t, tt.env)
			if err != nil {
				t.Fatalf("harness failed: %v\nstderr: %s", err, stderr)
			}
			if !equalArgv(helmArgv, tt.wantHelm) {
				t.Errorf("helm argv = %q, want %q", helmArgv, tt.wantHelm)
			}
			if !equalArgv(kubectlArgv, tt.wantKubectl) {
				t.Errorf("kubectl argv = %q, want %q", kubectlArgv, tt.wantKubectl)
			}
			if gotWarn := strings.Contains(stderr, "KUBECONFIG_FLAG is deprecated"); gotWarn != tt.wantWarn {
				t.Errorf("deprecation warning = %v, want %v (stderr: %s)", gotWarn, tt.wantWarn, stderr)
			}
		})
	}
}

// Every rejection below has to happen before the first cluster call. An option
// that is merely dropped would leave the reads and writes pointed at whatever
// the ambient kubeconfig names, so the assertion is that neither binary ran --
// not just that the exit status is non-zero.
func TestKubeConnection_FailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		env     []string
		wantMsg string
		denyMsg string
	}{
		{
			name:    "an untranslated option is refused",
			env:     []string{"KUBECONFIG_FLAG=--kube-apiserver https://example.invalid"},
			wantMsg: "does not support",
		},
		{
			name:    "a trailing option with no value is refused",
			env:     []string{"KUBECONFIG_FLAG=--kube-context"},
			wantMsg: "and no value",
		},
		{
			name:    "disagreeing context spellings are refused",
			env:     []string{"KUBE_CONTEXT=prod", "KUBECONFIG_FLAG=--kube-context staging"},
			wantMsg: "Refusing to guess which cluster",
		},
		{
			// Two tokens on purpose: with a trailing third the list would end
			// on an unrecognized token and the catch-all would abort anyway,
			// so the row would still pass with this guard removed.
			name:    "an option where a value belongs is refused",
			env:     []string{"KUBECONFIG_FLAG=--kube-context --kubeconfig"},
			wantMsg: "which is another option rather than a value",
		},
		{
			name:    "an empty joined context is refused",
			env:     []string{"KUBECONFIG_FLAG=--kube-context="},
			wantMsg: "whose value is empty",
		},
		{
			name:    "an empty joined kubeconfig is refused",
			env:     []string{"KUBECONFIG_FLAG=--kubeconfig="},
			wantMsg: "whose value is empty",
		},
		{
			// helm's --kube-token carries a bearer token in the joined form,
			// and these scripts run with their output attached to CI logs.
			name:    "a rejected option is named without its credential",
			env:     []string{"KUBECONFIG_FLAG=--kube-token=s3cr3t-bearer"},
			wantMsg: "--kube-token",
			denyMsg: "s3cr3t-bearer",
		},
		{
			name:    "an option-shaped value is named without its credential",
			env:     []string{"KUBECONFIG_FLAG=--kube-context --kube-token=s3cr3t-bearer"},
			wantMsg: "another option rather than a value",
			denyMsg: "s3cr3t-bearer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helmArgv, kubectlArgv, stderr, err := runKubeConn(t, tt.env)
			if err == nil {
				t.Fatalf("harness succeeded, want failure (stderr: %s)", stderr)
			}
			if !strings.Contains(stderr, tt.wantMsg) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantMsg)
			}
			if tt.denyMsg != "" && strings.Contains(stderr, tt.denyMsg) {
				t.Errorf("stderr disclosed %q: %s", tt.denyMsg, stderr)
			}
			if helmArgv != nil || kubectlArgv != nil {
				t.Errorf("a cluster call ran despite the rejection: helm=%q kubectl=%q", helmArgv, kubectlArgv)
			}
		})
	}
}

func equalArgv(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// deploy.sh runs each component as its own process (`cd dir && bash install.sh`)
// and install.sh runs `bash ./apply-crds.sh`, so every child re-runs this
// prologue from the environment alone. Bash cannot export an array, and the
// deprecated variable is unset once translated, so anything held only in
// HELM_CONN/KUBECTL_CONN is gone by the first child. The kubeconfig has to
// survive as an exported variable or the child's helm silently falls back to
// the ambient one.
func TestKubeConnection_PropagatesToChildScripts(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	argvDir := filepath.Join(dir, "argv")
	for _, d := range []string{binDir, argvDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Records the connection as the child binary would actually resolve it:
	// the flags it was handed plus the kubeconfig it inherits from the env.
	stub := `#!/bin/sh
{ printf 'KUBECONFIG=%s\n' "${KUBECONFIG:-}"; printf 'ARGV=%s\n' "$*"; } > "${ARGV_DIR}/${0##*/}.seen"
`
	for _, name := range []string{"helm", "kubectl"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(stub), 0o700); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}

	child := filepath.Join(dir, "child.sh")
	if err := os.WriteFile(child, []byte("#!/usr/bin/env bash\nset -euo pipefail\n\n"+
		KubeConnection()+"\nhelm ${HELM_CONN[@]+\"${HELM_CONN[@]}\"} upgrade\n"), 0o700); err != nil {
		t.Fatalf("write child: %v", err)
	}
	// PATH holds only the stubs, so the child is spawned by absolute path.
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	parent := filepath.Join(dir, "parent.sh")
	if err := os.WriteFile(parent, []byte("#!/usr/bin/env bash\nset -euo pipefail\n\n"+
		KubeConnection()+"\n"+bashPath+" "+child+"\n"), 0o700); err != nil {
		t.Fatalf("write parent: %v", err)
	}

	cmd := exec.Command("bash", parent)
	cmd.Env = []string{
		"PATH=" + binDir,
		"ARGV_DIR=" + argvDir,
		"KUBECONFIG_FLAG=--kubeconfig /tmp/kc.yaml --kube-context kind-aicr",
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("parent failed: %v\nstderr: %s", err, errBuf.String())
	}

	seen, readErr := os.ReadFile(filepath.Join(argvDir, "helm.seen"))
	if readErr != nil {
		t.Fatalf("child helm never ran: %v\nstderr: %s", readErr, errBuf.String())
	}
	got := string(seen)
	if !strings.Contains(got, "--kube-context kind-aicr") {
		t.Errorf("child helm lost the context; saw:\n%s", got)
	}
	if !strings.Contains(got, "/tmp/kc.yaml") {
		t.Errorf("child helm lost the kubeconfig, so it would use the ambient one; saw:\n%s", got)
	}
}
