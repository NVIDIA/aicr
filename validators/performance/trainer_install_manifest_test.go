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
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// backdateTrainerInstallManifest rewrites the persisted manifest's
// CreationTimestamp so reapOrphanedTrainerInstall treats it as abandoned
// rather than possibly mid-install.
func backdateTrainerInstallManifest(t *testing.T, client kubernetes.Interface) {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps(trainerNamespace).Get(context.Background(), trainerInstallManifestName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to fetch the manifest to backdate it: %v", err)
	}
	cm.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * defaults.NCCLExecutionLockStaleAge))
	if _, err := client.CoreV1().ConfigMaps(trainerNamespace).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to backdate the manifest: %v", err)
	}
}

// TestTrainerInstallManifest_RoundTrips checks that a persisted manifest can
// be read back with the same resource list, and that deleting it leaves
// nothing behind.
func TestTrainerInstallManifest_RoundTrips(t *testing.T) {
	client := fake.NewClientset()
	resources := []trainerResourceRef{
		{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespace: trainerNamespace, Name: trainerControllerDeployment},
	}

	persistTrainerInstallManifest(context.Background(), client, resources)

	_, got, err := loadTrainerInstallManifest(context.Background(), client)
	if err != nil {
		t.Fatalf("loadTrainerInstallManifest() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != trainerControllerDeployment {
		t.Errorf("loadTrainerInstallManifest() = %v, want the persisted resource back", got)
	}

	deleteTrainerInstallManifest(context.Background(), client)
	cm, _, err := loadTrainerInstallManifest(context.Background(), client)
	if err != nil {
		t.Fatalf("loadTrainerInstallManifest() after delete error = %v", err)
	}
	if cm != nil {
		t.Error("expected the manifest to be gone after deleteTrainerInstallManifest")
	}
}

// TestLoadTrainerInstallManifest_MissingIsNil checks that a namespace with no
// manifest reports nothing tracked instead of an error.
func TestLoadTrainerInstallManifest_MissingIsNil(t *testing.T) {
	cm, resources, err := loadTrainerInstallManifest(context.Background(), fake.NewClientset())
	if err != nil {
		t.Fatalf("loadTrainerInstallManifest() error = %v", err)
	}
	if cm != nil || resources != nil {
		t.Errorf("loadTrainerInstallManifest() = (%v, %v), want (nil, nil)", cm, resources)
	}
}

// TestLoadTrainerInstallManifest_MissingDataKeyIsEmpty checks that a
// ConfigMap present without the resources key reports an empty list instead
// of a decode error, so it does not permanently block orphan reaping.
func TestLoadTrainerInstallManifest_MissingDataKeyIsEmpty(t *testing.T) {
	client := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: trainerInstallManifestName, Namespace: trainerNamespace,
	}})

	cm, resources, err := loadTrainerInstallManifest(context.Background(), client)
	if err != nil {
		t.Fatalf("loadTrainerInstallManifest() error = %v", err)
	}
	if cm == nil || resources != nil {
		t.Errorf("loadTrainerInstallManifest() = (%v, %v), want (non-nil, nil)", cm, resources)
	}
}

// TestMergeTrainerResourceRefs_PreservesOrder checks that the merged list
// keeps a's, then b's, first-seen order with a duplicate updated in place.
// deleteTrainer tears down in reverse list order, so a random order here
// could delete a CRD or webhook before its controller.
func TestMergeTrainerResourceRefs_PreservesOrder(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	ref := func(name string) trainerResourceRef {
		return trainerResourceRef{GVR: gvr, Namespace: trainerNamespace, Name: name}
	}

	a := []trainerResourceRef{ref("crd"), ref("webhook"), ref("controller")}
	b := []trainerResourceRef{ref("controller"), ref("service")}

	got := mergeTrainerResourceRefs(a, b)

	want := []string{"crd", "webhook", "controller", "service"}
	if len(got) != len(want) {
		t.Fatalf("mergeTrainerResourceRefs() = %v, want %d entries", got, len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("entry %d = %q, want %q (full: %v)", i, got[i].Name, name, got)
		}
	}
}

