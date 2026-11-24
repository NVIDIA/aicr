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
	"bufio"
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v3"

	aicr "aicr.run/pkg/client/v1"
	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/serializer"
)

// sweepCmd creates the "sweep" CLI command.
func sweepCmd() *cli.Command {
	return &cli.Command{
		Name:     "sweep",
		Category: functionalCategoryName,
		Usage:    "Delete stale per-run validation resources a crashed or --no-cleanup run left behind",
		Description: `Delete per-run resources a validation run leaves behind when its own cleanup
never runs: a SIGKILL, OOM kill or node loss skips every deferred cleanup, and
--no-cleanup runs keep theirs deliberately. Each family is selected by label
AND shape, never by label alone, and every delete is pinned to the UID seen at
planning time, so an object recreated in between is left alone.

What is swept:
  - per-run conformance test namespaces: aicr-gpu-test-*,
    gang-scheduling-test-*, hpa-test-*, ai-metrics-test-* and
    gateway-traffic-* (32-hex run token), cluster-auto-test-* (8-hex
    token), each carrying the aicr.run/gpu-test-run label whose value must
    reconstruct the namespace name;
  - gang-scheduling pods and podgroups.scheduling.run.ai left in a Run:ai
    Project namespace — the namespace itself is never touched, and a
    namespace carrying the runai/queue label is never a candidate;
  - run-scoped objects in --namespace: the aicr-validator-<run-id>
    ServiceAccount and its cluster-admin ClusterRoleBinding (name, roleRef
    and single subject must all match what a run writes), the ConfigMaps a
    run writes for its run ID (exactly aicr-snapshot-<run-id>,
    aicr-validation-<run-id> or aicr-ctrf-<run-id>-<phase>), and
    namespaced snapshot-agent RBAC carrying component=snapshot-agent and
    an aicr.run/run-id label, its name ending in -<run-id>;
  - the snapshot agent's ClusterRole and ClusterRoleBinding, exactly
    aicr-node-reader-<run-id> with the same labels, matched regardless of
    --namespace: an aged run in another namespace loses this pair while its
    namespaced RBAC there is kept. component=agent-rbac is operator-owned
    and never matched.

A candidate must ALSO be older than --older-than (default 2h, above the
facade's whole-run cap and the largest single-check envelope, which is what
bounds a test namespace's life) and pass a liveness guard. Below the
default floor a namespace is kept while any non-terminal pod runs in it;
at or above the floor that guard no longer applies, because an aged
namespace cannot belong to a live check — the floor is pinned above every
embedded check's wait envelope; keep the 2h default for a --data catalog that
raises a check's timeout unless that timeout plus 4m5s exceeds it, then
pass --older-than above that sum — and the
workloads a killed run leaks never go
terminal (a RestartPolicyAlways Deployment's pods keep Running, gang
members the scheduler holds Pending), so the namespace is deleted with its pods.
Run:ai gang residue follows the same rule: below the default floor a pod or
PodGroup is kept while any gang pod of its run token is non-terminal.
Run-scoped objects are kept while a validator Job carrying their run ID
exists in ANY namespace (a run started with -n another-ns owns
cluster-scoped agent RBAC that this sweep would otherwise orphan). A --no-cleanup run's retained Jobs keep its
objects guarded until the Job TTL controller reaps them (about an hour
after they finish): its objects become eligible once the Jobs have expired
AND they are older than the floor, and the sweep never deletes the Jobs
themselves. The floor does not bound a whole uncapped validate run, and a
live run with cleanup on shows no Job between its checks, so an apply
re-plans against the cluster's current state, then — while run-scoped
candidates remain — lists the run-labeled Jobs and watches from the list's
resourceVersion for up to about 5m30s (a conservative margin over the usual
between-checks gap, not a proven bound) for a Job to appear, and re-plans
once more against current state before deleting anything run-scoped;
a run that stays suspended while idle between checks for longer than that
cannot be told from a dead one. An apiserver watch outlasts this window by
far, so a watch that ends before it elapses is an anomaly: the apply aborts
(exit 6) with nothing deleted — re-run. Objects already
terminating are reported with their remaining finalizers and conditions, and
their finalizers are never stripped. With --format json|yaml the report
document owns stdout, so the plan table and the prompt go to stderr.

What is NOT swept: the aicr-validation namespace itself, the snapshot
agent's staging ConfigMap (aicr-agent-snapshot-<run-id> — it carries no
run-id label, so it cannot be tied to a dead run), NCCL and
inference-perf benchmark namespaces, the AIPerf Job, the nvidia-smi verify
pod, robust-controller webhook probes, a Trainer self-install, CRE
Certification objects, and the pod-autoscaling PodMonitor left in the
recipe's Prometheus namespace. That PodMonitor carries the ownership label
but lives in a shared namespace this command never enters; the next
pod-autoscaling run prunes it. The benchmark namespaces carry the run-id
label, but this command selects namespaces only by aicr.run/gpu-test-run:
their run-id label is used only by aicr validate itself, to delete them
after giving up on the check that created them. None of the others carry the
labels this command trusts.

Safety: the kube context, the API server and the exact candidate list are
printed before anything is deleted. On a terminal the command asks for y/N;
--yes skips the prompt. A non-terminal stdin without --yes or --dry-run fails
rather than deleting. A delete answered NotFound means the object was already
gone; a delete refused with a conflict (the object may have been replaced or
modified) is left alone — re-run to retry. Either way a re-run is safe.

RBAC: the caller needs list and delete on namespaces, clusterrolebindings and
clusterroles, cluster-wide list and delete on pods and
podgroups.scheduling.run.ai, cluster-wide list and watch on jobs (the
liveness guard and the settle observation), and list and delete on
serviceaccounts, configmaps, roles and rolebindings in --namespace. A
forbidden list or watch fails the run before anything is deleted.

Exit codes: 0 when nothing failed, dry-run, skipped and terminating objects
included; 1 for flag-parse errors (an unreadable --older-than value or an
unknown flag); 2 for validation errors the command itself raises (a
non-positive --older-than, a duplicated flag, an unknown --format, an
unsupported or malformed output destination or an unwritable output file
(checked before the cluster is touched; ConfigMap write access is not
checked before the apply), or a confirmation that could not be asked);
4 when a list or watch is forbidden; 5 when the operation timed out; 6 when
the apiserver ended the settle watch early — nothing is deleted; re-run; 8
when a delete failed or a list failed for a reason other than permissions (a
forbidden DELETE lands here too: the partial-failure error wraps its
Unauthorized cause, so the reported code is Internal); 9 when the prompt was
declined or the command was interrupted.

Examples:
  # See what a sweep would delete, deleting nothing
  aicr sweep --dry-run

  # Sweep the default validation namespace interactively
  aicr sweep

  # CI: no prompt, JSON for the pipeline
  aicr sweep --yes --format json

  # A custom validation namespace and a tighter age floor
  aicr sweep --namespace my-validation --older-than 4h`,
		Flags:  sweepCmdFlags(),
		Action: runSweepCmd,
	}
}

