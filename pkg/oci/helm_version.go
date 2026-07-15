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

package oci

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/distribution/reference"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const maximumDistributionTagBytes = 128

// HelmChartVersionFromTag converts a raw Distribution tag into the strict
// SemVer form Helm stores in Chart.yaml. A single underscore encodes the
// SemVer build-metadata separator because '+' is not valid in a Distribution
// tag.
func HelmChartVersionFromTag(tag string) (string, error) {
	if err := validateDistributionTag(tag); err != nil {
		return "", err
	}
	if strings.Count(tag, "_") > 1 {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"Helm OCI tag contains multiple ambiguous build-metadata separators")
	}
	version := strings.Replace(tag, "_", "+", 1)
	if _, err := semver.StrictNewVersion(version); err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("Helm OCI tag %q does not encode a strict semantic version", tag), err)
	}
	encoded, err := helmTagFromStrictChartVersion(version)
	if err != nil {
		return "", err
	}
	if encoded != tag {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("Helm OCI tag %q does not round-trip through Chart.yaml semantic versioning", tag))
	}
	return version, nil
}

// HelmTagFromChartVersion converts a strict Chart.yaml SemVer value into its
// raw Distribution-tag representation. The SemVer build-metadata separator
// is encoded as one underscore.
func HelmTagFromChartVersion(version string) (string, error) {
	tag, err := helmTagFromStrictChartVersion(version)
	if err != nil {
		return "", err
	}
	decoded := strings.Replace(tag, "_", "+", 1)
	if decoded != version {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("Helm chart version %q does not round-trip through its OCI tag", version))
	}
	return tag, nil
}

func helmTagFromStrictChartVersion(version string) (string, error) {
	if _, err := semver.StrictNewVersion(version); err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("Helm chart version %q is not strict semantic versioning", version), err)
	}
	tag := strings.Replace(version, "+", "_", 1)
	if err := validateDistributionTag(tag); err != nil {
		return "", err
	}
	return tag, nil
}

func validateDistributionTag(tag string) error {
	if tag == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "OCI tag cannot be empty")
	}
	if len(tag) > maximumDistributionTagBytes {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("OCI tag exceeds %d bytes", maximumDistributionTagBytes))
	}
	named, err := reference.WithName("example.invalid/aicr")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to construct OCI tag validation reference", err)
	}
	if _, err := reference.WithTag(named, tag); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid OCI Distribution tag %q", tag), err)
	}
	return nil
}
