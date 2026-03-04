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

// Package measurement provides types and utilities for collecting, comparing, and filtering
// system measurements from various sources (Kubernetes, GPU, OS, SystemD).
//
// # Core Types
//
// The package defines a hierarchical structure for measurements:
//   - Type: Enum identifying the measurement source (K8s, GPU, OS, SystemD)
//   - Measurement: Contains a Type and a slice of Subtypes
//   - Subtype: Named collection of key-value data (e.g., "cluster", "node")
//   - Reading: Interface for type-safe scalar values (int, float64, string, bool, etc.)
//
// # Creating Measurements
//
// Use convenience constructors to create readings:
//
//	m := &Measurement{
//	    Type: TypeK8s,
//	    Subtypes: []Subtype{
//	        {
//	            Name: "cluster",
//	            Data: map[string]Reading{
//	                "version": Str("1.28.0"),
//	                "nodes":   Int(3),
//	                "ready":   Bool(true),
//	            },
//	        },
//	    },
//	}
//
// Or use the builder pattern for cleaner code:
//
//	m := NewMeasurement(TypeK8s).
//	    WithSubtype(
//	        NewSubtypeBuilder("cluster").
//	            Set("version", Str("1.28.0")).
//	            Set("nodes", Int(3)).
//	            Build(),
//	    )
//
// # Accessing Data
//
// Use type-safe getters to retrieve values:
//
//	version, err := m.GetSubtype("cluster").GetString("version")
//	nodes, err := m.GetSubtype("cluster").GetInt64("nodes")
//	ready, err := m.GetSubtype("cluster").GetBool("ready")
//
// # Filtering Data
//
// Filter sensitive or unwanted keys using wildcard patterns:
//
//	// Remove all keys containing "password" or starting with "secret"
//	filtered := FilterOut(readings, []string{"*password*", "secret*"})
//
//	// Keep only version and count fields
//	kept := FilterIn(readings, []string{"version", "count"})
//
// # Serialization
//
// Measurements support JSON and YAML marshaling/unmarshaling:
//
//	data, _ := json.Marshal(m)
//	yaml, _ := yaml.Marshal(m)
//
// The Reading interface is automatically marshaled to its underlying value,
// avoiding wrapper structures in the output.
package measurement
