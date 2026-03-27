// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package api

import (
	"context"
	"net/http"
	"regexp"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/discovery"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/server"
)

// dnsNameRegex validates DNS domain names: labels separated by dots, optional trailing dot.
// Each label: 1-63 chars of [a-z0-9_-], total <= 253 chars.
var dnsNameRegex = regexp.MustCompile(`^[a-z0-9_][a-z0-9_\-]{0,62}(\.[a-z0-9_][a-z0-9_\-]{0,62})*\.?$`)

// agentResponse represents a single agent in the /v1/agents JSON response.
type agentResponse struct {
	Name     string             `json:"name"`
	Protocol string             `json:"protocol"`
	Endpoint string             `json:"endpoint"`
	Port     uint16             `json:"port"`
	Priority uint16             `json:"priority"`
	Params   agentParamsSummary `json:"params,omitempty"`
}

// agentParamsSummary is a subset of SvcParams relevant for the API response.
type agentParamsSummary struct {
	Realm        string   `json:"realm,omitempty"`
	BAP          []string `json:"bap,omitempty"`
	Policy       string   `json:"policy,omitempty"`
	ConnectClass string   `json:"connectClass,omitempty"`
}

// handleAgents returns an http.HandlerFunc that discovers all agents in a domain
// and returns them as JSON. The domain is taken from the "domain" query parameter
// and defaults to defaultDomain.
func handleAgents(disc *discovery.Discoverer, defaultDomain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			server.WriteError(w, r, http.StatusMethodNotAllowed, errors.ErrCodeMethodNotAllowed,
				"Method not allowed", false, nil)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), defaults.DiscoveryHandlerTimeout)
		defer cancel()

		domain := r.URL.Query().Get("domain")
		if domain == "" {
			domain = defaultDomain
		}
		if len(domain) > 253 || !dnsNameRegex.MatchString(domain) {
			server.WriteError(w, r, http.StatusBadRequest, errors.ErrCodeInvalidRequest,
				"invalid domain parameter", false, map[string]any{"domain": domain})
			return
		}

		records, err := disc.DiscoverAll(ctx, domain)
		if err != nil {
			server.WriteErrorFromErr(w, r, err, "agent discovery failed", nil)
			return
		}

		agents := make([]agentResponse, 0, len(records))
		for _, rec := range records {
			agents = append(agents, agentResponse{
				Name:     rec.Name,
				Protocol: string(rec.Protocol),
				Endpoint: rec.Endpoint,
				Port:     rec.Port,
				Priority: rec.Priority,
				Params: agentParamsSummary{
					Realm:        rec.Params.Realm,
					BAP:          rec.Params.BAP,
					Policy:       rec.Params.Policy,
					ConnectClass: rec.Params.ConnectClass,
				},
			})
		}

		resp := map[string]any{
			"domain": domain,
			"agents": agents,
			"count":  len(agents),
		}

		serializer.RespondJSON(w, http.StatusOK, resp)
	}
}
