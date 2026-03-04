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

package measurement

// SubtypeBuilder provides a fluent API for building Subtype instances.
type SubtypeBuilder struct {
	name string
	data map[string]Reading
}

// NewSubtypeBuilder creates a new SubtypeBuilder with the given name.
func NewSubtypeBuilder(name string) *SubtypeBuilder {
	return &SubtypeBuilder{
		name: name,
		data: make(map[string]Reading),
	}
}

// Set adds or updates a key-value pair in the subtype data.
func (b *SubtypeBuilder) Set(key string, value Reading) *SubtypeBuilder {
	b.data[key] = value
	return b
}

// SetString is a convenience method for adding string values.
func (b *SubtypeBuilder) SetString(key, value string) *SubtypeBuilder {
	b.data[key] = Str(value)
	return b
}

// SetInt is a convenience method for adding int values.
func (b *SubtypeBuilder) SetInt(key string, value int) *SubtypeBuilder {
	b.data[key] = Int(value)
	return b
}

// SetInt64 is a convenience method for adding int64 values.
func (b *SubtypeBuilder) SetInt64(key string, value int64) *SubtypeBuilder {
	b.data[key] = Int64(value)
	return b
}

// SetUint is a convenience method for adding uint values.
func (b *SubtypeBuilder) SetUint(key string, value uint) *SubtypeBuilder {
	b.data[key] = Uint(value)
	return b
}

// SetUint64 is a convenience method for adding uint64 values.
func (b *SubtypeBuilder) SetUint64(key string, value uint64) *SubtypeBuilder {
	b.data[key] = Uint64(value)
	return b
}

// SetFloat64 is a convenience method for adding float64 values.
func (b *SubtypeBuilder) SetFloat64(key string, value float64) *SubtypeBuilder {
	b.data[key] = Float64(value)
	return b
}

// SetBool is a convenience method for adding bool values.
func (b *SubtypeBuilder) SetBool(key string, value bool) *SubtypeBuilder {
	b.data[key] = Bool(value)
	return b
}

// Build constructs and returns the Subtype.
func (b *SubtypeBuilder) Build() Subtype {
	return Subtype{
		Name: b.name,
		Data: b.data,
	}
}

// MeasurementBuilder provides a fluent API for building Measurement instances.
type MeasurementBuilder struct {
	measurementType Type
	subtypes        []Subtype
}

// NewMeasurement creates a new MeasurementBuilder with the given type.
func NewMeasurement(t Type) *MeasurementBuilder {
	return &MeasurementBuilder{
		measurementType: t,
		subtypes:        make([]Subtype, 0),
	}
}

// WithSubtype adds a subtype to the measurement.
func (b *MeasurementBuilder) WithSubtype(st Subtype) *MeasurementBuilder {
	b.subtypes = append(b.subtypes, st)
	return b
}

// WithSubtypeBuilder adds a subtype using a SubtypeBuilder.
func (b *MeasurementBuilder) WithSubtypeBuilder(builder *SubtypeBuilder) *MeasurementBuilder {
	b.subtypes = append(b.subtypes, builder.Build())
	return b
}

// Build constructs and returns the Measurement.
func (b *MeasurementBuilder) Build() *Measurement {
	return &Measurement{
		Type:     b.measurementType,
		Subtypes: b.subtypes,
	}
}
