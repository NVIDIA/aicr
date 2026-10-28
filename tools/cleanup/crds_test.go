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

package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var dgdGVR = schema.GroupVersionResource{Group: "nvidia.com", Version: "v1beta1", Resource: "dynamographdeployments"}

// webhookCRD returns a CRD in an AICR group that converts through a webhook,
// serving v1alpha1 and v1beta1 with v1beta1 stored.
func webhookCRD(name, group, plural string) *unstructured.Unstructured {
	u := crd(name, group)
	u.Object["spec"] = map[string]any{
		"group": group,
		"names": map[string]any{"plural": plural},
		"versions": []any{
			map[string]any{"name": "v1alpha1", "served": true, "storage": false},
			map[string]any{"name": "v1beta1", "served": true, "storage": true},
		},
		"conversion": map[string]any{
			"strategy": "Webhook",
			"webhook":  map[string]any{"clientConfig": map[string]any{"service": map[string]any{"name": "op", "namespace": "dynamo-system", "path": "/convert", "port": int64(443)}}},
		},
	}
	return u
}

func dgd(ns, name string, deleting bool, finalizers ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nvidia.com/v1beta1",
		"kind":       "DynamoGraphDeployment",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	}}
	u.SetFinalizers(finalizers)
	if deleting {
		now := metav1.Now()
		u.SetDeletionTimestamp(&now)
	}
	return u
}

// crdFixture returns a fixture whose dynamic client can also list
// DynamoGraphDeployments.
func crdFixture(dynObjs ...runtime.Object) *fixture {
	f := newFixture(nil)
	f.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			crdGVR: "CustomResourceDefinitionList",
			dgdGVR: "DynamoGraphDeploymentList",
		}, dynObjs...)
	return f
}

