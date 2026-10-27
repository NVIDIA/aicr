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

package localformat_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/NVIDIA/aicr/pkg/k8s/k8stest"
)

const (
	catalogPath = "../../../../docs/user/component-catalog.md"

	// catalogApplyLine is the line the controls rewrite. It must appear in the
	// block exactly once.
	catalogApplyLine = `kubectl apply --server-side --force-conflicts --field-manager="${MANAGER}" -f "${crd}"`

	crdCommandTimeout = 2 * time.Minute

	crdOldVersion  = "1.0.0"
	crdNewVersion  = "2.0.0"
	crdKeepVersion = "v1beta1"
	crdShortName   = "wdg"
	// crdForeignLabel stands in for anything another manager sets on a
	// CRD after install that the chart does not, such as a GitOps label.
	crdForeignLabel = "example.com/added-by"
)

var crdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// crdInstall is how the previous chart's CRDs got onto the cluster, which
// decides the field manager and operation that own them.
type crdInstall int

const (
	// installHelm runs the real helm CLI: manager "helm", operation Update on
	// Helm 3 and Apply on Helm 4.
	installHelm crdInstall = iota
	// installUpdateAs creates the CRD with an Update operation under a named
	// manager: Helm 3's shape, and terraform-provider-helm's.
	installUpdateAs
)

// TestCatalogCRDUpgradeCommand runs the component catalog's manual CRD-upgrade
// block exactly as published, against envtest, after a real `helm install` of
// the previous chart, so the CRD's ownership is whatever the installed helm
// major records. Each control rewrites only the apply line, to a form the
// catalog rules out, and asserts the damage the catalog gives as the reason.
//
// The fixture upgrade drops a version and a schema property, which every
// forced apply removes because spec.versions is atomic, and
// spec.names.shortNames, which only its sole owner, holding it through an
// Apply operation, removes. That second removal is what tells the ownership
// shapes apart.
func TestCatalogCRDUpgradeCommand(t *testing.T) {
	block := catalogCRDBlock(t)
	helmBin := requireTool(t, "helm")
	for _, tool := range []string{"bash", "tar", "find", "sort", "grep", "mktemp"} {
		requireTool(t, tool)
	}
	cluster := startCRDCluster(t)
	repo := newCRDChartRepo(t, helmBin)

	tests := []struct {
		name      string
		install   crdInstall
		installer string
		// coOwner, when set, applies the same shortNames value under its own
		// manager after install, so it shares ownership of the field.
		coOwner string
		manager string
		// apply replaces catalogApplyLine; empty runs the block as published.
		apply string
		// needsApply marks a case whose expectation holds only when helm
		// installed the CRD through an Apply operation, as Helm 4 does.
		needsApply bool
		// wantConflict marks a case whose apply must fail on a field
		// conflict and leave the installed CRD unchanged.
		wantConflict   bool
		wantShortNames bool
		wantLabel      bool
	}{
		{
			name:       "as published after a Helm 4 install",
			install:    installHelm,
			manager:    "helm",
			needsApply: true,
			wantLabel:  true,
		},
		{
			name:           "as published when another manager shares the field",
			install:        installHelm,
			coOwner:        "gitops",
			manager:        "helm",
			needsApply:     true,
			wantShortNames: true,
			wantLabel:      true,
		},
		{
			name:           "as published over Helm 3 ownership",
			install:        installUpdateAs,
			installer:      "helm",
			manager:        "helm",
			wantShortNames: true,
			wantLabel:      true,
		},
		{
			name:           "as published over terraform ownership",
			install:        installUpdateAs,
			installer:      "terraform-provider-helm",
			manager:        "terraform-provider-helm",
			wantShortNames: true,
			wantLabel:      true,
		},
		{
			name:    "control: create or replace discards other managers' fields",
			install: installHelm,
			manager: "helm",
			apply:   `{ kubectl create -f "${crd}" 2>/dev/null || kubectl replace -f "${crd}"; }`,
		},
		{
			name:           "control: apply without the field manager keeps a removed field",
			install:        installHelm,
			manager:        "helm",
			apply:          `kubectl apply --server-side --force-conflicts -f "${crd}"`,
			wantShortNames: true,
			wantLabel:      true,
		},
		{
			name:         "control: apply without forcing conflicts on helm's spec.versions",
			install:      installHelm,
			manager:      "helm",
			apply:        `kubectl apply --server-side -f "${crd}"`,
			needsApply:   true,
			wantConflict: true,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A chart and CRD group per case, so each one starts from its own
			// freshly installed CRD on the shared API server.
			chart := fmt.Sprintf("crd-fixture-%d", i)
			group := chart + ".aicr.test"
			crd := "widgets." + group
			repo.add(t, chart, group)

			switch tt.install {
			case installHelm:
				cluster.run(t, helmBin, "install", chart, chart,
					"--repo", repo.url, "--version", crdOldVersion, "--namespace", "default")
			case installUpdateAs:
				path := filepath.Join(t.TempDir(), "crd.yaml")
				writeFile(t, path, fixtureCRD(group, false))
				cluster.run(t, cluster.kubectl, "create", "--field-manager="+tt.installer, "-f", path)
			}
			installed := getCRDState(t, cluster.client, crd)
			if !slices.Equal(installed.versions, []string{"v1alpha1", crdKeepVersion}) {
				t.Fatalf("installed CRD has versions %v, want the old chart's", installed.versions)
			}
			if tt.needsApply && installed.helmOperation != metav1.ManagedFieldsOperationApply {
				msg := fmt.Sprintf("helm installed the CRD with operation %q, want Apply (Helm 4)", installed.helmOperation)
				if os.Getenv("CI") != "" {
					t.Fatal(msg)
				}
				t.Skip(msg)
			}
			cluster.run(t, cluster.kubectl, "label", "crd", crd, crdForeignLabel+"=other")
			if tt.coOwner != "" {
				path := filepath.Join(t.TempDir(), "shortnames.yaml")
				writeFile(t, path, fmt.Sprintf(
					"apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: %s\nspec:\n  names:\n    shortNames: [%s]\n",
					crd, crdShortName))
				cluster.run(t, cluster.kubectl, "apply", "--server-side", "--field-manager="+tt.coOwner, "-f", path)
			}

			script := pinCatalogBlock(t, block, map[string]string{
				"CHART":   chart,
				"VERSION": crdNewVersion,
				"REPO":    repo.url,
				"MANAGER": tt.manager,
			})
			if tt.apply != "" {
				script = replaceOnce(t, script, catalogApplyLine, tt.apply)
			}
			out, err := cluster.bash(t, script)
			if tt.wantConflict {
				if err == nil || !strings.Contains(out, "conflict") {
					t.Fatalf("want the apply to fail on a conflict, got err=%v\n%s", err, out)
				}
				if got := getCRDState(t, cluster.client, crd); !slices.Equal(got.versions, installed.versions) {
					t.Errorf("spec.versions = %v after a failed apply, want unchanged %v", got.versions, installed.versions)
				}
				return
			}
			if err != nil {
				t.Fatalf("catalog block failed: %v\n%s", err, out)
			}

			got := getCRDState(t, cluster.client, crd)
			if !slices.Equal(got.versions, []string{crdKeepVersion}) {
				t.Errorf("spec.versions = %v, want only %s", got.versions, crdKeepVersion)
			}
			if !slices.Equal(got.fields, []string{"added", "kept"}) {
				t.Errorf("%s spec properties = %v, want [added kept]", crdKeepVersion, got.fields)
			}
			if got.shortNames != tt.wantShortNames {
				t.Errorf("spec.names.shortNames present = %t, want %t", got.shortNames, tt.wantShortNames)
			}
			if got.label != tt.wantLabel {
				t.Errorf("label %s present = %t, want %t", crdForeignLabel, got.label, tt.wantLabel)
			}
		})
	}
}

