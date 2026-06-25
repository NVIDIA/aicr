// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	sigsyaml "sigs.k8s.io/yaml"
)

// kindInfo describes one Kind the stray-CRs check enumerates.
// ClusterScoped=true means the check lists cluster-wide; otherwise it
// lists only in Inputs.OperatorNamespace.
type kindInfo struct {
	GVK           schema.GroupVersionKind
	ClusterScoped bool
}

// managedKinds is the hardcoded set of CR Kinds the Network Operator
// chart manages and that `l8k generate` may render. Listed exhaustively
// (not derived from the rendered manifests) so the check catches strays
// even for Kinds the current profile doesn't render — e.g. an old
// SriovNetwork left behind when migrating from a sriov profile to a
// host_device profile.
//
// Operator-created service CRs (NicDevice, SriovNetworkNodeState,
// SriovOperatorConfig) are deliberately excluded — they are created by
// the operator itself per node / per install and have no l8k-rendered
// counterpart to compare against.
var managedKinds = []kindInfo{
	// Cluster-scoped — singletons or per-group resources.
	{GVK: gvk("mellanox.com", "v1alpha1", "NicClusterPolicy"), ClusterScoped: true},
	{GVK: gvk("mellanox.com", "v1alpha1", "NicNodePolicy"), ClusterScoped: true},
	{GVK: gvk("configuration.net.nvidia.com", "v1alpha1", "NicConfigurationTemplate"), ClusterScoped: true},
	{GVK: gvk("configuration.net.nvidia.com", "v1alpha1", "NicInterfaceNameTemplate"), ClusterScoped: true},

	// Namespaced — enumerated in the operator namespace.
	{GVK: gvk("sriovnetwork.openshift.io", "v1", "SriovNetworkNodePolicy")},
	{GVK: gvk("sriovnetwork.openshift.io", "v1", "SriovNetwork")},
	{GVK: gvk("sriovnetwork.openshift.io", "v1", "SriovIBNetwork")},
	{GVK: gvk("sriovnetwork.openshift.io", "v1", "SriovNetworkPoolConfig")},
	{GVK: gvk("sriovnetwork.openshift.io", "v1", "OVSNetwork")},
	{GVK: gvk("nv-ipam.nvidia.com", "v1alpha1", "IPPool")},
	{GVK: gvk("nv-ipam.nvidia.com", "v1alpha1", "CIDRPool")},
	{GVK: gvk("mellanox.com", "v1alpha1", "MacvlanNetwork")},
	{GVK: gvk("mellanox.com", "v1alpha1", "IPoIBNetwork")},
	{GVK: gvk("mellanox.com", "v1alpha1", "HostDeviceNetwork")},
	{GVK: gvk("spectrumx.nvidia.com", "v1alpha1", "SpectrumXRailPoolConfig")},
	{GVK: gvk("spectrumx.nvidia.com", "v1alpha2", "SpectrumXRailPoolConfig")},
}

func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

// CheckStrayCRs detects Network-Operator-managed CRs in the operator
// namespace (or cluster-wide for cluster-scoped Kinds) that l8k did
// not render — i.e. a previously-deployed configuration still in the
// cluster that conflicts with what `l8k generate` just produced.
// Each conflict surfaces as a Mismatch the `--overwrite-existing`
// remediation will delete.
//
// Soft-skipped when Inputs.KubeClient is nil — without a client we
// can't enumerate. Inputs.GeneratedManifests may be empty (which means
// "we rendered nothing" — every existing managed CR is a conflict); a
// nil slice is treated the same as an empty list.
//
// CRDs that aren't installed in the cluster (older Network Operator
// releases that don't ship a given CRD) are silently skipped — list
// errors on a missing CRD don't fail the check.
func CheckStrayCRs(ctx context.Context, in Inputs) Result {
	r := Result{
		Name: "Conflicting Network Operator resources",
		Code: CodeStrayCRs,
	}
	if in.KubeClient == nil {
		r.Skipped = true
		r.Reason = "no kube client available for conflict check"
		return r
	}

	namespace := in.OperatorNamespace
	if namespace == "" {
		r.Skipped = true
		r.Reason = "no operator namespace supplied; cannot enumerate namespaced CRs"
		return r
	}

	expected := expectedRefSet(in.GeneratedManifests)

	for _, kind := range managedKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   kind.GVK.Group,
			Version: kind.GVK.Version,
			Kind:    kind.GVK.Kind + "List",
		})
		opts := []client.ListOption{}
		if !kind.ClusterScoped {
			opts = append(opts, client.InNamespace(namespace))
		}
		if err := in.KubeClient.List(ctx, list, opts...); err != nil {
			// Tolerate "no matches for kind ...List" — the cluster
			// may not have this CRD (older operator release). Log
			// once at debug and move on.
			log.Log.V(1).Info("stray-CR list skipped",
				"kind", kind.GVK.Kind, "scope", scopeLabel(kind), "error", err.Error())
			continue
		}
		for i := range list.Items {
			obj := &list.Items[i]
			ref := ObjectRef{
				GVK:       kind.GVK,
				Namespace: obj.GetNamespace(),
				Name:      obj.GetName(),
			}
			if _, ok := expected[refKey(ref)]; ok {
				continue
			}
			// Expected and Actual are intentionally nil: this
			// check is a presence test, not a value diff —
			// Mismatch.String() formats it as "Path (Detail)"
			// instead of `expected=… actual=…` noise.
			r.Mismatches = append(r.Mismatches, Mismatch{
				Path:   strayPath(ref),
				Detail: fmt.Sprintf("%s/%s; %s", kind.GVK.GroupVersion(), kind.GVK.Kind, scopeLabel(kind)),
			})
		}
	}

	// Deterministic ordering for golden tests and stable HTML reports.
	sort.Slice(r.Mismatches, func(i, j int) bool {
		return r.Mismatches[i].Path < r.Mismatches[j].Path
	})

	if len(r.Mismatches) == 0 {
		r.Reason = fmt.Sprintf("no conflicting Network Operator resources in %s", namespace)
	} else {
		r.Reason = fmt.Sprintf("%d existing Network Operator resource(s) in %s conflict with the rendered manifests",
			len(r.Mismatches), namespace)
	}
	return r
}

