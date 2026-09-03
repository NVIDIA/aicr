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

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// TestSnapshotTemplateFlagCombinations tests all combinations of --template, --format, and --output flags.
// The rules are:
// 1. Template requires YAML format (explicit or default)
// 2. Template with --format json should error
// 3. Template with --format table should error
// 4. Template without output writes to stdout
// 5. Template with output writes to file
func TestSnapshotTemplateFlagCombinations(t *testing.T) {
	// Create temp directory for test files
	tmpDir := t.TempDir()

	// Create a valid template file
	templatePath := filepath.Join(tmpDir, "test.tmpl")
	if err := os.WriteFile(templatePath, []byte("{{ .Name }}"), 0o644); err != nil {
		t.Fatalf("failed to create template file: %v", err)
	}

	tests := []struct {
		name         string
		templatePath string
		format       string
		formatSet    bool // whether --format was explicitly set
		output       string
		wantErr      bool
		errContains  string
	}{
		// Template without format (should use YAML default)
		{
			name:         "template without format defaults to YAML",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       "",
			wantErr:      false,
		},
		// Template with explicit YAML format
		{
			name:         "template with explicit yaml format",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    true,
			output:       "",
			wantErr:      false,
		},
		// Template with JSON format should error
		{
			name:         "template with json format should error",
			templatePath: templatePath,
			format:       "json",
			formatSet:    true,
			output:       "",
			wantErr:      true,
			errContains:  "YAML format",
		},
		// Template with table format should error
		{
			name:         "template with table format should error",
			templatePath: templatePath,
			format:       "table",
			formatSet:    true,
			output:       "",
			wantErr:      true,
			errContains:  "YAML format",
		},
		// Template with file output
		{
			name:         "template with file output",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       filepath.Join(tmpDir, "output.yaml"),
			wantErr:      false,
		},
		// Template with stdout output (dash)
		{
			name:         "template with stdout output dash",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       "-",
			wantErr:      false,
		},
		// Template with empty output (stdout)
		{
			name:         "template with empty output (stdout)",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       "",
			wantErr:      false,
		},
		// Non-existent template file
		{
			name:         "non-existent template file",
			templatePath: "/non/existent/template.tmpl",
			format:       "yaml",
			formatSet:    false,
			output:       "",
			wantErr:      true,
			errContains:  "not found",
		},
		// Template path is a directory
		{
			name:         "template path is directory",
			templatePath: tmpDir,
			format:       "yaml",
			formatSet:    false,
			output:       "",
			wantErr:      true,
			errContains:  "directory",
		},
		// No template (standard output)
		{
			name:         "no template with yaml format",
			templatePath: "",
			format:       "yaml",
			formatSet:    true,
			output:       "",
			wantErr:      false,
		},
		{
			name:         "no template with json format",
			templatePath: "",
			format:       "json",
			formatSet:    true,
			output:       "",
			wantErr:      false,
		},
		// Template + ConfigMap URI output must be rejected: the template
		// writer only emits to local files, so a cm:// path would silently
		// create a file literally named "cm:..." instead of writing to K8s.
		{
			name:         "template with ConfigMap URI output is rejected",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       "cm://aicr/snap",
			wantErr:      true,
			errContains:  "ConfigMap",
		},
		{
			name:         "no template with ConfigMap URI output is allowed",
			templatePath: "",
			format:       "yaml",
			formatSet:    false,
			output:       "cm://aicr/snap",
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Exercise the real production function rather than a hand-copied
			// mirror — the prior helper drifted from the actual validation
			// rules during the ConfigMap-rejection addition.
			cmd := buildSnapshotCmdForTemplateTest(t, tt.templatePath, tt.format, tt.formatSet, tt.output)
			outFormat := serializer.Format(tt.format)
			_, err := parseSnapshotTemplateOptions(cmd, outFormat, aicr.SnapshotOutputOptions{})

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errContains)
				} else if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %v", tt.errContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// buildSnapshotCmdForTemplateTest constructs a parsed *cli.Command with
// --template / --format / --output set so parseSnapshotTemplateOptions can be
// exercised in isolation. formatSet=false omits --format so the test exercises
// the cmd.IsSet("format") branch.
func buildSnapshotCmdForTemplateTest(t *testing.T, templatePath, format string, formatSet bool, output string) *cli.Command {
	t.Helper()
	cmd := snapshotCmd()
	app := &cli.Command{Name: "aicr", Commands: []*cli.Command{cmd}}

	args := []string{"aicr", "snapshot"}
	if templatePath != "" {
		args = append(args, "--template", templatePath)
	}
	if formatSet {
		args = append(args, "--format", format)
	}
	if output != "" {
		args = append(args, "--output", output)
	}

	var captured *cli.Command
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		captured = c
		return nil
	}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("flag parse setup failed: %v", err)
	}
	if captured == nil {
		t.Fatal("flag parse setup did not capture cmd")
	}
	return captured
}

