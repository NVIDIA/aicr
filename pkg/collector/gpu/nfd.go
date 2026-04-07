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

package gpu

import (
	"context"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	nfdv1alpha1 "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"
	"sigs.k8s.io/node-feature-discovery/source"
	_ "sigs.k8s.io/node-feature-discovery/source/kernel"
	_ "sigs.k8s.io/node-feature-discovery/source/pci"
)

// PCI identification constants for NVIDIA GPU detection.
const (
	nvidiaVendorID     = "10de"
	pciClassVGA        = "0300"
	pciClass3D         = "0302"
	nvidiaKernelModule = "nvidia"
)

// NFD source and feature key constants for hardware detection.
const (
	nfdSourcePCI             = "pci"
	nfdSourceKernel          = "kernel"
	nfdPCIDeviceFeature      = "device"
	nfdKernelLoadedModuleKey = "loadedmodule"
	detectionSourceNFD       = "nfd"
)

// NFDHardwareDetector uses NFD source packages to detect GPU hardware
// via PCI enumeration and kernel module state from sysfs/procfs.
//
// NFDHardwareDetector is not safe for concurrent use. NFD source singletons
// are shared package-level state without synchronization. In AICR's architecture
// the GPU collector runs once per snapshot, so this is not a practical concern.
type NFDHardwareDetector struct{}

// Detect discovers GPU hardware using NFD PCI and kernel sources.
// PCI discovery is required; kernel module detection is best-effort.
//
// This method requires Linux with sysfs/procfs mounted. On other platforms
// (macOS, containers without /sys), PCI discovery will fail and an error
// is returned. The caller (Collector.Collect) handles this gracefully by
// falling back to nvidia-smi-only collection.
func (d *NFDHardwareDetector) Detect(ctx context.Context) (*HardwareInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.NFDDetectionTimeout)
	defer cancel()

	// Get PCI source for GPU device enumeration
	pciSrc := source.GetFeatureSource(nfdSourcePCI)
	if pciSrc == nil {
		return nil, errors.New(errors.ErrCodeInternal, "NFD PCI source not available")
	}

	if err := pciSrc.Discover(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "NFD PCI discovery failed", err)
	}

	pciFeatures := pciSrc.GetFeatures()

	// Check context between discovery phases
	select {
	case <-ctx.Done():
		return nil, errors.Wrap(errors.ErrCodeTimeout, "NFD detection canceled after PCI discovery", ctx.Err())
	default:
	}

	// Get kernel source (best-effort — driver detection is secondary)
	var kernelFeatures *nfdv1alpha1.Features
	kernelSrc := source.GetFeatureSource(nfdSourceKernel)
	if kernelSrc != nil {
		if err := kernelSrc.Discover(); err != nil {
			slog.Warn("NFD kernel discovery failed, driver detection unavailable",
				slog.String("error", err.Error()))
		} else {
			kernelFeatures = kernelSrc.GetFeatures()
		}
	}

	info := extractHardwareInfo(pciFeatures, kernelFeatures)
	info.DetectionSource = detectionSourceNFD

	return info, nil
}

// extractHardwareInfo parses NFD PCI and kernel features to build HardwareInfo.
// It counts NVIDIA GPUs by matching vendor ID and PCI class codes, and checks
// whether the nvidia kernel module is loaded.
func extractHardwareInfo(pciFeatures, kernelFeatures *nfdv1alpha1.Features) *HardwareInfo {
	info := &HardwareInfo{}

	if pciFeatures == nil {
		return info
	}

	// Count NVIDIA GPUs via PCI device enumeration
	pciDevices, ok := pciFeatures.Instances[nfdPCIDeviceFeature]
	if ok {
		for _, dev := range pciDevices.Elements {
			vendor := dev.Attributes["vendor"]
			class := dev.Attributes["class"]
			if vendor == nvidiaVendorID && (class == pciClassVGA || class == pciClass3D) {
				info.GPUCount++
			}
		}
	}

	info.GPUPresent = info.GPUCount > 0

	// Check nvidia kernel module state (best-effort)
	if kernelFeatures != nil {
		if loadedModules, ok := kernelFeatures.Flags[nfdKernelLoadedModuleKey]; ok {
			if _, loaded := loadedModules.Elements[nvidiaKernelModule]; loaded {
				info.DriverLoaded = true
			}
		}
	}

	return info
}
