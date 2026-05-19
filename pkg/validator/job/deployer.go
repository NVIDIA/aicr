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

package job

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	v1 "github.com/NVIDIA/aicr/pkg/api/validator/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

// Deployer manages the lifecycle of a single validator Job.
type Deployer struct {
	clientset        kubernetes.Interface
	factory          informers.SharedInformerFactory
	namespace        string
	runID            string
	cliVersion       string // CLI version — forwarded to validator containers via AICR_CLI_VERSION for inner-image resolution
	cliCommit        string // CLI commit SHA — forwarded via AICR_CLI_COMMIT for dev-build image resolution
	entry            catalog.ValidatorEntry
	jobName          string // Unique name generated client-side (set by DeployJob)
	imagePullSecrets []string
	tolerations      []corev1.Toleration
	nodeSelector     map[string]string // passed through to inner workloads via AICR_NODE_SELECTOR env var
}

// NewDeployer creates a Deployer for a single validator catalog entry.
// The factory must be a namespace-scoped SharedInformerFactory started by the caller.
// cliVersion is the CLI's own version string; empty is acceptable for dev builds
// and is forwarded to the validator container via the AICR_CLI_VERSION env var so
// the validator can resolve images it references outside the catalog (e.g. the
// AIPerf benchmark image used by inference-perf) using the same rewriting
// rules as catalog.Load. cliCommit is the git commit SHA, forwarded via
// AICR_CLI_COMMIT for SHA-based image tag resolution in dev builds.
func NewDeployer(
	clientset kubernetes.Interface,
	factory informers.SharedInformerFactory,
	namespace, runID, cliVersion, cliCommit string,
	entry catalog.ValidatorEntry,
	imagePullSecrets []string,
	tolerations []corev1.Toleration,
	nodeSelector map[string]string,
) *Deployer {

	return &Deployer{
		clientset:        clientset,
		factory:          factory,
		namespace:        namespace,
		runID:            runID,
		cliVersion:       cliVersion,
		cliCommit:        cliCommit,
		entry:            entry,
		imagePullSecrets: imagePullSecrets,
		tolerations:      tolerations,
		nodeSelector:     nodeSelector,
	}
}

// JobName returns the Kubernetes Job name assigned by the API server.
// Empty until DeployJob is called.
func (d *Deployer) JobName() string {
	return d.jobName
}

// DeployJob creates the validator Job using server-side apply.
// A unique name is generated and the Job is applied with the aicr-validator field manager.
func (d *Deployer) DeployJob(ctx context.Context) error {
	// Build JobPlan from deployer configuration
	plan := v1.BuildJobPlan(
		d.entry,
		d.runID,
		d.namespace,
		d.cliVersion,
		d.cliCommit,
		ServiceAccountName(d.runID),
		d.imagePullSecrets,
		d.tolerations,
		d.nodeSelector,
		os.Getenv("AICR_VALIDATOR_IMAGE_REGISTRY"),
		os.Getenv("AICR_VALIDATOR_IMAGE_TAG"),
	)

	// Use the job name from the plan
	d.jobName = plan.JobName

	// Render Job ApplyConfiguration from plan
	jobApply := v1.RenderPlanToApplyConfig(plan, plan.JobName)

	// Apply the Job with server-side apply
	applied, err := d.clientset.BatchV1().Jobs(d.namespace).Apply(
		ctx, jobApply, metav1.ApplyOptions{FieldManager: labels.ValueAICR, Force: true})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to apply Job %s", plan.JobName), err)
	}

	d.jobName = applied.Name

	slog.Debug("validator Job applied",
		"job", d.jobName,
		"validator", d.entry.Name,
		"namespace", d.namespace)

	return nil
}

// CleanupJob deletes the validator Job with foreground propagation
// (waits for pod deletion).
func (d *Deployer) CleanupJob(ctx context.Context) error {
	if d.jobName == "" {
		return nil
	}
	propagation := metav1.DeletePropagationForeground
	err := d.clientset.BatchV1().Jobs(d.namespace).Delete(ctx, d.jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	return k8s.IgnoreNotFound(err)
}

// WaitForCompletion watches the Job until it reaches a terminal state
// (Complete or Failed). Returns nil for both — the caller uses ExtractResult
// to determine pass/fail/skip from the exit code.
//
// Returns error only for infrastructure failures (watch error, timeout).
// Job failure (exit != 0) is NOT an error return — that decision lives here
// in the validator orchestrator, not in the shared pod.WaitForJobTerminal
// helper, which intentionally treats both Complete and Failed Jobs as
// legitimate completions and lets the caller classify them.
func (d *Deployer) WaitForCompletion(ctx context.Context, timeout time.Duration) error {
	waitTimeout := timeout + defaults.ValidatorWaitBuffer
	// pod.WaitForJobTerminal already returns structured errors with proper
	// codes (ErrCodeTimeout, ErrCodeUnavailable, ErrCodeInternal). Propagate
	// as-is so callers can distinguish retryable from terminal failures.
	if _, err := pod.WaitForJobTerminal(ctx, d.clientset, d.namespace, d.jobName, waitTimeout); err != nil {
		return err
	}
	return nil
}

// WaitForPodTermination watches the Job's pod until it reaches a terminal
// state. Prevents RBAC cleanup from racing with in-progress pod operations.
//
// Returns the underlying error from pod.WaitForTermination so callers can
// decide log severity. A nil error means the pod is gone or terminal; a
// non-nil error means the wait was abandoned (timeout, watch failure, or
// repeated watch closures) and the cleanup may race with an in-progress pod.
func (d *Deployer) WaitForPodTermination(ctx context.Context) error {
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		// Pod-not-found is the expected steady state once the Job's TTL
		// controller or foreground-propagation delete has already run.
		// Anything else (RBAC, transient API failure, timeout) must
		// propagate so the caller can decide whether to retry or escalate.
		var sErr *errors.StructuredError
		if stderrors.As(err, &sErr) && sErr.Code == errors.ErrCodeNotFound {
			slog.Debug("no pod found, skipping termination wait", "job", d.jobName)
			return nil
		}
		return err
	}

	if jobPod.Status.Phase == corev1.PodSucceeded || jobPod.Status.Phase == corev1.PodFailed {
		return nil
	}

	slog.Debug("waiting for pod termination", "pod", jobPod.Name)
	return pod.WaitForTermination(ctx, d.clientset, d.namespace, jobPod.Name)
}
