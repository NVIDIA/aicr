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
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/inventory"
	"github.com/NVIDIA/aicr/pkg/k8s"
	k8slabels "github.com/NVIDIA/aicr/pkg/k8s/labels"
	vlabels "github.com/NVIDIA/aicr/pkg/validator/labels"
)

// aicrCRDGroups are the CRD API groups owned by AICR components. A CRD matches
// only when its group is exactly one of these, never by suffix. Other vendors
// also publish *.nvidia.com groups, and deleting a CRD cascades to every CR of
// that kind. Groups an operator applies at runtime from its image, rather than
// shipping in its chart, are listed too.
var aicrCRDGroups = map[string]bool{
	"nfd.k8s-sigs.io":                 true,
	"nvidia.com":                      true,
	"resource.nvidia.com":             true,
	"maintenance.nvidia.com":          true,
	"nv-ipam.nvidia.com":              true,
	"configuration.net.nvidia.com":    true,
	"nvcre.nvidia.com":                true,
	"mellanox.com":                    true,
	"cert-manager.io":                 true,
	"acme.cert-manager.io":            true,
	"skyhook.nvidia.com":              true,
	"nodewright.nvidia.com":           true,
	"nvsentinel.nvidia.com":           true,
	"preflight.nvsentinel.nvidia.com": true,
	"nvsentinel.dgxc.nvidia.com":      true,
	"healthevents.dgxc.nvidia.com":    true,
	"janitor.dgxc.nvidia.com":         true,
	"monitoring.coreos.com":           true,
	"scheduling.run.ai":               true,
	"kai.scheduler":                   true,
	"grove.io":                        true,
	"scheduler.grove.io":              true,
	"agentgateway.dev":                true,
	"apps.nvidia.com":                 true,
	"kueue.x-k8s.io":                  true,
	"jobset.x-k8s.io":                 true,
	"trainer.kubeflow.org":            true,
	"slinky.slurm.net":                true,
}

const (
	driverDaemonSetName = "nvidia-driver-daemonset"
	gpuOperatorRelease  = "gpu-operator"
)

// disableConversionPatch drops a CRD's conversion webhook. The webhook field
// must be cleared, since the API server rejects it unless the strategy is
// Webhook.
var disableConversionPatch = []byte(`{"spec":{"conversion":{"strategy":"None","webhook":null}}}`)

var crdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// finalizerRescueSkip are namespaced resources that never block namespace
// deletion.
var finalizerRescueSkip = map[schema.GroupResource]bool{
	{Resource: "events"}:                                    true,
	{Group: "events.k8s.io", Resource: "events"}:            true,
	{Resource: "pods"}:                                      true,
	{Resource: "endpoints"}:                                 true,
	{Group: "discovery.k8s.io", Resource: "endpointslices"}: true,
}

type cleaner struct {
	opts          options
	log           *slog.Logger
	kubeContext   string
	server        string
	kube          kubernetes.Interface
	disc          discovery.DiscoveryInterface
	dyn           dynamic.Interface
	helm          helmClient // nil when helm is not installed
	namespaces    []string   // sorted
	owners        ownership
	readReleases  releaseReader // nil skips the inventory read
	components    []inventory.Component
	bundle        *bundleScope // nil without --bundle
	namespaceWait time.Duration
}

