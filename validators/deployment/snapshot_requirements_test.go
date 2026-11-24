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
	stderrors "errors"
	"strings"
	"testing"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe"
	v1 "aicr.run/pkg/validator/v1"
	"aicr.run/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// snapshotCluster describes the parts of a cluster the snapshot-requirements
// check reads, so each test states only what differs from a ready cluster.
type snapshotCluster struct {
	nodes    []corev1.Node
	operator string // GPU Operator version label; empty means no label
	// operatorImage, when set, is the image of the operator container. With no
	// operator label and no image there is no operator Deployment at all.
	operatorImage string
	namespace     map[string]string
	noNS          bool
	claim         *corev1.PersistentVolumeClaim
	extraNodes    []corev1.Node // non-GPU nodes
}

func snapshotGPUNode(name, kernel, runtimeVersion string, labels map[string]string) corev1.Node {
	all := map[string]string{
		"nvidia.com/gpu.present":       "true",
		"nvidia.com/cuda.driver.major": "580",
	}
	for k, v := range labels {
		all[k] = v
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: all},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			KernelVersion:           kernel,
			ContainerRuntimeVersion: runtimeVersion,
		}},
	}
}

func snapshotClaim(phase corev1.PersistentVolumeClaimPhase, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-pvc", Namespace: "snapshot"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: modes},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
}

// readySnapshotCluster meets every requirement: one GPU node on a new kernel
// under containerd with driver 580, GPU Operator 26.7.2, no MIG, an unrestricted
// snapshot namespace and a bound ReadWriteMany checkpoint claim.
func readySnapshotCluster() snapshotCluster {
	return snapshotCluster{
		nodes:     []corev1.Node{snapshotGPUNode("gpu-1", "6.12.115-140.210.amzn2023.x86_64", "containerd://2.3.7", nil)},
		operator:  "v26.7.2",
		namespace: map[string]string{},
		claim:     snapshotClaim(corev1.ClaimBound, corev1.ReadWriteMany),
	}
}

func (c snapshotCluster) objects() []runtime.Object {
	var objs []runtime.Object
	for i := range c.nodes {
		objs = append(objs, &c.nodes[i])
	}
	for i := range c.extraNodes {
		objs = append(objs, &c.extraNodes[i])
	}
	if c.operator != "" || c.operatorImage != "" {
		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator", Namespace: "gpu-operator"}}
		if c.operator != "" {
			deploy.Labels = map[string]string{"app.kubernetes.io/version": c.operator}
		}
		if c.operatorImage != "" {
			deploy.Spec.Template.Spec.Containers = []corev1.Container{{Name: "gpu-operator", Image: c.operatorImage}}
		}
		objs = append(objs, deploy)
	}
	if !c.noNS {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Labels: c.namespace}})
	}
	if c.claim != nil {
		objs = append(objs, c.claim)
	}
	return objs
}

func snapshotContext(declared bool, objs []runtime.Object) *validators.Context {
	var refs []recipe.ComponentRef
	if declared {
		refs = append(refs, recipe.ComponentRef{Name: "snapshot", Namespace: "snapshot", Type: recipe.ComponentTypeHelm})
	}
	return &validators.Context{
		Ctx:             context.Background(),
		Clientset:       k8sfake.NewClientset(objs...),
		ValidationInput: v1.ToValidationInput(&recipe.RecipeResult{ComponentRefs: refs}),
	}
}

func TestCheckSnapshotRequirements_SkipsWhenSnapshotNotDeclared(t *testing.T) {
	t.Parallel()

	err := checkSnapshotRequirements(snapshotContext(false, readySnapshotCluster().objects()))
	if !validators.IsSkip(err) {
		t.Fatalf("checkSnapshotRequirements() = %v, want a skip: the recipe does not declare snapshot", err)
	}
}

func TestCheckSnapshotRequirements_PassesOnReadyCluster(t *testing.T) {
	t.Parallel()

	if err := checkSnapshotRequirements(snapshotContext(true, readySnapshotCluster().objects())); err != nil {
		t.Fatalf("checkSnapshotRequirements() = %v, want nil on a cluster that meets every requirement", err)
	}
}

