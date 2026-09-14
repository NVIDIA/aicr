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

package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// concurrencyTestNamespace hosts both runs in TestConcurrentRuns. Both runs
// share one namespace on purpose: name-based isolation within a shared
// namespace is exactly the property issue #2120 reported broken (fixed
// object names meant two runs clobbered each other's Job, RBAC, and
// snapshot ConfigMap).
const concurrencyTestNamespace = "concurrent-runs-ns"

// Two run IDs in runid.Generate()'s YYYYMMDD-HHMMSS-<16 hex> format, equal
// in every field except the random suffix — isolation must hold between
// runs started in the same instant, not just runs separated in time.
const (
	concurrencyRunIDA = "20260821-090000-aaaaaaaaaaaaaaaa"
	concurrencyRunIDB = "20260821-090000-bbbbbbbbbbbbbbbb"
)

// concurrencyPodTimeEarly and concurrencyPodTimeLate seed distinct, ordered
// CreationTimestamps for TestConcurrentRuns's pod-selection assertion — see
// the comment above seedImposterPod's call site for why the ordering
// matters.
var (
	concurrencyPodTimeEarly = metav1.NewTime(time.Unix(1000, 0))
	concurrencyPodTimeLate  = metav1.NewTime(time.Unix(2000, 0))
)

