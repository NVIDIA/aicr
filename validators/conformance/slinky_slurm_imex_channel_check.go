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
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	slinkySlurmIMEXComputeDomainName         = "slinky-slurm-imex"
	slinkySlurmIMEXResourceClaimTemplateName = "slinky-slurm-imex-channels"
	slinkySlurmIMEXChannelPrefix             = "/dev/nvidia-caps-imex-channels/channel"

	// slinkySlurmIMEXChannelShell runs two overlapping but bounded Slurm jobs.
	// Each direct srun requests one GPU and a small CPU/memory footprint so the
	// two allocations can run concurrently without reserving the whole node.
	// --immediate prevents an unschedulable job from remaining pending; --time
	// bounds each allocation even if the shell or channel check misbehaves.
	slinkySlurmIMEXChannelShell = `run() {
  srun \
    --immediate=30 \
    --time=1:00 \
    --nodes=1 \
    --ntasks=1 \
    --cpus-per-task=1 \
    --mem=128M \
    --gres=gpu:1 \
    /bin/sh -c '
    channel=$(find /dev/nvidia-caps-imex-channels -maxdepth 1 -type c -name "channel*" -print) || {
      printf "IMEX_CHANNEL_ERROR=find failed\n" >&2
      exit 1
    }
    if test -n "$channel"; then
      channel_count=$(printf "%s\n" "$channel" | wc -l)
    else
      channel_count=0
    fi
    printf "IMEX_CHANNEL_COUNT=%s\n" "$channel_count" >&2
    printf "IMEX_CHANNEL_CANDIDATES:\n%s\n" "$channel" >&2
    if test "$channel_count" -ne 1; then
      printf "IMEX_CHANNEL_ERROR=expected exactly one channel\n" >&2
      exit 1
    fi
    printf "IMEX_CHANNEL=%s\n" "$channel"
    # Hold longer than --immediate so two successful jobs must overlap.
    sleep 40
  '
}
run & first=$!
run & second=$!
wait "$first"; first_rc=$?
wait "$second"; second_rc=$?
test "$first_rc" -eq 0 && test "$second_rc" -eq 0`
)

var slinkySlurmIMEXComputeDomainGVR = schema.GroupVersionResource{
	Group:    "resource.nvidia.com",
	Version:  versionV1beta1,
	Resource: "computedomains",
}

// CheckSlinkySlurmIMEXChannel verifies that two concurrent Slurm jobs
// each receive a distinct NVIDIA IMEX channel. IMEX-capable recipes opt in by
// selecting this check explicitly, without coupling it to a hardware name.
func CheckSlinkySlurmIMEXChannel(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	if ctx.RESTConfig == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "RESTConfig is not available")
	}
	if ctx.ValidationInput == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "validation is not available")
	}
	if !recipeHasComponent(ctx, slinkySlurmComponent) {
		return validators.Skip("slinky-slurm component not present in recipe")
	}

	namespace := resolveSlinkySlurmNamespace(ctx)
	if err := discoverSlinkySetAPIs(ctx); err != nil {
		return err
	}
	if err := skipIfAllNodeSetPodsAreKWOK(ctx, namespace); err != nil {
		return err
	}
	if err := requireSlinkySlurmIMEXResources(ctx, namespace); err != nil {
		return err
	}

	loginPod, err := findReadySlinkyLoginPod(ctx, namespace)
	if err != nil {
		return err
	}
	result, execErr := slinkyExecCommand(
		ctx.Ctx,
		ctx,
		namespace,
		loginPod.Name,
		[]string{"/bin/sh", "-c", slinkySlurmIMEXChannelShell},
		slinkyLoginPodExecOptions,
	)
	recordSlinkySlurmIMEXResult(ctx, namespace, loginPod.Name, result, execErr)
	if execErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to run concurrent Slinky Slurm IMEX jobs", execErr)
	}
	if result.ExitCode != 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("concurrent Slinky Slurm IMEX jobs: exit code %d", result.ExitCode))
	}

	channels, err := parseSlinkySlurmIMEXChannels(result.Stdout)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Slinky Slurm IMEX channels", "",
		fmt.Sprintf("first=%s\nsecond=%s", channels[0], channels[1]))
	return nil
}

