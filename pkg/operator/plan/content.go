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

package plan

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// ReasonContentTooLarge marks a recipe or release values document that does
// not fit one ConfigMap.
const ReasonContentTooLarge = "ContentTooLarge"

// Data keys of the content ConfigMaps a plan references.
const (
	RecipeDataKey     = "recipe.json"
	ValuesDataKey     = "values.json"
	SnapshotDataKey   = "snapshot.json"
	AssessmentDataKey = "assessment.json"
)

// Content is a content-addressed document a plan references: the recipe it
// was resolved from or one release's values. Its ConfigMap is immutable and
// named by the digest, so creating it twice is harmless.
type Content struct {
	Role   string
	Name   string
	Key    string
	Data   []byte
	Digest string
}

// RecipeContent returns the recipe content for a RecipeResult document.
func RecipeContent(recipeJSON []byte) *Content {
	d := DigestOf(recipeJSON)
	return &Content{Role: v1alpha1.RoleRecipe, Name: "aicr-recipe-" + short(d), Key: RecipeDataKey, Data: recipeJSON, Digest: d}
}

// ValuesContent returns a release's canonical values content.
func ValuesContent(releaseID string, valuesJSON []byte) *Content {
	d := DigestOf(valuesJSON)
	return &Content{Role: v1alpha1.RoleValues, Name: "aicr-values-" + releaseID + "-" + short(d), Key: ValuesDataKey, Data: valuesJSON, Digest: d}
}

// SnapshotContent returns the content of an AICR Snapshot document, kept
// in the operator namespace so a resolution survives its Job.
func SnapshotContent(snapshotJSON []byte) *Content {
	d := DigestOf(snapshotJSON)
	return &Content{Role: v1alpha1.RoleSnapshot, Name: "aicr-snapshot-" + short(d), Key: SnapshotDataKey, Data: snapshotJSON, Digest: d}
}

// RolloutSnapshotContent is SnapshotContent for the snapshot one rollout
// validates against: its name carries the run input, so it never shares a
// ConfigMap with discovery's snapshot of the same data.
func RolloutSnapshotContent(snapshotJSON []byte, input string) *Content {
	c := SnapshotContent(snapshotJSON)
	c.Name = "aicr-snapshot-" + input + "-" + short(c.Digest)
	return c
}

// AssessmentContent returns the content of an AICR upgrade-check report,
// which a rollout references.
func AssessmentContent(reportJSON []byte) *Content {
	d := DigestOf(reportJSON)
	return &Content{Role: v1alpha1.RoleAssessment, Name: "aicr-assessment-" + short(d), Key: AssessmentDataKey, Data: reportJSON, Digest: d}
}

// Ref returns the plan reference to the content.
func (c *Content) Ref() DigestedRef { return DigestedRef{ConfigMap: c.Name, Digest: c.Digest} }

// ConfigMap renders the immutable ConfigMap holding the content. labels are
// added to the operator's own (for example the resolution input).
func (c *Content) ConfigMap(namespace, runtimeName, runtimeUID string, labels map[string]string) (*corev1.ConfigMap, error) {
	if len(c.Data) > MaxConfigMapBytes {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s is %d bytes; a ConfigMap holds at most %d", c.Name, len(c.Data), MaxConfigMapBytes)).
			WithReason(ReasonContentTooLarge)
	}
	l := map[string]string{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelRuntime:   runtimeName,
		v1alpha1.LabelRole:      c.Role,
	}
	for k, v := range labels {
		l[k] = v
	}
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name: c.Name, Namespace: namespace, Labels: l,
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeUID: runtimeUID,
				v1alpha1.AnnotationDigest:     c.Digest,
			},
		},
		Immutable: ptr.To(true),
		Data:      map[string]string{c.Key: string(c.Data)},
	}, nil
}

// ReadContent returns a content ConfigMap's document after checking it is
// the content its name and annotation claim: the digest of the data must
// equal the annotation. A ConfigMap that fails is never trusted.
func ReadContent(cm *corev1.ConfigMap, key string) ([]byte, error) {
	raw, ok := cm.Data[key]
	if !ok {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf("ConfigMap %s has no %s", cm.Name, key))
	}
	d := DigestOf([]byte(raw))
	if d != cm.Annotations[v1alpha1.AnnotationDigest] || !strings.HasSuffix(cm.Name, "-"+short(d)) {
		return nil, operrors.New(operrors.ErrCodeConflict,
			fmt.Sprintf("ConfigMap %s holds content %s, not the digest it records", cm.Name, d))
	}
	return []byte(raw), nil
}

// short is the first 16 hex characters of a sha256 digest.
func short(digest string) string { return strings.TrimPrefix(digest, "sha256:")[:16] }
