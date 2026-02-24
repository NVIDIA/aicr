// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"encoding/xml"
	stderrors "errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Collector collects NVIDIA SMI configurations from nvidia-smi command output in XML format
// and parses them into NVSMIDevice structures
type Collector struct {
}

const nvidiaSMICommand = "nvidia-smi"

// Collect retrieves the NVIDIA SMI information by executing nvidia-smi command and
// parses the XML output into NVSMIDevice structures.
// If nvidia-smi is not installed, returns a measurement with gpu-count=0 (graceful degradation).
func (s *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting GPU information via nvidia-smi")

	// Check if nvidia-smi is available before attempting to run it
	if _, err := exec.LookPath(nvidiaSMICommand); err != nil {
		slog.Warn("nvidia-smi not found - no GPU data will be collected",
			slog.String("hint", "install NVIDIA drivers to enable GPU collection"))
		return noGPUMeasurement(), nil
	}

	// Use parent context deadline if it's sooner than our default timeout
	deadline, ok := ctx.Deadline()
	timeout := defaults.CollectorTimeout
	if ok {
		remaining := time.Until(deadline)
		if remaining < timeout && remaining > 0 {
			timeout = remaining
		}
	}

	// Add timeout to prevent runaway operations
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Check if context is canceled
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := executeCommand(ctx, nvidiaSMICommand, "-q", "-x")
	if err != nil {
		slog.Warn("nvidia-smi execution failed - no GPU data will be collected",
			slog.String("error", err.Error()))
		return noGPUMeasurement(), nil
	}
	smiReadings, err := getSMIReadings(data)
	if err != nil {
		slog.Warn("nvidia-smi output parsing failed - no GPU data will be collected",
			slog.String("error", err.Error()))
		return noGPUMeasurement(), nil
	}

	res := &measurement.Measurement{
		Type: measurement.TypeGPU,
		Subtypes: []measurement.Subtype{
			{
				Name: "smi",
				Data: smiReadings, // no need for filtering here since we control the fields in getSMIReadings
			},
		},
	}

	return res, nil
}

// noGPUMeasurement returns a measurement indicating no GPU is available.
// This is used for graceful degradation when nvidia-smi is not installed.
func noGPUMeasurement() *measurement.Measurement {
	return &measurement.Measurement{
		Type: measurement.TypeGPU,
		Subtypes: []measurement.Subtype{
			{
				Name: "smi",
				Data: map[string]measurement.Reading{
					measurement.KeyGPUCount: measurement.Int(0),
				},
			},
		},
	}
}

func getSMIReadings(data []byte) (map[string]measurement.Reading, error) {
	smiDevice, err := parseSMIDevice(data)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse nvidia-smi output", err)
	}

	smiData := make(map[string]measurement.Reading)

	smiData[measurement.KeyGPUDriver] = measurement.Str(smiDevice.DriverVersion)
	smiData["cuda-version"] = measurement.Str(smiDevice.CudaVersion)

	gpuCount := len(smiDevice.GPUs)
	smiData[measurement.KeyGPUCount] = measurement.Int(gpuCount)

	if gpuCount < 1 {
		slog.Warn("No GPUs found in nvidia-smi output")
		return smiData, nil
	}

	// Only include details for the first GPU to keep output concise
	gpu := smiDevice.GPUs[0]
	const prefix = "gpu."
	key := func(field string) string {
		return prefix + field
	}
	smiData[key(measurement.KeyGPUModel)] = measurement.Str(gpu.ProductName)
	smiData[key("product-architecture")] = measurement.Str(gpu.ProductArchitecture)
	smiData[key("display-mode")] = measurement.Str(gpu.DisplayMode)
	smiData[key("display-active")] = measurement.Str(gpu.DisplayActive)
	smiData[key("persistence-mode")] = measurement.Str(gpu.PersistenceMode)
	smiData[key("addressing-mode")] = measurement.Str(gpu.AddressingMode)
	smiData[key("vbios-version")] = measurement.Str(gpu.VbiosVersion)
	smiData[key("gsp-firmware-version")] = measurement.Str(gpu.GspFirmwareVersion)

	return smiData, nil
}

func executeCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.Output()
	if err != nil {
		// Include stderr in the error message if available
		var exitErr *exec.ExitError
		if stderrors.As(err, &exitErr) {
			return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to execute command %s (stderr: %s)", name, string(exitErr.Stderr)), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to execute command %s", name), err)
	}
	return output, nil
}

func parseSMIDevice(data []byte) (*NVSMIDevice, error) {
	var d NVSMIDevice
	err := xml.Unmarshal(data, &d)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to unmarshal NVIDIA SMI XML - %s", string(data)), err)
	}
	return &d, nil
}