func TestCheckSnapshotRequirements_FailsOnEachUnmetRequirement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(c *snapshotCluster)
		want   string
	}{
		{
			name:   "no GPU node",
			mutate: func(c *snapshotCluster) { c.nodes = nil },
			want:   "gpu-nodes",
		},
		{
			name: "kernel older than 5.13",
			mutate: func(c *snapshotCluster) {
				c.nodes = []corev1.Node{snapshotGPUNode("gpu-1", "5.4.0-1045-aws", "containerd://1.7.2", nil)}
			},
			want: "kernel",
		},
		{
			name: "unsupported container runtime",
			mutate: func(c *snapshotCluster) {
				c.nodes = []corev1.Node{snapshotGPUNode("gpu-1", "6.12.1", "docker://24.0.7", nil)}
			},
			want: "container-runtime",
		},
		{
			name:   "GPU Operator older than 26.3",
			mutate: func(c *snapshotCluster) { c.operator = "v25.3.4" },
			want:   "gpu-operator",
		},
		{
			name:   "GPU Operator not found",
			mutate: func(c *snapshotCluster) { c.operator = "" },
			want:   "gpu-operator",
		},
		{
			name: "driver older than 580",
			mutate: func(c *snapshotCluster) {
				c.nodes = []corev1.Node{snapshotGPUNode("gpu-1", "6.12.1", "containerd://2.3.7", map[string]string{"nvidia.com/cuda.driver.major": "575"})}
			},
			want: "driver",
		},
		{
			name: "driver version not reported",
			mutate: func(c *snapshotCluster) {
				n := snapshotGPUNode("gpu-1", "6.12.1", "containerd://2.3.7", nil)
				delete(n.Labels, "nvidia.com/cuda.driver.major")
				c.nodes = []corev1.Node{n}
			},
			want: "driver",
		},
		{
			name: "MIG enabled",
			mutate: func(c *snapshotCluster) {
				c.nodes = []corev1.Node{snapshotGPUNode("gpu-1", "6.12.1", "containerd://2.3.7", map[string]string{"nvidia.com/mig.config": "all-1g.10gb"})}
			},
			want: "mig",
		},
		{
			name: "namespace enforces a restricted Pod Security level",
			mutate: func(c *snapshotCluster) {
				c.namespace = map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}
			},
			want: "privileged-namespace",
		},
		{
			name: "ReadWriteOnce claim with two GPU nodes",
			mutate: func(c *snapshotCluster) {
				c.claim = snapshotClaim(corev1.ClaimBound, corev1.ReadWriteOnce)
				c.nodes = append(c.nodes, snapshotGPUNode("gpu-2", "6.12.115-140.210.amzn2023.x86_64", "containerd://2.3.7", nil))
			},
			want: "checkpoint-store",
		},
		{
			name:   "claim not bound",
			mutate: func(c *snapshotCluster) { c.claim = snapshotClaim(corev1.ClaimPending, corev1.ReadWriteMany) },
			want:   "checkpoint-store",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := readySnapshotCluster()
			tt.mutate(&c)

			err := checkSnapshotRequirements(snapshotContext(true, c.objects()))
			if err == nil {
				t.Fatalf("checkSnapshotRequirements() = nil, want a failure naming %q", tt.want)
			}
			if validators.IsSkip(err) {
				t.Fatalf("checkSnapshotRequirements() skipped, want a failure naming %q: a declared component must not skip", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to name %q", err.Error(), tt.want)
			}
		})
	}
}

// TestCheckSnapshotRequirements_OperatorImageReferences covers how the GPU
// Operator version is read from its image: a tag is a version, a digest alone is
// not, and a version label beats the image.
func TestCheckSnapshotRequirements_OperatorImageReferences(t *testing.T) {
	t.Parallel()

	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name    string
		label   string
		image   string
		wantErr string // "" means the check passes
	}{
		{name: "tag on the image", image: "nvcr.io/nvidia/gpu-operator:v26.7.2"},
		{name: "tag and digest on the image", image: "nvcr.io/nvidia/gpu-operator:v26.7.2@" + digest},
		{name: "label beats a digest-only image", label: "v26.7.2", image: "nvcr.io/nvidia/gpu-operator@" + digest},
		{
			name:    "digest-only image with no label fails closed",
			image:   "nvcr.io/nvidia/gpu-operator@" + digest,
			wantErr: "pinned by digest only",
		},
		{
			name:    "old tag on the image",
			image:   "nvcr.io/nvidia/gpu-operator:v25.3.4",
			wantErr: "Snapshot needs >= v26.3.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := readySnapshotCluster()
			c.operator = tt.label
			c.operatorImage = tt.image

			err := checkSnapshotRequirements(snapshotContext(true, c.objects()))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkSnapshotRequirements() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkSnapshotRequirements() = nil, want a failure containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), gpuOperatorComponent) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to name %s and contain %q", err.Error(), gpuOperatorComponent, tt.wantErr)
			}
		})
	}
}

// TestCheckSnapshotRequirements_ReportsEveryFailure keeps the check from
// stopping at the first unmet requirement: an operator fixing a cluster should
// see all of them in one run.
func TestCheckSnapshotRequirements_ReportsEveryFailure(t *testing.T) {
	t.Parallel()

	c := readySnapshotCluster()
	c.operator = "v25.3.4"
	c.nodes = []corev1.Node{snapshotGPUNode("gpu-1", "5.4.0", "docker://24.0.7", nil)}

	err := checkSnapshotRequirements(snapshotContext(true, c.objects()))
	if err == nil {
		t.Fatal("checkSnapshotRequirements() = nil, want a failure")
	}
	for _, want := range []string{"kernel", "container-runtime", "gpu-operator"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err.Error(), want)
		}
	}
}

