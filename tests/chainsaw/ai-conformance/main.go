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

// ai-conformance-check parses Chainsaw assertion YAML files and verifies that
// every declared resource exists in the target Kubernetes cluster.
//
// Usage:
//
//	go run ./tests/chainsaw/ai-conformance/ [--kubeconfig PATH] [--dir PATH]
package main

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/k8s/client"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

// resourceIdentity holds the minimal fields extracted from each YAML document.
// Fields beyond apiVersion/kind/metadata are silently discarded by the decoder,
// which lets us ignore Chainsaw-specific assertion syntax in status blocks.
type resourceIdentity struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	SourceFile string `yaml:"-"`
}

// qualifiedName returns "namespace/name" for namespaced resources or just "name".
func (r resourceIdentity) qualifiedName() string {
	if r.Metadata.Namespace != "" {
		return r.Metadata.Namespace + "/" + r.Metadata.Name
	}
	return r.Metadata.Name
}

// gvkString returns "apiVersion/Kind" for display.
func (r resourceIdentity) gvkString() string {
	return r.APIVersion + "/" + r.Kind
}

// checkResult holds the outcome of a single resource existence check.
type checkResult struct {
	Resource resourceIdentity
	Exists   bool
	Err      error
}

func main() {
	cmd := &cli.Command{
		Name:  "ai-conformance-check",
		Usage: "Check that all resources from Chainsaw assert files exist in the cluster",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "kubeconfig",
				Aliases: []string{"k"},
				Usage:   "path to kubeconfig file",
				Sources: cli.EnvVars("KUBECONFIG"),
			},
			&cli.StringFlag{
				Name:    "dir",
				Aliases: []string{"d"},
				Value:   "./cluster",
				Usage:   "directory containing assert-*.yaml files",
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Value: 2 * time.Minute,
				Usage: "overall operation timeout",
			},
			&cli.BoolFlag{
				Name:    "debug",
				Usage:   "enable debug logging",
				Sources: cli.EnvVars("DEBUG"),
			},
		},
		Action: run,
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		exitCode := errors.ExitCodeFromError(err)
		slog.Error("check failed", "error", err, "exitCode", exitCode)
		os.Exit(exitCode)
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	if cmd.Bool("debug") {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	ctx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
	defer cancel()

	// Parse YAML files.
	dir := cmd.String("dir")
	resources, err := parseAssertFiles(dir)
	if err != nil {
		return err
	}
	slog.Info("parsed assert files", "resources", len(resources), "dir", dir)

	// Build K8s clients.
	kubeconfig := cmd.String("kubeconfig")
	clientset, restConfig, err := client.BuildKubeClient(kubeconfig)
	if err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable, "failed to create kubernetes client", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create dynamic client", err)
	}

	// Build GVK-to-GVR mapping (single discovery call).
	gvkToGVR, err := buildGVKToGVRMap(clientset.Discovery())
	if err != nil {
		return err
	}
	slog.Debug("built GVK-to-GVR map", "entries", len(gvkToGVR))

	// Check all resources in parallel.
	results := checkResources(ctx, dynClient, resources, gvkToGVR)

	return printResults(results)
}

// parseAssertFiles reads all assert-*.yaml files in dir and returns the
// resource identities declared in them.
func parseAssertFiles(dir string) ([]resourceIdentity, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to read directory %s", dir), err)
	}

	var resources []resourceIdentity
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "assert-") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		path := filepath.Join(dir, name)
		parsed, err := parseYAMLFile(path, name)
		if err != nil {
			return nil, err
		}
		resources = append(resources, parsed...)
	}

	if len(resources) == 0 {
		return nil, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("no resources found in assert-*.yaml files under %s", dir))
	}
	return resources, nil
}

// parseYAMLFile decodes a multi-document YAML file into resource identities.
func parseYAMLFile(path, sourceFile string) ([]resourceIdentity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to open %s", path), err)
	}
	defer f.Close()

	var resources []resourceIdentity
	decoder := yaml.NewDecoder(f)
	for {
		var res resourceIdentity
		if err := decoder.Decode(&res); err != nil {
			if stderrors.Is(err, io.EOF) {
				break
			}
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to parse %s", sourceFile), err)
		}
		if res.APIVersion == "" || res.Kind == "" || res.Metadata.Name == "" {
			continue
		}
		res.SourceFile = sourceFile
		resources = append(resources, res)
	}
	return resources, nil
}

