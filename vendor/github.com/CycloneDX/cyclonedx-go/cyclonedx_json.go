// This file is part of CycloneDX Go
//
// Licensed under the Apache License, Version 2.0 (the “License”);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an “AS IS” BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0
// Copyright (c) OWASP Foundation. All Rights Reserved.

package cyclonedx

import (
	"encoding/json"
	"errors"
	"fmt"
)

func (ev EnvironmentVariableChoice) MarshalJSON() ([]byte, error) {
	if ev.Property != nil && *ev.Property != (Property{}) {
		return json.Marshal(ev.Property)
	} else if ev.Value != "" {
		return json.Marshal(ev.Value)
	}

	return []byte("{}"), nil
}

func (ev *EnvironmentVariableChoice) UnmarshalJSON(bytes []byte) error {
	var property Property
	err := json.Unmarshal(bytes, &property)
	if err != nil {
		var ute *json.UnmarshalTypeError
		if !errors.As(err, &ute) || ute.Value != "string" {
			return err
		}
	}

	if property != (Property{}) {
		ev.Property = &property
		return nil
	}

	var value string
	err = json.Unmarshal(bytes, &value)
	if err != nil {
		var ute *json.UnmarshalTypeError
		if !errors.As(err, &ute) || ute.Value != "object" {
			return err
		}
	}

	ev.Value = value
	return nil
}

type mlDatasetChoiceRefJSON struct {
	Ref string `json:"ref" xml:"-"`
}

func (dc MLDatasetChoice) MarshalJSON() ([]byte, error) {
	if dc.Ref != "" {
		return json.Marshal(mlDatasetChoiceRefJSON{Ref: dc.Ref})
	} else if dc.ComponentData != nil {
		return json.Marshal(dc.ComponentData)
	}

	return []byte("{}"), nil
}

func (dc *MLDatasetChoice) UnmarshalJSON(bytes []byte) error {
	var refObj mlDatasetChoiceRefJSON
	err := json.Unmarshal(bytes, &refObj)
	if err != nil {
		return err
	}

	if refObj.Ref != "" {
		dc.Ref = refObj.Ref
		return nil
	}

	var componentData ComponentData
	err = json.Unmarshal(bytes, &componentData)
	if err != nil {
		return err
	}

	if componentData != (ComponentData{}) {
		dc.ComponentData = &componentData
	}

	return nil
}

func (sv SpecVersion) MarshalJSON() ([]byte, error) {
	return json.Marshal(sv.String())
}

func (sv *SpecVersion) UnmarshalJSON(bytes []byte) error {
	var v string
	err := json.Unmarshal(bytes, &v)
	if err != nil {
		return err
	}

	switch v {
	case SpecVersion1_0.String():
		*sv = SpecVersion1_0
	case SpecVersion1_1.String():
		*sv = SpecVersion1_1
	case SpecVersion1_2.String():
		*sv = SpecVersion1_2
	case SpecVersion1_3.String():
		*sv = SpecVersion1_3
	case SpecVersion1_4.String():
		*sv = SpecVersion1_4
	case SpecVersion1_5.String():
		*sv = SpecVersion1_5
	case SpecVersion1_6.String():
		*sv = SpecVersion1_6
	default:
		return ErrInvalidSpecVersion
	}

	return nil
}

type toolsChoiceJSON struct {
	Components *[]Component `json:"components,omitempty" xml:"-"`
	Services   *[]Service   `json:"services,omitempty" xml:"-"`
}

func (tc ToolsChoice) MarshalJSON() ([]byte, error) {
	if tc.Tools != nil && (tc.Components != nil || tc.Services != nil) {
		return nil, fmt.Errorf("either a list of tools, or an object holding components and services can be used, but not both")
	}

	if tc.Tools != nil {
		return json.Marshal(tc.Tools)
	}

	choiceJSON := toolsChoiceJSON{
		Components: tc.Components,
		Services:   tc.Services,
	}
	if choiceJSON.Components != nil || choiceJSON.Services != nil {
		return json.Marshal(choiceJSON)
	}

	return []byte(nil), nil
}

func (tc *ToolsChoice) UnmarshalJSON(bytes []byte) error {
	var choiceJSON toolsChoiceJSON
	err := json.Unmarshal(bytes, &choiceJSON)
	if err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) || typeErr.Value != "array" {
			return err
		}

		var legacyTools []Tool
		err = json.Unmarshal(bytes, &legacyTools)
		if err != nil {
			return err
		}

		*tc = ToolsChoice{Tools: &legacyTools}
		return nil
	}

	if choiceJSON.Components != nil || choiceJSON.Services != nil {
		*tc = ToolsChoice{
			Components: choiceJSON.Components,
			Services:   choiceJSON.Services,
		}
	}

	return nil
}

func (ev *Evidence) UnmarshalJSON(bytes []byte) error {
	type Alias Evidence
	var aux = &struct {
		Identity json.RawMessage `json:"identity"`
		*Alias
	}{
		Alias: (*Alias)(ev),
	}

	if err := json.Unmarshal(bytes, &aux); err != nil {
		return err
	}

	// Try to unmarshal Identity field as struct
	if aux.Identity != nil {
		var identity EvidenceIdentity
		err := json.Unmarshal(aux.Identity, &identity)
		if err != nil {
			var typeErr *json.UnmarshalTypeError
			if !errors.As(err, &typeErr) || typeErr.Value != "array" {
				return err
			}

			// Try to unmarshal Identity field as array
			var identities []EvidenceIdentity
			err = json.Unmarshal(aux.Identity, &identities)
			if err != nil {
				return err
			}

			if len(identities) > 0 {
				ev.Identity = &identities
			}
			return nil
		}

		// The field is required, so we can check for emptiness using this field.
		// cf. https://cyclonedx.org/docs/1.6/json/#metadata_component_evidence_identity_oneOf_i0_items_field
		if identity.Field != "" {
			ev.Identity = &[]EvidenceIdentity{
				identity,
			}
		}
	}
	return nil
}

var jsonSchemas = map[SpecVersion]string{
	SpecVersion1_0: "",
	SpecVersion1_1: "",
	SpecVersion1_2: "http://cyclonedx.org/schema/bom-1.2.schema.json",
	SpecVersion1_3: "http://cyclonedx.org/schema/bom-1.3.schema.json",
	SpecVersion1_4: "http://cyclonedx.org/schema/bom-1.4.schema.json",
	SpecVersion1_5: "http://cyclonedx.org/schema/bom-1.5.schema.json",
	SpecVersion1_6: "http://cyclonedx.org/schema/bom-1.6.schema.json",
}
