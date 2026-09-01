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
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// Naming of the RBAC objects BuildServiceAccountRoleManifests renders.
//
// The names are deterministic — the same (namespace, ServiceAccount) pair
// always resolves to the same four names — and they cannot collide with a
// run-scoped name.
//
// The non-collision is structural, not probabilistic. Every run-scoped name
// is "<prefix>-<runID>" and every runID ends in 16 lowercase-hex characters
// (runid.Generate emits "<YYYYMMDD>-<HHMMSS>-<16 hex>"). These names end in
// the literal "-rbac", and "r" is not a hex digit, so no run-scoped name can
// ever equal one of them regardless of what prefix a caller supplies.
const (
	provisionedNamePrefix = "aicr-agent-"
	provisionedNameSuffix = "-rbac"
)

// File names of the rendered manifests, one object per file.
//
// The numeric prefixes are not decoration. `kubectl apply -f <dir>/` visits a
// directory in lexical order, so they put each Role ahead of the RoleBinding
// that references it, and they keep the reading order an operator sees when
// they list the directory the same as the order the objects take effect in.
const (
	roleFileName               = "01-role.yaml"
	roleBindingFileName        = "02-rolebinding.yaml"
	clusterRoleFileName        = "03-clusterrole.yaml"
	clusterRoleBindingFileName = "04-clusterrolebinding.yaml"
)

// ManifestOptions selects the ServiceAccount that
// BuildServiceAccountRoleManifests renders RBAC for.
type ManifestOptions struct {
	// Namespace is the namespace of the ServiceAccount, and the namespace
	// the rendered Role and RoleBinding declare. Required.
	Namespace string

	// ServiceAccountName is the name of the ServiceAccount the rendered
	// RoleBinding and ClusterRoleBinding name as their subject. Required.
	//
	// It is NOT verified to exist: rendering contacts no cluster, by
	// design (see BuildServiceAccountRoleManifests). A name that does not
	// resolve produces manifests that grant nothing, which the operator
	// sees when they review the files or when the ServiceAccount they
	// meant to name still cannot snapshot.
	ServiceAccountName string

	// DiscoverNetwork also renders the cluster-scoped MUTATING rules that
	// `aicr snapshot --discover-network` needs — nodes: patch,
	// pods/exec: create, and CRD, namespace, DaemonSet and namespaced-RBAC
	// create/delete (see discoverNetworkClusterRules).
	//
	// The rendered ClusterRole carries an explicit warning header
	// enumerating each mutating rule and the discovery step it exists for,
	// because deciding whether to grant them is the whole reason these
	// manifests are written out instead of applied.
	DiscoverNetwork bool
}

// Manifest is one rendered RBAC object: the bytes to write, the file name to
// write them under, and the object's kind and name so a caller can report
// what it wrote without re-deriving either.
type Manifest struct {
	// FileName is the name of the file within the output directory. It is
	// a bare file name, never a path.
	FileName string

	// Kind is the Kubernetes kind ("Role", "RoleBinding", "ClusterRole",
	// "ClusterRoleBinding").
	Kind string

	// Name is the object's metadata.name.
	Name string

	// Content is the complete file body: a YAML comment header explaining
	// what the object grants and why the agent needs it, followed by the
	// object itself.
	Content []byte
}

