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

// Package file provides utilities for reading files from the filesystem.
//
// This package wraps standard file I/O operations with error handling conventions
// used throughout the collector framework. It provides a simple interface for
// reading file contents as strings.
//
// # Usage
//
// Read a single file:
//
//	content, err := file.ReadFile("/etc/os-release")
//	if err != nil {
//	    // Handle error
//	}
//	fmt.Println(content)
//
// The function automatically handles:
//   - File opening and closing
//   - Content reading
//   - Error wrapping with context
//
// # Error Handling
//
// Errors are wrapped with descriptive context:
//
//	content, err := file.ReadFile("/nonexistent")
//	// Error: failed to open file "/nonexistent": no such file or directory
//
// Common error scenarios:
//   - File does not exist (os.ErrNotExist)
//   - Permission denied (os.ErrPermission)
//   - I/O errors during read
//
// # Use in Collectors
//
// Collectors use this package for reading configuration files:
//
//	content, err := file.ReadFile("/etc/default/grub")
//	if err != nil {
//	    return nil, fmt.Errorf("failed to read GRUB config: %w", err)
//	}
//	// Parse content...
//
// # Thread Safety
//
// Functions in this package are thread-safe and can be called concurrently
// from multiple collectors.
package file
