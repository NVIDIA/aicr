// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"context"
	"log/slog"
	"slices"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	// SubtypePerconaServerMongoDB contains conflict evidence for the Percona
	// Operator for MongoDB API. It does not report database or operator
	// health.
	SubtypePerconaServerMongoDB = "percona-server-mongodb"

	perconaAPIGroup = "psmdb.percona.com"
	perconaResource = "perconaservermongodbs"
	perconaKind     = "PerconaServerMongoDB"

	perconaKeyCollectionState = "collection-state"
	perconaKeyAPIAvailable    = "api-available"
	perconaKeyAPIVersion      = "api-version"

	// AICR's own datastore (the nvsentinel-mongodb component) is not a
	// conflict, so re-snapshotting an opted-in cluster stays clean.
	perconaAICRNamespace = "nvsentinel"
	perconaAICRName      = "nvsentinel-mongodb"

	// perconaOperatorSelector matches operator pods from the Percona Helm
	// chart (psmdb-operator) and from Percona's plain manifests and OLM
	// bundle (percona-server-mongodb-operator). A nameOverride escapes it.
	perconaOperatorSelector = "app.kubernetes.io/name in (psmdb-operator,percona-server-mongodb-operator)"

	perconaListPageSize = 100
	perconaMaxListPages = 20

	perconaStateAbsent           = "absent"
	perconaStateAPIDetected      = "api-detected"
	perconaStateAICROwned        = "aicr-owned"
	perconaStateOperatorDetected = "operator-detected"
	perconaStateCRsDetected      = "crs-detected"
	perconaStateUnknown          = "unknown"
)

type perconaSummary struct {
	state        string
	apiAvailable *bool
	apiVersion   string
}

func (s perconaSummary) subtype() measurement.Subtype {
	data := map[string]measurement.Reading{
		perconaKeyCollectionState: measurement.Str(s.state),
	}
	if s.apiAvailable != nil {
		data[perconaKeyAPIAvailable] = measurement.Bool(*s.apiAvailable)
	}
	if s.apiVersion != "" {
		data[perconaKeyAPIVersion] = measurement.Str(s.apiVersion)
	}
	return measurement.Subtype{Name: SubtypePerconaServerMongoDB, Data: data}
}

func unknownPerconaSubtype() measurement.Subtype {
	return perconaSummary{state: perconaStateUnknown}.subtype()
}

// collectPerconaServerMongoDB records Percona API-group presence, whether
// any PerconaServerMongoDB CR other than AICR's own exists, and whether a
// Percona operator pod runs outside AICR's namespace. It deliberately never
// infers database or operator health.
//
// States, most to least severe: crs-detected (a foreign CR exists),
// operator-detected (no foreign CR, but a foreign operator pod runs),
// aicr-owned (only AICR's own CR), api-detected (API served, no CRs),
// absent, and unknown when any lookup fails.
func (k *Collector) collectPerconaServerMongoDB(
	ctx context.Context,
	defaultDiscovery apiResourceDiscovery,
) measurement.Subtype {

	if err := ctx.Err(); err != nil {
		slog.Warn("Percona Operator for MongoDB discovery cancelled", slog.String("error", err.Error()))
		return unknownPerconaSubtype()
	}

	discoveryClient := k.perconaDiscovery
	if discoveryClient == nil {
		discoveryClient = defaultDiscovery
	}
	if discoveryClient == nil {
		slog.Warn("Percona Operator for MongoDB discovery client unavailable")
		return unknownPerconaSubtype()
	}

	groups, err := discoveryClient.ServerGroups()
	if err != nil {
		slog.Warn("failed to discover Kubernetes API groups for Percona Operator for MongoDB",
			slog.String("error", err.Error()))
		return unknownPerconaSubtype()
	}
	if groups == nil {
		slog.Warn("Kubernetes API group discovery returned no response for Percona Operator for MongoDB")
		return unknownPerconaSubtype()
	}

	group := findAPIGroup(groups, perconaAPIGroup)
	if group == nil {
		return perconaSummary{
			state:        perconaStateAbsent,
			apiAvailable: valuePtr(false),
		}.subtype()
	}

	gvr, ambiguous := discoverAPIResourceGVR(
		ctx,
		discoveryClient,
		group,
		perconaAPIGroup,
		perconaResource,
		perconaKind,
	)
	if gvr == nil {
		if ambiguous {
			return unknownPerconaSubtype()
		}
		// The served API group means psmdb.percona.com CRDs exist even
		// without the exact resource.
		return perconaSummary{
			state:        perconaStateAPIDetected,
			apiAvailable: valuePtr(false),
		}.subtype()
	}

	summary := perconaSummary{
		apiAvailable: valuePtr(true),
		apiVersion:   gvr.Version,
	}
	dynamicClient, err := k.getDynamicClient()
	if err != nil {
		slog.Warn("failed to initialize dynamic client for Percona Operator for MongoDB",
			slog.String("apiVersion", gvr.Version),
			slog.String("error", err.Error()))
		summary.state = perconaStateUnknown
		return summary.subtype()
	}

	foreign, aicrOwned, err := listPerconaServerMongoDBs(ctx, dynamicClient, *gvr)
	if err != nil {
		slog.Warn("failed to list PerconaServerMongoDB custom resources",
			slog.String("apiVersion", gvr.Version),
			slog.String("error", err.Error()))
		summary.state = perconaStateUnknown
		return summary.subtype()
	}
	if foreign {
		summary.state = perconaStateCRsDetected
		return summary.subtype()
	}

	operatorFound, err := k.foreignPerconaOperatorRunning(ctx)
	if err != nil {
		slog.Warn("failed to list Percona operator pods",
			slog.String("error", err.Error()))
		summary.state = perconaStateUnknown
		return summary.subtype()
	}

	switch {
	case operatorFound:
		summary.state = perconaStateOperatorDetected
	case aicrOwned:
		summary.state = perconaStateAICROwned
	default:
		summary.state = perconaStateAPIDetected
	}
	return summary.subtype()
}