// TestParseResourceList covers the --requests / --limits parser, including
// the duplicate-key rejection added per PR #762 review.
func TestParseResourceList(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantNil     bool
		wantKeys    map[corev1.ResourceName]string
		wantErr     bool
		wantErrSubs string
	}{
		{
			name:    "empty input -> nil ResourceList (no override)",
			input:   "",
			wantNil: true,
		},
		{
			name:    "whitespace only -> nil",
			input:   "   ",
			wantNil: true,
		},
		{
			name:  "single entry",
			input: "memory=1Gi",
			wantKeys: map[corev1.ResourceName]string{
				corev1.ResourceMemory: "1Gi",
			},
		},
		{
			name:  "multiple entries with whitespace tolerated",
			input: " cpu=500m , memory=1Gi , ephemeral-storage=2Gi ",
			wantKeys: map[corev1.ResourceName]string{
				corev1.ResourceCPU:              "500m",
				corev1.ResourceMemory:           "1Gi",
				corev1.ResourceEphemeralStorage: "2Gi",
			},
		},
		{
			name:  "extended resource (nvidia.com/gpu)",
			input: "nvidia.com/gpu=4",
			wantKeys: map[corev1.ResourceName]string{
				corev1.ResourceName("nvidia.com/gpu"): "4",
			},
		},
		{
			name:        "missing equals -> error",
			input:       "cpu",
			wantErr:     true,
			wantErrSubs: "name=quantity",
		},
		{
			name:        "empty key -> error",
			input:       "=1Gi",
			wantErr:     true,
			wantErrSubs: "empty",
		},
		{
			name:        "empty value -> error",
			input:       "memory=",
			wantErr:     true,
			wantErrSubs: "empty",
		},
		{
			name:        "invalid quantity -> error",
			input:       "memory=not-a-quantity",
			wantErr:     true,
			wantErrSubs: "memory=not-a-quantity",
		},
		{
			name:        "negative quantity rejected (cpu)",
			input:       "cpu=-1",
			wantErr:     true,
			wantErrSubs: "negative quantity",
		},
		{
			name:        "negative quantity rejected (memory with suffix)",
			input:       "memory=-1Gi",
			wantErr:     true,
			wantErrSubs: "negative quantity",
		},
		{
			name:        "negative quantity in second entry rejected",
			input:       "cpu=1,memory=-256Mi",
			wantErr:     true,
			wantErrSubs: "negative quantity",
		},
		{
			name:  "zero quantity allowed",
			input: "cpu=0",
			wantKeys: map[corev1.ResourceName]string{
				corev1.ResourceCPU: "0",
			},
		},
		{
			name:        "duplicate key rejected",
			input:       "cpu=1,cpu=2",
			wantErr:     true,
			wantErrSubs: "duplicate key",
		},
		{
			name:        "duplicate key after whitespace normalization rejected",
			input:       "memory=1Gi, memory =2Gi",
			wantErr:     true,
			wantErrSubs: "duplicate key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := snapshotter.ParseResourceList(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantErrSubs != "" && !strings.Contains(err.Error(), tt.wantErrSubs) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSubs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil ResourceList, got %v", got)
				}
				return
			}
			if len(got) != len(tt.wantKeys) {
				t.Fatalf("got %d keys, want %d (got=%v want=%v)", len(got), len(tt.wantKeys), got, tt.wantKeys)
			}
			for k, want := range tt.wantKeys {
				v, ok := got[k]
				if !ok {
					t.Errorf("missing key %q", k)
					continue
				}
				if v.String() != want {
					t.Errorf("key %q: got %q, want %q", k, v.String(), want)
				}
			}
		})
	}
}

