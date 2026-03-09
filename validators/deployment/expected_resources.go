// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/chainsaw"
	"github.com/NVIDIA/aicr/validators/helper"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
)

// checkExpectedResources verifies that all expected Kubernetes resources declared
// in the recipe's componentRefs exist and are healthy in the live cluster.
func checkExpectedResources(ctx *validators.Context) error {
	if ctx.Recipe == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "recipe is not available")
	}

	var chainsawAsserts []chainsaw.ComponentAssert
	var failures []string

	for _, ref := range ctx.Recipe.ComponentRefs {
		if ref.HealthCheckAsserts != "" && len(ref.ExpectedResources) == 0 {
			chainsawAsserts = append(chainsawAsserts, chainsaw.ComponentAssert{
				Name:       ref.Name,
				AssertYAML: ref.HealthCheckAsserts,
			})
			continue
		}

		for _, er := range ref.ExpectedResources {
			if err := helper.VerifyResource(ctx.Ctx, ctx.Clientset, er); err != nil {
				failures = append(failures, fmt.Sprintf("%s %s/%s (%s): %s",
					er.Kind, er.Namespace, er.Name, ref.Name, err.Error()))
			} else {
				fmt.Printf("  %s %s/%s: healthy\n", er.Kind, er.Namespace, er.Name)
			}
		}
	}

	if len(chainsawAsserts) > 0 {
		slog.Info("running health check assertions", "components", len(chainsawAsserts))
		fetcher, fetcherErr := buildResourceFetcher(ctx)
		if fetcherErr != nil {
			return fetcherErr
		}
		results := chainsaw.Run(ctx.Ctx, chainsawAsserts, defaults.ChainsawAssertTimeout, fetcher,
			chainsaw.WithChainsawBinary(chainsaw.NewChainsawBinary()))
		for _, r := range results {
			if r.Passed {
				fmt.Printf("  %s: chainsaw health check passed\n", r.Component)
			} else {
				msg := fmt.Sprintf("%s: chainsaw health check failed", r.Component)
				if r.Output != "" {
					msg += fmt.Sprintf(":\n%s", r.Output)
				}
				if r.Error != nil {
					msg += fmt.Sprintf("\nerror: %v", r.Error)
				}
				failures = append(failures, msg)
			}
		}
	}

	if len(failures) > 0 {
		fmt.Println("Failed resources:")
		for _, f := range failures {
			fmt.Printf("  %s\n", f)
		}
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("expected resource check failed: %d issue(s):\n  %s",
				len(failures), strings.Join(failures, "\n  ")))
	}

	fmt.Println("All expected resources are healthy")
	return nil
}

func buildResourceFetcher(ctx *validators.Context) (chainsaw.ResourceFetcher, error) {
	if ctx.RESTConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "no kubernetes client configuration available")
	}

	discoveryClient, err := kubernetes.NewForConfig(ctx.RESTConfig)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create discovery client", err)
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(
		memory.NewMemCacheClient(discoveryClient.Discovery()),
	)

	return chainsaw.NewClusterFetcher(ctx.DynamicClient, mapper), nil
}
