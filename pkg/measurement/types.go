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

package measurement

import (
	"encoding/json"
	"fmt"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// Common measurement keys exported for consistency and type safety.
const (
	// Kubernetes measurement keys
	KeyVersion     = "version"
	KeyNodes       = "nodes"
	KeyPods        = "pods"
	KeyNamespace   = "namespace"
	KeyClusterName = "cluster-name"
	KeyReady       = "ready"

	// GPU measurement keys
	KeyGPUDriver = "driver"
	KeyGPUModel  = "model"
	KeyGPUCount  = "gpu-count"
	KeyGPUMemory = "memory"
	KeyGPUTemp   = "temperature"
	KeyGPUPower  = "power"
	KeyGPUUUID   = "uuid"

	// OS measurement keys
	KeyOSName    = "name"
	KeyOSVersion = "os-version"
	KeyKernel    = "kernel"
	KeyArch      = "architecture"
	KeyHostname  = "hostname"

	// SystemD measurement keys
	KeyServiceName   = "service-name"
	KeyServiceState  = "state"
	KeyServiceStatus = "status"
	KeyEnabled       = "enabled"
	KeyActive        = "active"
)

// Type represents the category of a measurement (e.g., Kubernetes, GPU, OS, SystemD).
type Type string

// String returns the string representation of the measurement Type.
func (mt Type) String() string {
	return string(mt)
}

const (
	TypeK8s          Type = "K8s"
	TypeGPU          Type = "GPU"
	TypeOS           Type = "OS"
	TypeSystemD      Type = "SystemD"
	TypeNodeTopology Type = "NodeTopology"
)

// Types is the list of all supported measurement types.
var Types = []Type{
	TypeK8s,
	TypeGPU,
	TypeOS,
	TypeSystemD,
	TypeNodeTopology,
}

// ParseType parses a string into a measurement Type.
// Returns the Type and true if parsing succeeds, or empty Type and false if the string is invalid.
func ParseType(s string) (Type, bool) {
	for _, mt := range Types {
		if string(mt) == s {
			return mt, true
		}
	}
	return "", false
}

// Measurement represents collected data of a specific type with multiple subtypes.
// Each measurement contains a category (Type) and one or more Subtypes with their associated data.
type Measurement struct {
	Type     Type      `json:"type" yaml:"type"`
	Subtypes []Subtype `json:"subtypes,omitempty" yaml:"subtypes,omitempty"`
}

// Subtype represents a specific subcategory of measurement with associated data.
// Data contains the actual measurements as key-value pairs.
// Context provides additional metadata about the measurement environment.
type Subtype struct {
	Name    string             `json:"subtype,omitempty" yaml:"subtype,omitempty"`
	Data    map[string]Reading `json:"data" yaml:"data"`
	Context map[string]string  `json:"context,omitempty" yaml:"context,omitempty"`
}

// UnmarshalJSON custom unmarshaler for Subtype to handle Reading interface
func (st *Subtype) UnmarshalJSON(data []byte) error {
	// Create a temporary struct with raw data
	var tmp struct {
		Name    string            `json:"subtype"`
		Data    map[string]any    `json:"data"`
		Context map[string]string `json:"context"`
	}

	if err := json.Unmarshal(data, &tmp); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "failed to unmarshal subtype JSON", err)
	}

	st.Name = tmp.Name
	st.Context = tmp.Context
	st.Data = make(map[string]Reading)

	// Convert each value to a Reading using ToReading
	for k, v := range tmp.Data {
		st.Data[k] = ToReading(v)
	}

	return nil
}

// UnmarshalYAML custom unmarshaler for Subtype to handle Reading interface
func (st *Subtype) UnmarshalYAML(node *yaml.Node) error {
	// Create a temporary struct with raw data
	var tmp struct {
		Name    string            `yaml:"subtype"`
		Data    map[string]any    `yaml:"data"`
		Context map[string]string `yaml:"context"`
	}

	if err := node.Decode(&tmp); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "failed to decode subtype YAML", err)
	}

	st.Name = tmp.Name
	st.Context = tmp.Context
	st.Data = make(map[string]Reading)

	// Convert each value to a Reading using ToReading
	for k, v := range tmp.Data {
		st.Data[k] = ToReading(v)
	}

	return nil
}

// AllowedScalar is a constraint (compile-time) for what we allow as readings.
type AllowedScalar interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64 |
		~bool |
		~string
}

