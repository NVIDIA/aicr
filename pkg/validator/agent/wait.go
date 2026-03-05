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

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// waitForJobCompletion waits for the Job to complete or timeout.
func (d *Deployer) waitForJobCompletion(ctx context.Context, timeout time.Duration) error {
	err := pod.WaitForJobCompletion(ctx, d.clientset, d.config.Namespace, d.config.JobName, timeout)
	if err != nil {
		// If job failed with internal error, try to get detailed failure reason from Pod status
		var structuredErr *errors.StructuredError
		if goerrors.As(err, &structuredErr) && structuredErr.Code == errors.ErrCodeInternal {
			if jobPod, podErr := d.getPodForJob(ctx); podErr == nil {
				failureReason := d.getJobFailureReasonFromPod(jobPod)
				return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("job failed: %s", failureReason), err)
			}
		}
		return err
	}

	slog.Debug("Job completed successfully", "job", d.config.JobName)
	return nil
}

// getJobFailureReasonFromPod inspects the Pod status to determine detailed failure reason.
// This helps distinguish between test failures, image pull errors, crashes, etc.
func (d *Deployer) getJobFailureReasonFromPod(pod *corev1.Pod) string {
	// Check pod phase
	switch pod.Status.Phase {
	case corev1.PodPending:
		// Pod hasn't started - check for image pull issues
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if waiting := containerStatus.State.Waiting; waiting != nil {
				if strings.Contains(waiting.Reason, "ImagePull") {
					return fmt.Sprintf("Image pull failed: %s (image: %s)", waiting.Message, containerStatus.Image)
				}
				if waiting.Reason == "CrashLoopBackOff" {
					return fmt.Sprintf("Container in crash loop: %s", waiting.Message)
				}
				return fmt.Sprintf("Container waiting: %s - %s", waiting.Reason, waiting.Message)
			}
		}
		return "Pod pending"

	case corev1.PodFailed:
		// Pod completed but failed - check container exit codes
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if terminated := containerStatus.State.Terminated; terminated != nil {
				exitCode := terminated.ExitCode
				switch exitCode {
				case 1:
					// Exit code 1 typically means tests failed
					return fmt.Sprintf("Tests failed (exit code %d): %s", exitCode, terminated.Reason)
				case 2:
					// Exit code 2 typically means command/usage error
					return fmt.Sprintf("Command error (exit code %d): %s", exitCode, terminated.Reason)
				case 125, 126, 127:
					// Container/command execution errors
					return fmt.Sprintf("Execution error (exit code %d): %s", exitCode, terminated.Reason)
				case 137:
					// SIGKILL (OOMKilled or killed by system)
					if terminated.Reason == "OOMKilled" {
						return "Container killed due to out of memory (OOMKilled)"
					}
					return fmt.Sprintf("Container killed (exit code 137): %s", terminated.Reason)
				case 139:
					// SIGSEGV (segmentation fault)
					return "Container crashed with segmentation fault (exit code 139)"
				default:
					return fmt.Sprintf("Container exited with code %d: %s", exitCode, terminated.Reason)
				}
			}
		}
		return "Pod failed"

	case corev1.PodRunning:
		// Pod is still running but Job failed - shouldn't normally happen
		return "Pod still running but Job marked as failed"

	case corev1.PodSucceeded:
		// Pod succeeded but we're checking failure reason - shouldn't happen
		return "Pod succeeded (unexpected in failure check)"

	case corev1.PodUnknown:
		// Pod state is unknown
		return "Pod state unknown"

	default:
		return fmt.Sprintf("Unexpected pod phase: %s", pod.Status.Phase)
	}
}

// WaitForPodReady waits for the Job's pod to appear and reach the Running phase.
// This is used before streaming logs so we have a pod to follow.
// The caller must provide a timeout-bounded context.
func (d *Deployer) WaitForPodReady(ctx context.Context) error {
	slog.Debug("waiting for job pod to be ready", "job", d.config.JobName)

	// Poll for pod to appear and be Running
	ticker := time.NewTicker(defaults.PodPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "timed out waiting for pod ready", ctx.Err())
		case <-ticker.C:
			jobPod, err := d.getPodForJob(ctx)
			if err != nil {
				continue // Pod not yet created
			}
			if jobPod.Status.Phase == corev1.PodRunning {
				slog.Debug("job pod is running", "pod", jobPod.Name)
				return nil
			}
			if jobPod.Status.Phase == corev1.PodSucceeded || jobPod.Status.Phase == corev1.PodFailed {
				return nil // Already terminal, caller can still read logs
			}
		}
	}
}

