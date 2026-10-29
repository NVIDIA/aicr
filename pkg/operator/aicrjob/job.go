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

package aicrjob

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Stage is one AICR command the operator runs as a Job.
type Stage string

// Stages.
const (
	StageSnapshot   Stage = "snapshot"
	StageRecipe     Stage = "recipe"
	StageBaseRender Stage = "base-render"
	StageRender     Stage = "render"
	StageAssessment Stage = "assessment"
	StageValidation Stage = "validation"
)

// Paths inside every AICR Job container.
const (
	// ScratchPath is the size-limited writable directory; the root file
	// system is read-only.
	ScratchPath = "/scratch"
	// RegistryConfigPath holds the per-run docker config for pushing stages.
	RegistryConfigPath = "/run/aicr-registry"
	// TerminationMessagePath is where a failed run's log tail lands, and
	// where AICR v1.1.0 and later write the digest a pushing run published
	// (--image-refs). v1.0.x cannot: it renames a temporary file onto the
	// target, and the kubelet-mounted file cannot be replaced.
	TerminationMessagePath = "/dev/termination-log"
)

// sha256Digest is an OCI sha256 digest, the form --image-refs writes.
var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ImageRefsArgs make a pushing run report the digest it published through
// its termination message, when the AICR version writes it in place.
func ImageRefsArgs(aicrVersion string) []string {
	if !SupportsV11(aicrVersion) {
		return nil
	}
	return []string{"--image-refs", TerminationMessagePath}
}

// PushedDigest is the digest a pushing run reported through its termination
// message. It is false when the AICR version does not report one, or the
// message is not a digest: the pod is gone, or a failed run stopped before
// it pushed. The caller then looks the run's tag up in the registry.
func PushedDigest(aicrVersion string, o Outcome) (string, bool) {
	if !SupportsV11(aicrVersion) || !sha256Digest.MatchString(o.Message) {
		return "", false
	}
	return o.Message, true
}

type stageSpec struct {
	subcommand string
	namespace  func(Namespaces) string
	deadline   time.Duration
	// pushes: the stage pushes to the registry and needs a credential Secret.
	pushes bool
	// output: the stage writes its result to its run-scoped output ConfigMap.
	output bool
}

var stages = map[Stage]stageSpec{
	StageSnapshot:   {"snapshot", func(n Namespaces) string { return n.Discovery }, defaults.SnapshotJobDeadline, false, true},
	StageRecipe:     {"recipe", func(n Namespaces) string { return n.Jobs }, defaults.RecipeJobDeadline, false, true},
	StageBaseRender: {"bundle", func(n Namespaces) string { return n.Jobs }, defaults.RenderJobDeadline, true, false},
	StageRender:     {"bundle", func(n Namespaces) string { return n.Jobs }, defaults.RenderJobDeadline, true, false},
	StageAssessment: {"upgrade-check", func(n Namespaces) string { return n.Jobs }, defaults.AssessmentJobDeadline, false, true},
	StageValidation: {"validate", func(n Namespaces) string { return n.Validation }, defaults.ValidationJobDeadline, true, false},
}

// Namespaces are the installation's AICR Job namespaces.
type Namespaces struct {
	Jobs       string
	Discovery  string
	Validation string
}

// Request describes one AICR Job.
type Request struct {
	Stage       Stage
	RunID       string
	RuntimeName string
	RuntimeUID  string
	// Image is the pinned AICR image; it must be digest-pinned.
	Image string
	// Args follow the AICR subcommand.
	Args           []string
	Namespaces     Namespaces
	ServiceAccount string
	// CredentialSecret is the per-run docker config Secret; required for
	// stages that push. It is created after the Job, owned by it.
	CredentialSecret string
	// Deadline overrides the stage deadline when shorter (for example the
	// remaining rollout deadline for validation).
	Deadline time.Duration
	// Input is the first 16 hex characters of the inputDigest the run
	// serves, if any; it lets a restarted controller find the run by label.
	Input string
	// Attempt numbers the run among its stage's runs for the same input.
	Attempt int
	// NodeSelector and Tolerations place the Job's pod; empty places none.
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
}

var (
	runIDPattern  = regexp.MustCompile(`^[a-z0-9]{12}$`)
	inputPattern  = regexp.MustCompile(`^[a-f0-9]{16}$`)
	digestPinned  = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
	dnsNamePrefix = "aicr-"
)

// NewRunID returns a fresh run ID: 12 lowercase hex characters.
func NewRunID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", operrors.Wrap(operrors.ErrCodeInternal, "generate run ID", err)
	}
	return hex.EncodeToString(b), nil
}

// JobName returns the Job name for a stage and run.
func JobName(stage Stage, runID string) string {
	return fmt.Sprintf("%s%s-%s", dnsNamePrefix, stage, runID)
}

// Pushes reports whether a stage pushes to the registry and so needs a
// per-run credential Secret.
func (s Stage) Pushes() bool { return stages[s].pushes }

// WritesOutput reports whether a stage writes its result to an output
// ConfigMap (cm://).
func (s Stage) WritesOutput() bool { return stages[s].output }

