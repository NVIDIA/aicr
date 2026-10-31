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

package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
)

func recipeValidateCmd() *cli.Command {
	return &cli.Command{
		Name:  "validate",
		Usage: "Statically validate recipe catalog files without a cluster.",
		Description: `Validate an overlay, mixin, or registry file, or a catalog directory.
Use --data with a file to resolve references against that catalog.
Use --external-only with --data to validate a complete working-tree catalog.

Examples:
  aicr recipe validate --data recipes recipes/overlays/example.yaml
  aicr recipe validate --data recipes --external-only --format json`,
		Flags: []cli.Flag{
			dataFlag(),
			&cli.BoolFlag{Name: "external-only", Usage: "Validate only the catalog in --data, without embedded fallback (no FILE)", Category: catInput},
			&cli.StringFlag{Name: flagFormat, Value: verifyFormatText, Usage: "Output format (text or json)", Category: catOutput},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := validateSingleValueFlags(cmd, flagFormat, "data"); err != nil {
				return err
			}
			if cmd.Args().Len() > 1 || (cmd.Args().Len() == 0 && cmd.String("data") == "") {
				return errors.New(errors.ErrCodeInvalidRequest, "provide one file or --data <directory>")
			}
			if cmd.Bool("external-only") && (cmd.String("data") == "" || cmd.Args().Len() != 0) {
				return errors.New(errors.ErrCodeInvalidRequest, "--external-only requires --data and no file argument")
			}
			format := cmd.String(flagFormat)
			if format != verifyFormatText && format != verifyFormatJSON {
				return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unknown output format %q, valid formats are: text, json", format))
			}
			var diagnostics []aicr.RecipeDiagnostic
			var err error
			if cmd.Bool("external-only") {
				diagnostics, err = aicr.ValidateRecipeCatalogDirectory(ctx, cmd.String("data"))
			} else {
				diagnostics, err = aicr.ValidateRecipeCatalog(ctx, cmd.Args().First(), cmd.String("data"))
			}
			if err != nil {
				return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to validate recipe data")
			}
			if diagnostics == nil {
				diagnostics = []aicr.RecipeDiagnostic{}
			}
			w := cmd.Root().Writer
			switch {
			case format == verifyFormatJSON:
				encoder := json.NewEncoder(w)
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(diagnostics); err != nil {
					return errors.Wrap(errors.ErrCodeInternal, "failed to write validation report", err)
				}
			case len(diagnostics) == 0:
				if _, err := fmt.Fprintln(w, "Recipe validation passed."); err != nil {
					return errors.Wrap(errors.ErrCodeInternal, "failed to write validation report", err)
				}
			default:
				for _, d := range diagnostics {
					if _, err := fmt.Fprintf(w, "%s: %s: %s: %s\n", d.Severity, d.File, d.Field, d.Message); err != nil {
						return errors.Wrap(errors.ErrCodeInternal, "failed to write validation report", err)
					}
				}
			}
			for _, d := range diagnostics {
				if d.Severity == "error" {
					return errors.New(errors.ErrCodeInvalidRequest, "recipe validation failed")
				}
			}
			return nil
		},
	}
}