type NVSMIDevice struct {
	Timestamp     string `xml:"timestamp" json:"timestamp" yaml:"timestamp"`
	DriverVersion string `xml:"driver_version" json:"driverVersion" yaml:"driverVersion"`
	CudaVersion   string `xml:"cuda_version" json:"cudaVersion" yaml:"cudaVersion"`
	AttachedGpus  int    `xml:"attached_gpus" json:"attachedGPUs" yaml:"attachedGPUs"`
	GPUs          []GPU  `xml:"gpu" json:"gpu" yaml:"gpu"`
}

type GPU struct {
	ProductName               string                    `xml:"product_name" json:"productName" yaml:"productName"`
	ProductBrand              string                    `xml:"product_brand" json:"productBrand" yaml:"productBrand"`
	ProductArchitecture       string                    `xml:"product_architecture" json:"productArchitecture" yaml:"productArchitecture"`
	DisplayMode               string                    `xml:"display_mode" json:"displayMode" yaml:"displayMode"`
	DisplayActive             string                    `xml:"display_active" json:"displayActive" yaml:"displayActive"`
	PersistenceMode           string                    `xml:"persistence_mode" json:"persistenceMode" yaml:"persistenceMode"`
	AddressingMode            string                    `xml:"addressing_mode" json:"addressingMode" yaml:"addressingMode"`
	MigMode                   MigMode                   `xml:"mig_mode" json:"migMode" yaml:"migMode"`
	MigDevices                string                    `xml:"mig_devices" json:"migDevices" yaml:"migDevices"`
	AccountingMode            string                    `xml:"accounting_mode" json:"accountingMode" yaml:"accountingMode"`
	AccountingModeBufferSize  string                    `xml:"accounting_mode_buffer_size" json:"accountingModeBufferSize" yaml:"accountingModeBufferSize"`
	DriverModel               DriverModel               `xml:"driver_model" json:"driverModel" yaml:"driverModel"`
	Serial                    string                    `xml:"serial" json:"serial" yaml:"serial"`
	UUID                      string                    `xml:"uuid" json:"uuid" yaml:"uuid"`
	MinorNumber               string                    `xml:"minor_number" json:"minorNumber" yaml:"minorNumber"`
	VbiosVersion              string                    `xml:"vbios_version" json:"vbiosVersion" yaml:"vbiosVersion"`
	MultigpuBoard             string                    `xml:"multigpu_board" json:"multiGPUBoard" yaml:"multiGPUBoard"`
	BoardID                   string                    `xml:"board_id" json:"boardId" yaml:"boardId"`
	BoardPartNumber           string                    `xml:"board_part_number" json:"boardPartNumber" yaml:"boardPartNumber"`
	GpuPartNumber             string                    `xml:"gpu_part_number" json:"gpuPartNumber" yaml:"gpuPartNumber"`
	GpuFruPartNumber          string                    `xml:"gpu_fru_part_number" json:"gpuFRUPartNumber" yaml:"gpuFRUPartNumber"`
	PlatformInfo              PlatformInfo              `xml:"platformInfo" json:"platformInfo" yaml:"platformInfo"`
	InforomVersion            InforomVersion            `xml:"inforom_version" json:"inforomVersion" yaml:"inforomVersion"`
	InforomBbxFlush           InforomBbxFlush           `xml:"inforom_bbx_flush" json:"inforomBBXFlush" yaml:"inforomBBXFlush"`
	GpuOperationMode          OperationMode             `xml:"gpu_operation_mode" json:"gpuOperationMode" yaml:"gpuOperationMode"`
	C2cMode                   string                    `xml:"c2c_mode" json:"c2cMode" yaml:"c2cMode"`
	GpuVirtualizationMode     VirtualizationMode        `xml:"gpu_virtualization_mode" json:"gpuVirtualizationMode" yaml:"gpuVirtualizationMode"`
	GpuResetStatus            ResetStatus               `xml:"gpu_reset_status" json:"gpuResetStatus" yaml:"gpuResetStatus"`
	GpuRecoveryAction         string                    `xml:"gpu_recovery_action" json:"gpuRecoveryAction" yaml:"gpuRecoveryAction"`
	GspFirmwareVersion        string                    `xml:"gsp_firmware_version" json:"gspFirmwareVersion" yaml:"gspFirmwareVersion"`
	Ibmnpu                    Ibmnpu                    `xml:"ibmnpu" json:"ibmnpu" yaml:"ibmnpu"`
	Pci                       Pci                       `xml:"pci" json:"pci" yaml:"pci"`
	FanSpeed                  string                    `xml:"fan_speed" json:"fanSpeed" yaml:"fanSpeed"`
	PerformanceState          string                    `xml:"performance_state" json:"performanceState" yaml:"performanceState"`
	ClocksEventReasons        ClocksEventReasons        `xml:"clocks_event_reasons" json:"clocksEventReasons" yaml:"clocksEventReasons"`
	SparseOperationMode       string                    `xml:"sparse_operation_mode" json:"sparseOperationMode" yaml:"sparseOperationMode"`
	FbMemoryUsage             FbMemoryUsage             `xml:"fb_memory_usage" json:"fbMemoryUsage" yaml:"fbMemoryUsage"`
	Bar1MemoryUsage           Bar1MemoryUsage           `xml:"bar1_memory_usage" json:"bar1MemoryUsage" yaml:"bar1MemoryUsage"`
	CcProtectedMemoryUsage    CcProtectedMemoryUsage    `xml:"cc_protected_memory_usage" json:"ccProtectedMemoryUsage" yaml:"ccProtectedMemoryUsage"`
	ComputeMode               string                    `xml:"compute_mode" json:"computeMode" yaml:"computeMode"`
	Utilization               Utilization               `xml:"utilization" json:"utilization" yaml:"utilization"`
	EncoderStats              EncoderStats              `xml:"encoder_stats" json:"encoderStats" yaml:"encoderStats"`
	FbcStats                  FbcStats                  `xml:"fbc_stats" json:"fbcStats" yaml:"fbcStats"`
	DramEncryptionMode        DramEncryptionMode        `xml:"dram_encryption_mode" json:"dramEncryptionMode" yaml:"dramEncryptionMode"`
	EccMode                   EccMode                   `xml:"ecc_mode" json:"eccMode" yaml:"eccMode"`
	EccErrors                 EccErrors                 `xml:"ecc_errors" json:"eccErrors" yaml:"eccErrors"`
	RetiredPages              RetiredPages              `xml:"retired_pages" json:"retiredPages" yaml:"retiredPages"`
	RemappedRows              RemappedRows              `xml:"remapped_rows" json:"remappedRows" yaml:"remappedRows"`
	Temperature               Temperature               `xml:"temperature" json:"temperature" yaml:"temperature"`
	SupportedGpuTargetTemp    SupportedGpuTargetTemp    `xml:"supported_gpu_target_temp" json:"supportedGpuTargetTemp" yaml:"supportedGpuTargetTemp"`
	GpuPowerReadings          PowerReadings             `xml:"gpu_power_readings" json:"gpuPowerReadings" yaml:"gpuPowerReadings"`
	GpuMemoryPowerReadings    MemoryPowerReadings       `xml:"gpu_memory_power_readings" json:"gpuMemoryPowerReadings" yaml:"gpuMemoryPowerReadings"`
	ModulePowerReadings       ModulePowerReadings       `xml:"module_power_readings" json:"modulePowerReadings" yaml:"modulePowerReadings"`
	PowerSmoothing            string                    `xml:"power_smoothing" json:"powerSmoothing" yaml:"powerSmoothing"`
	PowerProfiles             PowerProfiles             `xml:"power_profiles" json:"powerProfiles" yaml:"powerProfiles"`
	Clocks                    Clocks                    `xml:"clocks" json:"clocks" yaml:"clocks"`
	ApplicationsClocks        ApplicationsClocks        `xml:"applications_clocks" json:"applicationsClocks" yaml:"applicationsClocks"`
	DefaultApplicationsClocks DefaultApplicationsClocks `xml:"default_applications_clocks" json:"defaultApplicationsClocks" yaml:"defaultApplicationsClocks"`
	DeferredClocks            DeferredClocks            `xml:"deferred_clocks" json:"deferredClocks" yaml:"deferredClocks"`
	MaxClocks                 MaxClocks                 `xml:"max_clocks" json:"maxClocks" yaml:"maxClocks"`
	MaxCustomerBoostClocks    MaxCustomerBoostClocks    `xml:"max_customer_boost_clocks" json:"maxCustomerBoostClocks" yaml:"maxCustomerBoostClocks"`
	ClockPolicy               ClockPolicy               `xml:"clock_policy" json:"clockPolicy" yaml:"clockPolicy"`
	Voltage                   Voltage                   `xml:"voltage" json:"voltage" yaml:"voltage"`
	Fabric                    Fabric                    `xml:"fabric" json:"fabric" yaml:"fabric"`
	SupportedClocks           SupportedClocks           `xml:"supported_clocks" json:"supportedClocks" yaml:"supportedClocks"`
	Processes                 string                    `xml:"processes" json:"processes" yaml:"processes"`
	AccountedProcesses        string                    `xml:"accounted_processes" json:"accountedProcesses" yaml:"accountedProcesses"`
	Capabilities              Capabilities              `xml:"capabilities" json:"capabilities" yaml:"capabilities"`
}