// TestCheckSnapshotRequirements_SingleNodeReadWriteOnceIsAccepted covers the
// node-local checkpoint store: a ReadWriteOnce claim works while there is one
// GPU node, because only that node's agent ever mounts it.
func TestCheckSnapshotRequirements_SingleNodeReadWriteOnceIsAccepted(t *testing.T) {
	t.Parallel()

	c := readySnapshotCluster()
	c.claim = snapshotClaim(corev1.ClaimBound, corev1.ReadWriteOnce)

	if err := checkSnapshotRequirements(snapshotContext(true, c.objects())); err != nil {
		t.Fatalf("checkSnapshotRequirements() = %v, want nil for a ReadWriteOnce claim on a single GPU node", err)
	}
}

// TestCheckSnapshotRequirements_AbsentClaimAndNamespaceAreNotFailures: a bundle
// that has not been applied yet, or that names a different claim, leaves nothing
// to verify; the check only fails on evidence that the store cannot work.
func TestCheckSnapshotRequirements_AbsentClaimAndNamespaceAreNotFailures(t *testing.T) {
	t.Parallel()

	c := readySnapshotCluster()
	c.claim = nil
	c.noNS = true

	if err := checkSnapshotRequirements(snapshotContext(true, c.objects())); err != nil {
		t.Fatalf("checkSnapshotRequirements() = %v, want nil when the claim and namespace do not exist yet", err)
	}
}

func TestSnapshotKernelSupported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kernel string
		want   bool
	}{
		{"6.12.115-140.210.amzn2023.x86_64", true},
		{"6.8.0-1021-aws", true},
		{"5.13.0", true},
		{"5.15.0-1030-gke", true},
		{"6.6.78.1-1.azl3", true},  // Azure Linux: four numeric components
		{"5.15.153.1-2.cm2", true}, // CBL-Mariner: four numeric components
		{"5.12.19-300.fc34", false},
		{"5.4.0-1045-aws", false},
		{"4.18.0-513.el8.x86_64", false},
		{"not-a-version", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := snapshotKernelSupported(tt.kernel); got != tt.want {
			t.Errorf("snapshotKernelSupported(%q) = %v, want %v", tt.kernel, got, tt.want)
		}
	}
}

func TestSnapshotRuntimeSupported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		version string
		want    bool
	}{
		{"containerd://2.3.7", true},
		{"containerd://1.7.2+unknown", true},
		{"cri-o://1.30.1", true},
		{"docker://24.0.7", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := snapshotRuntimeSupported(tt.version); got != tt.want {
			t.Errorf("snapshotRuntimeSupported(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

func TestSnapshotMIGEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"no MIG labels", map[string]string{"nvidia.com/gpu.present": "true"}, false},
		{"MIG config all-disabled", map[string]string{"nvidia.com/mig.config": "all-disabled"}, false},
		{"MIG strategy single without a config", map[string]string{"nvidia.com/mig.strategy": "single"}, false},
		{"MIG config selects a profile", map[string]string{"nvidia.com/mig.config": "all-1g.10gb"}, true},
		{"product names a MIG slice", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3-MIG-1g.10gb"}, true},
		{"mixed strategy with MIG profile devices", map[string]string{
			"nvidia.com/gpu.product":         "NVIDIA-H100-80GB-HBM3",
			"nvidia.com/mig.strategy":        "mixed",
			"nvidia.com/mig-1g.10gb.count":   "7",
			"nvidia.com/mig-1g.10gb.product": "NVIDIA-H100-80GB-HBM3-MIG-1g.10gb",
		}, true},
		{"mixed strategy with no MIG devices", map[string]string{"nvidia.com/mig.strategy": "mixed", "nvidia.com/mig-1g.10gb.count": "0"}, false},
		{"a MIG count that is not a number", map[string]string{"nvidia.com/mig-1g.10gb.count": "many"}, false},
		{"a MIG label that is not a count", map[string]string{"nvidia.com/mig-1g.10gb.memory": "9984"}, false},
	}
	for _, tt := range tests {
		if got := snapshotMIGEnabled(tt.labels); got != tt.want {
			t.Errorf("snapshotMIGEnabled(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCheckSnapshotRequirements_DriverLabels(t *testing.T) {
	t.Parallel()

	const current, legacy = "nvidia.com/cuda.driver-version.major", "nvidia.com/cuda.driver.major"
	tests := []struct {
		name    string
		labels  map[string]string // replaces the node's driver labels
		wantErr bool
	}{
		{name: "current label only", labels: map[string]string{current: "580"}},
		{name: "legacy label only", labels: map[string]string{legacy: "580"}},
		{name: "current label wins over a stale legacy one", labels: map[string]string{current: "575", legacy: "580"}, wantErr: true},
		{name: "old driver on the current label", labels: map[string]string{current: "570"}, wantErr: true},
		{name: "neither label", labels: map[string]string{}, wantErr: true},
		{name: "not a number", labels: map[string]string{current: "latest"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := readySnapshotCluster()
			node := snapshotGPUNode("gpu-1", "6.12.115-140.210.amzn2023.x86_64", "containerd://2.3.7", nil)
			delete(node.Labels, legacy)
			for k, v := range tt.labels {
				node.Labels[k] = v
			}
			c.nodes = []corev1.Node{node}

			err := checkSnapshotRequirements(snapshotContext(true, c.objects()))
			if tt.wantErr != (err != nil) {
				t.Fatalf("checkSnapshotRequirements() = %v, want error=%t", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "driver") {
				t.Errorf("error %q should name the driver requirement", err)
			}
		})
	}
}

// snapshotOperator is the Deployment the chart creates, mounting the checkpoint
// claim as the "checkpoints" volume.
func snapshotOperator(claim string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: snapshotOperatorDeployment, Namespace: "snapshot"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
			{Name: "other", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: snapshotCheckpointVolume, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			}},
		}}}},
	}
}

