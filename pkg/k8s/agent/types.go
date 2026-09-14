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
	"github.com/NVIDIA/aicr/pkg/runid"
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
//
// confirmed records whether ownership was established at creation time — a
// Create response came back naming this object. It is the discriminator
// Cleanup keys off, NOT uid == "": ownership is a property of how the entry
// was obtained, and a run must never infer it from a name plus a freshly-read
// UID (a Get followed by a UID-pinned delete proves only that the object did
// not change between the two calls).
//
// An entry stays unconfirmed when recordIntent added it and no Create
// response ever arrived. Cleanup must then re-establish ownership from the
// live object's own labels before deleting it — see resolveIntentUID.
type createdObject struct {
	kind      string
	name      string
	uid       types.UID
	confirmed bool
}

// Config holds the configuration for deploying the agent.
type Config struct {
	Namespace string

	// ServiceAccountName is exact-if-exists, and therefore carries two
	// meanings resolved once per Deploy (see resolveServiceAccount):
	//
	//   - A ServiceAccount of exactly this name already exists in
	//     Namespace: the agent pod runs as it verbatim and this run
	//     creates NO ServiceAccount, Role, RoleBinding, ClusterRole or
	//     ClusterRoleBinding. aicr adds and removes no permissions on an
	//     identity it did not create, and cleanup deletes none of them.
	//     This is what keeps a ServiceAccount carrying IRSA or GKE
	//     Workload Identity annotations usable: both providers pin trust
	//     to the ServiceAccount NAME, which a run-scoped name can never
	//     satisfy. Generate the RBAC that grants such a ServiceAccount
	//     the agent's permissions with
	//     BuildServiceAccountRoleManifests, then apply it out of band.
	//   - Otherwise: a prefix. The run creates "<prefix>-<RunID>" and
	//     the full run-scoped RBAC set, and deletes them at cleanup.
	//
	// Empty falls back to NameBase and is never probed for existence —
	// the fallback base is aicr's own default, not a name the caller
	// asked for, so a stray ServiceAccount sitting at it must not
	// silently capture the run.
	//
	// Exact mode waives per-run permission isolation: concurrent runs
	// sharing the ServiceAccount share its grants, and grants provisioned
	// for DiscoverNetwork persist beyond any one run.
	ServiceAccountName string

	JobName string

	// RunID scopes every resource this Deployer creates to a single run,
	// so concurrent snapshot-agent runs never collide on a shared resource
	// name. Callers generate it with runid.Generate() before deploying.
	//
	// Required, and validated by Deploy before any object is created: it
	// is folded into every run-owned name, so it must be a DNS-1123 label
	// (lowercase alphanumerics and "-", starting and ending alphanumeric,
	// at most 63 characters). Anything else fails with
	// errors.ErrCodeInvalidRequest.
	RunID string

	// NameBase prefixes generated resource names. It applies per name,
	// not all-or-nothing: jobName() falls back to it when JobName is
	// empty, and saName() (which also names the Role and RoleBinding)
	// falls back to it when ServiceAccountName is empty — so setting only
	// one of the two leaves NameBase governing the other. Defaults to
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

	// mu guards created and existingSA. Deploy's ensure* steps run
	// sequentially today, but GetSnapshot (which records the staging
	// ConfigMap) can be invoked from a different goroutine than Deploy,
	// and Cleanup reads the created-set while a caller could still be
	// recording into it, so every access is mutex-guarded.
	mu      sync.Mutex
	created []createdObject

	// invocationID identifies THIS Deployer, and only this one. It is
	// generated in NewDeployer and is reachable through no field of
	// Config, which is the entire point: Config.RunID is caller-settable
	// and deliberately shared (`aicr validate` hands one ID to its
	// live-capture agent and its validator Jobs), so two invocations can
	// stamp identical run labels. This value they cannot share.
	//
	// It is stamped as labels.InvocationID on every object this Deployer
	// creates and is what createdByThisInvocation requires before Cleanup
	// adopts an object whose Create response never arrived. Written once,
	// before the Deployer escapes NewDeployer, and never mutated, so it
	// needs no lock.
	invocationID string

	// existingSA is the exact, operator-named ServiceAccount this run
	// runs as instead of creating its own, or "" in prefix mode. It is
	// resolved once by resolveServiceAccount at the top of Deploy and
	// read afterwards by the Job builder. It shares mu with created
	// rather than carrying its own: the Deployer is reachable from the
	// caller's log-streaming and cleanup goroutines, so a field Deploy
	// writes must not be read unsynchronized from any of them.
	existingSA string
}

