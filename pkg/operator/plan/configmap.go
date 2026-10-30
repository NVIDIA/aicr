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
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

const (
	// DataKey holds the plan document in the ConfigMap.
	DataKey = "plan.json"
	// NamePrefix starts every plan ConfigMap name.
	NamePrefix = "aicr-plan-"
	// MaxConfigMapBytes is the ConfigMap size limit (1 MiB).
	MaxConfigMapBytes = 1 << 20
	// ReasonPlanUnreadable is the condition reason for a plan the operator
	// refuses to read (docs/operator/api.md#plan-configmap).
	ReasonPlanUnreadable = "PlanUnreadable"
)

// Name returns the ConfigMap name for a desiredStateDigest: the prefix plus
// the first 16 hex characters of the digest.
func Name(desiredStateDigest string) (string, error) {
	if !digestPattern.MatchString(desiredStateDigest) {
		return "", operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf("%q is not a sha256 digest", desiredStateDigest))
	}
	return NamePrefix + strings.TrimPrefix(desiredStateDigest, "sha256:")[:16], nil
}

// Encode validates doc and renders it as the canonical JSON stored in the
// ConfigMap. Struct field order makes the encoding deterministic.
func Encode(doc *Document) ([]byte, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "encode plan document", err)
	}
	return data, nil
}

// ToConfigMap renders doc as the immutable plan ConfigMap.
func ToConfigMap(doc *Document, namespace, runtimeName string) (*corev1.ConfigMap, error) {
	data, err := Encode(doc)
	if err != nil {
		return nil, err
	}
	name, err := Name(doc.DesiredStateDigest)
	if err != nil {
		return nil, err
	}
	immutable := true
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:     v1alpha1.ManagedByValue,
				v1alpha1.LabelRuntime:       runtimeName,
				v1alpha1.LabelRole:          v1alpha1.RolePlan,
				v1alpha1.LabelSchemaVersion: strconv.Itoa(doc.SchemaVersion),
			},
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeUID: doc.RuntimeUID,
				v1alpha1.AnnotationPlanDigest: doc.DesiredStateDigest,
			},
		},
		Immutable: &immutable,
		Data:      map[string]string{DataKey: string(data)},
	}
	if size := dataSize(cm); size > MaxConfigMapBytes {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest,
			fmt.Sprintf("plan ConfigMap is %d bytes; the limit is %d", size, MaxConfigMapBytes))
	}
	return cm, nil
}

// FromConfigMap reads a plan, refusing anything it cannot trust: a schema
// version it does not support, a label that disagrees with the payload, a
// payload digest that disagrees with the annotation, an invalid document, a
// recorded digest its content does not reproduce, or a ConfigMap not named
// for its digest.
// Every refusal is INVALID_REQUEST with reason PlanUnreadable in its context;
// the operator never guesses.
func FromConfigMap(cm *corev1.ConfigMap) (*Document, error) {
	unreadable := func(format string, args ...any) error {
		return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf(format, args...)).
			WithContext(map[string]any{operrors.ReasonKey: ReasonPlanUnreadable, "configMap": cm.Namespace + "/" + cm.Name})
	}
	if cm.Labels[v1alpha1.LabelRole] != v1alpha1.RolePlan {
		return nil, unreadable("ConfigMap is not a plan (role %q)", cm.Labels[v1alpha1.LabelRole])
	}
	labelVersion, err := strconv.Atoi(cm.Labels[v1alpha1.LabelSchemaVersion])
	if err != nil || !SupportedSchemaVersions[labelVersion] {
		return nil, unreadable("unsupported plan schema version label %q", cm.Labels[v1alpha1.LabelSchemaVersion])
	}
	raw, ok := cm.Data[DataKey]
	if !ok {
		return nil, unreadable("plan ConfigMap has no %s", DataKey)
	}
	doc := &Document{}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	if err := dec.Decode(doc); err != nil {
		return nil, unreadable("decode plan document: %v", err)
	}
	if doc.SchemaVersion != labelVersion {
		return nil, unreadable("payload schemaVersion %d disagrees with label %d", doc.SchemaVersion, labelVersion)
	}
	if doc.DesiredStateDigest != cm.Annotations[v1alpha1.AnnotationPlanDigest] {
		return nil, unreadable("payload digest %q disagrees with annotation %q", doc.DesiredStateDigest, cm.Annotations[v1alpha1.AnnotationPlanDigest])
	}
	if err := doc.Validate(); err != nil {
		return nil, unreadable("invalid plan document: %v", err)
	}
	computed, digestErr := ComputeDesiredStateDigest(doc)
	if digestErr != nil {
		return nil, unreadable("digest plan document: %v", digestErr)
	}
	if computed != doc.DesiredStateDigest {
		return nil, unreadable("plan content digests to %s, not its recorded %s", computed, doc.DesiredStateDigest)
	}
	if name, _ := Name(doc.DesiredStateDigest); name != cm.Name {
		return nil, unreadable("plan %s is stored as ConfigMap %s, not %s", doc.DesiredStateDigest, cm.Name, name)
	}
	return doc, nil
}

// dataSize is the size the API server limits: the total length of every data
// key and value.
func dataSize(cm *corev1.ConfigMap) int {
	n := 0
	for k, v := range cm.Data {
		n += len(k) + len(v)
	}
	for k, v := range cm.BinaryData {
		n += len(k) + len(v)
	}
	return n
}