// TestPersistTrainerInstallManifest_MergesConcurrentInstall checks that a
// second install's manifest write merges with, rather than overwrites, one
// already persisted by a concurrent installer.
func TestPersistTrainerInstallManifest_MergesConcurrentInstall(t *testing.T) {
	client := fake.NewClientset()
	deploymentGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	first := []trainerResourceRef{{GVR: deploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment}}
	second := []trainerResourceRef{{GVR: deploymentGVR, Namespace: trainerNamespace, Name: jobSetControllerDeployment}}

	persistTrainerInstallManifest(context.Background(), client, first)
	persistTrainerInstallManifest(context.Background(), client, second)

	_, got, err := loadTrainerInstallManifest(context.Background(), client)
	if err != nil {
		t.Fatalf("loadTrainerInstallManifest() error = %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected both installers' resources to be tracked, got %v", got)
	}
}

// TestRemoveTrainerInstallManifestEntries_LeavesConcurrentInstallTracked
// checks that removing one run's own resources from the manifest preserves
// a concurrent installer's entries, rather than wiping the whole manifest.
func TestRemoveTrainerInstallManifestEntries_LeavesConcurrentInstallTracked(t *testing.T) {
	client := fake.NewClientset()
	deploymentGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	ours := []trainerResourceRef{{GVR: deploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment}}
	theirs := []trainerResourceRef{{GVR: deploymentGVR, Namespace: trainerNamespace, Name: jobSetControllerDeployment}}

	persistTrainerInstallManifest(context.Background(), client, ours)
	persistTrainerInstallManifest(context.Background(), client, theirs)

	removeTrainerInstallManifestEntries(context.Background(), client, ours)

	_, got, err := loadTrainerInstallManifest(context.Background(), client)
	if err != nil {
		t.Fatalf("loadTrainerInstallManifest() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != jobSetControllerDeployment {
		t.Errorf("removeTrainerInstallManifestEntries() left %v, want only the other installer's resource", got)
	}
}

// TestRemoveTrainerInstallManifestEntries_DeletesWhenEmpty checks that the
// manifest is deleted once removing a run's resources leaves nothing behind.
func TestRemoveTrainerInstallManifestEntries_DeletesWhenEmpty(t *testing.T) {
	client := fake.NewClientset()
	resources := []trainerResourceRef{
		{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespace: trainerNamespace, Name: trainerControllerDeployment},
	}
	persistTrainerInstallManifest(context.Background(), client, resources)

	removeTrainerInstallManifestEntries(context.Background(), client, resources)

	cm, _, err := loadTrainerInstallManifest(context.Background(), client)
	if err != nil {
		t.Fatalf("loadTrainerInstallManifest() error = %v", err)
	}
	if cm != nil {
		t.Error("expected the manifest to be deleted once empty")
	}
}

// TestRemoveTrainerInstallManifestEntries_MissingIsNoOp checks that removing
// from a manifest that does not exist does not error or create one.
func TestRemoveTrainerInstallManifestEntries_MissingIsNoOp(t *testing.T) {
	client := fake.NewClientset()
	removeTrainerInstallManifestEntries(context.Background(), client, []trainerResourceRef{
		{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespace: trainerNamespace, Name: trainerControllerDeployment},
	})

	cm, _, err := loadTrainerInstallManifest(context.Background(), client)
	if err != nil {
		t.Fatalf("loadTrainerInstallManifest() error = %v", err)
	}
	if cm != nil {
		t.Error("expected no manifest to be created")
	}
}

// TestReapOrphanedTrainerInstall_SkipsWhenOtherNamespacesRemain checks that
// reaping is skipped while another NCCL benchmark namespace might still
// depend on Trainer, even if a manifest is present.
func TestReapOrphanedTrainerInstall_SkipsWhenOtherNamespacesRemain(t *testing.T) {
	client := fake.NewClientset()
	persistTrainerInstallManifest(context.Background(), client, []trainerResourceRef{
		{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespace: trainerNamespace, Name: trainerControllerDeployment},
	})

	reapOrphanedTrainerInstall(context.Background(), client, newTrainerFakeClient(), true)

	if _, got, _ := loadTrainerInstallManifest(context.Background(), client); got == nil {
		t.Error("expected the manifest to survive while other namespaces remain")
	}
}

// TestReapOrphanedTrainerInstall_SkipsFreshManifest checks that a manifest
// younger than NCCLExecutionLockStaleAge is left alone, since it may belong
// to an install still in progress rather than an abandoned one.
func TestReapOrphanedTrainerInstall_SkipsFreshManifest(t *testing.T) {
	client := fake.NewClientset()
	persistTrainerInstallManifest(context.Background(), client, []trainerResourceRef{
		{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespace: trainerNamespace, Name: trainerControllerDeployment},
	})
	// The fake tracker, unlike a real apiserver, does not auto-stamp
	// CreationTimestamp on Create, so it is set explicitly here.
	cm, err := client.CoreV1().ConfigMaps(trainerNamespace).Get(context.Background(), trainerInstallManifestName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to fetch the manifest to stamp it: %v", err)
	}
	cm.CreationTimestamp = metav1.Now()
	if _, err := client.CoreV1().ConfigMaps(trainerNamespace).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to stamp the manifest: %v", err)
	}

	reapOrphanedTrainerInstall(context.Background(), client, newTrainerFakeClient(), false)

	if _, got, _ := loadTrainerInstallManifest(context.Background(), client); got == nil {
		t.Error("expected a fresh manifest to survive reap")
	}
}

