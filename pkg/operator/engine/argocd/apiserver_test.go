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

package argocd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "aicr.run/api/operator/v1alpha1"
	operrors "aicr.run/pkg/operator/errors"
)

// apiServer is an envtest API server with the Argo CD CRDs, started by the
// first test that needs it: the rest of the package's tests need none.
var apiServer struct {
	once   sync.Once
	env    *envtest.Environment
	client client.WithWatch
	err    error
}

// namespaces numbers the namespace each serverAdapter writes in.
var namespaces atomic.Int64

func TestMain(m *testing.M) {
	code := m.Run()
	if apiServer.env != nil {
		_ = apiServer.env.Stop()
	}
	os.Exit(code)
}

// serverAdapter is an Adapter over a real API server, in a namespace of its
// own. The fake client this package builds keeps no managed fields and
// ignores a UID precondition on delete, and only a real server shows how
// server-side apply and these preconditions meet a concurrent writer.
func serverAdapter(ctx context.Context, t *testing.T) *Adapter {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is not set; the adapter's API server tests cannot run")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set (run `make test`)")
	}
	apiServer.once.Do(func() {
		apiServer.env = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "..", "tests", "operator", "chart", "testdata", "argocd-crds.yaml")},
			ErrorIfCRDPathMissing: true,
		}
		cfg, err := apiServer.env.Start()
		if err != nil {
			apiServer.err = err
			return
		}
		scheme := runtime.NewScheme()
		if err = clientgoscheme.AddToScheme(scheme); err != nil {
			apiServer.err = err
			return
		}
		apiServer.client, apiServer.err = client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	})
	if apiServer.err != nil {
		t.Fatalf("start the API server: %v", apiServer.err)
	}
	c := apiServer.client
	ns := fmt.Sprintf("argocd-%d", namespaces.Add(1))
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	return &Adapter{Writer: c, Reader: c, Mapper: c.RESTMapper(),
		Target: Target{Namespace: ns, Project: target.Project, RuntimeName: target.RuntimeName}}
}

// managers lists the field managers of an object's managed fields.
func managers(obj *unstructured.Unstructured) []string {
	out := make([]string, 0, len(obj.GetManagedFields()))
	for _, e := range obj.GetManagedFields() {
		out = append(out, e.Manager)
	}
	return out
}

// Ensure applies the operator's Application as its field manager, asks Argo
// CD to compare an Application whose spec it already has, and forces back
// the spec fields another manager took over or added, naming that manager.
// Refresh asks for a comparison on its own.
func TestEnsureOnAPIServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := serverAdapter(ctx, t)
	p, r := testPlan()
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	live, err := a.get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(managers(live), FieldManager) {
		t.Fatalf("Ensure wrote as %v, want %s", managers(live), FieldManager)
	}
	if _, err = a.Ensure(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	if live, err = a.get(ctx, r.ID); err != nil || live.GetAnnotations()[RefreshAnnotation] == "" {
		t.Fatalf("Ensure() of an unchanged spec requested no refresh: %v, %v", live.GetAnnotations(), err)
	}
	annotations := live.GetAnnotations()
	delete(annotations, RefreshAnnotation)
	live.SetAnnotations(annotations)
	if err = unstructured.SetNestedField(live.Object, false, "spec", "syncPolicy", "automated", "enabled"); err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedSlice(live.Object, []any{map[string]any{"kind": "Secret"}}, "spec", "ignoreDifferences"); err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedField(live.Object, int64(3), "spec", "revisionHistoryLimit"); err != nil {
		t.Fatal(err)
	}
	if err = a.Writer.Update(ctx, live, client.FieldOwner("kubectl-edit")); err != nil {
		t.Fatal(err)
	}
	ensured, err := a.Ensure(ctx, p, r, values)
	if err != nil || !slices.Equal(ensured.Overridden, []string{"kubectl-edit"}) {
		t.Fatalf("Ensure() over another manager's edit = %+v, %v; want kubectl-edit overridden", ensured, err)
	}
	live, err = a.get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := Application(a.Target, p, r, values)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(live.Object["spec"], desired.Object["spec"]) {
		t.Errorf("spec after Ensure = %v, want the plan's", live.Object["spec"])
	}

	annotations = live.GetAnnotations()
	delete(annotations, RefreshAnnotation)
	live.SetAnnotations(annotations)
	if err = a.Writer.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err = a.Refresh(ctx, r); err != nil {
		t.Fatalf("Refresh() = %v", err)
	}
	if live, err = a.get(ctx, r.ID); err != nil || live.GetAnnotations()[RefreshAnnotation] == "" {
		t.Errorf("Refresh() left annotations %v, %v", live.GetAnnotations(), err)
	}
}

