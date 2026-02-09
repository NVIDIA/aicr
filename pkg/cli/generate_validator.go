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

	"github.com/NVIDIA/eidos/pkg/validator/checks"
	cli "github.com/urfave/cli/v3"
)

func generateValidatorCmd() *cli.Command {
	return &cli.Command{
		Name:     "generate-validator",
		Usage:    "Generate scaffolding for a new constraint validator",
		Category: "Development",
		Description: `Generate all files needed for a new constraint validator:
- Helper functions file for validation logic
- Unit test file with table-driven tests
- Integration test file with registration

This ensures new validators follow the correct architecture and have complete test coverage.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "constraint",
				Usage:    "Constraint name (e.g., Deployment.my-app.version)",
				Required: true,
			},
			&cli.StringFlag{
				Name:     "phase",
				Usage:    "Validation phase: deployment, performance, or conformance",
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
			constraintName := cmd.String("constraint")
			phase := cmd.String("phase")
			description := cmd.String("description")
			outputDir := cmd.String("output")

			// Validate phase
			if phase != "deployment" && phase != "performance" && phase != "conformance" {
				return fmt.Errorf("--phase must be one of: deployment, performance, conformance")
			}

			// Default output directory
			if outputDir == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("failed to get current directory: %w", err)
				}
				outputDir = filepath.Join(cwd, "pkg", "validator", "checks", phase)
			}

			// Check if output directory exists
			if _, err := os.Stat(outputDir); os.IsNotExist(err) {
				return fmt.Errorf("output directory does not exist: %s", outputDir)
			}

			// Generate validator files
			cfg := checks.GeneratorConfig{
				ConstraintName: constraintName,
				Phase:          phase,
				Description:    description,
				OutputDir:      outputDir,
			}

			if err := checks.GenerateConstraintValidator(cfg); err != nil {
				return fmt.Errorf("failed to generate validator: %w", err)
			}

			return nil
		},
	}
}