type MigMode struct {
	CurrentMig string `xml:"current_mig" json:"currentMig" yaml:"currentMig"`
	PendingMig string `xml:"pending_mig" json:"pendingMig" yaml:"pendingMig"`
}

type DriverModel struct {
	CurrentDm string `xml:"current_dm" json:"currentDm" yaml:"currentDm"`
	PendingDm string `xml:"pending_dm" json:"pendingDm" yaml:"pendingDm"`
}

type PlatformInfo struct {
	ChassisSerialNumber string `xml:"chassis_serial_number" json:"chassisSerialNumber" yaml:"chassisSerialNumber"`
	SlotNumber          string `xml:"slot_number" json:"slotNumber" yaml:"slotNumber"`
	TrayIndex           string `xml:"tray_index" json:"trayIndex" yaml:"trayIndex"`
	HostID              string `xml:"host_id" json:"hostId" yaml:"hostId"`
	PeerType            string `xml:"peer_type" json:"peerType" yaml:"peerType"`
	ModuleID            string `xml:"module_id" json:"moduleId" yaml:"moduleId"`
}

type InforomVersion struct {
	ImgVersion string `xml:"img_version" json:"imgVersion" yaml:"imgVersion"`
	OemObject  string `xml:"oem_object" json:"oemObject" yaml:"oemObject"`
	EccObject  string `xml:"ecc_object" json:"eccObject" yaml:"eccObject"`
	PwrObject  string `xml:"pwr_object" json:"pwrObject" yaml:"pwrObject"`
}