// sweepCmdFlags returns the flags for the sweep command.
func sweepCmdFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:     flagNamespace,
			Aliases:  []string{"n"},
			Usage:    "Kubernetes namespace holding run-scoped validation resources",
			Sources:  cli.EnvVars("AICR_NAMESPACE"),
			Value:    defaults.ValidationNamespace,
			Category: catDeployment,
		},
		&cli.DurationFlag{
			Name:     "older-than",
			Usage:    "only delete objects older than this (the floor that keeps the sweep from racing a live run)",
			Value:    defaults.ValidationSweepMinAge,
			Category: catInput,
		},
		&cli.BoolFlag{
			Name:     "dry-run",
			Usage:    "list what would be deleted without deleting anything (never prompts)",
			Category: catInput,
		},
		&cli.BoolFlag{
			Name: flagAssumeYes,
			Usage: "skip the interactive confirmation prompt and delete the listed candidates. " +
				"Required in CI: a non-terminal stdin without --yes or --dry-run fails instead of deleting",
			Category: catInput,
		},
		outputFlag(),
		// Table, unlike the document-producing commands' yaml: the report is
		// a list an operator reads before confirming, and machine consumers
		// pass --format json. Mirrors upgrade-check.
		formatFlagDefault(serializer.FormatTable),
		kubeconfigFlag(),
	}
}