// buildGVKToGVRMap uses the discovery client to build a complete mapping from
// GroupVersionKind to GroupVersionResource. A single ServerPreferredResources
// call is made to avoid repeated API server round-trips.
func buildGVKToGVRMap(disc discovery.DiscoveryInterface) (map[schema.GroupVersionKind]schema.GroupVersionResource, error) {
	apiResourceLists, err := disc.ServerPreferredResources()
	if err != nil && len(apiResourceLists) == 0 {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "API discovery failed", err)
	}
	if err != nil {
		slog.Warn("partial API discovery error (continuing)", "error", err)
	}

	gvkToGVR := make(map[schema.GroupVersionKind]schema.GroupVersionResource)
	for _, list := range apiResourceLists {
		if list == nil {
			continue
		}
		gv, parseErr := schema.ParseGroupVersion(list.GroupVersion)
		if parseErr != nil {
			slog.Debug("skipping unparsable group version", "groupVersion", list.GroupVersion, "error", parseErr)
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") {
				continue // skip subresources
			}
			gvk := gv.WithKind(r.Kind)
			gvr := gv.WithResource(r.Name)
			gvkToGVR[gvk] = gvr
		}
	}
	return gvkToGVR, nil
}

// checkResources verifies existence of every resource in parallel using the
// dynamic client. Each goroutine writes to its own index in the pre-allocated
// results slice, so no mutex is needed.
func checkResources(
	ctx context.Context,
	dynClient dynamic.Interface,
	resources []resourceIdentity,
	gvkToGVR map[schema.GroupVersionKind]schema.GroupVersionResource,
) []checkResult {

	results := make([]checkResult, len(resources))
	g, gctx := errgroup.WithContext(ctx)

	for i, res := range resources {
		g.Go(func() error {
			results[i] = checkSingleResource(gctx, dynClient, res, gvkToGVR)
			return nil // never fail the group; results are per-resource
		})
	}

	_ = g.Wait()
	return results
}

// checkSingleResource performs a single GET to verify resource existence.
func checkSingleResource(
	ctx context.Context,
	dynClient dynamic.Interface,
	res resourceIdentity,
	gvkToGVR map[schema.GroupVersionKind]schema.GroupVersionResource,
) checkResult {

	gv, err := schema.ParseGroupVersion(res.APIVersion)
	if err != nil {
		return checkResult{Resource: res, Err: errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid apiVersion %q", res.APIVersion), err)}
	}

	gvk := gv.WithKind(res.Kind)
	gvr, ok := gvkToGVR[gvk]
	if !ok {
		return checkResult{Resource: res, Err: errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("unknown resource type %s", gvk))}
	}

	var rc dynamic.ResourceInterface
	if res.Metadata.Namespace != "" {
		rc = dynClient.Resource(gvr).Namespace(res.Metadata.Namespace)
	} else {
		rc = dynClient.Resource(gvr)
	}

	_, err = rc.Get(ctx, res.Metadata.Name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return checkResult{Resource: res, Exists: false}
		}
		return checkResult{Resource: res, Err: errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to get %s %s", res.Kind, res.qualifiedName()), err)}
	}
	return checkResult{Resource: res, Exists: true}
}

// printResults writes a grouped summary to stdout and returns an error if any
// resources are missing or checks failed.
func printResults(results []checkResult) error {
	// Group by source file, preserving insertion order.
	type fileGroup struct {
		name    string
		results []checkResult
	}
	seen := make(map[string]int) // file -> index in groups
	var groups []fileGroup

	for _, r := range results {
		idx, ok := seen[r.Resource.SourceFile]
		if !ok {
			idx = len(groups)
			seen[r.Resource.SourceFile] = idx
			groups = append(groups, fileGroup{name: r.Resource.SourceFile})
		}
		groups[idx].results = append(groups[idx].results, r)
	}

	var passed, failed, errored int

	fmt.Println("AI-Conformance Resource Existence Check")
	fmt.Println("========================================")

	for _, g := range groups {
		fmt.Printf("\nSource: %s\n", g.name)
		for _, r := range g.results {
			kind := r.Resource.gvkString()
			qname := r.Resource.qualifiedName()

			switch {
			case r.Err != nil:
				fmt.Printf("  ERROR  %-40s %-45s (%s)\n", kind, qname, r.Err)
				errored++
			case r.Exists:
				fmt.Printf("  PASS   %-40s %s\n", kind, qname)
				passed++
			default:
				fmt.Printf("  FAIL   %-40s %-45s (not found)\n", kind, qname)
				failed++
			}
		}
	}

	fmt.Println("\n========================================")
	fmt.Printf("Results: %d passed, %d failed, %d errors (%d total)\n",
		passed, failed, errored, len(results))

	if failed > 0 || errored > 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("%d resources not found, %d errors", failed, errored))
	}
	return nil
}