// TestConcurrentRuns is the concurrency proof required by issue #2120
// acceptance criterion #5: two overlapping snapshot-agent runs, deployed
// from separate goroutines against one shared fake clientset and one shared
// namespace, must never collide on object names, must never leak one run's
// RBAC permissions (specifically DiscoverNetwork's extra cluster rules)
// into the other, must each select only their own Pod, must each read back
// only their own snapshot bytes, and one run's Cleanup must never touch the
// other's objects — nor a ConfigMap that merely sits at a name that run's own
// naming formula would produce but that the run does not own. Run under
// -race.
//
// Fake-clientset limitations this test works around, not around-asserts:
//
//   - No Job controller runs against the fake clientset, so Pods never
//     appear on their own. This test creates them explicitly (seedRunPod),
//     carrying the run's RunID label and a controlling ownerReference to
//     that run's Job, mirroring what a real kube-controller-manager
//     produces.
//   - The fake ObjectTracker does not assign a UID on Create. Without one,
//     jobUID() would stay the zero UID after Deploy(), and pickLivePod
//     would silently skip the ownedByJob authorization check entirely
//     (see wait.go: "jobUID != "" && !ownedByJob(...)" only runs the check
//     when a UID is present) — degrading pod selection to label-only
//     matching and defeating the point of this test. A "create","jobs"
//     reactor below assigns each Job a real, name-derived UID so
//     ownedByJob is exercised for real.
//   - List DOES filter by LabelSelector against the fake clientset:
//     k8s.io/client-go/gentype's alsoFakeLister.List applies the selector
//     client-side after the ObjectTracker's own (selector-blind) List.
//     findPodName below is List-based, so it exercises real selection
//     logic — this test does not need to fall back to asserting a raw
//     selector string for that path. Watch does NOT filter (the fake
//     Watch reactor ignores ListOptions.LabelSelector entirely); this
//     test does not exercise the watch-based pod-discovery path
//     (findOrWatchPodName), so that gap does not apply here. That path —
//     the one production actually takes, since WaitForPodReady runs right
//     after Deploy — has its own ownership coverage in
//     TestFindOrWatchPodNameAuthorizesByJobOwnership (wait_test.go).
func TestConcurrentRuns(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()

	// Deploy's Step 0 is a permissions gate; allow everything so the test
	// exercises isolation, not authorization plumbing already covered by
	// permissions_test.go.
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: true, Reason: "test permissions allowed"},
		}, nil
	})

	// See the fake-clientset limitations note above: give every created Job
	// a deterministic, name-derived UID since the ObjectTracker assigns
	// none on its own.
	clientset.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			return false, nil, nil
		}
		job, ok := ca.GetObject().(*batchv1.Job)
		if !ok {
			return false, nil, nil
		}
		job.UID = types.UID(job.Name + "-uid")
		return false, nil, nil // not handled: fall through to the default tracker create
	})

	dA := NewDeployer(clientset, Config{
		Namespace: concurrencyTestNamespace,
		Image:     "aicr:test",
		RunID:     concurrencyRunIDA,
		Output:    fmt.Sprintf("cm://%s/%s", concurrencyTestNamespace, nameWithRunID(staticStagingConfigMapName, concurrencyRunIDA)),
		// OwnsOutputConfigMap is false for run A on purpose (unlike run B
		// below): it models an object — the staging ConfigMap seeded for
		// run A below — that sits at exactly the name run A's own naming
		// formula computes, but that run A does not own. Assertion 5's
		// ownership subtest needs exactly this shape, since name-scoping
		// alone (assertion 1) cannot explain that object surviving run A's
		// Cleanup. Run B keeps OwnsOutputConfigMap true so the "owned and
		// recorded" path stays covered too (assertion 4).
		OwnsOutputConfigMap: false,
		DiscoverNetwork:     false,
	})
	dB := NewDeployer(clientset, Config{
		Namespace:           concurrencyTestNamespace,
		Image:               "aicr:test",
		RunID:               concurrencyRunIDB,
		Output:              fmt.Sprintf("cm://%s/%s", concurrencyTestNamespace, nameWithRunID(staticStagingConfigMapName, concurrencyRunIDB)),
		OwnsOutputConfigMap: true,
		DiscoverNetwork:     true,
	})

	// Deploy both runs concurrently from separate goroutines — the exact
	// overlap issue #2120 reported as unsafe.
	var wg sync.WaitGroup
	deployErrs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); deployErrs[0] = dA.Deploy(ctx) }()
	go func() { defer wg.Done(); deployErrs[1] = dB.Deploy(ctx) }()
	wg.Wait()
	if deployErrs[0] != nil {
		t.Fatalf("run A Deploy() error = %v", deployErrs[0])
	}
	if deployErrs[1] != nil {
		t.Fatalf("run B Deploy() error = %v", deployErrs[1])
	}

	// The staging ConfigMap is written by the in-pod agent, not Deploy();
	// seed it directly for both runs so the seventh object kind exists and
	// GetSnapshot has something run-specific to read back.
	seedStagingConfigMap(t, ctx, clientset, dA, "cm-uid-a", "snapshot-bytes-for-run-A")
	seedStagingConfigMap(t, ctx, clientset, dB, "cm-uid-b", "snapshot-bytes-for-run-B")

	// --- Assertion 1: all seven object kinds exist twice, under distinct names.
	t.Run("all seven kinds exist twice under distinct names", func(t *testing.T) {
		assertSevenKindsExist(t, ctx, clientset, dA)
		assertSevenKindsExist(t, ctx, clientset, dB)

		namesA := []string{dA.saName(), dA.roleName(), dA.clusterRoleName(), dA.jobName(), dA.stagingConfigMapName()}
		namesB := []string{dB.saName(), dB.roleName(), dB.clusterRoleName(), dB.jobName(), dB.stagingConfigMapName()}
		for i := range namesA {
			if namesA[i] == namesB[i] {
				t.Errorf("run A and run B share object name %q; runs are not name-isolated", namesA[i])
			}
		}
	})

	// --- Assertion 2: DiscoverNetwork's extra ClusterRole rules do not leak across runs.
	t.Run("ClusterRole rules are isolated per run's DiscoverNetwork setting", func(t *testing.T) {
		crA, err := clientset.RbacV1().ClusterRoles().Get(ctx, dA.clusterRoleName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("run A ClusterRole not found: %v", err)
		}
		crB, err := clientset.RbacV1().ClusterRoles().Get(ctx, dB.clusterRoleName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("run B ClusterRole not found: %v", err)
		}

		if clusterRoleHasRule(crA.Rules, "pods/exec", "create") {
			t.Error("non-discovery run A's ClusterRole grants pods/exec create; expected none")
		}
		if clusterRoleHasRule(crA.Rules, "nodes", "patch") {
			t.Error("non-discovery run A's ClusterRole grants nodes patch; expected none")
		}
		if !clusterRoleHasRule(crB.Rules, "pods/exec", "create") {
			t.Error("discovery run B's ClusterRole is missing pods/exec create")
		}
		if !clusterRoleHasRule(crB.Rules, "nodes", "patch") {
			t.Error("discovery run B's ClusterRole is missing nodes patch")
		}
	})

	// Seed Pods: the fake clientset runs no Job controller, so Pods never
	// appear on their own. podA/podB get an explicit, earlier
	// CreationTimestamp than the imposter below: pickLivePod prefers the
	// youngest candidate, so an imposter that merely passed the label
	// filter (without also failing ownedByJob) would beat podA on
	// recency — making the ownership check load-bearing for this
	// assertion rather than incidentally masked by Pod name sort order.
	podA := seedRunPodAt(t, ctx, clientset, dA, "agent-pod-a", concurrencyPodTimeEarly)
	podB := seedRunPodAt(t, ctx, clientset, dB, "agent-pod-b", concurrencyPodTimeEarly)
	// Imposter: carries run A's RunID label (so it passes the label
	// selector dA.findPodName uses — List DOES honor selectors against the
	// fake clientset, see the top-of-function note), is strictly younger
	// than podA, but its controlling ownerReference points at run B's Job.
	// Proves pickLivePod's ownedByJob check, not the (forgeable) RunID
	// label or Pod recency, is what authorizes selection.
	seedImposterPod(t, ctx, clientset, dA, dB, "agent-pod-imposter", concurrencyPodTimeLate)

	// --- Assertion 3: each run's pod selection returns only its own pod.
	t.Run("pod selection returns only the run's own pod", func(t *testing.T) {
		gotA, err := dA.findPodName(ctx)
		if err != nil {
			t.Fatalf("run A findPodName() error = %v", err)
		}
		if gotA != podA.Name {
			t.Errorf("run A findPodName() = %q, want %q (imposter or run B pod leaked through)", gotA, podA.Name)
		}

		gotB, err := dB.findPodName(ctx)
		if err != nil {
			t.Fatalf("run B findPodName() error = %v", err)
		}
		if gotB != podB.Name {
			t.Errorf("run B findPodName() = %q, want %q", gotB, podB.Name)
		}
	})

	// --- Assertion 4: GetSnapshot returns each run's own staging ConfigMap bytes.
	t.Run("GetSnapshot returns each run's own bytes", func(t *testing.T) {
		gotA, err := dA.GetSnapshot(ctx)
		if err != nil {
			t.Fatalf("run A GetSnapshot() error = %v", err)
		}
		if string(gotA) != "snapshot-bytes-for-run-A" {
			t.Errorf("run A GetSnapshot() = %q, want %q", gotA, "snapshot-bytes-for-run-A")
		}

		gotB, err := dB.GetSnapshot(ctx)
		if err != nil {
			t.Fatalf("run B GetSnapshot() error = %v", err)
		}
		if string(gotB) != "snapshot-bytes-for-run-B" {
			t.Errorf("run B GetSnapshot() = %q, want %q", gotB, "snapshot-bytes-for-run-B")
		}
	})

	// --- Assertion 5: run A's Cleanup must not touch any of run B's objects.
	t.Run("run A Cleanup leaves every run B object intact", func(t *testing.T) {
		if err := dA.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
			t.Fatalf("run A Cleanup() error = %v", err)
		}

		assertSevenKindsExist(t, ctx, clientset, dB)

		// Confirm run A's own Job is actually gone, so this isn't a
		// Cleanup that vacuously "succeeds" without deleting anything.
		if _, err := clientset.BatchV1().Jobs(concurrencyTestNamespace).Get(ctx, dA.jobName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("run A's Job should be deleted by its own Cleanup, err = %v", err)
		}
	})

	// --- Assertion 5 (staging-ConfigMap ownership gate): run A's Cleanup
	// must not touch a staging-named ConfigMap this run does not own, even
	// when that name is exactly what run A's own naming formula computes.
	//
	// This is deliberately a separate subtest from the one above, which
	// compares run A against run B and so is satisfied by name-scoping
	// alone (assertion 1 already proves the two runs compute different
	// names). Here the names collide, so only Config.OwnsOutputConfigMap —
	// false for run A, which is why getSnapshotFromConfigMap in assertion 4
	// did not record the ConfigMap and why the name-based sweep does not
	// fire either — can explain the object surviving.
	//
	// Scope note: this pins the OWNERSHIP GATE, not created-set scoping as
	// such. A hypothetical Cleanup that recomputed its delete list from
	// d.stagingConfigMapName() but kept the same OwnsOutputConfigMap gate
	// would pass this subtest unchanged. The tests that actually
	// discriminate created-set scoping are TestCleanupPassesUIDPrecondition
	// (every delete carries the UID from its Create response — a value no
	// name-derived delete list can supply) and
	// TestCleanupResolvesUnconfirmedEntryBeforeDeleting, both in
	// deployer_test.go. The duplicate-RunID case, where name scoping cannot
	// help because both runs compute the same names, is
	// TestCleanupDuplicateRunIDKeepsFirstRunsStagingConfigMap.
	t.Run("run A Cleanup leaves its own unowned staging ConfigMap intact", func(t *testing.T) {
		if _, err := clientset.CoreV1().ConfigMaps(concurrencyTestNamespace).Get(ctx, dA.stagingConfigMapName(), metav1.GetOptions{}); err != nil {
			t.Errorf("the ConfigMap at run A's staging name %q should survive run A's Cleanup (OwnsOutputConfigMap is false, so it is not run A's to delete), err = %v", dA.stagingConfigMapName(), err)
		}
	})
}

