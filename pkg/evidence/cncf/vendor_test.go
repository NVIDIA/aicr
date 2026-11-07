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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// testVendor is a well-formed vendor block; the cases below break one field
// at a time.
func testVendor() Vendor {
	return Vendor{
		Name:              "Test Vendor",
		PlatformName:      "Test Platform",
		PlatformVersion:   "1.2.3",
		Description:       "A GPU platform under test.",
		ContactEmail:      "owner@vendor.example",
		WebsiteURL:        "https://vendor.example/platform",
		DocumentationURL:  "https://vendor.example/docs",
		RepoURL:           "https://github.com/vendor/platform",
		ProductLogoURL:    "https://vendor.example/logo.svg",
		K8sConformanceURL: "https://github.com/cncf/k8s-conformance/tree/master/v1.37/test-platform",
	}
}

func TestVendorValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Vendor)
		wantErr string
	}{
		{name: "well-formed"},
		{name: "repoUrl is optional", mutate: func(v *Vendor) { v.RepoURL = "" }},
		{name: "nil receiver", mutate: nil, wantErr: "vendor is required"},
		{name: "missing name", mutate: func(v *Vendor) { v.Name = " " }, wantErr: "vendor.name is required"},
		{name: "missing platformVersion", mutate: func(v *Vendor) { v.PlatformVersion = "" }, wantErr: "vendor.platformVersion is required"},
		{name: "missing productLogoUrl", mutate: func(v *Vendor) { v.ProductLogoURL = "" }, wantErr: "vendor.productLogoUrl is required"},
		{name: "missing k8sConformanceUrl", mutate: func(v *Vendor) { v.K8sConformanceURL = "" }, wantErr: "vendor.k8sConformanceUrl is required"},
		{name: "placeholder", mutate: func(v *Vendor) { v.Description = "[One sentence]" }, wantErr: `vendor.description has the placeholder value "[One sentence]"`},
		{name: "placeholder in optional field", mutate: func(v *Vendor) { v.RepoURL = "[repo]" }, wantErr: "vendor.repoUrl has the placeholder"},
		{name: "contact is not a mail address", mutate: func(v *Vendor) { v.ContactEmail = "owner at vendor" }, wantErr: "vendor.contactEmail"},
		{name: "website is relative", mutate: func(v *Vendor) { v.WebsiteURL = "vendor.example/platform" }, wantErr: "vendor.websiteUrl"},
		{name: "logo has another scheme", mutate: func(v *Vendor) { v.ProductLogoURL = "ftp://vendor.example/logo.svg" }, wantErr: "vendor.productLogoUrl"},
		{name: "repo is malformed", mutate: func(v *Vendor) { v.RepoURL = "https://%zz" }, wantErr: "vendor.repoUrl"},
		{name: "conformance url outside the k8s-conformance tree", mutate: func(v *Vendor) {
			v.K8sConformanceURL = "https://github.com/cncf/k8s-ai-conformance/tree/master/v1.37/x"
		}, wantErr: "vendor.k8sConformanceUrl"},
		{name: "conformance url with a nested path", mutate: func(v *Vendor) { v.K8sConformanceURL = "https://github.com/cncf/k8s-conformance/tree/master/v1.37/x/y" }, wantErr: "vendor.k8sConformanceUrl"},
		{name: "conformance url on main", mutate: func(v *Vendor) { v.K8sConformanceURL = "https://github.com/cncf/k8s-conformance/tree/main/v1.36/x/" }},
		{name: "upper-case scheme is refused", mutate: func(v *Vendor) { v.DocumentationURL = "HTTPS://vendor.example/docs" }, wantErr: "vendor.documentationUrl"},
		{name: "leading space is refused", mutate: func(v *Vendor) { v.WebsiteURL = " https://vendor.example/platform" }, wantErr: "vendor.websiteUrl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v *Vendor
			if tt.name != "nil receiver" {
				vv := testVendor()
				if tt.mutate != nil {
					tt.mutate(&vv)
				}
				v = &vv
			}
			err := v.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidateDocumented(t *testing.T) {
	doc := Documentation{Evidence: []string{"https://vendor.example/docs/autoscaling"}, Notes: "The node pools scale on pending pods."}
	tests := []struct {
		name          string
		version       Version
		documented    map[string]Documentation
		notApplicable map[string]string
		wantErr       string
	}{
		{name: "nil map", version: Version137},
		{name: "well-formed", version: Version137, documented: map[string]Documentation{"cluster_autoscaling": doc}},
		{name: "a SHOULD may be documented too", version: Version137, documented: map[string]Documentation{"gpu_sharing": doc}},
		{name: "unsupported version", version: "v1.30", wantErr: "unsupported"},
		{name: "unknown requirement", version: Version137, documented: map[string]Documentation{"nope": doc}, wantErr: `"nope" is not a requirement of the v1.37 checklist`},
		{name: "retired requirement at this version", version: Version137, documented: map[string]Documentation{"dra_support": doc}, wantErr: "not a requirement of the v1.37"},
		{
			name: "documented and not applicable at once", version: Version137,
			documented: map[string]Documentation{"cluster_autoscaling": doc}, notApplicable: map[string]string{"cluster_autoscaling": "fixed size"},
			wantErr: "also declared notApplicable",
		},
		{name: "no evidence", version: Version137, documented: map[string]Documentation{"cluster_autoscaling": {Notes: "x"}}, wantErr: "at least one documentation URL"},
		{name: "no notes", version: Version137, documented: map[string]Documentation{"cluster_autoscaling": {Evidence: doc.Evidence, Notes: " "}}, wantErr: "notes is required"},
		{name: "malformed url", version: Version137, documented: map[string]Documentation{"cluster_autoscaling": {Evidence: []string{"docs/autoscaling.md"}, Notes: "x"}}, wantErr: "documented.cluster_autoscaling.evidence"},
		{name: "upper-case scheme", version: Version137, documented: map[string]Documentation{"cluster_autoscaling": {Evidence: []string{"HTTPS://vendor.example/docs"}, Notes: "x"}}, wantErr: "lower case"},
		{name: "leading space", version: Version137, documented: map[string]Documentation{"cluster_autoscaling": {Evidence: []string{" https://vendor.example/docs"}, Notes: "x"}}, wantErr: "lower case"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDocumented(tt.version, tt.documented, tt.notApplicable)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateDocumented() = %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
