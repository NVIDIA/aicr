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

// Command cleanup removes AICR-installed resources from the cluster of the
// current kubeconfig context, in four phases:
//
//  1. Helm releases of AICR components in AICR component namespaces
//  2. aicr validate and snapshot-agent artifacts
//  3. Custom Resource Definitions in AICR-owned API groups
//  4. AICR component and check-created namespaces, with stuck-finalizer rescue
//
// A release is AICR's when pkg/inventory attributes it to a component under
// any deployer's naming, when it carries the AICR stamp, or when a bundle
// writes its name for a registry or legacyComponents component. With
// --bundle, phases 1 and 4 cover only what that bundle's bundle-info.yaml
// records.
//
// It leaves cluster-scoped resources outside AICR components alone. Do not run
// it during an aicr snapshot or aicr validate run, since phase 2 also deletes
// that run's objects.
//
// Usage:
//
//	go run ./tools/cleanup [flags]
package main

import (
	"bufio"
	"cmp"
	"context"
	stderrors "errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/inventory"
	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/logging"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const usageHeader = `Usage: go run ./tools/cleanup [flags]

Removes AICR-installed Kubernetes resources from the current kubeconfig context.
Exclusions fence out installs AICR does not own, for example nodewright
installed out-of-band:

  go run ./tools/cleanup --exclude-ns skyhook --exclude-crd skyhook.nvidia.com,nodewright.nvidia.com

--bundle limits helm uninstall and namespace deletion to what one bundle
installed, for example on a cluster shared with another tenant's bundle.
CRDs in AICR's API groups are still deleted cluster-wide, along with the
other tenant's custom resources, unless --keep-crds is set:

  go run ./tools/cleanup --bundle ./bundles --keep-crds

Flags:
`

type options struct {
	dryRun         bool
	assumeYes      bool
	keepCRDs       bool
	keepNamespaces bool
	excludeNS      []string
	excludeCRD     []string
	bundleDir      string
}

func main() {
	logging.SetDefaultCLILogger("info")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, slog.Default())
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, log *slog.Logger) int {
	opts, err := parseOptions(args, stderr)
	if stderrors.Is(err, flag.ErrHelp) {
		return errors.ExitSuccess
	}
	if err != nil {
		log.Error("invalid arguments", "error", err)
		return errors.ExitCodeFromError(err)
	}

	c, err := newCleaner(ctx, opts, log)
	if err != nil {
		log.Error("cleanup could not start", "error", err)
		return errors.ExitCodeFromError(err)
	}
	if err := c.run(ctx, newPrompt(stdin, stdout)); err != nil {
		log.Error("cleanup failed", "error", err)
		return errors.ExitCodeFromError(err)
	}
	return errors.ExitSuccess
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageHeader)
		fs.PrintDefaults()
	}

	fs.BoolVar(&opts.dryRun, "dry-run", false, "log what would be removed without changing the cluster")
	fs.BoolVar(&opts.dryRun, "n", false, "shorthand for --dry-run")
	fs.BoolVar(&opts.assumeYes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&opts.assumeYes, "y", false, "shorthand for --yes")
	fs.BoolVar(&opts.keepCRDs, "keep-crds", false, "keep CRDs, e.g. when reinstalling the same version")
	fs.BoolVar(&opts.keepNamespaces, "keep-namespaces", false, "skip namespace deletion")
	fs.Func("exclude-ns", "protect a namespace from helm uninstall and namespace deletion (repeatable, comma-separated)",
		csvAppender(&opts.excludeNS, "a namespace"))
	fs.Func("exclude-crd", "protect CRDs whose name contains this match from deletion (repeatable, comma-separated)",
		csvAppender(&opts.excludeCRD, "a match"))
	fs.Func("bundle", "limit helm uninstall and namespace deletion to the releases and namespaces this bundle directory's bundle-info.yaml records",
		func(value string) error {
			if value == "" || strings.HasPrefix(value, "-") {
				return errors.New(errors.ErrCodeInvalidRequest, "requires a bundle directory argument, got "+value)
			}
			opts.bundleDir = value
			return nil
		})

	if err := fs.Parse(args); err != nil {
		if stderrors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid flags", err)
	}
	if fs.NArg() > 0 {
		return opts, errors.New(errors.ErrCodeInvalidRequest, "unexpected argument: "+fs.Arg(0))
	}
	return opts, nil
}

// csvAppender returns a flag.Func handler that appends each non-empty, trimmed
// comma-separated token of the value to dst. It rejects a value starting with
// "-", which is the next flag swallowed by a missing argument, and names what
// in the error.
func csvAppender(dst *[]string, what string) func(string) error {
	return func(value string) error {
		if strings.HasPrefix(value, "-") {
			return errors.New(errors.ErrCodeInvalidRequest, "requires "+what+" argument, got "+value)
		}
		for tok := range strings.SplitSeq(value, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				*dst = append(*dst, tok)
			}
		}
		return nil
	}
}

func newCleaner(ctx context.Context, opts options, log *slog.Logger) (*cleaner, error) {
	kube, restConfig, err := client.GetKubeClient()
	if err != nil {
		return nil, err
	}
	dyn, err := client.NewDynamicClientForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	bounded, err := kubernetes.NewForConfig(client.BoundedConfig(restConfig))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create discovery client", err)
	}
	namespaces, err := ownedNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "load component registry", err)
	}

	if opts.dryRun {
		log = log.With("dryRun", true)
	}
	kubeconfig, mergedKubeconfig, kubeContext := kubeTarget()
	c := &cleaner{
		opts:          opts,
		log:           log,
		kubeContext:   cmp.Or(kubeContext, "unknown"),
		server:        restConfig.Host,
		kube:          kube,
		disc:          bounded.Discovery(),
		dyn:           dyn,
		namespaces:    namespaces,
		owners:        ownership{names: ownedReleaseNames(registry)},
		readReleases:  inventory.ReadReleases,
		components:    inventoryComponents(registry),
		namespaceWait: defaults.ClusterCleanupNamespaceDeleteWait,
	}
	if opts.bundleDir != "" {
		b, bundleErr := loadBundle(ctx, opts.bundleDir)
		if bundleErr != nil {
			return nil, bundleErr
		}
		c.bundle, c.namespaces, c.owners, c.readReleases = &b, b.namespaces, b.owners, nil
	}
	if _, err := exec.LookPath(helm.Binary); err == nil {
		// Pinned because helm otherwise resolves its own target, for example
		// from HELM_KUBECONTEXT. With neither flag, helm falls back to the
		// same in-cluster config or KUBECONFIG merge the clients use.
		c.helm = helmCLI{run: helm.Run, kubeArgs: helmKubeArgs(kubeconfig, kubeContext)}
		if err := exportHelmTarget(mergedKubeconfig); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// kubeTarget returns the kubeconfig the clients load and its current context.
// It sets path for a single file and merged for a multi-file KUBECONFIG, and
// neither for in-cluster config. kubeContext is empty when none can be read.
func kubeTarget() (path string, merged []string, kubeContext string) {
	rules := client.KubeconfigLoadingRules("")
	if rules == nil {
		return "", nil, ""
	}
	if rules.ExplicitPath == "" {
		merged = rules.Precedence
	}
	return rules.ExplicitPath, merged, contextName(clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}))
}

const helmAPIServerEnv = "HELM_KUBEAPISERVER"

// exportHelmTarget sets the environment helm inherits so it targets the same
// API server as the clients. It unsets HELM_KUBEAPISERVER, which would
// override the server of any kubeconfig or in-cluster config. It sets
// KUBECONFIG to the files merged lists, because helm splits KUBECONFIG
// without trimming its entries, and leaves it unchanged when merged is empty.
func exportHelmTarget(merged []string) error {
	if err := os.Unsetenv(helmAPIServerEnv); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "unset "+helmAPIServerEnv+" for helm", err)
	}
	if len(merged) == 0 {
		return nil
	}
	if err := os.Setenv("KUBECONFIG", strings.Join(merged, string(filepath.ListSeparator))); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "set KUBECONFIG for helm", err)
	}
	return nil
}

// contextName returns cfg's current context, or "" when it has none or cannot
// be read.
func contextName(cfg clientcmd.ClientConfig) string {
	raw, err := cfg.RawConfig()
	if err != nil {
		return ""
	}
	return raw.CurrentContext
}

// helmKubeArgs returns the helm flags selecting kubeconfig and kubeContext,
// omitting each that is empty.
func helmKubeArgs(kubeconfig, kubeContext string) []string {
	var args []string
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	return args
}

// A prompt asks the operator question and reports whether they answered yes.
// It reports false once ctx is done.
type prompt func(ctx context.Context, question string) bool

func newPrompt(in io.Reader, out io.Writer) prompt {
	reader := bufio.NewReader(in)
	return func(ctx context.Context, question string) bool {
		fmt.Fprintf(out, "%s (y/N): ", question)
		// A read from in cannot be canceled, so on ctx done the goroutine
		// stays blocked until the process exits.
		answers := make(chan string, 1)
		go func() {
			answer, _ := reader.ReadString('\n')
			answers <- answer
		}()
		select {
		case <-ctx.Done():
			fmt.Fprintln(out)
			return false
		case answer := <-answers:
			answer = strings.TrimSpace(answer)
			return answer == "y" || answer == "Y"
		}
	}
}
