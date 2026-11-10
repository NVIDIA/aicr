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

package aicr

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestValidateStatePreservesPartialResultsOnCancellation(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary cancellation", true: "cancellation and RBAC failure"}[cleanupFails], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var once sync.Once
			// This in-process API double echoes typed writes. Cancellation lands while
			// persisting the first phase, after cluster setup, without a live cluster.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { _ = r.Body.Close() }()
				if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/clusterrolebindings/") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","code":403,"message":"injected privileged revocation failure"}`)
					return
				}
				if r.Method == http.MethodDelete {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Status","status":"Success"}`)
					return
				}
				var obj map[string]any
				data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				if err != nil {
					t.Errorf("read fake request: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				// Typed Create uses Kubernetes protobuf; only its successful response
				// matters to setup. Apply requests below carry JSON objects.
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/configmaps") {
					obj = map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "fake"}}
				} else if err := json.Unmarshal(data, &obj); err != nil {
					t.Errorf("decode fake request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if strings.Contains(r.URL.Path, "/configmaps/aicr-ctrf-") {
					once.Do(cancel)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(obj); err != nil {
					t.Errorf("fake response: %v", err)
				}
			}))
			defer server.Close()
			kubeconfig := filepath.Join(t.TempDir(), "fake-kubeconfig")
			config := clientcmdapi.Config{CurrentContext: "fake", Clusters: map[string]*clientcmdapi.Cluster{"fake": {Server: server.URL}}, Contexts: map[string]*clientcmdapi.Context{"fake": {Cluster: "fake"}}}
			if err := clientcmd.WriteToFile(config, kubeconfig); err != nil {
				t.Fatal(err)
			}
			client := newClientForBundleTest(t)
			rec := newRecipeResultForBundleTest(client, []recipe.ComponentRef{{Name: "gpu-operator", Type: recipe.ComponentTypeHelm}}, nil)
			results, err := client.ValidateState(ctx, rec, &Snapshot{}, WithValidationKubeconfig(kubeconfig), WithValidationCleanup(cleanupFails), WithValidationPhases(PhaseDeployment))
			wantCode := errors.ErrCodeCanceled
			if cleanupFails {
				wantCode = errors.ErrCodeInternal
				if !strings.Contains(err.Error(), "injected privileged revocation failure") || !strings.Contains(err.Error(), "cluster-admin binding may be orphaned") {
					t.Errorf("cleanup failure discarded: %v", err)
				}
			}
			errorstest.WantReportedCode(t, err, wantCode)
			if !stderrors.Is(err, context.Canceled) {
				t.Errorf("cancellation cause discarded: %v", err)
			}
			if len(results) != 1 || results[0].Report == nil || results[0].Status != ctrf.StatusOther {
				t.Fatalf("partial results=%+v", results)
			}
			if len(results[0].RawReport) == 0 {
				t.Fatal("partial CTRF JSON was discarded")
			}
		})
	}
}
