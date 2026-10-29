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

package aicrjob

import (
	"fmt"

	"golang.org/x/mod/semver"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Supported AICR range: >=1.0.0, <2.0.0. v1 surfaces are frozen;
// v2 is a new contract that needs a new operator release.
const (
	MinAICRVersion = "v1.0.0"
	// ExclusiveMaxAICRVersion is the first unsupported version.
	ExclusiveMaxAICRVersion = "v2.0.0"
)

// v1.1.0 is the first AICR release with the pre-provisioned validator
// ServiceAccount, --image-refs written in place, and KMS-key evidence signing;
// earlier v1.x releases keep the fallbacks.
const v11 = "v1.1.0"

// SupportsV11 reports whether an AICR version has the v1.1.0 features.
func SupportsV11(version string) bool {
	return semver.Compare(normalize(version), v11) >= 0
}

// Condition reasons for version gate failures.
const (
	ReasonVersionMismatch     = "AICRVersionMismatch"
	ReasonArtifactUnsupported = "AICRArtifactUnsupported"
)

// CheckSupportedVersion rejects an AICR version outside the supported range,
// or one that is not a release version.
func CheckSupportedVersion(version string) error {
	if !semver.IsValid(version) || semver.Prerelease(version) != "" || semver.Build(version) != "" {
		return versionError(fmt.Sprintf("AICR version %q is not a release version", version))
	}
	if semver.Compare(version, MinAICRVersion) < 0 || semver.Compare(version, ExclusiveMaxAICRVersion) >= 0 {
		return versionError(fmt.Sprintf("AICR version %s is outside >=1.0.0, <2.0.0", version))
	}
	return nil
}

// CheckArtifactVersion is the version gate: the version an AICR artifact
// reports must equal the declared aicr.version exactly, and an artifact
// without a release version is rejected. A mismatch writes nothing to the
// engine and stalls with AICRVersionMismatch.
func CheckArtifactVersion(declared, reported string) error {
	if err := CheckSupportedVersion(declared); err != nil {
		return err
	}
	if reported == "" {
		return versionError("AICR artifact carries no release version")
	}
	if normalize(reported) != normalize(declared) {
		return versionError(fmt.Sprintf("AICR artifact reports %s, but aicr.version is %s", reported, declared))
	}
	return nil
}

func normalize(v string) string {
	if v != "" && v[0] != 'v' {
		return "v" + v
	}
	return v
}

func versionError(msg string) error {
	return operrors.New(operrors.ErrCodeInvalidRequest, msg).
		WithReason(ReasonVersionMismatch)
}
