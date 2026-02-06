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

package cli

import (
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/eidos/pkg/serializer"
)

// parseOutputFormat extracts and validates the output format from CLI flags.
// Returns the validated format or an error if the format is unknown.
func parseOutputFormat(cmd *cli.Command) (serializer.Format, error) {
	outFormat := serializer.Format(cmd.String("format"))
	if outFormat.IsUnknown() {
		return "", fmt.Errorf("unknown output format: %q, valid formats are: yaml, json, table", outFormat)
	}
	return outFormat, nil
}

// validateSingleValueFlags checks that single-value flags are not passed multiple times.
// Returns an error if any of the specified flags appear more than once.
func validateSingleValueFlags(cmd *cli.Command, flagNames ...string) error {
	for _, name := range flagNames {
		if cmd.Count(name) > 1 {
			return fmt.Errorf("flag --%s can only be specified once", name)
		}
	}
	return nil
}
