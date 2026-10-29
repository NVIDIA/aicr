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

// Package evidence binds AICR's deployment validation evidence to what the
// operator rolled out. AICR's in-toto statement attests the
// recipe, the validators and their results; it knows nothing of the plan,
// the operator or the approval. The operator pushes its own statement whose
// subject is AICR's evidence artifact and whose predicate names those, and
// records that statement's digest as the rollout's evidence.
package evidence

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Statement and predicate identifiers.
const (
	// StatementType is the in-toto Statement v1 type.
	StatementType = "https://in-toto.io/Statement/v1"
	// BindingPredicateType identifies the operator's binding predicate.
	BindingPredicateType = "https://operator.aicr.run/evidence-binding/v1"
	// BindingArtifactType is the OCI artifact type of a pushed binding.
	BindingArtifactType = "application/vnd.aicr.operator.evidence-binding.v1+json"
	// AICRStatementFile is where AICR's statement lives in its evidence bundle.
	AICRStatementFile = "statement.intoto.json"
	// aicrPredicatePrefix prefixes AICR's recipe-evidence predicate types.
	aicrPredicatePrefix = "https://aicr.run/recipe-evidence/"
)

// AICRSummary is what the operator reads from AICR's statement.
type AICRSummary struct {
	PredicateType   string
	AICRVersion     string
	AICRCommit      string
	RecipeName      string
	RecipeDigest    string
	ValidatorImages []ValidatorImage
	Phases          map[string]PhaseResult
	// KeyFingerprint identifies the KMS key that signed the evidence, when
	// it is key-signed.
	KeyFingerprint string
}

// ValidatorImage is a validator image and, when known, its digest.
type ValidatorImage struct {
	Image  string `json:"image"`
	Digest string `json:"digest,omitempty"`
}

