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

package resolver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

const uid = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"

func spec() *v1alpha1.ClusterRuntimeSpec {
	return &v1alpha1.ClusterRuntimeSpec{
		Intent: "training", Platform: "kubeflow",
		Criteria: &v1alpha1.DeclaredCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu"},
		Placement: v1alpha1.Placement{
			System: v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{"pool": "system", "a": "b"}},
			Accelerated: v1alpha1.NodeGroupPlacement{
				NodeSelector: map[string]v1alpha1.LabelValue{"pool": "gpu"},
				Tolerations: []corev1.Toleration{
					{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
					{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoExecute},
					{Operator: corev1.TolerationOpExists},
				},
			},
		},
		Overrides: []v1alpha1.Override{
			{Component: "gpu-operator", Path: "dcgmExporter.enabled", Value: apiextensionsv1.JSON{Raw: []byte(` false `)}},
			{Component: "nvsentinel", Path: "enabled", Value: apiextensionsv1.JSON{Raw: []byte(`false`)}},
			{Component: "gpu-operator", Path: "driver.env", Value: apiextensionsv1.JSON{Raw: []byte(`[ {"name": "A"} ]`)}},
		},
	}
}

func TestRecipeArgs(t *testing.T) {
	t.Parallel()
	s := spec()
	s.Profile = "gpu=dense"
	got := RecipeArgs(s, map[string]string{"service": "kind", "os": "amazonlinux"}, "cm://aicr-jobs/aicr-recipe-r1-snapshot", "cm://aicr-jobs/out")
	want := []string{"--snapshot", "cm://aicr-jobs/aicr-recipe-r1-snapshot", "--intent", "training", "--platform", "kubeflow",
		"--profile", "gpu=dense", "--service", "eks", "--accelerator", "h100", "--os", "ubuntu", "--format", "json",
		"--output", "cm://aicr-jobs/out"}
	if !slices.Equal(got, want) {
		t.Fatalf("RecipeArgs() = %v\nwant %v", got, want)
	}
	if got := RecipeArgs(&v1alpha1.ClusterRuntimeSpec{Intent: "inference"}, nil, "", "cm://x/y"); len(got) != 6 {
		t.Fatalf("minimal RecipeArgs() = %v", got)
	}
	// Without declared criteria, the snapshot's dimensions are stated.
	derived := RecipeArgs(&v1alpha1.ClusterRuntimeSpec{Intent: "training", Criteria: &v1alpha1.DeclaredCriteria{Accelerator: "h100"}},
		map[string]string{"service": "eks", "accelerator": "", "os": "amazonlinux"}, "cm://s", "cm://x/y")
	if want := "--snapshot cm://s --intent training --service eks --accelerator h100 --os amazonlinux"; !strings.HasPrefix(strings.Join(derived, " "), want) {
		t.Fatalf("derived RecipeArgs() = %v", derived)
	}
	// The TCPXO mapping reaches AICR in the administrator's pair order.
	gke := &v1alpha1.ClusterRuntimeSpec{Intent: "training", Configuration: &v1alpha1.RecipeConfiguration{
		GKE: &v1alpha1.GKEConfiguration{TCPXOInterfaces: []v1alpha1.NetworkInterfaceMapping{
			{InterfaceName: "eth2", Network: "c-gpu-nic-1"}, {InterfaceName: "eth1", Network: "c-gpu-nic-0"},
		}},
	}}
	if got, want := strings.Join(RecipeArgs(gke, nil, "", "cm://x/y"), " "),
		"--intent training --gke-tcpxo-interfaces eth2=c-gpu-nic-1,eth1=c-gpu-nic-0 --format json --output cm://x/y"; got != want {
		t.Fatalf("GKE RecipeArgs() = %s\nwant %s", got, want)
	}
	if got := RecipeArgs(&v1alpha1.ClusterRuntimeSpec{Intent: "training", Configuration: &v1alpha1.RecipeConfiguration{}}, nil, "", "cm://x/y"); slices.Contains(got, "--gke-tcpxo-interfaces") {
		t.Fatalf("empty configuration RecipeArgs() = %v", got)
	}
}