// Reading is a *runtime* interface (so it can be stored in a map with mixed types).
type Reading interface {
	isReading()
	Any() any
	String() string

	json.Marshaler
	json.Unmarshaler
	yaml.Marshaler
	yaml.Unmarshaler
}

// Scalar wraps an allowed scalar type.
// This is how we keep compile-time constraints while still using a runtime interface.
type Scalar[T AllowedScalar] struct {
	V T
}

func (Scalar[T]) isReading() {}

func (s Scalar[T]) Any() any { return s.V }

// String returns the string representation of the underlying scalar value.
func (s Scalar[T]) String() string {
	return fmt.Sprintf("%v", s.V)
}

// MarshalJSON makes the JSON value be the underlying scalar (not an object wrapper).
func (s Scalar[T]) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.V)
}

// MarshalYAML makes the YAML value be the underlying scalar (not an object wrapper).
func (s Scalar[T]) MarshalYAML() (any, error) {
	return s.V, nil
}

// ToReading creates a Reading from any allowed scalar type.
// If the type is not allowed, it returns a string representation.
func ToReading(v any) Reading {
	switch val := v.(type) {
	case int:
		return Int(val)
	case int64:
		return Int64(val)
	case uint:
		return Uint(val)
	case uint64:
		return Uint64(val)
	case float64:
		return Float64(val)
	case bool:
		return Bool(val)
	case string:
		return Str(val)
	default:
		return Str(fmt.Sprintf("%v", val))
	}
}

// ToReadingWithType converts a value to a Reading and returns whether the conversion
// was lossy (i.e., converted to string via fmt.Sprintf).
// This allows callers to detect if unexpected types were encountered.
func ToReadingWithType(v any) (Reading, bool) {
	switch val := v.(type) {
	case int:
		return Int(val), true
	case int64:
		return Int64(val), true
	case uint:
		return Uint(val), true
	case uint64:
		return Uint64(val), true
	case float64:
		return Float64(val), true
	case bool:
		return Bool(val), true
	case string:
		return Str(val), true
	default:
		return Str(fmt.Sprintf("%v", val)), false
	}
}

// UnmarshalJSON unmarshals a JSON value into the underlying scalar.
func (s *Scalar[T]) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &s.V)
}

// UnmarshalYAML unmarshals a YAML value into the underlying scalar.
func (s *Scalar[T]) UnmarshalYAML(node *yaml.Node) error {
	return node.Decode(&s.V)
}

// Convenience constructors for each allowed scalar type.
func Int(v int) Reading         { return &Scalar[int]{V: v} }
func Int64(v int64) Reading     { return &Scalar[int64]{V: v} }
func Uint(v uint) Reading       { return &Scalar[uint]{V: v} }
func Uint64(v uint64) Reading   { return &Scalar[uint64]{V: v} }
func Float64(v float64) Reading { return &Scalar[float64]{V: v} }
func Bool(v bool) Reading       { return &Scalar[bool]{V: v} }
func Str(v string) Reading      { return &Scalar[string]{V: v} }

// Validate checks if the measurement is properly formed.
func (m *Measurement) Validate() error {
	if m.Type == "" {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "measurement type cannot be empty")
	}
	if len(m.Subtypes) == 0 {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "measurement must have at least one subtype")
	}
	for i, st := range m.Subtypes {
		if err := st.Validate(); err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("subtype[%d]", i), err)
		}
	}
	return nil
}

// GetSubtype retrieves a subtype by name, returning nil if not found.
func (m *Measurement) GetSubtype(name string) *Subtype {
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == name {
			return &m.Subtypes[i]
		}
	}
	return nil
}

// HasSubtype checks if a subtype with the given name exists.
func (m *Measurement) HasSubtype(name string) bool {
	return m.GetSubtype(name) != nil
}

// GetOrCreateSubtype retrieves a subtype by name, creating it if it doesn't exist.
// This simplifies dynamic measurement building by avoiding manual check-and-append logic.
func (m *Measurement) GetOrCreateSubtype(name string) *Subtype {
	if st := m.GetSubtype(name); st != nil {
		return st
	}
	// Create new subtype with empty data
	newSubtype := Subtype{
		Name: name,
		Data: make(map[string]Reading),
	}
	m.Subtypes = append(m.Subtypes, newSubtype)
	// Return pointer to the newly added subtype
	return &m.Subtypes[len(m.Subtypes)-1]
}

