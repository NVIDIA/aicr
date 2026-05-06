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

package config

import (
	"context"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// configMapURIScheme matches the prefix used by the snapshot/recipe loaders
// for ConfigMap-backed inputs. AICRConfig deliberately does not support
// ConfigMap sources; this constant exists so we can reject those URIs with
// a tailored error pointing users at `kubectl`.
const configMapURIScheme = "cm://"

// Load reads and parses an AICRConfig from a local file path or
// HTTP(S) URL. ConfigMap (cm://) URIs are rejected.
//
// The returned AICRConfig is fully validated: kind/apiVersion match the
// expected constants, criteria enums parse against pkg/recipe parsers,
// and the deployer string parses against pkg/bundler/config.
func Load(ctx context.Context, source string) (*AICRConfig, error) {
	if source == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "config source is empty")
	}
	if strings.HasPrefix(source, configMapURIScheme) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"ConfigMap (cm://) sources are not supported by --config; "+
				"export the ConfigMap data with `kubectl get cm <name> -o yaml` and pass the resulting file")
	}

	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context canceled before config load", err)
	}

	cfg, err := serializer.FromFile[AICRConfig](source)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to load config from %q", source), err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