// assertSevenKindsExist verifies each of the seven run-owned object kinds a
// Deployer creates (ServiceAccount, Role, RoleBinding, ClusterRole,
// ClusterRoleBinding, Job, and the staging ConfigMap — see kindServiceAccount
// et al. in types.go) exists under d's run-scoped names.
func assertSevenKindsExist(t *testing.T, ctx context.Context, clientset kubernetes.Interface, d *Deployer) {
	t.Helper()
	ns := d.config.Namespace

	if _, err := clientset.CoreV1().ServiceAccounts(ns).Get(ctx, d.saName(), metav1.GetOptions{}); err != nil {
		t.Errorf("ServiceAccount %q not found: %v", d.saName(), err)
	}
	if _, err := clientset.RbacV1().Roles(ns).Get(ctx, d.roleName(), metav1.GetOptions{}); err != nil {
		t.Errorf("Role %q not found: %v", d.roleName(), err)
	}
	if _, err := clientset.RbacV1().RoleBindings(ns).Get(ctx, d.roleName(), metav1.GetOptions{}); err != nil {
		t.Errorf("RoleBinding %q not found: %v", d.roleName(), err)
	}
	if _, err := clientset.RbacV1().ClusterRoles().Get(ctx, d.clusterRoleName(), metav1.GetOptions{}); err != nil {
		t.Errorf("ClusterRole %q not found: %v", d.clusterRoleName(), err)
	}
	if _, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, d.clusterRoleName(), metav1.GetOptions{}); err != nil {
		t.Errorf("ClusterRoleBinding %q not found: %v", d.clusterRoleName(), err)
	}
	if _, err := clientset.BatchV1().Jobs(ns).Get(ctx, d.jobName(), metav1.GetOptions{}); err != nil {
		t.Errorf("Job %q not found: %v", d.jobName(), err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(ns).Get(ctx, d.stagingConfigMapName(), metav1.GetOptions{}); err != nil {
		t.Errorf("staging ConfigMap %q not found: %v", d.stagingConfigMapName(), err)
	}
}

// seedStagingConfigMap creates d's staging ConfigMap directly, standing in
// for the in-pod agent write that Deploy() itself never performs against
// the fake clientset.
//
// The labels below are d's own set for convenience only. The real in-pod
// writer (pkg/serializer's ConfigMap writer) stamps a different, smaller set —
// no managed-by and no run-ID label — because it also produces the user's
// delivered cm:// artifact. Nothing here selects on these labels: run scoping
// for this object comes from its name.
func seedStagingConfigMap(t *testing.T, ctx context.Context, clientset kubernetes.Interface, d *Deployer, uid, snapshotYAML string) {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.stagingConfigMapName(),
			Namespace: d.config.Namespace,
			UID:       types.UID(uid),
			Labels:    d.objectLabels(),
		},
		Data: map[string]string{"snapshot.yaml": snapshotYAML},
	}
	if _, err := clientset.CoreV1().ConfigMaps(d.config.Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed staging ConfigMap %q: %v", d.stagingConfigMapName(), err)
	}
}