// Namespace returns the namespace a stage's Jobs run in.
func (s Stage) Namespace(n Namespaces) string { return stages[s].namespace(n) }

func (r *Request) validate() (stageSpec, error) {
	invalid := func(format string, args ...any) error {
		return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	spec, ok := stages[r.Stage]
	switch {
	case !ok:
		return spec, invalid("unknown AICR stage %q", r.Stage)
	case !runIDPattern.MatchString(r.RunID):
		return spec, invalid("run ID %q is not 12 lowercase hex characters", r.RunID)
	case r.Input != "" && !inputPattern.MatchString(r.Input):
		return spec, invalid("input %q is not 16 lowercase hex characters", r.Input)
	case !digestPinned.MatchString(r.Image):
		return spec, invalid("AICR image %q is not pinned by digest", r.Image)
	case spec.namespace(r.Namespaces) == "":
		return spec, invalid("no namespace configured for stage %s", r.Stage)
	case r.ServiceAccount == "":
		return spec, invalid("stage %s has no ServiceAccount", r.Stage)
	case r.RuntimeName == "" || r.RuntimeUID == "":
		return spec, invalid("stage %s is not bound to a runtime", r.Stage)
	case spec.pushes && r.CredentialSecret == "":
		return spec, invalid("stage %s pushes and needs a per-run credential Secret", r.Stage)
	}
	return spec, nil
}

// Build renders the Job for a request. Every Job runs the pinned image with
// no retries, a deadline, a TTL backstop and the restricted pod security
// profile. Every Job is created suspended: the caller creates the run's
// Job-owned objects (Output, InputCopy, CredentialSecret), then resumes it,
// so garbage collection removes them with the Job even when the operator is
// down.
func Build(r *Request) (*batchv1.Job, error) {
	spec, err := r.validate()
	if err != nil {
		return nil, err
	}
	deadline := spec.deadline
	if r.Deadline > 0 && r.Deadline < deadline {
		deadline = r.Deadline
	}
	labels := map[string]string{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelRuntime:   r.RuntimeName,
		v1alpha1.LabelRunID:     r.RunID,
		v1alpha1.LabelStage:     string(r.Stage),
	}
	if r.Input != "" {
		labels[v1alpha1.LabelInput] = r.Input
	}
	scratchLimit := resource.MustParse(defaults.JobScratchSizeLimit)

	container := corev1.Container{
		Name:  "aicr",
		Image: r.Image,
		Args:  append([]string{spec.subcommand}, r.Args...),
		// AICR stages files relative to its working directory (bundle
		// staging), so it must be the writable scratch volume, not the image's
		// read-only working directory.
		WorkingDir: ScratchPath,
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: ScratchPath},
			{Name: "TMPDIR", Value: ScratchPath},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(defaults.JobCPURequest),
				corev1.ResourceMemory: resource.MustParse(defaults.JobMemoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse(defaults.JobMemoryLimit),
			},
		},
		VolumeMounts:           []corev1.VolumeMount{{Name: "scratch", MountPath: ScratchPath}},
		TerminationMessagePath: TerminationMessagePath,
		// AICR writes errors only to its log, so a failure falls back to the
		// log tail, which carries AICR's error line.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
	volumes := []corev1.Volume{{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &scratchLimit}},
	}}
	if spec.pushes {
		container.Env = append(container.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: RegistryConfigPath})
		container.VolumeMounts = append(container.VolumeMounts,
			corev1.VolumeMount{Name: "registry-credentials", MountPath: RegistryConfigPath, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "registry-credentials",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: r.CredentialSecret,
				Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
			}},
		})
	}

	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        JobName(r.Stage, r.RunID),
			Namespace:   spec.namespace(r.Namespaces),
			Labels:      labels,
			Annotations: map[string]string{v1alpha1.AnnotationRuntimeUID: r.RuntimeUID, v1alpha1.AnnotationAttempt: strconv.Itoa(max(r.Attempt, 1))},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](0),
			// A pod evicted by a drain, scale-down or node repair carries
			// DisruptionTarget: the Job replaces it without counting a
			// failure, so the run is not spent. With a pod failure
			// policy the replacement starts only once the evicted pod has
			// terminated, so two never run at once.
			PodFailurePolicy: &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{{
				Action:          batchv1.PodFailurePolicyActionIgnore,
				OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}},
			}}},
			ActiveDeadlineSeconds:   ptr.To(int64(deadline.Seconds())),
			TTLSecondsAfterFinished: ptr.To(int32(defaults.JobTTLAfterFinished.Seconds())),
			Suspend:                 ptr.To(true),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: r.ServiceAccount,
					NodeSelector:       r.NodeSelector,
					Tolerations:        r.Tolerations,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
					Volumes:    volumes,
				},
			},
		},
	}, nil
}

// DeletePropagation is the propagation every AICR Job deletion uses. The
// batch/v1 default orphans the Job's pods; Foreground keeps the Job until its
// pods are gone, so its run-scoped objects never disappear under a running pod.
const DeletePropagation = metav1.DeletePropagationForeground
