// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"
)

// EastWestDeviceID returns the normalized PCI device ID shared by every
// east-west PF in a source or render group. North-south PFs do not participate
// because Spectrum-X configuration targets the rail fabric only.
//
// The bool reports whether the group contains any east-west PFs. A group with
// missing or mixed east-west device IDs returns an error so hardware defaulting
// and manifest generation enforce the same inventory constraints.
func EastWestDeviceID(group ClusterConfig) (deviceID string, hasEastWest bool, err error) {
	for _, pf := range group.PFs {
		if pf.Traffic != "east-west" {
			continue
		}
		hasEastWest = true
		normalizedID := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(pf.DeviceID)), "0x")
		if normalizedID == "" {
			return "", true, fmt.Errorf("group %q has an east-west PF without a deviceID", group.Identifier)
		}
		if deviceID == "" {
			deviceID = normalizedID
			continue
		}
		if deviceID != normalizedID {
			return "", true, fmt.Errorf(
				"group %q east-west PFs have mixed deviceIDs: %q vs %q",
				group.Identifier, deviceID, normalizedID,
			)
		}
	}
	return deviceID, hasEastWest, nil
}

// EastWestDeviceIDForGroups returns the single device ID shared by all
// east-west PFs in the supplied groups. Groups without east-west PFs are
// skipped. This preserves the Spectrum-X constraint that one generation run
// cannot target multiple east-west NIC types with one profile configuration.
func EastWestDeviceIDForGroups(groups []ClusterConfig) (deviceID string, hasEastWest bool, err error) {
	for _, group := range groups {
		groupDeviceID, groupHasEastWest, groupErr := EastWestDeviceID(group)
		if groupErr != nil {
			return "", true, groupErr
		}
		if !groupHasEastWest {
			continue
		}
		hasEastWest = true
		if deviceID == "" {
			deviceID = groupDeviceID
			continue
		}
		if deviceID != groupDeviceID {
			return "", true, fmt.Errorf(
				"east-west PFs have mixed deviceIDs: %q vs %q",
				deviceID, groupDeviceID,
			)
		}
	}
	return deviceID, hasEastWest, nil
}