// PhaseResult counts a validation phase's checks.
type PhaseResult struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// ParseAICRStatement reads the summary of AICR's evidence statement.
func ParseAICRStatement(data []byte) (*AICRSummary, error) {
	var s struct {
		Type          string `json:"_type"`
		PredicateType string `json:"predicateType"`
		Predicate     struct {
			AICRVersion     string           `json:"aicrVersion"`
			AICRCommit      string           `json:"aicrCommit"`
			ValidatorImages []ValidatorImage `json:"validatorImages"`
			Recipe          struct{ Name, Digest string }
			Phases          map[string]PhaseResult `json:"phases"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInvalidRequest, "AICR evidence statement is not JSON", err)
	}
	if s.Type != StatementType || !strings.HasPrefix(s.PredicateType, aicrPredicatePrefix) {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest,
			fmt.Sprintf("AICR evidence statement has type %q and predicate %q", s.Type, s.PredicateType))
	}
	p := &s.Predicate
	return &AICRSummary{
		PredicateType: s.PredicateType, AICRVersion: p.AICRVersion, AICRCommit: p.AICRCommit,
		RecipeName: p.Recipe.Name, RecipeDigest: p.Recipe.Digest, ValidatorImages: p.ValidatorImages, Phases: p.Phases,
	}, nil
}

// Passed reports whether every phase that ran passed with no failure.
func (s *AICRSummary) Passed() bool {
	if len(s.Phases) == 0 {
		return false
	}
	for _, p := range s.Phases {
		if p.Failed > 0 || p.Passed == 0 {
			return false
		}
	}
	return true
}

// CheckValidators confirms the validators AICR names are the pinned ones:
// pinned maps an image repository (no tag or digest) to its pinned
// repository:tag@digest reference. Every validator AICR names must be
// pinned, at the pinned tag, and at the pinned digest when AICR records one;
// AICR v1.0.x records tags only, and refuses a mutable tag itself.
func CheckValidators(named []ValidatorImage, pinned map[string]string) error {
	if len(named) == 0 {
		return operrors.New(operrors.ErrCodeInvalidRequest, "the evidence names no validator image")
	}
	var mismatched []string
	for _, v := range named {
		repo, tag, dgst := splitRef(v.Image)
		if v.Digest != "" {
			dgst = v.Digest
		}
		want, ok := pinned[repo]
		if !ok {
			mismatched = append(mismatched, v.Image+atDigest(v.Digest)+" is not pinned")
			continue
		}
		_, wantTag, wantDigest := splitRef(want)
		if tag != wantTag || (dgst != "" && dgst != wantDigest) {
			mismatched = append(mismatched, fmt.Sprintf("%s ran, %s is pinned", v.Image+atDigest(v.Digest), want))
		}
	}
	if len(mismatched) == 0 {
		return nil
	}
	sort.Strings(mismatched)
	return operrors.New(operrors.ErrCodeInvalidRequest, "validator images differ from the pinned ones: "+strings.Join(mismatched, "; "))
}

func atDigest(d string) string {
	if d == "" {
		return ""
	}
	return "@" + d
}

// splitRef splits an image reference into repository, tag and digest.
func splitRef(ref string) (repo, tag, dgst string) {
	repo, dgst, _ = strings.Cut(ref, "@")
	if colon := strings.LastIndex(repo, ":"); colon > strings.LastIndex(repo, "/") {
		repo, tag = repo[:colon], repo[colon+1:]
	}
	return repo, tag, dgst
}

// Repository returns the repository part of an image reference.
func Repository(ref string) string {
	repo, _, _ := splitRef(ref)
	return repo
}

// Subject names the artifact a statement is about.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Binding is the operator's in-toto statement about one validation run.
type Binding struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Predicate `json:"predicate"`
}

// Predicate binds AICR's evidence to the rollout it validated.
type Predicate struct {
	// Qualifying is false for evidence that never counts toward
	// qualification: a run under the development approval bypass.
	Qualifying bool   `json:"qualifying"`
	Result     string `json:"result"`

	RuntimeUID     string            `json:"runtimeUID"`
	Rollout        string            `json:"rollout"`
	RunID          string            `json:"runID"`
	PlanDigest     string            `json:"planDigest"`
	InputDigest    string            `json:"inputDigest,omitempty"`
	SnapshotDigest string            `json:"snapshotDigest,omitempty"`
	Criteria       map[string]string `json:"criteria,omitempty"`
	Charts         []Chart           `json:"charts"`

	Operator Image `json:"operator"`
	AICR     Image `json:"aicr"`

	// ValidatorImages are the pinned validator images (installation
	// values); AICR's statement records what ran.
	ValidatorImages []ValidatorImage `json:"validatorImages,omitempty"`
	Approval        Approval         `json:"approval"`

	AICREvidence AICREvidence `json:"aicrEvidence"`
}

// Chart is one release's chart from the plan.
type Chart struct {
	Release    string `json:"release"`
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
}

// Image is an image reference, the digest it ran as, and its version.
type Image struct {
	Image   string `json:"image"`
	Digest  string `json:"digest,omitempty"`
	Version string `json:"version,omitempty"`
}

// Approval is the rollout's approval record.
type Approval struct {
	// State is NotRequired, Approved or Bypassed.
	State      string `json:"state"`
	ApprovedBy string `json:"approvedBy,omitempty"`
	Reason     string `json:"reason,omitempty"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	// Acknowledgements record who attested each manual release's
	// post-apply steps.
	Acknowledgements []Acknowledgement `json:"acknowledgements,omitempty"`
}

// Acknowledgement is an approver's statement that a release's post-apply
// steps are done.
type Acknowledgement struct {
	Release        string `json:"release"`
	AcknowledgedBy string `json:"acknowledgedBy"`
	AcknowledgedAt string `json:"acknowledgedAt,omitempty"`
}

// AICREvidence is AICR's evidence artifact and its summary.
type AICREvidence struct {
	Reference       string                 `json:"reference"`
	PredicateType   string                 `json:"predicateType"`
	AICRCommit      string                 `json:"aicrCommit,omitempty"`
	RecipeName      string                 `json:"recipeName"`
	RecipeDigest    string                 `json:"recipeDigest"`
	ValidatorImages []ValidatorImage       `json:"validatorImages"`
	Phases          map[string]PhaseResult `json:"phases"`
	// KeyFingerprint is the public-key fingerprint of the KMS key that
	// signed AICR's evidence; empty for unsigned evidence.
	KeyFingerprint string `json:"keyFingerprint,omitempty"`
}

// NewBinding returns the statement binding AICR's evidence, pushed as
// evidenceRef (repository@digest), to the predicate.
func NewBinding(evidenceRef, evidenceDigest string, aicr *AICRSummary, p Predicate) ([]byte, error) {
	hexDigest, ok := strings.CutPrefix(evidenceDigest, "sha256:")
	if !ok || len(hexDigest) != 64 {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, "evidence digest "+evidenceDigest+" is not a sha256 digest")
	}
	p.AICREvidence = AICREvidence{
		Reference: evidenceRef, PredicateType: aicr.PredicateType, AICRCommit: aicr.AICRCommit,
		RecipeName: aicr.RecipeName, RecipeDigest: aicr.RecipeDigest, ValidatorImages: aicr.ValidatorImages, Phases: aicr.Phases,
		KeyFingerprint: aicr.KeyFingerprint,
	}
	p.Result = "Failed"
	if aicr.Passed() {
		p.Result = "Passed"
	}
	sort.Slice(p.Charts, func(i, j int) bool { return p.Charts[i].Release < p.Charts[j].Release })
	data, err := json.Marshal(Binding{
		Type:          StatementType,
		Subject:       []Subject{{Name: evidenceRef, Digest: map[string]string{"sha256": hexDigest}}},
		PredicateType: BindingPredicateType,
		Predicate:     p,
	})
	if err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "encode evidence binding", err)
	}
	return data, nil
}

// ParseBinding reads a binding statement back.
func ParseBinding(data []byte) (*Binding, error) {
	b := &Binding{}
	if err := json.Unmarshal(data, b); err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInvalidRequest, "evidence binding is not JSON", err)
	}
	if b.Type != StatementType || b.PredicateType != BindingPredicateType || len(b.Subject) != 1 {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, "not an operator evidence binding")
	}
	return b, nil
}
