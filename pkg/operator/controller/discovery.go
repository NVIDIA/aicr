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

package controller

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/config"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/discovery"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/resolver"
)

// gpuStackRelease is the release whose install or upgrade must be
// confirmed by a new snapshot.
const gpuStackRelease = "gpu-operator"

// discovered is what discovery contributes to a resolution.
type discovered struct {
	groups   *discovery.Groups
	snapshot *plan.Content
	observed metav1.Time
	// criteria are AICR's snapshot-derived dimensions.
	criteria discovery.Criteria
}

// digest is the snapshot's digest; empty without discovery.
func (d *discovered) digest() string {
	if d == nil {
		return ""
	}
	return d.snapshot.Digest
}

// snapshotInput is what a snapshot answers. A change to any field
// takes a new snapshot; another node with a known tuple does not.
type snapshotInput struct {
	Accelerated v1alpha1.NodeGroupPlacement `json:"accelerated"`
	AICRImage   string                      `json:"aicrImage"`
	Tuples      string                      `json:"tuples"`
	Period      int64                       `json:"period"`
	Resnapshot  string                      `json:"resnapshot,omitempty"`
	GPUStack    string                      `json:"gpuStack,omitempty"`
}

// discover observes the node groups, reuses or takes the snapshot for the
// current snapshot input, and checks AICR's fingerprint of it against the
// declared criteria and the active plan's. It returns nil and no
// error while the snapshot Job runs.
func (r *ResolverReconciler) discover(ctx context.Context, rt *v1alpha1.ClusterRuntime, st *stages) (*discovered, time.Duration, error) {
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes); err != nil {
		return nil, 0, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list Nodes")
	}
	groups := discovery.Observe(nodes.Items, &rt.Spec.Placement)
	if err := groups.Check(); err != nil {
		return nil, 0, terminal(err)
	}
	active, _, err := r.activePlan(ctx, rt)
	if err != nil {
		return nil, 0, err
	}
	in := snapshotInput{
		Accelerated: rt.Spec.Placement.Accelerated, AICRImage: r.Config.AICRImage, Tuples: groups.TupleDigest(),
		Period:     r.now().Unix() / int64(defaults.SnapshotRefreshInterval/time.Second),
		Resnapshot: rt.Annotations[v1alpha1.AnnotationResnapshot],
	}
	if rel := release(active, gpuStackRelease); rel != nil {
		in.GPUStack = rel.Chart.Digest + "," + rel.Values.Digest
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, 0, operrors.Wrap(operrors.ErrCodeInternal, "encode snapshot input", err)
	}
	input := resolver.ShortDigest(plan.DigestOf(data))

	found, observed, err := r.storedSnapshot(ctx, rt, input)
	if err != nil {
		return nil, 0, err
	}
	if found == nil {
		job, wait, driveErr := st.drive(ctx, rt, r.snapshotRun(rt, input))
		if driveErr != nil || job == nil {
			return nil, wait, driveErr
		}
		if found, observed, err = r.storeSnapshot(ctx, rt, st, job, input); found == nil || err != nil {
			return nil, pollInterval, err
		}
	}
	snap, err := aicrjob.ParseSnapshot(found.Data, r.Config.AICRVersion)
	if err != nil {
		return nil, 0, terminal(err)
	}
	// Only a plan resolved from a snapshot has criteria a new snapshot
	// confirms; one resolved from declared criteria alone has none.
	var activeCriteria map[string]string
	if active != nil && active.Recipe != nil && active.Provenance.SnapshotDigest != "" {
		activeCriteria = active.Recipe.Criteria
	}
	criteria, err := discovery.CheckSnapshot(snap, rt.Spec.Criteria, activeCriteria)
	if err != nil {
		return nil, 0, terminal(err)
	}
	return &discovered{groups: groups, snapshot: found, observed: observed, criteria: criteria}, 0, nil
}

// snapshotRun is the snapshot stage: AICR's launcher in the discovery
// namespace, its agent pinned to the accelerated group.
func (r *ResolverReconciler) snapshotRun(rt *v1alpha1.ClusterRuntime, input string) *stageRun {
	return snapshotRun(r.Config, rt, input)
}

func snapshotRun(cfg *config.Config, rt *v1alpha1.ClusterRuntime, input string) *stageRun {
	return &stageRun{
		stage: aicrjob.StageSnapshot, input: input, serviceAccount: cfg.SnapshotServiceAccount,
		args: func(req *aicrjob.Request) ([]string, error) {
			return resolver.SnapshotArgs(&rt.Spec.Placement, cfg.DiscoveryNamespace, cfg.SnapshotAgentServiceAccount,
				cfg.AICRImage, cfg.SnapshotAgentRequests, aicrjob.OutputURI(req))
		},
	}
}