func patchedNames(actions []k8stesting.Action, resource string) []string {
	var out []string
	for _, a := range actions {
		if p, ok := a.(k8stesting.PatchAction); ok && p.GetResource().Resource == resource {
			name := p.GetName()
			if ns := p.GetNamespace(); ns != "" {
				name = ns + "/" + name
			}
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func TestDeleteCRDsDisablesConversionWebhook(t *testing.T) {
	tests := []struct {
		name        string
		opts        options
		wantPatched []string
		wantDeleted bool
	}{
		{
			name:        "webhook CRD is switched to no conversion, then deleted",
			opts:        options{assumeYes: true},
			wantPatched: []string{"dynamographdeployments.nvidia.com"},
			wantDeleted: true,
		},
		{
			name: "excluded CRD is left alone",
			opts: options{assumeYes: true, excludeCRD: []string{"dynamographdeployments"}},
		},
		{
			name: "dry run patches nothing",
			opts: options{assumeYes: true, dryRun: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := crdFixture(
				webhookCRD("dynamographdeployments.nvidia.com", "nvidia.com", "dynamographdeployments"),
				webhookCRD("widgets.example.com", "example.com", "widgets"),
				crd("clusterpolicies.nvidia.com", "nvidia.com"),
			)

			f.cleaner(tt.opts).deleteCRDs(context.Background())

			if got := patchedNames(f.dyn.Actions(), crdGVR.Resource); !slices.Equal(got, tt.wantPatched) {
				t.Errorf("patched CRDs = %q, want %q", got, tt.wantPatched)
			}
			var patchedBeforeDelete bool
			for _, a := range f.dyn.Actions() {
				if p, ok := a.(k8stesting.PatchAction); ok && p.GetResource() == crdGVR {
					if p.GetName() == "dynamographdeployments.nvidia.com" {
						patchedBeforeDelete = true
					}
					if got := string(p.GetPatch()); got != string(disableConversionPatch) {
						t.Errorf("patch body = %s, want %s", got, disableConversionPatch)
					}
				}
				if d, ok := a.(k8stesting.DeleteAction); ok && d.GetName() == "dynamographdeployments.nvidia.com" && !patchedBeforeDelete {
					t.Error("dynamographdeployments.nvidia.com was deleted before its conversion webhook was disabled")
				}
			}
			deleted := !slices.Contains(f.crdNames(t), "dynamographdeployments.nvidia.com")
			if deleted != tt.wantDeleted {
				t.Errorf("dynamographdeployments.nvidia.com deleted = %v, want %v", deleted, tt.wantDeleted)
			}
			if !slices.Contains(f.crdNames(t), "widgets.example.com") {
				t.Error("deleted widgets.example.com, which is not in an AICR group")
			}
		})
	}
}

func TestDeleteCRDsRescuesStuckInstances(t *testing.T) {
	stuck := webhookCRD("dynamographdeployments.nvidia.com", "nvidia.com", "dynamographdeployments")
	now := metav1.Now()
	stuck.SetDeletionTimestamp(&now)
	f := crdFixture(
		stuck,
		dgd("nemotron-serve", "held", true, "nvidia.com/finalizer"),
		dgd("nemotron-serve", "not-yet-deleting", false, "nvidia.com/finalizer"),
		dgd("nemotron-serve", "no-finalizers", true),
		dgd("fenced", "held", true, "nvidia.com/finalizer"),
	)
	// The API server keeps a CRD whose instances still carry finalizers.
	f.dyn.PrependReactor("delete", crdGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	f.cleaner(options{assumeYes: true, excludeNS: []string{"fenced"}}).deleteCRDs(context.Background())

	if got, want := patchedNames(f.dyn.Actions(), dgdGVR.Resource), []string{"nemotron-serve/held"}; !slices.Equal(got, want) {
		t.Errorf("instances with finalizers cleared = %q, want %q", got, want)
	}
	f.wantOutput(t,
		`msg="CRD stuck terminating, clearing finalizers of its instances" name=dynamographdeployments.nvidia.com`,
		`msg="cleared finalizers" resource=dynamographdeployments name=held namespace=nemotron-serve`,
		`msg="skipping finalizer rescue in excluded namespace" resource=dynamographdeployments name=held namespace=fenced`,
	)
}

func TestCRDListGVR(t *testing.T) {
	version := func(name string, served, storage bool) map[string]any {
		return map[string]any{"name": name, "served": served, "storage": storage}
	}
	tests := []struct {
		name     string
		versions []any
		want     string
		wantOK   bool
	}{
		{"storage version served", []any{version("v1alpha1", true, false), version("v1beta1", true, true)}, "v1beta1", true},
		{"storage version not served", []any{version("v1alpha1", true, false), version("v1beta1", false, true)}, "v1alpha1", true},
		{"nothing served", []any{version("v1", false, true)}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := crd("widgets.nvidia.com", "nvidia.com")
			u.Object["spec"] = map[string]any{
				"group":    "nvidia.com",
				"names":    map[string]any{"plural": "widgets"},
				"versions": tt.versions,
			}
			gvr, ok := crdListGVR(u)
			if ok != tt.wantOK || gvr.Version != tt.want {
				t.Errorf("crdListGVR() = %v, %v; want version %q, %v", gvr, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestWarnDeadConversionWebhooks(t *testing.T) {
	tests := []struct {
		name     string
		service  bool
		wantWarn bool
	}{
		{"webhook Service gone", false, true},
		{"webhook Service still served", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := crdFixture(
				webhookCRD("dynamographdeployments.nvidia.com", "nvidia.com", "dynamographdeployments"),
				webhookCRD("widgets.example.com", "example.com", "widgets"),
			)
			if tt.service {
				svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "dynamo-system"}}
				if _, err := f.kube.CoreV1().Services("dynamo-system").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
					t.Fatalf("create service: %v", err)
				}
			}

			f.cleaner(options{assumeYes: true, keepCRDs: true, keepNamespaces: true}).warnDeadConversionWebhooks(context.Background())

			out := f.out.String()
			warned := strings.Contains(out, "name=dynamographdeployments.nvidia.com service=dynamo-system/op")
			if warned != tt.wantWarn {
				t.Errorf("warned = %v, want %v\noutput:\n%s", warned, tt.wantWarn, out)
			}
			if strings.Contains(out, "widgets.example.com") {
				t.Errorf("warned about widgets.example.com, which is not in an AICR group\noutput:\n%s", out)
			}
		})
	}
}

func TestReportStillTerminating(t *testing.T) {
	stuckNS := terminatingNamespace("monitoring")
	stuckNS.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	stuckNS.Status.Conditions = []corev1.NamespaceCondition{
		{Type: corev1.NamespaceDeletionContentFailure, Status: corev1.ConditionTrue, Message: "failed to list nvidia.com/v1beta1"},
		{Type: corev1.NamespaceContentRemaining, Status: corev1.ConditionFalse},
	}
	f := crdFixture(webhookCRD("podcliques.grove.io", "grove.io", "podcliques"))
	for _, ns := range []*corev1.Namespace{stuckNS, {ObjectMeta: metav1.ObjectMeta{Name: "refused"}}} {
		if _, err := f.kube.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create namespace %s: %v", ns.Name, err)
		}
	}

	tests := []struct {
		name       string
		opts       options
		crds, nss  []string
		wantStuck  bool
		wantOutput []string
	}{
		{
			name:      "CRD and namespace still present",
			opts:      options{assumeYes: true},
			crds:      []string{"podcliques.grove.io", "gone.nvidia.com"},
			nss:       []string{"monitoring", "gone"},
			wantStuck: true,
			wantOutput: []string{
				`msg="CRD still terminating, so reinstalling its operator fails until it is gone. Check its instances for finalizers" name=podcliques.grove.io`,
				`msg="namespace still terminating" namespace=monitoring check="kubectl get ns monitoring -o yaml" condition=NamespaceDeletionContentFailure message="failed to list nvidia.com/v1beta1"`,
			},
		},
		{
			name:      "namespace whose delete was refused",
			opts:      options{assumeYes: true},
			nss:       []string{"refused"},
			wantStuck: true,
			wantOutput: []string{
				`msg="namespace was not deleted, see the delete failure above" namespace=refused`,
			},
		},
		{
			name: "everything gone",
			opts: options{assumeYes: true},
			crds: []string{"gone.nvidia.com"},
			nss:  []string{"gone"},
		},
		{
			name: "dry run checks nothing",
			opts: options{assumeYes: true, dryRun: true},
			crds: []string{"podcliques.grove.io"},
			nss:  []string{"monitoring"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.out.Reset()
			if got := f.cleaner(tt.opts).reportStillTerminating(context.Background(), tt.crds, tt.nss); got != tt.wantStuck {
				t.Errorf("reportStillTerminating() = %v, want %v", got, tt.wantStuck)
			}
			f.wantOutput(t, tt.wantOutput...)
			if !tt.wantStuck && strings.Contains(f.out.String(), "still terminating") {
				t.Errorf("reported a stuck resource\noutput:\n%s", f.out.String())
			}
		})
	}
}

func TestRunReportsStuckNamespaceInsteadOfComplete(t *testing.T) {
	f := newFixture([]runtime.Object{namespace("gpu-operator")})
	// The namespace controller cannot finish, so the namespace stays marked
	// for deletion.
	f.kube.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.DeleteAction).GetName()
		obj, err := f.kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("namespaces"), "", name)
		if err != nil {
			return true, nil, err
		}
		ns := obj.(*corev1.Namespace)
		ns.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		return true, nil, f.kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("namespaces"), ns, "")
	})

	if err := f.cleaner(options{assumeYes: true, keepCRDs: true}).run(context.Background(), confirmed); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	f.wantOutput(t,
		`msg="namespace still terminating" namespace=gpu-operator`,
		`msg="AICR cleanup finished with resources still terminating, see the warnings above"`,
	)
	if strings.Contains(f.out.String(), `msg="AICR cleanup complete"`) {
		t.Errorf("run reported complete with a namespace left\noutput:\n%s", f.out.String())
	}
}

