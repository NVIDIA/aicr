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

// Package schema derives JSON Schema documents from the Go types that define
// AICR's published artifacts.
//
// Artifact schemas are one of the four surfaces ROADMAP section 1 freezes at v1
// (issue #2113). Integrators need a machine-readable description of what a
// Snapshot or RecipeResult contains, and the project needs a baseline it can
// diff to catch a field being removed or narrowed.
//
// # Why reflection rather than a schema library
//
// A generator has to compile against the types, so it cannot be a pinned
// binary the way oasdiff is for the REST contract. Importing a schema library
// would put it in the module graph, and therefore in the SBOM and vulnerability
// surface of the shipped binaries, for something that only ever runs at build
// time. tools/api-diff and tools/openapi-diff both avoid that; this follows.
//
// The tradeoff is that this file is hand-written, so it is deliberately narrow.
// It covers the shapes the artifact types actually use — structs, embedded
// structs, pointers, slices, maps, strings, integers, booleans and any — and
// FAILS on anything else rather than emitting a plausible guess. A schema that
// silently describes an unsupported type incorrectly is worse than no schema,
// because the diff gate would then protect the wrong shape.
package schema

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// marshalerType and textMarshalerType detect types that define their own wire
// form, whose shape reflection over the Go fields cannot predict.
var (
	marshalerType     = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// Draft is the JSON Schema dialect the generated documents declare.
const Draft = "https://json-schema.org/draft/2020-12/schema"

// JSON Schema primitive type names.
const (
	typeString  = "string"
	typeBoolean = "boolean"
	typeInteger = "integer"
	typeNumber  = "number"
	typeArray   = "array"
	typeObject  = "object"
)

// Schema is a JSON Schema node.
//
// Field order matches the marshaled output, and every collection field is
// omitempty so a scalar node does not carry empty objects.
type Schema struct {
	Schema      string             `json:"$schema,omitempty"`
	ID          string             `json:"$id,omitempty"`
	Title       string             `json:"title,omitempty"`
	Description string             `json:"description,omitempty"`
	Type        string             `json:"-"`
	Enum        []string           `json:"enum,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Items       *Schema            `json:"items,omitempty"`

	// ContentEncoding describes a string that carries encoded bytes, which is
	// how encoding/json writes a []byte.
	ContentEncoding string `json:"contentEncoding,omitempty"`

	// AdditionalProperties is a *bool so that "false" is emitted and "unset"
	// is not. A plain bool would make every closed object indistinguishable
	// from one that never declared the constraint.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`

	// PropertyNames constrains map keys; nil for objects with fixed fields.
	PropertyNames *Schema `json:"propertyNames,omitempty"`

	// Nullable records that the encoder can write JSON null here, which
	// happens for any nil-able Go type whose tag lacks omitempty.
	Nullable bool `json:"-"`
}

// MarshalJSON writes the node, expressing nullability the JSON Schema 2020-12
// way: as a type array rather than the OpenAPI 3.0 `nullable` keyword, which
// this dialect does not define.
//
// Field order is explicit rather than inherited from struct order, because
// these documents are committed and compared byte-for-byte.
func (s Schema) MarshalJSON() ([]byte, error) {
	out := struct {
		Schema               string             `json:"$schema,omitempty"`
		ID                   string             `json:"$id,omitempty"`
		Title                string             `json:"title,omitempty"`
		Description          string             `json:"description,omitempty"`
		Type                 any                `json:"type,omitempty"`
		Enum                 []string           `json:"enum,omitempty"`
		Properties           map[string]*Schema `json:"properties,omitempty"`
		Required             []string           `json:"required,omitempty"`
		Items                *Schema            `json:"items,omitempty"`
		AdditionalProperties *bool              `json:"additionalProperties,omitempty"`
		PropertyNames        *Schema            `json:"propertyNames,omitempty"`
		ContentEncoding      string             `json:"contentEncoding,omitempty"`
	}{
		Schema: s.Schema, ID: s.ID, Title: s.Title, Description: s.Description,
		Enum: s.Enum, Properties: s.Properties, Required: s.Required,
		Items: s.Items, AdditionalProperties: s.AdditionalProperties,
		PropertyNames: s.PropertyNames, ContentEncoding: s.ContentEncoding,
	}

	switch {
	case s.Type == "":
		// An open value: no constraint, so null is already permitted.
	case s.Nullable:
		out.Type = []string{s.Type, "null"}
	default:
		out.Type = s.Type
	}
	return json.Marshal(out)
}

// Options controls generation for one artifact kind.
type Options struct {
	// Title is the artifact kind, e.g. "RecipeResult".
	Title string

	// Description explains what the artifact is, for integrators reading the
	// generated file without the Go source at hand.
	Description string

	// ID is the canonical URI for the schema.
	ID string

	// Enums supplies allowed values for named types, keyed by the type's
	// package-qualified name (e.g. "recipe.CriteriaServiceType"). A type with
	// no entry is described only by its kind.
	Enums map[string][]string

	// Authored marks an artifact a human writes rather than one AICR emits,
	// which changes what "required" means.
	//
	// For an emitted artifact the encoder's behavior is the contract: a field
	// without omitempty is always written, so a consumer may rely on it. For an
	// authored one it is not. ComponentRef.Source carries no omitempty, so the
	// encoder always writes it -- but only 17 of the 110 committed overlays set
	// it, and marking it required published a schema that rejected the other 93.
	Authored bool
}

// Generate derives a JSON Schema from a Go type.
//
// It returns an error rather than emitting a partial document: a schema that
// omits a field it could not model would be a baseline that silently stops
// protecting it.
func Generate(target reflect.Type, opts Options) (*Schema, error) {
	root, err := describe(target, opts, map[reflect.Type]bool{})
	if err != nil {
		return nil, err
	}

	root.Schema = Draft
	root.ID = opts.ID
	root.Title = opts.Title
	root.Description = opts.Description
	return root, nil
}

// describe converts one type, guarding against recursive types.
func describe(target reflect.Type, opts Options, seen map[reflect.Type]bool) (*Schema, error) {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}

	// A type with its own marshaler emits whatever it likes, and reflecting
	// over its Go fields describes something else entirely: time.Time has no
	// exported fields, so it would be published as an empty object while the
	// encoder writes an RFC 3339 string. Every document would fail validation
	// against its own schema. Reject rather than guess -- the caller extends
	// this package deliberately, which is the point of the fail-closed rule in
	// the package comment.
	if hasCustomMarshaler(target) {
		return nil, fmt.Errorf("type %s defines its own JSON encoding; "+
			"reflection over its fields would describe a different shape than it "+
			"emits. Add explicit support in pkg/schema before using it in a "+
			"published artifact", typeKey(target))
	}

	// Reached only after the marshaler check above, and the order matters:
	// json.RawMessage and any named []byte carrying a MarshalJSON are byte
	// slices whose wire form is arbitrary JSON, not base64. Classifying them
	// here first described `[1,2]` and `{"a":1}` as base64 strings.
	//
	// A plain byte slice has no marshaler, so it still lands here: it is the
	// one custom encoding whose output is fully defined, and encoding/json
	// writes it as a base64 string rather than an array of numbers --
	// including a defined type such as `type Digest []byte`, which an exact
	// []byte comparison would miss.
	if target.Kind() == reflect.Slice && target.Elem().Kind() == reflect.Uint8 {
		return &Schema{Type: typeString, ContentEncoding: "base64"}, nil
	}

	if enum, ok := opts.Enums[typeKey(target)]; ok && target.Kind() == reflect.String {
		sorted := append([]string(nil), enum...)
		sort.Strings(sorted)
		return &Schema{Type: typeString, Enum: sorted}, nil
	}

	// Every kind not listed falls to the default arm, which fails rather than
	// guessing. Enumerating chan, func and unsafe.Pointer here would only
	// restate that they are unsupported.
	//nolint:exhaustive // default arm fails closed for every unlisted kind
	switch target.Kind() {
	case reflect.String:
		return &Schema{Type: typeString}, nil
	case reflect.Bool:
		return &Schema{Type: typeBoolean}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: typeInteger}, nil
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: typeNumber}, nil

	case reflect.Interface:
		// `any` in an artifact is a deliberately open value (component values,
		// arbitrary overrides). An empty schema is the honest description:
		// anything validates. Narrowing it here would reject valid documents.
		if target.NumMethod() == 0 {
			return &Schema{}, nil
		}
		return nil, fmt.Errorf("unsupported non-empty interface type %s", typeKey(target))

	case reflect.Slice, reflect.Array:
		items, err := describe(target.Elem(), opts, seen)
		if err != nil {
			return nil, fmt.Errorf("slice element of %s: %w", typeKey(target), err)
		}
		return &Schema{Type: typeArray, Items: items}, nil

	case reflect.Map:
		if target.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map %s has a non-string key; JSON objects "+
				"cannot express it", typeKey(target))
		}
		// Value shape is intentionally not encoded as additionalProperties:
		// the diff gate compares structure, and a permissive map is described
		// by its key constraint plus an open value.
		return &Schema{Type: typeObject, PropertyNames: &Schema{Type: typeString}}, nil

	case reflect.Struct:
		return describeStruct(target, opts, seen)

	default:
		return nil, fmt.Errorf("unsupported kind %s for type %s",
			target.Kind(), typeKey(target))
	}
}

