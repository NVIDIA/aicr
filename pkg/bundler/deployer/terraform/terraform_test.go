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
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const testBundlerVersion = "v1.0.0"

// update regenerates goldens under testdata/ when set via `go test -update`.
var update = flag.Bool("update", false, "update golden files")

// TestGenerate_Scenarios is the golden-file suite for Generator.Generate.
// Each row supplies a configured Generator, the testdata/<name>/ dir holding
// the expected output, and the files to byte-compare. Error paths and the
// structural invariants that no single golden can express live in their own
// tests below.
func TestGenerate_Scenarios(t *testing.T) {
	scenarios := []struct {
		name    string
		gen     *Generator
		goldens []string
	}{
		{
			// The base case, and the one that locks the graph: three
			// components where gpu-operator declares both of the others.
			// Both the concurrency claim (nfd and cert-manager carry no
			// depends_on) and the module-call placement of the edge are
			// visible in this golden.
			name: "upstream_helm",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				nfd := ref("nfd", "node-feature-discovery", "node-feature-discovery", "0.18.1", "https://kubernetes-sigs.github.io/node-feature-discovery/charts")
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				gpu.DependencyRefs = []string{"cert-manager", "nfd"}
				return &Generator{
					RecipeResult: recipeWith(cm, nfd, gpu),
					ComponentValues: map[string]map[string]any{
						"cert-manager": {"crds": map[string]any{"enabled": true}},
						"gpu-operator": {"driver": map[string]any{"enabled": false}},
					},
					Version: testBundlerVersion,
				}
			}(),
			goldens: []string{
				fileMain, fileVersions, fileVars, fileOutputs,
				filepath.Join(moduleDir, "main.tf"),
				filepath.Join(moduleDir, "variables.tf"),
			},
		},
		{
			// A component with post-manifests expands into two releases,
			// both inside one module. The dependent names the component and
			// gets the tail for free — with the flat shape it had to name
			// "module.agentgateway_crds_post" to avoid racing the manifests.
			name: "post_manifest_tail",
			gen: func() *Generator {
				crds := ref("agentgateway-crds", "agentgateway-system", "agentgateway-crds", "v1.5.0", "oci://cr.agentgateway.dev/charts")
				gw := ref("agentgateway", "agentgateway-system", "agentgateway", "v1.5.0", "oci://cr.agentgateway.dev/charts")
				gw.DependencyRefs = []string{"agentgateway-crds"}
				return &Generator{
					RecipeResult: recipeWith(crds, gw),
					Version:      testBundlerVersion,
					ComponentPostManifests: map[string]map[string][]byte{
						"agentgateway-crds": {"gateway-api-crds.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: placeholder\n")},
					},
				}
			}(),
			goldens: []string{fileMain},
		},
		{
			// --serial discards the declared graph for one linear chain.
			name: "serial",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				nfd := ref("nfd", "node-feature-discovery", "node-feature-discovery", "0.18.1", "https://kubernetes-sigs.github.io/node-feature-discovery/charts")
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				gpu.DependencyRefs = []string{"cert-manager", "nfd"}
				return &Generator{
					RecipeResult: recipeWith(cm, nfd, gpu),
					Version:      testBundlerVersion,
					Serial:       true,
				}
			}(),
			goldens: []string{fileMain},
		},
		{
			// The containment shim adds a resource and a lifecycle block to
			// the shared module, and one more argument to every call.
			name: "cluster_rollover",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				return &Generator{
					RecipeResult:    recipeWith(cm),
					Version:         testBundlerVersion,
					ClusterRollover: true,
				}
			}(),
			goldens: []string{
				fileMain, fileVersions,
				filepath.Join(moduleDir, "main.tf"),
				filepath.Join(moduleDir, "variables.tf"),
			},
		},
		{
			// A component the shared override table marks asynchronous
			// carries literal wait/timeout arguments instead of the
			// bundle-wide variables, with the reason inline. Rendering it
			// as var.wait would let an operator silently re-enable a wait
			// that is known to time out.
			name: "async_component",
			gen: func() *Generator {
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				kai := ref("kai-scheduler", "kai-scheduler", "kai-scheduler", "v0.16.9", "oci://ghcr.io/kai-scheduler/kai-scheduler")
				kai.DependencyRefs = []string{"gpu-operator"}
				return &Generator{
					RecipeResult: recipeWith(gpu, kai),
					Version:      testBundlerVersion,
				}
			}(),
			goldens: []string{fileMain},
		},
		{
			// Child-module form: no provider block, no connection
			// variables, no tfvars example. The rollover shim's
			// cluster_keepers becomes a required variable, since there is
			// no caller-independent default a child module could carry.
			name: "child_module",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				return &Generator{
					RecipeResult:    recipeWith(cm),
					Version:         testBundlerVersion,
					ChildModule:     true,
					ClusterRollover: true,
				}
			}(),
			goldens: []string{fileMain, fileVersions, fileVars},
		},
		{
			// A component with dynamic value paths gets cluster-values.yaml
			// layered after values.yaml, so operator edits win.
			name: "dynamic_values",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				return &Generator{
					RecipeResult: recipeWith(cm),
					ComponentValues: map[string]map[string]any{
						"cert-manager": {"replicaCount": 1, "global": map[string]any{"imagePullSecrets": []any{}}},
					},
					DynamicValues: map[string][]string{"cert-manager": {"global.imagePullSecrets"}},
					Version:       testBundlerVersion,
				}
			}(),
			goldens: []string{fileMain},
		},
		{
			// A readiness gate becomes the last slot in its component's
			// module, so a dependent's depends_on covers it without naming
			// it. The module golden is what proves the slot ignores
			// var.wait.
			name: "readiness",
			gen: func() *Generator {
				nfd := ref("nfd", "node-feature-discovery", "node-feature-discovery", "0.18.1", "https://kubernetes-sigs.github.io/node-feature-discovery/charts")
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				gpu.DependencyRefs = []string{"nfd"}
				return &Generator{
					RecipeResult:       recipeWith(nfd, gpu),
					Version:            testBundlerVersion,
					ComponentReadiness: map[string]map[string][]byte{"gpu-operator": {"readiness.yaml": gateJob}},
				}
			}(),
			goldens: []string{
				fileMain, fileOutputs,
				filepath.Join(moduleDir, "main.tf"),
				filepath.Join(moduleDir, "variables.tf"),
			},
		},
		{
			// All four phases on one component: the shape the flat renderer
			// turned into four sibling calls and three inter-call edges.
			name: "composite",
			gen: func() *Generator {
				gw := ref("agentgateway", "agentgateway-system", "agentgateway", "v1.5.0", "oci://cr.agentgateway.dev/charts")
				sink := ref("nvsentinel", "nvsentinel", "nvsentinel", "v0.3.0", "https://helm.ngc.nvidia.com/nvidia")
				sink.DependencyRefs = []string{"agentgateway"}
				return &Generator{
					RecipeResult:           recipeWith(gw, sink),
					Version:                testBundlerVersion,
					ComponentPreManifests:  map[string]map[string][]byte{"agentgateway": {"quota.yaml": manifestDoc}},
					ComponentPostManifests: map[string]map[string][]byte{"agentgateway": {"gateway.yaml": manifestDoc}},
					ComponentReadiness:     map[string]map[string][]byte{"agentgateway": {"readiness.yaml": gateJob}},
				}
			}(),
			goldens: []string{fileMain, fileOutputs},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			outputDir := t.TempDir()
			out, err := sc.gen.Generate(context.Background(), outputDir)
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			if out == nil || len(out.Files) == 0 {
				t.Fatalf("Generate() returned no files")
			}
			// Every scenario's main.tf must be fmt-stable, not only the
			// ones whose goldens happen to be checked in.
			assertFmtStable(t, filepath.Join(outputDir, fileMain))
			for _, rel := range sc.goldens {
				assertGolden(t, outputDir, filepath.Join("testdata", sc.name), rel)
			}
			// A child module is configured by its caller's module block, so
			// a tfvars example would be a file nothing can read.
			_, statErr := os.Stat(filepath.Join(outputDir, fileTfvars))
			if sc.gen.ChildModule && statErr == nil {
				t.Errorf("child-module bundle emitted %s", fileTfvars)
			}
			if !sc.gen.ChildModule && statErr != nil {
				t.Errorf("root-module bundle is missing %s: %v", fileTfvars, statErr)
			}
		})
	}
}