type InforomBbxFlush struct {
	LatestTimestamp string `xml:"latest_timestamp" json:"latestTimestamp" yaml:"latestTimestamp"`
	LatestDuration  string `xml:"latest_duration" json:"latestDuration" yaml:"latestDuration"`
}

type OperationMode struct {
	CurrentGom string `xml:"current_gom" json:"currentGom" yaml:"currentGom"`
	PendingGom string `xml:"pending_gom" json:"pendingGom" yaml:"pendingGom"`
}

type VirtualizationMode struct {
	VirtualizationMode    string `xml:"virtualization_mode" json:"virtualizationMode" yaml:"virtualizationMode"`
	HostVgpuMode          string `xml:"host_vgpu_mode" json:"hostVGPUMode" yaml:"hostVGPUMode"`
	VgpuHeterogeneousMode string `xml:"vgpu_heterogeneous_mode" json:"vgpuHeterogeneousMode" yaml:"vgpuHeterogeneousMode"`
}

type ResetStatus struct {
	ResetRequired            string `xml:"reset_required" json:"resetRequired" yaml:"resetRequired"`
	DrainAndResetRecommended string `xml:"drain_and_reset_recommended" json:"drainAndResetRecommended" yaml:"drainAndResetRecommended"`
}

type Ibmnpu struct {
	RelaxedOrderingMode string `xml:"relaxed_ordering_mode" json:"relaxedOrderingMode" yaml:"relaxedOrderingMode"`
}

type Pci struct {
	PciBus                string         `xml:"pci_bus" json:"pciBus" yaml:"pciBus"`
	PciDevice             string         `xml:"pci_device" json:"pciDevice" yaml:"pciDevice"`
	PciDomain             string         `xml:"pci_domain" json:"pciDomain" yaml:"pciDomain"`
	PciBaseClass          string         `xml:"pci_base_class" json:"pciBaseClass" yaml:"pciBaseClass"`
	PciSubClass           string         `xml:"pci_sub_class" json:"pciSubClass" yaml:"pciSubClass"`
	PciDeviceID           string         `xml:"pci_device_id" json:"pciDeviceId" yaml:"pciDeviceId"`
	PciBusID              string         `xml:"pci_bus_id" json:"pciBusId" yaml:"pciBusId"`
	PciSubSystemID        string         `xml:"pci_sub_system_id" json:"pciSubSystemId" yaml:"pciSubSystemId"`
	PciGpuLinkInfo        PciGpuLinkInfo `xml:"pci_gpu_link_info" json:"pciGPULinkInfo" yaml:"pciGPULinkInfo"`
	PciBridgeChip         PciBridgeChip  `xml:"pci_bridge_chip" json:"pciBridgeChip" yaml:"pciBridgeChip"`
	ReplayCounter         string         `xml:"replay_counter" json:"replayCounter" yaml:"replayCounter"`
	ReplayRolloverCounter string         `xml:"replay_rollover_counter" json:"replayRolloverCounter" yaml:"replayRolloverCounter"`
	TxUtil                string         `xml:"tx_util" json:"txUtil" yaml:"txUtil"`
	RxUtil                string         `xml:"rx_util" json:"rxUtil" yaml:"rxUtil"`
	AtomicCapsOutbound    string         `xml:"atomic_caps_outbound" json:"atomicCapsOutbound" yaml:"atomicCapsOutbound"`
	AtomicCapsInbound     string         `xml:"atomic_caps_inbound" json:"atomicCapsInbound" yaml:"atomicCapsInbound"`
}

type PciGpuLinkInfo struct {
	PcieGen    PcieGen    `xml:"pcie_gen" json:"pcieGen" yaml:"pcieGen"`
	LinkWidths LinkWidths `xml:"link_widths" json:"linkWidths" yaml:"linkWidths"`
}

