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

package chainsaw

import (
	"context"
	"testing"
	"time"

	"aicr.run/pkg/recipe"
)

// TestKubeflowTrainingOperatorHealthCheckWebhookStates runs the
// kubeflow-training-operator health check against the webhook states it must
// tell apart: the five webhooks served with an injected CA, one of them
// without its CA, and that gap hidden behind a webhook that is not the
// operator's.
func TestKubeflowTrainingOperatorHealthCheckWebhookStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/kubeflow-training-operator/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}
	content := string(data)

	crds := []string{
		"jaxjobs.kubeflow.org", "paddlejobs.kubeflow.org", "pytorchjobs.kubeflow.org",
		"tfjobs.kubeflow.org", "xgboostjobs.kubeflow.org",
	}
	deployment := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "training-operator", "namespace": "kubeflow", "generation": int64(1)},
		"spec":     map[string]any{"replicas": int64(1)},
		"status": map[string]any{
			"replicas": int64(1), "updatedReplicas": int64(1),
			"availableReplicas": int64(1), "observedGeneration": int64(1),
		},
	}
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "training-operator-webhook-cert", "namespace": "kubeflow"},
		"data":     map[string]any{"tls.crt": "Y2VydA=="},
	}
	const ca = "Y2E="
	webhook := func(name, service, namespace, caBundle string) map[string]any {
		clientConfig := map[string]any{"service": map[string]any{"name": service, "namespace": namespace}}
		if caBundle != "" {
			clientConfig["caBundle"] = caBundle
		}
		return map[string]any{"name": name, "clientConfig": clientConfig}
	}
	ours := func(cas ...string) []any {
		kinds := []string{"jaxjob", "paddlejob", "pytorchjob", "tfjob", "xgboostjob"}
		webhooks := make([]any, 0, len(kinds))
		for i, kind := range kinds {
			webhooks = append(webhooks,
				webhook("validator."+kind+".training-operator.kubeflow.org", "training-operator", "kubeflow", cas[i]))
		}
		return webhooks
	}
	foreign := webhook("validator.other.example.com", "other", "elsewhere", ca)

	tests := []struct {
		name     string
		webhooks []any
		wantPass bool
	}{
		{name: "five webhooks with a CA → pass", webhooks: ours(ca, ca, ca, ca, ca), wantPass: true},
		{name: "one webhook without its CA → fail", webhooks: ours(ca, ca, "", ca, ca)},
		{name: "no CA injected yet → fail", webhooks: ours("", "", "", "", "")},
		{name: "missing CA hidden by a foreign webhook with one → fail",
			webhooks: append(ours(ca, ca, "", ca, ca), foreign)},
		{name: "a webhook missing → fail", webhooks: ours(ca, ca, ca, ca, ca)[:4]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			for _, name := range crds {
				f.addGet("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name, map[string]any{
					"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
					"metadata": map[string]any{"name": name},
					"status": map[string]any{"conditions": []any{
						map[string]any{"type": "Established", "status": "True"},
					}},
				})
			}
			f.addGet("apps/v1", "Deployment", "kubeflow", "training-operator", deployment)
			f.addGet("v1", "Secret", "kubeflow", "training-operator-webhook-cert", secret)
			f.addGet("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "",
				"validator.training-operator.kubeflow.org", map[string]any{
					"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration",
					"metadata": map[string]any{"name": "validator.training-operator.kubeflow.org"},
					"webhooks": tt.webhooks,
				})

			res := runChainsawTestInProcess(context.Background(), "kubeflow-training-operator", content, 2*time.Second, f)
			if res.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", res.Passed, tt.wantPass, res.Output)
			}
		})
	}
}