// BuildServiceAccountRoleManifests renders the Role, RoleBinding,
// ClusterRole and ClusterRoleBinding that grant the snapshot agent's
// permissions to an operator-supplied ServiceAccount.
//
// It APPLIES NOTHING and contacts no cluster. There is no clientset, no
// ServiceAccount lookup and no permission pre-flight on this path, so it
// works with no kubeconfig and no cluster privileges at all. Applying the
// manifests, and deleting them when the grant is no longer wanted, is the
// operator's decision and the operator's command.
//
// That is deliberate. The rules being granted include, under
// DiscoverNetwork, cluster-scoped mutating permissions (nodes: patch,
// pods/exec: create, CRD create) that outlive any single run. An operator
// consenting to that should be able to read exactly what they are granting
// first, which a command that provisions on their behalf does not allow.
//
// Every rule set comes from namespacedRules and clusterRules — the same
// definitions the run-scoped ensureRole and ensureClusterRole build from —
// so a rendered manifest can never drift from what a run-owned grant
// carries.
//
// The objects are NOT run-scoped: they carry no run-ID label, never enter a
// Deployer's created-set, and no run's Cleanup deletes them. Teardown is
// `kubectl delete -f <dir>/`.
//
// Trade-off the caller must surface to the operator: a shared ServiceAccount
// waives per-run permission isolation. Concurrent runs using it share its
// grants, and a DiscoverNetwork grant leaves mutating cluster permissions in
// place until the operator removes them.
func BuildServiceAccountRoleManifests(opts ManifestOptions) ([]Manifest, error) {
	if strings.TrimSpace(opts.Namespace) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"namespace is required: it is the namespace the rendered Role and RoleBinding declare")
	}
	if strings.TrimSpace(opts.ServiceAccountName) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"ServiceAccount name is required: the rendered bindings need a subject to name")
	}

	// Both halves of each composed name are valid on their own, but their
	// concatenation can exceed the length ceiling. Reject that here, while
	// rendering, rather than leaving the operator to discover it as an
	// opaque apiserver "Invalid value: metadata.name" at apply time.
	roleName := provisionedRoleName(opts.ServiceAccountName)
	clusterRoleName := provisionedClusterRoleName(opts.Namespace, opts.ServiceAccountName)
	for _, name := range []string{roleName, clusterRoleName} {
		problems := validation.IsDNS1123Subdomain(name)
		if len(problems) == 0 {
			continue
		}
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("ServiceAccount %q yields the RBAC object name %q, which is not a valid Kubernetes object name: %s",
				opts.ServiceAccountName, name, strings.Join(problems, "; ")),
			map[string]any{ctxKeyValue: opts.ServiceAccountName, ctxKeyResolvedName: name})
	}

	subjects := []rbacv1.Subject{{
		Kind:      kindServiceAccount,
		Name:      opts.ServiceAccountName,
		Namespace: opts.Namespace,
	}}
	// Each object gets its own ObjectMeta rather than sharing one value:
	// ObjectMeta copies by value but its Labels map does not, and four
	// objects aliasing one map is a mutation hazard for anything that later
	// adjusts labels per object.
	namespacedMeta := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: roleName, Namespace: opts.Namespace, Labels: provisionedLabels()}
	}
	clusterMeta := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: clusterRoleName, Labels: provisionedLabels()}
	}

	objects := []struct {
		fileName string
		kind     string
		name     string
		header   string
		object   any
	}{
		{
			fileName: roleFileName,
			kind:     kindRole,
			name:     roleName,
			header:   roleHeader(roleName, opts.Namespace, opts.ServiceAccountName),
			object: &rbacv1.Role{
				TypeMeta:   rbacTypeMeta(kindRole),
				ObjectMeta: namespacedMeta(),
				Rules:      namespacedRules(),
			},
		},
		{
			fileName: roleBindingFileName,
			kind:     kindRoleBinding,
			name:     roleName,
			header:   roleBindingHeader(roleName, opts.Namespace, opts.ServiceAccountName),
			object: &rbacv1.RoleBinding{
				TypeMeta:   rbacTypeMeta(kindRoleBinding),
				ObjectMeta: namespacedMeta(),
				Subjects:   subjects,
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacAPIGroup, Kind: kindRole, Name: roleName},
			},
		},
		{
			fileName: clusterRoleFileName,
			kind:     kindClusterRole,
			name:     clusterRoleName,
			header:   clusterRoleHeader(clusterRoleName, opts.ServiceAccountName, opts.DiscoverNetwork),
			object: &rbacv1.ClusterRole{
				TypeMeta:   rbacTypeMeta(kindClusterRole),
				ObjectMeta: clusterMeta(),
				Rules:      clusterRules(opts.DiscoverNetwork),
			},
		},
		{
			fileName: clusterRoleBindingFileName,
			kind:     kindClusterRoleBinding,
			name:     clusterRoleName,
			header:   clusterRoleBindingHeader(clusterRoleName, opts.Namespace, opts.ServiceAccountName),
			object: &rbacv1.ClusterRoleBinding{
				TypeMeta:   rbacTypeMeta(kindClusterRoleBinding),
				ObjectMeta: clusterMeta(),
				Subjects:   subjects,
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacAPIGroup, Kind: kindClusterRole, Name: clusterRoleName},
			},
		},
	}

	manifests := make([]Manifest, 0, len(objects))
	for _, o := range objects {
		content, err := renderManifest(o.header, o.object, opts.ServiceAccountName)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, Manifest{
			FileName: o.fileName,
			Kind:     o.kind,
			Name:     o.name,
			Content:  content,
		})
	}
	return manifests, nil
}

