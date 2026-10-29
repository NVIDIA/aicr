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

// Package plugin implements the kubectl aicr plugin: describe, approve,
// acknowledge and revoke rollouts (api.md#approval), and reproduce a plan's
// bundle with the
// AICR CLI. The approver's username comes from the API server, as `kubectl
// auth whoami` reports it, never from a flag.
package plugin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/lifecycle"
)

// Usage is the plugin's help text.
const Usage = `Usage: kubectl aicr COMMAND ROLLOUT [flags]
       kubectl aicr reproduce [PLAN] [flags]

Commands:
  describe   steps, verdicts, CRD changes, the assessment report, approval and drift
  approve    approve the rollout as yourself (--reason, --expires)
  acknowledge
             acknowledge, as yourself, that the post-apply steps of the
             release the rollout waits on are done
  revoke     revoke its approval as yourself (--reason)
  reproduce  write a plan's recipe and print the AICR CLI command that renders
             the same bundle (PLAN: name or digest; default the active plan)

Flags:
  --kubeconfig PATH            kubeconfig file (default: $KUBECONFIG or ~/.kube/config)
  --context NAME               kubeconfig context
  --operator-namespace NAME    where assessment reports live (default aicr-system)
  --reason TEXT                why (approve, revoke; required)
  --expires DURATION           how long the approval stays valid (approve; default 2h)
  --dir PATH                   where reproduce writes the recipe (default aicr-reproduce)
`

// Commands.
const (
	commandDescribe    = "describe"
	commandApprove     = "approve"
	commandAcknowledge = "acknowledge"
	commandRevoke      = "revoke"
	commandReproduce   = "reproduce"
)

// ErrUsage reports a command line the plugin cannot run.
var ErrUsage = errors.New("usage")

// Run executes one plugin command and writes its result to out.
func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return usage("a command is required")
	}
	command := args[0]
	fs := flag.NewFlagSet("kubectl aicr "+command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	kubeconfig := fs.String("kubeconfig", "", "")
	kubeContext := fs.String("context", "", "")
	namespace := fs.String("operator-namespace", "aicr-system", "")
	reason := fs.String("reason", "", "")
	expires := fs.Duration("expires", defaults.ApprovalLifetime, "")
	dir := fs.String("dir", "aicr-reproduce", "")
	switch command {
	case commandDescribe, commandApprove, commandAcknowledge, commandRevoke, commandReproduce:
	case "help", "-h", "--help":
		_, err := io.WriteString(out, Usage)
		return err
	default:
		return usage("unknown command " + command)
	}
	// Flags may come before or after the name.
	if err := fs.Parse(args[1:]); err != nil {
		return usage(err.Error())
	}
	if fs.NArg() == 0 && command != commandReproduce {
		return usage(command + " needs a rollout name")
	}
	name := fs.Arg(0)
	if fs.NArg() > 0 {
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return usage(err.Error())
		}
	}
	if fs.NArg() != 0 {
		return usage("unexpected arguments after " + name)
	}
	if (command == commandApprove || command == commandRevoke) && *reason == "" {
		return usage(command + " needs --reason")
	}
	if *expires <= 0 {
		return usage("--expires must be positive")
	}

	c, err := newClient(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.PluginTimeout)
	defer cancel()
	switch command {
	case commandDescribe:
		return lifecycle.Describe(ctx, c, *namespace, name, out)
	case commandReproduce:
		return reproduce(ctx, c, *namespace, name, *dir, out)
	}
	user, err := lifecycle.WhoAmI(ctx, c)
	if err != nil {
		return err
	}
	now := time.Now()
	if command == commandAcknowledge {
		ro, step, aErr := lifecycle.Acknowledge(ctx, c, name, user, now)
		if aErr != nil {
			return aErr
		}
		_, err = fmt.Fprintf(out, "rollout %s: %s acknowledged the post-apply steps of %s (%s); the rollout continues\n",
			ro.Name, user, step.Release, strings.Join(step.PostApplySteps, ", "))
		return err
	}
	if command == commandApprove {
		ro, aErr := lifecycle.Approve(ctx, c, name, user, *reason, now.Add(*expires))
		if aErr != nil {
			return aErr
		}
		_, err = fmt.Fprintf(out, "rollout %s approved by %s until %s\n", ro.Name, user, ro.Spec.Approval.ExpiresAt.UTC().Format(time.RFC3339))
		return err
	}
	ro, err := lifecycle.Revoke(ctx, c, name, user, *reason, now)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "rollout %s revoked by %s; the step in flight finishes and no further step starts\n", ro.Name, user)
	return err
}

func usage(msg string) error {
	return fmt.Errorf("%w: %s\n\n%s", ErrUsage, msg, Usage)
}

// newClient loads the kubeconfig as kubectl does.
func newClient(kubeconfig, kubeContext string) (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInvalidRequest, "load kubeconfig", err)
	}
	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err == nil {
		err = v1alpha1.AddToScheme(scheme)
	}
	if err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "register API types", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeUnavailable, "create client", err)
	}
	return c, nil
}