func requireSlinkySlurmIMEXResources(ctx *validators.Context, namespace string) error {
	dynamicClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	group, groupCtx := errgroup.WithContext(ctx.Ctx)
	group.Go(func() error {
		_, getErr := dynamicClient.Resource(slinkySlurmIMEXComputeDomainGVR).Namespace(namespace).Get(
			groupCtx,
			slinkySlurmIMEXComputeDomainName,
			metav1.GetOptions{},
		)
		if getErr != nil {
			return slinkySlurmIMEXResourceLookupError(
				"ComputeDomain", namespace, slinkySlurmIMEXComputeDomainName, getErr)
		}
		return nil
	})
	group.Go(func() error {
		_, getErr := ctx.Clientset.ResourceV1().ResourceClaimTemplates(namespace).Get(
			groupCtx,
			slinkySlurmIMEXResourceClaimTemplateName,
			metav1.GetOptions{},
		)
		if getErr != nil {
			return slinkySlurmIMEXResourceLookupError(
				"ResourceClaimTemplate", namespace, slinkySlurmIMEXResourceClaimTemplateName, getErr)
		}
		return nil
	})

	if err := group.Wait(); err != nil {
		return err
	}
	return nil
}

func slinkySlurmIMEXResourceLookupError(kind, namespace, name string, err error) error {
	code := errors.ErrCodeInternal
	if apierrors.IsNotFound(err) {
		code = errors.ErrCodeNotFound
	}
	return errors.Wrap(code, fmt.Sprintf("failed to get %s %s/%s", kind, namespace, name), err)
}

func parseSlinkySlurmIMEXChannels(stdout string) ([2]string, error) {
	channels := make([]string, 0, 2)
	for _, line := range strings.Split(stdout, "\n") {
		channel, found := strings.CutPrefix(strings.TrimSpace(line), "IMEX_CHANNEL=")
		if !found {
			continue
		}
		channel = strings.TrimSpace(channel)
		suffix := strings.TrimPrefix(channel, slinkySlurmIMEXChannelPrefix)
		if suffix == "" || suffix == channel || strings.Contains(suffix, "/") {
			return [2]string{}, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("invalid IMEX channel path %q", channel))
		}
		channels = append(channels, channel)
	}
	if len(channels) != 2 {
		return [2]string{}, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("expected two IMEX channels, got %d", len(channels)))
	}
	if channels[0] == channels[1] {
		return [2]string{}, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("concurrent Slurm jobs received the same IMEX channel %q", channels[0]))
	}
	return [2]string{channels[0], channels[1]}, nil
}

func recordSlinkySlurmIMEXResult(
	ctx *validators.Context,
	namespace, podName string,
	result podExecResult,
	execErr error,
) {

	var body strings.Builder
	fmt.Fprintf(&body, "Pod:      %s/%s\n", namespace, podName)
	fmt.Fprintf(&body, "Command:  /bin/sh -c %s\n", slinkySlurmIMEXChannelShell)
	fmt.Fprintf(&body, "ExitCode: %d\n", result.ExitCode)
	if execErr != nil {
		fmt.Fprintf(&body, "Error:    %v\n", execErr)
	}
	fmt.Fprintf(&body, "\nstdout:\n%s\n\nstderr:\n%s\n", result.Stdout, result.Stderr)

	recordRawTextArtifact(ctx, "Slinky Slurm IMEX channel result",
		fmt.Sprintf("kubectl exec -n %s %s -- /bin/sh -c '<bounded concurrent srun command>'", namespace, podName),
		body.String())
}
