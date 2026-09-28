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

package redact

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestPodTemplateFieldsMatchAPI pins ctrfPodTemplateFields to the JSON field
// names reachable from the vendored core/v1 PodTemplateSpec, so a k8s.io/api
// bump that adds or renames a field fails here instead of silently dropping
// (or admitting) provenance paths.
func TestPodTemplateFieldsMatchAPI(t *testing.T) {
	want := map[string]struct{}{}
	walkJSONFields(reflect.TypeOf(corev1.PodTemplateSpec{}), map[reflect.Type]bool{}, want)
	for f := range want {
		if _, ok := ctrfPodTemplateFields[f]; !ok {
			t.Errorf("API field %q missing from ctrfPodTemplateFields", f)
		}
	}
	for f := range ctrfPodTemplateFields {
		if _, ok := want[f]; !ok {
			t.Errorf("ctrfPodTemplateFields names %q, which is not a PodTemplateSpec field", f)
		}
	}
	for _, m := range ctrfFreeKeyMaps {
		if _, ok := want[m]; !ok {
			t.Errorf("free-key map %q is not a PodTemplateSpec field", m)
		}
	}
}

func walkJSONFields(t reflect.Type, seen map[reflect.Type]bool, out map[string]struct{}) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			if f.Anonymous {
				walkJSONFields(f.Type, seen, out)
			}
			continue
		}
		out[tag] = struct{}{}
		walkJSONFields(f.Type, seen, out)
	}
}