// TestReapOrphanedTrainerInstall_ReapsStaleAbandonedInstall checks that a
// stale manifest with no other namespaces around gets its Trainer resources
// deleted and the manifest cleared, closing the leak an abandoned install
// would otherwise leave behind.
func TestReapOrphanedTrainerInstall_ReapsStaleAbandonedInstall(t *testing.T) {
	client := fake.NewClientset()
	deployment := readyTrainerDeployment()
	deployment.SetUID(testNamespaceUID)
	dynamicClient := newTrainerFakeClient(deployment)

	persistTrainerInstallManifest(context.Background(), client, []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment, UID: testNamespaceUID},
	})
	backdateTrainerInstallManifest(t, client)

	reapOrphanedTrainerInstall(context.Background(), client, dynamicClient, false)

	if _, err := dynamicClient.Resource(trainerDeploymentGVR).Namespace(trainerNamespace).
		Get(context.Background(), trainerControllerDeployment, metav1.GetOptions{}); err == nil {
		t.Error("expected the orphaned Trainer Deployment to be deleted")
	}
	if _, got, _ := loadTrainerInstallManifest(context.Background(), client); got != nil {
		t.Error("expected the manifest to be deleted once the install was reaped")
	}
}

// TestTrustworthyTrainerResourceRefs checks which manifest entries are kept
// versus dropped, independent of the reap flow around it.
func TestTrustworthyTrainerResourceRefs(t *testing.T) {
	tests := []struct {
		name string
		ref  trainerResourceRef
		want bool
	}{
		{"namespaced with UID is trusted", trainerResourceRef{
			GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: "a", UID: testNamespaceUID}, true},
		{"cluster-scoped with UID is trusted", trainerResourceRef{
			GVR: trainerCRDGVR, Name: "a", UID: testNamespaceUID}, true},
		{"missing UID is rejected", trainerResourceRef{
			GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: "a"}, false},
		{"namespace outside trainerNamespace is rejected", trainerResourceRef{
			GVR: trainerDeploymentGVR, Namespace: "other", Name: "a", UID: testNamespaceUID}, false},
		{"cluster-scoped kind outside the allowlist is rejected", trainerResourceRef{
			GVR: trainerServiceGVR, Name: "a", UID: testNamespaceUID}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trustworthyTrainerResourceRefs([]trainerResourceRef{tt.ref})
			if (len(got) == 1) != tt.want {
				t.Errorf("trustworthyTrainerResourceRefs(%+v) = %v, want kept=%v", tt.ref, got, tt.want)
			}
		})
	}
}

