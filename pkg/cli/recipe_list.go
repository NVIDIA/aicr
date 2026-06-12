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
	"github.com/NVIDIA/aicr/pkg/health"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// healthNotApplicable is the placeholder rendered in the status/coverage table
// cells for non-leaf overlays, which pkg/health does not score (only leaf
// recipes resolve to a concrete combination).
const healthNotApplicable = "-"

// catalogListEntry augments a #1208 CatalogEntry with the ADR-009 §4
// structural-health axis for the leaf overlay backing it. Health is non-nil
// only for leaf overlays; the embedded CatalogEntry fields (name, criteria,
// is_leaf, source) marshal exactly as #1208 emitted them, so the health block
// is purely additive.
type catalogListEntry struct {
	// The yaml:",inline" tag is load-bearing: gopkg.in/yaml.v3 (used by
	// serializer.MarshalYAMLDeterministic) does NOT auto-inline an anonymous
	// struct field the way encoding/json does — without it the #1208 fields
	// nest under a "catalogentry:" key, breaking every existing YAML consumer.
	// The json:",inline" is a self-documenting no-op (encoding/json already
	// promotes anonymous fields).
	aicr.CatalogEntry `json:",inline" yaml:",inline"`

	Health *health.StructureHealth `json:"health,omitempty" yaml:"health,omitempty"`
}

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
			&cli.BoolFlag{
				Name:     flagNoHealth,
				Aliases:  []string{"skip-health"},
				Usage:    "Skip per-leaf structural-health computation. Omits the STATUS/COVERAGE table columns and the health block in json/yaml output.",
				Category: catOutput,
			},
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

			// The --no-health opt-out skips the per-leaf structural-health
			// computation entirely and renders the pre-#1228 catalog shape (no
			// STATUS/COVERAGE columns, no health block). A nil healthByName map
			// signals writeCatalogEntries to drop the health axis.
			showHealth := !cmd.Bool(flagNoHealth)

			var healthByName map[string]*health.StructureHealth
			if showHealth {
				// Delegate the structural-health computation to pkg/health (via
				// the facade, which binds this Client's own provider so --data
				// overlays are scored). Health is keyed by leaf overlay name so
				// each catalog entry can be paired with its verdict during
				// rendering.
				report, err := client.ComputeHealth(ctx, filter)
				if err != nil {
					return err
				}
				healthByName = make(map[string]*health.StructureHealth, len(report.Combos))
				for i := range report.Combos {
					healthByName[report.Combos[i].LeafOverlay] = &report.Combos[i].Structure
				}
			}

			return writeCatalogEntries(ctx, cmd, entries, healthByName, showHealth, format)
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
// requested format, pairing each entry with the structural-health verdict for
// its leaf overlay (healthByName, keyed by overlay name; nil for non-leaf
// overlays, which pkg/health does not score).
//
// json/yaml emit the full per-dimension status map and declared_coverage under
// a health object; table renders a compact structural status column and an
// R/D/P/C coverage summary.
//
// When showHealth is false (the --no-health opt-out), the health axis is
// dropped entirely: the table omits the STATUS/COVERAGE columns and json/yaml
// omit the health block, reproducing the pre-#1228 catalog shape. healthByName
// is nil in that case.
func writeCatalogEntries(ctx context.Context, cmd *cli.Command, entries []aicr.CatalogEntry, healthByName map[string]*health.StructureHealth, showHealth bool, format serializer.Format) error {
	w := cmd.Root().Writer

	switch format {
	case serializer.FormatJSON:
		data, err := json.MarshalIndent(withHealth(entries, healthByName), "", "  ")
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to marshal catalog entries as JSON", err)
		}
		if _, err := fmt.Fprintln(w, string(data)); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write JSON output", err)
		}

	case serializer.FormatYAML:
		data, err := serializer.MarshalYAMLDeterministic(withHealth(entries, healthByName))
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to marshal catalog entries as YAML", err)
		}
		if _, err := fmt.Fprint(w, string(data)); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write YAML output", err)
		}

	case serializer.FormatTable:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		header := "NAME\tSERVICE\tACCELERATOR\tINTENT\tOS\tPLATFORM\tIS_LEAF\tSTATUS\tCOVERAGE\tSOURCE"
		if !showHealth {
			header = "NAME\tSERVICE\tACCELERATOR\tINTENT\tOS\tPLATFORM\tIS_LEAF\tSOURCE"
		}
		if _, err := fmt.Fprintln(tw, header); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write table header", err)
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return errors.Wrap(errors.ErrCodeTimeout, "write canceled", err)
			}
			if !showHealth {
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
				continue
			}
			h := healthByName[e.Name]
			if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%v\t%s\t%s\t%s\n",
				e.Name,
				orAny(e.Criteria.Service),
				orAny(e.Criteria.Accelerator),
				orAny(e.Criteria.Intent),
				orAny(e.Criteria.OS),
				orAny(e.Criteria.Platform),
				e.IsLeaf,
				healthStatus(h),
				healthCoverage(h),
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

// withHealth pairs each catalog entry with the structural-health verdict for
// its leaf overlay, returning the augmented slice the structured formats
// marshal. Non-leaf overlays carry no health (nil), so the health block is
// omitted for them rather than emitting a misleading empty object.
func withHealth(entries []aicr.CatalogEntry, healthByName map[string]*health.StructureHealth) []catalogListEntry {
	out := make([]catalogListEntry, len(entries))
	for i, e := range entries {
		out[i] = catalogListEntry{CatalogEntry: e, Health: healthByName[e.Name]}
	}
	return out
}

// healthStatus renders the rolled-up structural status for a table cell, or the
// not-applicable placeholder when the overlay was not scored (non-leaf).
func healthStatus(h *health.StructureHealth) string {
	if h == nil {
		return healthNotApplicable
	}
	return h.Status
}

// healthCoverage renders the compact declared-coverage summary for a table
// cell, or the not-applicable placeholder when the overlay was not scored.
func healthCoverage(h *health.StructureHealth) string {
	if h == nil {
		return healthNotApplicable
	}
	return compactCoverage(h.Coverage)
}

// compactCoverage renders the declared-coverage descriptor as a one-line
// R/D/P/C summary of the named-check count each validation phase declares,
// e.g. "R:2 D:4 P:1 C:10". A nil descriptor (recipe did not resolve, so
// coverage is unknown) renders the not-applicable placeholder.
func compactCoverage(c *health.DeclaredCoverage) string {
	if c == nil {
		return healthNotApplicable
	}
	return c.Compact()
}

// orAny returns s if non-empty, otherwise the wildcard placeholder.
func orAny(s string) string {
	if s == "" || s == criteriaAny {
		return criteriaAny
	}
	return s
}