// TestCheckSnapshotRequirements_ClaimComesFromTheOperator covers a recipe that
// sets storage.pvc.name: once the operator is deployed, the claim it mounts is the
// one that counts, and it must exist.
func TestCheckSnapshotRequirements_ClaimComesFromTheOperator(t *testing.T) {
	t.Parallel()

	custom := func(phase corev1.PersistentVolumeClaimPhase, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
		claim := snapshotClaim(phase, modes...)
		claim.Name = "my-claim"
		return claim
	}
	tests := []struct {
		name    string
		objs    func(c snapshotCluster) []runtime.Object
		wantErr string // "" means the check passes
	}{
		{
			name: "the operator's own claim is bound and ReadWriteMany",
			objs: func(c snapshotCluster) []runtime.Object {
				c.claim = nil
				return append(c.objects(), snapshotOperator("my-claim"), custom(corev1.ClaimBound, corev1.ReadWriteMany))
			},
		},
		{
			name: "the operator's claim does not exist, though the default one does",
			objs: func(c snapshotCluster) []runtime.Object {
				return append(c.objects(), snapshotOperator("my-claim"))
			},
			wantErr: "claim snapshot/my-claim, which the snapshot operator mounts, does not exist",
		},
		{
			name: "the operator's claim is pending",
			objs: func(c snapshotCluster) []runtime.Object {
				c.claim = nil
				return append(c.objects(), snapshotOperator("my-claim"), custom(corev1.ClaimPending, corev1.ReadWriteMany))
			},
			wantErr: "claim snapshot/my-claim is Pending",
		},
		{
			name: "operator deployed with the default claim name and no claim",
			objs: func(c snapshotCluster) []runtime.Object {
				c.claim = nil
				return append(c.objects(), snapshotOperator(snapshotClaimName))
			},
			wantErr: "which the snapshot operator mounts, does not exist",
		},
		{
			name: "before the install a missing claim is not a failure",
			objs: func(c snapshotCluster) []runtime.Object {
				c.claim = nil
				return c.objects()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkSnapshotRequirements(snapshotContext(true, tt.objs(readySnapshotCluster())))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkSnapshotRequirements() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkSnapshotRequirements() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestCheckSnapshotRequirements_FailsClosedOnReadErrors: a declared component
// whose prerequisites cannot be read must fail, not pass or skip (#2122).
func TestCheckSnapshotRequirements_FailsClosedOnReadErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resource string
		want     string
	}{
		{name: "nodes cannot be listed", resource: "nodes", want: "nodes"},
		{name: "the claim cannot be read", resource: "persistentvolumeclaims", want: "could not read claim"},
		{name: "the namespace cannot be read", resource: "namespaces", want: "could not read namespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := snapshotContext(true, readySnapshotCluster().objects())
			client, ok := ctx.Clientset.(*k8sfake.Clientset)
			if !ok {
				t.Fatalf("clientset is %T, want the fake", ctx.Clientset)
			}
			client.PrependReactor("*", tt.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, stderrors.New("forbidden: read denied")
			})

			err := checkSnapshotRequirements(ctx)
			if err == nil {
				t.Fatalf("checkSnapshotRequirements() = nil, want a failure when %s", tt.name)
			}
			if validators.IsSkip(err) {
				t.Fatalf("checkSnapshotRequirements() skipped when %s: a declared component must not skip", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should contain %q", err, tt.want)
			}
		})
	}
}

// TestCheckSnapshotRequirements_SingleNodeExceptionIsReadWriteOnceOnly: the
// single-node exception is for a claim one node can write. A ReadOnlyMany or
// ReadWriteOncePod claim does not establish that, and a ReadWriteOnce claim is
// not enough once there is more than one GPU node.
func TestCheckSnapshotRequirements_SingleNodeExceptionIsReadWriteOnceOnly(t *testing.T) {
	t.Parallel()

	twoNodes := []corev1.Node{
		snapshotGPUNode("gpu-1", "6.12.115-140.210.amzn2023.x86_64", "containerd://2.3.7", nil),
		snapshotGPUNode("gpu-2", "6.12.115-140.210.amzn2023.x86_64", "containerd://2.3.7", nil),
	}
	tests := []struct {
		name  string
		nodes []corev1.Node
		modes []corev1.PersistentVolumeAccessMode
		want  string // "" means the check passes
	}{
		{name: "one node, ReadWriteOnce", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		{name: "one node, ReadWriteOnce and ReadOnlyMany", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany, corev1.ReadWriteOnce}},
		{name: "one node, ReadOnlyMany only", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}, want: "needs ReadWriteMany"},
		{name: "one node, ReadWriteOncePod only", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, want: "needs ReadWriteMany"},
		{name: "two nodes, ReadWriteOnce", nodes: twoNodes, modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, want: "the cluster has 2 GPU nodes, whose agents all mount it"},
		{name: "two nodes, ReadWriteMany", nodes: twoNodes, modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := readySnapshotCluster()
			if tt.nodes != nil {
				c.nodes = tt.nodes
			}
			c.claim = snapshotClaim(corev1.ClaimBound, tt.modes...)

			err := checkSnapshotRequirements(snapshotContext(true, c.objects()))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("checkSnapshotRequirements() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("checkSnapshotRequirements() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// pinnableSnapshotOperator is the operator Deployment as the chart renders it: it mounts
// the checkpoint claim, and may be pinned to a node.
func pinnableSnapshotOperator(nodeSelector map[string]string) *appsv1.Deployment {
	labels := map[string]string{"app": "snapshot-operator"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-operator", Namespace: "snapshot"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: nodeSelector,
					Volumes: []corev1.Volume{{
						Name:         "checkpoints",
						VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "snapshot-pvc"}},
					}},
				},
			},
		},
	}
}

