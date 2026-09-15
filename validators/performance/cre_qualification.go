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

package main

import "github.com/NVIDIA/aicr/pkg/recipe"

// creCombination is a recipe criteria pair a Cluster Readiness Engine check can
// be qualified on.
type creCombination struct {
	Service     recipe.CriteriaServiceType
	Accelerator recipe.CriteriaAcceleratorType
}

// creCatalogEntry names the certification catalog entry a check drives on one
// combination, together with the node footprint it was qualified at. Both vary
// per combination: training/nemotron5-56b requires 32 GPUs and fails on a SKU
// where nemotron5-8b passes, and a node count proven on one accelerator says
// nothing about another.
type creCatalogEntry struct {
	Domain   string
	Variant  string
	MaxNodes int
}

// creQualifiedEntries is the qualification record — the combinations each CRE
// check has been measured on end to end, and what it was measured with. No
// driver, metric-extraction, cleanup, or evidence path branches on service or
// accelerator, so qualifying a new combination is an entry here plus a measured
// threshold in the recipe, never a new code path.
//
// eks x h100 was measured on 2026-09-02 against 2x p5.48xlarge: 489.80 GB/s bus
// bandwidth and a 0.7671 goodput ratio.
var creQualifiedEntries = map[string]map[creCombination]creCatalogEntry{
	checkNameCRENCCLAllReduceBW: {
		{Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100}: {
			Domain:   creNCCLDomain,
			Variant:  creNCCLVariant,
			MaxNodes: 2,
		},
	},
	checkNameCRETrainingGoodput: {
		{Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100}: {
			Domain:   creTrainingDomain,
			Variant:  creTrainingVariant,
			MaxNodes: 2,
		},
	},
}

// lookupCREQualification reports the catalog entry checkName was qualified with
// on the given combination. A false second return means the combination carries
// no qualification, which callers report as a skip rather than a failure: an
// unmeasured combination has no calibrated threshold to judge against.
func lookupCREQualification(
	checkName string,
	service recipe.CriteriaServiceType,
	accelerator recipe.CriteriaAcceleratorType,
) (creCatalogEntry, bool) {

	entries, ok := creQualifiedEntries[checkName]
	if !ok {
		return creCatalogEntry{}, false
	}
	entry, ok := entries[creCombination{Service: service, Accelerator: accelerator}]
	return entry, ok
}