// refKey serialises an ObjectRef to a deterministic string identifier
// used as the map key inside expectedRefSet.
func refKey(r ObjectRef) string {
	return fmt.Sprintf("%s|%s|%s|%s", r.GVK.GroupVersion().String(), r.GVK.Kind, r.Namespace, r.Name)
}

// expectedRefSet builds a lookup map from a flat slice of refs. Used to
// O(1) check "is this CR something l8k rendered?".
func expectedRefSet(refs []ObjectRef) map[string]struct{} {
	out := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		out[refKey(r)] = struct{}{}
	}
	return out
}

// strayPath formats an ObjectRef for the Mismatch.Path column —
// cluster-scoped Kinds drop the namespace, namespaced ones include it.
func strayPath(r ObjectRef) string {
	if r.Namespace == "" {
		return fmt.Sprintf("%s/%s", r.GVK.Kind, r.Name)
	}
	return fmt.Sprintf("%s/%s/%s", r.GVK.Kind, r.Namespace, r.Name)
}

func scopeLabel(k kindInfo) string {
	if k.ClusterScoped {
		return "cluster-scoped"
	}
	return "namespaced"
}

// ScanGeneratedManifests walks the deployment directory and returns one
// ObjectRef per K8s manifest document found. Skips workload-example
// manifests and the helm values.yaml — same filter the deploy phase
// uses. The caller passes the result as Inputs.GeneratedManifests.
//
// Errors when manifestsDir is unreadable; on a per-file decode failure
// the file is logged and skipped (so a single broken doc doesn't kill
// the whole stray check).
func ScanGeneratedManifests(manifestsDir string) ([]ObjectRef, error) {
	if manifestsDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(manifestsDir)
	if err != nil {
		return nil, fmt.Errorf("read deployment dir %s: %w", manifestsDir, err)
	}
	var out []ObjectRef
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := filepath.Ext(name)
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		if name == "values.yaml" {
			// helm values, not a K8s manifest
			continue
		}
		if strings.Contains(strings.ToLower(name), "example") {
			// example workload manifest — not part of the
			// network-operator surface
			continue
		}
		path := filepath.Join(manifestsDir, name)
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			log.Log.V(1).Info("ScanGeneratedManifests: read failed", "file", path, "error", rerr.Error())
			continue
		}
		for _, doc := range splitYAMLDocs(string(content)) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := sigsyaml.Unmarshal([]byte(doc), obj); err != nil {
				log.Log.V(1).Info("ScanGeneratedManifests: decode failed", "file", path, "error", err.Error())
				continue
			}
			if obj.GetKind() == "" {
				continue
			}
			gv, perr := schema.ParseGroupVersion(obj.GetAPIVersion())
			if perr != nil {
				log.Log.V(1).Info("ScanGeneratedManifests: bad apiVersion", "file", path, "apiVersion", obj.GetAPIVersion())
				continue
			}
			out = append(out, ObjectRef{
				GVK:       gv.WithKind(obj.GetKind()),
				Namespace: obj.GetNamespace(),
				Name:      obj.GetName(),
			})
		}
	}
	return out, nil
}

// splitYAMLDocs splits a multi-document YAML string on `---` separators.
// Inlined here (rather than importing the existing one in
// pkg/networkoperatorplugin) to keep preflight a leaf package with no
// cycle back into networkoperatorplugin.
func splitYAMLDocs(s string) []string {
	var docs []string
	var cur []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "---") {
			if len(cur) > 0 {
				docs = append(docs, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, ln)
	}
	if len(cur) > 0 {
		docs = append(docs, strings.Join(cur, "\n"))
	}
	return docs
}