func TestDeleteCRDsFailedDeleteIsNotReported(t *testing.T) {
	f := crdFixture(webhookCRD("dynamographdeployments.nvidia.com", "nvidia.com", "dynamographdeployments"))
	f.dyn.PrependReactor("delete", crdGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd unavailable"))
	})

	if got := f.cleaner(options{assumeYes: true}).deleteCRDs(context.Background()); len(got) != 0 {
		t.Errorf("deleteCRDs() = %q, want no CRDs reported deleted", got)
	}
	f.wantOutput(t, `msg="CRD left with its conversion webhook disabled after its delete failed. `+
		`Re-run cleanup, or reinstall its operator with the CRD replaced" name=dynamographdeployments.nvidia.com`)
	if strings.Contains(f.out.String(), "stuck terminating") {
		t.Errorf("rescued a CRD that was never deleted\noutput:\n%s", f.out.String())
	}
}

func TestReportStillTerminatingGetErrorIsNotGone(t *testing.T) {
	f := crdFixture()
	f.kube.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver restarting")
	})

	if !f.cleaner(options{assumeYes: true}).reportStillTerminating(context.Background(), nil, []string{"monitoring"}) {
		t.Error("reportStillTerminating() = false, want a Get error counted as not confirmed gone")
	}
	f.wantOutput(t, `msg="could not confirm the namespace is gone" namespace=monitoring`)
}