// TestCheckSnapshotRequirements_ReadWriteOnceNeedsTheOperatorOnTheGPUNode: the
// operator mounts the claim too, so a ReadWriteOnce claim works only if the
// operator is pinned to the single GPU node or is the cluster's only node.
func TestCheckSnapshotRequirements_ReadWriteOnceNeedsTheOperatorOnTheGPUNode(t *testing.T) {
	t.Parallel()

	cpuNode := snapshotGPUNode("cpu-1", "6.12.115-140.210.amzn2023.x86_64", "containerd://2.3.7", nil)
	cpuNode.Labels = map[string]string{}
	tests := []struct {
		name     string
		withCPU  bool
		operator *appsv1.Deployment
		want     string // "" means the check passes
	}{
		{name: "only node, not installed", want: ""},
		{name: "a second node and no operator yet", withCPU: true, want: "can be scheduled on any of the 2 nodes"},
		{name: "operator pinned to the GPU node", withCPU: true, operator: pinnableSnapshotOperator(map[string]string{"kubernetes.io/hostname": "gpu-1"})},
		{name: "operator pinned to another node", withCPU: true, operator: pinnableSnapshotOperator(map[string]string{"kubernetes.io/hostname": "cpu-1"}), want: "is pinned to kubernetes.io/hostname=cpu-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := readySnapshotCluster()
			c.claim = snapshotClaim(corev1.ClaimBound, corev1.ReadWriteOnce)
			if tt.withCPU {
				c.extraNodes = []corev1.Node{cpuNode}
			}
			objs := c.objects()
			if tt.operator != nil {
				objs = append(objs, tt.operator)
			}
			err := checkSnapshotRequirements(snapshotContext(true, objs))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("checkSnapshotRequirements() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("checkSnapshotRequirements() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// TestCheckSnapshotRequirements_RawBlockClaimIsRejected: Snapshot mounts the
// checkpoint claim as a filesystem, so an explicit Block volume mode fails
// whatever the access mode; an omitted or Filesystem mode is fine.
func TestCheckSnapshotRequirements_RawBlockClaimIsRejected(t *testing.T) {
	t.Parallel()

	block, filesystem := corev1.PersistentVolumeBlock, corev1.PersistentVolumeFilesystem
	tests := []struct {
		name string
		mode *corev1.PersistentVolumeMode
		want string
	}{
		{name: "Block", mode: &block, want: "volumeMode Block"},
		{name: "Filesystem", mode: &filesystem},
		{name: "omitted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := readySnapshotCluster()
			c.claim.Spec.VolumeMode = tt.mode
			err := checkSnapshotRequirements(snapshotContext(true, c.objects()))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("checkSnapshotRequirements() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("checkSnapshotRequirements() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// TestCheckSnapshotRequirements_NodeListFailureStillReportsIndependentRequirements:
// when the GPU nodes cannot be listed, the GPU Operator, the claim and the
// namespace are still evaluated and reported, so one run lists what it can.
func TestCheckSnapshotRequirements_NodeListFailureStillReportsIndependentRequirements(t *testing.T) {
	t.Parallel()

	c := readySnapshotCluster()
	c.operator = "v25.3.4" // below the floor, independent of the nodes
	c.namespace = map[string]string{snapshotPodSecurityLabel: "baseline"}
	ctx := snapshotContext(true, c.objects())
	client, ok := ctx.Clientset.(*k8sfake.Clientset)
	if !ok {
		t.Fatalf("clientset is %T, want the fake", ctx.Clientset)
	}
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("nodes"), "x", stderrors.New("RBAC: access denied"))
	})

	err := checkSnapshotRequirements(ctx)
	if err == nil {
		t.Fatal("checkSnapshotRequirements() = nil, want a failure")
	}
	for _, want := range []string{"gpu-nodes: could not list GPU nodes", "gpu-operator: GPU Operator v25.3.4", "privileged-namespace: "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
	if got := errorstest.ReportedCode(err); got != errors.ErrCodeUnauthorized {
		t.Errorf("error code = %s, want %s", got, errors.ErrCodeUnauthorized)
	}
}

// admissionProbe records the pod creates a context's clientset receives and
// answers them without storing anything, so a test sees exactly what the check
// asked the API server to admit.
type admissionProbe struct {
	creates []k8stesting.CreateActionImpl
}

func (p *admissionProbe) install(t *testing.T, ctx *validators.Context, denial error) {
	t.Helper()
	client, ok := ctx.Clientset.(*k8sfake.Clientset)
	if !ok {
		t.Fatalf("clientset is %T, want the fake", ctx.Clientset)
	}
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, isCreate := action.(k8stesting.CreateActionImpl)
		if !isCreate {
			t.Fatalf("action is %T, want a create", action)
		}
		p.creates = append(p.creates, create)
		return true, create.GetObject(), denial
	})
}

func hasOnePrivilegedContainer(pod *corev1.Pod) bool {
	if len(pod.Spec.Containers) != 1 {
		return false
	}
	sc := pod.Spec.Containers[0].SecurityContext

	return sc != nil && sc.Privileged != nil && *sc.Privileged
}

// TestCheckSnapshotRequirements_NamespaceWithoutAPodSecurityLabelIsAskedToAdmit:
// with no enforce label the cluster's Pod Security default applies, so the check
// asks the API server, as a dry run, rather than reading the absence as consent.
func TestCheckSnapshotRequirements_NamespaceWithoutAPodSecurityLabelIsAskedToAdmit(t *testing.T) {
	t.Parallel()

	denial := apierrors.NewForbidden(corev1.Resource("pods"), "aicr-snapshot-admission-probe-",
		stderrors.New(`violates PodSecurity "restricted:latest": privileged (container "probe" must not set securityContext.privileged=true)`))
	tests := []struct {
		name    string
		labels  map[string]string
		denial  error
		wantOK  bool
		wantAsk bool   // whether the API server is asked at all
		want    string // error text, when the check fails
	}{
		{name: "no label, privileged pod admitted", labels: map[string]string{}, wantOK: true, wantAsk: true},
		{name: "no label, a restricted default refuses it", labels: map[string]string{}, denial: denial, wantAsk: true, want: `violates PodSecurity "restricted:latest"`},
		{name: "enforce privileged is not asked", labels: map[string]string{snapshotPodSecurityLabel: "privileged"}, wantOK: true},
		{name: "enforce baseline fails without asking", labels: map[string]string{snapshotPodSecurityLabel: "baseline"}, want: `enforces the "baseline" Pod Security level`},
		{name: "enforce restricted fails without asking", labels: map[string]string{snapshotPodSecurityLabel: "restricted"}, want: `enforces the "restricted" Pod Security level`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := readySnapshotCluster()
			c.namespace = tt.labels
			ctx := snapshotContext(true, c.objects())
			probe := &admissionProbe{}
			probe.install(t, ctx, tt.denial)

			err := checkSnapshotRequirements(ctx)
			switch {
			case tt.wantOK && err != nil:
				t.Fatalf("checkSnapshotRequirements() = %v, want nil", err)
			case !tt.wantOK && (err == nil || !strings.Contains(err.Error(), "privileged-namespace: ") || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("checkSnapshotRequirements() = %v, want a privileged-namespace failure containing %q", err, tt.want)
			}
			if asked := len(probe.creates) > 0; asked != tt.wantAsk {
				t.Fatalf("API server asked to admit a pod = %v, want %v", asked, tt.wantAsk)
			}
			if !tt.wantAsk {
				return
			}
			if len(probe.creates) != 1 {
				t.Fatalf("pod creates = %d, want 1", len(probe.creates))
			}
			create := probe.creates[0]
			if got := create.CreateOptions.DryRun; len(got) != 1 || got[0] != metav1.DryRunAll {
				t.Errorf("create options dry run = %v, want [%s]: the probe pod must never be stored", got, metav1.DryRunAll)
			}
			pod, ok := create.GetObject().(*corev1.Pod)
			if !ok || create.GetNamespace() != "snapshot" {
				t.Fatalf("probe is %T in namespace %q, want a pod in the snapshot namespace", create.GetObject(), create.GetNamespace())
			}
			if !hasOnePrivilegedContainer(pod) {
				t.Errorf("probe pod = %+v, want one privileged container, the shape of Snapshot's node agent", pod.Spec)
			}
			if !pod.Spec.HostPID || !pod.Spec.HostIPC || !pod.Spec.HostNetwork {
				t.Errorf("probe pod hostPID/hostIPC/hostNetwork = %v/%v/%v, want all true like the node agent",
					pod.Spec.HostPID, pod.Spec.HostIPC, pod.Spec.HostNetwork)
			}
			hostPaths := map[string]bool{}
			for _, v := range pod.Spec.Volumes {
				if v.HostPath != nil {
					hostPaths[v.HostPath.Path] = true
				}
			}
			for _, want := range snapshotAgentHostPaths {
				if !hostPaths[want] {
					t.Errorf("probe pod has no hostPath volume for %s, which the node agent mounts", want)
				}
			}
			res := pod.Spec.Containers[0].Resources
			if res.Requests.Cpu().IsZero() || res.Requests.Memory().IsZero() || res.Limits.Cpu().IsZero() || res.Limits.Memory().IsZero() {
				t.Errorf("probe resources = %+v, want cpu and memory requests and limits so a ResourceQuota does not refuse the dry run", res)
			}
		})
	}
}

// TestCheckSnapshotRequirements_ForbiddenReadsAreUnauthorized: a denied read of a
// declared prerequisite names RBAC (UNAUTHORIZED), not an internal fault, and a
// timeout stays a timeout. The code survives the aggregation over all the
// requirements.
func TestCheckSnapshotRequirements_ForbiddenReadsAreUnauthorized(t *testing.T) {
	t.Parallel()

	forbidden := func(resource string) error {
		return apierrors.NewForbidden(corev1.Resource(resource), "x", stderrors.New("RBAC: access denied"))
	}
	tests := []struct {
		name     string
		resource string
		err      error
		want     errors.ErrorCode
	}{
		{name: "nodes forbidden", resource: "nodes", err: forbidden("nodes"), want: errors.ErrCodeUnauthorized},
		{name: "claim forbidden", resource: "persistentvolumeclaims", err: forbidden("persistentvolumeclaims"), want: errors.ErrCodeUnauthorized},
		{name: "namespace forbidden", resource: "namespaces", err: forbidden("namespaces"), want: errors.ErrCodeUnauthorized},
		{name: "nodes unauthorized", resource: "nodes", err: apierrors.NewUnauthorized("token expired"), want: errors.ErrCodeUnauthorized},
		{name: "claim read times out", resource: "persistentvolumeclaims", err: apierrors.NewTimeoutError("slow", 1), want: errors.ErrCodeTimeout},
		{name: "claim read fails", resource: "persistentvolumeclaims", err: apierrors.NewInternalError(stderrors.New("boom")), want: errors.ErrCodeInternal},
		// A failed read of the operator Deployment leaves the claim in use unknown;
		// the default claim, which may be bound ReadWriteMany, must not stand in.
		{name: "operator deployment forbidden", resource: "deployments", err: forbidden("deployments"), want: errors.ErrCodeUnauthorized},
		{name: "operator deployment read times out", resource: "deployments", err: apierrors.NewTimeoutError("slow", 1), want: errors.ErrCodeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := snapshotContext(true, readySnapshotCluster().objects())
			client, ok := ctx.Clientset.(*k8sfake.Clientset)
			if !ok {
				t.Fatalf("clientset is %T, want the fake", ctx.Clientset)
			}
			client.PrependReactor("*", tt.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.err
			})

			err := checkSnapshotRequirements(ctx)
			if err == nil {
				t.Fatal("checkSnapshotRequirements() = nil, want a failure when a read is refused")
			}
			if got := errorstest.ReportedCode(err); got != tt.want {
				t.Errorf("error code = %s, want %s (error: %v)", got, tt.want, err)
			}
		})
	}
}

// TestCheckSnapshotRequirements_AdmissionDryRunTimeoutIsNotAPolicyDenial: a dry run
// that times out received no admission decision, so the failure carries TIMEOUT and
// does not tell the user to relabel the namespace.
func TestCheckSnapshotRequirements_AdmissionDryRunTimeoutIsNotAPolicyDenial(t *testing.T) {
	t.Parallel()

	c := readySnapshotCluster()
	c.namespace = map[string]string{}
	ctx := snapshotContext(true, c.objects())
	probe := &admissionProbe{}
	probe.install(t, ctx, apierrors.NewTimeoutError("slow", 1))

	err := checkSnapshotRequirements(ctx)
	if err == nil {
		t.Fatal("checkSnapshotRequirements() = nil, want a failure when admission cannot be evaluated")
	}
	if got := errorstest.ReportedCode(err); got != errors.ErrCodeTimeout {
		t.Errorf("error code = %s, want %s (error: %v)", got, errors.ErrCodeTimeout, err)
	}
	if !strings.Contains(err.Error(), "admission could not be evaluated") {
		t.Errorf("error %q should say admission could not be evaluated", err)
	}
	if strings.Contains(err.Error(), "label the namespace") {
		t.Errorf("error %q must not advise relabeling the namespace when no policy decision was received", err)
	}
}

// TestSnapshotFailureCode: a denied read wins over a timeout, which wins over a
// plain unmet requirement.
func TestSnapshotFailureCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		failed []snapshotRequirement
		want   errors.ErrorCode
	}{
		{name: "unmet requirements only", failed: []snapshotRequirement{{name: "kernel"}}, want: errors.ErrCodeInternal},
		{name: "a timeout", failed: []snapshotRequirement{{name: "kernel"}, {name: "claim", code: errors.ErrCodeTimeout}}, want: errors.ErrCodeTimeout},
		{name: "a denial after a timeout", failed: []snapshotRequirement{{code: errors.ErrCodeTimeout}, {code: errors.ErrCodeUnauthorized}}, want: errors.ErrCodeUnauthorized},
		{name: "a denial before a timeout", failed: []snapshotRequirement{{code: errors.ErrCodeUnauthorized}, {code: errors.ErrCodeTimeout}}, want: errors.ErrCodeUnauthorized},
	}
	for _, tt := range tests {
		if got := snapshotFailureCode(tt.failed); got != tt.want {
			t.Errorf("%s: snapshotFailureCode() = %s, want %s", tt.name, got, tt.want)
		}
	}
}

