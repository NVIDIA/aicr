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

	"github.com/NVIDIA/aicr/pkg/measurement"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// SubtypeMariaDBOperator contains conflict evidence for the official
	// MariaDB Operator API. It does not report database or operator health.
	SubtypeMariaDBOperator = "mariadb-operator"

	mariaDBAPIGroup = "k8s.mariadb.com"
	mariaDBResource = "mariadbs"
	mariaDBKind     = "MariaDB"

	mariaDBKeyCollectionState = "collection-state"
	mariaDBKeyAPIAvailable    = "api-available"
	mariaDBKeyAPIVersion      = "api-version"

	mariaDBStateAbsent      = "absent"
	mariaDBStateAPIDetected = "api-detected"
	mariaDBStateCRsDetected = "crs-detected"
	mariaDBStateUnknown     = "unknown"
)

type mariaDBSummary struct {
	state        string
	apiAvailable *bool
	apiVersion   string
}

func (s mariaDBSummary) subtype() measurement.Subtype {
	data := map[string]measurement.Reading{
		mariaDBKeyCollectionState: measurement.Str(s.state),
	}
	if s.apiAvailable != nil {
		data[mariaDBKeyAPIAvailable] = measurement.Bool(*s.apiAvailable)
	}
	if s.apiVersion != "" {
		data[mariaDBKeyAPIVersion] = measurement.Str(s.apiVersion)
	}
	return measurement.Subtype{Name: SubtypeMariaDBOperator, Data: data}
}

func unknownMariaDBSubtype() measurement.Subtype {
	return mariaDBSummary{state: mariaDBStateUnknown}.subtype()
}

// collectMariaDBOperator records only official API-group and MariaDB CR
// presence. It deliberately never infers that a usable database exists.
func (k *Collector) collectMariaDBOperator(
	ctx context.Context,
	defaultDiscovery apiResourceDiscovery,
) measurement.Subtype {

	if err := ctx.Err(); err != nil {
		slog.Warn("MariaDB Operator discovery cancelled", slog.String("error", err.Error()))
		return unknownMariaDBSubtype()
	}

	discoveryClient := k.mariaDBDiscovery
	if discoveryClient == nil {
		discoveryClient = defaultDiscovery
	}
	if discoveryClient == nil {
		slog.Warn("MariaDB Operator discovery client unavailable")
		return unknownMariaDBSubtype()
	}

	groups, err := discoveryClient.ServerGroups()
	if err != nil {
		slog.Warn("failed to discover Kubernetes API groups for MariaDB Operator",
			slog.String("error", err.Error()))
		return unknownMariaDBSubtype()
	}
	if groups == nil {
		slog.Warn("Kubernetes API group discovery returned no response for MariaDB Operator")
		return unknownMariaDBSubtype()
	}

	group := findAPIGroup(groups, mariaDBAPIGroup)
	if group == nil {
		return mariaDBSummary{
			state:        mariaDBStateAbsent,
			apiAvailable: valuePtr(false),
		}.subtype()
	}

	gvr, ambiguous := discoverAPIResourceGVR(
		ctx,
		discoveryClient,
		group,
		mariaDBAPIGroup,
		mariaDBResource,
		mariaDBKind,
	)
	if gvr == nil {
		if ambiguous {
			return unknownMariaDBSubtype()
		}
		// The official API group itself is conflict evidence even when the
		// exact MariaDB resource is not served.
		return mariaDBSummary{
			state:        mariaDBStateAPIDetected,
			apiAvailable: valuePtr(false),
		}.subtype()
	}

	summary := mariaDBSummary{
		apiAvailable: valuePtr(true),
		apiVersion:   gvr.Version,
	}
	dynamicClient, err := k.getDynamicClient()
	if err != nil {
		slog.Warn("failed to initialize dynamic client for MariaDB Operator",
			slog.String("apiVersion", gvr.Version),
			slog.String("error", err.Error()))
		summary.state = mariaDBStateUnknown
		return summary.subtype()
	}

	mariaDBs, err := dynamicClient.Resource(*gvr).
		Namespace(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Warn("failed to list official MariaDB custom resources",
			slog.String("apiVersion", gvr.Version),
			slog.String("error", err.Error()))
		summary.state = mariaDBStateUnknown
		return summary.subtype()
	}
	if mariaDBs == nil {
		slog.Warn("official MariaDB custom resource list returned no response",
			slog.String("apiVersion", gvr.Version))
		summary.state = mariaDBStateUnknown
		return summary.subtype()
	}

	if len(mariaDBs.Items) == 0 {
		summary.state = mariaDBStateAPIDetected
	} else {
		summary.state = mariaDBStateCRsDetected
	}
	return summary.subtype()
}
