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
	"sync"

	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Standard Kubernetes recommended labels applied to all agent-managed
// resources. Centralized here so selectors and resource templates stay in sync.
const (
	labelAppName      = "app.kubernetes.io/name"
	labelAppManagedBy = "app.kubernetes.io/managed-by"
	appName           = "aicr"
)

// Kind labels recorded in Deployer.created for each run-owned object type.
// Cleanup dispatches on these to call the matching resource-specific delete.
const (
	kindServiceAccount     = "ServiceAccount"
	kindRole               = "Role"
	kindRoleBinding        = "RoleBinding"
	kindClusterRole        = "ClusterRole"
	kindClusterRoleBinding = "ClusterRoleBinding"
	kindJob                = "Job"
	kindConfigMap          = "ConfigMap"
)

// createdObject records one object this Deployer created (or, for the
// staging ConfigMap it does not itself create, observed itself owning) so
// Cleanup can delete exactly this set instead of deriving a delete list from
// configured names. name is the run-scoped name the object was created
// under; uid pins the eventual delete via metav1.Preconditions so a
// same-named object belonging to a different run is never collected.
type createdObject struct {
	kind string
	name string
	uid  types.UID
}

// Config holds the configuration for deploying the agent.
type Config struct {
	Namespace          string
	ServiceAccountName string
	JobName            string

	// RunID scopes every resource this Deployer creates to a single run,
	// so concurrent snapshot-agent runs never collide on a shared resource
	// name. Callers generate it with runid.Generate() before deploying.
	RunID string

	// NameBase prefixes generated resource names only — it has no effect
	// when ServiceAccountName or JobName is already set. Defaults to
	// "aicr" when empty.
	NameBase string

	Image            string
	ImagePullSecrets []string
	NodeSelector     map[string]string
	Tolerations      []corev1.Toleration
	Output           string
	Debug            bool
	Privileged       bool   // If true, run with privileged security context (required for GPU/SystemD collectors)
	RequireGPU       bool   // If true, request nvidia.com/gpu resource (required for CDI environments)
	RuntimeClassName string // If set, use this runtimeClassName on the pod and inject NVIDIA_VISIBLE_DEVICES=all (alternative to RequireGPU)
	MaxNodesPerEntry int    // Max node names per topology entry (0 = unlimited)
	OS               string // Recipe OS criteria value. When set to oskind.Talos, systemd hostPath mounts are skipped and the in-pod agent uses the Talos service backend.

	// ClusterConfigPath, when set, forwards to the in-pod network
	// collector via AICR_CLUSTER_CONFIG_PATH so it ingests an existing
	// l8k cluster-config.yaml. The path must resolve inside the pod —
	// today's Job mode does NOT auto-mount the caller's host file, so
	// the snapshotter's deployAndWaitForResult rejects a Job-mode call
	// with ClusterConfigPath set (returns ErrCodeInvalidRequest).
	// ConfigMap-backed mounting is tracked as a follow-up; until then
	// file ingestion is local-mode-only (developer runs the CLI with
	// AICR_AGENT_MODE=true), and Job mode is best used with
	// DiscoverNetwork for live cluster discovery.
	ClusterConfigPath string

	// DiscoverNetwork, when true, forwards via AICR_DISCOVER_NETWORK to
	// enable the in-pod network collector's live l8k discovery path.
	// Discovery is NOT read-only — it patches NicClusterPolicy and writes
	// nvidia.kubernetes-launch-kit.* node labels.
	DiscoverNetwork bool

	// Requests overrides the per-resource container requests on the agent pod.
	// When nil, the privileged/restricted defaults in job.go are used. Keys
	// must match standard Kubernetes resource names (cpu, memory,
	// ephemeral-storage); unknown keys are passed through unchanged.
	Requests corev1.ResourceList

	// Limits overrides the per-resource container limits on the agent pod.
	// When nil, the privileged/restricted defaults in job.go are used.
	// RequireGPU adds nvidia.com/gpu=1 to the merged limits ONLY when the
	// caller did not already supply that key — so a caller can request
	// e.g. nvidia.com/gpu=4 alongside RequireGPU and keep their value.
	Limits corev1.ResourceList

	// OwnsOutputConfigMap is true when Output names the staging ConfigMap
	// this Deployer's own Job writes (the default run-scoped
	// `cm://<namespace>/<generated-name>` URI), rather than a ConfigMap
	// the caller supplied out of band via a hand-written `cm://` Output
	// URI. GetSnapshot enters the ConfigMap into the created-set for
	// Cleanup only when this is true — a caller-supplied ConfigMap is
	// the caller's artifact and must never be deleted by this Deployer.
	OwnsOutputConfigMap bool
}

// Deployer manages the deployment and lifecycle of the agent Job.
type Deployer struct {
	clientset kubernetes.Interface
	config    Config

	// mu guards created. Deploy's ensure* steps run sequentially today,
	// but GetSnapshot (which records the staging ConfigMap) can be
	// invoked from a different goroutine than Deploy, and Cleanup reads
	// the created-set while a caller could still be recording into it,
	// so every access is mutex-guarded.
	mu      sync.Mutex
	created []createdObject
}

// NewDeployer creates a new agent Deployer with the given configuration.
func NewDeployer(clientset kubernetes.Interface, config Config) *Deployer {
	return &Deployer{
		clientset: clientset,
		config:    config,
	}
}

// objectLabels returns the standard label set applied to every run-owned
// object this Deployer creates: the ServiceAccount, Role, RoleBinding,
// ClusterRole, ClusterRoleBinding, Job, and the Job's pod template. Each
// call returns a fresh map so callers attaching it to two objects (e.g. a
// Job and its pod template) never alias the same underlying map.
func (d *Deployer) objectLabels() map[string]string {
	return map[string]string{
		labels.Name:      labels.ValueAICR,
		labels.ManagedBy: labels.ValueAICR,
		labels.Component: labels.ValueSnapshotAgent,
		labels.RunID:     d.config.RunID,
	}
}

// recordCreated appends a run-owned object to the created-set. Cleanup
// builds its UID-pinned delete list from exactly this set, so every ensure*
// call that successfully creates an object — and GetSnapshot, for the
// staging ConfigMap it observes but does not itself create — must call this
// on success. Safe for concurrent use.
func (d *Deployer) recordCreated(kind, name string, uid types.UID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.created = append(d.created, createdObject{kind: kind, name: name, uid: uid})
}

// createdSnapshot returns a defensive copy of the created-set taken under
// lock. Callers must not read d.created directly.
func (d *Deployer) createdSnapshot() []createdObject {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]createdObject, len(d.created))
	copy(out, d.created)
	return out
}

// jobUID returns the UID of the Job this Deployer created, or the zero UID
// if Deploy has not (yet) reached the Job-create step — including when
// Deploy failed before getting there. Pod selection (see ownedByJob in
// wait.go) authorizes candidates against exactly this UID.
func (d *Deployer) jobUID() types.UID {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.created {
		if c.kind == kindJob {
			return c.uid
		}
	}
	return ""
}

// hasCreated reports whether the created-set already holds an object of
// kind. Cleanup uses it to decide whether the staging ConfigMap still needs
// a name-based sweep (the run failed before getSnapshotFromConfigMap could
// observe its UID) or was already recorded. Safe for concurrent use.
func (d *Deployer) hasCreated(kind string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.created {
		if c.kind == kind {
			return true
		}
	}
	return false
}

// CleanupOptions controls what resources to remove during cleanup.
type CleanupOptions struct {
	Enabled bool // If true, removes Job and all RBAC resources
}
