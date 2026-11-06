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
	"strings"

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

// SupportsV11 reports whether an AICR version has the v1.1.0 features. A
// prerelease counts as the release it precedes.
func SupportsV11(version string) bool {
	return semver.Compare(Core(version), v11) >= 0
}

// Core is an AICR version without its prerelease and build suffixes, the
// major.minor.patch that version-gated behavior compares. It is empty for a
// version that is not semantic.
func Core(version string) string {
	c := semver.Canonical(version)
	c = strings.TrimSuffix(c, semver.Build(c))
	return strings.TrimSuffix(c, semver.Prerelease(c))
}

// Condition reasons for version gate failures.
const (
	ReasonVersionMismatch     = "AICRVersionMismatch"
	ReasonArtifactUnsupported = "AICRArtifactUnsupported"
)

// CheckSupportedVersion rejects an AICR version that CheckDeclaredVersion
// rejects or whose release is outside the supported range. A prerelease is in
// range when the release it precedes is.
func CheckSupportedVersion(version string) error {
	if err := CheckDeclaredVersion(version); err != nil {
		return err
	}
	if core := Core(version); semver.Compare(core, MinAICRVersion) < 0 || semver.Compare(core, ExclusiveMaxAICRVersion) >= 0 {
		return versionError(fmt.Sprintf("AICR version %s is outside >=1.0.0, <2.0.0", version))
	}
	return nil
}

// CheckDeclaredVersion rejects an AICR version that is not vX.Y.Z with an
// optional prerelease.
func CheckDeclaredVersion(version string) error {
	if !semver.IsValid(version) || semver.Canonical(version) != version {
		return versionError(fmt.Sprintf("AICR version %q is not vX.Y.Z with an optional prerelease", version))
	}
	return nil
}

// CheckPrereleaseAllowed rejects a prerelease AICR version unless allowed is
// true. It does not check the format.
func CheckPrereleaseAllowed(version string, allowed bool) error {
	if semver.Prerelease(version) != "" && !allowed {
		return versionError(fmt.Sprintf("AICR version %s is a prerelease and needs aicr.allowPrerelease", version))
	}
	return nil
}

// CheckArtifactVersion is the version gate: the version an AICR artifact
// reports must equal the declared aicr.version exactly, and an artifact
// without a version is rejected. A mismatch writes nothing to the engine and
// stalls with AICRVersionMismatch.
func CheckArtifactVersion(declared, reported string) error {
	if err := CheckSupportedVersion(declared); err != nil {
		return err
	}
	if reported == "" {
		return versionError("AICR artifact carries no release version")
	}
	if normalize(reported) != declared {
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
