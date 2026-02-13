// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package serializer provides utilities for serializing data to various formats.
//
// The package supports three main output formats:
//   - JSON: Machine-readable structured data with proper indentation
//   - YAML: Human-readable configuration format
//   - Table: Human-readable tabular output with flattened keys
//
// Usage:
//
//	writer := serializer.NewWriter(serializer.FormatJSON, os.Stdout)
//	defer writer.Close() // Important: close to release file handles
//	if err := writer.Serialize(ctx, data); err != nil {
//		log.Fatal(err)
//	}
//
// For HTTP responses:
//
//	serializer.RespondJSON(w, http.StatusOK, data)
//
// The package automatically handles:
//   - Proper content-type headers for HTTP responses
//   - Buffering to prevent partial responses on errors
//   - Flattening nested structures for table format
//   - Resource cleanup via Close() method
package serializer

import "context"

// Serializer is an interface for serializing snapshot data.
// Implementations of this interface can serialize data to various formats
// such as JSON, YAML, or plain text.
//
// The context parameter is used for cancellation and timeouts, particularly
// important for implementations that perform I/O operations (e.g., ConfigMap writes).
type Serializer interface {
	Serialize(ctx context.Context, snapshot any) error
}

// Closer is an optional interface that Serializers can implement
// if they need to release resources (e.g., close file handles).
type Closer interface {
	Close() error
}
