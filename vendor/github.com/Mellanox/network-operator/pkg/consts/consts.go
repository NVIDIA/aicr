/*
Copyright 2020 NVIDIA

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

// Package consts contains constants used throughout the project.
package consts

// Note: if a different logger is used than zap (operator-sdk default), these values would probably need to change.
const (
	// LogLevelError is the ERROR log level.
	LogLevelError = iota - 2
	// LogLevelWarning is the WARNING log level.
	LogLevelWarning
	// LogLevelInfo is the INFO log level.
	LogLevelInfo
	// LogLevelDebug is the DEBUG log level.
	LogLevelDebug
)

const (
	// NicClusterPolicyResourceName is the name for the NicClusterPolicy resource.
	NicClusterPolicyResourceName = "nic-cluster-policy"
	// OfedDriverLabel is the label key for ofed driver Pods and DaemonSets.
	OfedDriverLabel = "nvidia.com/ofed-driver"
	// NicConfigurationDaemonLabel is the label key for nic-configuration-daemon Pods and DaemonSets.
	NicConfigurationDaemonLabel = "nvidia.com/nic-configuration-daemon"
	// StateLabel is the label key describing which state the operator created a Kubernetes object from.
	StateLabel = "nvidia.network-operator.state"
	// DSOwnerLabel is the label key identifying which CRD type owns a DaemonSet.
	DSOwnerLabel = "ds-owner"
	// DefaultCniBinDirectory is the default location of the CNI binaries on a host.
	DefaultCniBinDirectory = "/opt/cni/bin"
	// DefaultCniNetworkDirectory is the default location of the CNI network configuration on a host.
	DefaultCniNetworkDirectory = "/etc/cni/net.d"
	// OcpCniBinDirectory is the location of the CNI binaries on an OpenShift host.
	OcpCniBinDirectory = "/var/lib/cni/bin"
	// OfedDriverSkipDrainLabelSelector contains labelselector which is used to indicate
	// that network-operator pod should be skipped during the drain operation which
	// is executed by the upgrade controller.
	OfedDriverSkipDrainLabelSelector = "nvidia.com/ofed-driver-upgrade-drain.skip!=true"
	// ControllerRevisionAnnotation is the key for annotations used to store revision information on Kubernetes objects.
	ControllerRevisionAnnotation = "nvidia.network-operator.revision"
	// ConfigHashAnnotation is the key for annotations used to store config hash on pod templates.
	ConfigHashAnnotation = "nvidia.network-operator.config-hash"
)