// TestOutputDestinationParsing tests parsing of various output destinations.
func TestOutputDestinationParsing(t *testing.T) {
	tests := []struct {
		name           string
		output         string
		isStdout       bool
		isFile         bool
		isConfigMap    bool
		expectFilePath string
	}{
		{
			name:     "empty output is stdout",
			output:   "",
			isStdout: true,
		},
		{
			name:     "dash is stdout",
			output:   "-",
			isStdout: true,
		},
		{
			name:     "stdout:// is stdout",
			output:   serializer.StdoutURI,
			isStdout: true,
		},
		{
			name:           "file path",
			output:         "/tmp/snapshot.yaml",
			isFile:         true,
			expectFilePath: "/tmp/snapshot.yaml",
		},
		{
			name:           "relative file path",
			output:         "snapshot.yaml",
			isFile:         true,
			expectFilePath: "snapshot.yaml",
		},
		{
			name:        "configmap URI",
			output:      "cm://gpu-operator/aicr-snapshot",
			isConfigMap: true,
		},
		{
			name:        "configmap URI custom namespace",
			output:      "cm://custom-ns/my-snapshot",
			isConfigMap: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isStdout := tt.output == "" || tt.output == "-" || tt.output == serializer.StdoutURI
			isConfigMap := len(tt.output) > len(serializer.ConfigMapURIScheme) &&
				tt.output[:len(serializer.ConfigMapURIScheme)] == serializer.ConfigMapURIScheme
			isFile := !isStdout && !isConfigMap

			if isStdout != tt.isStdout {
				t.Errorf("isStdout = %v, want %v", isStdout, tt.isStdout)
			}
			if isFile != tt.isFile {
				t.Errorf("isFile = %v, want %v", isFile, tt.isFile)
			}
			if isConfigMap != tt.isConfigMap {
				t.Errorf("isConfigMap = %v, want %v", isConfigMap, tt.isConfigMap)
			}
		})
	}
}

// TestSnapshotCmd_AddRolesFlagWiring covers the CLI surface of the
// generate-and-exit invocation. Rendering the manifests is covered in
// pkg/k8s/agent and pkg/snapshotter; what this asserts is the wiring an
// operator touches: the flag exists under the agent-deployment category, its
// usage says outright that it applies nothing (the flag NAME reads like it
// mutates the cluster, so nobody may run it and assume the grant is live),
// and it is single-valued so a repeated flag is rejected rather than silently
// taking the last value and writing manifests for the wrong ServiceAccount.
func TestSnapshotCmd_AddRolesFlagWiring(t *testing.T) {
	t.Run("flag is registered under agent deployment", func(t *testing.T) {
		var found cli.Flag
		for _, f := range snapshotCmd().Flags {
			for _, n := range f.Names() {
				if n == flagAddRolesToSA {
					found = f
				}
			}
		}
		if found == nil {
			t.Fatalf("snapshot command must define --%s", flagAddRolesToSA)
		}
		sf, ok := found.(*cli.StringFlag)
		if !ok {
			t.Fatalf("--%s is %T, want *cli.StringFlag", flagAddRolesToSA, found)
		}
		if sf.Category != catAgentDeployment {
			t.Errorf("--%s category = %q, want %q", flagAddRolesToSA, sf.Category, catAgentDeployment)
		}
		if !strings.Contains(sf.Usage, "without taking a snapshot") {
			t.Errorf("--%s usage must say it takes no snapshot; got %q", flagAddRolesToSA, sf.Usage)
		}
		if !strings.Contains(sf.Usage, "APPLIES NOTHING") {
			t.Errorf("--%s usage must state plainly that it applies nothing; got %q", flagAddRolesToSA, sf.Usage)
		}
		if !strings.Contains(sf.Usage, "kubectl apply") || !strings.Contains(sf.Usage, "kubectl delete") {
			t.Errorf("--%s usage must name the apply and delete commands; got %q", flagAddRolesToSA, sf.Usage)
		}
	})

	t.Run("repeated flag is rejected", func(t *testing.T) {
		err := runSnapshotCmdExpectErr(t, []string{
			"--" + flagAddRolesToSA, "sa-one",
			"--" + flagAddRolesToSA, "sa-two",
		})
		if err == nil {
			t.Fatalf("repeated --%s accepted; the last value would silently win", flagAddRolesToSA)
		}
		if !strings.Contains(err.Error(), flagAddRolesToSA) {
			t.Errorf("error = %q, want it to name --%s", err.Error(), flagAddRolesToSA)
		}
	})
}