func (c *cleaner) run(ctx context.Context, confirm prompt) error {
	c.log.Info("target cluster", "context", c.kubeContext, "server", c.server)
	c.reportExclusions()
	c.reportBundle()

	releases, releasesKnown := c.listReleases(ctx)
	if releasesKnown {
		c.readInstalled(ctx)
	}
	driverManaged := c.driverManaged(ctx, releases)
	if driverManaged {
		c.driverGuidance()
	}

	question := fmt.Sprintf("Remove AICR resources from context %q (server %s)?", c.kubeContext, c.server)
	if !c.opts.assumeYes && !c.opts.dryRun && !confirm(ctx, question) {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "cleanup interrupted at the confirmation prompt")
		}
		c.log.Info("cleanup aborted")
		return nil
	}

	c.uninstallReleases(ctx, releases)
	c.removeValidatorArtifacts(ctx)
	var deletedCRDs, deletedNamespaces []string
	if c.opts.keepCRDs {
		c.log.Info("skipping CRD deletion", "flag", "--keep-crds")
		c.warnDeadConversionWebhooks(ctx)
	} else {
		deletedCRDs = c.deleteCRDs(ctx)
	}
	switch {
	case c.opts.keepNamespaces:
		c.log.Info("skipping namespace deletion", "flag", "--keep-namespaces")
	case !releasesKnown:
		c.log.Warn("skipping namespace deletion, since deleting a namespace with no helm release list could orphan releases in it")
	default:
		deletedNamespaces = c.deleteNamespaces(ctx, releases)
	}
	stuck := c.reportStillTerminating(ctx, deletedCRDs, deletedNamespaces)
	if err := ctx.Err(); err != nil {
		return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "cleanup interrupted")
	}

	if stuck {
		c.log.Warn("AICR cleanup finished with resources still terminating, see the warnings above")
	} else {
		c.log.Info("AICR cleanup complete")
	}
	if !c.opts.dryRun {
		c.log.Info("confirm no AICR remnants remain", "check", "kubectl get ns && helm ls -A")
	}
	if driverManaged {
		c.driverGuidance()
	}
	return nil
}

// reportExclusions logs the fences and warns on a half-fenced install.
// Protecting only the namespace still lets CRD deletion cascade to the
// install's CRs, and protecting only the CRDs still deletes the namespace.
func (c *cleaner) reportExclusions() {
	ns, crd := len(c.opts.excludeNS) > 0, len(c.opts.excludeCRD) > 0
	if ns {
		c.log.Info("excluding namespaces from helm uninstall and namespace deletion", "namespaces", c.opts.excludeNS)
	}
	if crd {
		c.log.Info("excluding CRDs", "matching", c.opts.excludeCRD)
	}
	if ns && !crd {
		c.log.Warn("--exclude-ns given without --exclude-crd, so the install's CRDs and CRs are still deleted")
	}
	if crd && !ns {
		c.log.Warn("--exclude-crd given without --exclude-ns, so the install's releases and namespace are still deleted")
	}
}

func (c *cleaner) excludedNS(ns string) bool {
	return slices.Contains(c.opts.excludeNS, ns)
}

// excludedCRD matches by substring, so an owned group such as nvidia.com can
// still be fenced out per install.
func (c *cleaner) excludedCRD(name string) bool {
	return slices.ContainsFunc(c.opts.excludeCRD, func(pat string) bool {
		return strings.Contains(name, pat)
	})
}

func (c *cleaner) owned(ns string) bool {
	_, found := slices.BinarySearch(c.namespaces, ns)
	return found
}

// listReleases reports false when helm is not installed or the listing failed,
// so the caller cannot tell which namespaces hold releases.
func (c *cleaner) listReleases(ctx context.Context) ([]helmRelease, bool) {
	if c.helm == nil {
		c.log.Warn("helm is not installed, skipping helm uninstall")
		return nil, false
	}
	releases, err := c.helm.list(ctx)
	if err != nil {
		c.log.Warn("could not list helm releases, skipping helm uninstall", "error", err)
		return nil, false
	}
	return releases, true
}

// driverManaged reports whether gpu-operator installs the NVIDIA kernel driver
// on this cluster. The nvidia-driver-daemonset is the primary signal, found by
// name in any namespace because Talos recipes move the operator. The release's
// computed driver.enabled value is the fallback. Detection is read-only, runs
// under --dry-run, and fails open.
func (c *cleaner) driverManaged(ctx context.Context, releases []helmRelease) bool {
	ctx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()

	list, err := c.kube.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", driverDaemonSetName).String(),
	})
	if err != nil {
		c.log.Debug("could not list DaemonSets for driver detection", "error", err)
	} else if slices.ContainsFunc(list.Items, func(ds appsv1.DaemonSet) bool { return ds.Name == driverDaemonSetName }) {
		return true
	}
	if c.helm == nil {
		return false
	}
	// Without the DaemonSet, fall back to the release's computed driver.enabled.
	for _, rel := range releases {
		if rel.Name != gpuOperatorRelease {
			continue
		}
		values, err := c.helm.values(ctx, rel)
		if err != nil {
			c.log.Debug("could not read gpu-operator values for driver detection", "error", err)
			return false
		}
		driver, _ := values["driver"].(map[string]any)
		enabled, _ := driver["enabled"].(bool)
		return enabled
	}
	return false
}

