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

package recipe

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"gopkg.in/yaml.v3"
)

// DecodeRecipeResult decodes one in-memory RecipeResult artifact. Profile
// artifacts are decoded strictly and as exactly one document; legacy
// artifacts retain their historical additive/non-strict compatibility. The
// typed version/profile gate runs in both cases so projections cannot discard
// profile identity.
func DecodeRecipeResult(data []byte, format serializer.Format) (*RecipeResult, error) {
	var artifactHeader struct {
		Kind       string `json:"kind" yaml:"kind"`
		APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	}
	headerReader, err := serializer.NewReader(format, bytes.NewReader(data))
	if err != nil {
		return nil, errors.PropagateOrWrap(
			err, errors.ErrCodeInvalidRequest, "failed to create recipe artifact header reader")
	}
	if headerErr := headerReader.Deserialize(&artifactHeader); headerErr != nil {
		return nil, errors.PropagateOrWrap(headerErr, errors.ErrCodeInvalidRequest,
			"failed to decode recipe artifact header")
	}
	if header.IsSupportedProfileAPIVersion(artifactHeader.APIVersion) && artifactHeader.Kind == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("recipe artifact apiVersion %q requires kind %q",
				artifactHeader.APIVersion, RecipeResultKind))
	}
	if artifactHeader.Kind != "" && artifactHeader.Kind != RecipeResultKind {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("recipe artifact has kind %q, expected %q",
				artifactHeader.Kind, RecipeResultKind))
	}
	if header.IsSupportedProfileAPIVersion(artifactHeader.APIVersion) {
		if profileErr := validateProfileExcludedOverlays(data, format); profileErr != nil {
			return nil, profileErr
		}
	}

	var opts []serializer.ReaderOption
	if header.IsSupportedProfileAPIVersion(artifactHeader.APIVersion) {
		opts = append(opts, serializer.WithStrict())
	}
	reader, err := serializer.NewReader(format, bytes.NewReader(data), opts...)
	if err != nil {
		return nil, errors.PropagateOrWrap(
			err, errors.ErrCodeInvalidRequest, "failed to create recipe artifact reader")
	}
	var result RecipeResult
	if err := reader.Deserialize(&result); err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
			"failed to decode recipe artifact")
	}
	if err := result.ValidateProfileContract(); err != nil {
		return nil, err
	}
	return &result, nil
}

func validateProfileExcludedOverlays(data []byte, format serializer.Format) error {
	switch format {
	case serializer.FormatJSON:
		var artifact struct {
			Metadata struct {
				ExcludedOverlays []json.RawMessage `json:"excludedOverlays"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(data, &artifact); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				"failed to inspect profile excluded overlays", err)
		}
		for index, item := range artifact.Metadata.ExcludedOverlays {
			type rawExcludedOverlay ExcludedOverlay
			var overlay *rawExcludedOverlay
			decoder := json.NewDecoder(bytes.NewReader(item))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&overlay); err != nil {
				return errors.Wrap(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("failed to decode excluded overlay at index %d", index), err)
			}
			if overlay == nil {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("excluded overlay at index %d must be an object", index))
			}
		}
		return nil

	case serializer.FormatYAML:
		var artifact struct {
			Metadata struct {
				ExcludedOverlays []yaml.Node `yaml:"excludedOverlays"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal(data, &artifact); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				"failed to inspect profile excluded overlays", err)
		}
		for index := range artifact.Metadata.ExcludedOverlays {
			if err := validateProfileExcludedOverlayYAML(
				&artifact.Metadata.ExcludedOverlays[index], index,
			); err != nil {
				return err
			}
		}
		return nil

	case serializer.FormatTable:
		return errors.New(errors.ErrCodeInvalidRequest,
			"table format does not support recipe artifacts")

	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported recipe artifact format %q", format))
	}
}

func validateProfileExcludedOverlayYAML(node *yaml.Node, index int) error {
	if node.Kind != yaml.MappingNode {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("excluded overlay at index %d must be an object", index))
	}
	for contentIndex := 0; contentIndex+1 < len(node.Content); contentIndex += 2 {
		key := node.Content[contentIndex]
		value := node.Content[contentIndex+1]
		if key.Kind != yaml.ScalarNode || key.ShortTag() != excludedOverlayYAMLStringTag {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("excluded overlay at index %d contains a non-string field name", index))
		}
		switch key.Value {
		case "name", "reason":
		default:
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("excluded overlay at index %d contains unknown field %q", index, key.Value))
		}
		if value.Kind != yaml.ScalarNode || value.ShortTag() != excludedOverlayYAMLStringTag {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("excluded overlay field %q at index %d must be a string", key.Value, index))
		}
	}
	return nil
}