// TestCheckSnapshotRequirements_ReadWriteOnceComparesTheHostnameLabel: the
// operator's nodeSelector names a kubernetes.io/hostname value, which need not
// equal the node name.
func TestCheckSnapshotRequirements_ReadWriteOnceComparesTheHostnameLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		selector string
		want     string
	}{
		{name: "selector matches the hostname label", selector: "gpu-1"},
		{name: "selector names the node, not its label", selector: "gpu-1.example", want: "is pinned to kubernetes.io/hostname=gpu-1.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := readySnapshotCluster()
			c.claim = snapshotClaim(corev1.ClaimBound, corev1.ReadWriteOnce)
			c.nodes = []corev1.Node{snapshotGPUNode("gpu-1.example", "6.12.115-140.210.amzn2023.x86_64", "containerd://2.3.7",
				map[string]string{"kubernetes.io/hostname": "gpu-1"})}
			cpu := snapshotGPUNode("cpu-1", "6.12.115-140.210.amzn2023.x86_64", "containerd://2.3.7", nil)
			cpu.Labels = map[string]string{}
			c.extraNodes = []corev1.Node{cpu}
			objs := append(c.objects(), pinnableSnapshotOperator(map[string]string{"kubernetes.io/hostname": tt.selector}))

			err := checkSnapshotRequirements(snapshotContext(true, objs))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("checkSnapshotRequirements() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("checkSnapshotRequirements() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// TestCheckSnapshotRequirements_AdmissionProbeRunsAsTheAgentServiceAccount: a
// policy may admit privileged pods for the agent's service account only, so the
// probe uses the deployed agent's account, and the namespace default before the
// install.
func TestCheckSnapshotRequirements_AdmissionProbeRunsAsTheAgentServiceAccount(t *testing.T) {
	t.Parallel()

	agent := func(sa string) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "snapshot-agent", Namespace: "snapshot"},
			Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: sa}}},
		}
	}
	tests := []struct {
		name    string
		objects []runtime.Object
		wantSA  string
	}{
		{name: "agent deployed", objects: []runtime.Object{agent("snapshot-agent-sa")}, wantSA: "snapshot-agent-sa"},
		{name: "not installed", wantSA: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := readySnapshotCluster()
			c.namespace = map[string]string{}
			ctx := snapshotContext(true, append(c.objects(), tt.objects...))
			probe := &admissionProbe{}
			probe.install(t, ctx, nil)

			if err := checkSnapshotRequirements(ctx); err != nil {
				t.Fatalf("checkSnapshotRequirements() = %v, want nil", err)
			}
			if len(probe.creates) != 1 {
				t.Fatalf("probe creates = %d, want 1", len(probe.creates))
			}
			pod, ok := probe.creates[0].GetObject().(*corev1.Pod)
			if !ok {
				t.Fatalf("probe object is %T, want a pod", probe.creates[0].GetObject())
			}
			if got := pod.Spec.ServiceAccountName; got != tt.wantSA {
				t.Errorf("probe service account = %q, want %q", got, tt.wantSA)
			}
		})
	}
}

