// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

// Package nicconfigdaemon bootstraps a self-contained NIC Configuration Daemon
// into a private namespace so that `l8k discover` can collect NicDevice CRs
// without requiring a pre-installed Network Operator Helm release.
package nicconfigdaemon

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"strings"
	"text/template"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	yaml "sigs.k8s.io/yaml"

	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon/assets"
)

const (
	// Namespace is the private namespace into which `l8k discover` deploys
	// the NIC Configuration Daemon and its companion RBAC.
	Namespace = "nvidia-k8s-launch-kit"

	// DaemonImageName is the container image short name; the caller composes
	// the full image reference as "<repository>/<DaemonImageName>:<version>".
	DaemonImageName = "nic-configuration-operator-daemon"

	// SAName is the ServiceAccount used by the bootstrapped daemon. Renamed
	// from upstream "controller-manager" to avoid colliding with a coexisting
	// Network Operator install in the same cluster.
	SAName = "k8s-launch-kit-nic-config-daemon"

	// ClusterRoleName / ClusterRoleBindingName are cluster-scoped so they must
	// not collide with upstream "manager-role" / "manager-rolebinding".
	ClusterRoleName        = "k8s-launch-kit-nic-config-daemon"
	ClusterRoleBindingName = "k8s-launch-kit-nic-config-daemon"

	// DaemonSetName matches the upstream name. The discovery code looks for
	// pods owned by a DaemonSet with this name in the Namespace above.
	DaemonSetName = "nic-configuration-daemon"

	// DefaultLogLevel is used when Options.LogLevel is empty.
	DefaultLogLevel = "info"
)

// Options configures a bootstrap of the self-contained NIC Configuration
// Daemon.
type Options struct {
	// Repository is the container image repository (e.g. "nvcr.io/nvidia/mellanox").
	Repository string

	// Version is the image tag corresponding to the chosen Network Operator
	// release line (e.g. "v1.3.1").
	Version string

	// ImagePullSecrets is forwarded onto the DaemonSet pod spec.
	ImagePullSecrets []string

	// LogLevel sets the LOG_LEVEL env var on the daemon container. Defaults
	// to DefaultLogLevel when empty.
	LogLevel string
}

// Image returns the fully qualified image reference for the daemon.
func (o Options) Image() string {
	return fmt.Sprintf("%s/%s:%s", strings.TrimRight(o.Repository, "/"), DaemonImageName, o.Version)
}

