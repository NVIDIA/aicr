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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

const registryHost = "registry.aicr-registry.svc.cluster.local:5000"

// resolveFullLane pauses the runtime, so nothing is rolled out on a cluster
// without GPUs, and enables every lane component: the plan must serve all
// of them from the runtime registry, mirrored from HTTPS and OCI upstreams
// or packaged from AICR's local charts.
func resolveFullLane(ctx context.Context, t *testing.T) {
	t.Helper()
	rt := getRuntime(ctx, t)
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Spec.Paused = true
	kept := rt.Spec.Overrides[:0]
	for _, o := range rt.Spec.Overrides {
		if o.Path != "enabled" {
			kept = append(kept, o)
		}
	}
	rt.Spec.Overrides = kept
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatalf("enable the full lane: %v", err)
	}
	digest := waitResolved(ctx, t)
	doc := readPlan(ctx, t, digest)
	sources := map[plan.ChartSource]int{}
	oci := 0
	for _, r := range doc.Releases {
		sources[r.Chart.Source]++
		if !strings.HasPrefix(r.Chart.Repository, "oci://"+registryHost+"/") || !strings.HasPrefix(r.Chart.Digest, "sha256:") {
			t.Errorf("release %s is not served from the runtime registry: %+v", r.ID, r.Chart)
		}
		if r.Chart.Upstream != nil && strings.HasPrefix(r.Chart.Upstream.Repository, "oci://") {
			oci++
		}
	}
	t.Logf("full lane plan %s: %d releases, %v, %d from OCI upstreams", digest, len(doc.Releases), sources, oci)
	if len(doc.Releases) < 15 || sources[plan.ChartWrapper] == 0 || oci == 0 {
		t.Fatalf("plan releases = %d, sources = %v, OCI upstreams = %d", len(doc.Releases), sources, oci)
	}
	if ros := rolloutsTo(ctx, t, digest); len(ros) != 0 {
		t.Fatalf("a paused runtime started rollout %s", ros[0].Name)
	}
	reproduceMatches(ctx, t, digest, false)
}

// tamperBlocksPlan rewrites the mirrored cert-manager tag in the runtime
// registry with other content, then asks for a new plan: mirroring must
// refuse it, the runtime stalls, and the engine keeps the active plan.
func tamperBlocksPlan(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	before := applicationVersion(ctx, t)
	const repo, tag = "aicr/cluster/charts/cert-manager", "v1.20.2"
	var original []byte
	withRegistry(ctx, t, func(base string) { original = retag(ctx, t, base, repo, tag) })
	// Put the mirrored content back, so a later run starts from a clean registry.
	t.Cleanup(func() {
		withRegistry(context.WithoutCancel(ctx), t, func(base string) {
			registryCall(context.WithoutCancel(ctx), t, http.MethodPut, fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, tag), original)
		})
	})
	setCPU(ctx, t, `"20m"`)
	reason := waitStalled(ctx, t, "already holds")
	if reason != "Conflict" {
		t.Errorf("Stalled reason = %q, want Conflict", reason)
	}
	app := application(ctx, t)
	if app.GetAnnotations()[v1alpha1.AnnotationPlanDigest] != active {
		t.Errorf("tampered content reached the engine: Application at plan %s", app.GetAnnotations()[v1alpha1.AnnotationPlanDigest])
	}
	t.Logf("Application resourceVersion %s -> %s (Argo CD status only)", before, app.GetResourceVersion())
}

// retag points tag at a manifest that differs from the mirrored one only by
// an annotation, as tampering or an upstream rewrite would, through the
// registry's HTTP API. It returns the original manifest.
func retag(ctx context.Context, t *testing.T, base, repo, tag string) []byte {
	t.Helper()
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, tag)
	data := registryCall(ctx, t, http.MethodGet, url, nil)
	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s:%s: %v", repo, tag, err)
	}
	if manifest.Annotations == nil {
		manifest.Annotations = map[string]string{}
	}
	manifest.Annotations["tampered"] = "true"
	changed, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	registryCall(ctx, t, http.MethodPut, url, changed)
	t.Logf("%s:%s now points at %s instead of %s", repo, tag, digest.FromBytes(changed), digest.FromBytes(data))
	return data
}

func registryCall(ctx context.Context, t *testing.T, method, url string, body []byte) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	creds, err := registry.DockerConfigCredentials(registryAuth)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := creds(ctx, registryHost)
	req.SetBasicAuth(c.Username, c.Password)
	req.Header.Set("Accept", ocispec.MediaTypeImageManifest)
	if body != nil {
		req.Header.Set("Content-Type", ocispec.MediaTypeImageManifest)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %s", method, url, resp.Status)
	}
	return out
}