// Verify applies one sync operation as the operator, and writes nothing
// while a sync is pending (Argo CD has not taken the operation yet) or
// running (Argo CD took it and cleared it).
func TestVerifyOnAPIServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := serverAdapter(ctx, t)
	p, r := testPlan()
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(ctx, p, r, values); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	live, err := a.get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rev, _, _ := unstructured.NestedString(live.Object, "operation", "sync", "revision"); rev != chartDigest {
		t.Fatalf("operation = %v", live.Object["operation"])
	}

	// A pending operation Verify did not write stays as it is.
	if err = unstructured.SetNestedField(live.Object, "pending", "operation", "sync", "revision"); err != nil {
		t.Fatal(err)
	}
	if err = a.Writer.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err = a.Verify(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	if live, err = a.get(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if rev, _, _ := unstructured.NestedString(live.Object, "operation", "sync", "revision"); rev != "pending" {
		t.Errorf("Verify replaced a pending operation: %v", live.Object["operation"])
	}

	// As Argo CD starts the sync, it clears the operation and records the
	// sync as running: Verify requests no second one.
	unstructured.RemoveNestedField(live.Object, "operation")
	if err = unstructured.SetNestedField(live.Object, "Running", "status", "operationState", "phase"); err != nil {
		t.Fatal(err)
	}
	if err = a.Writer.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err = a.Verify(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	if live, err = a.get(ctx, r.ID); err != nil || live.Object["operation"] != nil {
		t.Errorf("Verify requested a sync while one was running: %v, %v", live.Object["operation"], err)
	}
}

// Remove marks the recorded Application as a field manager of its own,
// leaving the operator's spec owned, and deletes it behind the resources
// finalizer.
func TestRemoveOnAPIServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := serverAdapter(ctx, t)
	p, r := testPlan()
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	live, err := a.get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Remove(ctx, uid, r.ID, string(live.GetUID()), "rollout-x-1"); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	if live, err = a.get(ctx, r.ID); err != nil {
		t.Fatalf("the finalizer should hold the Application: %v", err)
	}
	if live.GetDeletionTimestamp() == nil || !slices.Contains(live.GetFinalizers(), ResourcesFinalizer) ||
		live.GetAnnotations()[v1alpha1.AnnotationDeleteRollout] != "rollout-x-1" {

		t.Fatalf("removed Application = %v", live.Object["metadata"])
	}
	if got := managers(live); !slices.Contains(got, RemovalFieldManager) || !slices.Contains(got, FieldManager) {
		t.Errorf("field managers after Remove = %v, want %s beside %s", got, RemovalFieldManager, FieldManager)
	}
	if src, _, _ := unstructured.NestedMap(live.Object, "spec", "source"); src["targetRevision"] != chartDigest {
		t.Error("marking the Application for removal dropped the operator's spec")
	}
}

// The UID Remove's writes carry keeps them to the recorded object: a
// same-name Application created between Remove's read and its mark, or
// between the mark and the delete, is neither marked nor deleted, and one
// deleted before the mark is not recreated by it. A retry then finds the
// recorded object gone.
func TestRemoveWritesOnlyTheRecordedObject(t *testing.T) {
	t.Parallel()
	for name, replace := range map[string]bool{"replaced": true, "deleted": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			a := serverAdapter(ctx, t)
			p, r := testPlan()
			if _, err := a.Ensure(ctx, p, r, values); err != nil {
				t.Fatal(err)
			}
			recorded, err := a.get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			var replacement *unstructured.Unstructured
			changed := false
			writer := a.Writer
			a.Writer = interceptor.NewClient(writer.(client.WithWatch), interceptor.Funcs{
				Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
					if !changed {
						changed = true
						if werr := c.Delete(ctx, recorded); werr != nil {
							return werr
						}
						if replace {
							replacement, _ = Application(a.Target, p, r, values)
							if werr := c.Create(ctx, replacement); werr != nil {
								return werr
							}
						}
					}
					return c.Apply(ctx, obj, opts...)
				},
			})
			if err = a.Remove(ctx, uid, r.ID, string(recorded.GetUID()), "rollout-x-1"); err == nil {
				t.Fatal("Remove() wrote to an object other than the recorded one")
			}
			live, err := a.get(ctx, r.ID)
			if !replace {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("the mark recreated the deleted Application: %v, %v", live, err)
				}
				a.Writer = writer
				if err = a.Remove(ctx, uid, r.ID, string(recorded.GetUID()), "rollout-x-1"); err != nil {
					t.Errorf("Remove() retried after the Application is gone = %v", err)
				}
				return
			}
			if err != nil || live.GetUID() != replacement.GetUID() {
				t.Fatalf("the replacement is gone: %v, %v", live, err)
			}
			if live.GetDeletionTimestamp() != nil || slices.Contains(live.GetFinalizers(), ResourcesFinalizer) ||
				live.GetAnnotations()[v1alpha1.AnnotationDeleteRollout] != "" {

				t.Errorf("Remove marked the replacement: %v", live.Object["metadata"])
			}
		})
	}
}