// driverGuidance warns that the GPU nodes need a reboot before the next
// install. The reboot is left to the operator because it is cloud-specific.
func (c *cleaner) driverGuidance() {
	c.log.Warn("gpu-operator manages the NVIDIA kernel driver here, so reboot the GPU nodes after cleanup and before reinstalling",
		"reason", "nvidia_uvm can stay wedged mid-unload, and the next install fails driver validation with Init:CrashLoopBackOff",
		"steps", "kubectl cordon each GPU node, reboot its instance, kubectl uncordon it once Ready")
}

func (c *cleaner) uninstallReleases(ctx context.Context, releases []helmRelease) {
	c.log.Info("phase 1: uninstalling AICR helm releases in AICR namespaces")
	for _, rel := range releases {
		if ctx.Err() != nil {
			return
		}
		log := c.log.With("release", rel.Name, "namespace", rel.Namespace)
		owned, managedBy := c.owners.owns(rel)
		switch {
		case c.excludedNS(rel.Namespace):
			log.Info("skipping helm release in excluded namespace")
		case !c.owned(rel.Namespace):
		case !owned:
			log.Info("skipping helm release AICR does not own")
		case managedBy != "":
			log.Warn("skipping helm release a controller manages", "reason", managedBy)
		default:
			log.Info("uninstalling helm release")
			if c.opts.dryRun {
				continue
			}
			if err := c.helm.uninstall(ctx, rel); err != nil {
				log.Warn("helm uninstall failed", "error", err)
			}
		}
	}
}