// storedSnapshot finds the snapshot kept for a snapshot input in the
// operator namespace: the newest by creation, verified against its digest.
func (r *ResolverReconciler) storedSnapshot(ctx context.Context, rt *v1alpha1.ClusterRuntime, input string) (*plan.Content, metav1.Time, error) {
	list := &corev1.ConfigMapList{}
	err := r.Reader.List(ctx, list, client.InNamespace(r.Config.OperatorNamespace), client.MatchingLabels{
		v1alpha1.LabelRole: v1alpha1.RoleSnapshot, v1alpha1.LabelRuntime: rt.Name, v1alpha1.LabelInput: input,
	})
	if err != nil {
		return nil, metav1.Time{}, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list snapshots")
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[j].CreationTimestamp.Before(&list.Items[i].CreationTimestamp)
	})
	for i := range list.Items {
		cm := &list.Items[i]
		if cm.Annotations[v1alpha1.AnnotationRuntimeUID] != string(rt.UID) {
			continue
		}
		data, readErr := plan.ReadContent(cm, plan.SnapshotDataKey)
		if readErr != nil {
			log.FromContext(ctx).Info("ignoring an unreadable snapshot", "configMap", cm.Name, "error", readErr.Error())
			continue
		}
		return plan.SnapshotContent(data), cm.CreationTimestamp, nil
	}
	return nil, metav1.Time{}, nil
}

// storeSnapshot keeps a finished snapshot run's output in the operator
// namespace, removes older snapshots of the runtime, then removes the run:
// the snapshot outlives its Job. A lost output runs the snapshot again. A
// rollout's deployment snapshot goes with its rollout, never here.
func (r *ResolverReconciler) storeSnapshot(ctx context.Context, rt *v1alpha1.ClusterRuntime, st *stages, job *batchv1.Job,
	input string) (*plan.Content, metav1.Time, error) {

	data, err := st.output(ctx, job)
	if operrors.CodeOf(err) == operrors.ErrCodeNotFound {
		log.FromContext(ctx).Info("snapshot run output is gone; running it again", "job", job.Name)
		return nil, metav1.Time{}, st.remove(ctx, job)
	}
	if err != nil {
		return nil, metav1.Time{}, err
	}
	if _, err = aicrjob.ParseSnapshot(data, r.Config.AICRVersion); err != nil {
		return nil, metav1.Time{}, terminal(err)
	}
	content := plan.SnapshotContent(data)
	cm, err := content.ConfigMap(r.Config.OperatorNamespace, rt.Name, string(rt.UID), map[string]string{v1alpha1.LabelInput: input})
	if err != nil {
		return nil, metav1.Time{}, terminal(err)
	}
	if err = r.createOnce(ctx, cm, v1alpha1.AnnotationDigest, content.Digest); err != nil {
		return nil, metav1.Time{}, err
	}
	old := &corev1.ConfigMapList{}
	if err = r.Reader.List(ctx, old, client.InNamespace(r.Config.OperatorNamespace),
		client.MatchingLabels{v1alpha1.LabelRole: v1alpha1.RoleSnapshot, v1alpha1.LabelRuntime: rt.Name}); err != nil {
		return nil, metav1.Time{}, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list snapshots")
	}
	for i := range old.Items {
		if o := &old.Items[i]; o.Name != content.Name && len(o.OwnerReferences) == 0 {
			if err = r.Client.Delete(ctx, o); client.IgnoreNotFound(err) != nil {
				return nil, metav1.Time{}, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "delete snapshot "+o.Name)
			}
		}
	}
	return content, metav1.NewTime(r.now()), st.remove(ctx, job)
}

// activePlan reads the plan the runtime runs. found is false before the
// first rollout and when the active plan is lost, which does not block
// discovery.
func (r *ResolverReconciler) activePlan(ctx context.Context, rt *v1alpha1.ClusterRuntime) (p *plan.Document, found bool, err error) {
	if rt.Status.ActivePlanDigest == "" {
		return nil, false, nil
	}
	p, err = r.readPlan(ctx, rt.Status.ActivePlanDigest)
	switch {
	case err != nil && operrors.IsPermanent(err):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return p, true, nil
}

func (r *ResolverReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
