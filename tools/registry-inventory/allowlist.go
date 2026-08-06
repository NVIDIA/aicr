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

package main

import (
	stderrors "errors"
	"io/fs"
	"os"
	"sort"

	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// allowlistFile is the committed set of approved egress hosts, relative to this
// tool's package directory.
const allowlistFile = "registry-allowlist.yaml"

type allowlist struct {
	Hosts []string `yaml:"hosts"`
}

func loadAllowlist(path string) (map[string]struct{}, error) {
	data, err := os.ReadFile(path) //nolint:gosec // fixed in-repo path
	if err != nil {
		code := errors.ErrCodeInternal
		if stderrors.Is(err, fs.ErrNotExist) {
			code = errors.ErrCodeNotFound
		}
		return nil, errors.Wrap(code, "read allowlist", err)
	}
	var al allowlist
	if err := yaml.Unmarshal(data, &al); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse allowlist", err)
	}
	set := make(map[string]struct{}, len(al.Hosts))
	for _, h := range al.Hosts {
		set[h] = struct{}{}
	}
	return set, nil
}

// diffAllowlist compares the detected host set against the committed allowlist.
// It returns hosts that are detected-but-not-allowlisted (unknown → a failure)
// and hosts that are allowlisted-but-no-longer-detected (unused → informational
// only, so a removed dependency doesn't hard-fail an unrelated PR).
func diffAllowlist(path string, hosts []string) (unknown, unused []string, err error) {
	allow, err := loadAllowlist(path)
	if err != nil {
		return nil, nil, err
	}
	detected := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		detected[h] = struct{}{}
		if _, ok := allow[h]; !ok {
			unknown = append(unknown, h)
		}
	}
	for h := range allow {
		if _, ok := detected[h]; !ok {
			unused = append(unused, h)
		}
	}
	sort.Strings(unknown)
	sort.Strings(unused)
	return unknown, unused, nil
}