// deleteObject deletes one object with del, which it skips under --dry-run.
// It ignores NotFound and logs any other error. It reports whether the object
// was deleted or already gone, which is false under --dry-run.
func (c *cleaner) deleteObject(ctx context.Context, resource, namespace, name string, del func(context.Context) error) bool {
	log := c.log.With("resource", resource, "name", name)
	if namespace != "" {
		log = log.With("namespace", namespace)
	}
	log.Info("deleting")
	if c.opts.dryRun {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()
	if err := k8s.IgnoreNotFound(del(ctx)); err != nil {
		log.Warn("delete failed", "error", err)
		return false
	}
	return true
}

// deleteCRDs deletes the CRDs in AICR-owned groups. Phase 1 has already
// uninstalled the operators that serve their conversion webhooks and clear
// their instances' finalizers, so a CRD whose instances live on, such as a
// user's workload in a namespace AICR does not own, would stay Terminating:
// its instances keep their finalizers, and every list of the kind, including
// the namespace controller's, fails on the dead webhook. deleteCRDs therefore
// switches each such CRD to no conversion before deleting it, and then
// rescues the instances of CRDs still Terminating. It returns the CRDs it
// deleted, or nil under --dry-run.
func (c *cleaner) deleteCRDs(ctx context.Context) []string {
	c.log.Info("phase 3: deleting AICR component CRDs")
	listCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	list, err := c.dyn.Resource(crdGVR).List(listCtx, metav1.ListOptions{})
	cancel()
	if err != nil {
		c.log.Warn("could not list CRDs, skipping CRD deletion", "error", err)
		return nil
	}
	var deleted []string
	for _, crd := range list.Items {
		if ctx.Err() != nil {
			return nil
		}
		group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
		if !aicrCRDGroups[group] {
			continue
		}
		name := crd.GetName()
		if c.excludedCRD(name) {
			c.log.Info("skipping excluded CRD", "name", name)
			continue
		}
		disabled := c.disableConversionWebhook(ctx, &crd)
		ok := c.deleteObject(ctx, crdGVR.Resource, "", name, func(ctx context.Context) error {
			return c.dyn.Resource(crdGVR).Delete(ctx, name, metav1.DeleteOptions{})
		})
		if !ok {
			if disabled {
				c.log.Warn("CRD left with its conversion webhook disabled after its delete failed. Re-run cleanup, or reinstall its operator with the CRD replaced",
					"name", name)
			}
			continue
		}
		deleted = append(deleted, name)
	}
	if c.opts.dryRun || len(deleted) == 0 {
		return nil
	}

	select {
	case <-ctx.Done():
		return nil
	case <-time.After(c.namespaceWait):
	}
	for _, name := range deleted {
		if ctx.Err() != nil {
			return nil
		}
		c.rescueCRDInstances(ctx, name)
	}
	return deleted
}

// warnDeadConversionWebhooks warns, for each AICR CRD kept by --keep-crds,
// when its conversion webhook Service is gone, as it is once phase 1 has
// uninstalled the operator. Every list of the kind then fails, so deleting any
// namespace hangs until the operator is reinstalled.
func (c *cleaner) warnDeadConversionWebhooks(ctx context.Context) {
	listCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	list, err := c.dyn.Resource(crdGVR).List(listCtx, metav1.ListOptions{})
	cancel()
	if err != nil {
		c.log.Debug("could not list CRDs to check conversion webhooks", "error", err)
		return
	}
	for _, crd := range list.Items {
		if ctx.Err() != nil {
			return
		}
		group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
		strategy, _, _ := unstructured.NestedString(crd.Object, "spec", "conversion", "strategy")
		if !aicrCRDGroups[group] || strategy != "Webhook" {
			continue
		}
		// Read the fields one by one: the API server defaults the integer port,
		// which NestedStringMap rejects.
		svcName, _, _ := unstructured.NestedString(crd.Object, "spec", "conversion", "webhook", "clientConfig", "service", "name")
		svcNS, _, _ := unstructured.NestedString(crd.Object, "spec", "conversion", "webhook", "clientConfig", "service", "namespace")
		if svcName == "" || svcNS == "" {
			continue
		}
		getCtx, getCancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
		_, err := c.kube.CoreV1().Services(svcNS).Get(getCtx, svcName, metav1.GetOptions{})
		getCancel()
		if !apierrors.IsNotFound(err) {
			continue
		}
		advice := "Reinstall the operator before deleting namespaces, or keep them with --keep-namespaces"
		if c.opts.keepNamespaces {
			advice = "Reinstall the operator before deleting any namespace"
		}
		c.log.Warn("kept CRD converts through a webhook whose Service is gone, so deleting any namespace hangs until its operator is reinstalled. "+advice,
			"name", crd.GetName(), "service", svcNS+"/"+svcName)
	}
}

// reportStillTerminating waits once more, then warns about each CRD and
// namespace this run tried to delete that still exists, naming a namespace's
// deletion failure condition when it has one. A namespace with no deletion
// timestamp was never deleted, for example because RBAC or admission refused
// the delete, and is reported as such. It reports whether any remain.
func (c *cleaner) reportStillTerminating(ctx context.Context, crds, namespaces []string) bool {
	if c.opts.dryRun || len(crds)+len(namespaces) == 0 {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(c.namespaceWait):
	}
	stuck := false
	for _, name := range crds {
		if ctx.Err() != nil {
			return false
		}
		getCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
		_, err := c.dyn.Resource(crdGVR).Get(getCtx, name, metav1.GetOptions{})
		cancel()
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			stuck = true
			c.log.Warn("could not confirm the CRD is gone", "name", name, "error", err)
		default:
			stuck = true
			c.log.Warn("CRD still terminating, so reinstalling its operator fails until it is gone. "+
				"Check its instances for finalizers", "name", name)
		}
	}
	for _, name := range namespaces {
		if ctx.Err() != nil {
			return false
		}
		getCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
		ns, err := c.kube.CoreV1().Namespaces().Get(getCtx, name, metav1.GetOptions{})
		cancel()
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			stuck = true
			c.log.Warn("could not confirm the namespace is gone", "namespace", name, "error", err)
			continue
		}
		stuck = true
		if ns.DeletionTimestamp == nil {
			c.log.Warn("namespace was not deleted, see the delete failure above", "namespace", name)
			continue
		}
		args := []any{"namespace", name, "check", "kubectl get ns " + name + " -o yaml"}
		for _, cond := range ns.Status.Conditions {
			if cond.Status == corev1.ConditionTrue && strings.HasPrefix(string(cond.Type), "NamespaceDeletion") {
				args = append(args, "condition", string(cond.Type), "message", cond.Message)
				break
			}
		}
		c.log.Warn("namespace still terminating", args...)
	}
	return stuck
}

