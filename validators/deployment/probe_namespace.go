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

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/validator/labels"
	"aicr.run/validators"
)

// runIDEnv carries the orchestrator's run ID into the validator pod.
const runIDEnv = "AICR_RUN_ID"

// probeNamespace is the per-run namespace a check creates its probe pods in,
// following the conformance checks' gpuTestRun: named
// defaults.GPUTestNamespacePrefix plus a 128-bit random token and labeled
// labels.GPUTestRun=token, so `aicr sweep` reclaims it if the check is killed
// before its cleanup runs. Deleting it deletes every probe pod with it.
//
// observed, foreign and neverCreated carry the create's outcome to delete:
//   - observed: the namespace was seen (Create succeeded, or a Get after
//     AlreadyExists found this run's token), and uid is its UID.
//   - foreign: the name is held by a namespace without this run's token,
//     which delete must never touch.
//   - neverCreated: the apiserver definitively rejected the Create.
//
// None set means the namespace was never seen with this run's token: the
// Create failed ambiguously and may still have persisted, or an existing
// namespace's labels could not be read. delete resolves either by name and
// token.
type probeNamespace struct {
	token        string
	name         string
	uid          types.UID
	observed     bool
	foreign      bool
	neverCreated bool

	imagePullSecrets   []string
	validatorNamespace string
}

// newProbeNamespace draws the run token and reads the forwarded
// --image-pull-secret names, each once: a name given twice would otherwise be
// copied twice and fail on AlreadyExists.
func newProbeNamespace() (*probeNamespace, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate the probe namespace token", err)
	}
	token := hex.EncodeToString(b)
	return &probeNamespace{
		token:              token,
		name:               defaults.GPUTestNamespacePrefix + token,
		imagePullSecrets:   uniqueNames(validators.ImagePullSecretNames()),
		validatorNamespace: validators.ValidatorNamespace(),
	}, nil
}

// labels are stamped on the namespace and on every object created in it: the
// ownership token, and the orchestrator's run ID when it is a valid label
// value.
func (p *probeNamespace) labels() map[string]string {
	out := map[string]string{labels.GPUTestRun: p.token}
	if runID := os.Getenv(runIDEnv); runID != "" {
		if errs := validation.IsValidLabelValue(runID); len(errs) == 0 {
			out[labels.RunID] = runID
		} else {
			slog.Warn("not labeling probe objects with an invalid run ID", "runID", runID, "errors", errs)
		}
	}
	return out
}

// imagePullSecretRefs references the copied pull Secrets from a pod spec.
func (p *probeNamespace) imagePullSecretRefs() []corev1.LocalObjectReference {
	if len(p.imagePullSecrets) == 0 {
		return nil
	}
	refs := make([]corev1.LocalObjectReference, 0, len(p.imagePullSecrets))
	for _, name := range p.imagePullSecrets {
		refs = append(refs, corev1.LocalObjectReference{Name: name})
	}
	return refs
}

// create creates the namespace and copies the --image-pull-secret Secrets
// into it. An AlreadyExists namespace carrying this run's token is this run's
// own ambiguously committed create and is adopted; any other is foreign and
// fails with ErrCodeConflict.
func (p *probeNamespace) create(ctx context.Context, client kubernetes.Interface) error {
	opCtx, cancel := context.WithTimeout(ctx, defaults.ResourceVerificationTimeout)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.name, Labels: p.labels()}}
	created, err := client.CoreV1().Namespaces().Create(opCtx, ns, metav1.CreateOptions{})
	switch {
	case err == nil:
		p.observed, p.uid = true, created.UID
	case k8serrors.IsAlreadyExists(err):
		existing, getErr := client.CoreV1().Namespaces().Get(opCtx, p.name, metav1.GetOptions{})
		if getErr != nil {
			// Ownership is unknown, not foreign: delete reads the namespace
			// again and removes it only when it carries this run's token.
			return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf(
				"probe namespace %s already exists and its ownership could not be verified", p.name), getErr)
		}
		if existing.Labels[labels.GPUTestRun] != p.token {
			p.foreign = true
			return errors.New(errors.ErrCodeConflict, fmt.Sprintf(
				"probe namespace %s already exists without this run's %s label; refusing to reuse it",
				p.name, labels.GPUTestRun))
		}
		p.observed, p.uid = true, existing.UID
	default:
		p.neverCreated = createDefinitelyRejected(err)
		return errors.Wrap(errors.ErrCodeInternal, "failed to create probe namespace "+p.name, err)
	}

	for _, name := range p.imagePullSecrets {
		if err := p.copyImagePullSecret(ctx, client, name); err != nil {
			return err
		}
	}
	return nil
}

