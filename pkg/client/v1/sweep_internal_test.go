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

package aicr

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator/sweep"
)

// fakeSweepKubeconfig writes a kubeconfig naming a context whose server needs
// no dialing: the facade builds clients from it without contacting anything.
func fakeSweepKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	config := clientcmdapi.Config{
		CurrentContext: "sweep-test",
		Clusters:       map[string]*clientcmdapi.Cluster{"sweep-test": {Server: "https://sweep-test.invalid:6443"}},
		Contexts:       map[string]*clientcmdapi.Context{"sweep-test": {Cluster: "sweep-test"}},
	}
	if err := clientcmd.WriteToFile(config, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeSweep records the sweep entry points the facade reached and answers
// with canned results.
type fakeSweep struct {
	planOpts  []sweep.Options
	applySeen []*sweep.Report
	plan      *sweep.Report
	planErr   error
	applyErr  error
}

func (f *fakeSweep) client(t *testing.T) *Client {
	t.Helper()
	deps := defaultClientDependencies()
	deps.sweepPlan = func(_ context.Context, _ kubernetes.Interface, _ dynamic.Interface, opts sweep.Options) (*sweep.Report, error) {
		f.planOpts = append(f.planOpts, opts)
		return f.plan, f.planErr
	}
	deps.sweepApply = func(_ context.Context, _ kubernetes.Interface, _ dynamic.Interface, r *sweep.Report) error {
		// Snapshot what the apply step received before mutating statuses, so
		// the assertion reads the input, not the fake's own work.
		seen := *r
		seen.Objects = append([]sweep.Object(nil), r.Objects...)
		f.applySeen = append(f.applySeen, &seen)
		for i := range r.Objects {
			if r.Objects[i].Status == sweep.StatusWouldDelete {
				r.Objects[i].Status = sweep.StatusDeleted
			}
		}
		return f.applyErr
	}
	client, err := newClientWithContextAndDependencies(
		context.Background(), deps, WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func sampleSweepPlan() *sweep.Report {
	return &sweep.Report{
		Namespace: defaults.ValidationNamespace,
		OlderThan: defaults.ValidationSweepMinAge,
		Objects: []sweep.Object{
			{
				Kind:   "ClusterRoleBinding",
				Name:   "aicr-validator-run-1",
				UID:    types.UID("uid-1"),
				Age:    3 * time.Hour,
				RunID:  "run-1",
				Status: sweep.StatusWouldDelete,
			},
			{
				Kind:   "Namespace",
				Name:   "aicr-gpu-test-0123456789abcdef0123456789abcdef",
				UID:    types.UID("uid-2"),
				Age:    25 * time.Minute,
				Status: sweep.StatusSkipped,
				Reason: "below the floor",
			},
		},
	}
}

func TestPlanValidationSweep(t *testing.T) {
	kubeconfig := fakeSweepKubeconfig(t)
	f := &fakeSweep{plan: sampleSweepPlan()}
	client := f.client(t)

	report, err := client.PlanValidationSweep(context.Background(), SweepOptions{Kubeconfig: kubeconfig})
	if err != nil {
		t.Fatalf("PlanValidationSweep() error = %v", err)
	}

	// Options were defaulted before reaching the sweep package.
	if len(f.planOpts) != 1 {
		t.Fatalf("sweep plan calls = %d, want 1", len(f.planOpts))
	}
	got := f.planOpts[0]
	if got.Namespace != defaults.ValidationNamespace || got.OlderThan != defaults.ValidationSweepMinAge {
		t.Errorf("sweep options = %+v, want namespace %q and floor %v", got, defaults.ValidationNamespace, defaults.ValidationSweepMinAge)
	}

	// The report is facade-owned and names the cluster it read.
	if report.ClusterContext != "sweep-test" {
		t.Errorf("ClusterContext = %q, want %q", report.ClusterContext, "sweep-test")
	}
	if report.Server != "https://sweep-test.invalid:6443" {
		t.Errorf("Server = %q, want %q", report.Server, "https://sweep-test.invalid:6443")
	}
	if report.Namespace != defaults.ValidationNamespace || report.OlderThan != defaults.ValidationSweepMinAge {
		t.Errorf("report scope = %q/%v, want %q/%v", report.Namespace, report.OlderThan,
			defaults.ValidationNamespace, defaults.ValidationSweepMinAge)
	}
	if len(report.Objects) != 2 || report.Objects[0].Status != SweepWouldDelete || report.Objects[1].Status != SweepSkipped {
		t.Errorf("objects = %+v", report.Objects)
	}
	if got := len(report.Candidates()); got != 1 {
		t.Errorf("Candidates() = %d, want 1", got)
	}
}

func TestPlanValidationSweepRejectsNegativeOlderThan(t *testing.T) {
	f := &fakeSweep{plan: sampleSweepPlan()}
	client := f.client(t)
	_, err := client.PlanValidationSweep(context.Background(), SweepOptions{OlderThan: -time.Hour})
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if len(f.planOpts) != 0 {
		t.Error("the sweep entry point was reached with invalid options")
	}
}

func TestPlanValidationSweepPropagatesPlanError(t *testing.T) {
	kubeconfig := fakeSweepKubeconfig(t)
	f := &fakeSweep{planErr: errors.New(errors.ErrCodeUnauthorized, "cannot list namespaces")}
	client := f.client(t)
	_, err := client.PlanValidationSweep(context.Background(), SweepOptions{Kubeconfig: kubeconfig})
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnauthorized)
}

func TestApplyValidationSweep(t *testing.T) {
	kubeconfig := fakeSweepKubeconfig(t)
	f := &fakeSweep{plan: sampleSweepPlan()}
	client := f.client(t)

	plan, err := client.PlanValidationSweep(context.Background(), SweepOptions{Kubeconfig: kubeconfig})
	if err != nil {
		t.Fatalf("PlanValidationSweep() error = %v", err)
	}
	report, err := client.ApplyValidationSweep(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyValidationSweep() error = %v", err)
	}

	// The apply step received the planned candidates with their UIDs, and the
	// report carries the apply step's statuses back.
	if len(f.applySeen) != 1 {
		t.Fatalf("sweep apply calls = %d, want 1", len(f.applySeen))
	}
	seen := f.applySeen[0]
	if len(seen.Objects) != 2 || seen.Objects[0].UID != types.UID("uid-1") || seen.Objects[0].Status != sweep.StatusWouldDelete {
		t.Errorf("apply input = %+v", seen.Objects)
	}
	if report.Objects[0].Status != SweepDeleted {
		t.Errorf("applied object status = %q, want %q", report.Objects[0].Status, SweepDeleted)
	}
	if report.ClusterContext != plan.ClusterContext || report.Server != plan.Server {
		t.Errorf("applied report lost the target: %+v", report)
	}
}

func TestApplyValidationSweepReportsPartialFailure(t *testing.T) {
	kubeconfig := fakeSweepKubeconfig(t)
	f := &fakeSweep{
		plan:     sampleSweepPlan(),
		applyErr: errors.New(errors.ErrCodeInternal, "sweep failed to delete 1 object(s)"),
	}
	client := f.client(t)

	plan, err := client.PlanValidationSweep(context.Background(), SweepOptions{Kubeconfig: kubeconfig})
	if err != nil {
		t.Fatalf("PlanValidationSweep() error = %v", err)
	}
	report, err := client.ApplyValidationSweep(context.Background(), plan)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	if report == nil {
		t.Fatal("a partial failure must still return the report")
	}
}

func TestApplyValidationSweepRejectsNilPlan(t *testing.T) {
	f := &fakeSweep{}
	client := f.client(t)

	_, err := client.ApplyValidationSweep(context.Background(), nil)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if len(f.applySeen) != 0 {
		t.Error("the sweep entry point was reached for a nil plan")
	}
}

// TestApplyValidationSweepRejectsReportsNotPlanned: the apply path is bound
// to plans PlanValidationSweep produced. A struct literal or a JSON round
// trip cannot carry the unexported marker, so both are rejected before any
// cluster is contacted — re-plan instead.
func TestApplyValidationSweepRejectsReportsNotPlanned(t *testing.T) {
	kubeconfig := fakeSweepKubeconfig(t)
	f := &fakeSweep{plan: sampleSweepPlan()}
	client := f.client(t)

	plan, err := client.PlanValidationSweep(context.Background(), SweepOptions{Kubeconfig: kubeconfig})
	if err != nil {
		t.Fatalf("PlanValidationSweep() error = %v", err)
	}

	t.Run("struct literal", func(t *testing.T) {
		_, err := client.ApplyValidationSweep(context.Background(), &SweepReport{
			Namespace: plan.Namespace,
			OlderThan: plan.OlderThan,
			Objects:   plan.Objects,
		})
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})

	t.Run("JSON round trip", func(t *testing.T) {
		data, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		var decoded SweepReport
		if err = json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.planned || decoded.kubeconfig != "" {
			t.Fatal("the JSON round trip must drop the unexported marker and target")
		}
		_, err = client.ApplyValidationSweep(context.Background(), &decoded)
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})

	if len(f.applySeen) != 0 {
		t.Error("the sweep entry point was reached for a report PlanValidationSweep did not produce")
	}
}