type PcieGen struct {
	MaxLinkGen           string `xml:"max_link_gen" json:"maxLinkGen" yaml:"maxLinkGen"`
	CurrentLinkGen       string `xml:"current_link_gen" json:"currentLinkGen" yaml:"currentLinkGen"`
	DeviceCurrentLinkGen string `xml:"device_current_link_gen" json:"deviceCurrentLinkGen" yaml:"deviceCurrentLinkGen"`
	MaxDeviceLinkGen     string `xml:"max_device_link_gen" json:"maxDeviceLinkGen" yaml:"maxDeviceLinkGen"`
	MaxHostLinkGen       string `xml:"max_host_link_gen" json:"maxHostLinkGen" yaml:"maxHostLinkGen"`
}

type LinkWidths struct {
	MaxLinkWidth     string `xml:"max_link_width" json:"maxLinkWidth" yaml:"maxLinkWidth"`
	CurrentLinkWidth string `xml:"current_link_width" json:"currentLinkWidth" yaml:"currentLinkWidth"`
}

type PciBridgeChip struct {
	BridgeChipType string `xml:"bridge_chip_type" json:"bridgeChipType" yaml:"bridgeChipType"`
	BridgeChipFw   string `xml:"bridge_chip_fw" json:"bridgeChipFw" yaml:"bridgeChipFw"`
}

type ClocksEventReasons struct {
	ClocksEventReasonGpuIdle                   string `xml:"clocks_event_reason_gpu_idle" json:"clocksEventReasonGPUIdle" yaml:"clocksEventReasonGPUIdle"`
	ClocksEventReasonApplicationsClocksSetting string `xml:"clocks_event_reason_applications_clocks_setting" json:"clocksEventReasonApplicationsClocksSetting" yaml:"clocksEventReasonApplicationsClocksSetting"`
	ClocksEventReasonSwPowerCap                string `xml:"clocks_event_reason_sw_power_cap" json:"clocksEventReasonSwPowerCap" yaml:"clocksEventReasonSwPowerCap"`
	ClocksEventReasonHwSlowdown                string `xml:"clocks_event_reason_hw_slowdown" json:"clocksEventReasonHwSlowdown" yaml:"clocksEventReasonHwSlowdown"`
	ClocksEventReasonHwThermalSlowdown         string `xml:"clocks_event_reason_hw_thermal_slowdown" json:"clocksEventReasonHwThermalSlowdown" yaml:"clocksEventReasonHwThermalSlowdown"`
	ClocksEventReasonHwPowerBrakeSlowdown      string `xml:"clocks_event_reason_hw_power_brake_slowdown" json:"clocksEventReasonHwPowerBrakeSlowdown" yaml:"clocksEventReasonHwPowerBrakeSlowdown"`
	ClocksEventReasonSyncBoost                 string `xml:"clocks_event_reason_sync_boost" json:"clocksEventReasonSyncBoost" yaml:"clocksEventReasonSyncBoost"`
	ClocksEventReasonSwThermalSlowdown         string `xml:"clocks_event_reason_sw_thermal_slowdown" json:"clocksEventReasonSwThermalSlowdown" yaml:"clocksEventReasonSwThermalSlowdown"`
	ClocksEventReasonDisplayClocksSetting      string `xml:"clocks_event_reason_display_clocks_setting" json:"clocksEventReasonDisplayClocksSetting" yaml:"clocksEventReasonDisplayClocksSetting"`
}

type FbMemoryUsage struct {
	Total    string `xml:"total" json:"total" yaml:"total"`
	Reserved string `xml:"reserved" json:"reserved" yaml:"reserved"`
	Used     string `xml:"used" json:"used" yaml:"used"`
	Free     string `xml:"free" json:"free" yaml:"free"`
}

type Bar1MemoryUsage struct {
	Total string `xml:"total" json:"total" yaml:"total"`
	Used  string `xml:"used" json:"used" yaml:"used"`
	Free  string `xml:"free" json:"free" yaml:"free"`
}

type CcProtectedMemoryUsage struct {
	Total string `xml:"total" json:"total" yaml:"total"`
	Used  string `xml:"used" json:"used" yaml:"used"`
	Free  string `xml:"free" json:"free" yaml:"free"`
}

type Utilization struct {
	GpuUtil     string `xml:"gpu_util" json:"gpuUtil" yaml:"gpuUtil"`
	MemoryUtil  string `xml:"memory_util" json:"memoryUtil" yaml:"memoryUtil"`
	EncoderUtil string `xml:"encoder_util" json:"encoderUtil" yaml:"encoderUtil"`
	DecoderUtil string `xml:"decoder_util" json:"decoderUtil" yaml:"decoderUtil"`
	JpegUtil    string `xml:"jpeg_util" json:"jpegUtil" yaml:"jpegUtil"`
	OfaUtil     string `xml:"ofa_util" json:"ofaUtil" yaml:"ofaUtil"`
}