// rbacTypeMeta returns the apiVersion/kind pair a standalone manifest needs.
// The typed objects client-go hands back leave TypeMeta empty — the wire
// format carries it out of band — so a file written from one is unusable
// with `kubectl apply` until it is set explicitly.
func rbacTypeMeta(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: kind}
}

// renderManifest assembles one file: the explanatory header, the marshaled
// object, and the shared trailer that states nothing was applied and how to
// apply and revoke.
//
// Marshaling goes through sigs.k8s.io/yaml, which encodes the typed struct
// via encoding/json rather than walking a Go map, so the field order is the
// struct's and two runs over the same inputs produce identical bytes.
func renderManifest(header string, obj any, serviceAccount string) ([]byte, error) {
	body, err := yaml.Marshal(obj)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to render the RBAC manifest", err)
	}
	var buf strings.Builder
	buf.WriteString(header)
	buf.WriteString(manifestTrailer(serviceAccount))
	buf.WriteString("---\n")
	buf.Write(body)
	return []byte(buf.String()), nil
}

// manifestTrailer is the block every rendered file ends its header with. It
// repeats on all four because an operator may open, review, and act on one
// file alone, and the single fact none of them can afford to omit is that
// nothing has been applied yet.
func manifestTrailer(serviceAccount string) string {
	return fmt.Sprintf(`#
# Generated by: aicr snapshot --add-roles-to-service-account %s
#
# NOTHING HAS BEEN APPLIED TO YOUR CLUSTER. aicr wrote these files and
# contacted no cluster to do it. Review them, then grant and revoke yourself:
#
#   kubectl apply  -f <this directory>/     # grant the permissions
#   kubectl delete -f <this directory>/     # revoke them again
#
# No aicr run creates, refreshes, or deletes these objects. They last until
# you delete them.
#
`, serviceAccount)
}

// roleHeader explains the namespaced grant: two rules, both confined to one
// namespace, and what the agent does with each.
func roleHeader(name, namespace, serviceAccount string) string {
	return fmt.Sprintf(`# aicr snapshot agent -- namespaced permissions
#
#   Role/%s
#   in namespace %s
#
# Bound to ServiceAccount %q by %s.
#
# Grants only what the agent Job needs inside this one namespace:
#
#   configmaps: create, get, update, patch
#     The agent runs as a Kubernetes Job and cannot hand its result back to
#     the CLI directly. It stages the snapshot it collected in a ConfigMap
#     in this namespace and the CLI reads that ConfigMap. Confined to this
#     namespace: it grants nothing in any other.
#
#   pods: get, list
#     The agent reads its own pod to learn which node it was scheduled onto,
#     and lists pods when collecting workload state.
#
# Read-mostly and namespace-local: nothing here is cluster-scoped, and
# nothing here can modify a node, a CRD, or any workload.
`, name, namespace, serviceAccount, roleBindingFileName)
}

