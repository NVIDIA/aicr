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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const testBundlerVersion = "v1.0.0"

// update regenerates goldens under testdata/ when set via `go test -update`.
var update = flag.Bool("update", false, "update golden files")

// deploymentGolden is the file a scenario's deployment steps and notes are
// compared as, since they are printed, not written into the bundle.
const deploymentGolden = "deployment.txt"

// scenario is one row of the golden suite: a configured Generator, the
// testdata/<name>/ directory holding what it must render, and the files to
// byte-compare. absent lists files the bundle must not contain. steps adds
// the printed deployment steps and notes as deployment.txt, with the output
// directory spelled <bundle>.
type scenario struct {
	name    string
	gen     *Generator
	goldens []string
	absent  []string
	steps   bool
}

// TestGenerate_Scenarios is the golden suite for Generator.Generate: every
// file the deployer renders is compared byte for byte, so a reviewer reads the
// file an operator gets rather than a fragment of it. The comment on each
// scenario names the invariant its goldens hold. Error paths, which render no
// file, live in their own tests below.
func TestGenerate_Scenarios(t *testing.T) {
	moduleFiles := []string{
		filepath.Join(moduleDir, "main.tf"),
		filepath.Join(moduleDir, "variables.tf"),
		filepath.Join(moduleDir, "outputs.tf"),
	}
	rootFiles := append([]string{fileMain, fileVersions, fileVars, fileOutputs, fileReadme, fileTfvars}, moduleFiles...)

	scenarios := []scenario{
		{
			// The base case, and the one that locks the graph: three
			// components where gpu-operator declares both of the others.
			// Both the concurrency claim (nfd and cert-manager carry no
			// depends_on) and the module-call placement of the edge are
			// visible in main.tf. Every root-form file is compared here.
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
			goldens: rootFiles,
			steps:   true,
		},
		{
			// A component with post-manifests expands into two releases,
			// both inside one module. The dependent names the component and
			// gets the tail for free — with the flat shape it had to name
			// "module.agentgateway_crds_post" to avoid racing the manifests.
			// The post slot carries its folder's content digest.
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
			// --serial discards the declared graph for one linear chain, and
			// the README says so in place of the concurrency paragraph.
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
			goldens: []string{fileMain, fileReadme},
		},
		{
			// The containment shim, with a gate so all four slots are in
			// view. The module golden holds both halves of the contract: a
			// terraform_data keyed on var.cluster_keepers (built in, so
			// hashicorp/helm stays the only provider in versions.tf), and a
			// replace_triggered_by on every slot with NO create_before_destroy
			// — replace_triggered_by decides whether a release is replaced,
			// create_before_destroy decides the order, and two helm_releases
			// cannot swap create-then-destroy because the name is the
			// identity. The root variables.tf requires the keepers rather
			// than defaulting them, and main.tf passes var.cluster_keepers,
			// never a connection attribute. README, tfvars and the printed
			// notes carry the same facts in prose.
			name: "cluster_rollover",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				return &Generator{
					RecipeResult:       recipeWith(cm),
					Version:            testBundlerVersion,
					ClusterRollover:    true,
					ComponentReadiness: map[string]map[string][]byte{"cert-manager": {"readiness.yaml": gateJob}},
				}
			}(),
			goldens: rootFiles,
			steps:   true,
		},
		{
			// A component the shared override table marks asynchronous
			// carries literal wait/timeout arguments instead of the
			// bundle-wide variables, with the reason inline, and atomic
			// pinned false beside them: Helm forces wait on whenever atomic
			// is set, so var.atomic would re-arm the wait. Rendering var.wait
			// would let an operator silently re-enable a wait known to time
			// out. The gate slot in the module golden reads none of those:
			// wait is a constant, the timeout is derived from the gate's
			// deadline rather than var.timeout, and there is no
			// wait_for_jobs on any slot (the gate blocks as a helm hook, and
			// deploy.sh passes none either), so an async component cannot
			// disarm its own gate.
			name: "async_component",
			gen: func() *Generator {
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				kai := ref("kai-scheduler", "kai-scheduler", "kai-scheduler", "v0.16.9", "oci://ghcr.io/kai-scheduler/kai-scheduler")
				kai.DependencyRefs = []string{"gpu-operator"}
				return &Generator{
					RecipeResult:       recipeWith(gpu, kai),
					Version:            testBundlerVersion,
					ComponentReadiness: map[string]map[string][]byte{"kai-scheduler": {"readiness.yaml": gateJob}},
				}
			}(),
			goldens: []string{fileMain, filepath.Join(moduleDir, "main.tf")},
		},
		{
			// Child-module form: no provider block, no connection
			// variables, no tfvars example, and printed steps that send the
			// operator to the calling configuration rather than into the
			// bundle with kubeconfig advice. The rollover shim's
			// cluster_keepers is a required variable here as in the root
			// form.
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
			goldens: []string{fileMain, fileVersions, fileVars, fileOutputs, fileReadme},
			absent:  []string{fileTfvars},
			steps:   true,
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
			// var.wait. The rendered gate chart is compared too: gatemanifest
			// annotates the Job as a post-install/post-upgrade helm hook with
			// hook-delete-policy before-hook-creation, and localformat strips
			// helm.sh/hook* from every folder EXCEPT the readiness phase.
			// Narrow that exemption and the gate silently stops re-asserting
			// on upgrade, because a Job's spec.template is immutable.
			name: "readiness",
			gen: func() *Generator {
				nfd := ref("nfd", "node-feature-discovery", "node-feature-discovery", "0.18.1", "https://kubernetes-sigs.github.io/node-feature-discovery/charts")
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				gpu.DependencyRefs = []string{"nfd"}
				return &Generator{
					RecipeResult:       recipeWith(nfd, gpu),
					Version:            testBundlerVersion,
					ComponentReadiness: map[string]map[string][]byte{"gpu-operator": {"readiness.yaml": annotatedGateJob}},
				}
			}(),
			goldens: append([]string{
				fileMain, fileOutputs,
				filepath.Join("003-gpu-operator-readiness", "Chart.yaml"),
				filepath.Join("003-gpu-operator-readiness", "templates", "readiness.yaml"),
			}, moduleFiles...),
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
		{
			// --vendor-charts: the primary is installed from the wrapper
			// chart localformat wrote into the bundle, so its module call
			// carries a path and a content digest instead of upstream
			// coordinates, and the printed notes say no registry is needed.
			name: "vendored",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				nfd := ref("nfd", "node-feature-discovery", "node-feature-discovery", "0.18.1", "https://kubernetes-sigs.github.io/node-feature-discovery/charts")
				nfd.DependencyRefs = []string{"cert-manager"}
				return &Generator{
					RecipeResult: recipeWith(cm, nfd),
					Version:      testBundlerVersion,
					VendorCharts: true,
					Puller:       &fakePuller{},
				}
			}(),
			goldens: []string{fileMain, fileReadme},
			steps:   true,
		},
		{
			// An edge to a component the bundle filtered out disappears
			// rather than rendering `depends_on = [module.absent]`, which is
			// not a plan-time failure the operator can debug but a parse
			// error in output AICR reported as generated. The golden has no
			// cert_manager call and no depends_on at all.
			name: "dropped_edge",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				cm.Overrides = map[string]any{"enabled": false}
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				gpu.DependencyRefs = []string{"cert-manager"}
				return &Generator{RecipeResult: recipeWith(cm, gpu), Version: testBundlerVersion}
			}(),
			goldens: []string{fileMain},
		},
		{
			// checksums.txt is the bundle's closed world: it lists every
			// file that ships, so it is also where helm's install.sh and
			// upstream.env would show up if the strip ever stopped covering
			// a folder. It must attest to the .tf layer, the module, each
			// folder's values files and the gate chart, and nothing else.
			name: "checksums",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				return &Generator{
					RecipeResult:       recipeWith(cm),
					Version:            testBundlerVersion,
					IncludeChecksums:   true,
					ComponentReadiness: map[string]map[string][]byte{"cert-manager": {"readiness.yaml": annotatedGateJob}},
				}
			}(),
			goldens: []string{"checksums.txt"},
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
			goldenDir := filepath.Join("testdata", sc.name)
			// Every scenario's main.tf must be fmt-stable, not only the
			// ones whose goldens happen to be checked in.
			assertFmtStable(t, filepath.Join(outputDir, fileMain))
			for _, rel := range sc.goldens {
				assertGolden(t, outputDir, goldenDir, rel)
			}
			for _, rel := range sc.absent {
				if _, statErr := os.Stat(filepath.Join(outputDir, rel)); statErr == nil {
					t.Errorf("bundle contains %s, which this form must not emit", rel)
				}
			}
			if sc.steps {
				assertGoldenBytes(t, goldenDir, deploymentGolden, deploymentText(out, outputDir))
			}
		})
	}
}

