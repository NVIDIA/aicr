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

// Package file provides a configurable parser for line-oriented configuration
// files (e.g., /etc/default/grub, /etc/os-release, /proc/sys entries).
//
// This package wraps standard file I/O with bounded reads and error handling
// conventions used throughout the collector framework. It exposes a single
// Parser type configured via functional options.
//
// # Usage
//
// Parse a key=value config file into a map:
//
//	p := file.NewParser(
//	    file.WithKVDelimiter("="),
//	    file.WithSkipComments(true),
//	)
//	kv, err := p.GetMap("/etc/os-release")
//	if err != nil {
//	    // Handle error
//	}
//	fmt.Println(kv["PRETTY_NAME"])
//
// Or read raw lines:
//
//	lines, err := p.GetLines("/proc/modules")
//
// # Options
//
//   - WithDelimiter — entry separator (default "\n")
//   - WithMaxSize — maximum file size in bytes (defaults from pkg/defaults)
//   - WithSkipComments — drop lines starting with "#"
//   - WithKVDelimiter — key/value separator within each entry
//   - WithVDefault — fallback value when an entry has no value
//   - WithVTrimChars — characters trimmed from values (e.g., quotes)
//   - WithSkipEmptyValues — drop entries whose value is empty
//
// # Error Handling
//
// Errors are wrapped with descriptive context. Common scenarios:
//   - File does not exist (os.ErrNotExist)
//   - Permission denied (os.ErrPermission)
//   - File exceeds the configured maximum size
//   - I/O errors during read
//
// # Thread Safety
//
// A Parser is read-only after construction and safe for concurrent use.
package file
