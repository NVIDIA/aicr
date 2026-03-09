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

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	testSubtypeCluster = "cluster"
	testSubtypeNode    = "node"
	testSubtypePod     = "pod"
	testVersion        = "1.28.0"
)

func TestType_String(t *testing.T) {
	tests := []struct {
		name string
		mt   Type
		want string
	}{
		{"K8s", TypeK8s, "K8s"},
		{"SMI", TypeGPU, "GPU"},
		{"OS", TypeOS, "OS"},
		{"SystemD", TypeSystemD, "SystemD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mt.String(); got != tt.want {
				t.Errorf("Type.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseType(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   Type
		wantOk bool
	}{
		{"valid k8s", "K8s", TypeK8s, true},
		{"valid os", "OS", TypeOS, true},
		{"invalid", "Invalid", "", false},
		{"empty", "", "", false},
		{"lowercase", "grub", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotOk := ParseType(tt.input)
			if got != tt.want || gotOk != tt.wantOk {
				t.Errorf("ParseType(%q) = (%v, %v), want (%v, %v)", tt.input, got, gotOk, tt.want, tt.wantOk)
			}
		})
	}
}

func TestToReading(t *testing.T) {
	tests := []struct {
		name      string
		value     any
		wantValue any
		wantType  string
	}{
		{"int", 42, 42, "int"},
		{"int64", int64(9223372036854775807), int64(9223372036854775807), "int64"},
		{"uint", uint(42), uint(42), "uint"},
		{"uint64", uint64(18446744073709551615), uint64(18446744073709551615), "uint64"},
		{"float64", 3.14, 3.14, "float64"},
		{"bool true", true, true, "bool"},
		{"bool false", false, false, "bool"},
		{"string", "hello", "hello", "string"},
		{"empty string", "", "", "string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToReading(tt.value)
			if got == nil {
				t.Fatal("ToReading() returned nil")
			}
			gotValue := got.Any()
			if gotValue != tt.wantValue {
				t.Errorf("ToReading(%v).Any() = %v (%T), want %v (%T)", tt.value, gotValue, gotValue, tt.wantValue, tt.wantValue)
			}
		})
	}
}