type EncoderStats struct {
	SessionCount   string `xml:"session_count" json:"sessionCount" yaml:"sessionCount"`
	AverageFps     string `xml:"average_fps" json:"averageFps" yaml:"averageFps"`
	AverageLatency string `xml:"average_latency" json:"averageLatency" yaml:"averageLatency"`
}

type FbcStats struct {
	SessionCount   string `xml:"session_count" json:"sessionCount" yaml:"sessionCount"`
	AverageFps     string `xml:"average_fps" json:"averageFps" yaml:"averageFps"`
	AverageLatency string `xml:"average_latency" json:"averageLatency" yaml:"averageLatency"`
}

type DramEncryptionMode struct {
	CurrentDramEncryption string `xml:"current_dram_encryption" json:"currentDramEncryption" yaml:"currentDramEncryption"`
	PendingDramEncryption string `xml:"pending_dram_encryption" json:"pendingDramEncryption" yaml:"pendingDramEncryption"`
}

type EccMode struct {
	CurrentEcc string `xml:"current_ecc" json:"currentEcc" yaml:"currentEcc"`
	PendingEcc string `xml:"pending_ecc" json:"pendingEcc" yaml:"pendingEcc"`
}

type EccErrors struct {
	Volatile                          Volatile                          `xml:"volatile" json:"volatile" yaml:"volatile"`
	Aggregate                         Aggregate                         `xml:"aggregate" json:"aggregate" yaml:"aggregate"`
	AggregateUncorrectableSramSources AggregateUncorrectableSramSources `xml:"aggregate_uncorrectable_sram_sources" json:"aggregateUncorrectableSramSources" yaml:"aggregateUncorrectableSramSources"`
}

type Volatile struct {
	SramCorrectable         string `xml:"sram_correctable" json:"sramCorrectable" yaml:"sramCorrectable"`
	SramUncorrectableParity string `xml:"sram_uncorrectable_parity" json:"sramUncorrectableParity" yaml:"sramUncorrectableParity"`
	SramUncorrectableSecded string `xml:"sram_uncorrectable_secded" json:"sramUncorrectableSecded" yaml:"sramUncorrectableSecded"`
	DramCorrectable         string `xml:"dram_correctable" json:"dramCorrectable" yaml:"dramCorrectable"`
	DramUncorrectable       string `xml:"dram_uncorrectable" json:"dramUncorrectable" yaml:"dramUncorrectable"`
}

type Aggregate struct {
	SramCorrectable         string `xml:"sram_correctable" json:"sramCorrectable" yaml:"sramCorrectable"`
	SramUncorrectableParity string `xml:"sram_uncorrectable_parity" json:"sramUncorrectableParity" yaml:"sramUncorrectableParity"`
	SramUncorrectableSecded string `xml:"sram_uncorrectable_secded" json:"sramUncorrectableSecded" yaml:"sramUncorrectableSecded"`
	DramCorrectable         string `xml:"dram_correctable" json:"dramCorrectable" yaml:"dramCorrectable"`
	DramUncorrectable       string `xml:"dram_uncorrectable" json:"dramUncorrectable" yaml:"dramUncorrectable"`
	SramThresholdExceeded   string `xml:"sram_threshold_exceeded" json:"sramThresholdExceeded" yaml:"sramThresholdExceeded"`
}

type AggregateUncorrectableSramSources struct {
	SramL2              string `xml:"sram_l2" json:"sramL2" yaml:"sramL2"`
	SramSm              string `xml:"sram_sm" json:"sramSm" yaml:"sramSm"`
	SramMicrocontroller string `xml:"sram_microcontroller" json:"sramMicrocontroller" yaml:"sramMicrocontroller"`
	SramPcie            string `xml:"sram_pcie" json:"sramPcie" yaml:"sramPcie"`
	SramOther           string `xml:"sram_other" json:"sramOther" yaml:"sramOther"`
}

type RetiredPages struct {
	MultipleSingleBitRetirement MultipleSingleBitRetirement `xml:"multiple_single_bit_retirement" json:"multipleSingleBitRetirement" yaml:"multipleSingleBitRetirement"`
	DoubleBitRetirement         DoubleBitRetirement         `xml:"double_bit_retirement" json:"doubleBitRetirement" yaml:"doubleBitRetirement"`
	PendingBlacklist            string                      `xml:"pending_blacklist" json:"pendingBlacklist" yaml:"pendingBlacklist"`
	PendingRetirement           string                      `xml:"pending_retirement" json:"pendingRetirement" yaml:"pendingRetirement"`
}

type MultipleSingleBitRetirement struct {
	RetiredCount    string `xml:"retired_count" json:"retiredCount" yaml:"retiredCount"`
	RetiredPagelist string `xml:"retired_pagelist" json:"retiredPagelist" yaml:"retiredPagelist"`
}

