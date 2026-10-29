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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

func TestName(t *testing.T) {
	t.Parallel()
	got, err := Name(digestA)
	if err != nil || got != "aicr-plan-aaaaaaaaaaaaaaaa" {
		t.Fatalf("Name() = %q, %v", got, err)
	}
	if _, err := Name("sha256:short"); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("Name(bad) = %v, want INVALID_REQUEST", err)
	}
}

func TestEncodeIsDeterministic(t *testing.T) {
	t.Parallel()
	a, err := Encode(validDocument())
	if err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	for range 10 {
		b, err := Encode(validDocument())
		if err != nil {
			t.Fatalf("Encode() = %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Fatal("Encode() is not deterministic")
		}
	}
	if _, err := Encode(&Document{}); err == nil {
		t.Fatal("Encode() accepted an invalid document")
	}
}

func TestConfigMapRoundTrip(t *testing.T) {
	t.Parallel()
	doc := validDocument()
	cm, err := ToConfigMap(doc, "aicr-system", v1alpha1.SingletonName)
	if err != nil {
		t.Fatalf("ToConfigMap() = %v", err)
	}
	if cm.Name != "aicr-plan-aaaaaaaaaaaaaaaa" || cm.Namespace != "aicr-system" {
		t.Errorf("name = %s/%s", cm.Namespace, cm.Name)
	}
	if cm.Immutable == nil || !*cm.Immutable {
		t.Error("plan ConfigMap must be immutable")
	}
	wantLabels := map[string]string{
		v1alpha1.LabelManagedBy:     v1alpha1.ManagedByValue,
		v1alpha1.LabelRuntime:       v1alpha1.SingletonName,
		v1alpha1.LabelRole:          v1alpha1.RolePlan,
		v1alpha1.LabelSchemaVersion: "1",
	}
	if !reflect.DeepEqual(cm.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", cm.Labels, wantLabels)
	}
	if cm.Annotations[v1alpha1.AnnotationPlanDigest] != digestA || cm.Annotations[v1alpha1.AnnotationRuntimeUID] != uid {
		t.Errorf("annotations = %v", cm.Annotations)
	}

	got, err := FromConfigMap(cm)
	if err != nil {
		t.Fatalf("FromConfigMap() = %v", err)
	}
	if !reflect.DeepEqual(got, doc) {
		t.Fatalf("round trip changed the document:\n got %+v\nwant %+v", got, doc)
	}
}

func TestToConfigMapRejectsOversizedPlan(t *testing.T) {
	t.Parallel()
	doc := validDocument()
	doc.Recipe.Warnings = []string{strings.Repeat("w", MaxConfigMapBytes)}
	if _, err := ToConfigMap(doc, "aicr-system", v1alpha1.SingletonName); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("ToConfigMap(oversized) = %v, want INVALID_REQUEST", err)
	}
}

// Every refusal is PlanUnreadable; the operator never guesses.
func TestFromConfigMapRefusesUntrustedPlans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*corev1.ConfigMap)
	}{
		{"not a plan", func(cm *corev1.ConfigMap) { cm.Labels[v1alpha1.LabelRole] = v1alpha1.RoleValues }},
		{"missing schema label", func(cm *corev1.ConfigMap) { delete(cm.Labels, v1alpha1.LabelSchemaVersion) }},
		{"unknown schema label", func(cm *corev1.ConfigMap) { cm.Labels[v1alpha1.LabelSchemaVersion] = "2" }},
		{"label disagrees with payload", func(cm *corev1.ConfigMap) {
			cm.Data[DataKey] = strings.Replace(cm.Data[DataKey], `"schemaVersion":1`, `"schemaVersion":2`, 1)
		}},
		{"digest disagrees with annotation", func(cm *corev1.ConfigMap) { cm.Annotations[v1alpha1.AnnotationPlanDigest] = digestB }},
		{"missing payload", func(cm *corev1.ConfigMap) { delete(cm.Data, DataKey) }},
		{"corrupt payload", func(cm *corev1.ConfigMap) { cm.Data[DataKey] = "{not json" }},
		{"invalid document", func(cm *corev1.ConfigMap) {
			cm.Data[DataKey] = strings.Replace(cm.Data[DataKey], `"engine":"ArgoCD"`, `"engine":"Nope"`, 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cm, err := ToConfigMap(validDocument(), "aicr-system", v1alpha1.SingletonName)
			if err != nil {
				t.Fatalf("ToConfigMap() = %v", err)
			}
			tt.mutate(cm)
			_, err = FromConfigMap(cm)
			var se *operrors.StructuredError
			if !asStructured(err, &se) || se.Code != operrors.ErrCodeInvalidRequest || se.Context["reason"] != ReasonPlanUnreadable {
				t.Fatalf("FromConfigMap() = %v, want INVALID_REQUEST with reason %s", err, ReasonPlanUnreadable)
			}
		})
	}
}

func asStructured(err error, target **operrors.StructuredError) bool {
	se, ok := err.(*operrors.StructuredError) //nolint:errorlint // FromConfigMap returns the structured error directly
	if ok {
		*target = se
	}
	return ok
}
