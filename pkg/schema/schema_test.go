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

package schema

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// The generated schemas become a committed baseline that a merge gate diffs, so
// a reflector that quietly describes the wrong shape does not fail — it freezes
// the wrong contract and reports success forever after. Every branch below is
// therefore checked against what encoding/json actually does, not against what
// the reflector intends to do.

type embedded struct {
	Kind       string            `json:"kind,omitempty"`
	APIVersion string            `json:"apiVersion,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type named struct {
	Inner string `json:"inner"`
}

// customJSON controls its own wire form, which reflection over its fields
// cannot predict.
type customJSON struct {
	Hidden string
}

// MarshalJSON satisfies json.Marshaler. The error is always nil: the signature
// is the whole point, since the reflector detects the interface rather than
// calling it. unparam flags the constant nil, which is inherent to the
// interface and not a real error path.
//
//nolint:unparam
func (c customJSON) MarshalJSON() ([]byte, error) { return []byte(`"custom"`), nil }

// customBytes is a byte slice whose marshaler emits an object, not base64 --
// the shape the ordering fix exists to protect.
type customBytes []byte

// MarshalJSON emits an object rather than base64, which is what makes this type
// useful here. The error is always nil for the same reason as customJSON above.
//
//nolint:unparam
func (c customBytes) MarshalJSON() ([]byte, error) { return []byte(`{"a":1}`), nil }

type sample struct {
	embedded `json:",inline"`

	Required   string   `json:"required"`
	Optional   string   `json:"optional,omitempty"`
	Pointer    *string  `json:"pointer"`
	Slice      []string `json:"slice"`
	Nested     named    `json:"nested"`
	NestedList []named  `json:"nestedList"`
	Count      int      `json:"count"`
	Flag       bool     `json:"flag"`
	Open       any      `json:"open"`
	Skipped    string   `json:"-"`
	unexported string   //nolint:unused // presence is the point: it must not appear
	Untagged   string
}

// TestGenerateMatchesEncodingJSON is the anchor test: the property set the
// reflector produces must equal the key set encoding/json emits for a fully
// populated value.
//
// Asserting against a hand-written expectation would only prove the reflector
// agrees with my belief about encoding/json. Marshaling a real value proves it
// agrees with the encoder, which is what integrators actually receive.
func TestGenerateMatchesEncodingJSON(t *testing.T) {
	pointer := "set"
	value := sample{
		embedded:   embedded{Kind: "K", APIVersion: "v1", Metadata: map[string]string{"a": "b"}},
		Required:   "r",
		Optional:   "o",
		Pointer:    &pointer,
		Slice:      []string{"s"},
		Nested:     named{Inner: "i"},
		NestedList: []named{{Inner: "i"}},
		Count:      1,
		Flag:       true,
		Open:       "anything",
		Skipped:    "never",
		Untagged:   "u",
	}

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}
	var encoded map[string]any
	if unmarshalErr := json.Unmarshal(raw, &encoded); unmarshalErr != nil {
		t.Fatalf("unmarshal sample: %v", unmarshalErr)
	}

	got, err := Generate(reflect.TypeOf(sample{}), Options{Title: "Sample"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if diff := keyDiff(encoded, got.Properties); diff != "" {
		t.Errorf("schema properties do not match what encoding/json emits:\n%s", diff)
	}
}

// TestGenerateMatchesEncodingJSONForZeroValue is the nil-side companion to the
// anchor test.
//
// The populated-value comparison cannot see nullability: every pointer and
// slice is set, so nothing is written as null. A zero value is where the
// encoder emits "field":null, and where a schema describing a non-null type
// rejects the encoder's own output.
func TestGenerateMatchesEncodingJSONForZeroValue(t *testing.T) {
	raw, err := json.Marshal(sample{})
	if err != nil {
		t.Fatalf("marshal zero sample: %v", err)
	}
	var encoded map[string]any
	if unmarshalErr := json.Unmarshal(raw, &encoded); unmarshalErr != nil {
		t.Fatalf("unmarshal zero sample: %v", unmarshalErr)
	}

	got, err := Generate(reflect.TypeOf(sample{}), Options{Title: "Sample"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	for key, value := range encoded {
		node, declared := got.Properties[key]
		if !declared {
			t.Errorf("encoder emits %q on a zero value but the schema omits it", key)
			continue
		}
		if value == nil && node.Type != "" && !node.Nullable {
			t.Errorf("encoder writes %q as null on a zero value, but the schema "+
				"declares type %q without null; the schema rejects the encoder's "+
				"own output", key, node.Type)
		}
	}
}

// TestGenerateEmbeddedIsFlattened pins the case that would silently break every
// snapshot: Snapshot embeds header.Header, so kind and apiVersion must appear at
// the top level rather than under a nested object.
func TestGenerateEmbeddedIsFlattened(t *testing.T) {
	got, err := Generate(reflect.TypeOf(sample{}), Options{Title: "Sample"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	for _, field := range []string{"kind", "apiVersion", "metadata"} {
		if _, ok := got.Properties[field]; !ok {
			t.Errorf("embedded field %q is missing; the embedded struct was not "+
				"flattened and no real artifact would validate against this schema",
				field)
		}
	}
	if _, nested := got.Properties["embedded"]; nested {
		t.Error("embedded struct appears as its own property; encoding/json " +
			"flattens it")
	}
}

// TestGenerateRequiredMatchesAlwaysEmittedFields asserts required contains
// exactly the fields the encoder always writes.
//
// Getting this backwards is the subtle failure: marking an omitempty field
// required makes the schema reject documents the server legitimately produces.
func TestGenerateRequiredMatchesAlwaysEmittedFields(t *testing.T) {
	got, err := Generate(reflect.TypeOf(sample{}), Options{Title: "Sample"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// "pointer" belongs here: without omitempty the encoder writes
	// "pointer":null rather than omitting the key. An earlier version of this
	// test asserted the opposite, which is how the schema came to describe a
	// non-null string for a field the encoder can emit as null.
	want := []string{"count", "flag", "nested", "nestedList", "open", "pointer",
		"required", "slice", "Untagged"}
	sort.Strings(want)

	gotRequired := append([]string(nil), got.Required...)
	sort.Strings(gotRequired)

	if !reflect.DeepEqual(gotRequired, want) {
		t.Errorf("required = %v, want %v", gotRequired, want)
	}

	for _, notRequired := range []string{"optional", "kind", "apiVersion", "metadata"} {
		for _, req := range got.Required {
			if req == notRequired {
				t.Errorf("%q is required, but the encoder omits it (omitempty or "+
					"pointer); the schema would reject valid documents", notRequired)
			}
		}
	}
}

// TestGenerateTypeMapping covers the scalar and container branches.
func TestGenerateTypeMapping(t *testing.T) {
	got, err := Generate(reflect.TypeOf(sample{}), Options{Title: "Sample"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	tests := map[string]string{
		"required":   typeString,
		"count":      typeInteger,
		"flag":       typeBoolean,
		"slice":      typeArray,
		"nested":     typeObject,
		"nestedList": typeArray,
		"metadata":   typeObject,
		"open":       "", // an open value carries no type constraint
	}
	for field, wantType := range tests {
		node, ok := got.Properties[field]
		if !ok {
			t.Errorf("property %q missing", field)
			continue
		}
		if node.Type != wantType {
			t.Errorf("%q type = %q, want %q", field, node.Type, wantType)
		}
	}

	if items := got.Properties["nestedList"].Items; items == nil || items.Type != typeObject {
		t.Error("nestedList items should describe the element struct")
	}
	if names := got.Properties["metadata"].PropertyNames; names == nil || names.Type != typeString {
		t.Error("map should constrain its key type")
	}
}

// TestGenerateDereferencesPointers asserts a pointer field is described by the
// type it points at, not skipped.
func TestGenerateDereferencesPointers(t *testing.T) {
	got, err := Generate(reflect.TypeOf(sample{}), Options{Title: "Sample"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	node, ok := got.Properties["pointer"]
	if !ok {
		t.Fatal("pointer property missing")
	}
	if node.Type != typeString {
		t.Errorf("pointer type = %q, want %q", node.Type, typeString)
	}
}

// TestGenerateAppliesEnums covers the enum injection used for criteria values.
func TestGenerateAppliesEnums(t *testing.T) {
	got, err := Generate(reflect.TypeOf(sample{}), Options{
		Title: "Sample",
		Enums: map[string][]string{"schema.sample": {"ignored"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Enum != nil {
		t.Error("an enum keyed to a struct type must not be applied; only string " +
			"types carry enums")
	}

	type colour string
	type holder struct {
		Colour colour `json:"colour"`
	}
	withEnum, err := Generate(reflect.TypeOf(holder{}), Options{
		Title: "Holder",
		Enums: map[string][]string{"schema.colour": {"red", "blue"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	node := withEnum.Properties["colour"]
	if !reflect.DeepEqual(node.Enum, []string{"blue", "red"}) {
		t.Errorf("enum = %v, want sorted [blue red]", node.Enum)
	}
}

// TestGenerateFailsClosed asserts unsupported shapes error instead of producing
// a plausible-looking schema.
//
// This is the property that makes the committed baseline trustworthy: a type the
// reflector cannot model must stop the build, not silently narrow the contract.
func TestGenerateFailsClosed(t *testing.T) {
	type withChannel struct {
		Ch chan int `json:"ch"`
	}
	type withFunc struct {
		Fn func() `json:"fn"`
	}
	type withIntMap struct {
		M map[int]string `json:"m"`
	}
	type recursive struct {
		Next *recursive `json:"next"`
	}
	type withMethodInterface struct {
		E error `json:"e"`
	}

	type withTime struct {
		At time.Time `json:"at"`
	}
	type withRawMessage struct {
		Raw json.RawMessage `json:"raw"`
	}
	type withCustomBytes struct {
		Custom customBytes `json:"custom"`
	}
	type withMarshaler struct {
		Custom customJSON `json:"custom"`
	}

	tests := []struct {
		name    string
		typ     reflect.Type
		wantErr string
	}{
		// time.Time has no exported fields, so reflecting over it produced
		// {"type":"object"} while the encoder writes an RFC 3339 string --
		// a schema that rejects every document it describes.
		{"time.Time", reflect.TypeOf(withTime{}), "defines its own JSON encoding"},
		// Byte slices whose marshaler emits arbitrary JSON. The base64 branch
		// runs after the marshaler check for exactly these: classifying them
		// as byte slices first described `[1,2]` and `{"a":1}` as base64
		// strings.
		{"json.RawMessage", reflect.TypeOf(withRawMessage{}), "defines its own JSON encoding"},
		{"named byte slice with marshaler", reflect.TypeOf(withCustomBytes{}), "defines its own JSON encoding"},
		{"custom marshaler", reflect.TypeOf(withMarshaler{}), "defines its own JSON encoding"},
		{"channel", reflect.TypeOf(withChannel{}), "unsupported kind"},
		{"func", reflect.TypeOf(withFunc{}), "unsupported kind"},
		{"non-string map key", reflect.TypeOf(withIntMap{}), "non-string key"},
		{"recursive", reflect.TypeOf(recursive{}), "recursive type"},
		{"interface with methods", reflect.TypeOf(withMethodInterface{}), "non-empty interface"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Generate(tt.typ, Options{Title: tt.name})
			if err == nil {
				t.Fatalf("expected an error, got a schema: %+v", got)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
			if got != nil {
				t.Error("a failed generation must return no schema; a partial " +
					"document would be committed as the contract")
			}
		})
	}
}

// TestGenerateSetsDocumentHeader covers the draft, id and title wiring.
func TestGenerateSetsDocumentHeader(t *testing.T) {
	got, err := Generate(reflect.TypeOf(named{}), Options{
		Title:       "Named",
		Description: "a description",
		ID:          "https://example.invalid/named.json",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Schema != Draft {
		t.Errorf("$schema = %q, want %q", got.Schema, Draft)
	}
	if got.ID != "https://example.invalid/named.json" {
		t.Errorf("$id = %q", got.ID)
	}
	if got.Title != "Named" || got.Description != "a description" {
		t.Errorf("title/description = %q/%q", got.Title, got.Description)
	}
}

// keyDiff compares the encoder's key set against the schema's property set.
func keyDiff(encoded map[string]any, properties map[string]*Schema) string {
	var missing, extra []string
	for key := range encoded {
		if _, ok := properties[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range properties {
		if _, ok := encoded[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	var report strings.Builder
	if len(missing) > 0 {
		report.WriteString("  encoder emits but schema omits: " +
			strings.Join(missing, ", ") + "\n")
	}
	if len(extra) > 0 {
		report.WriteString("  schema declares but encoder never emits: " +
			strings.Join(extra, ", ") + "\n")
	}
	return report.String()
}

// TestGenerateDescribesByteSlicesAsBase64 pins the one custom encoding that is
// fully defined, so it is described rather than rejected.
//
// encoding/json writes a []byte as a base64 string. Describing it as an array
// of integers -- which reflection over its element type produces -- would
// publish a schema no real document satisfies.
func TestGenerateDescribesByteSlicesAsBase64(t *testing.T) {
	type withBytes struct {
		Data []byte `json:"data"`
	}

	got, err := Generate(reflect.TypeOf(withBytes{}), Options{Title: "WithBytes"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	node := got.Properties["data"]
	if node.Type != typeString || node.ContentEncoding != "base64" {
		t.Errorf("[]byte described as type=%q contentEncoding=%q, want string/base64",
			node.Type, node.ContentEncoding)
	}

	// Confirm against the encoder rather than against the expectation above.
	encoded, err := json.Marshal(withBytes{Data: []byte("hi")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"data":"`) {
		t.Errorf("encoder wrote %s; the schema assumes a JSON string", encoded)
	}
}