// disableConversionWebhook sets a webhook-converted CRD's conversion strategy
// to None, so its objects can still be listed once the webhook's operator is
// gone. It is called only on CRDs about to be deleted, since a kept CRD would
// keep serving unconverted objects after the operator is reinstalled. It
// reports whether it disabled the webhook, which is false under --dry-run.
func (c *cleaner) disableConversionWebhook(ctx context.Context, crd *unstructured.Unstructured) bool {
	strategy, _, _ := unstructured.NestedString(crd.Object, "spec", "conversion", "strategy")
	if strategy != "Webhook" {
		return false
	}
	log := c.log.With("name", crd.GetName())
	log.Info("disabling conversion webhook before deleting CRD")
	if c.opts.dryRun {
		return false
	}
	patchCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()
	if _, err := c.dyn.Resource(crdGVR).Patch(patchCtx, crd.GetName(), types.MergePatchType, disableConversionPatch, metav1.PatchOptions{}); err != nil {
		log.Warn("could not disable conversion webhook, so listing this kind may fail until the CRD is gone", "error", err)
		return false
	}
	return true
}

// rescueCRDInstances clears the finalizers of the instances of CRD name that
// its deletion has already marked for deletion, if the CRD is still
// Terminating. It leaves instances in excluded namespaces alone.
func (c *cleaner) rescueCRDInstances(ctx context.Context, name string) {
	getCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	crd, err := c.dyn.Resource(crdGVR).Get(getCtx, name, metav1.GetOptions{})
	cancel()
	if err != nil || crd.GetDeletionTimestamp() == nil {
		return
	}
	gvr, ok := crdListGVR(crd)
	if !ok {
		c.log.Warn("CRD stuck terminating with no served version, cannot clear its instances' finalizers", "name", name)
		return
	}
	c.log.Warn("CRD stuck terminating, clearing finalizers of its instances", "name", name)
	listCtx, listCancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	list, err := c.dyn.Resource(gvr).List(listCtx, metav1.ListOptions{})
	listCancel()
	if err != nil {
		c.log.Warn("could not list CRD instances for finalizer rescue", "name", name, "error", err)
		return
	}
	clearFinalizers := []byte(`{"metadata":{"finalizers":[]}}`)
	for _, obj := range list.Items {
		if ctx.Err() != nil {
			return
		}
		if obj.GetDeletionTimestamp() == nil || len(obj.GetFinalizers()) == 0 {
			continue
		}
		ns := obj.GetNamespace()
		log := c.log.With("resource", gvr.Resource, "name", obj.GetName())
		if ns != "" {
			log = log.With("namespace", ns)
		}
		if c.excludedNS(ns) {
			log.Info("skipping finalizer rescue in excluded namespace")
			continue
		}
		patchCtx, patchCancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
		_, err := c.dyn.Resource(gvr).Namespace(ns).Patch(patchCtx, obj.GetName(), types.MergePatchType, clearFinalizers, metav1.PatchOptions{})
		patchCancel()
		if err != nil {
			log.Warn("could not clear finalizers", "error", err)
			continue
		}
		log.Info("cleared finalizers")
	}
}

// crdListGVR returns the resource to list crd's instances through: its storage
// version if served, else its first served version.
func crdListGVR(crd *unstructured.Unstructured) (schema.GroupVersionResource, bool) {
	group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "plural")
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	var chosen string
	for _, v := range versions {
		version, _ := v.(map[string]any)
		name, _ := version["name"].(string)
		served, _ := version["served"].(bool)
		if !served || name == "" {
			continue
		}
		if storage, _ := version["storage"].(bool); storage {
			chosen = name
			break
		}
		if chosen == "" {
			chosen = name
		}
	}
	if chosen == "" || group == "" || plural == "" {
		return schema.GroupVersionResource{}, false
	}
	return schema.GroupVersionResource{Group: group, Version: chosen, Resource: plural}, true
}

