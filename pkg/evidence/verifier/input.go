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

package verifier

import (
	"os"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// DetectInputForm classifies a user-supplied input. Only directory
// input is supported; pointer and OCI forms are rejected.
func DetectInputForm(input string) (InputForm, error) {
	if input == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest, "input is empty")
	}
	info, err := os.Stat(input)
	if err != nil {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"input "+input+" is not a directory (pointer and OCI forms not yet supported)")
	}
	if !info.IsDir() {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"input "+input+" is not a directory (pointer and OCI forms not yet supported)")
	}
	return InputFormDir, nil
}