// TestSnapshotCmd_ServiceAccountNameUsageStatesExactIfExists pins the one
// thing an operator has to learn from `--help`: --service-account-name is no
// longer only a prefix. A usage string that still says "prefix" alone would
// send an IRSA user straight into the silent credential loss this change
// exists to close.
func TestSnapshotCmd_ServiceAccountNameUsageStatesExactIfExists(t *testing.T) {
	for _, f := range snapshotCmd().Flags {
		sf, ok := f.(*cli.StringFlag)
		if !ok || sf.Name != "service-account-name" {
			continue
		}
		if !strings.Contains(sf.Usage, "xact-if-exists") {
			t.Errorf("--service-account-name usage does not state the exact-if-exists rule; got %q", sf.Usage)
		}
		return
	}
	t.Fatal("snapshot command must define --service-account-name")
}

// TestSnapshotCmd_AddRolesWritesManifestsWithoutACluster runs the real command
// action end to end. It is the test that pins the whole point of the flag: with
// KUBECONFIG pointed at a file no client can be built from and the in-cluster
// environment cleared, the invocation still succeeds and writes a reviewable
// directory. Any clientset construction, ServiceAccount lookup, or permission
// pre-flight on this path would fail here rather than pass quietly.
func TestSnapshotCmd_AddRolesWritesManifestsWithoutACluster(t *testing.T) {
	unreachable := filepath.Join(t.TempDir(), "not-a-kubeconfig")
	if seedErr := os.WriteFile(unreachable, []byte("this is not a kubeconfig\n"), 0o600); seedErr != nil {
		t.Fatalf("seeding the kubeconfig: %v", seedErr)
	}
	t.Setenv("KUBECONFIG", unreachable)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Chdir(t.TempDir())

	var buf bytes.Buffer
	cmd := snapshotCmd()
	cmd.Writer = &buf
	if err := cmd.Run(context.Background(), []string{
		"snapshot",
		"--namespace", "gpu-operator",
		"--" + flagAddRolesToSA, "irsa-snapshotter",
		"--discover-network",
	}); err != nil {
		t.Fatalf("snapshot --%s error = %v; the path must need no cluster at all", flagAddRolesToSA, err)
	}

	entries, readErr := os.ReadDir(".")
	if readErr != nil {
		t.Fatalf("reading the working directory: %v", readErr)
	}
	var dir string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "snapshot-rbac-") {
			dir = e.Name()
		}
	}
	if dir == "" {
		t.Fatalf("no snapshot-rbac-<run-id> directory written; got %v", entries)
	}

	for _, name := range []string{"01-role.yaml", "02-rolebinding.yaml", "03-clusterrole.yaml", "04-clusterrolebinding.yaml"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("reading %s: %v", name, err)
			continue
		}
		if !strings.HasPrefix(string(body), "# ") {
			t.Errorf("%s does not open with a YAML comment header", name)
		}
	}

	// The report goes to the command writer, not stdout, and must name the
	// directory and both halves of the operator's workflow.
	out := buf.String()
	for _, want := range []string{"NOTHING WAS APPLIED", dir, "kubectl apply -f", "kubectl delete -f", "MUTATING"} {
		if !strings.Contains(out, want) {
			t.Errorf("command output does not contain %q; got:\n%s", want, out)
		}
	}
}

