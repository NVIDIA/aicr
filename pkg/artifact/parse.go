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

package artifact

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Masterminds/semver/v3"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"gopkg.in/yaml.v3"
)

const (
	chartArtifactType = "chart"
	imageArtifactType = "image"
)

var (
	namePattern       = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	repositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	hostPattern       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]+)?$`)
)

// Invalid identifies artifact failures consistently across consumers.
func Invalid(reason, message string) error {
	return errors.NewWithContext(errors.ErrCodeInvalidRequest, reason+": "+message, map[string]any{"reason": reason})
}

func invalid(message string) error { return Invalid("artifact-invalid", message) }

// ParseInventory strictly reads exactly one bounded YAML or JSON document.
func ParseInventory(data []byte) (*Inventory, error) {
	var inventory Inventory
	if err := decode(data, &inventory); err != nil {
		return nil, err
	}
	if !header.IsSupportedArtifactInventory(inventory.Kind, inventory.APIVersion) {
		return nil, invalid("expected ArtifactInventory with apiVersion " + APIVersion)
	}
	if inventory.Artifacts == nil {
		return nil, invalid("artifacts must be an array")
	}
	if len(inventory.Artifacts) > defaults.ArtifactInventoryEntries {
		return nil, invalid("too many inventory artifacts")
	}
	if inventory.Metadata != nil && inventory.Metadata.Name != "" && !validName(inventory.Metadata.Name) {
		return nil, invalid("invalid metadata.name")
	}
	seen := map[string]bool{}
	for _, item := range inventory.Artifacts {
		if !validName(item.Name) || seen[item.Name] {
			return nil, invalid("invalid or duplicate artifact name " + item.Name)
		}
		seen[item.Name] = true
		if err := validateIdentity(item); err != nil {
			return nil, err
		}
	}
	return &inventory, nil
}

// ParseBindings validates shapes even for bindings that are inactive.
func ParseBindings(data []byte) (*Bindings, error) {
	var bindings Bindings
	if err := decode(data, &bindings); err != nil {
		return nil, err
	}
	if !header.IsSupportedArtifactBindings(bindings.Kind, bindings.APIVersion) {
		return nil, invalid("expected ArtifactBindings with apiVersion " + APIVersion)
	}
	if bindings.Charts == nil || bindings.Images == nil {
		return nil, invalid("charts and images must be maps")
	}
	if bindings.IgnoredArtifacts == nil {
		bindings.IgnoredArtifacts = map[string]Ignored{}
	}
	if err := ValidateBindings(&bindings); err != nil {
		return nil, err
	}
	return &bindings, nil
}

func decode(data []byte, dst any) error {
	if len(data) > defaults.ArtifactDocumentBytes || !utf8.Valid(data) {
		return invalid("document exceeds size limit or is not UTF-8")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid artifact document", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !stderrors.Is(err, io.EOF) {
		return invalid("expected exactly one document")
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return invalid("document must be an object")
	}
	value, err := strictNode(node.Content[0], 0)
	if err != nil {
		return err
	}
	if keyErr := validateFieldNames(value, reflect.TypeOf(dst).Elem()); keyErr != nil {
		return keyErr
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "encode artifact fields", err)
	}
	jsonDecoder := json.NewDecoder(bytes.NewReader(encoded))
	jsonDecoder.DisallowUnknownFields()
	if err := jsonDecoder.Decode(dst); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid artifact fields", err)
	}
	return nil
}

func validateFieldNames(value any, shape reflect.Type) error {
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	switch node := value.(type) {
	case map[string]any:
		fields := map[string]reflect.Type{}
		if shape.Kind() == reflect.Struct {
			for i := range shape.NumField() {
				field := shape.Field(i)
				fields[strings.Split(field.Tag.Get("json"), ",")[0]] = field.Type
			}
		}
		for name, child := range node {
			var childType reflect.Type
			switch {
			case shape.Kind() == reflect.Struct:
				childType = fields[name]
				if childType == nil {
					return invalid("unknown field " + name)
				}
			case shape.Kind() == reflect.Map:
				childType = shape.Elem()
			default:
				continue
			}
			if err := validateFieldNames(child, childType); err != nil {
				return err
			}
		}
	case []any:
		if shape.Kind() == reflect.Slice {
			for _, child := range node {
				if err := validateFieldNames(child, shape.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func strictNode(node *yaml.Node, depth int) (any, error) {
	if depth > defaults.ArtifactDocumentDepth {
		return nil, invalid("document exceeds nesting limit")
	}
	if node.Anchor != "" || node.Kind == yaml.AliasNode || node.Style&yaml.TaggedStyle != 0 {
		return nil, invalid("anchors, aliases and explicit tags are forbidden")
	}
	switch node.Kind {
	case yaml.MappingNode:
		result := map[string]any{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" || key.Anchor != "" || key.Style&yaml.TaggedStyle != 0 {
				return nil, invalid("mapping keys must be plain strings, without merge keys")
			}
			if _, exists := result[key.Value]; exists {
				return nil, invalid("duplicate key " + key.Value)
			}
			value, err := strictNode(node.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			result[key.Value] = value
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := strictNode(child, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case yaml.ScalarNode:
		// All scalar fields in these contracts are strings. Reject YAML's implicit
		// coercion of numeric, boolean, timestamp and null leaves into Go strings.
		if node.Tag != "!!str" || node.Value == "" {
			return nil, invalid("artifact scalar fields must be strings; quote numeric labels")
		}
		return node.Value, nil
	case yaml.DocumentNode, yaml.AliasNode:
		return nil, invalid("nested documents and aliases are forbidden")
	default:
		return nil, invalid("unsupported YAML node")
	}
}

func validName(value string) bool {
	return len(value) <= defaults.ArtifactNameLength && namePattern.MatchString(value)
}

func validateIdentity(item Identity) error {
	if item.Type != chartArtifactType && item.Type != imageArtifactType && item.Type != "other" {
		return invalid("invalid artifact type for " + item.Name)
	}
	parsed, err := url.Parse(item.Reference)
	if err != nil || !strings.HasPrefix(item.Reference, "oci://") || parsed.Scheme != "oci" || parsed.User != nil || !hostPattern.MatchString(parsed.Host) || (parsed.Host != "localhost" && !strings.ContainsAny(parsed.Host, ".:")) || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || !repositoryPattern.MatchString(strings.TrimPrefix(parsed.Path, "/")) || strings.ContainsAny(item.Reference, "@%?#") {
		return invalid("invalid OCI reference for " + item.Name)
	}
	if item.Version == "" || !digestPattern.MatchString(item.ManifestDigest) {
		return invalid("version and manifestDigest required for " + item.Name)
	}
	if item.ArchiveDigest != "" && (item.Type != chartArtifactType || !digestPattern.MatchString(item.ArchiveDigest)) {
		return invalid("invalid archiveDigest for " + item.Name)
	}
	if item.Type == chartArtifactType {
		if len(item.Version) > defaults.ArtifactChartTagBytes || strings.Contains(item.Version, "+") || strings.Count(item.Version, "_") > 1 {
			return invalid("chart version must be a literal Helm OCI tag")
		}
		version := strings.ReplaceAll(strings.TrimPrefix(item.Version, "v"), "_", "+")
		if _, err := semver.StrictNewVersion(version); err != nil {
			return invalid("invalid chart version for " + item.Name)
		}
	}
	if item.Platform != nil {
		if item.Type != imageArtifactType {
			return invalid("platform is image-only")
		}
		return validatePlatform(item.Platform)
	}
	return nil
}

func validatePlatform(platform *Platform) error {
	if platform != nil && (!validName(platform.OS) || !validName(platform.Architecture) || (platform.Variant != nil && !validName(*platform.Variant))) {
		return invalid("invalid platform")
	}
	return nil
}

// Pointer parses an object-only JSON Pointer, retaining escaped key semantics.
func Pointer(pointer string) ([]string, error) {
	if !strings.HasPrefix(pointer, "/") {
		return nil, invalid("JSON Pointer must start with /")
	}
	parts := strings.Split(pointer[1:], "/")
	for i, part := range parts {
		for j := 0; j < len(part); j++ {
			if part[j] == '~' {
				if j+1 >= len(part) || (part[j+1] != '0' && part[j+1] != '1') {
					return nil, invalid("invalid JSON Pointer escape")
				}
				j++
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

// ValidateBindings checks the merged disposition and target shape invariants.
func ValidateBindings(bindings *Bindings) error {
	seen, components := map[string]bool{}, map[string]bool{}
	claim := func(name string) error {
		if !validName(name) || seen[name] {
			return invalid("invalid or overlapping disposition " + name)
		}
		seen[name] = true
		return nil
	}
	for name, binding := range bindings.Charts {
		if err := claim(name); err != nil {
			return err
		}
		if !validName(binding.Component) || components[binding.Component] {
			return invalid("invalid or duplicate chart component " + binding.Component)
		}
		components[binding.Component] = true
	}
	for name, binding := range bindings.Images {
		if err := claim(name); err != nil {
			return err
		}
		if len(binding.Targets) == 0 {
			return invalid("image targets must not be empty")
		}
		for _, target := range binding.Targets {
			if !validName(target.Component) {
				return invalid("invalid target component")
			}
			if !validValuesFile(target.ValuesFile) {
				return invalid("invalid target valuesFile " + target.ValuesFile)
			}
			if err := validatePlatform(target.Platform); err != nil {
				return err
			}
			write := target.Write
			if write.Reference != "" {
				if write.Repository != "" || write.Digest != "" || write.ClearTag != "" {
					return invalid("reference write cannot have split fields")
				}
			} else if write.Repository == "" || write.Digest == "" {
				return invalid("write requires reference or repository and digest")
			}
			for _, pointer := range []string{write.Reference, write.Repository, write.Digest, write.ClearTag} {
				if pointer != "" {
					if _, err := Pointer(pointer); err != nil {
						return err
					}
				}
			}
		}
	}
	for name, ignored := range bindings.IgnoredArtifacts {
		if err := claim(name); err != nil {
			return err
		}
		if strings.TrimSpace(ignored.Reason) == "" {
			return invalid("ignore reason is required")
		}
	}
	return nil
}

func validValuesFile(value string) bool {
	if value == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return false
		}
	}
	switch path.Ext(value) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}

// EmptyBindings is the canonical empty disposition document.
func EmptyBindings() *Bindings {
	return &Bindings{APIVersion: APIVersion, Kind: BindingsKind, Charts: map[string]Chart{}, Images: map[string]Image{}, IgnoredArtifacts: map[string]Ignored{}}
}

// Merge replaces whole dispositions by artifact name, including across kinds.
// Inputs are never changed. Target slices are copied to isolate composed writes.
func Merge(layers ...*Bindings) (*Bindings, error) {
	result := EmptyBindings()
	clear := func(name string) {
		delete(result.Charts, name)
		delete(result.Images, name)
		delete(result.IgnoredArtifacts, name)
	}
	for _, layer := range layers {
		if layer == nil {
			continue
		}
		for name, value := range layer.Charts {
			clear(name)
			result.Charts[name] = value
		}
		for name, value := range layer.Images {
			clear(name)
			value.Targets = append([]Target(nil), value.Targets...)
			for i := range value.Targets {
				value.Targets[i].Platform = clonePlatform(value.Targets[i].Platform)
			}
			result.Images[name] = value
		}
		for name, value := range layer.IgnoredArtifacts {
			clear(name)
			result.IgnoredArtifacts[name] = value
		}
	}
	if err := ValidateBindings(result); err != nil {
		return nil, err
	}
	return result, nil
}

// Account requires every producer artifact to have a matching disposition.
func Account(inventory *Inventory, bindings *Bindings) error {
	for _, item := range inventory.Artifacts {
		if _, ok := bindings.IgnoredArtifacts[item.Name]; ok {
			continue
		}
		if _, ok := bindings.Charts[item.Name]; ok {
			if item.Type != chartArtifactType || item.ArchiveDigest == "" {
				return invalid("chart binding requires chart and archiveDigest for " + item.Name)
			}
			continue
		}
		if _, ok := bindings.Images[item.Name]; ok {
			if item.Type != imageArtifactType {
				return invalid("image binding requires image for " + item.Name)
			}
			continue
		}
		return Invalid("artifact-unbound", fmt.Sprintf("no binding for %s", item.Name))
	}
	return nil
}