// TestCheckSnapshotRequirements_AgentDaemonSetReadFailureIsNotHidden: when the
// agent's service account cannot be read, the probe must not silently run as
// "default"; the requirement fails with the read's code.
func TestCheckSnapshotRequirements_AgentDaemonSetReadFailureIsNotHidden(t *testing.T) {
	t.Parallel()

	c := readySnapshotCluster()
	c.namespace = map[string]string{}
	ctx := snapshotContext(true, c.objects())
	client, ok := ctx.Clientset.(*k8sfake.Clientset)
	if !ok {
		t.Fatalf("clientset is %T, want the fake", ctx.Clientset)
	}
	client.PrependReactor("list", "daemonsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("daemonsets"), "x", stderrors.New("RBAC: access denied"))
	})
	probe := &admissionProbe{}
	probe.install(t, ctx, nil)

	err := checkSnapshotRequirements(ctx)
	if err == nil || !strings.Contains(err.Error(), "could not read the snapshot agent DaemonSet") {
		t.Fatalf("checkSnapshotRequirements() = %v, want a failure naming the DaemonSet read", err)
	}
	if got := errorstest.ReportedCode(err); got != errors.ErrCodeUnauthorized {
		t.Errorf("error code = %s, want %s", got, errors.ErrCodeUnauthorized)
	}
	if len(probe.creates) != 0 {
		t.Errorf("probe creates = %d, want 0: the probe must not run as an unverified account", len(probe.creates))
	}
}
