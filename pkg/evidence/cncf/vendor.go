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
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Vendor is the product metadata a CNCF submission's PRODUCT.yaml carries:
// the validate config key spec.validate.evidence.cncf.vendor. Every field
// but RepoURL is required by the upstream validator at the pinned revision
// (its metadataFields table), which also rejects a bracketed placeholder
// such as "[Your Company]" and checks that each URL answers. Validate
// applies the shape rules here, before a run, and leaves reachability to the
// validator, which is the authority on it.
type Vendor struct {
	// Name is vendorName: the legal entity certifying, exactly as listed
	// under CNCF Members in the CNCF landscape, which the validator checks.
	Name string

	// PlatformName and PlatformVersion identify the product certified, not
	// the Kubernetes it runs; kubernetesVersion comes from the checklist.
	PlatformName    string
	PlatformVersion string

	// Description is the one-sentence description of the offering.
	Description string

	// ContactEmail is contactEmailAddress.
	ContactEmail string

	// WebsiteURL, DocumentationURL and ProductLogoURL are the product
	// website, its documentation and its logo (SVG, AI or EPS). RepoURL is
	// the optional source repository of an open-source product.
	WebsiteURL       string
	DocumentationURL string
	RepoURL          string
	ProductLogoURL   string

	// K8sConformanceURL is the product's Kubernetes conformance submission:
	// https://github.com/cncf/k8s-conformance/tree/master/vX.Y/<product>.
	K8sConformanceURL string
}

// k8sConformanceURLPattern is the upstream validator's pattern for
// k8sConformanceUrl, verbatim.
var k8sConformanceURLPattern = regexp.MustCompile(`^https://github\.com/cncf/k8s-conformance/tree/(master|main)/v\d+\.\d+/[^/]+/?$`)

// Validate applies the upstream shape rules to the vendor fields so a run
// is refused before any check rather than by the validator after every
// check: each required field present and not a bracketed placeholder, each
// URL an absolute http(s) URL, the contact a mail address, and the
// Kubernetes conformance URL in the k8s-conformance tree. Whether the URLs
// answer is the validator's check and is not made here.
//
// Errors:
//   - ErrCodeInvalidRequest naming the first field that fails, with the
//     config key it comes from.
func (v *Vendor) Validate() error {
	if v == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"spec.validate.evidence.cncf.vendor is required for a submission run: PRODUCT.yaml carries its fields")
	}
	fields := []struct {
		key, value string
		required   bool
	}{
		{"name", v.Name, true},
		{"platformName", v.PlatformName, true},
		{"platformVersion", v.PlatformVersion, true},
		{"description", v.Description, true},
		{"contactEmail", v.ContactEmail, true},
		{"websiteUrl", v.WebsiteURL, true},
		{"documentationUrl", v.DocumentationURL, true},
		{"productLogoUrl", v.ProductLogoURL, true},
		{"k8sConformanceUrl", v.K8sConformanceURL, true},
		{"repoUrl", v.RepoURL, false},
	}
	for _, f := range fields {
		t := strings.TrimSpace(f.value)
		switch {
		case t == "" && f.required:
			return vendorError(f.key, "is required")
		case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
			return vendorError(f.key, fmt.Sprintf("has the placeholder value %q", t))
		}
	}
	if _, err := mail.ParseAddress(v.ContactEmail); err != nil {
		return vendorError("contactEmail", fmt.Sprintf("%q is not a mail address", v.ContactEmail))
	}
	for _, f := range []struct{ key, value string }{
		{"websiteUrl", v.WebsiteURL},
		{"documentationUrl", v.DocumentationURL},
		{"productLogoUrl", v.ProductLogoURL},
		{"repoUrl", v.RepoURL},
		{"k8sConformanceUrl", v.K8sConformanceURL},
	} {
		if f.value == "" {
			continue
		}
		if problem := httpURLProblem(f.value); problem != "" {
			return vendorError(f.key, problem)
		}
	}
	if !k8sConformanceURLPattern.MatchString(v.K8sConformanceURL) {
		return vendorError("k8sConformanceUrl",
			fmt.Sprintf("%q is not of the form https://github.com/cncf/k8s-conformance/tree/master/vX.Y/<product>", v.K8sConformanceURL))
	}
	return nil
}

func vendorError(key, problem string) error {
	return errors.New(errors.ErrCodeInvalidRequest, "spec.validate.evidence.cncf.vendor."+key+" "+problem)
}

// httpURLProblem reports why raw is not an absolute http or https URL with
// a host, or "" when it is one. The scheme must be spelled in lower case
// with nothing before it: PRODUCT.yaml carries the value verbatim, and the
// upstream validator recognizes a URL by its literal "http://" or
// "https://" prefix, reading anything else as a local file path.
func httpURLProblem(raw string) string {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Sprintf("%q is not an absolute http(s) URL (the scheme must be lower case with no leading space)", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Sprintf("%q is not a URL: %v", raw, err)
	}
	if u.Host == "" {
		return fmt.Sprintf("%q is not an absolute http(s) URL", raw)
	}
	return ""
}

// Documentation is a vendor's manual attestation of one requirement the run
// only observed: the validate config key
// spec.validate.evidence.cncf.documented.<requirement>. Upstream accepts
// public documentation as evidence where no automated test applies, and the
// aggregation marks a result observation-only when a check recorded the
// cluster's arrangement without demonstrating it (StatusObserved). The
// attestation applies to that status alone: a demonstrated requirement needs
// none, and a failed or untested one is not papered over by documentation.
type Documentation struct {
	// Evidence lists the public documentation URLs cited in PRODUCT.yaml;
	// at least one, each an absolute http(s) URL the validator will request.
	Evidence []string

	// Notes is the PRODUCT.yaml note for the requirement, required.
	Notes string
}

// ValidateDocumented checks the documented map a caller supplies: each key a
// requirement of the checklist at version v and not also declared not
// applicable, with at least one well-formed documentation URL and a note.
//
// Errors:
//   - ErrCodeInvalidRequest when v is unsupported, a key is not a
//     requirement of that version or is also in notApplicable, an entry has
//     no evidence URL or a malformed one, or its note is blank.
func ValidateDocumented(v Version, documented map[string]Documentation, notApplicable map[string]string) error {
	cl, err := ChecklistFor(v)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(documented))
	for id := range documented {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		key := "spec.validate.evidence.cncf.documented." + id
		d := documented[id]
		switch {
		case cl.Requirement(id) == nil:
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"%s: %q is not a requirement of the %s checklist", key, id, v))
		case notApplicable[id] != "":
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"%s: %q is also declared notApplicable; a requirement is documented or not applicable, not both", key, id))
		case len(d.Evidence) == 0:
			return errors.New(errors.ErrCodeInvalidRequest, key+": at least one documentation URL is required under evidence")
		case strings.TrimSpace(d.Notes) == "":
			return errors.New(errors.ErrCodeInvalidRequest, key+": notes is required")
		}
		for _, u := range d.Evidence {
			if problem := httpURLProblem(u); problem != "" {
				return errors.New(errors.ErrCodeInvalidRequest, key+".evidence: "+problem)
			}
		}
	}
	return nil
}