// catalogCRDBlock returns the one bash block in the catalog that applies CRDs
// under a field manager.
func catalogCRDBlock(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var found []string
	for _, m := range regexp.MustCompile("(?s)```bash\n(.*?)```").FindAllStringSubmatch(string(b), -1) {
		if strings.Contains(m[1], catalogApplyLine) {
			found = append(found, m[1])
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one bash block in %s containing %q, found %d", catalogPath, catalogApplyLine, len(found))
	}
	return found[0]
}

// pinCatalogBlock replaces each NAME="..." assignment the reader is told to
// edit. Each must appear exactly once at the start of a line, so a renamed
// variable fails here rather than running against the documented default.
func pinCatalogBlock(t *testing.T, block string, vars map[string]string) string {
	t.Helper()
	for name, value := range vars {
		re := regexp.MustCompile(`(?m)^` + name + `=.*$`)
		if n := len(re.FindAllStringIndex(block, -1)); n != 1 {
			t.Fatalf("want exactly one %s= assignment in the catalog block, found %d", name, n)
		}
		block = re.ReplaceAllLiteralString(block, fmt.Sprintf("%s=%q", name, value))
	}
	return block
}

func replaceOnce(t *testing.T, s, old, repl string) string {
	t.Helper()
	if n := strings.Count(s, old); n != 1 {
		t.Fatalf("want exactly one %q in the catalog block, found %d", old, n)
	}
	return strings.Replace(s, old, repl, 1)
}

// requireTool fails in CI, where the go-test action installs every tool this
// test needs, and skips elsewhere.
func requireTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is required in CI but not on PATH", name)
		}
		t.Skipf("%s not available", name)
	}
	return path
}

type crdCluster struct {
	client  dynamic.Interface
	kubectl string
	env     []string
}