// runSweepCmd executes the sweep command: plan (read-only), display, confirm
// unless told not to, then apply.
func runSweepCmd(ctx context.Context, cmd *cli.Command) error {
	if err := validateSingleValueFlags(cmd,
		flagNamespace, "older-than", "dry-run", flagAssumeYes, flagOutput, flagFormat, "kubeconfig"); err != nil {
		return err
	}
	outFormat, err := parseOutputFormat(cmd)
	if err != nil {
		return err
	}
	// Reject an unsupported destination before anything is planned or
	// applied: the table has no ConfigMap representation.
	toConfigMap := strings.HasPrefix(strings.TrimSpace(cmd.String(flagOutput)), serializer.ConfigMapURIScheme)
	if outFormat == serializer.FormatTable && toConfigMap {
		return errors.New(errors.ErrCodeInvalidRequest, "table output does not support ConfigMap destinations")
	}

	olderThan := cmd.Duration("older-than")
	if olderThan <= 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("--older-than must be a positive duration, got %v", olderThan))
	}
	dryRun := cmd.Bool("dry-run")
	assumeYes := cmd.Bool(flagAssumeYes)
	createdOutput, err := checkSweepOutput(cmd.String(flagOutput))
	if err != nil {
		return err
	}
	reportWritten := false
	if createdOutput != "" {
		// The pre-flight created an empty file; an exit that writes no
		// report must not leave it behind.
		defer func() {
			if reportWritten {
				return
			}
			if rmErr := os.Remove(createdOutput); rmErr != nil && !stderrors.Is(rmErr, fs.ErrNotExist) {
				slog.Warn("failed to remove unwritten sweep report file", "path", createdOutput, "error", rmErr)
			}
		}()
	}

	client, err := embeddedClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	opts := aicr.SweepOptions{
		Kubeconfig: cmd.String("kubeconfig"),
		Namespace:  cmd.String(flagNamespace),
		OlderThan:  olderThan,
	}
	plan, err := client.PlanValidationSweep(ctx, opts)
	if err != nil {
		return err
	}
	plan.DryRun = dryRun

	w := cmd.Root().Writer
	candidates := plan.Candidates()
	willApply := !dryRun && len(candidates) > 0
	needPrompt := willApply && !assumeYes

	// The confirmation display — context, server and the exact list — always
	// precedes an apply (prompted or --yes), and is the whole output in
	// table mode. With --format json|yaml the document owns stdout, so the
	// display and the prompt go to the error writer instead.
	display := w
	if outFormat != serializer.FormatTable {
		display = cmd.Root().ErrWriter
		if display == nil {
			display = os.Stderr
		}
	}
	if outFormat == serializer.FormatTable || willApply {
		if err := printSweepPlan(display, plan); err != nil {
			return err
		}
	}

	report := plan
	var applyErr error
	switch {
	case dryRun:
		// Listed, nothing deleted.
	case len(candidates) == 0:
		// Nothing eligible; the table already says why for each object.
	default:
		if needPrompt {
			if err := confirmSweep(cmd.Reader, display, len(candidates), plan.ClusterContext, plan.Server); err != nil {
				return err
			}
		}
		var applied *aicr.SweepReport
		applied, applyErr = client.ApplyValidationSweep(ctx, plan)
		if applied != nil {
			// A nil report means the apply never reached the cluster (the
			// error says why); keep the plan as the report of record.
			report = applied
		}
	}

	// The report is written first: informing and erroring are not
	// alternatives, and the exit code is orthogonal to the report.
	if err := writeSweepReport(ctx, cmd, outFormat, opts.Kubeconfig, report, report != plan); err != nil {
		if applyErr != nil {
			// applyErr first: the exit code reports the apply's failure.
			return stderrors.Join(applyErr, err)
		}
		return err
	}
	reportWritten = true
	return applyErr
}

// checkSweepOutput rejects a malformed cm:// URI or an unwritable --output
// file before the cluster is touched, so a bad destination cannot surface
// only after the apply has deleted. The file is opened without O_TRUNC so a
// run that later fails before writing leaves an existing report intact. It
// returns the file's path when the check created it, so the caller can
// remove it if no report is written.
func checkSweepOutput(output string) (string, error) {
	output = strings.TrimSpace(output)
	switch {
	case output == "" || output == "-" || output == serializer.StdoutURI:
		return "", nil
	case strings.HasPrefix(output, serializer.ConfigMapURIScheme):
		// Constructing the writer parses the URI without contacting the
		// cluster, and reports a malformed one as InvalidRequest.
		_, err := serializer.NewFileWriterOrStdoutWithKubeconfig(serializer.FormatJSON, output, "")
		return "", err
	}
	_, statErr := os.Stat(output)
	created := ""
	if stderrors.Is(statErr, fs.ErrNotExist) {
		created = output
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("cannot write --output %q", output), err)
	}
	if err := f.Close(); err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("cannot write --output %q", output), err)
	}
	return created, nil
}

// printSweepPlan writes the target header and the plan table to w. In table
// mode this is the primary output on the root writer; with --format json|yaml
// it is the pre-apply display, written to the error writer so the document
// owns stdout.
func printSweepPlan(w io.Writer, plan *aicr.SweepReport) error {
	buf := &bytes.Buffer{}
	renderSweepPlan(buf, plan)
	if _, err := fmt.Fprint(w, buf.String()); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write sweep plan", err)
	}
	return nil
}