// NewDeployer creates a new agent Deployer with the given configuration.
func NewDeployer(clientset kubernetes.Interface, config Config) *Deployer {
	return &Deployer{
		clientset:    clientset,
		config:       config,
		invocationID: runid.Generate(),
	}
}

// objectLabels returns the standard label set applied to every run-owned
// object this Deployer creates: the ServiceAccount, Role, RoleBinding,
// ClusterRole, ClusterRoleBinding, Job, and the Job's pod template. Each
// call returns a fresh map so callers attaching it to two objects (e.g. a
// Job and its pod template) never alias the same underlying map.
func (d *Deployer) objectLabels() map[string]string {
	l := map[string]string{
		labels.Name:      labels.ValueAICR,
		labels.ManagedBy: labels.ValueAICR,
		labels.Component: labels.ValueSnapshotAgent,
		labels.RunID:     d.config.RunID,
	}
	// Omitted rather than stamped empty when this Deployer was built as a
	// struct literal instead of through NewDeployer (in-package tests do
	// that). An empty value is a legal label value that proves nothing, and
	// writing one would let two such Deployers "match" each other;
	// createdByThisInvocation refuses an empty invocation ID for the same
	// reason.
	if d.invocationID != "" {
		l[labels.InvocationID] = d.invocationID
	}
	return l
}

// setExistingServiceAccount records that this run uses the operator's
// already-existing ServiceAccount verbatim rather than creating its own.
// Called once, by resolveServiceAccount. Safe for concurrent use.
func (d *Deployer) setExistingServiceAccount(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.existingSA = name
}

// existingServiceAccount returns the operator-supplied ServiceAccount this
// run adopted verbatim, or "" when the run creates and owns its own
// run-scoped one. Safe for concurrent use.
func (d *Deployer) existingServiceAccount() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.existingSA
}

// managesRBAC reports whether this run creates (and therefore later deletes)
// the ServiceAccount, Role, RoleBinding, ClusterRole and ClusterRoleBinding.
// False in exact-ServiceAccount mode: aicr adds and removes no permissions
// on an identity it did not create, so none of those objects is created,
// none enters the created-set, and Cleanup consequently has nothing of those
// kinds to delete.
func (d *Deployer) managesRBAC() bool {
	return d.existingServiceAccount() == ""
}

// recordIntent enters a run-owned object into the created-set with the zero
// UID, immediately BEFORE its Create call. It closes the window in which a
// committed Create whose response never arrives (client timeout, apiserver
// rollout, LB 502/504, connection reset, context cancellation in the
// response window) leaves an object nothing will ever delete: the ensure*
// call returns a non-AlreadyExists error, Deploy aborts, and the deferred
// Cleanup — enabled by default — would otherwise never learn the object
// exists. Because the name is run-unique, no later run reclaims it either,
// so the orphan is permanent.
//
// The entry is unconfirmed: no Create response ever named the object, so the
// run-scoped name is the only thing tying it to this run, and a name proves
// nothing about who created what currently sits at it. Cleanup therefore
// re-establishes ownership from the live object's labels before deleting it
// (resolveIntentUID) rather than deleting by bare name. An AlreadyExists
// response is the one case that proves the object is NOT ours, so every
// ensure* discards the intent on that branch (see discardIntent).
// Safe for concurrent use.
func (d *Deployer) recordIntent(kind, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.created = append(d.created, createdObject{kind: kind, name: name})
}