// describeStruct converts a struct, flattening embedded fields the way
// encoding/json does.
func describeStruct(target reflect.Type, opts Options, seen map[reflect.Type]bool) (*Schema, error) {
	if seen[target] {
		// A recursive artifact type would loop forever. None exist today, so
		// fail loudly rather than emitting a truncated node that the diff gate
		// would then treat as the contract.
		return nil, fmt.Errorf("recursive type %s is not supported", typeKey(target))
	}
	seen[target] = true
	defer delete(seen, target)

	node := &Schema{Type: typeObject, Properties: map[string]*Schema{}}

	if err := collectFields(target, opts, seen, node); err != nil {
		return nil, err
	}

	sort.Strings(node.Required)
	if len(node.Properties) == 0 {
		node.Properties = nil
	}
	return node, nil
}

// collectFields walks a struct's fields into node, recursing through embedded
// structs so their fields appear at the parent level.
func collectFields(target reflect.Type, opts Options, seen map[reflect.Type]bool, node *Schema) error {
	for i := range target.NumField() {
		field := target.Field(i)

		name, omitempty, skip := jsonFieldName(field)
		if skip {
			continue
		}

		// An anonymous field of an *unexported struct type* still has its
		// exported fields promoted by encoding/json, and reflect reports
		// IsExported()==false for it. Rejecting on that alone dropped the whole
		// embedded block. Snapshot happened to embed an exported header, so the
		// bug was invisible there and would have surfaced the first time an
		// artifact embedded an internal type.
		embeddedStruct := field.Anonymous && name == "" && structBehind(field.Type) != nil

		if !field.IsExported() && !embeddedStruct {
			continue
		}

		// encoding/json flattens an anonymous struct field whose tag gives no
		// name. Snapshot embeds header.Header this way, which is where kind and
		// apiVersion come from — describing it as a nested object would produce
		// a schema no real snapshot validates against.
		if embeddedStruct {
			if err := collectFields(structBehind(field.Type), opts, seen, node); err != nil {
				return err
			}
			continue
		}

		if name == "" {
			name = field.Name
		}

		child, err := describe(field.Type, opts, seen)
		if err != nil {
			return fmt.Errorf("field %s.%s: %w", target.Name(), field.Name, err)
		}
		child.Description = strings.TrimSpace(child.Description)

		// Without omitempty the encoder always writes the key -- and writes
		// null when the value is a nil pointer, slice, map or interface. The
		// earlier shape excluded pointers from required and described them as
		// non-null, so a document carrying the null the encoder itself produces
		// failed against its own schema.
		writesNull := !omitempty && isNilable(field.Type)
		if writesNull {
			child.Nullable = true
		}
		node.Properties[name] = child

		// Required is exactly the set the encoder always writes, null included.
		// Authored artifacts declare nothing required, because what a writer
		// emits says nothing about what an author must supply.
		if !opts.Authored && !omitempty {
			node.Required = append(node.Required, name)
		}
	}
	return nil
}