type DoubleBitRetirement struct {
	RetiredCount    string `xml:"retired_count" json:"retiredCount" yaml:"retiredCount"`
	RetiredPagelist string `xml:"retired_pagelist" json:"retiredPagelist" yaml:"retiredPagelist"`
}

type RemappedRows struct {
	RemappedRowCorr      string               `xml:"remapped_row_corr" json:"remappedRowCorr" yaml:"remappedRowCorr"`
	RemappedRowUnc       string               `xml:"remapped_row_unc" json:"remappedRowUnc" yaml:"remappedRowUnc"`
	RemappedRowPending   string               `xml:"remapped_row_pending" json:"remappedRowPending" yaml:"remappedRowPending"`
	RemappedRowFailure   string               `xml:"remapped_row_failure" json:"remappedRowFailure" yaml:"remappedRowFailure"`
	RowRemapperHistogram RowRemapperHistogram `xml:"row_remapper_histogram" json:"rowRemapperHistogram" yaml:"rowRemapperHistogram"`
}

type RowRemapperHistogram struct {
	RowRemapperHistogramMax     string `xml:"row_remapper_histogram_max" json:"rowRemapperHistogramMax" yaml:"rowRemapperHistogramMax"`
	RowRemapperHistogramHigh    string `xml:"row_remapper_histogram_high" json:"rowRemapperHistogramHigh" yaml:"rowRemapperHistogramHigh"`
	RowRemapperHistogramPartial string `xml:"row_remapper_histogram_partial" json:"rowRemapperHistogramPartial" yaml:"rowRemapperHistogramPartial"`
	RowRemapperHistogramLow     string `xml:"row_remapper_histogram_low" json:"rowRemapperHistogramLow" yaml:"rowRemapperHistogramLow"`
	RowRemapperHistogramNone    string `xml:"row_remapper_histogram_none" json:"rowRemapperHistogramNone" yaml:"rowRemapperHistogramNone"`
}

type Temperature struct {
	GpuTemp                      string `xml:"gpu_temp" json:"gpuTemp" yaml:"gpuTemp"`
	GpuTempTlimit                string `xml:"gpu_temp_tlimit" json:"gpuTempTlimit" yaml:"gpuTempTlimit"`
	GpuTempMaxTlimitThreshold    string `xml:"gpu_temp_max_tlimit_threshold" json:"gpuTempMaxTlimitThreshold" yaml:"gpuTempMaxTlimitThreshold"`
	GpuTempSlowTlimitThreshold   string `xml:"gpu_temp_slow_tlimit_threshold" json:"gpuTempSlowTlimitThreshold" yaml:"gpuTempSlowTlimitThreshold"`
	GpuTempMaxGpuTlimitThreshold string `xml:"gpu_temp_max_gpu_tlimit_threshold" json:"gpuTempMaxGPUTlimitThreshold" yaml:"gpuTempMaxGPUTlimitThreshold"`
	GpuTargetTemperature         string `xml:"gpu_target_temperature" json:"gpuTargetTemperature" yaml:"gpuTargetTemperature"`
	MemoryTemp                   string `xml:"memory_temp" json:"memoryTemp" yaml:"memoryTemp"`
	GpuTempMaxMemTlimitThreshold string `xml:"gpu_temp_max_mem_tlimit_threshold" json:"gpuTempMaxMemTlimitThreshold" yaml:"gpuTempMaxMemTlimitThreshold"`
}

type SupportedGpuTargetTemp struct {
	GpuTargetTempMin string `xml:"gpu_target_temp_min" json:"gpuTargetTempMin" yaml:"gpuTargetTempMin"`
	GpuTargetTempMax string `xml:"gpu_target_temp_max" json:"gpuTargetTempMax" yaml:"gpuTargetTempMax"`
}

type PowerReadings struct {
	PowerState          string `xml:"power_state" json:"powerState" yaml:"powerState"`
	PowerDraw           string `xml:"power_draw" json:"powerDraw" yaml:"powerDraw"`
	CurrentPowerLimit   string `xml:"current_power_limit" json:"currentPowerLimit" yaml:"currentPowerLimit"`
	RequestedPowerLimit string `xml:"requested_power_limit" json:"requestedPowerLimit" yaml:"requestedPowerLimit"`
	DefaultPowerLimit   string `xml:"default_power_limit" json:"defaultPowerLimit" yaml:"defaultPowerLimit"`
	MinPowerLimit       string `xml:"min_power_limit" json:"minPowerLimit" yaml:"minPowerLimit"`
	MaxPowerLimit       string `xml:"max_power_limit" json:"maxPowerLimit" yaml:"maxPowerLimit"`
}

type MemoryPowerReadings struct {
	PowerDraw string `xml:"power_draw" json:"powerDraw" yaml:"powerDraw"`
}