func TestRenderArgs(t *testing.T) {
	t.Parallel()
	got, err := RenderArgs(spec(), "cm://aicr-jobs/r", "oci://reg/bundles/cluster:run-x", true)
	if err != nil {
		t.Fatalf("RenderArgs() = %v", err)
	}
	want := []string{"--recipe", "cm://aicr-jobs/r", "--deployer", "helm", "--readiness-hooks",
		"--system-node-selector", "a=b", "--system-node-selector", "pool=system",
		"--accelerated-node-selector", "pool=gpu",
		"--accelerated-node-toleration", "nvidia.com/gpu:NoSchedule",
		"--accelerated-node-toleration", "dedicated=gpu:NoExecute",
		"--accelerated-node-toleration", "*",
		"--set-json", "gpu-operator:dcgmExporter.enabled=false",
		"--set", "nvsentinel:enabled=false",
		"--set-json", `gpu-operator:driver.env=[{"name":"A"}]`,
		"--output", "oci://reg/bundles/cluster:run-x", "--plain-http"}
	if !slices.Equal(got, want) {
		t.Fatalf("RenderArgs() = %v\nwant %v", got, want)
	}
	// The plan records the flags between the recipe and the output.
	flags, err := RenderFlags(spec())
	if err != nil || !slices.Equal(flags, want[2:len(want)-3]) {
		t.Fatalf("RenderFlags() = %v, %v\nwant %v", flags, err, want[2:len(want)-3])
	}
}

func TestSnapshotArgs(t *testing.T) {
	t.Parallel()
	args, err := SnapshotArgs(&spec().Placement, "aicr-discovery", "aicr-snapshot-agent", "ghcr.io/nvidia/aicr@sha256:x", "cpu=250m",
		"cm://aicr-discovery/aicr-snapshot-r1")
	if err != nil {
		t.Fatal(err)
	}
	want := "--namespace aicr-discovery --service-account-name aicr-snapshot-agent --image ghcr.io/nvidia/aicr@sha256:x " +
		"--node-selector pool=gpu --toleration nvidia.com/gpu:NoSchedule --toleration dedicated=gpu:NoExecute --toleration * " +
		"--requests cpu=250m --timeout 10m0s --format json --output cm://aicr-discovery/aicr-snapshot-r1"
	if got := strings.Join(args, " "); got != want {
		t.Fatalf("SnapshotArgs() =\n%s\nwant\n%s", got, want)
	}
	bad := spec()
	bad.Placement.Accelerated.Tolerations = []corev1.Toleration{{Key: "k", Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(5))}}
	if _, err = SnapshotArgs(&bad.Placement, "ns", "sa", "img", "", "cm://ns/x"); operrors.ReasonOf(err) != ReasonInvalidPlacement {
		t.Fatalf("SnapshotArgs(tolerationSeconds) = %v", err)
	}
}

func TestRenderArgsRejects(t *testing.T) {
	t.Parallel()
	tolerations := map[string]corev1.Toleration{
		"seconds":           {Key: "k", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To[int64](5)},
		"no effect":         {Key: "k", Operator: corev1.TolerationOpExists},
		"exists with value": {Key: "k", Value: "v", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		"equal no value":    {Key: "k", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule},
	}
	for name, tol := range tolerations {
		s := spec()
		s.Placement.System.Tolerations = []corev1.Toleration{tol}
		_, err := RenderArgs(s, "r", "o", false)
		if operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest || !strings.Contains(err.Error(), "placement.system") {
			t.Errorf("%s: RenderArgs() = %v", name, err)
		}
	}
	overrides := map[string]v1alpha1.Override{
		"not JSON":       {Component: "c", Path: "p", Value: apiextensionsv1.JSON{Raw: []byte(`{`)}},
		"empty":          {Component: "c", Path: "p"},
		"too large":      {Component: "c", Path: "p", Value: apiextensionsv1.JSON{Raw: []byte(`"` + strings.Repeat("x", MaxOverrideValueBytes) + `"`)}},
		"toggle string":  {Component: "c", Path: "enabled", Value: apiextensionsv1.JSON{Raw: []byte(`"no"`)}},
		"toggle missing": {Component: "c", Path: "enabled", Value: apiextensionsv1.JSON{Raw: []byte(`null`)}},
	}
	for name, o := range overrides {
		s := spec()
		s.Overrides = []v1alpha1.Override{o}
		if _, err := RenderArgs(s, "r", "o", false); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("%s: RenderArgs() = %v, want INVALID_REQUEST", name, err)
		}
	}
}