// WaitForJobPodTermination waits for the Job's pod to reach a terminal state
// or be deleted. This prevents race conditions where RBAC resources are cleaned
// up while the pod is still running cleanup operations (e.g., chainsaw namespace
// deletion). The caller must provide a timeout-bounded context.
func (d *Deployer) WaitForJobPodTermination(ctx context.Context) {
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		slog.Debug("no pod found for job, skipping termination wait", "job", d.config.JobName)
		return
	}

	if jobPod.Status.Phase == corev1.PodSucceeded || jobPod.Status.Phase == corev1.PodFailed {
		slog.Debug("pod already in terminal state", "pod", jobPod.Name, "phase", jobPod.Status.Phase)
		return
	}

	slog.Debug("waiting for job pod termination", "pod", jobPod.Name, "job", d.config.JobName)

	watcher, err := d.clientset.CoreV1().Pods(d.config.Namespace).Watch(
		ctx,
		metav1.ListOptions{
			FieldSelector: "metadata.name=" + jobPod.Name,
		},
	)
	if err != nil {
		slog.Warn("failed to watch pod for termination", "pod", jobPod.Name, "error", err)
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Warn("timed out waiting for pod termination", "pod", jobPod.Name)
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}

			if event.Type == watch.Deleted {
				slog.Debug("pod deleted", "pod", jobPod.Name)
				return
			}

			watchedPod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}

			if watchedPod.Status.Phase == corev1.PodSucceeded || watchedPod.Status.Phase == corev1.PodFailed {
				slog.Debug("pod reached terminal state", "pod", watchedPod.Name, "phase", watchedPod.Status.Phase)
				return
			}
		}
	}
}

// getResultFromJobLogs retrieves the validation result from Job pod logs.
func (d *Deployer) getResultFromJobLogs(ctx context.Context) (*ValidationResult, error) {
	// Get the pod for this Job
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to find pod", err)
	}

	// Get pod logs using shared function
	logs, err := pod.GetPodLogs(ctx, d.clientset, d.config.Namespace, jobPod.Name, "")
	if err != nil {
		return nil, err
	}

	// Parse logs to extract JSON test output between markers
	captureJSON := false
	var jsonLines []string

	scanner := bufio.NewScanner(strings.NewReader(logs))
	for scanner.Scan() {
		line := scanner.Text()

		// Capture lines between markers
		if strings.Contains(line, "--- BEGIN TEST OUTPUT ---") {
			captureJSON = true
			continue
		}
		if strings.Contains(line, "--- END TEST OUTPUT ---") {
			captureJSON = false
			continue
		}

		if captureJSON {
			jsonLines = append(jsonLines, line)
		}
	}

	// Parse go test JSON output
	jsonOutput := strings.Join(jsonLines, "\n")
	result, err := parseGoTestJSON(jsonOutput)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse test results", err)
	}

	return result, nil
}

// slogWriter is an io.Writer that parses go-test JSON output and routes
// meaningful progress lines (health check results, test pass/fail) to
// slog.Info, while sending verbose details to slog.Debug. This keeps the
// default CLI output concise during long validation runs.
type slogWriter struct{}

func (w slogWriter) Write(p []byte) (n int, err error) {
	line := strings.TrimRight(string(p), "\n")
	if line == "" {
		return len(p), nil
	}

	// Try to parse as go-test JSON
	var event struct {
		Action string `json:"Action"`
		Output string `json:"Output"`
	}
	if json.Unmarshal([]byte(line), &event) == nil {
		if event.Action == actionOutput && event.Output != "" {
			output := strings.TrimRight(event.Output, "\n")
			if isProgressLine(output) {
				slog.Info(output)
			} else {
				slog.Debug(output)
			}
			return len(p), nil
		}
		// Non-output events (start, run, pass, fail)
		slog.Debug(line)
		return len(p), nil
	}

	// Non-JSON lines (wrapper messages like "Running validation tests...")
	slog.Info(line)
	return len(p), nil
}

// isProgressLine returns true for lines that convey meaningful validation progress.
func isProgressLine(s string) bool {
	return strings.Contains(s, "health check passed") ||
		strings.Contains(s, "health check failed") ||
		strings.Contains(s, "Check passed") ||
		strings.Contains(s, "Check failed") ||
		strings.Contains(s, "--- PASS:") ||
		strings.Contains(s, "--- FAIL:") ||
		strings.Contains(s, "running health check") ||
		strings.HasPrefix(s, "PASS") ||
		strings.HasPrefix(s, "FAIL")
}

// streamPodLogs streams logs from the Job's pod.
func (d *Deployer) streamPodLogs(ctx context.Context) error {
	// Get the pod for this Job
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "failed to find pod", err)
	}

	return pod.StreamLogs(ctx, d.clientset, d.config.Namespace, jobPod.Name, "", slogWriter{})
}

// getPodLogsAsString retrieves all pod logs as a string.
// This is useful for capturing logs when a Job fails.
func (d *Deployer) getPodLogsAsString(ctx context.Context) (string, error) {
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeNotFound, "failed to find pod", err)
	}

	return pod.GetPodLogs(ctx, d.clientset, d.config.Namespace, jobPod.Name, "")
}

// getPodForJob finds the pod created by the Job.
func (d *Deployer) getPodForJob(ctx context.Context) (*corev1.Pod, error) {
	pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("aicr.nvidia.com/job=%s", d.config.JobName),
	})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to list pods for job", err)
	}

	if len(pods.Items) == 0 {
		return nil, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("no pods found for Job %q", d.config.JobName))
	}

	return &pods.Items[0], nil
}