// deploymentText renders the printed steps and notes for a golden, with the
// output directory spelled <bundle> since it is a temp dir here.
func deploymentText(out *deployer.Output, outputDir string) []byte {
	var b strings.Builder
	b.WriteString("steps:\n")
	for _, s := range out.DeploymentSteps {
		b.WriteString("  " + strings.ReplaceAll(s, outputDir, "<bundle>") + "\n")
	}
	b.WriteString("notes:\n")
	for _, n := range out.DeploymentNotes {
		b.WriteString("  " + strings.ReplaceAll(n, outputDir, "<bundle>") + "\n")
	}
	return []byte(b.String())
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

// TestGenerate_StripsHelmOnlyFiles locks the folder contract on the file
// system and in the output record: localformat writes helm's install.sh and
// upstream.env into every folder, and a terraform bundle must ship neither.
// Not clutter — install.sh installs the release names Terraform manages, so a
// second entry point beside the state is a way to drift it. The checksums
// scenario above shows the surviving file list.
func TestGenerate_StripsHelmOnlyFiles(t *testing.T) {
	cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
	g := &Generator{
		RecipeResult:       recipeWith(cm),
		Version:            testBundlerVersion,
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

// fakePuller is a ChartPuller that returns canned bytes, so the vendored
// layout renders without a helm binary.
type fakePuller struct{}

func (*fakePuller) Pull(_ context.Context, c localformat.Component) ([]byte, localformat.VendorRecord, string, error) {
	tarball := c.ChartName + "-" + c.Version + ".tgz"
	return []byte("FAKE TGZ for " + c.Name), localformat.VendorRecord{
		Name:        c.Name,
		Chart:       c.ChartName,
		Version:     c.Version,
		Repository:  c.Repository,
		SHA256:      "deadbeef",
		TarballName: tarball,
	}, tarball, nil
}

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

// assertGolden compares the file at outDir/relPath with goldenDir/relPath.
func assertGolden(t *testing.T, outDir, goldenDir, relPath string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(outDir, relPath))
	if err != nil {
		t.Fatalf("read actual %s: %v", relPath, err)
	}
	assertGoldenBytes(t, goldenDir, relPath, got)
}

// assertGoldenBytes compares got with goldenDir/relPath, rewriting the golden
// under -update.
func assertGoldenBytes(t *testing.T, goldenDir, relPath string, got []byte) {
	t.Helper()
	goldenPath := filepath.Join(goldenDir, relPath)
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
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