func TestInputsDigest(t *testing.T) {
	t.Parallel()
	rt := &v1alpha1.ClusterRuntime{Spec: *spec()}
	rt.UID = uid
	in := NewInputs(rt, "img@sha256:1", "v1.0.0", "dev", plan.EngineArgoCD, "argocd.v1", "oci://reg/x")
	d1, err := in.Digest()
	if err != nil || len(ShortDigest(d1)) != 16 {
		t.Fatalf("Digest() = %s, %v", d1, err)
	}
	rt.Spec.Overrides[0].Value = apiextensionsv1.JSON{Raw: []byte(`true`)}
	d2, _ := NewInputs(rt, "img@sha256:1", "v1.0.0", "dev", plan.EngineArgoCD, "argocd.v1", "oci://reg/x").Digest()
	if d1 == d2 {
		t.Fatal("an override change kept the inputDigest")
	}
	d3, _ := NewInputs(rt, "img@sha256:2", "v1.0.0", "dev", plan.EngineArgoCD, "argocd.v1", "oci://reg/x").Digest()
	if d3 == d2 {
		t.Fatal("an AICR image change kept the inputDigest")
	}
	// An unset configuration is absent from the inputs, so runtimes from
	// before the field keep their inputDigest; setting one resolves again.
	if data, _ := json.Marshal(NewInputs(rt, "img@sha256:2", "v1.0.0", "dev", plan.EngineArgoCD, "argocd.v1", "oci://reg/x")); strings.Contains(string(data), "configuration") {
		t.Fatalf("unset configuration encoded: %s", data)
	}
	rt.Spec.Configuration = &v1alpha1.RecipeConfiguration{GKE: &v1alpha1.GKEConfiguration{
		TCPXOInterfaces: []v1alpha1.NetworkInterfaceMapping{{InterfaceName: "eth1", Network: "c-gpu-nic-0"}},
	}}
	if d4, _ := NewInputs(rt, "img@sha256:2", "v1.0.0", "dev", plan.EngineArgoCD, "argocd.v1", "oci://reg/x").Digest(); d4 == d3 {
		t.Fatal("a configuration change kept the inputDigest")
	}
}

func TestReleaseValuesMergeLikeHelm(t *testing.T) {
	t.Parallel()
	got, err := releaseValues(map[string][]byte{
		"values.yaml":         []byte("# Generated by AICR\n---\na:\n  b: 1\n  c: [1, 2]\nkeep: x\ndrop: y\n"),
		"cluster-values.yaml": []byte("---\na:\n  b: 2\n  c: [3]\ndrop: null\nnew: {n: true}\n"),
	})
	if err != nil {
		t.Fatalf("releaseValues() = %v", err)
	}
	want := `{"a":{"b":2,"c":[3]},"drop":null,"keep":"x","new":{"n":true}}`
	if string(got) != want {
		t.Fatalf("releaseValues() = %s\nwant %s", got, want)
	}
	empty, err := releaseValues(map[string][]byte{"cluster-values.yaml": []byte("# Generated by AICR\n---\n")})
	if err != nil || string(empty) != "{}" {
		t.Fatalf("empty values = %s, %v", empty, err)
	}
	for _, bad := range []string{"a: [1", "a: 1\n---\nb: 2\n", "- list"} {
		if _, err := releaseValues(map[string][]byte{"values.yaml": []byte(bad)}); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("releaseValues(%q) = %v, want INVALID_REQUEST", bad, err)
		}
	}
}

func TestParseUpstreamEnv(t *testing.T) {
	t.Parallel()
	up, err := parseUpstreamEnv([]byte("CHART='cert-manager'\nREPO='https://charts.jetstack.io'\nVERSION='v1.20.2'\n\n# Generated by AICR\n"))
	if err != nil || up.Chart != "cert-manager" || up.Repository != "https://charts.jetstack.io" || up.Version != "v1.20.2" {
		t.Fatalf("parseUpstreamEnv() = %+v, %v", up, err)
	}
	oci, err := parseUpstreamEnv([]byte("CHART=oci://ghcr.io/x/chart\nREPO=''\nVERSION='1.0'\n"))
	if err != nil || oci.Repository != "" || oci.Chart != "oci://ghcr.io/x/chart" {
		t.Fatalf("OCI upstream = %+v, %v", oci, err)
	}
	for _, bad := range []string{"CHART=$(id)\nVERSION=1", "CHART='a'\"'\"'b'\nVERSION=1", "VERSION='1'", "garbage", "CHART=a b\nVERSION=1"} {
		if _, err := parseUpstreamEnv([]byte(bad)); err == nil {
			t.Errorf("parseUpstreamEnv(%q) accepted", bad)
		}
	}
}

func chartArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestChartOwnsCRDs(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		files map[string]string
		want  bool
	}{
		"crds dir":     {map[string]string{"c/Chart.yaml": "name: c", "c/crds/x.yaml": "x"}, true},
		"template crd": {map[string]string{"c/templates/crds.yaml": "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n"}, true},
		"subchart crd": {map[string]string{"c/charts/s/crds/x.yaml": "x"}, true},
		"no crds":      {map[string]string{"c/templates/cm.yaml": "kind: ConfigMap", "c/values.yaml": "kind: CustomResourceDefinition"}, false},
		// Fail closed: a CRD declared anywhere a chart can render it from.
		"quoted kind":       {map[string]string{"c/templates/crd.yaml": "kind: \"CustomResourceDefinition\"\n"}, true},
		"json template":     {map[string]string{"c/templates/crd.json": `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition"}`}, true},
		"crd in files/":     {map[string]string{"c/files/x-crd.yaml": "kind: CustomResourceDefinition\n", "c/templates/crd.yaml": "{{ .Files.Get \"files/x-crd.yaml\" }}"}, true},
		"packaged chart":    {map[string]string{"c/charts/s-1.0.0.tgz": string(chartArchive(t, map[string]string{"s/crds/x.yaml": "x"}))}, true},
		"too large to scan": {map[string]string{"c/templates/big.yaml": strings.Repeat("#", maxTemplateBytes+1)}, true},
	}
	for name, tt := range tests {
		got, err := chartOwnsCRDs(chartArchive(t, tt.files))
		if err != nil || got != tt.want {
			t.Errorf("%s: chartOwnsCRDs() = %v, %v; want %v", name, got, err, tt.want)
		}
	}
	if _, err := chartOwnsCRDs([]byte("plain")); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("chartOwnsCRDs(plain) = %v", err)
	}
}

// fakeDelivery records mirrored charts and returns a fixed archive; wrappers
// are packaged for real, so their digests are the deterministic ones.
type fakeDelivery struct {
	archive []byte
	err     error
	got     []registry.UpstreamChart
	oci     []registry.UpstreamChart
	wrapped []string
}

func (f *fakeDelivery) MirrorHTTPChart(_ context.Context, up registry.UpstreamChart, target registry.Repository) (*registry.MirroredChart, error) {
	f.got = append(f.got, up)
	if f.err != nil {
		return nil, f.err
	}
	return &registry.MirroredChart{
		Repository: target.Child(up.Chart), Digest: plan.DigestOf([]byte(up.Chart)),
		ArchiveDigest: plan.DigestOf(f.archive), Archive: f.archive,
	}, nil
}

func (f *fakeDelivery) MirrorOCIChart(_ context.Context, up registry.UpstreamChart, target registry.Repository) (*registry.MirroredChart, error) {
	f.oci = append(f.oci, up)
	if f.err != nil {
		return nil, f.err
	}
	name := up.Chart[strings.LastIndex(up.Chart, "/")+1:]
	return &registry.MirroredChart{
		Repository: target.Child(name), Digest: plan.DigestOf([]byte(up.Chart)),
		ArchiveDigest: plan.DigestOf(f.archive), Archive: f.archive,
	}, nil
}

func (f *fakeDelivery) PushWrapper(_ context.Context, target registry.Repository, files map[string][]byte) (*registry.MirroredChart, error) {
	f.wrapped = append(f.wrapped, target.Path)
	archive, _, err := registry.PackageWrapper(files)
	if err != nil {
		return nil, err
	}
	return &registry.MirroredChart{Repository: target, Digest: plan.DigestOf(archive), ArchiveDigest: plan.DigestOf(archive), Archive: archive}, nil
}

