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
	"context"
	"fmt"
	"os"
	"path/filepath"

	cli "github.com/urfave/cli/v3"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

func generateValidatorCmd() *cli.Command {
	return &cli.Command{
		Name:     "generate-validator",
		Usage:    "Generate scaffolding for a new check or constraint validator",
		Category: "Development",
		Description: `Generate files for a new validation check or constraint:

Check (no expected value, yes/no validation):
  eidos generate-validator --check operator-health --phase deployment

Constraint (has expected value from recipe):
  eidos generate-validator --constraint Deployment.gpu-operator.version --phase deployment

This ensures new validators follow the correct architecture.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "check",
				Usage: "Check name (e.g., operator-health) - for yes/no validations",
			},
			&cli.StringFlag{
				Name:  "constraint",
				Usage: "Constraint name (e.g., Deployment.my-app.version) - for value comparisons",
			},
			&cli.StringFlag{
				Name:     "phase",
				Usage:    "Validation phase: readiness, deployment, performance, or conformance",
				Required: true,
			},
			&cli.StringFlag{
				Name:  "description",
				Usage: "Description of what this validator checks",
			},
			&cli.StringFlag{
				Name:  "output",
				Usage: "Output directory (default: pkg/validator/checks/<phase>)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			checkName := cmd.String("check")
			constraintName := cmd.String("constraint")
			phase := cmd.String("phase")
			description := cmd.String("description")
			outputDir := cmd.String("output")

			// Must specify either check or constraint
			if checkName == "" && constraintName == "" {
				return fmt.Errorf("must specify either --check or --constraint")
			}
			if checkName != "" && constraintName != "" {
				return fmt.Errorf("cannot specify both --check and --constraint")
			}

			// Validate phase
			validPhases := map[string]bool{"readiness": true, "deployment": true, "performance": true, "conformance": true}
			if !validPhases[phase] {
				return errors.New(errors.ErrCodeInvalidRequest, "--phase must be one of: readiness, deployment, performance, conformance")
			}

			// Default output directory
			if outputDir == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return errors.Wrap(errors.ErrCodeInternal, "failed to get current directory", err)
				}
				outputDir = filepath.Join(cwd, "pkg", "validator", "checks", phase)
			}

			// Check if output directory exists
			if _, err := os.Stat(outputDir); os.IsNotExist(err) {
				return errors.New(errors.ErrCodeNotFound, fmt.Sprintf("output directory does not exist: %s", outputDir))
			}

			// Generate validator files
			cfg := checks.GeneratorConfig{
				CheckName:      checkName,
				ConstraintName: constraintName,
				Phase:          phase,
				Description:    description,
				OutputDir:      outputDir,
			}

			if checkName != "" {
				if err := checks.GenerateCheck(cfg); err != nil {
					return errors.Wrap(errors.ErrCodeInternal, "failed to generate check", err)
				}
			} else {
				if err := checks.GenerateConstraintValidator(cfg); err != nil {
					return errors.Wrap(errors.ErrCodeInternal, "failed to generate constraint validator", err)
				}
			}

			return nil
		},
	}
}