// listPerconaServerMongoDBs pages through PerconaServerMongoDB CRs until it
// finds one that is not AICR's own. It fails rather than truncating, so an
// unread page can never hide a foreign CR.
func listPerconaServerMongoDBs(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
) (foreign bool, aicrOwned bool, err error) {

	opts := metav1.ListOptions{Limit: perconaListPageSize}
	for range perconaMaxListPages {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, false, errors.Wrap(errors.ErrCodeTimeout,
				"PerconaServerMongoDB listing cancelled", ctxErr)
		}
		page, listErr := client.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, opts)
		if listErr != nil {
			return false, false, errors.Wrap(errors.ErrCodeInternal,
				"failed to list PerconaServerMongoDB custom resources", listErr)
		}
		if page == nil {
			return false, false, errors.New(errors.ErrCodeInternal,
				"PerconaServerMongoDB custom resource list returned no response")
		}
		for i := range page.Items {
			if page.Items[i].GetNamespace() == perconaAICRNamespace &&
				page.Items[i].GetName() == perconaAICRName {

				aicrOwned = true
				continue
			}
			return true, aicrOwned, nil
		}
		if page.GetContinue() == "" {
			return false, aicrOwned, nil
		}
		opts.Continue = page.GetContinue()
	}
	return false, false, errors.New(errors.ErrCodeInternal,
		"PerconaServerMongoDB listing exceeded the page bound")
}

// perconaAICRReleases are the Helm release names AICR's psmdb-operator pods
// carry in app.kubernetes.io/instance: the component name under the helm,
// helmfile and Argo CD deployers, and Flux's default <targetNamespace>-<name>.
var perconaAICRReleases = []string{"psmdb-operator", "nvsentinel-psmdb-operator"}

// foreignPerconaOperatorRunning reports whether a Percona operator pod runs
// that AICR did not install: outside AICR's namespace, or inside it under
// another Helm release. One matching pod settles it.
func (k *Collector) foreignPerconaOperatorRunning(ctx context.Context) (bool, error) {
	if k.ClientSet == nil {
		return false, errors.New(errors.ErrCodeInternal, "kubernetes client unavailable")
	}
	opts := metav1.ListOptions{LabelSelector: perconaOperatorSelector, Limit: perconaListPageSize}
	for range perconaMaxListPages {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, errors.Wrap(errors.ErrCodeTimeout, "Percona operator pod listing cancelled", ctxErr)
		}
		pods, err := k.ClientSet.CoreV1().Pods(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return false, errors.Wrap(errors.ErrCodeInternal, "failed to list Percona operator pods", err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			aicrOwned := pod.Namespace == perconaAICRNamespace &&
				slices.Contains(perconaAICRReleases, pod.Labels["app.kubernetes.io/instance"])
			if !aicrOwned {
				return true, nil
			}
		}
		if pods.Continue == "" {
			return false, nil
		}
		opts.Continue = pods.Continue
	}
	return false, errors.New(errors.ErrCodeInternal, "Percona operator pod listing exceeded the page bound")
}