// roleBindingHeader explains that this file is what makes the Role take
// effect, and states the consequence of the unverified ServiceAccount name:
// Kubernetes accepts a binding to a subject that does not exist and it
// simply grants nothing, so a typo fails silently.
func roleBindingHeader(name, namespace, serviceAccount string) string {
	return fmt.Sprintf(`# aicr snapshot agent -- binds the namespaced permissions
#
#   RoleBinding/%[1]s
#   in namespace %[2]s
#     Role/%[1]s  ->  ServiceAccount %[2]s/%[3]s
#
# Applying this is what actually gives ServiceAccount %[3]q the rules
# in %[4]s. Until it is applied, that Role grants nothing to anyone.
#
# CHECK THE NAME FIRST. aicr rendered these manifests without contacting a
# cluster, so it did not verify that this ServiceAccount exists. Kubernetes
# accepts a binding whose subject does not exist and simply grants nothing,
# so a typo here fails silently rather than loudly:
#
#   kubectl get serviceaccount %[3]s -n %[2]s
#
# The ServiceAccount must be one you created and control -- typically one
# carrying IRSA (eks.amazonaws.com/role-arn) or GKE Workload Identity
# (iam.gke.io/gcp-service-account) annotations. aicr never creates it.
`, name, namespace, serviceAccount, roleFileName)
}

// clusterRoleHeader explains the cluster-scoped grant. Without
// discoverNetwork it is entirely read-only and says so; with it, the
// mutating rules get a rule-by-rule account of the discovery step each one
// exists for, since that is the grant an operator most needs to read before
// consenting to it.
func clusterRoleHeader(name, serviceAccount string, discoverNetwork bool) string {
	header := fmt.Sprintf(`# aicr snapshot agent -- cluster-scoped permissions
#
#   ClusterRole/%s
#   (cluster-scoped -- these rules apply in EVERY namespace)
#
# Bound to ServiceAccount %q by %s.
#
# The baseline rule set below is READ-ONLY -- every verb is get, list or
# watch, and nothing in it creates, patches, or deletes anything:
#
#   nodes: get, list
#     Node inventory: labels, taints, allocatable capacity, kubelet and
#     container-runtime versions, OS image.
#
#   pods: get, list
#     Cluster-wide workload inventory, used to detect which GPU and
#     networking components are already deployed.
#
#   nvidia.com clusterpolicies: get, list
#     GPU Operator ClusterPolicy: the driver, toolkit, and device-plugin
#     configuration currently in effect.
#
#   slinky.slurm.net controllers, nodesets, loginsets, restapis,
#   accountings: list
#     Slurm-on-Kubernetes topology, when Slinky is installed.
#
#   k8s.mariadb.com mariadbs: list
#     The MariaDB instance backing Slurm accounting, when present.
`, name, serviceAccount, clusterRoleBindingFileName)
	if !discoverNetwork {
		return header
	}
	return header + discoverNetworkHeaderSection()
}

// discoverNetworkHeaderSection is the warning block appended to the
// ClusterRole manifest when DiscoverNetwork is set. Each rule is named
// alongside the concrete discovery step that needs it, so an operator
// reading "nodes: patch" can tell from the file why it is there.
func discoverNetworkHeaderSection() string {
	return `#
# ==========================================================================
# WARNING -- --discover-network was requested, so this ClusterRole ALSO
# carries MUTATING, cluster-scoped rules. Read them before you apply it.
# ==========================================================================
#
# Live network discovery (k8s-launch-kit) does not read topology from the
# API. It stands up a probe DaemonSet, execs into it, and writes what it
# learns back onto your cluster. Every mutating rule below maps to one
# concrete step of that flow:
#
#   nodes: patch
#     Writes nvidia.kubernetes-launch-kit.machine and .gpu labels onto every
#     node discovery matches. This modifies YOUR nodes, and the labels
#     remain after the run finishes.
#
#   pods/exec: create
#     Execs into each probe pod to read NIC VPD and link metadata via the
#     in-pod CLI. Note that pods/exec: create at cluster scope permits
#     running commands in ANY pod in the cluster, not only the probe pods.
#
#   namespaces: get, create, delete
#   apps daemonsets: get, list, watch, create, delete
#   serviceaccounts, configmaps: get, create, delete
#   rbac.authorization.k8s.io roles, rolebindings: get, create, delete
#     Discovery creates the nvidia-k8s-launch-kit namespace, deploys the
#     nic-configuration-daemon DaemonSet and its supporting RBAC into it,
#     and deletes the namespace when it is done.
#
#   apiextensions.k8s.io customresourcedefinitions:
#     get, list, create, update, patch
#     Installs the nic-configuration-operator CRDs (NicDevice,
#     NicClusterPolicy) when they are absent. This is a cluster-wide schema
#     change that outlives the run.
#
#   configuration.net.nvidia.com nicdevices: get, list
#     Reads the NicDevice CRs the probe daemon publishes.
#
#   mellanox.com nicclusterpolicies: get, patch
#     Server-side-applies the NicConfigurationOperator section of YOUR
#     existing NicClusterPolicy.
#
# These permissions last as long as the objects do -- they are NOT scoped to
# one run. A run that lets aicr create its own ServiceAccount instead gets
# the same rules for the lifetime of that one run and has them revoked at
# cleanup. If you need discovery only occasionally, prefer that, or keep a
# separate ServiceAccount used only for discovery runs.
`
}