// Ensure creates the private namespace, applies the NIC Configuration Operator
// CRDs if they are missing, and applies the daemon SA/RBAC/DaemonSet via
// server-side apply. It is idempotent: re-running over a partially-bootstrapped
// state is safe.
//
// CRDs are intentionally only created when absent — a coexisting Network
// Operator install may own the CRDs at a different version and we must not
// fight it.
func Ensure(ctx context.Context, c client.Client, opts Options) error {
	if opts.Repository == "" {
		return fmt.Errorf("nicconfigdaemon: Options.Repository is required")
	}
	if opts.Version == "" {
		return fmt.Errorf("nicconfigdaemon: Options.Version is required")
	}
	if opts.LogLevel == "" {
		opts.LogLevel = DefaultLogLevel
	}

	if err := ensureCRDs(ctx, c); err != nil {
		return fmt.Errorf("failed to ensure CRDs: %w", err)
	}

	manifests, err := renderDaemonManifests(opts)
	if err != nil {
		return fmt.Errorf("failed to render daemon manifests: %w", err)
	}

	for _, obj := range manifests {
		if err := applyUnstructured(ctx, c, obj); err != nil {
			return fmt.Errorf("failed to apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		log.Log.V(1).Info("Applied bootstrap manifest",
			"kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())
	}
	return nil
}

// ensureCRDs reads every YAML under the embedded crds/ dir and creates it on
// the cluster if no CRD with the same name exists yet.
func ensureCRDs(ctx context.Context, c client.Client) error {
	entries, err := fs.ReadDir(assets.CRDs, "crds")
	if err != nil {
		return fmt.Errorf("read embedded CRDs: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no CRDs embedded under crds/")
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, rErr := fs.ReadFile(assets.CRDs, "crds/"+entry.Name())
		if rErr != nil {
			return fmt.Errorf("read embedded CRD %s: %w", entry.Name(), rErr)
		}
		desired := &apiextv1.CustomResourceDefinition{}
		if uErr := yaml.Unmarshal(data, desired); uErr != nil {
			return fmt.Errorf("decode embedded CRD %s: %w", entry.Name(), uErr)
		}

		existing := &apiextv1.CustomResourceDefinition{}
		getErr := c.Get(ctx, types.NamespacedName{Name: desired.Name}, existing)
		switch {
		case getErr == nil:
			log.Log.V(1).Info("CRD already present; leaving untouched",
				"name", desired.Name)
		case apierrors.IsNotFound(getErr):
			// Strip the resourceVersion just in case the YAML carried one.
			desired.ResourceVersion = ""
			if cErr := c.Create(ctx, desired); cErr != nil && !apierrors.IsAlreadyExists(cErr) {
				return fmt.Errorf("create CRD %s: %w", desired.Name, cErr)
			}
			log.Log.Info("Installed NIC Configuration Operator CRD",
				"name", desired.Name)
		default:
			return fmt.Errorf("get CRD %s: %w", desired.Name, getErr)
		}
	}
	return nil
}

// renderDaemonManifests executes the embedded daemon.yaml.tmpl against opts
// and decodes each YAML document into an *unstructured.Unstructured ready for
// server-side apply.
func renderDaemonManifests(opts Options) ([]*unstructured.Unstructured, error) {
	tmpl, err := template.New("daemon").Parse(assets.DaemonTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse daemon template: %w", err)
	}

	data := map[string]interface{}{
		"Namespace":              Namespace,
		"SAName":                 SAName,
		"ClusterRoleName":        ClusterRoleName,
		"ClusterRoleBindingName": ClusterRoleBindingName,
		"DaemonSetName":          DaemonSetName,
		"Image":                  opts.Image(),
		"ImagePullSecrets":       opts.ImagePullSecrets,
		"LogLevel":               opts.LogLevel,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute daemon template: %w", err)
	}

	return decodeMultiDocYAML(buf.Bytes())
}

// decodeMultiDocYAML splits a multi-document YAML stream on `---` separators
// and decodes each document into *unstructured.Unstructured with GVK set.
func decodeMultiDocYAML(raw []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, doc := range splitYAMLDocuments(string(raw)) {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}
		// Skip pure-comment documents.
		nonComment := false
		for _, line := range strings.Split(trimmed, "\n") {
			if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
				nonComment = true
				break
			}
		}
		if !nonComment {
			continue
		}

		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
			return nil, fmt.Errorf("decode manifest: %w", err)
		}
		if obj.GetKind() == "" {
			return nil, fmt.Errorf("manifest is missing kind: %s", trimmed)
		}
		if apiv := obj.GetAPIVersion(); apiv != "" {
			gv, gErr := schema.ParseGroupVersion(apiv)
			if gErr != nil {
				return nil, fmt.Errorf("parse apiVersion %q: %w", apiv, gErr)
			}
			obj.SetGroupVersionKind(gv.WithKind(obj.GetKind()))
		}
		out = append(out, obj)
	}
	return out, nil
}

// splitYAMLDocuments splits a YAML stream on lines that start with '---'.
// (Mirrors the helper in pkg/networkoperatorplugin/deploy.go but kept local
// so this package has no dependency on its caller.)
func splitYAMLDocuments(s string) []string {
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

// applyUnstructured creates the object if it does not exist, otherwise
// overwrites it via Update with the existing resourceVersion. This Get →
// Create-or-Update pattern is functionally equivalent to a single-owner SSA
// for the kinds we render (Namespace, SA, ClusterRole, ClusterRoleBinding,
// DaemonSet) and works against both real clusters and the controller-runtime
// fake client (which does not implement server-side apply).
func applyUnstructured(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	getErr := c.Get(ctx, key, existing)
	switch {
	case apierrors.IsNotFound(getErr):
		create := obj.DeepCopy()
		create.SetResourceVersion("")
		if err := c.Create(ctx, create); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	case getErr != nil:
		return getErr
	default:
		update := obj.DeepCopy()
		update.SetResourceVersion(existing.GetResourceVersion())
		return c.Update(ctx, update)
	}
}