func readBundle(t *testing.T, dir string) *registry.Bundle {
	t.Helper()
	b := &registry.Bundle{Digest: plan.DigestOf([]byte(dir)), Files: map[string][]byte{}}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b.Files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func buildInput(t *testing.T) *BuildInput {
	t.Helper()
	charts, _ := registry.ParseRepository("oci://registry.local:5000/aicr/cluster/charts")
	wrappers, _ := registry.ParseRepository("oci://registry.local:5000/aicr/cluster/wrappers")
	recipe := plan.RecipeContent([]byte(`{}`))
	return &BuildInput{
		Bundle: readBundle(t, filepath.Join("testdata", "bundle-cert-manager")), BundleURI: "oci://registry.local:5000/aicr/cluster/bundles/cluster@sha256:x",
		DeclaredVersion: "v1.0.0", RuntimeUID: uid, Engine: plan.EngineArgoCD, EnginePolicy: "argocd.v1",
		Charts: charts, Wrappers: wrappers, InputDigest: plan.DigestOf([]byte("in")),
		Provenance: plan.Provenance{AICRVersion: "v1.0.0", OperatorVersion: "dev"},
		Recipe:     &plan.Recipe{Name: "h100-eks-ubuntu-training-kubeflow", ConfigMapRef: recipe.Ref()},
	}
}

// One mirrored chart becomes one pinned release whose values are AICR's
// merged files, and the same bundle always yields the same plan digest.
func TestBuildSingleRelease(t *testing.T) {
	t.Parallel()
	delivery := &fakeDelivery{archive: chartArchive(t, map[string]string{"cert-manager/templates/crds.yaml": "kind: CustomResourceDefinition"})}
	built, err := Build(context.Background(), delivery, buildInput(t))
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	p := built.Plan
	if len(p.Releases) != 1 || len(built.Values) != 1 {
		t.Fatalf("releases = %d, values = %d", len(p.Releases), len(built.Values))
	}
	r := p.Releases[0]
	if r.ID != "cert-manager" || r.Phase != plan.PhasePrimary || r.TargetNamespace != "cert-manager" || !r.OwnsCRDs ||
		r.Chart.Source != plan.ChartMirrored || r.Chart.Repository != "oci://registry.local:5000/aicr/cluster/charts/cert-manager" ||
		r.Chart.Upstream.Repository != "https://charts.jetstack.io" || r.Chart.Version != "v1.20.2" {

		t.Fatalf("release = %+v", r)
	}
	if delivery.got[0] != (registry.UpstreamChart{Repository: "https://charts.jetstack.io", Chart: "cert-manager", Version: "v1.20.2"}) {
		t.Fatalf("mirrored = %+v", delivery.got)
	}
	var values map[string]any
	if err = json.Unmarshal(built.Values[0].Data, &values); err != nil || values["fullnameOverride"] != "cert-manager" {
		t.Fatalf("values = %s, %v", built.Values[0].Data, err)
	}
	if r.Values != built.Values[0].Ref() || p.Provenance.Source != plan.SourceResolved || p.Provenance.Bundle == "" {
		t.Fatalf("plan = %+v", p)
	}
	again, err := Build(context.Background(), &fakeDelivery{archive: delivery.archive}, buildInput(t))
	if err != nil || again.Plan.DesiredStateDigest != p.DesiredStateDigest {
		t.Fatalf("rebuild digest = %v, %v; want %s", again.Plan.DesiredStateDigest, err, p.DesiredStateDigest)
	}
}

// An AICR patch release that renders the same releases, charts and values
// yields the same desiredStateDigest: versions, image, bundle digest and
// input digest are provenance, not identity.
func TestBuildDigestIgnoresAICRPatchRelease(t *testing.T) {
	t.Parallel()
	delivery := &fakeDelivery{archive: chartArchive(t, map[string]string{"cert-manager/Chart.yaml": "name: cert-manager"})}
	built, err := Build(context.Background(), delivery, buildInput(t))
	if err != nil {
		t.Fatal(err)
	}

	patch := buildInput(t)
	recipe := bytes.Replace(patch.Bundle.Files["recipe.yaml"], []byte("\n  version: 1.0.0\n"), []byte("\n  version: 1.0.1\n"), 1)
	sum := sha256.Sum256(recipe)
	info := strings.ReplaceAll(string(patch.Bundle.Files[aicrjob.BundleInfoFile]), "version: 1.0.0", "version: 1.0.1")
	info = strings.Replace(info, "sha256:9f3f0ba334e85d5ae3afa2b9b3c19c920428b5ffb7b82aeffd02d8823e30d345", "sha256:"+hex.EncodeToString(sum[:]), 1)
	patch.Bundle.Files["recipe.yaml"], patch.Bundle.Files[aicrjob.BundleInfoFile] = recipe, []byte(info)
	patch.DeclaredVersion = "v1.0.1"
	patch.BundleURI = "oci://registry.local:5000/aicr/cluster/bundles/cluster@sha256:y"
	patch.InputDigest = plan.DigestOf([]byte("patch"))
	patch.Provenance.AICRVersion, patch.Provenance.AICRImage = "v1.0.1", "ghcr.io/nvidia/aicr@sha256:patch"

	again, err := Build(context.Background(), &fakeDelivery{archive: delivery.archive}, patch)
	if err != nil {
		t.Fatalf("Build(v1.0.1) = %v", err)
	}
	if again.Plan.DesiredStateDigest != built.Plan.DesiredStateDigest || again.Plan.Provenance.AICRVersion != "v1.0.1" {
		t.Fatalf("patch release digest = %s (provenance %s), want %s", again.Plan.DesiredStateDigest,
			again.Plan.Provenance.AICRVersion, built.Plan.DesiredStateDigest)
	}
}

func TestBuildFailsClosed(t *testing.T) {
	t.Parallel()
	archive := chartArchive(t, map[string]string{"cert-manager/Chart.yaml": "name: cert-manager"})
	tests := map[string]struct {
		edit func(*BuildInput)
		err  error
		code operrors.ErrorCode
	}{
		"version mismatch": {edit: func(in *BuildInput) { in.DeclaredVersion = "v1.1.0" }, code: operrors.ErrCodeInvalidRequest},
		"no bundle info":   {edit: func(in *BuildInput) { delete(in.Bundle.Files, aicrjob.BundleInfoFile) }, code: operrors.ErrCodeInvalidRequest},
		"no chart at all": {edit: func(in *BuildInput) {
			delete(in.Bundle.Files, "001-cert-manager/upstream.env")
		}, code: operrors.ErrCodeInvalidRequest},
		"OCI upstream for another chart": {edit: func(in *BuildInput) {
			in.Bundle.Files["001-cert-manager/upstream.env"] = []byte("CHART='oci://ghcr.io/x'\nREPO=''\nVERSION='v1.20.2'\n")
		}, code: operrors.ErrCodeInvalidRequest},
		"version disagrees with recipe": {edit: func(in *BuildInput) {
			in.Bundle.Files["001-cert-manager/upstream.env"] = []byte("CHART='cert-manager'\nREPO='https://charts.jetstack.io'\nVERSION='v9'\n")
		}, code: operrors.ErrCodeInvalidRequest},
		"upstream disagrees with recipe": {edit: func(in *BuildInput) {
			in.Bundle.Files["001-cert-manager/upstream.env"] = []byte("CHART='cert-manager'\nREPO='https://evil.example.com'\nVERSION='v1.20.2'\n")
		}, code: operrors.ErrCodeInvalidRequest},
		"mirror conflict": {err: operrors.New(operrors.ErrCodeConflict, "tampered"), code: operrors.ErrCodeConflict},
		"bad values": {edit: func(in *BuildInput) {
			in.Bundle.Files["001-cert-manager/values.yaml"] = []byte("a: [")
		}, code: operrors.ErrCodeInvalidRequest},
	}
	for name, tt := range tests {
		in := buildInput(t)
		if tt.edit != nil {
			tt.edit(in)
		}
		_, err := Build(context.Background(), &fakeDelivery{archive: archive, err: tt.err}, in)
		if operrors.CodeOf(err) != tt.code {
			t.Errorf("%s: Build() = %v, want %s", name, err, tt.code)
		}
	}
}

// Edges follow the bundle recipe: dependents wait for every non-pre release
// of a dependency, injected releases depend on their primary.
func TestDependencies(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "aicrjob", "testdata", "v1.0.0", "bundle-h100-eks-ubuntu-training-kubeflow")
	raw, err := os.ReadFile(filepath.Join(dir, aicrjob.BundleInfoFile))
	if err != nil {
		t.Fatal(err)
	}
	info, err := aicrjob.ParseBundleInfo(raw, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	recipeRaw, _ := os.ReadFile(filepath.Join(dir, "recipe.yaml"))
	recipe, err := info.ParseBundleRecipe(recipeRaw, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]*aicrjob.ComponentRef{}
	for i := range recipe.ComponentRefs {
		refs[recipe.ComponentRefs[i].Name] = &recipe.ComponentRefs[i]
	}
	byComponent := map[string][]*aicrjob.BundleRelease{}
	releases := map[string]*aicrjob.BundleRelease{}
	for i := range info.Layout.Releases {
		r := &info.Layout.Releases[i]
		byComponent[r.Component] = append(byComponent[r.Component], r)
		releases[r.Name] = r
	}
	deps := func(name string) []string {
		r := releases[name]
		return dependencies(r, refs[r.Component], byComponent)
	}
	if got := deps("gpu-operator-readiness"); !slices.Equal(got, []string{"gpu-operator"}) {
		t.Errorf("readiness deps = %v", got)
	}
	if got := deps("kubeflow-trainer-post"); !slices.Equal(got, []string{"kubeflow-trainer"}) {
		t.Errorf("post deps = %v", got)
	}
	// Whatever depends on gpu-operator also waits for its readiness gate.
	for name, r := range releases {
		if r.Phase != plan.PhasePrimary || !slices.Contains(refs[r.Component].DependencyRefs, "gpu-operator") {
			continue
		}
		if got := deps(name); !slices.Contains(got, "gpu-operator") || !slices.Contains(got, "gpu-operator-readiness") {
			t.Errorf("%s deps = %v, want gpu-operator and its readiness gate", name, got)
		}
	}
}

// The whole H100 EKS lane: every release is served from the runtime
// registry. HTTPS and OCI upstreams are mirrored, and AICR's local charts
// (readiness, post and manifest-only releases) are packaged as wrappers.
func TestBuildLaneBundle(t *testing.T) {
	t.Parallel()
	in := buildInput(t)
	in.Bundle = readBundle(t, filepath.Join("..", "aicrjob", "testdata", "v1.0.0", "bundle-h100-eks-ubuntu-training-kubeflow"))
	delivery := &fakeDelivery{archive: chartArchive(t, map[string]string{"c/Chart.yaml": "name: c"})}
	built, err := Build(context.Background(), delivery, in)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if len(built.Plan.Releases) != 17 || len(delivery.got) != 9 || len(delivery.oci) != 5 || len(delivery.wrapped) != 3 {
		t.Fatalf("releases=%d http=%d oci=%d wrappers=%d", len(built.Plan.Releases), len(delivery.got), len(delivery.oci), len(delivery.wrapped))
	}
	byID := map[string]plan.Release{}
	for _, r := range built.Plan.Releases {
		byID[r.ID] = r
	}
	kai := byID["kai-scheduler"]
	if kai.Chart.Source != plan.ChartMirrored || kai.Chart.Upstream.Repository != "oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler" ||
		kai.Chart.Repository != "oci://registry.local:5000/aicr/cluster/charts/kai-scheduler" {

		t.Errorf("kai-scheduler chart = %+v", kai.Chart)
	}
	readiness := byID["gpu-operator-readiness"]
	if readiness.Chart.Source != plan.ChartWrapper || readiness.Chart.Upstream != nil || readiness.ReadinessFor != "gpu-operator" ||
		readiness.Chart.Repository != "oci://registry.local:5000/aicr/cluster/wrappers/gpu-operator-readiness" || readiness.Chart.Version != "1.0.0" {

		t.Errorf("readiness chart = %+v", readiness)
	}
	if byID["nodewright-customizations"].Chart.Source != plan.ChartWrapper || byID["kubeflow-trainer-post"].Phase != plan.PhasePost {
		t.Errorf("wrappers = %+v %+v", byID["nodewright-customizations"].Chart, byID["kubeflow-trainer-post"])
	}
	again, err := Build(context.Background(), &fakeDelivery{archive: delivery.archive}, in)
	if err != nil || again.Plan.DesiredStateDigest != built.Plan.DesiredStateDigest {
		t.Fatalf("rebuilding the lane plan changed its digest: %v", err)
	}
	rebuildFromApplications(t, built)
}

// Deployment validation runs against the recipe the bundle was rendered
// from, which AICR filters by the render flags, not the recipe Job's
// output: the lane bundles leave out dra-node-labeler, which the recipe
// still names, so validation must not require it. The render recipe stays
// the plan's recipe for upgrade-check and reproduce.
func TestBuildRecordsTheBundleRecipeForValidation(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"v1.0.0", "v1.1.2"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			in := buildInput(t)
			in.DeclaredVersion = version
			in.Bundle = readBundle(t, filepath.Join("..", "aicrjob", "testdata", version, "bundle-h100-eks-ubuntu-training-kubeflow"))
			render := in.Recipe.ConfigMapRef
			built, err := Build(context.Background(), &fakeDelivery{archive: chartArchive(t, map[string]string{"c/Chart.yaml": "name: c"})}, in)
			if err != nil {
				t.Fatalf("Build() = %v", err)
			}
			got := built.Plan.Recipe
			if built.ValidationRecipe == nil || got == nil || got.ConfigMapRef != render || in.Recipe.ValidationConfigMapRef != nil ||
				got.ValidationConfigMapRef == nil || *got.ValidationConfigMapRef != built.ValidationRecipe.Ref() {

				t.Fatalf("plan recipe = %+v, validation content = %+v", got, built.ValidationRecipe)
			}
			if got.Validation() != built.ValidationRecipe.Ref() {
				t.Fatalf("Validation() = %+v, want the bundle recipe", got.Validation())
			}
			var recipe struct {
				ComponentRefs []struct {
					Name string `json:"name"`
				} `json:"componentRefs"`
			}
			if err = json.Unmarshal(built.ValidationRecipe.Data, &recipe); err != nil {
				t.Fatal(err)
			}
			names := make([]string, 0, len(recipe.ComponentRefs))
			for _, c := range recipe.ComponentRefs {
				names = append(names, c.Name)
			}
			if len(names) != 15 || slices.Contains(names, "dra-node-labeler") || !slices.Contains(names, "gpu-operator") {
				t.Fatalf("validation recipe components = %v", names)
			}
		})
	}
}