// deleteNamespaces deletes the owned and per-run check namespaces, skipping
// excluded ones. It keeps:
//   - a namespace holding any of releases that AICR does not own, since
//     deleting it deletes the release record and leaves the release's
//     cluster-scoped objects with no way to uninstall them
//   - a namespace holding an AICR release a controller manages, which would
//     recreate what was deleted
//   - a sharedNamespaces entry, unless releases has an AICR release there
//     that is not uninstalled
//
// Under --bundle it skips the check namespaces. It returns the namespaces it
// tried to delete, including any whose delete was refused, or nil under
// --dry-run.
func (c *cleaner) deleteNamespaces(ctx context.Context, releases []helmRelease) []string {
	c.log.Info("phase 4: deleting AICR component and check namespaces")
	for _, ns := range slices.Sorted(maps.Keys(notOwnedNamespaces)) {
		c.log.Info("skipping namespace AICR does not own", "namespace", ns, "reason", notOwnedNamespaces[ns])
	}
	foreign := map[string][]string{}
	managed := map[string][]string{}
	installed := map[string]bool{}
	for _, rel := range releases {
		owned, managedBy := c.owners.owns(rel)
		switch {
		case !owned:
			foreign[rel.Namespace] = append(foreign[rel.Namespace], rel.Name)
		case managedBy != "":
			managed[rel.Namespace] = append(managed[rel.Namespace], rel.Name)
		// A history kept by helm uninstall --keep-history says nothing about
		// who owns the namespace now.
		case rel.Status != helmStatusUninstalled:
			installed[rel.Namespace] = true
		}
	}

	candidates := c.namespaces
	// No bundle installs a check namespace.
	if c.bundle == nil {
		candidates = slices.Concat(candidates, c.checkNamespaces(ctx))
	}
	var targets []string
	for _, ns := range candidates {
		switch {
		case c.excludedNS(ns):
			c.log.Info("skipping excluded namespace", "namespace", ns)
		case len(foreign[ns]) > 0:
			c.log.Info("keeping namespace that holds helm releases AICR does not own", "namespace", ns, "releases", foreign[ns])
		case len(managed[ns]) > 0:
			c.log.Info("keeping namespace that holds helm releases a controller manages", "namespace", ns, "releases", managed[ns])
		case sharedNamespaces[ns] != "" && !installed[ns]:
			c.log.Info("keeping shared namespace with no AICR helm release found", "namespace", ns, "reason", sharedNamespaces[ns])
		default:
			targets = append(targets, ns)
		}
	}
	for _, ns := range targets {
		if ctx.Err() != nil {
			return nil
		}
		c.deleteNamespace(ctx, ns)
	}
	if c.opts.dryRun {
		return nil
	}

	select {
	case <-ctx.Done():
		return nil
	case <-time.After(c.namespaceWait):
	}
	for _, ns := range targets {
		if ctx.Err() != nil {
			return nil
		}
		c.rescueNamespace(ctx, ns)
	}
	return targets
}

// checkNamespaces returns the sorted per-run namespaces conformance checks
// left behind. When the list fails it logs a warning and returns nil.
func (c *cleaner) checkNamespaces(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()
	list, err := c.kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.log.Warn("could not list namespaces, so per-run check namespaces were not swept. Re-run cleanup once the API server is reachable",
			"prefix", defaults.GangSchedulingTestNamespacePrefix, "error", err)
		return nil
	}
	var found []string
	for _, ns := range list.Items {
		if strings.HasPrefix(ns.Name, defaults.GangSchedulingTestNamespacePrefix) {
			found = append(found, ns.Name)
		}
	}
	slices.Sort(found)
	return found
}

// deleteNamespace deletes ns without waiting, and does nothing if ns does not
// exist.
func (c *cleaner) deleteNamespace(ctx context.Context, ns string) {
	getCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	_, err := c.kube.CoreV1().Namespaces().Get(getCtx, ns, metav1.GetOptions{})
	cancel()
	if err != nil {
		if !apierrors.IsNotFound(err) {
			c.log.Warn("could not read namespace", "namespace", ns, "error", err)
		}
		return
	}
	c.deleteObject(ctx, "namespaces", "", ns, func(ctx context.Context) error {
		return c.kube.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	})
}

