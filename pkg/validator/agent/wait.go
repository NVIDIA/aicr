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

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/NVIDIA/eidos/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// waitForJobCompletion waits for the Job to complete or timeout.
func (d *Deployer) waitForJobCompletion(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	watcher, err := d.clientset.BatchV1().Jobs(d.config.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", d.config.JobName),
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch Job", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "timeout waiting for Job completion", ctx.Err())
		case event := <-watcher.ResultChan():
			if event.Type == watch.Deleted {
				return errors.New(errors.ErrCodeInternal, "job was deleted")
			}

			job, ok := event.Object.(*batchv1.Job)
			if !ok {
				continue
			}

			// Check for completion conditions
			for _, condition := range job.Status.Conditions {
				switch condition.Type {
				case batchv1.JobComplete:
					if condition.Status == corev1.ConditionTrue {
						slog.Debug("Job completed successfully",
							"job", d.config.JobName)
						return nil
					}
				case batchv1.JobFailed:
					if condition.Status == corev1.ConditionTrue {
						// Get detailed failure reason from Pod status
						failureReason := d.getJobFailureReason(ctx, condition.Reason, condition.Message)
						return errors.New(errors.ErrCodeInternal, fmt.Sprintf("job failed: %s", failureReason))
					}
				case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
					// These conditions don't affect completion, continue waiting
					continue
				}
			}
		}
	}
}

// getJobFailureReason inspects the Pod status to determine detailed failure reason.
// This helps distinguish between test failures, image pull errors, crashes, etc.
func (d *Deployer) getJobFailureReason(ctx context.Context, conditionReason, conditionMessage string) string {
	// Get the pod for this Job
	pod, err := d.getPodForJob(ctx)
	if err != nil {
		// Can't get pod details, return generic Job condition message
		return fmt.Sprintf("%s: %s", conditionReason, conditionMessage)
	}

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
		return fmt.Sprintf("Pod pending: %s", conditionMessage)

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
		return fmt.Sprintf("Pod failed: %s", conditionMessage)

	case corev1.PodRunning:
		// Pod is still running but Job failed - shouldn't normally happen
		return fmt.Sprintf("Pod still running but Job marked as failed: %s", conditionMessage)

	case corev1.PodSucceeded:
		// Pod succeeded but we're checking failure reason - shouldn't happen
		return fmt.Sprintf("Pod succeeded (unexpected in failure check): %s", conditionMessage)

	case corev1.PodUnknown:
		// Pod state is unknown
		return fmt.Sprintf("Pod state unknown: %s", conditionMessage)

	default:
		return fmt.Sprintf("%s: %s (pod phase: %s)", conditionReason, conditionMessage, pod.Status.Phase)
	}
}

// getResultFromJobLogs retrieves the validation result from Job pod logs.
func (d *Deployer) getResultFromJobLogs(ctx context.Context) (*ValidationResult, error) {
	// Get the pod for this Job
	pod, err := d.getPodForJob(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to find pod", err)
	}

	// Get pod logs
	req := d.clientset.CoreV1().Pods(d.config.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to get pod logs", err)
	}
	defer stream.Close()

	// Read all logs
	var logBuffer strings.Builder
	scanner := bufio.NewScanner(stream)
	captureJSON := false
	var jsonLines []string

	for scanner.Scan() {
		line := scanner.Text()
		logBuffer.WriteString(line)
		logBuffer.WriteString("\n")

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

	if scanErr := scanner.Err(); scanErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read pod logs", scanErr)
	}

	// Parse go test JSON output
	jsonOutput := strings.Join(jsonLines, "\n")
	result, err := parseGoTestJSON(jsonOutput)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse test results", err)
	}

	return result, nil
}

// streamPodLogs streams logs from the Job's pod.
func (d *Deployer) streamPodLogs(ctx context.Context) error {
	// Get the pod for this Job
	pod, err := d.getPodForJob(ctx)
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "failed to find pod", err)
	}

	req := d.clientset.CoreV1().Pods(d.config.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Follow: true,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to open log stream", err)
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}

	return scanner.Err()
}

// getPodLogsAsString retrieves all pod logs as a string.
// This is useful for capturing logs when a Job fails.
func (d *Deployer) getPodLogsAsString(ctx context.Context) (string, error) {
	pod, err := d.getPodForJob(ctx)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeNotFound, "failed to find pod", err)
	}

	req := d.clientset.CoreV1().Pods(d.config.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to get pod logs", err)
	}
	defer stream.Close()

	var logBuffer strings.Builder
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		logBuffer.WriteString(scanner.Text())
		logBuffer.WriteString("\n")
	}

	if scanErr := scanner.Err(); scanErr != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to read pod logs", scanErr)
	}

	return logBuffer.String(), nil
}

// getPodForJob finds the pod created by the Job.
func (d *Deployer) getPodForJob(ctx context.Context) (*corev1.Pod, error) {
	pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("eidos.nvidia.com/job=%s", d.config.JobName),
	})
	if err != nil {
		return nil, err
	}

	if len(pods.Items) == 0 {
		return nil, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("no pods found for Job %q", d.config.JobName))
	}

	return &pods.Items[0], nil
}

const (
	// Test status constants
	statusPass = "pass"
	statusFail = "fail"
	statusSkip = "skip"
	statusRun  = "running"
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

	// Split JSON output by lines
	scanner := bufio.NewScanner(strings.NewReader(jsonOutput))
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
			if event.Output != "" {
				overallOutput = append(overallOutput, event.Output)
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
		case "output":
			if event.Output != "" {
				test.Output = append(test.Output, strings.TrimSuffix(event.Output, "\n"))
			}
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