// TestGenerateAuthoredArtifactsDeclareNothingRequired covers the input/output
// distinction.
//
// A field without omitempty is always written by the encoder, but that says
// nothing about what a human must put in a hand-authored overlay.
// ComponentRef.Source is exactly that case: always emitted, set by only 17 of
// the 110 committed overlays.
func TestGenerateAuthoredArtifactsDeclareNothingRequired(t *testing.T) {
	emitted, err := Generate(reflect.TypeOf(named{}), Options{Title: "Named"})
	if err != nil {
		t.Fatalf("Generate emitted: %v", err)
	}
	if len(emitted.Required) == 0 {
		t.Error("an emitted artifact should declare required fields; the encoder " +
			"always writes them")
	}

	authored, err := Generate(reflect.TypeOf(named{}), Options{Title: "Named", Authored: true})
	if err != nil {
		t.Fatalf("Generate authored: %v", err)
	}
	if len(authored.Required) != 0 {
		t.Errorf("an authored artifact declared %v required; a schema that "+
			"demands fields real overlays omit rejects the project's own catalog",
			authored.Required)
	}
}

// TestGenerateDescribesDefinedByteSlices covers a named type whose underlying
// type is []byte.
//
// encoding/json base64-encodes it exactly like []byte, but an exact type
// comparison misses it and the array branch would describe a list of integers.
func TestGenerateDescribesDefinedByteSlices(t *testing.T) {
	type Digest []byte
	type holder struct {
		D Digest `json:"d,omitempty"`
	}

	got, err := Generate(reflect.TypeOf(holder{}), Options{Title: "Holder"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	node := got.Properties["d"]
	if node.Type != typeString || node.ContentEncoding != "base64" {
		t.Errorf("defined byte slice described as type=%q contentEncoding=%q, "+
			"want string/base64", node.Type, node.ContentEncoding)
	}

	encoded, err := json.Marshal(holder{D: Digest("hi")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"d":"aGk="`) {
		t.Errorf("encoder wrote %s; the schema assumes a base64 string", encoded)
	}
}
