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

package serializer

// DeepCopyAnyMap returns a deep copy of a map[string]any tree, recursing into
// both nested maps and slices so callers may safely mutate the returned tree
// without leaking writes back to the source. A nil input returns an empty,
// non-nil map so callers can immediately Insert without a nil check.
//
// Scalars (numbers, strings, bools) fall through to the default branch and
// are returned by value. Slice copy semantics intentionally cover []any
// rather than typed slices, since the common feed is YAML-decoded data.
func DeepCopyAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return make(map[string]any)
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = DeepCopyAny(v)
	}
	return out
}

// DeepCopyAny returns a deep copy of v. Maps recurse via DeepCopyAnyMap,
// []any recurses element-by-element, and all other values are returned
// as-is (scalars are values; rare aliased reference types like channels
// or pointers are intentionally aliased — YAML decoding does not produce
// them, and a generic clone would risk surprising semantics).
func DeepCopyAny(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return DeepCopyAnyMap(val)
	case []any:
		cp := make([]any, len(val))
		for i, item := range val {
			cp[i] = DeepCopyAny(item)
		}
		return cp
	default:
		return v
	}
}
