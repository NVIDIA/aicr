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
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/eidos/pkg/serializer"
)

func TestParseOutputFormat(t *testing.T) {
	tests := []struct {
		name       string
		format     string
		wantFormat serializer.Format
		wantErr    bool
	}{
		{
			name:       "valid yaml format",
			format:     "yaml",
			wantFormat: serializer.FormatYAML,
			wantErr:    false,
		},
		{
			name:       "valid json format",
			format:     "json",
			wantFormat: serializer.FormatJSON,
			wantErr:    false,
		},
		{
			name:       "valid table format",
			format:     "table",
			wantFormat: serializer.FormatTable,
			wantErr:    false,
		},
		{
			name:       "invalid format xml",
			format:     "xml",
			wantFormat: "",
			wantErr:    true,
		},
		{
			name:       "invalid format csv",
			format:     "csv",
			wantFormat: "",
			wantErr:    true,
		},
		{
			name:       "invalid format unknown",
			format:     "unknown",
			wantFormat: "",
			wantErr:    true,
		},
		{
			name:       "empty format",
			format:     "",
			wantFormat: "",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a minimal CLI command with the format flag
			cmd := &cli.Command{
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "format",
						Value: tt.format,
					},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					got, err := parseOutputFormat(c)
					if (err != nil) != tt.wantErr {
						t.Errorf("parseOutputFormat() error = %v, wantErr %v", err, tt.wantErr)
						return nil
					}
					if !tt.wantErr && got != tt.wantFormat {
						t.Errorf("parseOutputFormat() = %v, want %v", got, tt.wantFormat)
					}
					return nil
				},
			}

			// Run the command with the test format
			err := cmd.Run(context.Background(), []string{"test"})
			if err != nil {
				t.Fatalf("failed to run command: %v", err)
			}
		})
	}
}

func TestValidateSingleValueFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		flags   []string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "single flag once is valid",
			args:    []string{"test", "--recipe", "recipe.yaml"},
			flags:   []string{"recipe"},
			wantErr: false,
		},
		{
			name:    "single flag twice should error",
			args:    []string{"test", "--recipe", "first.yaml", "--recipe", "second.yaml"},
			flags:   []string{"recipe"},
			wantErr: true,
			errMsg:  "flag --recipe can only be specified once",
		},
		{
			name:    "multiple different flags once each is valid",
			args:    []string{"test", "--recipe", "recipe.yaml", "--output", "out.yaml"},
			flags:   []string{"recipe", "output"},
			wantErr: false,
		},
		{
			name:    "flag not in check list can be duplicated",
			args:    []string{"test", "--other", "first", "--other", "second"},
			flags:   []string{"recipe"},
			wantErr: false,
		},
		{
			name:    "flag with alias twice should error",
			args:    []string{"test", "-r", "first.yaml", "--recipe", "second.yaml"},
			flags:   []string{"recipe"},
			wantErr: true,
			errMsg:  "flag --recipe can only be specified once",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotErr error
			cmd := &cli.Command{
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "recipe",
						Aliases: []string{"r"},
					},
					&cli.StringFlag{
						Name:    "output",
						Aliases: []string{"o"},
					},
					&cli.StringFlag{
						Name: "other",
					},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					gotErr = validateSingleValueFlags(c, tt.flags...)
					return nil
				},
			}

			err := cmd.Run(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("failed to run command: %v", err)
			}

			if (gotErr != nil) != tt.wantErr {
				t.Errorf("validateSingleValueFlags() error = %v, wantErr %v", gotErr, tt.wantErr)
			}
			if tt.wantErr && gotErr != nil && gotErr.Error() != tt.errMsg {
				t.Errorf("validateSingleValueFlags() error message = %q, want %q", gotErr.Error(), tt.errMsg)
			}
		})
	}
}