// A plan written before it recorded a validation recipe validates against
// its render recipe.
func TestRecipeValidationFallsBackToTheRenderRecipe(t *testing.T) {
	t.Parallel()
	r := &plan.Recipe{ConfigMapRef: plan.DigestedRef{ConfigMap: "aicr-recipe-a", Digest: "sha256:a"}}
	if r.Validation() != r.ConfigMapRef {
		t.Fatalf("Validation() = %+v", r.Validation())
	}
}

// rebuildFromApplications renders every release's Application as the
// adapter writes it, round-trips it through JSON as the API server returns
// it, suspends automated sync on some, and rebuilds the plan from them: the
// digest and every values digest match.
func rebuildFromApplications(t *testing.T, built *Built) {
	t.Helper()
	target := argocd.Target{Namespace: "argocd", Project: "aicr", RuntimeName: "cluster"}
	values := map[string][]byte{}
	for _, c := range built.Values {
		values[c.Digest] = c.Data
	}
	apps := make([]unstructured.Unstructured, 0, len(built.Plan.Releases))
	for i := range built.Plan.Releases {
		r := &built.Plan.Releases[i]
		app, err := argocd.Application(target, built.Plan, r, values[r.Values.Digest])
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(app.Object)
		if err != nil {
			t.Fatal(err)
		}
		live := unstructured.Unstructured{}
		if err = json.Unmarshal(data, &live.Object); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err = unstructured.SetNestedField(live.Object, false, "spec", "syncPolicy", "automated", "enabled"); err != nil {
				t.Fatal(err)
			}
		}
		apps = append(apps, live)
	}
	slices.Reverse(apps)
	doc, contents, err := argocd.Rebuild(target, built.Plan.RuntimeUID, apps)
	if err != nil {
		t.Fatalf("Rebuild() = %v", err)
	}
	if doc.DesiredStateDigest != built.Plan.DesiredStateDigest || len(contents) != len(built.Values) || doc.Provenance.Source != plan.SourceRebuilt {
		t.Fatalf("rebuilt digest %s, want %s", doc.DesiredStateDigest, built.Plan.DesiredStateDigest)
	}
	for _, c := range contents {
		if _, ok := values[c.Digest]; !ok {
			t.Errorf("rebuilt values %s match no original", c.Name)
		}
	}

	// An edited Application cannot be rebuilt from, and says which.
	edited := apps[0].DeepCopy()
	if err = unstructured.SetNestedField(edited.Object, "other", "spec", "destination", "server"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = argocd.Rebuild(target, built.Plan.RuntimeUID, []unstructured.Unstructured{*edited}); operrors.ReasonOf(err) != argocd.ReasonApplicationEdited ||
		!strings.Contains(err.Error(), edited.GetName()) {

		t.Errorf("Rebuild(edited) = %v", err)
	}
}
