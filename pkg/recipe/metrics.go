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

package recipe

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Recipe generation metrics
	recipeBuiltDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "aicr_recipe_build_duration_seconds",
			Help:    "Duration of recipe generation in seconds",
			Buckets: []float64{1, 5, 10, 30, 60, 120, 300},
		},
	)

	// Recipe metadata cache metrics
	recipeCacheHits = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "aicr_recipe_cache_hits_total",
			Help: "Total number of recipe metadata cache hits",
		},
	)
	recipeCacheMisses = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "aicr_recipe_cache_misses_total",
			Help: "Total number of recipe metadata cache misses (initial loads)",
		},
	)
)