// TestGenerate_EdgeToDisabledComponentIsDropped locks the rule that keeps a
// generated bundle parseable: an edge to a component that was filtered out
// must disappear, because `depends_on = [module.absent]` is not a plan-time
// failure the operator can debug, it is a parse error in output AICR reported
// as successfully generated.
func TestGenerate_EdgeToDisabledComponentIsDropped(t *testing.T) {
	cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
	cm.Overrides = map[string]any{"enabled": false}
	gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
	gpu.DependencyRefs = []string{"cert-manager"}

	g := &Generator{RecipeResult: recipeWith(cm, gpu), Version: testBundlerVersion}
	outputDir := t.TempDir()
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	main := readFile(t, filepath.Join(outputDir, fileMain))
	if strings.Contains(main, "cert_manager") {
		t.Errorf("main.tf references the disabled component:\n%s", main)
	}
	// "depends_on" also appears in the generated header comment, so match
	// the argument itself.
	if strings.Contains(main, "depends_on = [") {
		t.Errorf("main.tf kept a depends_on with no surviving target:\n%s", main)
	}
}

func TestGenerate_RejectsKustomizeComponent(t *testing.T) {
	k := ref("my-kustomization", "default", "", "", "https://github.com/example/repo")
	k.Type = recipe.ComponentTypeKustomize

	g := &Generator{RecipeResult: recipeWith(k), Version: testBundlerVersion}
	_, err := g.Generate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Generate() accepted a Kustomize component")
	}
	if !strings.Contains(err.Error(), "Helm components only") {
		t.Errorf("error does not name the limitation: %v", err)
	}
}

