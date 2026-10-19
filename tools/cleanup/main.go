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
	"context"
	stderrors "errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/logging"
)

const usageHeader = `Usage: go run ./tools/cleanup [flags]

Removes AICR-installed Kubernetes resources from the current kubeconfig context.
Exclusions fence out installs AICR does not own, for example nodewright
installed out-of-band:

  go run ./tools/cleanup --exclude-ns skyhook --exclude-crd skyhook.nvidia.com,nodewright.nvidia.com

Flags:
`

type options struct {
	dryRun         bool
	assumeYes      bool
	keepCRDs       bool
	keepNamespaces bool
	excludeNS      []string
	excludeCRD     []string
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
	fs.BoolVar(&opts.assumeYes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&opts.keepCRDs, "keep-crds", false, "keep CRDs, e.g. when reinstalling the same version")
	fs.BoolVar(&opts.keepNamespaces, "keep-namespaces", false, "skip namespace deletion")
	fs.Func("exclude-ns", "protect a namespace from helm uninstall and namespace deletion (repeatable, comma-separated)",
		csvAppender(&opts.excludeNS, "a namespace"))
	fs.Func("exclude-crd", "protect CRDs whose name contains this match from deletion (repeatable, comma-separated)",
		csvAppender(&opts.excludeCRD, "a match"))

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
	releaseNames, err := ownedReleaseNames()
	if err != nil {
		return nil, err
	}

	if opts.dryRun {
		log = log.With("dryRun", true)
	}
	c := &cleaner{
		opts:          opts,
		log:           log,
		kubeContext:   currentContext(),
		server:        restConfig.Host,
		kube:          kube,
		disc:          bounded.Discovery(),
		dyn:           dyn,
		namespaces:    namespaces,
		releaseNames:  releaseNames,
		namespaceWait: defaults.ClusterCleanupNamespaceDeleteWait,
	}
	if _, err := exec.LookPath(helm.Binary); err == nil {
		c.helm = helmCLI{run: helm.Run}
	}
	return c, nil
}

// currentContext returns the current context of the kubeconfig the clients
// load, for display only.
func currentContext() string {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path := client.ResolveKubeconfigPath(""); path != "" {
		rules.ExplicitPath = path
	}
	return contextName(clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}))
}

// contextName returns cfg's current context, or "unknown" when it has none or
// cannot be read.
func contextName(cfg clientcmd.ClientConfig) string {
	raw, err := cfg.RawConfig()
	if err != nil || raw.CurrentContext == "" {
		return "unknown"
	}
	return raw.CurrentContext
}

// A prompt asks the operator question and reports whether they answered yes.
type prompt func(question string) bool

func newPrompt(in io.Reader, out io.Writer) prompt {
	reader := bufio.NewReader(in)
	return func(question string) bool {
		fmt.Fprintf(out, "%s (y/N): ", question)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(answer)
		return answer == "y" || answer == "Y"
	}
}
