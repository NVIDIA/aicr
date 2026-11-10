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

package cncf

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Upstream PRODUCT.yaml status vocabulary at the pinned validator revision
// (validStatuses in scripts/validate.go). Partially Implemented is valid
// syntax upstream but never produced: a requirement is either established
// or not.
const (
	productImplemented    = "Implemented"
	productNotImplemented = "Not Implemented"
	productNotApplicable  = "N/A"
)

// productDocument is PRODUCT.yaml: upstream's metadata block and the spec
// categories in the checklist's order, each row carrying the upstream id,
// description and level verbatim with the status, evidence and notes this
// run produced.
type productDocument struct {
	Metadata productMetadata `yaml:"metadata"`
	Spec     productSpec     `yaml:"spec"`
}

// productMetadata uses upstream's camelCase field names; the validator
// accepts those or their snake_case forms.
type productMetadata struct {
	KubernetesVersion   string `yaml:"kubernetesVersion"`
	PlatformName        string `yaml:"platformName"`
	PlatformVersion     string `yaml:"platformVersion"`
	VendorName          string `yaml:"vendorName"`
	WebsiteURL          string `yaml:"websiteUrl"`
	RepoURL             string `yaml:"repoUrl,omitempty"`
	DocumentationURL    string `yaml:"documentationUrl"`
	ProductLogoURL      string `yaml:"productLogoUrl"`
	Description         string `yaml:"description"`
	ContactEmailAddress string `yaml:"contactEmailAddress"`
	K8sConformanceURL   string `yaml:"k8sConformanceUrl"`
}

// productSpec fixes the categories in upstream order; every carried
// checklist has rows in each, and the validator requires every category of
// the schema to be present.
type productSpec struct {
	Accelerators            []productRequirement `yaml:"accelerators"`
	Networking              []productRequirement `yaml:"networking"`
	SchedulingOrchestration []productRequirement `yaml:"schedulingOrchestration"`
	Observability           []productRequirement `yaml:"observability"`
	Security                []productRequirement `yaml:"security"`
	Operator                []productRequirement `yaml:"operator"`
}

type productRequirement struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	Level       string   `yaml:"level"`
	Status      string   `yaml:"status"`
	Evidence    []string `yaml:"evidence"`
	Notes       string   `yaml:"notes"`
}

// productRow is one requirement as PRODUCT.yaml and the README present it.
type productRow struct {
	Requirement RequirementResult
	Description string
	Status      string
	Evidence    []string
	Notes       string
}

// buildProductRows renders each requirement of the aggregate into its
// PRODUCT.yaml row. pages is the set of evidence page files the renderer
// wrote, so a row never cites a page that does not exist.
//
// Status is never typed: an established requirement is Implemented, a
// requirement declared not applicable is N/A with its justification, and
// everything else is Not Implemented with the aggregate's reason, which for
// an applicable MUST is also what fails the run. Evidence for a demonstrated
// requirement is its page plus `junit.xml#<check>` and `e2e.log#<check>` for
// each check on the proof path, the fragment form the validator resolves
// before its upstream-name map. A requirement attested by documentation
// cites the documentation URLs and the page as the observation record, and
// never the testcase: an observed result is not automated proof.
func buildProductRows(cl *Checklist, agg *Aggregate, pages map[string]bool) []productRow {
	rows := make([]productRow, 0, len(agg.Requirements))
	for _, r := range agg.Requirements {
		row := productRow{Requirement: r, Evidence: []string{}}
		if req := cl.Requirement(r.ID); req != nil {
			row.Description = req.Description
		}
		page := ""
		if r.Page != "" && pages[r.Page] {
			page = evidenceDirName + "/" + r.Page
		}
		switch {
		case r.Status == StatusNotApplicable:
			row.Status = productNotApplicable
			if page != "" {
				row.Evidence = append(row.Evidence, page)
			}
			row.Notes = r.Reason
		case r.Status == StatusPassed:
			row.Status = productImplemented
			if page != "" {
				row.Evidence = append(row.Evidence, page)
			}
			for _, check := range r.Proof {
				row.Evidence = append(row.Evidence, junitFile+"#"+check, e2eLogFile+"#"+check)
			}
			row.Notes = demonstratedNote(r)
		case r.Established():
			row.Status = productImplemented
			row.Evidence = append(row.Evidence, r.Documented.Evidence...)
			if page != "" {
				row.Evidence = append(row.Evidence, page)
			}
			row.Notes = strings.TrimSpace(r.Documented.Notes) +
				" AICR recorded the cluster's arrangement without a behavioral test (" + r.Reason + "); the claim rests on the documentation cited."
		default:
			row.Status = productNotImplemented
			row.Notes = notClaimedNote(r)
		}
		rows = append(rows, row)
	}
	return rows
}