// discardIntent drops the unconfirmed entry recordIntent added for (kind,
// name). Called only when a Create returns AlreadyExists: the object at that
// name exists but this run did not create it, so it must not enter this
// run's delete list. Safe for concurrent use.
func (d *Deployer) discardIntent(kind, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.created {
		if d.created[i].kind == kind && d.created[i].name == name && !d.created[i].confirmed {
			d.created = append(d.created[:i], d.created[i+1:]...)
			return
		}
	}
}

// recordCreated confirms a run-owned object in the created-set. Cleanup
// builds its UID-pinned delete list from exactly this set, so every ensure*
// call that successfully creates an object — and GetSnapshot, for the
// staging ConfigMap it observes but does not itself create — must call this
// on success.
//
// It marks the entry confirmed — this is the point at which this run's
// ownership of the object is established, and the only place that flag is
// set.
//
// It upserts: when recordIntent already entered (kind, name) unconfirmed, the
// observed UID is written onto that entry rather than appended as a second
// one, so the set holds one entry per object and jobUID() sees the real Job
// UID. Safe for concurrent use.
func (d *Deployer) recordCreated(kind, name string, uid types.UID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.created {
		if d.created[i].kind == kind && d.created[i].name == name && !d.created[i].confirmed {
			d.created[i].uid = uid
			d.created[i].confirmed = true
			return
		}
	}
	d.created = append(d.created, createdObject{kind: kind, name: name, uid: uid, confirmed: true})
}

// stagingSweepJob returns the CONFIRMED Job entry that licenses Cleanup to
// look for a staging ConfigMap not in the created-set, and false when no sweep
// is warranted. Both halves of the answer are read from the single snapshot
// Cleanup already took, rather than re-entering the mutex: reading the set
// twice would let a recordCreated landing between the two reads produce a
// snapshot that misses the staging ConfigMap while the second read reports it
// present, skipping both the created-set delete and the sweep and leaking the
// object.
//
// Two conditions must hold.
//
// No ConfigMap entry: an entry means getSnapshotFromConfigMap already
// observed the object's UID, so Cleanup deletes it from the created-set and
// the sweep would be a redundant second delete of the same name.
//
// A CONFIRMED Job entry: the staging ConfigMap is written only by this run's
// in-pod agent, so this run cannot have produced one unless the apiserver
// confirmed the Job that runs that agent. Anything sitting at the staging
// name when no confirmed Job exists belongs to someone else — notably a
// caller that reused another run's RunID (Config.RunID is public SDK surface,
// deliberately settable for pinned e2e/chainsaw runs) and failed on its first
// AlreadyExists before recording anything. Sweeping there would delete the
// first run's live staging ConfigMap.
//
// An unconfirmed (recordIntent-only) Job entry deliberately does not qualify.
// It cannot be told apart from a duplicate-RunID collision whose AlreadyExists
// never came back, and the window it forfeits is empty in practice: Deploy
// aborts the moment that Create fails and Cleanup runs seconds later, far
// short of the time the agent needs to collect a snapshot and write it.
//
// The entry is returned rather than a bare bool because a confirmed Job is a
// necessary but not sufficient condition: it says this invocation created a
// Job at that name once, not that the Job standing there now is it. Cleanup
// completes the proof with stillHoldsJob before sweeping anything.
func stagingSweepJob(objs []createdObject) (createdObject, bool) {
	var job createdObject
	var jobConfirmed bool
	for _, o := range objs {
		switch o.kind {
		case kindConfigMap:
			return createdObject{}, false
		case kindJob:
			if o.confirmed && !jobConfirmed {
				job = o
				jobConfirmed = true
			}
		}
	}
	return job, jobConfirmed
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
// Deploy failed before getting there, and while the Job's Create is in
// flight (recordIntent has entered the name but no UID is known yet). Pod
// selection (see ownedByJob in wait.go) authorizes candidates against
// exactly this UID.
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
// kind. Cleanup does NOT use it — it derives the same answer from the single
// snapshot it already took, via containsKind, so the decision cannot straddle
// two lock acquisitions. This remains as the locked accessor for callers
// (tests) that hold no snapshot. Safe for concurrent use.
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
