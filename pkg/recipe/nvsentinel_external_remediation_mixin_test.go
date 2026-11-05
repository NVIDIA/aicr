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

package recipe

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"text/template"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	externalRemediationMixin = "nvsentinel-external-remediation"
	externalRemediationKey   = "external-remediation"
)

// externalRemediationAction is upstream's documented ExternalRemediationRequest
// action at NVSentinel v1.28.0, with the template key's .yaml suffix dropped.
var externalRemediationAction = map[string]any{
	"apiGroup":              "nvsentinel.dgxc.nvidia.com",
	"version":               "v1",
	"kind":                  "ExternalRemediationRequest",
	"scope":                 "Cluster",
	"completeConditionType": "ExternalRemediationComplete",
	"templateFileName":      externalRemediationKey,
	"equivalenceGroup":      externalRemediationKey,
}

// externalRemediationMaintenance returns fault-remediation.maintenance from
// values or overrides.
func externalRemediationMaintenance(values map[string]any) (actions, templates map[string]any) {
	fr, _ := values["fault-remediation"].(map[string]any)
	maintenance, _ := fr["maintenance"].(map[string]any)
	actions, _ = maintenance["actions"].(map[string]any)
	templates, _ = maintenance["templates"].(map[string]any)
	return actions, templates
}

