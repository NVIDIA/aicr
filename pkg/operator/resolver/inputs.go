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

package resolver

import (
	"encoding/json"
	"strings"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// Inputs are everything a resolution depends on. Their digest, inputDigest,
// is provenance: it decides whether to resolve again, never
// whether to roll out; desiredStateDigest decides that. Optional fields are
// omitted when unset, so a runtime that leaves a newer field unset keeps the
// inputDigest it had before the field existed.
type Inputs struct {
	RuntimeUID      string                        `json:"runtimeUID"`
	Intent          string                        `json:"intent"`
	Platform        string                        `json:"platform,omitempty"`
	Profile         string                        `json:"profile,omitempty"`
	Criteria        *v1alpha1.DeclaredCriteria    `json:"criteria,omitempty"`
	Placement       v1alpha1.Placement            `json:"placement"`
	Configuration   *v1alpha1.RecipeConfiguration `json:"configuration,omitempty"`
	Overrides       []v1alpha1.Override           `json:"overrides,omitempty"`
	AICRImage       string                        `json:"aicrImage"`
	AICRVersion     string                        `json:"aicrVersion"`
	OperatorVersion string                        `json:"operatorVersion"`
	Engine          plan.Engine                   `json:"engine"`
	EnginePolicy    string                        `json:"enginePolicy"`
	Registry        string                        `json:"registry"`
	// SnapshotDigest is the AICR snapshot the recipe was resolved from;
	// empty without discovery.
	SnapshotDigest string `json:"snapshotDigest,omitempty"`
}

// NewInputs collects a runtime's resolution inputs.
func NewInputs(rt *v1alpha1.ClusterRuntime, aicrImage, aicrVersion, operatorVersion string,
	engine plan.Engine, enginePolicy, registry string) *Inputs {

	s := &rt.Spec
	return &Inputs{
		RuntimeUID: string(rt.UID), Intent: s.Intent, Platform: s.Platform, Profile: s.Profile,
		Criteria: s.Criteria, Placement: s.Placement, Configuration: s.Configuration, Overrides: s.Overrides,
		AICRImage: aicrImage, AICRVersion: aicrVersion, OperatorVersion: operatorVersion,
		Engine: engine, EnginePolicy: enginePolicy, Registry: registry,
	}
}

// Digest returns the inputDigest: SHA-256 over the inputs' canonical JSON.
func (in *Inputs) Digest() (string, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return "", operrors.Wrap(operrors.ErrCodeInternal, "encode resolution inputs", err)
	}
	return plan.DigestOf(data), nil
}

// ShortDigest is the first 16 hex characters of a digest, the form labels carry.
func ShortDigest(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")[:16]
}
