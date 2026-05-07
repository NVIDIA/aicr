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

// Subtype names emitted by the service collector. They intentionally match
// the systemd backend's service names so recipe constraints referencing
// "SystemD.containerd.service" / "SystemD.kubelet.service" remain
// schema-compatible across OS criteria.
const (
	SubtypeContainerd = "containerd.service"
	SubtypeKubelet    = "kubelet.service"
)

// Reading keys specific to the service subtypes.
const (
	keyActiveState  = "ActiveState"
	keyUnitFileName = "UnitFileName"
	keyRuntimeName  = "RuntimeName"
)

// ServiceCollector emits SystemD-equivalent service measurements derived
// from the Kubernetes Node object's NodeInfo. Used in place of
// pkg/collector/systemd on Talos Linux nodes (no systemd, no D-Bus).
//
// See package doc (doc.go) for the three-phase escalation path —
// (1) Kubernetes-API stub like this one, (2) gRPC reflection, (3) vendor
// the MPL-2.0 Talos client. Step up only when a constraint genuinely
// requires data the previous phase cannot provide.
type ServiceCollector struct {
	cfg *config
}

// NewServiceCollector constructs a Talos service collector. With no options
// it resolves its Kubernetes client and node name lazily at Collect time.
func NewServiceCollector(opts ...Option) *ServiceCollector {
	return &ServiceCollector{cfg: newConfig(opts)}
}

// Collect fetches the Kubernetes Node and returns a TypeSystemD measurement
// with one subtype per service whose state we can infer from NodeInfo.
//
// Failure modes degrade gracefully (mirroring systemd.Collector): on any
// fetch failure the measurement returned has TypeSystemD with no subtypes
// so the snapshot can continue collecting other dimensions.
func (s *ServiceCollector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting Talos service state from Kubernetes Node info")

	ctx, cancel := context.WithTimeout(ctx, defaults.CollectorTimeout)
	defer cancel()

	node := s.cfg.fetchNode(ctx)
	if node == nil {
		return emptyServiceMeasurement(), nil
	}

	subs := []measurement.Subtype{
		buildContainerdSubtype(&node.Status.NodeInfo),
		buildKubeletSubtype(&node.Status.NodeInfo),
	}
	return &measurement.Measurement{
		Type:     measurement.TypeSystemD,
		Subtypes: subs,
	}, nil
}

// buildContainerdSubtype derives a containerd.service-equivalent subtype
// from NodeInfo.ContainerRuntimeVersion (e.g., "containerd://1.7.20").
//
// ActiveState semantics:
//   - empty                          -> "unknown" (no signal from kubelet)
//   - parseable as "containerd://X"  -> "active"  (containerd is the runtime)
//   - any other non-empty value      -> "unknown" (CRI-O, malformed, etc.)
//
// The conservative "unknown" for non-containerd runtimes prevents
// constraints written against SystemD.containerd.service from silently
// matching nodes that aren't actually running containerd.
func buildContainerdSubtype(info *corev1.NodeSystemInfo) measurement.Subtype {
	b := measurement.NewSubtypeBuilder(SubtypeContainerd).
		SetString(keyUnitFileName, SubtypeContainerd).
		SetString(keySource, sourceTalosNodeInfo)

	if info.ContainerRuntimeVersion == "" {
		b.SetString(keyActiveState, "unknown")
		return b.Build()
	}

	runtimeName, runtimeVersion := splitRuntimeID(info.ContainerRuntimeVersion)
	b.SetString(keyRuntimeName, runtimeName)

	if runtimeName != "containerd" || runtimeVersion == "" {
		b.SetString(keyActiveState, "unknown")
		return b.Build()
	}
	b.SetString(keyActiveState, "active").
		SetString(keyVersion, runtimeVersion)

	return b.Build()
}

// buildKubeletSubtype derives a kubelet.service-equivalent subtype from
// NodeInfo.KubeletVersion (e.g., "v1.32.4").
func buildKubeletSubtype(info *corev1.NodeSystemInfo) measurement.Subtype {
	b := measurement.NewSubtypeBuilder(SubtypeKubelet).
		SetString(keyUnitFileName, SubtypeKubelet).
		SetString(keySource, sourceTalosNodeInfo)

	if info.KubeletVersion == "" {
		b.SetString(keyActiveState, "unknown")
		return b.Build()
	}

	b.SetString(keyActiveState, "active").
		SetString(keyVersion, info.KubeletVersion)

	return b.Build()
}

// splitRuntimeID parses "containerd://1.7.20" into ("containerd", "1.7.20").
// If the string has no "://" separator, the whole value is returned as the
// runtime name with an empty version.
func splitRuntimeID(s string) (string, string) {
	parts := strings.SplitN(s, "://", 2)
	if len(parts) != 2 {
		return s, ""
	}
	return parts[0], parts[1]
}

func emptyServiceMeasurement() *measurement.Measurement {
	return &measurement.Measurement{
		Type:     measurement.TypeSystemD,
		Subtypes: []measurement.Subtype{},
	}
}