type ModulePowerReadings struct {
	PowerState          string `xml:"power_state" json:"powerState" yaml:"powerState"`
	PowerDraw           string `xml:"power_draw" json:"powerDraw" yaml:"powerDraw"`
	CurrentPowerLimit   string `xml:"current_power_limit" json:"currentPowerLimit" yaml:"currentPowerLimit"`
	RequestedPowerLimit string `xml:"requested_power_limit" json:"requestedPowerLimit" yaml:"requestedPowerLimit"`
	DefaultPowerLimit   string `xml:"default_power_limit" json:"defaultPowerLimit" yaml:"defaultPowerLimit"`
	MinPowerLimit       string `xml:"min_power_limit" json:"minPowerLimit" yaml:"minPowerLimit"`
	MaxPowerLimit       string `xml:"max_power_limit" json:"maxPowerLimit" yaml:"maxPowerLimit"`
}

type PowerProfiles struct {
	PowerProfileRequestedProfiles string `xml:"power_profile_requested_profiles" json:"powerProfileRequestedProfiles" yaml:"powerProfileRequestedProfiles"`
	PowerProfileEnforcedProfiles  string `xml:"power_profile_enforced_profiles" json:"powerProfileEnforcedProfiles" yaml:"powerProfileEnforcedProfiles"`
}

type Clocks struct {
	GraphicsClock string `xml:"graphics_clock" json:"graphicsClock" yaml:"graphicsClock"`
	SmClock       string `xml:"sm_clock" json:"smClock" yaml:"smClock"`
	MemClock      string `xml:"mem_clock" json:"memClock" yaml:"memClock"`
	VideoClock    string `xml:"video_clock" json:"videoClock" yaml:"videoClock"`
}

type ApplicationsClocks struct {
	GraphicsClock string `xml:"graphics_clock" json:"graphicsClock" yaml:"graphicsClock"`
	MemClock      string `xml:"mem_clock" json:"memClock" yaml:"memClock"`
}

type DefaultApplicationsClocks struct {
	GraphicsClock string `xml:"graphics_clock" json:"graphicsClock" yaml:"graphicsClock"`
	MemClock      string `xml:"mem_clock" json:"memClock" yaml:"memClock"`
}

type DeferredClocks struct {
	MemClock string `xml:"mem_clock" json:"memClock" yaml:"memClock"`
}

type MaxClocks struct {
	GraphicsClock string `xml:"graphics_clock" json:"graphicsClock" yaml:"graphicsClock"`
	SmClock       string `xml:"sm_clock" json:"smClock" yaml:"smClock"`
	MemClock      string `xml:"mem_clock" json:"memClock" yaml:"memClock"`
	VideoClock    string `xml:"video_clock" json:"videoClock" yaml:"videoClock"`
}

type MaxCustomerBoostClocks struct {
	GraphicsClock string `xml:"graphics_clock" json:"graphicsClock" yaml:"graphicsClock"`
}

type ClockPolicy struct {
	AutoBoost        string `xml:"auto_boost" json:"autoBoost" yaml:"autoBoost"`
	AutoBoostDefault string `xml:"auto_boost_default" json:"autoBoostDefault" yaml:"autoBoostDefault"`
}

type Voltage struct {
	GraphicsVolt string `xml:"graphics_volt" json:"graphicsVolt" yaml:"graphicsVolt"`
}

type Fabric struct {
	State       string `xml:"state" json:"state" yaml:"state"`
	Status      string `xml:"status" json:"status" yaml:"status"`
	Cliqueid    string `xml:"cliqueId" json:"cliqueId" yaml:"cliqueId"`
	Clusteruuid string `xml:"clusterUuid" json:"clusterUuid" yaml:"clusterUuid"`
	Health      Health `xml:"health" json:"health" yaml:"health"`
}

type Health struct {
	Bandwidth               string `xml:"bandwidth" json:"bandwidth" yaml:"bandwidth"`
	RouteRecoveryInProgress string `xml:"route_recovery_in_progress" json:"routeRecoveryInProgress" yaml:"routeRecoveryInProgress"`
	RouteUnhealthy          string `xml:"route_unhealthy" json:"routeUnhealthy" yaml:"routeUnhealthy"`
	AccessTimeoutRecovery   string `xml:"access_timeout_recovery" json:"accessTimeoutRecovery" yaml:"accessTimeoutRecovery"`
}

type SupportedClocks struct {
	SupportedMemClock SupportedMemClock `xml:"supported_mem_clock" json:"supportedMemClock" yaml:"supportedMemClock"`
}

type SupportedMemClock struct {
	Value                  string `xml:"value" json:"value" yaml:"value"`
	SupportedGraphicsClock string `xml:"supported_graphics_clock" json:"supportedGraphicsClock" yaml:"supportedGraphicsClock"`
}

type Capabilities struct {
	Egm string `xml:"egm" json:"egm" yaml:"egm"`
}
