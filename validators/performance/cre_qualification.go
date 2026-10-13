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

import (
	"fmt"
	"math"
	"strings"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// creCombination is a recipe criteria pair a Cluster Readiness Engine check can
// be qualified on.
type creCombination struct {
	Service     recipe.CriteriaServiceType
	Accelerator recipe.CriteriaAcceleratorType
}

// creQualifiedEntries is the qualification record — the combinations each CRE
// check has been measured on end to end, and what it was measured with. No
// driver, metric-extraction, cleanup, or evidence path branches on service or
// accelerator, so qualifying a new combination is an entry here plus a measured
// threshold in the recipe, never a new code path.
//
// nccl-cre-all-reduce-bw is qualified on eks x h100. It was re-measured on the
// CRE version the registry pins, v0.6.0, on 2026-10-09 against 2x p5.48xlarge:
// 489.51 GB/s bus bandwidth, against 489.80 GB/s at the original 2026-09-02
// qualification on v0.1.0.
//
// cre-training-goodput carries no qualified combination. It measured 0.7671 on
// eks x h100 on CRE v0.1.0, but the nemotron5-8b catalog entry it drives does
// not run on v0.6.0: every rank exits in GPTModel.__init__ because the entry
// passes --attention-backend flash without clearing NVTE_FUSED_ATTN, which its
// base image sets to 1 and megatron-core rejects. The entry installs
// megatron-core at pod start from an unpinned revision, so that assertion
// arrived without a change on either side. Reproduced on 2026-10-09 from a
// hand-written Certification, which puts it upstream of AICR's driver. An empty
// map reports the check unqualified, so it skips instead of failing a healthy
// cluster for a broken workload, and the qualification returns by fixing the
// entry rather than by changing code here.
var creQualifiedEntries = map[string]map[creCombination]creCatalogEntry{
	checkNameCRENCCLAllReduceBW: {
		{Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100}: {
			Domain:   creNCCLDomain,
			Variant:  creNCCLVariant,
			MaxNodes: 2,
		},
	},
	checkNameCRETrainingGoodput: {},
}

// creThreshold parses a CRE check's constraint and refuses one the check cannot
// honor: an operator other than >=, since both CRE checks compare measured >=
// threshold and any other would be evaluated as a floor while the trace line
// printed the operator the recipe actually asked for — "<= 0.5" reported as
// satisfied by a ratio above it — and a non-finite threshold, which ParseFloat
// accepts but no measurement can be judged against. Refusing runs before the
// Certification is created, so no GPU work starts on a constraint the verdict
// cannot keep.
func creThreshold(checkName, value string) (float64, error) {
	trimmed := strings.TrimSpace(value)
	end := 0
	for end < len(trimmed) && strings.ContainsRune("><=!", rune(trimmed[end])) {
		end++
	}
	if operator := trimmed[:end]; operator != "" && operator != ">=" {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s compares with >= and cannot honor constraint %q", checkName, value))
	}
	threshold, err := parseThreshold(value)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s cannot judge a measurement against non-finite threshold %q", checkName, value))
	}
	return threshold, nil
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
