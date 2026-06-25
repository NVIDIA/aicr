/*
2024 NVIDIA CORPORATION & AFFILIATES
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package consts

const (
	MellanoxVendor       = "15b3"
	BlueField3DeviceID   = "a2dc"
	BlueField3LxDeviceID = "a2d9"
	BlueField2DeviceID   = "a2d6"
	BlueField4DeviceID   = "a2df"

	Ethernet   = "Ethernet"
	Infiniband = "Infiniband"

	ConfigUpdateInProgressCondition     = "ConfigUpdateInProgress"
	FirmwareConfigMatchCondition        = "FirmwareConfigMatch"
	IncorrectSpecReason                 = "IncorrectSpec"
	UpdateStartedReason                 = "UpdateStarted"
	PendingRebootReason                 = "PendingReboot"
	PendingNodeMaintenanceReason        = "PendingNodeMaintenance"
	NonVolatileConfigUpdateFailedReason = "NonVolatileConfigUpdateFailed"
	RuntimeConfigUpdateFailedReason     = "RuntimeConfigUpdateFailed"
	UpdateSuccessfulReason              = "UpdateSuccessful"
	SpecValidationFailed                = "SpecValidationFailed"
	FirmwareError                       = "FirmwareError"
	PendingFirmwareUpdateReason         = "PendingFirmwareUpdate"
	PendingOwnFirmwareUpdateMessage     = "Configuration update is pending the device's firmware update"
	PendingOtherFirmwareUpdateMessage   = "Configuration update is pending the firmware update on other devices on the node"
	FirmwareUpdateInProgressCondition   = "FirmwareUpdateInProgress"
	FirmwareUpdateStartedReason         = "FirmwareUpdateStarted"
	FirmwareUpdateFailedReason          = "FirmwareUpdateFailed"
	FirmwareSourceNotReadyReason        = "FirmwareSourceNotReady"
	FirmwareSourceFailedReason          = "FirmwareSourceFailed"

	DeviceConfigSpecEmptyReason   = "DeviceConfigSpecEmpty"
	DeviceFirmwareSpecEmptyReason = "DeviceFirmwareSpecEmpty"
	DeviceFwMatchReason           = "DeviceFirmwareConfigMatch"
	DeviceFwMatchMessage          = "Firmware matches the requested version"
	DeviceFwMismatchReason        = "DeviceFirmwareConfigMismatch"
	DeviceFwMismatchMessage       = "Firmware doesn't match the requested version, can't proceed with update because policy is set to Validate"

	PartNumberPrefix          = "PN"
	PartNumberDescription     = "Part Number"
	SerialNumberPrefix        = "SN"
	SerialNumberDescription   = "Serial Number"
	ModelNamePrefix           = "IDTAG"
	ModelNameDescription      = "Board Id"
	FirmwareVersionPrefix     = "fw version:"
	PSIDPrefix                = "psid:"
	LinkStatsPrefix           = "lnksta"
	MaxReadReqPrefix          = "maxreadreq"
	TrustStatePrefix          = "priority trust state:"
	PfcEnabledPrefix          = "enabled"
	ZeroTrustHostConfigPrefix = "level"

	SuperNIC = "SuperNIC"

	NetClass = 0x02

	LastAppliedStateAnnotation = "lastAppliedState"

	NvParamFalse              = "0"
	NvParamTrue               = "1"
	NvParamLinkTypeInfiniband = "1"
	NvParamLinkTypeEthernet   = "2"
	NvParamZero               = "0"
	NvParamBF3DpuMode         = "0"
	NvParamBF3NicMode         = "1"

	SriovEnabledParam        = "SRIOV_EN"
	SriovNumOfVfsParam       = "NUM_OF_VFS"
	LinkTypeP1Param          = "LINK_TYPE_P1"
	LinkTypeP2Param          = "LINK_TYPE_P2"
	MaxAccOutReadParam       = "MAX_ACC_OUT_READ"
	RoceCcPrioMaskP1Param    = "ROCE_CC_PRIO_MASK_P1"
	RoceCcPrioMaskP2Param    = "ROCE_CC_PRIO_MASK_P2"
	CnpDscpP1Param           = "CNP_DSCP_P1"
	CnpDscpP2Param           = "CNP_DSCP_P2"
	Cnp802pPrioP1Param       = "CNP_802P_PRIO_P1"
	Cnp802pPrioP2Param       = "CNP_802P_PRIO_P2"
	AtsEnabledParam          = "ATS_ENABLED"
	AdvancedPCISettingsParam = "ADVANCED_PCI_SETTINGS"
	BF3OperationModeParam    = "INTERNAL_CPU_OFFLOAD_ENGINE"

	SecondPortPrefix = "P2"

	EnvBaremetal = "Baremetal"

	MaintenanceRequestor   = "configuration.nic.mellanox.com"
	MaintenanceRequestName = "nic-configuration-operator-maintenance"

	HostPath = "/host"

	SupportedNicFirmwareConfigmap = "nic-configuration-operator-supported-nic-firmware"
	Mlx5ModuleVersionPath         = "/sys/bus/pci/drivers/mlx5_core/module/version"

	FwConfigNotAppliedAfterRebootErrorMsg = "firmware configuration failed to apply after reboot"

	NicFirmwareStorage             = "/nic-firmware"
	NicFirmwareBinariesFolder      = "firmware-binaries"
	BFBFolder                      = "bfb"
	DocaSpcXCCFolder               = "doca-spc-x-cc"
	NicFirmwareBinaryFileExtension = ".bin"
	BFBFileExtension               = ".bfb"
	DebPackageExtension            = ".deb"

	TempDir = "/tmp"

	// FirmwareSourceDownloadingStatus is set when the FW binary archives are being downloaded
	FirmwareSourceDownloadingStatus = "Downloading"
	// FirmwareSourceDownloadFailedStatus is set when the FW binary archives download failed with an error
	FirmwareSourceDownloadFailedStatus = "DownloadFailed"
	// FirmwareSourceProcessingStatus is set when the downloaded FW binaries are being unzipped from archives, moved to the cache storage according to their metadata
	FirmwareSourceProcessingStatus = "Processing"
	// FirmwareSourceProcessingFailedStatus is set when the FW binaries couldn't be unzipped or processed
	FirmwareSourceProcessingFailedStatus = "ProcessingFailed"
	// FirmwareSourceSuccessStatus is set when all FW binary were successfully downloaded and processed and the cache is ready to be used
	FirmwareSourceSuccessStatus = "Success"
	// FirmwareSourceCacheVerificationFailedStatus is set when the initial cache validation has failed, e.g. when the metadata file exists but can't be read
	FirmwareSourceCacheVerificationFailedStatus = "CacheVerificationFailed"

	FirmwareUpdatePolicyValidate = "Validate"
	FirmwareUpdatePolicyUpdate   = "Update"

	FirmwareSourceFinalizerName = "configuration.net.nvidia.com/nic-configuration-operator"

	TrustModeDscp = "dscp"
	TrustModePfc  = "pfc"

	OverlayNone = "none"
	OverlayL3   = "l3"

	NodeNicConfigurationWaitLabel = "network.nvidia.com/operator.nic-configuration.wait"
	LabelValueTrue                = "true"
	LabelValueFalse               = "false"

	FEATURE_GATE_FW_RESET_AFTER_CONFIG_UPDATE = "FW_RESET_AFTER_CONFIG_UPDATE"

	MultiplaneModeNone     = "none"
	MultiplaneModeSwplb    = "swplb"
	MultiplaneModeHwplb    = "hwplb"
	MultiplaneModeUniplane = "uniplane"

	HostRestrictionLevelPrivileged = "privileged"
	HostRestrictionLevelRestricted = "restricted"

	// InterfaceNameCondition is the condition type for interface name template application
	InterfaceNameCondition = "InterfaceNameApplied"
	// InterfaceNameAppliedReason indicates interface names were applied successfully
	InterfaceNameAppliedReason = "InterfaceNameApplied"
	// InterfaceNameMismatchReason indicates interface names don't match the expected values
	InterfaceNameMismatchReason = "InterfaceNameMismatch"
)
