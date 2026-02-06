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
		return fmt.Errorf("failed to watch Job: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for Job completion: %w", ctx.Err())
		case event := <-watcher.ResultChan():
			if event.Type == watch.Deleted {
				return fmt.Errorf("job was deleted")
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
						return fmt.Errorf("job failed: %s", condition.Message)
					}
				case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
					// These conditions don't affect completion, continue waiting
					continue
				}
			}
		}
	}
}

// getResultFromJobLogs retrieves the validation result from Job pod logs.
func (d *Deployer) getResultFromJobLogs(ctx context.Context) (*ValidationResult, error) {
	// Get the pod for this Job
	pod, err := d.getPodForJob(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find pod: %w", err)
	}

	// Get pod logs
	req := d.clientset.CoreV1().Pods(d.config.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod logs: %w", err)
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
		return nil, fmt.Errorf("failed to read pod logs: %w", scanErr)
	}

	// Parse go test JSON output
	jsonOutput := strings.Join(jsonLines, "\n")
	result, err := parseGoTestJSON(jsonOutput)
	if err != nil {
		return nil, fmt.Errorf("failed to parse test results: %w", err)
	}

	return result, nil
}

// streamPodLogs streams logs from the Job's pod.
func (d *Deployer) streamPodLogs(ctx context.Context) error {
	// Get the pod for this Job
	pod, err := d.getPodForJob(ctx)
	if err != nil {
		return fmt.Errorf("failed to find pod: %w", err)
	}

	req := d.clientset.CoreV1().Pods(d.config.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Follow: true,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("failed to open log stream: %w", err)
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}

	return scanner.Err()
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
		return nil, fmt.Errorf("no pods found for Job %q", d.config.JobName)
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
	for _, test := range testResults {
		result.Tests = append(result.Tests, *test)
	}

	// Calculate overall duration from all tests
	var totalDuration time.Duration
	for _, test := range result.Tests {
		totalDuration += test.Duration
	}
	result.Duration = totalDuration

	// Set summary message
	passCount := 0
	failCount := 0
	skipCount := 0
	for _, test := range result.Tests {
		switch test.Status {
		case statusPass:
			passCount++
		case statusFail:
			failCount++
		case statusSkip:
			skipCount++
		}
	}

	// Generate summary message based on test counts
	switch {
	case failCount > 0:
		result.Message = fmt.Sprintf("%d tests: %d passed, %d failed, %d skipped", len(result.Tests), passCount, failCount, skipCount)
	case skipCount > 0:
		result.Message = fmt.Sprintf("%d tests: %d passed, %d skipped", len(result.Tests), passCount, skipCount)
	default:
		result.Message = fmt.Sprintf("%d tests passed", passCount)
	}

	// Store overall output in details
	if len(overallOutput) > 0 {
		result.Details["output"] = overallOutput
	}

	return result, nil
}
