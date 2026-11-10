// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package bom

import "fmt"

// Node CPU architectures that DOCA driver tags are published for.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// qualifiedDriverOSIDs is the set of node OS IDs (the os-release ID) for which
// driver tags of the form <version>-<id><versionID> are known to be published.
var qualifiedDriverOSIDs = map[string]struct{}{"ubuntu": {}, "rhel": {}}

// DriverTarget is the node OS and architecture an operator completes a driver
// tag for. A field the recipe does not determine is empty.
type DriverTarget struct {
	OSID      string // os-release ID, e.g. "ubuntu"
	OSVersion string // os-release VERSION_ID, e.g. "24.04"
	Arch      string // ArchAMD64 or ArchARM64
}

// TagPattern describes how the operator completes the policy version, for
// messages that name a tag AICR cannot list.
func (d DriverImage) TagPattern() string {
	if d.Kind == DriverKindDOCA {
		return "<version>-<os><os-version>-<arch>"
	}
	return "<version>-<os><os-version>"
}

// Qualify returns the reference the operator pulls on a node matching t. When
// no single published tag can be named it returns ok=false and the reason.
// A digest reference is returned unchanged, because the digest already names
// one image.
func (d DriverImage) Qualify(t DriverTarget) (ref, reason string, ok bool) {
	if ParseImageRef(d.Ref).Digest != "" {
		return d.Ref, "", true
	}
	if d.Precompiled {
		return "", "precompiled driver tags also name each node's kernel and cannot be listed ahead of time", false
	}
	if t.OSID == "" || t.OSVersion == "" {
		return "", "the recipe does not pin one OS (OS.release.ID and OS.release.VERSION_ID)", false
	}
	if _, known := qualifiedDriverOSIDs[t.OSID]; !known {
		return "", fmt.Sprintf("no published driver tag pattern is known for OS %q", t.OSID), false
	}
	ref = d.Ref + "-" + t.OSID + t.OSVersion
	if d.Kind != DriverKindDOCA {
		return ref, "", true
	}
	if t.Arch != ArchAMD64 && t.Arch != ArchARM64 {
		return "", "the recipe criteria do not determine the node architecture", false
	}
	return ref + "-" + t.Arch, "", true
}
