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
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/validator/sweep"
)

// SweepOptions configures a validation-residue sweep: the cluster to sweep
// and how stale an object must be before it qualifies.
type SweepOptions struct {
	// Kubeconfig selects the cluster, resolved exactly like every other
	// cluster-touching facade method: explicit path, KUBECONFIG, the default
	// location, then in-cluster. Empty means that chain.
	Kubeconfig string

	// Namespace is the validation namespace holding run-scoped RBAC and
	// ConfigMaps. Empty selects defaults.ValidationNamespace.
	Namespace string

	// OlderThan is the age floor: only objects older than it are candidates.
	// Zero selects defaults.ValidationSweepMinAge. Negative is rejected; the
	// floor is the backstop that keeps the sweep from racing a live run, so
	// it is never silently weakened.
	OlderThan time.Duration
}

// SweepStatus is what the sweep did or would do with one object. The values
// mirror pkg/validator/sweep's statuses without aliasing them: the facade
// owns its contract.
type SweepStatus string

const (
	// SweepWouldDelete marks a planned candidate (a plan, or a dry run).
	SweepWouldDelete SweepStatus = "would-delete"
	// SweepDeleted marks an object whose delete the API server accepted, or
	// which was already gone.
	SweepDeleted SweepStatus = "deleted"
	// SweepDeleting marks a namespace whose delete was accepted; termination
	// is not awaited.
	SweepDeleting SweepStatus = "deleting"
	// SweepSkipped marks an object a guard kept; Reason names the guard.
	SweepSkipped SweepStatus = "skipped"
	// SweepTerminating marks an object whose deletion already started.
	SweepTerminating SweepStatus = "terminating"
	// SweepFailed marks an object whose delete failed.
	SweepFailed SweepStatus = "failed"
)

// SweepObject is one cluster object the sweep examined.
//
// Facade-owned rather than an alias of pkg/validator/sweep.Object: the report
// carries JSON and YAML tags that define the shape of the CLI's published
// --format output, so aliasing would freeze the internal package's layout as
// SDK contract (the same reasoning as MirrorInventory).
type SweepObject struct {
	Kind      string        `json:"kind" yaml:"kind"`
	Namespace string        `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name      string        `json:"name" yaml:"name"`
	UID       string        `json:"uid,omitempty" yaml:"uid,omitempty"`
	Age       time.Duration `json:"age" yaml:"age"`
	RunID     string        `json:"runID,omitempty" yaml:"runID,omitempty"`
	Status    SweepStatus   `json:"status" yaml:"status"`
	Reason    string        `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// SweepReport is the outcome of PlanValidationSweep or ApplyValidationSweep:
// the cluster it ran against and every object examined, in deletion order.
type SweepReport struct {
	// ClusterContext is the kubeconfig's current context name, empty for an
	// in-cluster configuration.
	ClusterContext string `json:"clusterContext,omitempty" yaml:"clusterContext,omitempty"`

	// Server is the API server URL the sweep talked to.
	Server string `json:"server" yaml:"server"`

	// Namespace is the validation namespace that was swept for run-scoped
	// objects.
	Namespace string `json:"namespace" yaml:"namespace"`

	// OlderThan is the effective age floor that was applied.
	OlderThan time.Duration `json:"olderThan" yaml:"olderThan"`

	// DryRun marks a report nothing was deleted for.
	DryRun bool `json:"dryRun" yaml:"dryRun"`

	// Objects is everything examined: candidates, kept objects with their
	// guards named, and objects already terminating.
	Objects []SweepObject `json:"objects" yaml:"objects"`

	// kubeconfig is the path the plan was built from, carried so
	// ApplyValidationSweep re-deletes against the cluster the user confirmed.
	// planned marks a report PlanValidationSweep produced. Both are
	// unexported, so a hand-built or JSON-deserialized report has neither,
	// and ApplyValidationSweep refuses it: a caller-invented candidate list
	// must not reach a cluster the caller never planned against.
	kubeconfig string
	planned    bool
}

// Candidates returns the objects marked SweepWouldDelete — the exact set
// ApplyValidationSweep will attempt.
func (r *SweepReport) Candidates() []SweepObject {
	var out []SweepObject
	for _, o := range r.Objects {
		if o.Status == SweepWouldDelete {
			out = append(out, o)
		}
	}
	return out
}

// PlanValidationSweep lists the stale validation resources on the cluster
// opts names, deleting nothing. The returned report names the cluster context
// and API server it read so a caller can show the operator exactly what would
// be deleted, and where, before confirming.
//
// Errors:
//   - ErrCodeInvalidRequest when opts are invalid (a negative OlderThan) or
//     no kube configuration resolves.
//   - ErrCodeUnauthorized when the caller cannot list a resource family.
//   - ErrCodeTimeout/ErrCodeCanceled when ctx dies.
//   - ErrCodeInternal for any other API failure.
func (c *Client) PlanValidationSweep(ctx context.Context, opts SweepOptions) (*SweepReport, error) {
	sopts, err := opts.normalized()
	if err != nil {
		return nil, err
	}
	kube, dyn, restConfig, err := sweepClients(opts.Kubeconfig)
	if err != nil {
		return nil, err
	}
	planned, err := c.deps.sweepPlan(ctx, kube, dyn, sopts)
	if err != nil {
		return nil, err
	}
	report := sweepReportFrom(planned)
	report.kubeconfig = opts.Kubeconfig
	report.planned = true
	report.ClusterContext, report.Server = sweepTarget(opts.Kubeconfig, restConfig.Host)
	return report, nil
}

// ApplyValidationSweep deletes the candidates of a plan PlanValidationSweep
// returned. The plan's kubeconfig binds the cluster; apply then re-plans
// against that cluster's current state with the plan's namespace and age
// floor and deletes only the intersection — objects a fresh plan still
// marks would-delete with the same UID. A candidate the re-plan no longer
// selects is reported SweepSkipped, so a stale or tampered report cannot
// widen the deletion set. Every delete is pinned to the planned object's
// UID, so an object replaced since planning is left alone, and a re-run is
// idempotent.
//
// Only a report PlanValidationSweep produced is accepted: the marker is
// unexported, so a hand-built SweepReport — or a plan that round-tripped
// through JSON, which drops unexported fields — is rejected; re-plan first.
//
// Errors:
//   - ErrCodeInvalidRequest when plan is nil or was not produced by
//     PlanValidationSweep, or the re-plan rejects the plan's age floor.
//   - ErrCodeUnauthorized when the re-plan's list is Forbidden — before
//     anything is deleted.
//   - ErrCodeTimeout/ErrCodeCanceled when ctx dies, including during the
//     settle window — again before anything is deleted.
//   - ErrCodeInternal for partial delete failures, still returning the
//     report with the failed objects marked SweepFailed (a Forbidden delete
//     is additionally reported per object with ErrCodeUnauthorized), and
//     for any other API failure.
func (c *Client) ApplyValidationSweep(ctx context.Context, plan *SweepReport) (*SweepReport, error) {
	if plan == nil || !plan.planned {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"plan was not produced by PlanValidationSweep")
	}
	kube, dyn, _, err := sweepClients(plan.kubeconfig)
	if err != nil {
		return nil, err
	}
	applied := plan.toSweepReport()
	applyErr := c.deps.sweepApply(ctx, kube, dyn, applied)
	report := sweepReportFrom(applied)
	report.kubeconfig = plan.kubeconfig
	report.planned = plan.planned
	report.ClusterContext, report.Server = plan.ClusterContext, plan.Server
	if applyErr != nil {
		return report, applyErr
	}
	return report, nil
}