// seedRunPodAt creates the agent Pod for d's Job, since the fake clientset
// runs no Job controller to create it automatically. The Pod carries d's
// full label set (RunID included), createdAt as its CreationTimestamp, and
// a controlling ownerReference to d's Job, mirroring what a real Job
// controller produces.
func seedRunPodAt(t *testing.T, ctx context.Context, clientset kubernetes.Interface, d *Deployer, podName string, createdAt metav1.Time) *corev1.Pod {
	t.Helper()
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName,
			Namespace:         d.config.Namespace,
			Labels:            d.objectLabels(),
			CreationTimestamp: createdAt,
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       kindJob,
					Name:       d.jobName(),
					UID:        d.jobUID(),
					Controller: &controller,
				},
			},
		},
	}
	created, err := clientset.CoreV1().Pods(d.config.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed pod %q: %v", podName, err)
	}
	return created
}

// seedImposterPod creates a Pod labeled as labelOwner's own (so it passes
// labelOwner's List label selector) with createdAt as its
// CreationTimestamp, but controlled by jobOwner's Job — proving pod
// selection is authorized by the controlling ownerReference, not the RunID
// label alone (writable by anything that can update Pods in the namespace)
// or Pod recency.
func seedImposterPod(t *testing.T, ctx context.Context, clientset kubernetes.Interface, labelOwner, jobOwner *Deployer, podName string, createdAt metav1.Time) *corev1.Pod {
	t.Helper()
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName,
			Namespace:         labelOwner.config.Namespace,
			Labels:            labelOwner.objectLabels(),
			CreationTimestamp: createdAt,
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       kindJob,
					Name:       jobOwner.jobName(),
					UID:        jobOwner.jobUID(),
					Controller: &controller,
				},
			},
		},
	}
	created, err := clientset.CoreV1().Pods(labelOwner.config.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed imposter pod %q: %v", podName, err)
	}
	return created
}

// clusterRoleHasRule reports whether rules contains a rule permitting verb
// on resource, mirroring the resource/verb matching TestDeployer_EnsureRBAC's
// discovery subtest uses elsewhere in this package.
func clusterRoleHasRule(rules []rbacv1.PolicyRule, resource, verb string) bool {
	for _, r := range rules {
		for _, res := range r.Resources {
			if res != resource {
				continue
			}
			for _, v := range r.Verbs {
				if v == verb {
					return true
				}
			}
		}
	}
	return false
}
