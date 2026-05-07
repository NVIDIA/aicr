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

package localformat

import (
	"context"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// buildKustomize runs `kustomize build` against path using the in-process
// kustomize Go library, returning the flattened single-YAML-document output.
// Uses filesys.MakeFsOnDisk so relative resource refs inside the kustomization
// resolve as they would on the command line.
//
// Cancellation is best-effort: krusty.Kustomizer.Run does not accept a
// context, so we honor ctx by checking ctx.Err() before invocation. A
// cancellation that fires mid-build is observed only after Run returns;
// for bundle overlays Run completes in milliseconds, so this is acceptable.
//
// Returns ErrCodeInternal on kustomize build or YAML marshal failure;
// ErrCodeTimeout if the context is canceled before invocation.
func buildKustomize(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", err)
	}
	fs := filesys.MakeFsOnDisk()
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rm, err := k.Run(fs, path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "kustomize build failed", err)
	}
	out, err := rm.AsYaml()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "kustomize YAML marshal", err)
	}
	return out, nil
}