// TestMixinNVSentinelExternalRemediation_Composes pins what the mixin sets,
// alone, beside the remediation and observe steps, and beside every companion
// mixin that touches fault-remediation or the object monitor: upstream's
// action and its template, with nothing else, and the step's own action map
// left intact.
func TestMixinNVSentinelExternalRemediation_Composes(t *testing.T) {
	ctx, store := remediationStore(t)
	if _, ok := store.Mixins[externalRemediationMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", externalRemediationMixin, externalRemediationMixin)
	}

	combos := [][]string{
		{externalRemediationMixin},
		{remediationMixin, externalRemediationMixin},
		{observeMixin, externalRemediationMixin},
		{remediationMixin, healthEventsAnalyzerMixin, externalRemediationMixin},
		{remediationMixin, healthEventsAnalyzerMixin, logCollectorMixin, externalRemediationMixin},
		{remediationMixin, "nvsentinel-object-monitor", externalRemediationMixin},
		{observeMixin, "nvsentinel-object-monitor", externalRemediationMixin},
	}
	for _, mixins := range combos {
		t.Run(strings.Join(mixins, "+"), func(t *testing.T) {
			spec := remediationLeaf(mixins...)
			if _, err := store.mergeMixins(ctx, &spec); err != nil {
				t.Fatalf("mergeMixins: %v", err)
			}
			nvsentinel, ok := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
			if !ok {
				t.Fatal("nvsentinel missing from merged spec")
			}
			actions, templates := externalRemediationMaintenance(nvsentinel.Overrides)
			if got := actions[externalRemediationKey]; !reflect.DeepEqual(got, externalRemediationAction) {
				t.Errorf("fault-remediation.maintenance.actions.%s = %v, want exactly %v", externalRemediationKey, got, externalRemediationAction)
			}
			if _, ok := templates[externalRemediationKey].(string); !ok {
				t.Errorf("fault-remediation.maintenance.templates.%s missing", externalRemediationKey)
			}
			wantActions := []string{externalRemediationKey}
			wantTemplates := []string{externalRemediationKey}
			if slices.Contains(mixins, remediationMixin) || slices.Contains(mixins, observeMixin) {
				wantActions = append(wantActions, "COMPONENT_RESET")
				wantTemplates = append(wantTemplates, "gpureset")
				assertComponentResetIsGPUReset(t, strings.Join(mixins, "+"), nvsentinel.Overrides)
			}
			slices.Sort(wantActions)
			slices.Sort(wantTemplates)
			if got := slices.Sorted(maps.Keys(actions)); !slices.Equal(got, wantActions) {
				t.Errorf("fault-remediation.maintenance.actions keys = %v, want %v", got, wantActions)
			}
			if got := slices.Sorted(maps.Keys(templates)); !slices.Equal(got, wantTemplates) {
				t.Errorf("fault-remediation.maintenance.templates keys = %v, want %v", got, wantTemplates)
			}
		})
	}

	// Alone, the mixin touches only the action and its template.
	ref, ok := findComponentRefByName(store.Mixins[externalRemediationMixin].Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	if got := slices.Sorted(maps.Keys(ref.Overrides)); !slices.Equal(got, []string{"fault-remediation"}) {
		t.Errorf("mixin top-level overrides = %v, want [fault-remediation]", got)
	}
	fr, _ := ref.Overrides["fault-remediation"].(map[string]any)
	if got := slices.Sorted(maps.Keys(fr)); !slices.Equal(got, []string{"maintenance"}) {
		t.Errorf("mixin fault-remediation overrides = %v, want [maintenance]", got)
	}
	actions, templates := externalRemediationMaintenance(ref.Overrides)
	if len(actions) != 1 || len(templates) != 1 {
		t.Errorf("mixin sets actions %v and templates %v, want only %s in each", slices.Sorted(maps.Keys(actions)),
			slices.Sorted(maps.Keys(templates)), externalRemediationKey)
	}
	if n := len(store.Mixins[externalRemediationMixin].Spec.ComponentRefs); n != 1 {
		t.Errorf("mixin has %d componentRefs, want only nvsentinel: the pipeline comes from a remediation step", n)
	}
}

// TestMixinNVSentinelExternalRemediation_ComposesOntoEveryLeaf resolves every
// shipped leaf with the mixin composed beside the remediation step and beside
// the observe step, and requires the action and its template to survive into
// the resolved values unchanged.
func TestMixinNVSentinelExternalRemediation_ComposesOntoEveryLeaf(t *testing.T) {
	for _, step := range []string{remediationMixin, observeMixin} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			store, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
			if err != nil {
				t.Fatalf("buildMetadataStore: %v", err)
			}
			resolved := 0
			for name, overlay := range store.Overlays {
				if overlay.Spec.Criteria == nil {
					continue
				}
				overlay.Spec.Mixins = append(overlay.Spec.Mixins, step, externalRemediationMixin)
				t.Run(name, func(t *testing.T) {
					result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
					if err != nil {
						t.Fatalf("resolving %s with %s and %s composed: %v", name, step, externalRemediationMixin, err)
					}
					if !slices.Contains(result.Metadata.AppliedOverlays, name) {
						t.Fatalf("%s: criteria resolved to %v, which does not include it", name, result.Metadata.AppliedOverlays)
					}
					if _, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel"); !ok {
						return
					}
					values, err := result.GetValuesForComponentWithContext(ctx, "nvsentinel")
					if err != nil {
						t.Fatalf("%s: GetValuesForComponentWithContext: %v", name, err)
					}
					actions, templates := externalRemediationMaintenance(values)
					if got := actions[externalRemediationKey]; !reflect.DeepEqual(got, externalRemediationAction) {
						t.Errorf("%s: resolved action %s = %v, want %v", name, externalRemediationKey, got, externalRemediationAction)
					}
					if _, ok := templates[externalRemediationKey].(string); !ok {
						t.Errorf("%s: resolved values lack the %s template", name, externalRemediationKey)
					}
					assertComponentResetIsGPUReset(t, name, values)
				})
				resolved++
			}
			if resolved == 0 {
				t.Fatal("no leaves were resolved -- the overlay walker is broken")
			}
		})
	}
}

// externalRemediationRecommendedAction prints as fault-remediation's proto
// enum does.
type externalRemediationRecommendedAction int32

func (externalRemediationRecommendedAction) String() string { return "CUSTOM" }

// externalRemediationHealthEvent carries the HealthEvent fields the template
// reads, with fault-remediation's names.
type externalRemediationHealthEvent struct {
	Agent                   string
	ComponentClass          string
	CheckName               string
	NodeName                string
	IsFatal                 bool
	Message                 string
	RecommendedAction       externalRemediationRecommendedAction
	CustomRecommendedAction string
}

// externalRemediationTemplateData is shaped like fault-remediation's
// TemplateData (fault-remediation/pkg/remediation/
// fault_remediation_client_interface.go at v1.28.0).
type externalRemediationTemplateData struct {
	NodeName                 string
	ImpactedEntityScopeValue string
	HealthEventID            string
	RecommendedActionName    string
	TraceID                  string
	SpanID                   string
	HealthEvent              *externalRemediationHealthEvent
	ApiGroup                 string
	Version                  string
	Kind                     string
	Namespace                string
}