// The delete carries the recorded UID as a precondition, so an Application
// replaced between Remove's mark and its delete survives.
func TestRemoveDeletesOnlyTheRecordedObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := serverAdapter(ctx, t)
	p, r := testPlan()
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	recorded, err := a.get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var replacement *unstructured.Unstructured
	a.Writer = interceptor.NewClient(a.Writer.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			marked := &unstructured.Unstructured{}
			marked.SetGroupVersionKind(ApplicationGVK)
			if werr := c.Get(ctx, client.ObjectKeyFromObject(obj), marked); werr != nil {
				return werr
			}
			marked.SetFinalizers(nil)
			if werr := c.Update(ctx, marked); werr != nil {
				return werr
			}
			if werr := c.Delete(ctx, marked); werr != nil {
				return werr
			}
			replacement, _ = Application(a.Target, p, r, values)
			if werr := c.Create(ctx, replacement); werr != nil {
				return werr
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	if err = a.Remove(ctx, uid, r.ID, string(recorded.GetUID()), "rollout-x-1"); err == nil {
		t.Fatal("Remove() deleted an object other than the recorded one")
	}
	live, err := a.get(ctx, r.ID)
	if err != nil || live.GetUID() != replacement.GetUID() || live.GetDeletionTimestamp() != nil {
		t.Fatalf("the replacement was deleted: %v, %v", live, err)
	}
}

// Suspend and Orphan write as field managers of their own, so a later
// Ensure tells them from another tool's edits.
func TestSuspendAndOrphanOnAPIServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := serverAdapter(ctx, t)
	p, r := testPlan()
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	if changed, err := a.Suspend(ctx, p.RuntimeUID, true); err != nil || !slices.Equal(changed, []string{r.ID}) {
		t.Fatalf("Suspend() = %v, %v", changed, err)
	}
	if names, err := a.Orphan(ctx, p.RuntimeUID); err != nil || !slices.Equal(names, []string{r.ID}) {
		t.Fatalf("Orphan() = %v, %v", names, err)
	}
	live, err := a.get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if automatedEnabled(live) || live.GetAnnotations()[v1alpha1.AnnotationOrphanedFrom] != p.RuntimeUID {
		t.Fatalf("after Suspend and Orphan: %v", live.Object)
	}
	if got := managers(live); !slices.Contains(got, SuspensionFieldManager) || !slices.Contains(got, OrphanFieldManager) {
		t.Errorf("field managers = %v, want %s and %s", got, SuspensionFieldManager, OrphanFieldManager)
	}
	if ensured, err := a.Ensure(ctx, p, r, values); err != nil || len(ensured.Overridden) != 0 {
		t.Errorf("Ensure after Suspend and Orphan = %+v, %v; want no foreign override", ensured, err)
	}
}

// Adopt moves an orphaned Application to the adopting runtime, and its
// patch's test operations refuse one that changed between Adopt's read and
// its write: taken by another runtime, or recreated under the same name.
func TestAdoptOnAPIServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const newUID, thirdUID = "99999999-8888-7777-6666-555555555555", "77777777-8888-7777-6666-555555555555"
	for _, race := range []string{"", "taken by another runtime", "recreated"} {
		name := race
		if name == "" {
			name = "adopted"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := serverAdapter(ctx, t)
			p, r := testPlan()
			if _, err := a.Ensure(ctx, p, r, values); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Orphan(ctx, p.RuntimeUID); err != nil {
				t.Fatal(err)
			}
			live, err := a.get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch race {
			case "taken by another runtime":
				a.Writer = interceptor.NewClient(a.Writer.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						taken := live.DeepCopy()
						annotations := taken.GetAnnotations()
						annotations[v1alpha1.AnnotationRuntimeUID] = thirdUID
						taken.SetAnnotations(annotations)
						if werr := c.Update(ctx, taken); werr != nil {
							return werr
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				})
			case "recreated":
				a.Writer = interceptor.NewClient(a.Writer.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if werr := c.Delete(ctx, live.DeepCopy()); werr != nil {
							return werr
						}
						again := live.DeepCopy()
						again.SetResourceVersion("")
						again.SetUID("")
						if werr := c.Create(ctx, again); werr != nil {
							return werr
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				})
			}
			err = a.Adopt(ctx, uid, newUID, r.ID, string(live.GetUID()))
			after, getErr := a.get(ctx, r.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			owner := after.GetAnnotations()[v1alpha1.AnnotationRuntimeUID]
			switch race {
			case "":
				if err != nil || owner != newUID || after.GetAnnotations()[v1alpha1.AnnotationOrphanedFrom] != "" {
					t.Errorf("Adopt() = %v; annotations %v", err, after.GetAnnotations())
				}
			case "taken by another runtime":
				if err == nil || operrors.CodeOf(err) == "" || owner != thirdUID {
					t.Errorf("Adopt() of a taken Application = %v; owner %s, want %s", err, owner, thirdUID)
				}
			case "recreated":
				if err == nil || after.GetUID() == live.GetUID() || owner != uid {
					t.Errorf("Adopt() of a recreated Application = %v; uid %s, owner %s, want it left to %s", err, after.GetUID(), owner, uid)
				}
			}
		})
	}
}