// SubtypeNames returns all subtype names.
func (m *Measurement) SubtypeNames() []string {
	names := make([]string, len(m.Subtypes))
	for i, st := range m.Subtypes {
		names[i] = st.Name
	}
	return names
}

// Merge combines two measurements by adding or updating subtypes from other into m.
// If a subtype exists in both measurements, the data is merged (other's values take precedence).
// Returns an error if the measurements have different types.
func (m *Measurement) Merge(other *Measurement) error {
	if m.Type != other.Type {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("cannot merge measurements of different types: %s and %s", m.Type, other.Type))
	}

	for _, otherSt := range other.Subtypes {
		existingSt := m.GetSubtype(otherSt.Name)
		if existingSt == nil {
			// Subtype doesn't exist, add it
			m.Subtypes = append(m.Subtypes, Subtype{
				Name: otherSt.Name,
				Data: copyReadings(otherSt.Data),
			})
		} else {
			// Subtype exists, merge data (other wins on conflicts)
			for key, value := range otherSt.Data {
				existingSt.Data[key] = value
			}
		}
	}
	return nil
}

// copyReadings creates a shallow copy of a readings map.
func copyReadings(src map[string]Reading) map[string]Reading {
	dst := make(map[string]Reading, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// Validate checks if the subtype is properly formed.
func (st *Subtype) Validate() error {
	if len(st.Data) == 0 {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "subtype data cannot be empty")
	}
	return nil
}

// Has checks if a key exists in the subtype data.
func (st *Subtype) Has(key string) bool {
	_, exists := st.Data[key]
	return exists
}

// Get retrieves a reading by key, returning nil if not found.
func (st *Subtype) Get(key string) Reading {
	return st.Data[key]
}

// Keys returns all keys in the subtype data.
func (st *Subtype) Keys() []string {
	keys := make([]string, 0, len(st.Data))
	for k := range st.Data {
		keys = append(keys, k)
	}
	return keys
}

// GetString attempts to retrieve a string value, returning an error if not found or wrong type.
func (st *Subtype) GetString(key string) (string, error) {
	reading := st.Data[key]
	if reading == nil {
		return "", aicrerrors.New(aicrerrors.ErrCodeNotFound, fmt.Sprintf("key %q not found", key))
	}
	v, ok := reading.Any().(string)
	if !ok {
		return "", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("key %q is not a string", key))
	}
	return v, nil
}

// GetInt64 attempts to retrieve an int64 value, returning an error if not found or wrong type.
func (st *Subtype) GetInt64(key string) (int64, error) {
	reading := st.Data[key]
	if reading == nil {
		return 0, aicrerrors.New(aicrerrors.ErrCodeNotFound, fmt.Sprintf("key %q not found", key))
	}
	// Handle both int64 and int
	switch v := reading.Any().(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return 0, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("key %q is not an integer", key))
	}
}

// GetUint64 attempts to retrieve a uint64 value, returning an error if not found or wrong type.
func (st *Subtype) GetUint64(key string) (uint64, error) {
	reading := st.Data[key]
	if reading == nil {
		return 0, aicrerrors.New(aicrerrors.ErrCodeNotFound, fmt.Sprintf("key %q not found", key))
	}
	// Handle both uint64 and uint
	switch v := reading.Any().(type) {
	case uint64:
		return v, nil
	case uint:
		return uint64(v), nil
	default:
		return 0, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("key %q is not an unsigned integer", key))
	}
}

// GetFloat64 attempts to retrieve a float64 value, returning an error if not found or wrong type.
func (st *Subtype) GetFloat64(key string) (float64, error) {
	reading := st.Data[key]
	if reading == nil {
		return 0, aicrerrors.New(aicrerrors.ErrCodeNotFound, fmt.Sprintf("key %q not found", key))
	}
	v, ok := reading.Any().(float64)
	if !ok {
		return 0, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("key %q is not a float64", key))
	}
	return v, nil
}

// GetBool attempts to retrieve a bool value, returning an error if not found or wrong type.
func (st *Subtype) GetBool(key string) (bool, error) {
	reading := st.Data[key]
	if reading == nil {
		return false, aicrerrors.New(aicrerrors.ErrCodeNotFound, fmt.Sprintf("key %q not found", key))
	}
	v, ok := reading.Any().(bool)
	if !ok {
		return false, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("key %q is not a bool", key))
	}
	return v, nil
}