// TestExternalRemediationTemplateRenders executes the mixin's template as
// fault-remediation does -- text/template with no extra functions, then
// sigs.k8s.io/yaml -- for both datastores' event IDs and a message that would
// break unquoted YAML, and checks what janitor and its webhook rely on: a name
// that is a valid object name and taint value, the node the request is for,
// and the routing fields copied unchanged.
func TestExternalRemediationTemplateRenders(t *testing.T) {
	_, store := remediationStore(t)
	ref, ok := findComponentRefByName(store.Mixins[externalRemediationMixin].Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	_, templates := externalRemediationMaintenance(ref.Overrides)
	text, _ := templates[externalRemediationKey].(string)
	tmpl, err := template.New(externalRemediationKey).Parse(text)
	if err != nil {
		t.Fatalf("parse the %s template: %v", externalRemediationKey, err)
	}

	const node = "gpu-node-0.us-east-1.compute.internal"
	messages := []struct{ name, text string }{
		{"plain", "plain"},
		{"quotes colon newline", `condition "Repair": needed` + "\nsecond line\tafter a tab"},
		{"non-ASCII", "GPU défaillant – 修理    "},
		{"YAML indicators", "key: value # not a comment, {braces} [brackets] & *alias !tag %percent @at `tick`"},
	}
	ids := []struct{ name, id string }{
		{"MongoDB ObjectID", "65f0c0ffee0123456789abcd"},
		{"PostgreSQL UUID", "3f2b8c1e-9d4a-4f6b-8e2a-7c1d5e9f0a3b"},
	}
	for _, id := range ids {
		for _, m := range messages {
			message := m.text
			t.Run(id.name+"/"+m.name, func(t *testing.T) {
				event := &externalRemediationHealthEvent{
					Agent: "kubernetes-object-monitor", ComponentClass: "Node", CheckName: "platform-node-repair",
					NodeName: node, IsFatal: true, Message: message, CustomRecommendedAction: externalRemediationKey,
				}
				var out strings.Builder
				if err := tmpl.Execute(&out, externalRemediationTemplateData{
					NodeName: node, HealthEventID: id.id, RecommendedActionName: externalRemediationKey,
					TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331", HealthEvent: event,
					ApiGroup: "nvsentinel.dgxc.nvidia.com", Version: "v1", Kind: "ExternalRemediationRequest",
				}); err != nil {
					t.Fatalf("execute: %v", err)
				}
				var obj struct {
					APIVersion string `json:"apiVersion"`
					Kind       string `json:"kind"`
					Metadata   struct {
						Name        string            `json:"name"`
						Labels      map[string]string `json:"labels"`
						Annotations map[string]string `json:"annotations"`
					} `json:"metadata"`
					Spec struct {
						HealthEvent map[string]any `json:"healthEvent"`
					} `json:"spec"`
				}
				if err := yaml.UnmarshalStrict([]byte(out.String()), &obj); err != nil {
					t.Fatalf("rendered request is not YAML: %v\n%s", err, out.String())
				}
				if obj.APIVersion != "nvsentinel.dgxc.nvidia.com/v1" || obj.Kind != "ExternalRemediationRequest" {
					t.Errorf("apiVersion/kind = %s/%s", obj.APIVersion, obj.Kind)
				}
				name := obj.Metadata.Name
				if name != "extrr-"+id.id {
					t.Errorf("metadata.name = %q, want extrr-%s", name, id.id)
				}
				// janitor sets the name as the release taint's value.
				for _, problem := range append(validation.IsValidLabelValue(name), validation.IsDNS1123Subdomain(name)...) {
					t.Errorf("metadata.name %q: %s", name, problem)
				}
				if got := obj.Metadata.Annotations["nvsentinel.nvidia.com/preserve"]; got != "true" {
					t.Errorf("preserve annotation = %q, want \"true\"", got)
				}
				he := obj.Spec.HealthEvent
				want := map[string]any{
					"nodeName": node, "message": message, "recommendedAction": "CUSTOM",
					"customRecommendedAction": externalRemediationKey, "isFatal": true,
					"processingStrategy": "EXECUTE_REMEDIATION", "agent": "kubernetes-object-monitor",
				}
				for k, v := range want {
					if he[k] != v {
						t.Errorf("spec.healthEvent.%s = %#v, want %#v", k, he[k], v)
					}
				}
			})
		}
	}
}