// TestWriteManifestReport asserts the properties the output must state
// outright, because an operator who does not read them cannot discover any of
// them from the cluster: nothing was applied, where the manifests are, the
// command that makes the grant live, the command that removes it again, and
// that sharing one ServiceAccount waives per-run permission isolation. The
// --discover-network warning is separate because that grant is the one that
// is also mutating.
func TestWriteManifestReport(t *testing.T) {
	dir := "snapshot-rbac-20260821-142233-9f3a1c0b7e2d4a55"
	res := &snapshotter.AgentRolesResult{
		Dir:                dir,
		RunID:              "20260821-142233-9f3a1c0b7e2d4a55",
		Namespace:          "gpu-operator",
		ServiceAccountName: "irsa-snapshotter",
		Objects: []snapshotter.AgentRoleObject{
			{Kind: "Role", Name: "aicr-agent-irsa-snapshotter-rbac", Path: dir + "/01-role.yaml"},
			{Kind: "RoleBinding", Name: "aicr-agent-irsa-snapshotter-rbac", Path: dir + "/02-rolebinding.yaml"},
			{Kind: "ClusterRole", Name: "aicr-agent-gpu-operator.irsa-snapshotter-rbac", Path: dir + "/03-clusterrole.yaml"},
			{Kind: "ClusterRoleBinding", Name: "aicr-agent-gpu-operator.irsa-snapshotter-rbac", Path: dir + "/04-clusterrolebinding.yaml"},
		},
	}

	tests := []struct {
		name            string
		discoverNetwork bool
		wantSubstrings  []string
		notWant         string
	}{
		{
			name: "read-only grant",
			wantSubstrings: []string{
				`ServiceAccount "irsa-snapshotter" in namespace`,
				"NOTHING WAS APPLIED",
				"kubectl apply -f " + dir + "/",
				"kubectl delete -f " + dir + "/",
				"01-role.yaml",
				"role/aicr-agent-irsa-snapshotter-rbac",
				"clusterrolebinding/aicr-agent-gpu-operator.irsa-snapshotter-rbac",
				"not verified to exist",
				"permission isolation is waived",
				"aicr snapshot --namespace gpu-operator --service-account-name irsa-snapshotter",
			},
			notWant: "MUTATING",
		},
		{
			name:            "discovery grant warns about the mutating rules",
			discoverNetwork: true,
			wantSubstrings: []string{
				"NOTHING WAS APPLIED",
				"MUTATING",
				"grants them permanently, not for one run",
				"03-clusterrole.yaml",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := *res
			r.DiscoverNetwork = tt.discoverNetwork
			var buf bytes.Buffer
			writeManifestReport(&buf, &r)

			for _, want := range tt.wantSubstrings {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("report does not contain %q; got:\n%s", want, buf.String())
				}
			}
			if tt.notWant != "" && strings.Contains(buf.String(), tt.notWant) {
				t.Errorf("report contains %q but should not; got:\n%s", tt.notWant, buf.String())
			}
		})
	}
}

// stringFlagValue returns the declared Value: default of a string flag on cmd.
// It is the same fact testdata/cli-surface.golden pins, read from the live
// command so the two cannot drift apart silently.
func stringFlagValue(t *testing.T, cmd *cli.Command, flagName string) string {
	t.Helper()
	for _, f := range cmd.Flags {
		sf, ok := f.(*cli.StringFlag)
		if !ok || sf.Name != flagName {
			continue
		}
		return sf.Value
	}
	t.Fatalf("%s command must define --%s", cmd.Name, flagName)
	return ""
}

