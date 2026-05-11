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

package server

import (
	"net/http"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// healthResponse represents health check response
type healthResponse struct {
	Status    string    `json:"status" yaml:"status"`
	Timestamp time.Time `json:"timestamp" yaml:"timestamp"`
	Reason    string    `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// handleHealth handles GET /health
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{keyMethod: r.Method})
		return
	}

	resp := healthResponse{
		Status:    "healthy",
		Timestamp: time.Now(),
	}

	serializer.RespondJSON(w, http.StatusOK, resp)
}

// handleReady handles GET /ready
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{keyMethod: r.Method})
		return
	}

	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()

	if !ready {
		resp := healthResponse{
			Status:    "not_ready",
			Timestamp: time.Now(),
			Reason:    "service is initializing",
		}
		serializer.RespondJSON(w, http.StatusServiceUnavailable, resp)
		return
	}

	resp := healthResponse{
		Status:    "ready",
		Timestamp: time.Now(),
	}

	serializer.RespondJSON(w, http.StatusOK, resp)
}
