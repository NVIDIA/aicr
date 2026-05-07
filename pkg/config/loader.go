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
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// configMapURIScheme matches the prefix used by the snapshot/recipe loaders
// for ConfigMap-backed inputs. AICRConfig deliberately does not support
// ConfigMap sources; this constant exists so we can reject those URIs with
// a tailored error pointing users at `kubectl`.
const configMapURIScheme = "cm://"

// fileURIScheme is rejected explicitly because os.ReadFile would otherwise
// produce a confusing "file not found: 'file:///abs/path'" error. Users
// should pass the bare path.
const fileURIScheme = "file://"

// Load reads and parses an AICRConfig from a local file path or
// HTTP(S) URL. ConfigMap (cm://) URIs are rejected.
//
// Decoding is strict: unknown fields cause an error, so typos like
// `spec.bundel.deployment.deployer` fail at load time rather than silently
// producing zero values.
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
	if strings.HasPrefix(source, fileURIScheme) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"file:// URIs are not supported by --config; pass the path directly (e.g. /etc/aicr/config.yaml)")
	}

	data, format, err := readSource(ctx, source)
	if err != nil {
		return nil, err
	}

	cfg := &AICRConfig{}
	if err := decodeStrict(data, format, cfg); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse config from %q", source), err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// sourceFormat is the on-the-wire format detected from the source path or URL.
type sourceFormat int

const (
	formatYAML sourceFormat = iota
	formatJSON
)

// readSource fetches the raw bytes from a file path or HTTP(S) URL and
// returns them along with the detected format. HTTP responses are bounded
// by defaults.HTTPResponseBodyLimit; oversized bodies are rejected.
func readSource(ctx context.Context, source string) ([]byte, sourceFormat, error) {
	format := detectFormat(source)
	switch {
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		data, err := readHTTP(ctx, source)
		return data, format, err
	default:
		data, err := readFile(ctx, source)
		return data, format, err
	}
}

func detectFormat(source string) sourceFormat {
	lower := strings.ToLower(source)
	// Strip query/fragment for URL extension matching.
	if i := strings.IndexAny(lower, "?#"); i >= 0 {
		lower = lower[:i]
	}
	if strings.HasSuffix(lower, ".json") {
		return formatJSON
	}
	return formatYAML
}

func readFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context canceled before file read", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is user-supplied --config target
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("config file not found: %q", path), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to read config file %q", path), err)
	}
	return data, nil
}

func readHTTP(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid config URL %q", url), err)
	}
	client := &http.Client{Timeout: defaults.HTTPClientTimeout}
	resp, err := client.Do(req) //nolint:gosec // G107: --config URL is the user's explicit choice; scheme is gated to http(s) by readSource and body is bounded by HTTPResponseBodyLimit

	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, fmt.Sprintf("failed to fetch config from %q", url), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New(errors.ErrCodeUnavailable,
			fmt.Sprintf("config fetch %q returned HTTP %d", url, resp.StatusCode))
	}

	limited := io.LimitReader(resp.Body, defaults.HTTPResponseBodyLimit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read config body from %q", url), err)
	}
	if int64(len(data)) > defaults.HTTPResponseBodyLimit {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("config body from %q exceeds %d-byte limit", url, defaults.HTTPResponseBodyLimit))
	}
	return data, nil
}

// decodeStrict parses raw bytes into target using strict semantics: unknown
// fields cause an error and only a single document is accepted. Both YAML
// and JSON inputs are supported.
//
// The trailing-data check uses a second Decode + io.EOF rather than
// json.Decoder.More() — More() returns false for some malformed trailing
// tokens (e.g. a stray "]"), letting garbage through.
func decodeStrict(data []byte, format sourceFormat, target any) error {
	switch format {
	case formatJSON:
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(target); err != nil {
			return err
		}
		var extra any
		if err := dec.Decode(&extra); !stderrors.Is(err, io.EOF) {
			if err == nil {
				return errors.New(errors.ErrCodeInvalidRequest, "unexpected trailing data after JSON document")
			}
			return errors.Wrap(errors.ErrCodeInvalidRequest, "trailing JSON data is not valid", err)
		}
		return nil
	case formatYAML:
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(target); err != nil {
			return err
		}
		var extra any
		if err := dec.Decode(&extra); !stderrors.Is(err, io.EOF) {
			if err == nil {
				return errors.New(errors.ErrCodeInvalidRequest, "unexpected trailing YAML document; AICRConfig accepts a single document")
			}
			return errors.Wrap(errors.ErrCodeInvalidRequest, "trailing YAML data is not valid", err)
		}
		return nil
	default:
		return errors.New(errors.ErrCodeInternal, "unsupported config format")
	}
}