// TestSnapshotCmd_DeclaredNameDefaultsNeverReachTheDeployer is the regression
// guard for the two-sided requirement on --job-name and
// --service-account-name: the released v1 defaults stay declared, and neither
// is ever mistaken for something the operator typed.
//
// The declared half is a compatibility contract. "aicr" is what `--help`
// prints and what integrations read; dropping it is a breaking change to a
// frozen v1 surface that owes the deprecation window in RELEASING.md.
//
// The delivered half is a security property, and it is the one worth the
// test. Config.ServiceAccountName is exact-if-exists: agent.Deployer's
// resolveServiceAccount probes for a ServiceAccount of exactly that name and,
// on a hit, runs the agent under it and creates and deletes NO RBAC for the
// run. It returns early on an empty name precisely so aicr's own default
// cannot trigger that. If an unset flag resolved to "aicr" here instead, then
// on any cluster still carrying a leftover "aicr" ServiceAccount from a
// pre-ADR-020 install EVERY run would silently enter exact mode and manage no
// permissions at all. Exact mode must be reachable only from a name the
// operator actually supplied.
func TestSnapshotCmd_DeclaredNameDefaultsNeverReachTheDeployer(t *testing.T) {
	cmd := snapshotCmd()
	for _, flagName := range []string{"job-name", "service-account-name"} {
		if got := stringFlagValue(t, cmd, flagName); got != name {
			t.Errorf("--%s declared default = %q, want the released %q; removing it "+
				"breaks the frozen v1 CLI surface", flagName, got, name)
		}
	}

	// No name flags passed: both must arrive empty, so pkg/k8s/agent derives
	// "<NameBase>-<RunID>" and probes nothing.
	opts := captureSnapshotOpts(t, []string{"-o", "-"})
	if opts.serviceAccountName != "" {
		t.Errorf("serviceAccountName = %q for an unset flag, want empty; a "+
			"non-empty value is probed for existence and a hit disables RBAC "+
			"management for the whole run", opts.serviceAccountName)
	}
	if opts.jobName != "" {
		t.Errorf("jobName = %q for an unset flag, want empty", opts.jobName)
	}

	// The names the deployer actually builds from are unchanged: NameBase
	// carries the same "aicr" prefix the declared defaults used to supply.
	agentCfg := opts.toAgentConfig()
	if agentCfg.ServiceAccountName != "" || agentCfg.JobName != "" {
		t.Errorf("AgentConfig names = (job %q, sa %q), want both empty",
			agentCfg.JobName, agentCfg.ServiceAccountName)
	}
	if agentCfg.NameBase != name {
		t.Errorf("AgentConfig.NameBase = %q, want %q — without it the run-scoped "+
			"names would lose the prefix the released defaults produced",
			agentCfg.NameBase, name)
	}

	// Guard against the vacuous pass: a name the operator DID type must
	// survive, since that is the only route into exact-ServiceAccount mode.
	explicit := captureSnapshotOpts(t, []string{
		"-o", "-",
		"--job-name", "snapshot-gpu-nodes",
		"--service-account-name", "irsa-snapshotter",
	})
	if explicit.serviceAccountName != "irsa-snapshotter" {
		t.Errorf("serviceAccountName = %q, want the operator's %q",
			explicit.serviceAccountName, "irsa-snapshotter")
	}
	if explicit.jobName != "snapshot-gpu-nodes" {
		t.Errorf("jobName = %q, want the operator's %q",
			explicit.jobName, "snapshot-gpu-nodes")
	}

	// Passing the default explicitly is still explicit input: the operator
	// typed it, so it is probed like any other name.
	typedDefault := captureSnapshotOpts(t, []string{"-o", "-", "--service-account-name", name})
	if typedDefault.serviceAccountName != name {
		t.Errorf("serviceAccountName = %q for an explicitly passed %q, want it "+
			"preserved", typedDefault.serviceAccountName, name)
	}
}
