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
	"strings"
	"text/tabwriter"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func recipeListCmd() *cli.Command {
	return &cli.Command{
		Name:  cmdNameRecipeList,
		Usage: "List recipe overlays in the catalog.",
		Description: `Enumerate all overlay recipes in the catalog and their criteria.

Each entry shows the overlay name, its criteria dimensions, whether it is a
leaf overlay (no other overlay inherits from it), and its data source
(embedded or external).

Filter flags narrow the output to overlays that carry the specified criteria
value. Unspecified flags match all overlays for that dimension.

Examples:

List all overlays (table format):
  aicr recipe list

List all overlays as JSON:
  aicr recipe list --format json

Filter to EKS training overlays:
  aicr recipe list --service eks --intent training

Filter to H100 overlays as JSON:
  aicr recipe list --accelerator h100 --format json

Include overlays from an external data directory:
  aicr recipe list --data /path/to/custom-recipes`,
		Flags: []cli.Flag{
			withCompletions(&cli.StringFlag{
				Name:     flagService,
				Usage:    fmt.Sprintf("Filter by service type (e.g. %s)", strings.Join(recipe.GetCriteriaServiceTypes(), ", ")),
				Category: catQueryParameters,
			}, recipe.GetCriteriaServiceTypes),
			withCompletions(&cli.StringFlag{
				Name:     flagAccelerator,
				Aliases:  []string{"gpu"},
				Usage:    fmt.Sprintf("Filter by accelerator type (e.g. %s)", strings.Join(recipe.GetCriteriaAcceleratorTypes(), ", ")),
				Category: catQueryParameters,
			}, recipe.GetCriteriaAcceleratorTypes),
			withCompletions(&cli.StringFlag{
				Name:     flagIntent,
				Usage:    fmt.Sprintf("Filter by workload intent (e.g. %s)", strings.Join(recipe.GetCriteriaIntentTypes(), ", ")),
				Category: catQueryParameters,
			}, recipe.GetCriteriaIntentTypes),
			withCompletions(&cli.StringFlag{
				Name:     flagOS,
				Usage:    fmt.Sprintf("Filter by OS type (e.g. %s)", strings.Join(recipe.GetCriteriaOSTypes(), ", ")),
				Category: catQueryParameters,
			}, recipe.GetCriteriaOSTypes),
			withCompletions(&cli.StringFlag{
				Name:     flagPlatform,
				Usage:    fmt.Sprintf("Filter by platform type (e.g. %s)", strings.Join(recipe.GetCriteriaPlatformTypes(), ", ")),
				Category: catQueryParameters,
			}, recipe.GetCriteriaPlatformTypes),
			dataFlag(),
			withCompletions(&cli.StringFlag{
				Name:     flagFormat,
				Aliases:  []string{"t"},
				Value:    string(serializer.FormatTable),
				Usage:    "Output format (json, yaml, table)",
				Category: catOutput,
			}, func() []string { return []string{"json", "yaml", "table"} }),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := validateSingleValueFlags(cmd, flagService, flagAccelerator, flagIntent, flagOS, flagPlatform, flagFormat); err != nil {
				return err
			}

			client, err := recipeClientFromCmd(cmd, nil)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if err = client.LoadCatalog(ctx); err != nil {
				return err
			}

			var filter *aicr.Criteria
			if hasAnyCriteriaFlag(cmd) {
				filter, err = buildCatalogFilter(cmd, client)
				if err != nil {
					return err
				}
			}

			entries, err := client.ListCatalog(ctx, filter)
			if err != nil {
				return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to list catalog")
			}

			format := serializer.Format(cmd.String(flagFormat))
			if format.IsUnknown() {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("unknown output format %q, valid formats are: json, yaml, table", cmd.String(flagFormat)))
			}

			return writeCatalogEntries(ctx, cmd, entries, format)
		},
	}
}

// hasAnyCriteriaFlag reports whether the user provided at least one filter flag.
func hasAnyCriteriaFlag(cmd *cli.Command) bool {
	for _, name := range []string{flagService, flagAccelerator, flagIntent, flagOS, flagPlatform} {
		if cmd.IsSet(name) {
			return true
		}
	}
	return false
}

// buildCatalogFilter constructs a Criteria filter from the CLI flags.
// Each flag value is parsed through the client's criteria registry so
// --data overlay values are accepted. Returns an error for unrecognized values.
func buildCatalogFilter(cmd *cli.Command, client *aicr.Client) (*aicr.Criteria, error) {
	reg := client.CriteriaRegistry()
	filter := &aicr.Criteria{}

	if s := cmd.String(flagService); s != "" {
		parsed, err := reg.ParseService(s)
		if err != nil {
			return nil, err
		}
		filter.Service = string(parsed)
	}
	if s := cmd.String(flagAccelerator); s != "" {
		parsed, err := reg.ParseAccelerator(s)
		if err != nil {
			return nil, err
		}
		filter.Accelerator = string(parsed)
	}
	if s := cmd.String(flagIntent); s != "" {
		parsed, err := reg.ParseIntent(s)
		if err != nil {
			return nil, err
		}
		filter.Intent = string(parsed)
	}
	if s := cmd.String(flagOS); s != "" {
		parsed, err := reg.ParseOS(s)
		if err != nil {
			return nil, err
		}
		filter.OS = string(parsed)
	}
	if s := cmd.String(flagPlatform); s != "" {
		parsed, err := reg.ParsePlatform(s)
		if err != nil {
			return nil, err
		}
		filter.Platform = string(parsed)
	}
	return filter, nil
}

// writeCatalogEntries writes catalog entries to the command's writer in the
// requested format.
func writeCatalogEntries(ctx context.Context, cmd *cli.Command, entries []aicr.CatalogEntry, format serializer.Format) error {
	w := cmd.Root().Writer

	switch format {
	case serializer.FormatJSON:
		data, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to marshal catalog entries as JSON", err)
		}
		if _, err := fmt.Fprintln(w, string(data)); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write JSON output", err)
		}

	case serializer.FormatYAML:
		data, err := serializer.MarshalYAMLDeterministic(entries)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to marshal catalog entries as YAML", err)
		}
		if _, err := fmt.Fprint(w, string(data)); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write YAML output", err)
		}

	case serializer.FormatTable:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "NAME\tSERVICE\tACCELERATOR\tINTENT\tOS\tPLATFORM\tIS_LEAF\tSOURCE"); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write table header", err)
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return errors.Wrap(errors.ErrCodeTimeout, "write canceled", err)
			}
			if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%v\t%s\n",
				e.Name,
				orAny(e.Criteria.Service),
				orAny(e.Criteria.Accelerator),
				orAny(e.Criteria.Intent),
				orAny(e.Criteria.OS),
				orAny(e.Criteria.Platform),
				e.IsLeaf,
				e.Source,
			); err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to write table row", err)
			}
		}
		if err := tw.Flush(); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to flush table output", err)
		}
		if len(entries) == 0 {
			if _, err := fmt.Fprintln(w, "(no matching overlays)"); err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to write empty message", err)
			}
		}
	}

	return nil
}

// orAny returns s if non-empty, otherwise the wildcard placeholder.
func orAny(s string) string {
	if s == "" || s == criteriaAny {
		return criteriaAny
	}
	return s
}