// copyImagePullSecret copies one --image-pull-secret Secret from the
// validator namespace: a pod resolves imagePullSecrets only in its own
// namespace.
func (p *probeNamespace) copyImagePullSecret(ctx context.Context, client kubernetes.Interface, name string) error {
	opCtx, cancel := context.WithTimeout(ctx, defaults.ImagePullSecretCopyTimeout)
	defer cancel()

	source, err := client.CoreV1().Secrets(p.validatorNamespace).Get(opCtx, name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf(
				"image pull secret %q named by --image-pull-secret does not exist in the validator namespace %s",
				name, p.validatorNamespace), err)
		}
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf(
			"failed to read image pull secret %q in the validator namespace %s", name, p.validatorNamespace), err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.name, Labels: p.labels()},
		Type:       source.Type,
		Data:       source.Data,
	}
	if _, err := client.CoreV1().Secrets(p.name).Create(opCtx, secret, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf(
			"failed to copy image pull secret %q into probe namespace %s", name, p.name), err)
	}
	return nil
}

// delete deletes the namespace this run created, on a fresh context so it
// runs after the check's own budget is spent. The delete is UID-guarded, so
// it can never remove a namespace recreated at the same name. It returns once
// the delete is accepted; the namespace controller removes the probe pods.
// An ambiguous create is resolved by name and token first; a namespace that
// has not surfaced by then carries the ownership label for `aicr sweep`.
func (p *probeNamespace) delete(client kubernetes.Interface) error {
	if p.foreign || p.neverCreated {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
	defer cancel()

	if !p.observed {
		existing, err := client.CoreV1().Namespaces().Get(ctx, p.name, metav1.GetOptions{})
		switch {
		case k8serrors.IsNotFound(err):
			slog.Warn("probe namespace never surfaced after an ambiguous create; sweep reclaims it if it does",
				"namespace", p.name, "label", labels.GPUTestRun+"="+p.token)
			return nil
		case err != nil:
			return errors.Wrap(errors.ErrCodeInternal, "CLEANUP FAILED: could not read probe namespace "+p.name, err)
		case existing.Labels[labels.GPUTestRun] != p.token:
			return nil
		}
		p.uid = existing.UID
	}

	opts := metav1.DeleteOptions{}
	if p.uid != "" {
		uid := p.uid
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	err := client.CoreV1().Namespaces().Delete(ctx, p.name, opts)
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf(
			"CLEANUP FAILED: probe namespace %s was not deleted; its probe pods may still hold devices", p.name), err)
	}
	return nil
}

// uniqueNames returns names without repeats, in first-seen order.
func uniqueNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// createDefinitelyRejected reports whether a Create error is a verdict the
// apiserver rendered before storage (authn, authz, admission, validation),
// so the object cannot have persisted. Anything else may have committed.
func createDefinitelyRejected(err error) bool {
	return k8serrors.IsForbidden(err) ||
		k8serrors.IsUnauthorized(err) ||
		k8serrors.IsInvalid(err) ||
		k8serrors.IsBadRequest(err) ||
		k8serrors.IsMethodNotSupported(err) ||
		k8serrors.IsNotAcceptable(err) ||
		k8serrors.IsUnsupportedMediaType(err) ||
		k8serrors.IsRequestEntityTooLargeError(err)
}
