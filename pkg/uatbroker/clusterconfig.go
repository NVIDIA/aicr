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

package uatbroker

import (
	"bytes"
	stderrors "errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// maxClusterConfigBytes bounds a cluster-config read, well above the ~10 KiB
// committed configs.
const maxClusterConfigBytes int64 = 256 << 10 // 256 KiB

// clusterConfig is the subset of a github.com/mchmarny/cluster Cluster config
// that names capacity reservations.
type clusterConfig struct {
	Compute struct {
		EKS struct {
			NodeGroups struct {
				Workers []struct {
					Name     string `yaml:"name"`
					Capacity struct {
						Reservation struct {
							Target string `yaml:"target"`
						} `yaml:"reservation"`
					} `yaml:"capacity"`
				} `yaml:"workers"`
			} `yaml:"nodeGroups"`
		} `yaml:"eks"`
		GKE struct {
			NodePools struct {
				Workers []struct {
					Name       string `yaml:"name"`
					NodeConfig struct {
						CapacityReservations []string `yaml:"capacityReservations"`
					} `yaml:"nodeConfig"`
				} `yaml:"workers"`
			} `yaml:"nodePools"`
		} `yaml:"gke"`
	} `yaml:"compute"`
}

// reservationRef is a capacity reservation that a cluster-config worker pool
// targets.
type reservationRef struct {
	Worker string
	Target string
}

// parseClusterConfigReservations returns the capacity reservations that the
// cluster-config data targets for cloud, read from:
//
//   - aws: each EKS worker's non-empty capacity.reservation.target
//   - gcp: each GKE worker's nodeConfig.capacityReservations entry
//   - azure, kind: nothing, since they declare no reservation field
//
// An empty document targets none.
func parseClusterConfigReservations(cloud string, data []byte) ([]reservationRef, error) {
	var cfg clusterConfig
	// No KnownFields, since every other field belongs to the actuator's schema.
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&cfg); err != nil && !stderrors.Is(err, io.EOF) {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse cluster-config", err)
	}
	var refs []reservationRef
	switch cloud {
	case CloudAWS:
		for _, w := range cfg.Compute.EKS.NodeGroups.Workers {
			if t := w.Capacity.Reservation.Target; t != "" {
				refs = append(refs, reservationRef{Worker: w.Name, Target: t})
			}
		}
	case CloudGCP:
		for _, w := range cfg.Compute.GKE.NodePools.Workers {
			for _, t := range w.NodeConfig.CapacityReservations {
				refs = append(refs, reservationRef{Worker: w.Name, Target: t})
			}
		}
	}
	return refs, nil
}

// CheckClusterConfigReservations returns an error unless each row's
// cluster-config, resolved against root, targets exactly the row's
// reservation-id. Special cases are:
//
//   - a row with a reservation-id fails if its cluster-config targets none
//   - a row without a reservation-id fails if its cluster-config targets any
//   - a cluster-config-path that is not local to root fails
//
// The error lists every mismatch across all rows.
func (r *Registry) CheckClusterConfigReservations(root string) error {
	var problems []string
	for i := range r.Reservations {
		res := &r.Reservations[i]
		p, err := res.clusterConfigProblems(root)
		if err != nil {
			return err
		}
		problems = append(problems, p...)
	}
	if len(problems) > 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"reservation registry disagrees with its cluster-configs: "+strings.Join(problems, "; "))
	}
	return nil
}

// clusterConfigProblems returns problems, one message per disagreement between
// res.ReservationID and the targets of res's cluster-config under root. err is
// non-nil only when that cluster-config path is not local or the file cannot
// be read or parsed.
func (res *Reservation) clusterConfigProblems(root string) (problems []string, err error) {
	rel := filepath.FromSlash(res.ClusterConfigPath)
	if !filepath.IsLocal(rel) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("reservation %s cluster-config-path %q must be a relative path inside the repository",
				res.Name, res.ClusterConfigPath))
	}
	data, err := readFileBounded(filepath.Join(root, rel), "cluster-config", maxClusterConfigBytes)
	if err != nil {
		return nil, err
	}
	refs, err := parseClusterConfigReservations(res.Cloud, data)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("reservation %s cluster-config %s", res.Name, res.ClusterConfigPath), err)
	}

	if res.ReservationID == "" {
		for _, ref := range refs {
			problems = append(problems, fmt.Sprintf(
				"reservation %s has no reservation-id but %s worker %q targets %s",
				res.Name, res.ClusterConfigPath, ref.Worker, ref.Target))
		}
		return problems, nil
	}
	if len(refs) == 0 {
		return []string{fmt.Sprintf(
			"reservation %s has reservation-id %s but %s declares no capacity reservation",
			res.Name, res.ReservationID, res.ClusterConfigPath)}, nil
	}
	for _, ref := range refs {
		if ref.Target != res.ReservationID {
			problems = append(problems, fmt.Sprintf(
				"reservation %s has reservation-id %s but %s worker %q targets %s",
				res.Name, res.ReservationID, res.ClusterConfigPath, ref.Worker, ref.Target))
		}
	}
	return problems, nil
}