// TestGenerate_RejectsControlCharacters: a recipe value with a newline would
// end the `# <dir> — <chart> <version>` comment above a module call and put
// the rest of the value at the top level of main.tf, where a resource with a
// local-exec provisioner runs on the operator's host at apply. Generate
// refuses such a value before writing anything.
func TestGenerate_RejectsControlCharacters(t *testing.T) {
	const payload = "resource \"terraform_data\" \"pwn\" {}"
	cases := map[string]recipe.ComponentRef{
		"version":   ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2\n"+payload, "https://charts.jetstack.io"),
		"chart":     ref("cert-manager", "cert-manager", "cert-manager\r\n"+payload, "v1.17.2", "https://charts.jetstack.io"),
		"source":    ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io\n"+payload),
		"name":      ref("cert-manager\n"+payload, "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
		"namespace": ref("cert-manager", "cert-manager\n"+payload, "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
		// DEL and a C1 control (NEL) are not newlines and still count.
		"del": ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2\x7f"+payload, "https://charts.jetstack.io"),
		"c1":  ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2\u0085"+payload, "https://charts.jetstack.io"),
	}
	for field, r := range cases {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			g := &Generator{RecipeResult: recipeWith(r), Version: testBundlerVersion}
			_, err := g.Generate(context.Background(), dir)
			if err == nil {
				t.Fatalf("Generate() accepted a %s with a control character", field)
			}
			if !strings.Contains(err.Error(), "control character") {
				t.Errorf("error does not name the problem: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, fileMain)); statErr == nil {
				t.Errorf("main.tf was written despite the rejection")
			}
			if strings.ContainsFunc(err.Error(), unicode.IsControl) {
				t.Errorf("error echoes the raw control character: %q", err.Error())
			}
		})
	}
}

// TestCommentText guards the second line of defense: whatever reaches a
// comment, it stays one line.
func TestCommentText(t *testing.T) {
	cases := map[string]string{
		"plain":                      "plain",
		"011-x — chart v1\nresource": "011-x — chart v1 resource",
		"a\r\nb\tc\x1b[0m":           "a  b c [0m",
		"unicode — kept":             "unicode — kept",
	}
	for in, want := range cases {
		if got := commentText(in); got != want {
			t.Errorf("commentText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestModulePath: the path.module interpolation stays live and the folder
// name is escaped, so a folder name cannot close the string.
func TestModulePath(t *testing.T) {
	cases := map[string]string{
		`011-gpu-operator`:  `"${path.module}/011-gpu-operator"`,
		`a"b`:               `"${path.module}/a\"b"`,
		`a${var.x}`:         `"${path.module}/a$${var.x}"`,
		"011-x/values.yaml": `"${path.module}/011-x/values.yaml"`,
	}
	for in, want := range cases {
		if got := modulePath(in); got != want {
			t.Errorf("modulePath(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestGenerate_DeploymentStepsFollowTheForm: a child module has no provider
// and no connection variables, so the printed steps must send the operator to
// the calling configuration, not into the bundle with kubeconfig advice.
func TestGenerate_DeploymentStepsFollowTheForm(t *testing.T) {
	gen := func(child bool) *deployer.Output {
		t.Helper()
		cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
		g := &Generator{RecipeResult: recipeWith(cm), Version: testBundlerVersion, ChildModule: child, ClusterRollover: true}
		out, err := g.Generate(context.Background(), t.TempDir())
		if err != nil {
			t.Fatalf("Generate(child=%v): %v", child, err)
		}
		return out
	}
	joined := func(ss []string) string { return strings.Join(ss, "\n") }

	root := gen(false)
	if !strings.HasPrefix(root.DeploymentSteps[0], "cd ") {
		t.Errorf("root steps do not start in the bundle: %q", root.DeploymentSteps[0])
	}
	if !strings.Contains(joined(root.DeploymentNotes), "kubeconfig_path") {
		t.Errorf("root notes do not say how to point the bundle at a cluster:\n%s", joined(root.DeploymentNotes))
	}
	if !strings.Contains(joined(root.DeploymentNotes), "The variable is required: set it in terraform.tfvars") {
		t.Errorf("root rollover note does not say cluster_keepers is required:\n%s", joined(root.DeploymentNotes))
	}

	child := gen(true)
	if !strings.Contains(child.DeploymentSteps[0], "as a module") {
		t.Errorf("child steps do not send the operator to the calling configuration: %q", child.DeploymentSteps[0])
	}
	all := joined(child.DeploymentSteps) + "\n" + joined(child.DeploymentNotes)
	for _, absent := range []string{"kubeconfig_path", "cluster_host", "cd ", "root-module default"} {
		if strings.Contains(all, absent) {
			t.Errorf("child-module output mentions %q, which a child module does not have:\n%s", absent, all)
		}
	}
	if !strings.Contains(joined(child.DeploymentNotes), "The variable is required") {
		t.Errorf("child rollover note does not say cluster_keepers is required:\n%s", joined(child.DeploymentNotes))
	}
}

func TestGenerate_RejectsCyclicGraph(t *testing.T) {
	a := ref("a", "default", "a", "1.0.0", "https://example.com/charts")
	b := ref("b", "default", "b", "1.0.0", "https://example.com/charts")
	a.DependencyRefs = []string{"b"}
	b.DependencyRefs = []string{"a"}

	g := &Generator{RecipeResult: recipeWith(a, b), Version: testBundlerVersion}
	if _, err := g.Generate(context.Background(), t.TempDir()); err == nil {
		t.Fatal("Generate() accepted a cyclic dependency graph")
	}
}

func TestGenerate_RejectsNilRecipeResult(t *testing.T) {
	g := &Generator{Version: testBundlerVersion}
	if _, err := g.Generate(context.Background(), t.TempDir()); err == nil {
		t.Fatal("Generate() accepted a nil RecipeResult")
	}
}

func TestGenerate_RespectsCancelledContext(t *testing.T) {
	cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
	g := &Generator{RecipeResult: recipeWith(cm), Version: testBundlerVersion}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Generate(ctx, t.TempDir()); err == nil {
		t.Fatal("Generate() ignored a cancelled context")
	}
}

func TestModuleLabel(t *testing.T) {
	cases := map[string]string{
		"gpu-operator":      "gpu_operator",
		"nvidia.dra.driver": "nvidia_dra_driver",
		"cert-manager-post": "cert_manager_post",
		"1password-connect": "_1password_connect",
		"already_fine":      "already_fine",
	}
	for in, want := range cases {
		if got := moduleLabel(in); got != want {
			t.Errorf("moduleLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildComponents_LabelCollision covers the one way the name -> label
// mapping can lose information. Two component names that differ only in a
// separator collapse to the same module label, which terraform reports as a
// duplicate block in a file AICR wrote; catching it here attributes it to the
// recipe instead.
func TestBuildComponents_LabelCollision(t *testing.T) {
	a := ref("gpu-operator", "default", "gpu-operator", "1.0.0", "https://example.com/charts")
	b := ref("gpu.operator", "default", "gpu-operator", "1.0.0", "https://example.com/charts")

	g := &Generator{RecipeResult: recipeWith(a, b), Version: testBundlerVersion}
	_, err := g.Generate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Generate() accepted two components that map to the same module label")
	}
	if !strings.Contains(err.Error(), "module label") {
		t.Errorf("error does not explain the collision: %v", err)
	}
}

// TestGenerate_AsyncOverrideDoesNotDisarmTheGate locks Finding 6 of the
// review: the shared override table marks kai-scheduler asynchronous, and with
// one module call per FOLDER that wait=false literal landed on the component's
// readiness call too — a gate that submits its Job and returns. The slot reads
// a constant instead of var.wait, so the module cannot express that mistake.
func TestGenerate_AsyncOverrideDoesNotDisarmTheGate(t *testing.T) {
	kai := ref("kai-scheduler", "kai-scheduler", "kai-scheduler", "v0.16.9", "oci://ghcr.io/kai-scheduler/kai-scheduler")
	g := &Generator{
		RecipeResult:       recipeWith(kai),
		Version:            testBundlerVersion,
		ComponentReadiness: map[string]map[string][]byte{"kai-scheduler": {"readiness.yaml": gateJob}},
	}
	outputDir := t.TempDir()
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// The call carries the override, and pins atomic with it: Helm forces
	// wait on whenever atomic is set, so var.atomic would undo the override.
	main := readFile(t, filepath.Join(outputDir, fileMain))
	if !strings.Contains(main, "wait    = false") {
		t.Errorf("main.tf lost the async override:\n%s", main)
	}
	if !strings.Contains(main, "atomic  = false") || strings.Contains(stripComments(main), "atomic  = var.atomic") {
		t.Errorf("the async override leaves atomic on var.atomic, which re-arms the wait:\n%s", main)
	}
	// ...and the gate slot ignores it.
	mod := readFile(t, filepath.Join(outputDir, moduleDir, "main.tf"))
	const gateBlock = `resource "helm_release" "readiness"`
	at := strings.Index(mod, gateBlock)
	if at < 0 {
		t.Fatalf("module has no readiness slot:\n%s", mod)
	}
	gate := stripComments(mod[at:])
	if strings.Contains(gate, "var.wait") {
		t.Errorf("the readiness slot reads var.wait, so an async component disarms its own gate:\n%s", gate)
	}
	if !strings.Contains(gate, "wait    = true") {
		t.Errorf("the readiness slot does not wait, so it does not gate:\n%s", gate)
	}
	// The gate blocks as a helm hook, not as a tracked Job. wait_for_jobs
	// covers the release's own resource set, which a hook is not, so its
	// presence here would be inert decoration -- and deploy.sh omits the
	// same flag on the same release for the same reason.
	if strings.Contains(gate, "wait_for_jobs") {
		t.Errorf("the readiness slot carries an inert wait_for_jobs:\n%s", gate)
	}
	// The gate owns the deadline. helm's timeout on this release is derived
	// from the gate's max-wait plus deploy.sh's buffer, never var.timeout,
	// whose default (15m) would have helm give up on a gate that is allowed
	// to wait 90m.
	if strings.Contains(gate, "var.timeout") {
		t.Errorf("the readiness slot reads var.timeout, so helm can preempt the gate:\n%s", gate)
	}
	wantTimeout := fmt.Sprintf("timeout = %d", int((defaults.ReadinessGateMaxWait + defaults.ReadinessGateHelmTimeoutBuffer).Seconds()))
	if !strings.Contains(gate, wantTimeout) {
		t.Errorf("the readiness slot's timeout is not derived from the gate's deadline (want %q):\n%s", wantTimeout, gate)
	}
	// And the ordinary slots carry no wait_for_jobs either: deploy.sh passes
	// none for them, so "ready" means the same thing under both deployers.
	if ordinary := stripComments(mod[:at]); strings.Contains(ordinary, "wait_for_jobs") {
		t.Errorf("an ordinary slot waits for bare Jobs, which deploy.sh does not:\n%s", ordinary)
	}
}

// TestGenerate_GateKeepsItsHookAnnotations pins the half of the gate contract
// that lives outside this package. gatemanifest annotates the gate Job as a
// helm hook; localformat strips helm.sh/hook* from every folder it writes
// EXCEPT the readiness phase. Narrow that exemption to one deployer and this
// gate silently stops re-asserting on upgrade, because a Job's spec.template
// is immutable and nothing else deletes and recreates it.
func TestGenerate_GateKeepsItsHookAnnotations(t *testing.T) {
	gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.0", "https://helm.ngc.nvidia.com/nvidia")
	g := &Generator{
		RecipeResult:       recipeWith(gpu),
		Version:            testBundlerVersion,
		ComponentReadiness: map[string]map[string][]byte{"gpu-operator": {"readiness.yaml": annotatedGateJob}},
	}
	outputDir := t.TempDir()
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	matches, globErr := filepath.Glob(filepath.Join(outputDir, "*-readiness", "templates", "*.yaml"))
	if globErr != nil {
		t.Fatalf("glob gate manifest: %v", globErr)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one rendered gate manifest, got %v", matches)
	}
	job := readFile(t, matches[0])
	for _, want := range []string{
		"helm.sh/hook: post-install,post-upgrade",
		"helm.sh/hook-delete-policy: before-hook-creation",
	} {
		if !strings.Contains(job, want) {
			t.Errorf("the shipped gate lost %q:\n%s", want, job)
		}
	}
}

// TestGenerate_RolloverContainsAndNothingElse pins the two halves of the
// containment shim that a golden diff states but does not argue for.
//
// The negative half is the point. replace_triggered_by and
// create_before_destroy are independent: the first decides WHETHER a release
// is replaced, the second decides the ORDER. Adding create_before_destroy is a
// one-line edit that no golden reads as a warning, and it changes the bundle's
// behavior from destroy-then-create -- Terraform's default -- to a
// create-then-destroy that two helm_releases cannot actually perform, because
// a release name is its identity and the new one collides with the old.
func TestGenerate_RolloverContainsAndNothingElse(t *testing.T) {
	cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
	g := &Generator{
		RecipeResult:       recipeWith(cm),
		Version:            testBundlerVersion,
		ClusterRollover:    true,
		ComponentReadiness: map[string]map[string][]byte{"cert-manager": {"readiness.yaml": gateJob}},
	}
	outputDir := t.TempDir()
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	mod := stripComments(readFile(t, filepath.Join(outputDir, moduleDir, "main.tf")))

	if !strings.Contains(mod, `resource "terraform_data" "cluster"`) {
		t.Errorf("no containment shim in the module:\n%s", mod)
	}
	if !strings.Contains(mod, "triggers_replace = var.cluster_keepers") {
		t.Errorf("the shim is not keyed on the cluster keepers:\n%s", mod)
	}
	// Never derived from the connection: an endpoint can outlive the cluster
	// behind it, so the root form declares its own keepers variable too.
	if main := stripComments(readFile(t, filepath.Join(outputDir, fileMain))); strings.Contains(main, "var.cluster_host") {
		t.Errorf("the shim is keyed on the connection, not on the operator's keepers:\n%s", main)
	}
	if vars := readFile(t, filepath.Join(outputDir, fileVars)); !strings.Contains(vars, `variable "cluster_keepers"`) {
		t.Errorf("root module declares no cluster_keepers variable:\n%s", vars)
	}
	if got := strings.Count(mod, "replace_triggered_by = [terraform_data.cluster]"); got != 4 {
		t.Errorf("slots tied to the shim = %d, want 4 (pre, primary, post, readiness):\n%s", got, mod)
	}
	if strings.Contains(mod, "create_before_destroy") {
		t.Errorf("rollover replaces create-before-destroy; it must replace destroy-then-create:\n%s", mod)
	}

	// terraform_data is built in, so opting into rollover must not add a
	// provider the operator then has to mirror into an air-gapped registry.
	for _, f := range []string{fileVersions, filepath.Join(moduleDir, "main.tf")} {
		if body := readFile(t, filepath.Join(outputDir, f)); strings.Contains(body, "hashicorp/null") {
			t.Errorf("%s still requires hashicorp/null:\n%s", f, body)
		}
	}
}

// stripComments drops whole-line comments so an assertion about generated
// ARGUMENTS is not satisfied, or defeated, by prose that mentions them.
func stripComments(hcl string) string {
	var kept []string
	for _, line := range strings.Split(hcl, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func TestHCLString(t *testing.T) {
	cases := map[string]string{
		`plain`:             `"plain"`,
		`with "quotes"`:     `"with \"quotes\""`,
		`back\slash`:        `"back\\slash"`,
		`${var.evil}`:       `"$${var.evil}"`,
		`%{ if true }`:      `"%%{ if true }"`,
		"tab\tand\nnewline": `"tab\tand\nnewline"`,
	}
	for in, want := range cases {
		if got := hclString(in); got != want {
			t.Errorf("hclString(%q) = %s, want %s", in, got, want)
		}
	}
}

// attrLine matches a single `name = value` argument line, capturing the
// indent and the name so assertFmtStable can check the column the `=` landed
// in without parsing HCL.
var attrLine = regexp.MustCompile(`^(\s*)([A-Za-z_][A-Za-z0-9_]*)( +)= `)

// assertFmtStable verifies that every contiguous run of argument lines has
// its `=` aligned to exactly one space past the longest name in the run —
// which is what `terraform fmt` produces, and therefore what makes generated
// output byte-identical to committed output.
//
// A run ends at any line that is not a simple argument (a blank line, a
// closing brace, a list element), matching fmt's own reset rule. Lines inside
// multi-line values are skipped for the same reason: they are not arguments.
func assertFmtStable(t *testing.T, path string) {
	t.Helper()
	var run []struct {
		lineNo int
		indent string
		name   string
		eqCol  int
	}
	flush := func() {
		if len(run) == 0 {
			return
		}
		want := 0
		for _, l := range run {
			if n := len(l.indent) + len(l.name); n > want {
				want = n
			}
		}
		want++ // the single space before '='
		for _, l := range run {
			if l.eqCol != want {
				t.Errorf("%s:%d: %q has '=' at column %d, terraform fmt would put it at %d",
					filepath.Base(path), l.lineNo, l.name, l.eqCol, want)
			}
		}
		run = run[:0]
	}

	for i, line := range strings.Split(readFile(t, path), "\n") {
		m := attrLine.FindStringSubmatch(line)
		if m == nil {
			flush()
			continue
		}
		run = append(run, struct {
			lineNo int
			indent string
			name   string
			eqCol  int
		}{i + 1, m[1], m[2], len(m[1]) + len(m[2]) + len(m[3])})
	}
	flush()
}

// gateJob and manifestDoc stand in for real payloads. localformat renders both
// through the manifest pipeline and wraps them in a chart; their content only
// has to be valid YAML with a kind localformat does not special-case.
var (
	gateJob = []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: gate\n  namespace: '{{ .Release.Namespace }}'\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n        - name: gate\n          image: ghcr.io/nvidia/aicr-gate:latest\n")

	manifestDoc = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: placeholder\n")

	// annotatedGateJob is what gatemanifest.Render actually emits for this
	// deployer: a Job annotated as a post-install/post-upgrade helm hook.
	annotatedGateJob = []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: gate\n  namespace: '{{ .Release.Namespace }}'\n  annotations:\n    helm.sh/hook: post-install,post-upgrade\n    helm.sh/hook-delete-policy: before-hook-creation\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n        - name: gate\n          image: ghcr.io/nvidia/aicr-gate:latest\n")
)

func ref(name, ns, chart, version, source string) recipe.ComponentRef {
	return recipe.ComponentRef{
		Name:      name,
		Namespace: ns,
		Chart:     chart,
		Version:   version,
		Source:    source,
		Type:      recipe.ComponentTypeHelm,
	}
}

func recipeWith(refs ...recipe.ComponentRef) *recipe.RecipeResult {
	r := &recipe.RecipeResult{}
	r.Metadata.Version = testBundlerVersion
	r.ComponentRefs = refs
	order := make([]string, 0, len(refs))
	for _, ref := range refs {
		order = append(order, ref.Name)
	}
	r.DeploymentOrder = order
	return r
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func assertGolden(t *testing.T, outDir, goldenDir, relPath string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(outDir, relPath))
	if err != nil {
		t.Fatalf("read actual %s: %v", relPath, err)
	}
	goldenPath := filepath.Join(goldenDir, relPath)
	if *update {
		if err = os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err = os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", relPath, got, want)
	}
}

// TestGenerate_StripsHelmOnlyFiles locks the folder contract: localformat
// writes helm's install.sh and upstream.env into every folder, and a terraform
// bundle must ship neither. Not clutter — install.sh installs the release
// names Terraform manages, so a second entry point beside the state is a way
// to drift it. The files must also leave the output record, or checksums.txt
// would attest to paths that do not exist.
func TestGenerate_StripsHelmOnlyFiles(t *testing.T) {
	cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
	g := &Generator{
		RecipeResult:       recipeWith(cm),
		Version:            testBundlerVersion,
		IncludeChecksums:   true,
		ComponentReadiness: map[string]map[string][]byte{"cert-manager": {"readiness.yaml": annotatedGateJob}},
	}
	outputDir := t.TempDir()
	out, err := g.Generate(context.Background(), outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	for _, rel := range out.Files {
		if helmOnlyFile(filepath.Base(rel)) {
			t.Errorf("output record lists a helm-only file: %s", rel)
		}
	}
	var folders int
	walkErr := filepath.WalkDir(outputDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if helmOnlyFile(d.Name()) {
			t.Errorf("bundle contains a helm-only file: %s", path)
		}
		if d.Name() == fileValues {
			folders++
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
	if folders != 2 {
		t.Errorf("values.yaml folders = %d, want 2 (chart and readiness)", folders)
	}
	checksums := readFile(t, filepath.Join(outputDir, "checksums.txt"))
	if strings.Contains(checksums, "install.sh") || strings.Contains(checksums, "upstream.env") {
		t.Errorf("checksums.txt attests to a stripped file:\n%s", checksums)
	}
}
