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

package skeleton

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/plugin"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// renderTimeout bounds one AICR CLI render.
const renderTimeout = 5 * time.Minute

// reproduceMatches renders a plan's bundle again with the AICR CLI, exactly
// as `kubectl aicr reproduce` tells a user to, and requires zero diff with
// the bundle the operator's render pushed. Each release's plan values, and
// with applications each of the plan's Argo CD Applications, must equal that
// release's values.yaml merged with cluster-values.yaml from the CLI bundle,
// as AICR's deploy.sh passes them to Helm.
func reproduceMatches(ctx context.Context, t *testing.T, digest string, applications bool) {
	t.Helper()
	r, err := plugin.LoadReproduction(ctx, k8s, operatorNamespace, digest)
	if err != nil {
		t.Fatalf("load the reproduction: %v", err)
	}
	dir := renderDir(t)
	if err = os.WriteFile(filepath.Join(dir, plugin.RecipeFile), r.Recipe, 0o600); err != nil {
		t.Fatal(err)
	}
	renderCtx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()
	args := append([]string{"run", "--rm", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}, r.ContainerArgs(dir)...)
	if out, runErr := exec.CommandContext(renderCtx, "docker", args...).CombinedOutput(); runErr != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), runErr, out)
	}
	cli := readTree(t, filepath.Join(dir, plugin.CLIBundle))
	operator := pullBundle(ctx, t, r.BundleRef())
	if diff := treeDiff(operator, cli); len(diff) > 0 {
		t.Fatalf("the operator's bundle %s differs from the AICR CLI's render in %d files: %s",
			r.Bundle, len(diff), strings.Join(diff[:min(len(diff), 10)], ", "))
	}

	want := releaseValues(t, cli)
	doc := readPlan(ctx, t, digest)
	for _, rel := range doc.Releases {
		cm := &corev1.ConfigMap{}
		if err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: rel.Values.ConfigMap}, cm); err != nil {
			t.Fatal(err)
		}
		raw, readErr := plan.ReadContent(cm, plan.ValuesDataKey)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if got := decodeJSON(t, raw); !reflect.DeepEqual(got, want[rel.ID]) {
			t.Errorf("plan values of %s differ from the CLI bundle's merged values", rel.ID)
		}
	}
	t.Logf("plan %s: zero diff over %d bundle files; %d releases' values match", digest, len(cli), len(doc.Releases))
	if !applications {
		return
	}
	apps := &unstructured.UnstructuredList{}
	apps.SetGroupVersionKind(argocd.ApplicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err = k8s.List(ctx, apps, client.InNamespace("argocd"), client.MatchingLabels{v1alpha1.LabelRuntime: v1alpha1.SingletonName}); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.GetAnnotations()[v1alpha1.AnnotationPlanDigest] != digest {
			continue
		}
		values, _, _ := unstructured.NestedMap(app.Object, "spec", "source", "helm", "valuesObject")
		if values == nil {
			values = map[string]any{}
		}
		if got := decodeJSON(t, encodeJSON(t, values)); !reflect.DeepEqual(got, want[app.GetName()]) {
			t.Errorf("Application %s values differ from the CLI bundle's merged values", app.GetName())
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no Application is at plan %s", digest)
	}
}

// renderDir is a directory for one CLI render under .operator-e2e in the
// repository, which Docker Desktop shares with its VM; macOS's temporary
// directory is not shared.
func renderDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(filepath.Join("..", "..", "..", ".operator-e2e"), "reproduce-")
	if err == nil {
		dir, err = filepath.Abs(dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// pullBundle fetches a bundle the operator's render pushed to the in-cluster
// registry and unpacks its one layer.
func pullBundle(ctx context.Context, t *testing.T, ref string) map[string][]byte {
	t.Helper()
	repoPath, dgst, ok := strings.Cut(strings.TrimPrefix(ref, registryHost+"/"), "@")
	if !ok {
		t.Fatalf("bundle %s is not a digest reference in the runtime registry", ref)
	}
	creds, err := registry.DockerConfigCredentials(registryAuth)
	if err != nil {
		t.Fatal(err)
	}
	var files map[string][]byte
	withRegistry(ctx, t, func(base string) {
		repo, repoErr := remote.NewRepository(strings.TrimPrefix(base, "http://") + "/" + repoPath)
		if repoErr != nil {
			t.Fatal(repoErr)
		}
		repo.PlainHTTP = true
		repo.Client = &auth.Client{Credential: func(ctx context.Context, _ string) (auth.Credential, error) {
			return creds(ctx, registryHost)
		}}
		_, raw, fetchErr := oras.FetchBytes(ctx, repo, dgst, oras.DefaultFetchBytesOptions)
		if fetchErr != nil {
			t.Fatalf("fetch %s: %v", ref, fetchErr)
		}
		var manifest ocispec.Manifest
		if fetchErr = json.Unmarshal(raw, &manifest); fetchErr != nil || len(manifest.Layers) != 1 {
			t.Fatalf("bundle manifest %s: %v, %d layers", ref, fetchErr, len(manifest.Layers))
		}
		layer, fetchErr := content.FetchAll(ctx, repo, manifest.Layers[0])
		if fetchErr != nil {
			t.Fatalf("fetch the bundle layer: %v", fetchErr)
		}
		files = untarGz(t, layer)
	})
	return files
}

func untarGz(t *testing.T, layer []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[path.Clean(strings.TrimPrefix(h.Name, "./"))] = data
	}
}

func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || !d.Type().IsRegular() {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// treeDiff lists the paths whose content differs or that only one tree has.
func treeDiff(a, b map[string][]byte) []string {
	var diff []string
	for p, data := range a {
		if other, ok := b[p]; !ok || !bytes.Equal(data, other) {
			diff = append(diff, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			diff = append(diff, p)
		}
	}
	sort.Strings(diff)
	return diff
}

// releaseValues merges each release's values files from a bundle as Helm
// merges `-f values.yaml -f cluster-values.yaml`: maps key by key, anything
// else replaced. Releases are named by bundle-info.yaml's layout.
func releaseValues(t *testing.T, files map[string][]byte) map[string]any {
	t.Helper()
	var info struct {
		Layout struct {
			Releases []struct{ Name, Path string } `yaml:"releases"`
		} `yaml:"layout"`
	}
	if err := yaml.Unmarshal(files["bundle-info.yaml"], &info); err != nil || len(info.Layout.Releases) == 0 {
		t.Fatalf("bundle-info.yaml: %v, %d releases", err, len(info.Layout.Releases))
	}
	out := map[string]any{}
	for _, rel := range info.Layout.Releases {
		merged := map[string]any{}
		for _, name := range []string{"values.yaml", "cluster-values.yaml"} {
			raw, ok := files[rel.Path+"/"+name]
			if !ok {
				continue
			}
			doc := map[string]any{}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("%s/%s: %v", rel.Path, name, err)
			}
			merged = helmMerge(merged, doc)
		}
		out[rel.Name] = decodeJSON(t, encodeJSON(t, merged))
	}
	return out
}

func helmMerge(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = helmMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

func encodeJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
