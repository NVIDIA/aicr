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

package talos

import (
	"context"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/measurement"
	corev1 "k8s.io/api/core/v1"
)

// OS subtype names emitted by this collector.
const (
	SubtypeRelease    = "release"
	SubtypeExtensions = "extensions"
)

// extensionLabelKey is the prefix Talos applies to Node labels that
// indicate an installed system extension. Example labels:
//
//	extensions.talos.dev/nvidia-container-toolkit: 1.16.1-v1.7.6
//	extensions.talos.dev/nvidia-open-gpu-kernel-modules: 555.42.06-v1.7.6
//
// The label suffix becomes the reading key in the extensions subtype and
// the label value becomes the reading.
const extensionLabelKey = "extensions.talos.dev/"

// /etc/os-release-equivalent reading keys, populated from NodeInfo so the
// shape stays compatible with constraints written against the standard OS
// collector's release subtype on other distributions.
const (
	keyOSReleaseID         = "ID"
	keyOSReleasePrettyName = "PRETTY_NAME"
	keyOSReleaseVersionID  = "VERSION_ID"
	keyKernelVersion       = "KERNEL_VERSION"
	keyOperatingSystem     = "OPERATING_SYSTEM"
)

// OSCollector emits OS-level measurements for Talos nodes by reading the
// Kubernetes Node object. It produces:
//
//   - "release" subtype, populated from NodeInfo.OSImage / KernelVersion
//     (replaces /etc/os-release, which Talos does not expose to
//     unprivileged pods).
//   - "extensions" subtype, populated from `extensions.talos.dev/*` Node
//     labels, which advertise installed Talos system extensions such as
//     nvidia-container-toolkit and nvidia-open-gpu-kernel-modules. This
//     surfaces information that no other AICR collector captures (the GPU
//     collector reports the runtime/loaded driver via nvidia-smi, which
//     can disagree with the installation manifest during upgrades).
//
// The /proc-based subtypes from pkg/collector/os (grub, sysctl, kmod) are
// intentionally not produced here; they are largely fixed by the Talos
// image and add little drift signal. If a future constraint needs them,
// compose them in.
type OSCollector struct {
	cfg *config
}

// NewOSCollector constructs a Talos OS collector. With no options it
// resolves its Kubernetes client and node name lazily at Collect time.
func NewOSCollector(opts ...Option) *OSCollector {
	return &OSCollector{cfg: newConfig(opts)}
}

// Collect fetches the Kubernetes Node and returns a TypeOS measurement
// with release and (when extension labels are present) extensions
// subtypes. On any fetch failure it returns an empty TypeOS measurement
// rather than an error so the snapshot continues.
func (c *OSCollector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting Talos OS state from Kubernetes Node info")

	ctx, cancel := context.WithTimeout(ctx, defaults.CollectorTimeout)
	defer cancel()

	node := c.cfg.fetchNode(ctx)
	if node == nil {
		return emptyOSMeasurement(), nil
	}

	subs := []measurement.Subtype{
		buildReleaseSubtype(&node.Status.NodeInfo),
	}
	if ext := buildExtensionsSubtype(node.Labels); ext != nil {
		subs = append(subs, *ext)
	}

	return &measurement.Measurement{
		Type:     measurement.TypeOS,
		Subtypes: subs,
	}, nil
}

// buildReleaseSubtype derives an /etc/os-release-equivalent subtype from
// NodeInfo. Always emits at least the Source key so the subtype is valid
// even when NodeInfo is empty.
func buildReleaseSubtype(info *corev1.NodeSystemInfo) measurement.Subtype {
	b := measurement.NewSubtypeBuilder(SubtypeRelease).
		SetString(keySource, sourceTalosNodeInfo)

	if info.OSImage != "" {
		b.SetString(keyOSReleasePrettyName, info.OSImage)
		if id, version := parseOSImage(info.OSImage); id != "" {
			b.SetString(keyOSReleaseID, id)
			if version != "" {
				b.SetString(keyOSReleaseVersionID, version)
			}
		}
	}
	if info.KernelVersion != "" {
		b.SetString(keyKernelVersion, info.KernelVersion)
	}
	if info.OperatingSystem != "" {
		b.SetString(keyOperatingSystem, info.OperatingSystem)
	}
	return b.Build()
}

// parseOSImage extracts an os-release-style ID and VERSION_ID from
// NodeInfo.OSImage. The Talos format is `Talos (vX.Y.Z)`. This parser
// is intentionally Talos-specific: any other format (RHEL's
// `Red Hat Enterprise Linux 9.4 (Plow)`, Ubuntu's `Ubuntu 22.04.5 LTS`,
// etc.) yields empty results so the caller leaves ID/VERSION_ID unset
// rather than emitting a misleading parse like ID=red, VERSION_ID=Plow.
//
// This collector only runs when the OS criterion is talos, so non-Talos
// inputs are off the production path; declining to parse them keeps the
// data shape honest if someone ever exercises this code with another
// OSImage.
func parseOSImage(image string) (id, version string) {
	if image == "" {
		return "", ""
	}
	parts := strings.SplitN(image, " (", 2)
	if len(parts) != 2 {
		return "", ""
	}
	if !strings.EqualFold(strings.TrimSpace(parts[0]), "Talos") {
		return "", ""
	}
	id = "talos"
	v := strings.TrimSuffix(strings.TrimSpace(parts[1]), ")")
	v = strings.TrimPrefix(v, "v")
	version = v
	return id, version
}

// buildExtensionsSubtype derives an "extensions" subtype from Node labels
// of the form `extensions.talos.dev/<name>: <version>`. Returns nil when no
// matching labels are present so the caller can omit the subtype rather
// than emit one with no extension data.
func buildExtensionsSubtype(labels map[string]string) *measurement.Subtype {
	found := make(map[string]string)
	for k, v := range labels {
		if !strings.HasPrefix(k, extensionLabelKey) {
			continue
		}
		name := strings.TrimPrefix(k, extensionLabelKey)
		if name == "" {
			continue
		}
		found[name] = v
	}
	if len(found) == 0 {
		return nil
	}

	b := measurement.NewSubtypeBuilder(SubtypeExtensions).
		SetString(keySource, sourceTalosNodeInfo)
	for name, version := range found {
		b.SetString(name, version)
	}
	sub := b.Build()
	return &sub
}

func emptyOSMeasurement() *measurement.Measurement {
	return &measurement.Measurement{
		Type:     measurement.TypeOS,
		Subtypes: []measurement.Subtype{},
	}
}