// withRegistry runs f with the base URL of the in-cluster registry, reached
// through a port-forward.
func withRegistry(ctx context.Context, t *testing.T, f func(base string)) {
	t.Helper()
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace("aicr-registry"), client.MatchingLabels{"app": "registry"}); err != nil || len(pods.Items) == 0 {
		t.Fatalf("find registry pod: %v", err)
	}
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	url := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace("aicr-registry").Name(pods.Items[0].Name).SubResource("portforward").URL()
	stop, ready := make(chan struct{}), make(chan struct{})
	defer close(stop)
	fw, err := portforward.New(spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url),
		[]string{"0:5000"}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = fw.ForwardPorts() }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("port-forward did not become ready")
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		t.Fatalf("port-forward: %v", err)
	}
	f(fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local))
}

// assertNoCredentialLeaks checks that the registry password, in clear and
// in its docker config encoding, is in none of the places the operator
// writes or that record its work.
func assertNoCredentialLeaks(ctx context.Context, t *testing.T) {
	t.Helper()
	creds, err := registry.DockerConfigCredentials(registryAuth)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := creds(ctx, registryHost)
	secrets := []string{c.Password, base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))}
	check := func(where string, v any) {
		data, merr := json.Marshal(v)
		if merr != nil {
			t.Fatal(merr)
		}
		for _, s := range secrets {
			if bytes.Contains(data, []byte(s)) {
				t.Errorf("a registry credential appears in %s", where)
			}
		}
	}
	runtimes := &v1alpha1.ClusterRuntimeList{}
	rollouts := &v1alpha1.RuntimeRolloutList{}
	events := &corev1.EventList{}
	jobs := &batchv1.JobList{}
	for _, l := range []struct {
		where string
		list  client.ObjectList
		opts  []client.ListOption
	}{
		{"ClusterRuntime", runtimes, nil},
		{"RuntimeRollouts", rollouts, nil},
		{"Events", events, nil},
		{"AICR Jobs", jobs, []client.ListOption{client.InNamespace("aicr-jobs")}},
	} {
		if lerr := k8s.List(ctx, l.list, l.opts...); lerr != nil {
			t.Fatal(lerr)
		}
		check(l.where, l.list)
	}
	for _, ns := range []string{"aicr-system", "aicr-jobs"} {
		cms := &corev1.ConfigMapList{}
		if lerr := k8s.List(ctx, cms, client.InNamespace(ns)); lerr != nil {
			t.Fatal(lerr)
		}
		check("ConfigMaps in "+ns, cms)
	}
	pods, err := clientset.CoreV1().Pods("aicr-system").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=aicr-operator"})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("find operator pod: %v", err)
	}
	logs, err := clientset.CoreV1().Pods("aicr-system").GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check("operator logs", string(logs))
	t.Logf("checked %d bytes of operator logs, %d Events, %d Jobs", len(logs), len(events.Items), len(jobs.Items))
	// The operator runs with RBAC exactly as installed: no informer or call
	// was refused.
	if strings.Contains(strings.ToLower(string(logs)), "forbidden") {
		t.Error("the operator logs show a forbidden request")
	}
}

// waitResolved waits for Resolved=True on the current generation and
// returns the candidate plan.
func waitResolved(ctx context.Context, t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(resolveTimeout)
	for time.Now().Before(deadline) {
		rt := getRuntime(ctx, t)
		c := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionResolved)
		if c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == rt.Generation {
			return rt.Status.CandidatePlanDigest
		}
		if c != nil && c.Status == metav1.ConditionFalse && c.Reason != "Resolving" && c.ObservedGeneration == rt.Generation {
			t.Fatalf("resolution failed: %s: %s", c.Reason, c.Message)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("not resolved within %s; status %s", resolveTimeout, statusJSON(ctx, t))
	return ""
}

func readPlan(ctx context.Context, t *testing.T, digest string) *plan.Document {
	t.Helper()
	name, err := plan.Name(digest)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: name}, cm); err != nil {
		t.Fatal(err)
	}
	doc, err := plan.FromConfigMap(cm)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func rolloutsTo(ctx context.Context, t *testing.T, digest string) []v1alpha1.RuntimeRollout {
	t.Helper()
	list := &v1alpha1.RuntimeRolloutList{}
	if err := k8s.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	var out []v1alpha1.RuntimeRollout
	for i := range list.Items {
		if list.Items[i].Spec.ToPlanDigest == digest {
			out = append(out, list.Items[i])
		}
	}
	return out
}
