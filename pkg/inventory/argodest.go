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

package inventory

import (
	"context"
	"encoding/base64"
	stderrors "errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Argo CD's names for the cluster it runs in, and the label on the Secrets
// that register every other cluster it deploys to.
const (
	argoInClusterServer       = "https://kubernetes.default.svc"
	argoInClusterName         = "in-cluster"
	argoClusterSecretSelector = "argocd.argoproj.io/secret-type=cluster" //nolint:gosec // G101 false positive: a label selector, not a credential
)

// argoClusterSecretSubject names the read in messages and in an RBAC rule.
const argoClusterSecretSubject = "Argo CD cluster Secrets" //nolint:gosec // G101 false positive: a message subject, not a credential

var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: resourceSecrets}

// argoDestinations decides whether an Application deploys into the cluster
// being read.
//
// On an Argo CD management cluster most Applications deploy elsewhere, and one
// carrying a component's name and namespace would otherwise become this
// cluster's installed baseline: a remote install at the --to version reads as
// the local upgrade already made. A destination is local only when it is
// provably this cluster. A destination named rather than addressed is resolved
// through Argo's cluster Secrets, read once and only if some Application needs
// them, and one that cannot be resolved, or resolves both ways, is refused
// rather than guessed at.
type argoDestinations struct {
	client dynamic.Interface
	named  map[string][]string
	loaded bool
}

// local reports whether item deploys into the cluster being read.
func (d *argoDestinations) local(ctx context.Context, item *unstructured.Unstructured) (bool, error) {
	server, _, err := unstructured.NestedString(item.Object, "spec", "destination", "server")
	if err != nil {
		return false, fieldError(item, err, "spec.destination.server", "a string")
	}
	name, _, err := unstructured.NestedString(item.Object, "spec", "destination", "name")
	if err != nil {
		return false, fieldError(item, err, "spec.destination.name", "a string")
	}

	switch {
	case server != "" && name != "":
		return false, destinationError(item,
			"sets both spec.destination.server and spec.destination.name, which Argo CD refuses")
	case server != "":
		return server == argoInClusterServer, nil
	case name == "":
		return false, destinationError(item, "sets neither spec.destination.server nor spec.destination.name")
	}

	servers, err := d.serversNamed(ctx, name)
	if err != nil {
		return false, err
	}
	if len(servers) == 0 {
		if name == argoInClusterName {
			return true, nil
		}

		return false, destinationError(item,
			fmt.Sprintf("names destination cluster %q, which no Argo CD cluster Secret defines", name))
	}
	inCluster := 0
	for _, s := range servers {
		if s == argoInClusterServer {
			inCluster++
		}
	}
	switch inCluster {
	case len(servers):
		return true, nil
	case 0:
		return false, nil
	}

	return false, errors.NewWithContext(errors.ErrCodeConflict,
		fmt.Sprintf("the Argo CD Application %q in namespace %q names destination cluster %q, which more than "+
			"one Argo CD cluster Secret defines, and they disagree on whether it is this cluster",
			item.GetName(), item.GetNamespace(), name),
		applicationContext(item))
}

// serversNamed returns every server the cluster Secrets register under name.
func (d *argoDestinations) serversNamed(ctx context.Context, name string) ([]string, error) {
	if !d.loaded {
		named, err := argoClusterSecrets(ctx, d.client)
		if err != nil {
			return nil, err
		}
		d.named, d.loaded = named, true
	}

	return d.named[name], nil
}

// argoClusterSecrets maps each registered cluster name to the servers
// registered under it, across every Argo CD instance on the cluster.
//
// Only name and server are decoded. These Secrets carry the credentials Argo
// uses to reach each cluster, and nothing else from them is kept, logged or
// put in an error. One that names nothing or does not decode is skipped: it
// cannot register a name, and a destination naming only such a cluster stays
// unresolved and is refused.
func argoClusterSecrets(ctx context.Context, client dynamic.Interface) (map[string][]string, error) {
	named := map[string][]string{}
	opts := metav1.ListOptions{
		LabelSelector: argoClusterSecretSelector,
		Limit:         defaults.ArgoApplicationListPageSize,
	}
	for {
		if err := ctxErr(ctx, argoClusterSecretSubject); err != nil {
			return nil, err
		}
		page, err := client.Resource(secretGVR).Namespace(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return nil, argoClusterSecretError(err)
		}
		for i := range page.Items {
			name, nameOK := secretField(&page.Items[i], "name")
			server, serverOK := secretField(&page.Items[i], "server")
			if !nameOK || !serverOK {
				continue
			}
			named[name] = append(named[name], server)
		}
		if page.GetContinue() == "" {
			return named, nil
		}
		if err := advance(&opts, page.GetContinue(), resourceSecrets); err != nil {
			return nil, err
		}
	}
}

// secretField decodes one non-empty data key of a Secret.
func secretField(secret *unstructured.Unstructured, key string) (string, bool) {
	encoded, _, err := unstructured.NestedString(secret.Object, "data", key)
	if err != nil || encoded == "" {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		return "", false
	}

	return string(decoded), true
}

// destinationError reports an Application whose destination cluster cannot be
// determined.
func destinationError(item *unstructured.Unstructured, problem string) error {
	return errors.NewWithContext(errors.ErrCodeInternal,
		fmt.Sprintf("the Argo CD Application %q in namespace %q %s, so whether it deploys into this cluster "+
			"cannot be determined", item.GetName(), item.GetNamespace(), problem),
		applicationContext(item))
}

// argoClusterSecretError classifies a failed List of the cluster Secrets.
func argoClusterSecretError(err error) error {
	errCtx := map[string]any{ctxKeyResource: resourceSecrets}

	if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return abortError(err, argoClusterSecretSubject, errCtx)
	}
	if apierrors.IsForbidden(err) {
		return errors.WrapWithContext(errors.ErrCodeUnauthorized,
			"cannot list secrets across all namespaces, so an Argo CD Application's named destination cluster "+
				"cannot be resolved; grant 'list secrets' at cluster scope and re-run",
			err, errCtx)
	}

	return errors.WrapWithContext(errors.ErrCodeInternal,
		"failed to list the Argo CD cluster Secrets that resolve a named destination", err, errCtx)
}