// normalized applies facade defaults and validates.
func (o SweepOptions) normalized() (sweep.Options, error) {
	if o.OlderThan < 0 {
		return sweep.Options{}, errors.New(errors.ErrCodeInvalidRequest,
			"OlderThan must not be negative")
	}
	if o.Namespace == "" {
		o.Namespace = defaults.ValidationNamespace
	}
	if o.OlderThan == 0 {
		o.OlderThan = defaults.ValidationSweepMinAge
	}
	return sweep.Options{Namespace: o.Namespace, OlderThan: o.OlderThan}, nil
}

// sweepClients builds the typed and dynamic clients from one resolved
// kubeconfig, so both halves of a sweep talk to the same cluster (the same
// reasoning as inventory.Read).
func sweepClients(kubeconfig string) (kubernetes.Interface, dynamic.Interface, *rest.Config, error) {
	typed, restConfig, err := k8sclient.GetKubeClientWithConfig(kubeconfig)
	if err != nil {
		return nil, nil, nil, err
	}
	d, err := k8sclient.NewDynamicClientForConfig(restConfig)
	if err != nil {
		return nil, nil, nil, err
	}
	return typed, d, restConfig, nil
}

// sweepTarget resolves the display identity of the cluster a sweep ran
// against: the kubeconfig's current context and the API server URL. The
// context name is empty for an in-cluster configuration, which carries no
// kubeconfig context.
func sweepTarget(kubeconfig, server string) (contextName, host string) {
	host = server
	if rules := k8sclient.KubeconfigLoadingRules(kubeconfig); rules != nil {
		raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).RawConfig()
		if err == nil {
			contextName = raw.CurrentContext
		}
	}
	return contextName, host
}

// sweepReportFrom maps the internal plan onto the facade's report type.
func sweepReportFrom(r *sweep.Report) *SweepReport {
	out := &SweepReport{
		Namespace: r.Namespace,
		OlderThan: r.OlderThan,
		Objects:   make([]SweepObject, 0, len(r.Objects)),
	}
	for _, o := range r.Objects {
		out.Objects = append(out.Objects, SweepObject{
			Kind:      o.Kind,
			Namespace: o.Namespace,
			Name:      o.Name,
			UID:       string(o.UID),
			Age:       o.Age,
			RunID:     o.RunID,
			Status:    SweepStatus(o.Status),
			Reason:    o.Reason,
		})
	}
	return out
}

// toSweepReport maps the facade report back onto the internal plan type.
// Objects keep the status the facade reported; sweep.Execute re-plans before
// deleting, so only candidates a fresh plan still selects are attempted.
func (r *SweepReport) toSweepReport() *sweep.Report {
	out := &sweep.Report{
		Namespace: r.Namespace,
		OlderThan: r.OlderThan,
		Objects:   make([]sweep.Object, 0, len(r.Objects)),
	}
	for _, o := range r.Objects {
		out.Objects = append(out.Objects, sweep.Object{
			Kind:      o.Kind,
			Namespace: o.Namespace,
			Name:      o.Name,
			UID:       types.UID(o.UID),
			Age:       o.Age,
			RunID:     o.RunID,
			Status:    sweep.Status(o.Status),
			Reason:    o.Reason,
		})
	}
	return out
}
