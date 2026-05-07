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

package snapshotter

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Snapshot collection metrics
	snapshotCollectionDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "aicr_snapshot_collection_duration_seconds",
			Help:    "Time taken to collect a complete node snapshot",
			Buckets: []float64{1, 5, 10, 30, 60, 120, 300},
		},
	)

	snapshotCollectionTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aicr_snapshot_collection_total",
			Help: "Total number of snapshot collection attempts",
		},
		[]string{"status"}, // success or error
	)

	snapshotCollectorDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aicr_snapshot_collector_duration_seconds",
			Help:    "Time taken by individual collectors",
			Buckets: []float64{0.1, 0.5, 1, 5, 10, 30},
		},
		[]string{"collector"}, // image, k8s, kmod, systemd, grub, sysctl, smi, metadata
	)

	snapshotMeasurementCount = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "aicr_snapshot_measurements",
			Help: "Number of measurements in the last collected snapshot",
		},
	)
)