const (
	// Go test JSON action/status constants
	actionOutput = "output"
	statusPass   = "pass"
	statusFail   = "fail"
	statusSkip   = "skip"
	statusRun    = "running"
)

// GoTestEvent represents a single event from go test -json output.
type GoTestEvent struct {
	Time    time.Time
	Action  string
	Package string
	Test    string
	Output  string
	Elapsed float64
}

// parseGoTestJSON parses go test JSON output into a ValidationResult.
// Extracts individual test results from the JSON event stream.
//
//nolint:unparam // error return used for future error handling improvements
func parseGoTestJSON(jsonOutput string) (*ValidationResult, error) {
	result := &ValidationResult{
		Status:  "pass",
		Details: make(map[string]interface{}),
		Tests:   []TestResult{},
	}

	// Track individual tests
	testResults := make(map[string]*TestResult)
	var overallOutput []string

	// Track incomplete output lines per test.
	// go test -json splits long t.Logf output across multiple "output" events.
	// A complete line has a trailing \n; a partial line does not.
	pendingOutput := make(map[string]string)

	// Split JSON output by lines
	scanner := bufio.NewScanner(strings.NewReader(jsonOutput))
	scanner.Buffer(make([]byte, defaults.LogScannerBufferSize), defaults.LogScannerBufferSize)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var event GoTestEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			// Skip malformed JSON lines
			continue
		}

		// Handle package-level events (no Test field)
		if event.Test == "" {
			if event.Action == statusFail {
				result.Status = statusFail
			}
			if event.Action == actionOutput && event.Output != "" {
				key := ""
				pending := pendingOutput[key]
				combined := pending + event.Output
				if strings.HasSuffix(combined, "\n") {
					overallOutput = append(overallOutput, strings.TrimSuffix(combined, "\n"))
					delete(pendingOutput, key)
				} else {
					pendingOutput[key] = combined
				}
			}
			continue
		}

		// Handle test-specific events
		testName := event.Test

		// Initialize test result if not seen before
		if _, exists := testResults[testName]; !exists {
			testResults[testName] = &TestResult{
				Name:   testName,
				Status: statusPass, // Default to pass
				Output: []string{},
			}
		}

		test := testResults[testName]

		switch event.Action {
		case "run":
			// Test started
			test.Status = statusRun
		case statusPass:
			test.Status = statusPass
			if event.Elapsed > 0 {
				test.Duration = time.Duration(event.Elapsed * float64(time.Second))
			}
		case statusFail:
			test.Status = statusFail
			result.Status = statusFail // Mark overall result as failed
			if event.Elapsed > 0 {
				test.Duration = time.Duration(event.Elapsed * float64(time.Second))
			}
		case statusSkip:
			test.Status = statusSkip
			if event.Elapsed > 0 {
				test.Duration = time.Duration(event.Elapsed * float64(time.Second))
			}
		case actionOutput:
			if event.Output != "" {
				pending := pendingOutput[testName]
				combined := pending + event.Output
				if strings.HasSuffix(combined, "\n") {
					test.Output = append(test.Output, strings.TrimSuffix(combined, "\n"))
					delete(pendingOutput, testName)
				} else {
					pendingOutput[testName] = combined
				}
			}
		}
	}

	// Flush any remaining partial output lines (edge case: last line lacks \n).
	for key, pending := range pendingOutput {
		if pending == "" {
			continue
		}
		line := strings.TrimSuffix(pending, "\n")
		if key == "" {
			overallOutput = append(overallOutput, line)
		} else if test, ok := testResults[key]; ok {
			test.Output = append(test.Output, line)
		}
	}

	// Convert map to slice
	result.Tests = make([]TestResult, 0, len(testResults))
	for _, test := range testResults {
		result.Tests = append(result.Tests, *test)
	}

	// Summarize results
	summarizeTestResults(result, overallOutput)

	return result, nil
}

// summarizeTestResults populates Duration, Message, and Details from parsed tests.
func summarizeTestResults(result *ValidationResult, overallOutput []string) {
	var totalDuration time.Duration
	passCount, failCount, skipCount := 0, 0, 0

	for _, test := range result.Tests {
		totalDuration += test.Duration
		switch test.Status {
		case statusPass:
			passCount++
		case statusFail:
			failCount++
		case statusSkip:
			skipCount++
		}
	}
	result.Duration = totalDuration

	switch {
	case failCount > 0:
		result.Message = fmt.Sprintf("%d tests: %d passed, %d failed, %d skipped", len(result.Tests), passCount, failCount, skipCount)
	case skipCount > 0:
		result.Message = fmt.Sprintf("%d tests: %d passed, %d skipped", len(result.Tests), passCount, skipCount)
	default:
		result.Message = fmt.Sprintf("%d tests passed", passCount)
	}

	if len(overallOutput) > 0 {
		result.Details["output"] = overallOutput
	}
}