// renderSweepPlan renders the target header and the plan table into buf.
func renderSweepPlan(buf *bytes.Buffer, plan *aicr.SweepReport) {
	fmt.Fprintf(buf, "Cluster context: %s\n", orDash(plan.ClusterContext))
	fmt.Fprintf(buf, "API server:      %s\n", orDash(plan.Server))
	fmt.Fprintf(buf, "Namespace:       %s\n", plan.Namespace)
	fmt.Fprintf(buf, "Minimum age:     %s\n", plan.OlderThan)
	if plan.DryRun {
		fmt.Fprintln(buf, "Mode:            dry-run (nothing will be deleted)")
	}
	if len(plan.Objects) == 0 {
		fmt.Fprintln(buf, "\nNothing to sweep: no stale validation resources found.")
		return
	}
	fmt.Fprintln(buf)
	writeSweepTable(buf, plan.Objects)
	if len(plan.Candidates()) == 0 && !plan.DryRun {
		fmt.Fprintln(buf, "\nNothing to delete: everything found is too young, guarded, or already terminating.")
	}
}

// writeSweepTable renders one table of sweep objects into buf. Writes to a
// bytes.Buffer cannot fail, so only the tabwriter flush could return an
// error — and it cannot either.
func writeSweepTable(buf *bytes.Buffer, objects []aicr.SweepObject) {
	tw := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNAMESPACE\tNAME\tAGE\tACTION\tREASON")
	for _, o := range objects {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			o.Kind, orDash(o.Namespace), o.Name, o.Age.Round(time.Second), o.Status, orDash(o.Reason))
	}
	_ = tw.Flush() // flushing into a bytes.Buffer cannot fail
}

// writeSweepReport writes the final report in the requested format. Table
// goes to the command's writer (or --output file); json and yaml go through
// the serializer conventions (file, stdout, or a cm:// URI). applied marks
// the post-apply report, whose statuses supersede the plan's.
func writeSweepReport(ctx context.Context, cmd *cli.Command, outFormat serializer.Format, kubeconfig string, report *aicr.SweepReport, applied bool) (err error) {
	output := cmd.String(flagOutput)

	if outFormat == serializer.FormatTable {
		output = strings.TrimSpace(output)
		toFile := output != "" && output != "-" && output != serializer.StdoutURI
		if !applied && !toFile {
			// The plan table already printed is the report.
			return nil
		}
		w := cmd.Root().Writer
		if toFile {
			f, createErr := os.Create(output)
			if createErr != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to create output file", createErr)
			}
			defer func() {
				if closeErr := f.Close(); closeErr != nil {
					err = stderrors.Join(err, errors.Wrap(errors.ErrCodeInternal, "failed to close output file", closeErr))
				}
			}()
			w = f
		}
		buf := &bytes.Buffer{}
		if applied {
			fmt.Fprintln(buf, "\nSweep result:")
			writeSweepTable(buf, report.Objects)
		} else {
			renderSweepPlan(buf, report)
		}
		if _, writeErr := fmt.Fprint(w, buf.String()); writeErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write sweep result", writeErr)
		}
		return nil
	}

	ser, err := serializer.NewFileWriterOrStdoutWithKubeconfig(outFormat, output, kubeconfig)
	if err != nil {
		return err
	}
	defer func() {
		if closer, ok := ser.(interface{ Close() error }); ok {
			if closeErr := closer.Close(); closeErr != nil {
				err = stderrors.Join(err, errors.Wrap(errors.ErrCodeInternal, "failed to close serializer", closeErr))
			}
		}
	}()

	return ser.Serialize(ctx, report)
}

// confirmSweep gates deletion behind an interactive y/N, defaulting to no.
// Unlike the keyless-signing disclosure (confirmKeylessSigningDisclosure), a
// non-terminal stdin FAILS rather than proceeding: this prompt guards
// deletion, so a script that cannot answer must not delete.
func confirmSweep(in io.Reader, out io.Writer, candidates int, contextName, server string) error {
	if !isInteractiveStream(in) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"sweep deletes cluster resources and needs confirmation, but stdin is not a terminal; "+
				"re-run with --yes to delete without prompting, or --dry-run to only list")
	}

	fmt.Fprintf(out, "\nDelete the %d object(s) marked %s above from context %q (server %s)? [y/N]: ",
		candidates, aicr.SweepWouldDelete, orDash(contextName), orDash(server))

	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to read sweep confirmation", err)
		}
		return errSweepDeclined()
	}
	switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
	case "y", flagAssumeYes:
		return nil
	default:
		return errSweepDeclined()
	}
}

// errSweepDeclined reports an operator cancellation at the sweep prompt.
func errSweepDeclined() error {
	return errors.New(errors.ErrCodeCanceled,
		"sweep declined at the confirmation prompt; nothing was deleted "+
			"(re-run with --yes to skip this prompt, or --dry-run to only list)")
}