// rescueNamespace clears the finalizers of objects in namespace name if it is
// stuck Terminating, as happens when a finalizer's controller is already gone.
func (c *cleaner) rescueNamespace(ctx context.Context, name string) {
	getCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	ns, err := c.kube.CoreV1().Namespaces().Get(getCtx, name, metav1.GetOptions{})
	cancel()
	if err != nil || ns.Status.Phase != corev1.NamespaceTerminating {
		return
	}
	c.log.Warn("namespace stuck terminating, clearing finalizers", "namespace", name)
	clearFinalizers := []byte(`{"metadata":{"finalizers":[]}}`)
	for _, gvr := range c.namespacedResources() {
		if ctx.Err() != nil {
			return
		}
		listCtx, listCancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
		list, err := c.dyn.Resource(gvr).Namespace(name).List(listCtx, metav1.ListOptions{})
		listCancel()
		if err != nil {
			c.log.Debug("could not list objects for finalizer rescue", "resource", gvr.Resource, "namespace", name, "error", err)
			continue
		}
		for _, obj := range list.Items {
			if len(obj.GetFinalizers()) == 0 {
				continue
			}
			log := c.log.With("resource", gvr.Resource, "name", obj.GetName(), "namespace", name)
			patchCtx, patchCancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
			_, err := c.dyn.Resource(gvr).Namespace(name).Patch(patchCtx, obj.GetName(), types.MergePatchType, clearFinalizers, metav1.PatchOptions{})
			patchCancel()
			if err != nil {
				log.Warn("could not clear finalizers", "error", err)
				continue
			}
			log.Info("cleared finalizers")
		}
	}
}

// namespacedResources returns the sorted preferred version of each namespaced
// resource that supports list and patch, minus finalizerRescueSkip. It uses
// partial results when discovery fails for some groups.
func (c *cleaner) namespacedResources() []schema.GroupVersionResource {
	lists, err := discovery.ServerPreferredNamespacedResources(c.disc)
	if err != nil && len(lists) == 0 {
		c.log.Warn("API discovery failed, cannot clear finalizers", "error", err)
		return nil
	}
	lists = discovery.FilteredBy(discovery.SupportsAllVerbs{Verbs: []string{"list", "patch"}}, lists)
	gvrs, err := discovery.GroupVersionResources(lists)
	if err != nil {
		c.log.Warn("API discovery returned an unparsable group version, cannot clear finalizers", "error", err)
		return nil
	}
	var out []schema.GroupVersionResource
	for gvr := range gvrs {
		if !finalizerRescueSkip[gvr.GroupResource()] {
			out = append(out, gvr)
		}
	}
	slices.SortFunc(out, func(a, b schema.GroupVersionResource) int {
		return strings.Compare(a.String(), b.String())
	})
	return out
}

// isAICRClusterRBAC reports whether obj is cluster RBAC from aicr validate or
// the snapshot agent. It is false for objects labeled component=agent-rbac,
// which an operator applied for their own ServiceAccount and only they can
// restore.
func isAICRClusterRBAC(obj metav1.Object) bool {
	l := obj.GetLabels()
	if l[k8slabels.Component] == k8slabels.ValueAgentRBAC {
		return false
	}
	name := obj.GetName()
	// Current objects match by label, and legacy ones by name.
	return l[k8slabels.ManagedBy] == k8slabels.ValueAICR ||
		l[k8slabels.Name] == vlabels.ValueValidator ||
		strings.HasPrefix(name, vlabels.ValueValidator+"-") ||
		name == defaults.AgentName ||
		name == defaults.AgentClusterRoleName
}

// isValidationRunObject reports whether obj was created by an aicr validate
// run.
func isValidationRunObject(obj metav1.Object) bool {
	l := obj.GetLabels()
	return l[k8slabels.Name] == k8slabels.ValueAICR && l[k8slabels.Component] == vlabels.ValueValidation
}

// isSnapshotAgentObject reports whether obj belongs to any snapshot agent run,
// including one still in flight. It is false for objects labeled
// component=agent-rbac.
func isSnapshotAgentObject(obj metav1.Object) bool {
	l := obj.GetLabels()
	if l[k8slabels.Component] == k8slabels.ValueAgentRBAC {
		return false
	}
	// Legacy objects match by their fixed name, and current ones by label.
	return obj.GetName() == defaults.AgentName || isSnapshotAgentRunObject(obj)
}

// isSnapshotAgentRunObject reports whether obj carries the labels a snapshot
// agent run stamps on what it creates. It never matches by name, so an
// unlabeled ServiceAccount an administrator provisioned is not matched.
func isSnapshotAgentRunObject(obj metav1.Object) bool {
	l := obj.GetLabels()
	return l[k8slabels.Name] == k8slabels.ValueAICR && l[k8slabels.Component] == k8slabels.ValueSnapshotAgent
}