func TestScalar_JSON(t *testing.T) {
	tests := []struct {
		name    string
		reading Reading
		want    string
	}{
		{"int", Int(42), "42"},
		{"int64", Int64(9223372036854775807), "9223372036854775807"},
		{"uint", Uint(42), "42"},
		{"uint64", Uint64(18446744073709551615), "18446744073709551615"},
		{"float64", Float64(3.14), "3.14"},
		{"bool true", Bool(true), "true"},
		{"bool false", Bool(false), "false"},
		{"string", Str("hello"), `"hello"`},
		{"empty string", Str(""), `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.reading)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if string(data) != tt.want {
				t.Errorf("Marshal() = %v, want %v", string(data), tt.want)
			}
		})
	}
}

func TestScalar_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		reading Reading
		wantVal any
	}{
		{"int", "42", &Scalar[int]{}, 42},
		{"int64", "9223372036854775807", &Scalar[int64]{}, int64(9223372036854775807)},
		{"uint", "42", &Scalar[uint]{}, uint(42)},
		{"uint64", "18446744073709551615", &Scalar[uint64]{}, uint64(18446744073709551615)},
		{"float64", "3.14", &Scalar[float64]{}, float64(3.14)},
		{"bool true", "true", &Scalar[bool]{}, true},
		{"bool false", "false", &Scalar[bool]{}, false},
		{"string", `"hello"`, &Scalar[string]{}, "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tt.json), tt.reading); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			got := tt.reading.Any()
			if got != tt.wantVal {
				t.Errorf("Unmarshal() value = %v (%T), want %v (%T)", got, got, tt.wantVal, tt.wantVal)
			}
		})
	}
}

func TestMeasurement_Validate(t *testing.T) {
	tests := []struct {
		name    string
		m       *Measurement
		wantErr bool
	}{
		{
			name: "valid measurement",
			m: &Measurement{
				Type: TypeK8s,
				Subtypes: []Subtype{
					{
						Name: testSubtypeCluster,
						Data: map[string]Reading{
							"version": Str(testVersion),
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "empty type",
			m: &Measurement{
				Type: "",
				Subtypes: []Subtype{
					{
						Name: testSubtypeCluster,
						Data: map[string]Reading{
							"version": Str(testVersion),
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "nil subtypes",
			m: &Measurement{
				Type:     TypeK8s,
				Subtypes: nil,
			},
			wantErr: true,
		},
		{
			name: "empty subtypes",
			m: &Measurement{
				Type:     TypeK8s,
				Subtypes: []Subtype{},
			},
			wantErr: true,
		},
		{
			name: "subtype with empty data",
			m: &Measurement{
				Type: TypeK8s,
				Subtypes: []Subtype{
					{
						Name: testSubtypeCluster,
						Data: map[string]Reading{},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMeasurement_GetSubtype(t *testing.T) {
	m := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{
				Name: testSubtypeCluster,
				Data: map[string]Reading{
					"version": Str(testVersion),
				},
			},
			{
				Name: testSubtypeNode,
				Data: map[string]Reading{
					"count": Int(3),
				},
			},
		},
	}

	t.Run("existing subtype", func(t *testing.T) {
		st := m.GetSubtype(testSubtypeCluster)
		if st == nil {
			t.Fatal("GetSubtype() returned nil")
			return
		}
		if st.Name != testSubtypeCluster {
			t.Errorf("GetSubtype() name = %v, want cluster", st.Name)
		}
	})

	t.Run("non-existing subtype", func(t *testing.T) {
		st := m.GetSubtype("missing")
		if st != nil {
			t.Errorf("GetSubtype() = %v, want nil", st)
		}
	})
}

func Test_hasSubtype(t *testing.T) {
	m := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{Name: testSubtypeCluster, Data: map[string]Reading{"version": Str(testVersion)}},
			{Name: testSubtypeNode, Data: map[string]Reading{"count": Int(3)}},
		},
	}

	tests := []struct {
		name string
		st   string
		want bool
	}{
		{"existing cluster", testSubtypeCluster, true},
		{"existing node", testSubtypeNode, true},
		{"non-existing", "missing", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.hasSubtype(tt.st); got != tt.want {
				t.Errorf("HasSubtype(%q) = %v, want %v", tt.st, got, tt.want)
			}
		})
	}
}

func Test_subtypeNames(t *testing.T) {
	m := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{Name: testSubtypeCluster, Data: map[string]Reading{"version": Str(testVersion)}},
			{Name: testSubtypeNode, Data: map[string]Reading{"count": Int(3)}},
			{Name: testSubtypePod, Data: map[string]Reading{"ready": Bool(true)}},
		},
	}

	names := m.subtypeNames()
	if len(names) != 3 {
		t.Fatalf("SubtypeNames() returned %d names, want 3", len(names))
	}

	expectedNames := []string{testSubtypeCluster, testSubtypeNode, testSubtypePod}
	for i, expected := range expectedNames {
		if names[i] != expected {
			t.Errorf("SubtypeNames()[%d] = %v, want %v", i, names[i], expected)
		}
	}
}

func TestSubtype_Validate(t *testing.T) {
	tests := []struct {
		name    string
		st      *Subtype
		wantErr bool
	}{
		{
			name: "valid subtype",
			st: &Subtype{
				Name: "test",
				Data: map[string]Reading{"key": Str("value")},
			},
			wantErr: false,
		},
		{
			name: "empty data",
			st: &Subtype{
				Name: "test",
				Data: map[string]Reading{},
			},
			wantErr: true,
		},
		{
			name: "nil data",
			st: &Subtype{
				Name: "test",
				Data: nil,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.st.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSubtype_Has(t *testing.T) {
	st := &Subtype{
		Name: "test",
		Data: map[string]Reading{
			"version": Str(testVersion),
			"nodes":   Int(3),
		},
	}

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"existing key version", "version", true},
		{"existing key nodes", "nodes", true},
		{"non-existing key", "missing", false},
		{"empty key", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := st.Has(tt.key); got != tt.want {
				t.Errorf("Has(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestSubtype_Get(t *testing.T) {
	st := &Subtype{
		Name: "test",
		Data: map[string]Reading{
			"version": Str(testVersion),
		},
	}

	t.Run("existing key", func(t *testing.T) {
		got := st.Get("version")
		if got == nil {
			t.Fatal("Get() returned nil")
		}
		if v, ok := got.Any().(string); !ok || v != testVersion {
			t.Errorf("Get() = %v, want 1.28.0", got.Any())
		}
	})

	t.Run("non-existing key", func(t *testing.T) {
		got := st.Get("missing")
		if got != nil {
			t.Errorf("Get() = %v, want nil", got)
		}
	})
}

func Test_keys(t *testing.T) {
	st := &Subtype{
		Name: "test",
		Data: map[string]Reading{
			"version": Str(testVersion),
			"nodes":   Int(3),
			"ready":   Bool(true),
		},
	}

	keys := st.keys()
	if len(keys) != 3 {
		t.Fatalf("Keys() returned %d keys, want 3", len(keys))
	}

	keyMap := make(map[string]bool)
	for _, k := range keys {
		keyMap[k] = true
	}

	expectedKeys := []string{"version", "nodes", "ready"}
	for _, k := range expectedKeys {
		if !keyMap[k] {
			t.Errorf("Keys() missing key %q", k)
		}
	}
}

func TestSubtype_GetString(t *testing.T) {
	st := &Subtype{
		Name: "test",
		Data: map[string]Reading{
			"version": Str(testVersion),
			"count":   Int(3),
		},
	}

	tests := []struct {
		name    string
		key     string
		want    string
		wantErr bool
	}{
		{"valid string", "version", testVersion, false},
		{"wrong type", "nodes", "", true},
		{"missing key", "missing", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := st.GetString(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetString(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetString(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestSubtype_GetInt64(t *testing.T) {
	st := &Subtype{
		Name: "test",
		Data: map[string]Reading{
			"int_value":   Int(42),
			"int64_value": Int64(9223372036854775807),
			"version":     Str(testVersion),
		},
	}

	tests := []struct {
		name    string
		key     string
		want    int64
		wantErr bool
	}{
		{"int value", "int_value", 42, false},
		{"int64 value", "int64_value", 9223372036854775807, false},
		{"wrong type", "version", 0, true},
		{"missing key", "missing", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := st.GetInt64(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetInt64(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetInt64(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func Test_getUint64(t *testing.T) {
	st := &Subtype{
		Name: "test",
		Data: map[string]Reading{
			"uint_value":   Uint(42),
			"uint64_value": Uint64(18446744073709551615),
			"version":      Str("1.0.0"),
		},
	}

	tests := []struct {
		name    string
		key     string
		want    uint64
		wantErr bool
	}{
		{"uint value", "uint_value", 42, false},
		{"uint64 value", "uint64_value", 18446744073709551615, false},
		{"wrong type", "version", 0, true},
		{"missing key", "missing", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := st.getUint64(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetUint64(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetUint64(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func Test_getFloat64(t *testing.T) {
	st := &Subtype{
		Name: "test",
		Data: map[string]Reading{
			"temperature": Float64(82.5),
			"version":     Str("1.0.0"),
		},
	}

	tests := []struct {
		name    string
		key     string
		want    float64
		wantErr bool
	}{
		{"valid float64", "temperature", 82.5, false},
		{"wrong type", "version", 0, true},
		{"missing key", "missing", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := st.getFloat64(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetFloat64(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetFloat64(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func Test_getBool(t *testing.T) {
	st := &Subtype{
		Name: "test",
		Data: map[string]Reading{
			"ready":   Bool(true),
			"stopped": Bool(false),
			"version": Str("1.0.0"),
		},
	}

	tests := []struct {
		name    string
		key     string
		want    bool
		wantErr bool
	}{
		{"true value", "ready", true, false},
		{"false value", "stopped", false, false},
		{"wrong type", "version", false, true},
		{"missing key", "missing", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := st.getBool(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetBool(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetBool(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestMeasurement_JSON(t *testing.T) {
	original := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{
				Name: testSubtypeCluster,
				Data: map[string]Reading{
					"version": Str(testVersion),
					"nodes":   Int(3),
					"ready":   Bool(true),
					"cpu":     Float64(85.5),
				},
			},
		},
	}

	// Marshal
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	// Verify JSON structure
	var jsonData map[string]any
	if err := json.Unmarshal(data, &jsonData); err != nil {
		t.Fatalf("Unmarshal to map error = %v", err)
	}

	// Verify basic fields in JSON
	if jsonData["type"] != string(original.Type) {
		t.Errorf("JSON type = %v, want %v", jsonData["type"], original.Type)
	}

	// Verify subtypes field exists
	subtypes, ok := jsonData["subtypes"].([]any)
	if !ok {
		t.Fatalf("JSON subtypes is not an array")
	}
	if len(subtypes) != len(original.Subtypes) {
		t.Errorf("JSON subtypes length = %d, want %d", len(subtypes), len(original.Subtypes))
	}

	// Verify first subtype
	if len(subtypes) > 0 {
		st, ok := subtypes[0].(map[string]any)
		if !ok {
			t.Fatalf("JSON subtype[0] is not a map")
		}
		if st["subtype"] != testSubtypeCluster {
			t.Errorf("JSON subtype[0].subtype = %v, want cluster", st["subtype"])
		}

		dataMap, ok := st["data"].(map[string]any)
		if !ok {
			t.Fatalf("JSON subtype[0].data is not a map")
		}
		if dataMap["version"] != testVersion {
			t.Errorf("JSON subtype[0].data.version = %v, want 1.28.0", dataMap["version"])
		}
	}
}

func TestConvenienceConstructors(t *testing.T) {
	tests := []struct {
		name    string
		reading Reading
		wantVal any
	}{
		{"Int", Int(42), 42},
		{"Int64", Int64(9223372036854775807), int64(9223372036854775807)},
		{"Uint", Uint(42), uint(42)},
		{"Uint64", Uint64(18446744073709551615), uint64(18446744073709551615)},
		{"Float64", Float64(3.14159), float64(3.14159)},
		{"Bool true", Bool(true), true},
		{"Bool false", Bool(false), false},
		{"Str", Str("hello world"), "hello world"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.reading.Any()
			if got != tt.wantVal {
				t.Errorf("Any() = %v (%T), want %v (%T)", got, got, tt.wantVal, tt.wantVal)
			}

			// Verify it implements Reading interface
			tt.reading.isReading()

			// Verify it can be marshaled
			_, err := json.Marshal(tt.reading)
			if err != nil {
				t.Errorf("Marshal() error = %v", err)
			}
		})
	}
}

func TestMeasurement_YAML(t *testing.T) {
	original := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{
				Name: testSubtypeCluster,
				Data: map[string]Reading{
					"version": Str(testVersion),
					"nodes":   Int(3),
					"ready":   Bool(true),
				},
			},
		},
	}

	// Marshal to YAML
	yamlData, err := yaml.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	t.Logf("YAML output:\n%s", string(yamlData))

	// Unmarshal back
	var restored Measurement
	if err := yaml.Unmarshal(yamlData, &restored); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	// Verify type
	if restored.Type != original.Type {
		t.Errorf("Type = %v, want %v", restored.Type, original.Type)
	}

	// Verify subtypes length
	if len(restored.Subtypes) != len(original.Subtypes) {
		t.Errorf("Subtypes length = %d, want %d", len(restored.Subtypes), len(original.Subtypes))
	}

	// Verify values are not wrapped in "v" structure
	if len(restored.Subtypes) > 0 {
		// Check string value
		version, err := restored.Subtypes[0].GetString("version")
		if err != nil {
			t.Errorf("Failed to get version string: %v", err)
		} else if version != testVersion {
			t.Errorf("version = %v, want %s", version, testVersion)
		}

		// Check int value
		nodes, err := restored.Subtypes[0].GetInt64("nodes")
		if err != nil {
			t.Errorf("Failed to get nodes int: %v", err)
		} else if nodes != 3 {
			t.Errorf("nodes = %v, want 3", nodes)
		}

		// Check bool value
		ready, err := restored.Subtypes[0].getBool("ready")
		if err != nil {
			t.Errorf("Failed to get ready bool: %v", err)
		} else if !ready {
			t.Errorf("ready = %v, want true", ready)
		}

		// Verify data map doesn't contain nested "v" structures
		for key, value := range restored.Subtypes[0].Data {
			anyVal := value.Any()
			t.Logf("Key: %s, Value: %v, Type: %T", key, anyVal, anyVal)
		}
	}
}

func Test_getOrCreateSubtype(t *testing.T) {
	t.Run("get existing subtype", func(t *testing.T) {
		m := &Measurement{
			Type: TypeK8s,
			Subtypes: []Subtype{
				{Name: "cluster", Data: map[string]Reading{"version": Str("1.28.0")}},
			},
		}

		st := m.getOrCreateSubtype("cluster")
		if st == nil {
			t.Fatal("GetOrCreateSubtype() returned nil")
			return
		}
		if st.Name != "cluster" {
			t.Errorf("Name = %v, want cluster", st.Name)
		}
		if len(m.Subtypes) != 1 {
			t.Errorf("Subtypes length = %d, want 1", len(m.Subtypes))
		}
	})

	t.Run("create new subtype", func(t *testing.T) {
		m := &Measurement{
			Type:     TypeK8s,
			Subtypes: []Subtype{},
		}

		st := m.getOrCreateSubtype("new_subtype")
		if st == nil {
			t.Fatal("GetOrCreateSubtype() returned nil")
			return
		}
		if st.Name != "new_subtype" {
			t.Errorf("Name = %v, want new_subtype", st.Name)
		}
		if len(m.Subtypes) != 1 {
			t.Errorf("Subtypes length = %d, want 1", len(m.Subtypes))
		}
		if st.Data == nil {
			t.Error("Data map is nil")
		}
	})

	t.Run("modify returned subtype", func(t *testing.T) {
		m := &Measurement{
			Type:     TypeK8s,
			Subtypes: []Subtype{},
		}

		st := m.getOrCreateSubtype("test")
		st.Data["key"] = Str("value")

		// Verify the change is reflected in the measurement
		retrieved := m.GetSubtype("test")
		if retrieved == nil {
			t.Fatal("GetSubtype() returned nil")
		}
		val, err := retrieved.GetString("key")
		if err != nil || val != "value" {
			t.Errorf("GetString(key) = %v, %v; want value, nil", val, err)
		}
	})
}

func TestMeasurement_Merge(t *testing.T) {
	t.Run("merge new subtypes", func(t *testing.T) {
		m1 := &Measurement{
			Type: TypeK8s,
			Subtypes: []Subtype{
				{Name: "cluster", Data: map[string]Reading{"version": Str("1.28.0")}},
			},
		}

		m2 := &Measurement{
			Type: TypeK8s,
			Subtypes: []Subtype{
				{Name: "pod", Data: map[string]Reading{"count": Int(100)}},
			},
		}

		err := m1.Merge(m2)
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}

		if len(m1.Subtypes) != 2 {
			t.Errorf("Subtypes length = %d, want 2", len(m1.Subtypes))
		}

		if !m1.hasSubtype("cluster") || !m1.hasSubtype("pod") {
			t.Error("Expected both cluster and pod subtypes")
		}
	})

	t.Run("merge existing subtype with new data", func(t *testing.T) {
		m1 := &Measurement{
			Type: TypeK8s,
			Subtypes: []Subtype{
				{Name: "cluster", Data: map[string]Reading{
					"version": Str("1.28.0"),
					"nodes":   Int(3),
				}},
			},
		}

		m2 := &Measurement{
			Type: TypeK8s,
			Subtypes: []Subtype{
				{Name: "cluster", Data: map[string]Reading{
					"pods":  Int(100),
					"nodes": Int(5), // Override
				}},
			},
		}

		err := m1.Merge(m2)
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}

		cluster := m1.GetSubtype("cluster")
		if cluster == nil {
			t.Fatal("GetSubtype(cluster) returned nil")
		}

		// Check that version is preserved
		version, err := cluster.GetString("version")
		if err != nil || version != "1.28.0" {
			t.Errorf("version = %v, %v; want 1.28.0, nil", version, err)
		}

		// Check that nodes was overridden
		nodes, err := cluster.GetInt64("nodes")
		if err != nil || nodes != 5 {
			t.Errorf("nodes = %v, %v; want 5, nil", nodes, err)
		}

		// Check that pods was added
		pods, err := cluster.GetInt64("pods")
		if err != nil || pods != 100 {
			t.Errorf("pods = %v, %v; want 100, nil", pods, err)
		}
	})

	t.Run("different types error", func(t *testing.T) {
		m1 := &Measurement{Type: TypeK8s, Subtypes: []Subtype{}}
		m2 := &Measurement{Type: TypeGPU, Subtypes: []Subtype{}}

		err := m1.Merge(m2)
		if err == nil {
			t.Error("Merge() expected error for different types")
		}
	})

	t.Run("empty measurement", func(t *testing.T) {
		m1 := &Measurement{Type: TypeK8s, Subtypes: []Subtype{}}
		m2 := &Measurement{Type: TypeK8s, Subtypes: []Subtype{}}

		err := m1.Merge(m2)
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}

		if len(m1.Subtypes) != 0 {
			t.Errorf("Subtypes length = %d, want 0", len(m1.Subtypes))
		}
	})
}

func Test_toReadingWithType(t *testing.T) {
	tests := []struct {
		name       string
		value      any
		wantValue  any
		wantLossy  bool
		wantLossed bool
	}{
		{"int", 42, 42, false, true},
		{"int64", int64(9223372036854775807), int64(9223372036854775807), false, true},
		{"uint", uint(42), uint(42), false, true},
		{"uint64", uint64(18446744073709551615), uint64(18446744073709551615), false, true},
		{"float64", 3.14, 3.14, false, true},
		{"bool true", true, true, false, true},
		{"bool false", false, false, false, true},
		{"string", "hello", "hello", false, true},
		{"struct", struct{ X int }{X: 42}, "{42}", true, false},
		{"slice", []int{1, 2, 3}, "[1 2 3]", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, exactType := toReadingWithType(tt.value)
			if got == nil {
				t.Fatal("toReadingWithType() returned nil")
			}

			gotValue := got.Any()
			if tt.wantLossy {
				// For lossy conversions, just check it's a string
				if _, ok := gotValue.(string); !ok {
					t.Errorf("toReadingWithType(%v) returned %T, want string", tt.value, gotValue)
				}
			} else {
				if gotValue != tt.wantValue {
					t.Errorf("toReadingWithType(%v) = %v, want %v", tt.value, gotValue, tt.wantValue)
				}
			}

			if exactType != tt.wantLossed {
				t.Errorf("toReadingWithType(%v) exactType = %v, want %v", tt.value, exactType, tt.wantLossed)
			}
		})
	}
}

func TestConstants(t *testing.T) {
	// Just verify constants are defined and unique
	constants := []string{
		KeyVersion, keyNodes, keyPods, keyNamespace, keyClusterName, keyReady,
		KeyGPUDriver, KeyGPUModel, KeyGPUCount, keyGPUMemory, keyGPUTemp, keyGPUPower, keyGPUUUID,
		keyOSName, keyOSVersion, keyKernel, keyArch, keyHostname,
		keyServiceName, keyServiceState, keyServiceStatus, keyEnabled, keyActive,
	}

	seen := make(map[string]bool)
	for _, c := range constants {
		if c == "" {
			t.Error("Found empty constant")
		}
		if seen[c] {
			t.Errorf("Duplicate constant: %s", c)
		}
		seen[c] = true
	}

	if len(seen) != len(constants) {
		t.Errorf("Expected %d unique constants, got %d", len(constants), len(seen))
	}
}

func TestScalar_String(t *testing.T) {
	tests := []struct {
		name    string
		reading Reading
		want    string
	}{
		{"int", Int(42), "42"},
		{"int negative", Int(-42), "-42"},
		{"int64", Int64(9223372036854775807), "9223372036854775807"},
		{"uint", Uint(42), "42"},
		{"uint64", Uint64(18446744073709551615), "18446744073709551615"},
		{"float64", Float64(3.14), "3.14"},
		{"float64 whole", Float64(42.0), "42"},
		{"bool true", Bool(true), "true"},
		{"bool false", Bool(false), "false"},
		{"string", Str("hello"), "hello"},
		{"string empty", Str(""), ""},
		{"string with spaces", Str("hello world"), "hello world"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use type assertion to get the String() method
			if s, ok := tt.reading.(interface{ String() string }); ok {
				got := s.String()
				if got != tt.want {
					t.Errorf("String() = %q, want %q", got, tt.want)
				}
			} else {
				t.Error("reading does not implement String()")
			}
		})
	}
}

func TestScalar_UnmarshalYAML(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		scalar  Reading
		wantVal any
	}{
		{"int", "42", &Scalar[int]{}, 42},
		{"int negative", "-42", &Scalar[int]{}, -42},
		{"int64", "9223372036854775807", &Scalar[int64]{}, int64(9223372036854775807)},
		{"uint", "42", &Scalar[uint]{}, uint(42)},
		{"uint64", "18446744073709551615", &Scalar[uint64]{}, uint64(18446744073709551615)},
		{"float64", "3.14", &Scalar[float64]{}, float64(3.14)},
		{"bool true", "true", &Scalar[bool]{}, true},
		{"bool false", "false", &Scalar[bool]{}, false},
		{"string", "hello", &Scalar[string]{}, "hello"},
		{"string quoted", `"hello world"`, &Scalar[string]{}, "hello world"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := yaml.Unmarshal([]byte(tt.yaml), tt.scalar)
			if err != nil {
				t.Fatalf("UnmarshalYAML() error = %v", err)
			}
			got := tt.scalar.Any()
			if got != tt.wantVal {
				t.Errorf("UnmarshalYAML() value = %v (%T), want %v (%T)", got, got, tt.wantVal, tt.wantVal)
			}
		})
	}
}

func TestScalar_UnmarshalYAML_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		yaml   string
		scalar Reading
	}{
		{"int with string", "not_a_number", &Scalar[int]{}},
		{"bool with string", "not_a_bool", &Scalar[bool]{}},
		{"float with string", "not_a_float", &Scalar[float64]{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := yaml.Unmarshal([]byte(tt.yaml), tt.scalar)
			if err == nil {
				t.Error("expected error for invalid YAML value")
			}
		})
	}
}

func TestScalar_isReading(t *testing.T) {
	// This test verifies that all scalar types implement the isReading interface method
	readings := []Reading{
		Int(42),
		Int64(42),
		Uint(42),
		Uint64(42),
		Float64(3.14),
		Bool(true),
		Str("test"),
	}

	for i, r := range readings {
		// The isReading method should exist and be callable
		r.isReading()
		t.Logf("Reading %d implements isReading()", i)
	}
}

func TestScalar_MarshalYAML(t *testing.T) {
	tests := []struct {
		name    string
		reading Reading
		want    string
	}{
		{"int", Int(42), "42\n"},
		{"int64", Int64(100), "100\n"},
		{"uint", Uint(42), "42\n"},
		{"uint64", Uint64(100), "100\n"},
		{"float64", Float64(3.14), "3.14\n"},
		{"bool true", Bool(true), "true\n"},
		{"bool false", Bool(false), "false\n"},
		{"string", Str("hello"), "hello\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := yaml.Marshal(tt.reading)
			if err != nil {
				t.Fatalf("MarshalYAML() error = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("MarshalYAML() = %q, want %q", string(got), tt.want)
			}
		})
	}
}

func TestSubtype_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantName string
		wantData map[string]any
		wantErr  bool
	}{
		{
			name:     "string values",
			input:    `{"subtype":"server","data":{"version":"v1.32.0"}}`,
			wantName: "server",
			wantData: map[string]any{"version": "v1.32.0"},
		},
		{
			name:     "numeric values",
			input:    `{"subtype":"gpu","data":{"count":4}}`,
			wantName: "gpu",
			wantData: map[string]any{"count": float64(4)},
		},
		{
			name:     "boolean values",
			input:    `{"subtype":"feature","data":{"enabled":true}}`,
			wantName: "feature",
			wantData: map[string]any{"enabled": true},
		},
		{
			name:     "with context",
			input:    `{"subtype":"node","data":{"hostname":"node-1"},"context":{"zone":"us-east"}}`,
			wantName: "node",
			wantData: map[string]any{"hostname": "node-1"},
		},
		{
			name:    "invalid json",
			input:   `{invalid`,
			wantErr: true,
		},
		{
			name:     "empty data",
			input:    `{"subtype":"empty","data":{}}`,
			wantName: "empty",
			wantData: map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var st Subtype
			err := json.Unmarshal([]byte(tt.input), &st)
			if (err != nil) != tt.wantErr {
				t.Fatalf("UnmarshalJSON() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if st.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", st.Name, tt.wantName)
			}
			for k, wantV := range tt.wantData {
				gotReading, ok := st.Data[k]
				if !ok {
					t.Errorf("missing key %q in data", k)
					continue
				}
				if gotReading.Any() != wantV {
					t.Errorf("Data[%q] = %v, want %v", k, gotReading.Any(), wantV)
				}
			}
		})
	}
}
