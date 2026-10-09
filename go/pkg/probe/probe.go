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

// Package probe exercises aicr.run/v2 vanity-module resolution; it is not
// part of the AICR API.
package probe

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

const modulePath = "aicr.run/v2"

//go:embed data/hello.txt
var embedded string

// Embedded returns the go:embed payload, proving embedded assets ship in the module zip.
func Embedded() string {
	return strings.TrimSpace(embedded)
}

// Module returns the path and version of aicr.run/v2 as recorded in the build info.
func Module() (string, string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	if bi.Main.Path == modulePath {
		return bi.Main.Path, bi.Main.Version
	}
	for _, d := range bi.Deps {
		if d.Path == modulePath {
			return d.Path, d.Version
		}
	}
	return "", ""
}