// TestReapOrphanedTrainerInstall_RejectsEntryWithoutUID checks that a
// manifest entry with no recorded UID is left alone instead of deleted. The
// manifest ConfigMap is writable by anyone with access to trainerNamespace,
// and a delete with no UID precondition would remove whatever currently
// holds that name, not necessarily what this run installed.
func TestReapOrphanedTrainerInstall_RejectsEntryWithoutUID(t *testing.T) {
	client := fake.NewClientset()
	deployment := readyTrainerDeployment()
	deployment.SetUID(testNamespaceUID)
	dynamicClient := newTrainerFakeClient(deployment)

	persistTrainerInstallManifest(context.Background(), client, []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: trainerNamespace, Name: trainerControllerDeployment},
	})
	backdateTrainerInstallManifest(t, client)

	reapOrphanedTrainerInstall(context.Background(), client, dynamicClient, false)

	if _, err := dynamicClient.Resource(trainerDeploymentGVR).Namespace(trainerNamespace).
		Get(context.Background(), trainerControllerDeployment, metav1.GetOptions{}); err != nil {
		t.Errorf("expected the Deployment to survive a UID-less manifest entry: %v", err)
	}
}

// TestReapOrphanedTrainerInstall_RejectsEntryOutsideNamespace checks that a
// manifest entry naming a namespace other than trainerNamespace is left
// alone instead of deleted. A legitimate self-install never creates a
// namespaced resource anywhere else, so such an entry can only be a
// tampered or corrupted manifest.
func TestReapOrphanedTrainerInstall_RejectsEntryOutsideNamespace(t *testing.T) {
	client := fake.NewClientset()
	const otherNamespace = "unrelated-namespace"
	victim := newTestObject("apps/v1", "Deployment", otherNamespace, "someone-elses-deployment")
	victim.SetUID(testNamespaceUID)
	dynamicClient := newTrainerFakeClient(victim)

	persistTrainerInstallManifest(context.Background(), client, []trainerResourceRef{
		{GVR: trainerDeploymentGVR, Namespace: otherNamespace, Name: "someone-elses-deployment", UID: testNamespaceUID},
	})
	backdateTrainerInstallManifest(t, client)

	reapOrphanedTrainerInstall(context.Background(), client, dynamicClient, false)

	if _, err := dynamicClient.Resource(trainerDeploymentGVR).Namespace(otherNamespace).
		Get(context.Background(), "someone-elses-deployment", metav1.GetOptions{}); err != nil {
		t.Errorf("expected the out-of-namespace resource to survive: %v", err)
	}
}

// TestReapOrphanedTrainerInstall_RejectsUntrustedClusterScopedKind checks
// that a manifest entry naming a cluster-scoped kind outside
// trustedClusterScopedTrainerGVRs is left alone instead of deleted. The
// Trainer overlay never produces such a kind at cluster scope, so an entry
// like this can only be a tampered or corrupted manifest.
func TestReapOrphanedTrainerInstall_RejectsUntrustedClusterScopedKind(t *testing.T) {
	client := fake.NewClientset()
	victim := newTestObject("v1", "Service", "", "someone-elses-service")
	victim.SetUID(testNamespaceUID)
	dynamicClient := newTrainerFakeClient(victim)

	persistTrainerInstallManifest(context.Background(), client, []trainerResourceRef{
		{GVR: trainerServiceGVR, Name: "someone-elses-service", UID: testNamespaceUID},
	})
	backdateTrainerInstallManifest(t, client)

	reapOrphanedTrainerInstall(context.Background(), client, dynamicClient, false)

	if _, err := dynamicClient.Resource(trainerServiceGVR).
		Get(context.Background(), "someone-elses-service", metav1.GetOptions{}); err != nil {
		t.Errorf("expected the untrusted cluster-scoped resource to survive: %v", err)
	}
}