// demonstratedNote is the PRODUCT.yaml note of a demonstrated requirement:
// what its page demonstrates, from the requirement map, and the checks that
// established it.
func demonstratedNote(r RequirementResult) string {
	note := fmt.Sprintf("Demonstrated by the AICR conformance check %s (junit.xml and e2e.log testcases of the same name).",
		strings.Join(r.Proof, " and "))
	if page := pageForRequirement(r.ID); page != nil {
		note = page.Description + " " + note
	}
	return note
}

// notClaimedNote is the PRODUCT.yaml note of a requirement that is not
// established: for a SHOULD the vendor does not claim, for a MUST the reason
// the run is incomplete.
func notClaimedNote(r RequirementResult) string {
	switch {
	case r.Reason == untestedNoCheck:
		return "Not claimed: AICR has no check for this requirement and no documentation was cited."
	case r.Level == LevelShould:
		return "Not claimed by this run: " + r.Reason + "."
	}
	return "Not established by this run: " + r.Reason + "."
}

// renderProduct marshals PRODUCT.yaml for the version's checklist.
func renderProduct(v Version, vendor Vendor, cl *Checklist, rows []productRow) ([]byte, error) {
	doc := productDocument{Metadata: productMetadata{
		KubernetesVersion:   string(v),
		PlatformName:        vendor.PlatformName,
		PlatformVersion:     vendor.PlatformVersion,
		VendorName:          vendor.Name,
		WebsiteURL:          vendor.WebsiteURL,
		RepoURL:             vendor.RepoURL,
		DocumentationURL:    vendor.DocumentationURL,
		ProductLogoURL:      vendor.ProductLogoURL,
		Description:         vendor.Description,
		ContactEmailAddress: vendor.ContactEmail,
		K8sConformanceURL:   vendor.K8sConformanceURL,
	}}
	byID := make(map[string]productRow, len(rows))
	for _, row := range rows {
		byID[row.Requirement.ID] = row
	}
	for _, req := range cl.Requirements {
		row, ok := byID[req.ID]
		if !ok {
			return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf("requirement %q of %s has no aggregate row", req.ID, v))
		}
		pr := productRequirement{
			ID:          req.ID,
			Description: req.Description,
			Level:       string(req.Level),
			Status:      row.Status,
			Evidence:    row.Evidence,
			Notes:       row.Notes,
		}
		switch req.Category {
		case categoryAccelerators:
			doc.Spec.Accelerators = append(doc.Spec.Accelerators, pr)
		case categoryNetworking:
			doc.Spec.Networking = append(doc.Spec.Networking, pr)
		case categorySchedulingOrchestration:
			doc.Spec.SchedulingOrchestration = append(doc.Spec.SchedulingOrchestration, pr)
		case categoryObservability:
			doc.Spec.Observability = append(doc.Spec.Observability, pr)
		case categorySecurity:
			doc.Spec.Security = append(doc.Spec.Security, pr)
		case categoryOperator:
			doc.Spec.Operator = append(doc.Spec.Operator, pr)
		default:
			return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf("requirement %q has unknown category %q", req.ID, req.Category))
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "marshal PRODUCT.yaml", err)
	}
	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "marshal PRODUCT.yaml", err)
	}
	return buf.Bytes(), nil
}