// isNilable reports whether a type can hold nil, and therefore whether the
// encoder can write JSON null for it.
func isNilable(target reflect.Type) bool {
	// Only the nil-able kinds are listed; everything else is a value type that
	// the encoder never writes as null.
	//nolint:exhaustive // default arm covers every non-nil-able kind
	switch target.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	default:
		return false
	}
}

// hasCustomMarshaler reports whether a type controls its own JSON encoding.
//
// Both the value and pointer forms are checked: a marshaler declared on a
// pointer receiver still governs the encoding of a field of that type once the
// encoder can address it, and missing that case would silently reopen the very
// hole this guards.
func hasCustomMarshaler(target reflect.Type) bool {
	pointer := reflect.PointerTo(target)
	for _, candidate := range []reflect.Type{target, pointer} {
		if candidate.Implements(marshalerType) || candidate.Implements(textMarshalerType) {
			return true
		}
	}
	return false
}

// structBehind returns the struct type a field denotes, following pointers, or
// nil when the field is not a struct.
func structBehind(target reflect.Type) reflect.Type {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target.Kind() != reflect.Struct {
		return nil
	}
	return target
}

// jsonFieldName reports the wire name for a field, whether it is omitempty, and
// whether the encoder skips it entirely.
func jsonFieldName(field reflect.StructField) (name string, omitempty, skip bool) {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return "", false, false
	}
	if tag == "-" {
		return "", false, true
	}

	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty, false
}

// typeKey is the package-qualified name used for enum lookup and errors.
func typeKey(target reflect.Type) string {
	if target.Name() == "" {
		return target.String()
	}
	if target.PkgPath() == "" {
		return target.Name()
	}
	pkg := target.PkgPath()
	if idx := strings.LastIndex(pkg, "/"); idx >= 0 {
		pkg = pkg[idx+1:]
	}
	return pkg + "." + target.Name()
}