// clusterRoleBindingHeader explains the cluster-scoped binding and warns
// about the one hazard aicr can no longer detect for the operator: the
// generated name is not injective, so an apply can silently retarget an
// existing binding. Reading the file is the check that replaces the cluster
// read the old provisioning path performed.
func clusterRoleBindingHeader(name, namespace, serviceAccount string) string {
	return fmt.Sprintf(`# aicr snapshot agent -- binds the cluster-scoped permissions
#
#   ClusterRoleBinding/%[1]s
#   (cluster-scoped)
#     ClusterRole/%[1]s  ->  ServiceAccount %[2]s/%[3]s
#
# Applying this is what gives ServiceAccount %[3]q the rules in
# %[4]s, across every namespace in the cluster.
#
# The name joins the namespace and the ServiceAccount on ".", which no
# namespace may contain, so no other (namespace, ServiceAccount) pair can
# compose this name.
`, name, namespace, serviceAccount, clusterRoleFileName)
}

// provisionedRoleName returns the Role and RoleBinding name for a
// ServiceAccount. It is injective within a namespace — the name is a pure
// function of the ServiceAccount name, and a ServiceAccount name is unique
// in its namespace — so two ServiceAccounts can never share these objects.
func provisionedRoleName(serviceAccount string) string {
	return provisionedNamePrefix + serviceAccount + provisionedNameSuffix
}

// provisionedClusterRoleName returns the ClusterRole and ClusterRoleBinding
// name for a ServiceAccount. The namespace is part of the name because these
// objects are cluster-scoped and the same ServiceAccount name can exist in
// several namespaces.
//
// The two segments join on "." rather than "-" so the composition is
// injective. A "-" join is not: namespace "a-b" with ServiceAccount "c" and
// namespace "a" with ServiceAccount "b-c" compose the same string, and
// applying the second render over the first would retarget the existing
// ClusterRoleBinding and revoke the other ServiceAccount's cluster
// permissions. Nothing here could detect that, because no cluster is read.
//
// "." is safe and sufficient: a namespace is a DNS-1123 *label*, which cannot
// contain a dot, while a ClusterRole name is a DNS-1123 *subdomain*, which
// can. The first dot therefore always separates namespace from ServiceAccount,
// whatever either contains.
func provisionedClusterRoleName(namespace, serviceAccount string) string {
	return provisionedNamePrefix + namespace + "." + serviceAccount + provisionedNameSuffix
}

// provisionedLabels is the label set stamped on every rendered object.
// It deliberately omits labels.RunID and labels.InvocationID: these objects
// belong to no run and to no invocation, so Deployer.createdByThisInvocation
// can never match one and no run's Cleanup can reclaim it. The component value is what distinguishes them from the
// run-scoped snapshot-agent objects in selectors and sweeps.
func provisionedLabels() map[string]string {
	return map[string]string{
		labels.Name:      labels.ValueAICR,
		labels.ManagedBy: labels.ValueAICR,
		labels.Component: labels.ValueAgentRBAC,
	}
}