// startCRDCluster starts an envtest API server and returns what a shell
// needs to reach it: a kubeconfig, envtest's own kubectl first on PATH, and
// helm state isolated from the developer's.
func startCRDCluster(t *testing.T) *crdCluster {
	t.Helper()
	env, cfg := k8stest.Start(t)

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	shell := k8stest.ShellEnv(t, env, cfg, envtest.User{Name: "crd-upgrader", Groups: []string{"system:masters"}})

	return &crdCluster{
		client:  client,
		kubectl: env.ControlPlane.KubectlPath,
		env: append(append(os.Environ(), shell...),
			"TMPDIR="+t.TempDir(),
			"HELM_CACHE_HOME="+t.TempDir(),
			"HELM_CONFIG_HOME="+t.TempDir(),
			"HELM_DATA_HOME="+t.TempDir(),
		),
	}
}

func (c *crdCluster) output(t *testing.T, name string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), crdCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = c.env
	return cmd.CombinedOutput()
}

func (c *crdCluster) run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := c.output(t, name, args...); err != nil {
		t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, out)
	}
}

func (c *crdCluster) bash(t *testing.T, script string) (string, error) {
	t.Helper()
	out, err := c.output(t, "bash", "-c", script)
	return string(out), err
}

// crdChartRepo is a classic chart repository on a local HTTP server, so the
// documented `helm pull --repo` runs unchanged.
type crdChartRepo struct {
	dir  string
	url  string
	helm string
}

func newCRDChartRepo(t *testing.T, helmBin string) *crdChartRepo {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return &crdChartRepo{dir: dir, url: srv.URL + "/", helm: helmBin}
}

// add publishes the old and new version of a chart whose only content is
// the fixture CRD under crds/, where Helm installs it from.
func (r *crdChartRepo) add(t *testing.T, chart, group string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), crdCommandTimeout)
	defer cancel()
	for _, v := range []struct {
		version  string
		upgraded bool
	}{{crdOldVersion, false}, {crdNewVersion, true}} {
		src := filepath.Join(t.TempDir(), chart)
		writeFile(t, filepath.Join(src, "Chart.yaml"),
			fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\n", chart, v.version))
		writeFile(t, filepath.Join(src, "crds", "crd.yaml"), fixtureCRD(group, v.upgraded))
		if out, err := exec.CommandContext(ctx, r.helm, "package", src, "--destination", r.dir).CombinedOutput(); err != nil {
			t.Fatalf("helm package %s %s: %v\n%s", chart, v.version, err, out)
		}
	}
	if out, err := exec.CommandContext(ctx, r.helm, "repo", "index", r.dir).CombinedOutput(); err != nil {
		t.Fatalf("helm repo index: %v\n%s", err, out)
	}
}

// fixtureCRD is the old chart's CRD or the new one's. The new one drops the
// v1alpha1 version, the "dropped" property, and spec.names.shortNames, and
// adds an "added" property. v1alpha1 is never the storage version, so
// removing it passes the API server's storedVersions check.
func fixtureCRD(group string, upgraded bool) string {
	shortNames := "    shortNames: [" + crdShortName + "]\n"
	versions := `  - name: v1alpha1
    served: true
    storage: false
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              kept: {type: string}
  - name: v1beta1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              kept: {type: string}
              dropped: {type: string}
`
	if upgraded {
		shortNames = ""
		versions = `  - name: v1beta1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              kept: {type: string}
              added: {type: string}
`
	}
	return fmt.Sprintf(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.%s
spec:
  group: %s
  scope: Namespaced
  names:
    kind: Widget
    listKind: WidgetList
    plural: widgets
    singular: widget
%s  versions:
%s`, group, group, shortNames, versions)
}

type crdState struct {
	versions      []string
	fields        []string
	shortNames    bool
	label         bool
	helmOperation metav1.ManagedFieldsOperationType
}

func getCRDState(t *testing.T, client dynamic.Interface, name string) crdState {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), crdCommandTimeout)
	defer cancel()
	obj, err := client.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CRD %s: %v", name, err)
	}
	var s crdState
	_, s.shortNames, _ = unstructured.NestedStringSlice(obj.Object, "spec", "names", "shortNames")
	_, s.label = obj.GetLabels()[crdForeignLabel]
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager == "helm" {
			s.helmOperation = mf.Operation
		}
	}
	versions, _, err := unstructured.NestedSlice(obj.Object, "spec", "versions")
	if err != nil {
		t.Fatalf("read spec.versions: %v", err)
	}
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("spec.versions entry is %T", v)
		}
		version, _, _ := unstructured.NestedString(m, "name")
		s.versions = append(s.versions, version)
		if version != crdKeepVersion {
			continue
		}
		props, _, err := unstructured.NestedMap(m, "schema", "openAPIV3Schema", "properties", "spec", "properties")
		if err != nil {
			t.Fatalf("read %s schema: %v", version, err)
		}
		for f := range props {
			s.fields = append(s.fields, f)
		}
		slices.Sort(s.fields)
	}
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
