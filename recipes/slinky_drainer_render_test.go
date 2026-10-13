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

package recipes

import (
	stderrors "errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/manifest"
)

// slinkyDrainerManifests is the plugin with its CRD, which ships as the
// separate slinky-drainer-crds component.
var slinkyDrainerManifests = []string{
	"components/slinky-drainer-crds/manifests/drainrequest-crd.yaml",
	"components/slinky-drainer/manifests/drain-template.yaml",
	"components/slinky-drainer/manifests/slinky-drainer.yaml",
}

// renderSlinkyDrainer renders the component's manifests the way the bundler's
// first pass does, with values.yaml overlaid by extra, and decodes them.
func renderSlinkyDrainer(t *testing.T, extra map[string]any) (string, []map[string]any) {
	t.Helper()
	raw, err := fs.ReadFile(FS, "components/slinky-drainer/values.yaml")
	if err != nil {
		t.Fatalf("read values: %v", err)
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse values: %v", err)
	}
	for k, v := range extra {
		values[k] = v
	}

	var out strings.Builder
	for _, path := range slinkyDrainerManifests {
		content, err := fs.ReadFile(FS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rendered, err := manifest.Render(content, manifest.RenderInput{
			ComponentName: "slinky-drainer",
			Namespace:     "nvsentinel",
			ChartName:     "slinky-drainer",
			ChartVersion:  "0.0.0",
			Values:        values,
		})
		if err != nil {
			t.Fatalf("render %s: %v", path, err)
		}
		out.Write(rendered)
		out.WriteString("\n")
	}

	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(out.String()))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode rendered manifests: %v", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return out.String(), docs
}

func slinkyDrainerObject(t *testing.T, docs []map[string]any, kind string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, doc := range docs {
		if doc["kind"] == kind {
			if found != nil {
				t.Fatalf("rendered two %s objects", kind)
			}
			found = doc
		}
	}
	if found == nil {
		t.Fatalf("no %s rendered", kind)
	}
	return found
}

func slinkyDrainerPodSpec(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	deployment := slinkyDrainerObject(t, docs, "Deployment")
	spec, _ := deployment["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	return podSpec
}

// TestSlinkyDrainerManifestsRender renders the vendored plugin with its
// shipped values and checks what the differences from upstream promise: every
// namespaced object in the release namespace, the plugin watching
// slinky-slurm's worker namespace, no template action left for the
// localformat deployers' second Helm pass, and pod RBAC that can delete but
// not evict or create.
func TestSlinkyDrainerManifestsRender(t *testing.T) {
	out, docs := renderSlinkyDrainer(t, nil)
	if strings.Contains(out, "{{") {
		t.Error("rendered manifests contain template actions; Helm would render them again under localformat deployers")
	}

	kinds := make([]string, 0, len(docs))
	for _, doc := range docs {
		kind, _ := doc["kind"].(string)
		kinds = append(kinds, kind)
		meta, _ := doc["metadata"].(map[string]any)
		switch kind {
		case "CustomResourceDefinition", "ClusterRole", "ClusterRoleBinding":
			if ns, set := meta["namespace"]; set {
				t.Errorf("cluster-scoped %s has namespace %v", kind, ns)
			}
		default:
			if meta["namespace"] != "nvsentinel" {
				t.Errorf("%s %v in namespace %v, want the release namespace nvsentinel", kind, meta["name"], meta["namespace"])
			}
		}
	}
	slices.Sort(kinds)
	want := []string{"ClusterRole", "ClusterRoleBinding", "ConfigMap", "CustomResourceDefinition", "Deployment", "ServiceAccount"}
	if !slices.Equal(kinds, want) {
		t.Errorf("rendered kinds = %v, want %v", kinds, want)
	}

	binding := slinkyDrainerObject(t, docs, "ClusterRoleBinding")
	subjects, _ := binding["subjects"].([]any)
	if len(subjects) != 1 {
		t.Fatalf("ClusterRoleBinding has %d subjects, want 1", len(subjects))
	}
	if subject, _ := subjects[0].(map[string]any); subject["namespace"] != "nvsentinel" || subject["name"] != "slinky-drainer" {
		t.Errorf("ClusterRoleBinding subject = %v, want nvsentinel/slinky-drainer", subject)
	}

	podSpec := slinkyDrainerPodSpec(t, docs)
	containers, _ := podSpec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("Deployment has %d containers, want 1", len(containers))
	}
	container, _ := containers[0].(map[string]any)
	var namespaceArgs []string
	args, _ := container["args"].([]any)
	if len(args) == 0 {
		t.Fatal("slinky-drainer container has no args")
	}
	for _, a := range args {
		if s, _ := a.(string); strings.HasPrefix(s, "--slinky-namespace=") {
			namespaceArgs = append(namespaceArgs, s)
		}
	}
	if !slices.Equal(namespaceArgs, []string{"--slinky-namespace=slurm"}) {
		t.Errorf("--slinky-namespace args = %v, want exactly --slinky-namespace=slurm", namespaceArgs)
	}
	for _, key := range []string{"nodeSelector", "tolerations"} {
		if v, set := podSpec[key]; set {
			t.Errorf("pod spec carries %s %v with no system scheduling values", key, v)
		}
	}

	role := slinkyDrainerObject(t, docs, "ClusterRole")
	rules, _ := role["rules"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		resources, _ := rule["resources"].([]any)
		for _, res := range resources {
			if res == "pods/eviction" || res == "secrets" {
				t.Errorf("ClusterRole grants %s", res)
			}
			if res != "pods" {
				continue
			}
			var verbs []string
			ruleVerbs, _ := rule["verbs"].([]any)
			for _, v := range ruleVerbs {
				s, _ := v.(string)
				verbs = append(verbs, s)
			}
			slices.Sort(verbs)
			if !slices.Equal(verbs, []string{"delete", "get", "list", "watch"}) {
				t.Errorf("ClusterRole pod verbs = %v, want exactly [delete get list watch]", verbs)
			}
		}
	}
}

// TestSlinkyDrainerManifestsRenderSystemScheduling checks the values the
// registry's nodeScheduling.system paths inject reach the pod spec.
func TestSlinkyDrainerManifestsRenderSystemScheduling(t *testing.T) {
	_, docs := renderSlinkyDrainer(t, map[string]any{
		"systemNodeSelector": map[string]any{"nodeGroup": "system"},
		"systemTolerations": []any{
			map[string]any{"key": "dedicated", "operator": "Equal", "value": "system", "effect": "NoSchedule"},
		},
	})
	podSpec := slinkyDrainerPodSpec(t, docs)
	if selector, _ := podSpec["nodeSelector"].(map[string]any); selector["nodeGroup"] != "system" {
		t.Errorf("nodeSelector = %v, want nodeGroup: system", podSpec["nodeSelector"])
	}
	tolerations, _ := podSpec["tolerations"].([]any)
	if len(tolerations) != 1 {
		t.Fatalf("tolerations = %v, want the one injected", podSpec["tolerations"])
	}
	if tol, _ := tolerations[0].(map[string]any); tol["key"] != "dedicated" || tol["effect"] != "NoSchedule" {
		t.Errorf("toleration = %v, want dedicated/NoSchedule", tol)
	}
}
