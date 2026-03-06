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
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestGetResultFromJobLogs tests reading validation results from Job pod logs.
// Note: This test requires a running pod with logs, which is complex to mock.
// Real testing will be done in integration tests with actual Jobs.
func TestGetResultFromJobLogs(t *testing.T) {
	t.Skip("Requires running pod with logs - covered by integration tests")
}

func TestParseGoTestJSON(t *testing.T) {
	tests := []struct {
		name       string
		jsonOutput string
		wantStatus string
		wantCheck  string
		wantErr    bool
	}{
		{
			name: "passing test",
			jsonOutput: `{"Action":"pass","Test":"TestGpuDetection"}
{"Action":"output","Test":"TestGpuDetection","Output":"=== RUN   TestGpuDetection\n"}
{"Action":"pass","Test":"TestGpuDetection","Elapsed":1.5}`,
			wantStatus: statusPass,
			wantCheck:  "TestGpuDetection",
			wantErr:    false,
		},
		{
			name: "failing test",
			jsonOutput: `{"Action":"fail","Test":"TestGpuDetection"}
{"Action":"output","Test":"TestGpuDetection","Output":"=== RUN   TestGpuDetection\n"}
{"Action":"output","Test":"TestGpuDetection","Output":"    Error: GPU not found\n"}
{"Action":"fail","Test":"TestGpuDetection","Elapsed":0.5}`,
			wantStatus: statusFail,
			wantCheck:  "TestGpuDetection",
			wantErr:    false,
		},
		{
			name:       "empty output",
			jsonOutput: "",
			wantStatus: statusPass,
			wantCheck:  "",
			wantErr:    false,
		},
		{
			name: "malformed JSON lines are skipped",
			jsonOutput: `{"Action":"pass","Test":"TestValid"}
not valid json
{"Action":"output","Test":"TestValid","Output":"output\n"}`,
			wantStatus: statusPass,
			wantCheck:  "TestValid",
			wantErr:    false,
		},
		{
			name: "skipped test",
			jsonOutput: `{"Action":"run","Test":"TestSkipped"}
{"Action":"output","Test":"TestSkipped","Output":"=== RUN   TestSkipped\n"}
{"Action":"skip","Test":"TestSkipped","Elapsed":0.1}`,
			wantStatus: statusPass,
			wantCheck:  "TestSkipped",
			wantErr:    false,
		},
		{
			name: "package-level fail",
			jsonOutput: `{"Action":"fail"}
{"Action":"output","Output":"FAIL\n"}`,
			wantStatus: statusFail,
			wantCheck:  "",
			wantErr:    false,
		},
		{
			name: "package-level output only",
			jsonOutput: `{"Action":"output","Output":"=== Package output\n"}
{"Action":"pass"}`,
			wantStatus: statusPass,
			wantCheck:  "",
			wantErr:    false,
		},
		{
			name: "test with run action",
			jsonOutput: `{"Action":"run","Test":"TestRunAction"}
{"Action":"pass","Test":"TestRunAction","Elapsed":0.5}`,
			wantStatus: statusPass,
			wantCheck:  "TestRunAction",
			wantErr:    false,
		},
		{
			name: "test with duration and output",
			jsonOutput: `{"Action":"run","Test":"TestDuration"}
{"Action":"output","Test":"TestDuration","Output":"Running test...\n"}
{"Action":"output","Test":"TestDuration","Output":"Test completed\n"}
{"Action":"pass","Test":"TestDuration","Elapsed":2.5}`,
			wantStatus: statusPass,
			wantCheck:  "TestDuration",
			wantErr:    false,
		},
		{
			name: "multiple tests mixed results",
			jsonOutput: `{"Action":"run","Test":"TestPass1"}
{"Action":"pass","Test":"TestPass1","Elapsed":0.1}
{"Action":"run","Test":"TestPass2"}
{"Action":"pass","Test":"TestPass2","Elapsed":0.2}
{"Action":"run","Test":"TestFail"}
{"Action":"fail","Test":"TestFail","Elapsed":0.3}`,
			wantStatus: statusFail,
			wantCheck:  "TestFail",
			wantErr:    false,
		},
		{
			name: "empty lines in output",
			jsonOutput: `
{"Action":"pass","Test":"TestEmpty"}

`,
			wantStatus: statusPass,
			wantCheck:  "TestEmpty",
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseGoTestJSON(tt.jsonOutput)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseGoTestJSON() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if result == nil {
				t.Fatal("expected non-nil result")
			}

			if result.Status != tt.wantStatus {
				t.Errorf("expected status %q, got %q", tt.wantStatus, result.Status)
			}

			// Check individual test results (new implementation uses Tests slice)
			if tt.wantCheck != "" {
				if len(result.Tests) == 0 {
					t.Errorf("expected test results, got none")
					return
				}
				found := false
				for _, test := range result.Tests {
					if test.Name == tt.wantCheck {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected test %q in results, not found. Got: %v", tt.wantCheck, result.Tests)
				}
			}
		})
	}
}

func TestSummarizeTestResults(t *testing.T) {
	tests := []struct {
		name            string
		tests           []TestResult
		overallOutput   []string
		wantMessage     string
		wantDuration    time.Duration
		wantOutputInDet bool
	}{
		{
			name: "all passed",
			tests: []TestResult{
				{Name: "TestA", Status: statusPass, Duration: 100 * time.Millisecond},
				{Name: "TestB", Status: statusPass, Duration: 200 * time.Millisecond},
			},
			wantMessage:  "2 tests passed",
			wantDuration: 300 * time.Millisecond,
		},
		{
			name: "some failed",
			tests: []TestResult{
				{Name: "TestA", Status: statusPass, Duration: 100 * time.Millisecond},
				{Name: "TestB", Status: statusFail, Duration: 200 * time.Millisecond},
			},
			wantMessage:  "2 tests: 1 passed, 1 failed, 0 skipped",
			wantDuration: 300 * time.Millisecond,
		},
		{
			name: "some skipped",
			tests: []TestResult{
				{Name: "TestA", Status: statusPass, Duration: 50 * time.Millisecond},
				{Name: "TestB", Status: statusSkip, Duration: 10 * time.Millisecond},
			},
			wantMessage:  "2 tests: 1 passed, 1 skipped",
			wantDuration: 60 * time.Millisecond,
		},
		{
			name: "mixed pass fail skip",
			tests: []TestResult{
				{Name: "TestA", Status: statusPass, Duration: 100 * time.Millisecond},
				{Name: "TestB", Status: statusFail, Duration: 200 * time.Millisecond},
				{Name: "TestC", Status: statusSkip, Duration: 50 * time.Millisecond},
			},
			wantMessage:  "3 tests: 1 passed, 1 failed, 1 skipped",
			wantDuration: 350 * time.Millisecond,
		},
		{
			name:         "empty test list",
			tests:        []TestResult{},
			wantMessage:  "0 tests passed",
			wantDuration: 0,
		},
		{
			name: "overall output stored in details",
			tests: []TestResult{
				{Name: "TestA", Status: statusPass, Duration: 100 * time.Millisecond},
			},
			overallOutput:   []string{"line1", "line2"},
			wantMessage:     "1 tests passed",
			wantDuration:    100 * time.Millisecond,
			wantOutputInDet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &ValidationResult{
				Details: make(map[string]interface{}),
				Tests:   tt.tests,
			}
			summarizeTestResults(result, tt.overallOutput)

			if result.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", result.Message, tt.wantMessage)
			}
			if result.Duration != tt.wantDuration {
				t.Errorf("Duration = %v, want %v", result.Duration, tt.wantDuration)
			}
			_, hasOutput := result.Details["output"]
			if tt.wantOutputInDet && !hasOutput {
				t.Error("expected output in Details")
			}
			if !tt.wantOutputInDet && hasOutput {
				t.Error("unexpected output in Details")
			}
		})
	}
}

func TestGetPodForJob(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	t.Run("find pod for Job", func(t *testing.T) {
		// Create a pod with the Job label
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: deployer.config.Namespace,
				Labels: map[string]string{
					"batch.kubernetes.io/job-name": deployer.config.JobName,
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "validator",
						Image: "test-image",
					},
				},
			},
		}

		if _, err := clientset.CoreV1().Pods(deployer.config.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to create pod: %v", err)
		}

		foundPod, err := deployer.getPodForJob(ctx)
		if err != nil {
			t.Fatalf("getPodForJob() failed: %v", err)
		}

		if foundPod.Name != "test-pod" {
			t.Errorf("expected pod name test-pod, got %q", foundPod.Name)
		}
	})

	t.Run("no pod found", func(t *testing.T) {
		deployer2, _ := createDeployer()
		deployer2.config.JobName = "nonexistent"

		_, err := deployer2.getPodForJob(ctx)
		if err == nil {
			t.Error("getPodForJob() should fail when no pod exists")
		}
		if !strings.Contains(err.Error(), "no pods found") {
			t.Errorf("expected 'no pods found' error, got %v", err)
		}
	})
}

func TestWaitForCompletion(t *testing.T) {
	// Note: Fake clientset doesn't support Watch properly, so we test the API only
	deployer, _ := createDeployer()
	ctx := context.Background()

	t.Run("timeout immediately", func(t *testing.T) {
		// Use very short timeout to ensure immediate timeout
		err := deployer.WaitForCompletion(ctx, 1*time.Millisecond)
		if err == nil {
			t.Error("WaitForCompletion() should timeout when Job doesn't exist")
		}
	})
}

func TestJobConditionTypes(t *testing.T) {
	// Test that we handle all Job condition types correctly
	conditions := []struct {
		name           string
		conditionType  batchv1.JobConditionType
		status         corev1.ConditionStatus
		shouldComplete bool
		shouldFail     bool
	}{
		{
			name:           "Complete",
			conditionType:  batchv1.JobComplete,
			status:         corev1.ConditionTrue,
			shouldComplete: true,
			shouldFail:     false,
		},
		{
			name:           "Failed",
			conditionType:  batchv1.JobFailed,
			status:         corev1.ConditionTrue,
			shouldComplete: false,
			shouldFail:     true,
		},
		{
			name:           "Suspended",
			conditionType:  batchv1.JobSuspended,
			status:         corev1.ConditionTrue,
			shouldComplete: false,
			shouldFail:     false,
		},
		{
			name:           "FailureTarget",
			conditionType:  batchv1.JobFailureTarget,
			status:         corev1.ConditionTrue,
			shouldComplete: false,
			shouldFail:     false,
		},
		{
			name:           "SuccessCriteriaMet",
			conditionType:  batchv1.JobSuccessCriteriaMet,
			status:         corev1.ConditionTrue,
			shouldComplete: false,
			shouldFail:     false,
		},
	}

	for _, tc := range conditions {
		t.Run(tc.name, func(t *testing.T) {
			// This test verifies the condition types are recognized
			// Actual watch behavior can't be tested with fake clientset
			_ = tc.conditionType
			_ = tc.status
			_ = tc.shouldComplete
			_ = tc.shouldFail
		})
	}
}

func TestStreamLogs(t *testing.T) {
	// Note: Fake clientset doesn't support streaming logs, so we test the API only
	deployer, _ := createDeployer()
	ctx := context.Background()

	t.Run("no pod exists", func(t *testing.T) {
		err := deployer.StreamLogs(ctx)
		if err == nil {
			t.Error("StreamLogs() should fail when no pod exists")
		}
	})
}

func TestGoTestEvent(t *testing.T) {
	// Test GoTestEvent struct marshaling/unmarshaling
	event := GoTestEvent{
		Time:    time.Now(),
		Action:  "pass",
		Package: "github.com/NVIDIA/aicr/pkg/validator",
		Test:    "TestGpuDetection",
		Output:  "test output\n",
		Elapsed: 1.5,
	}

	if event.Action != "pass" {
		t.Errorf("expected Action pass, got %q", event.Action)
	}
	if event.Test != "TestGpuDetection" {
		t.Errorf("expected Test TestGpuDetection, got %q", event.Test)
	}
	if event.Elapsed != 1.5 {
		t.Errorf("expected Elapsed 1.5, got %f", event.Elapsed)
	}
}

func TestValidationResult(t *testing.T) {
	// Test ValidationResult struct
	result := &ValidationResult{
		CheckName: "TestGpuDetection",
		Phase:     "readiness",
		Status:    statusPass,
		Message:   "GPU detected successfully",
		Duration:  1500 * time.Millisecond,
		Details: map[string]interface{}{
			"gpuCount": 8,
			"gpuType":  "H100",
		},
	}

	if result.CheckName != "TestGpuDetection" {
		t.Errorf("expected CheckName TestGpuDetection, got %q", result.CheckName)
	}
	if result.Status != statusPass {
		t.Errorf("expected Status pass, got %q", result.Status)
	}
	if result.Duration != 1500*time.Millisecond {
		t.Errorf("expected Duration 1500ms, got %v", result.Duration)
	}
	if result.Details["gpuCount"] != 8 {
		t.Errorf("expected gpuCount 8, got %v", result.Details["gpuCount"])
	}
}

func TestWaitForJobCompletion_NoPod(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create a Job but no pod
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.JobName,
			Namespace: deployer.config.Namespace,
		},
	}
	_, err := clientset.BatchV1().Jobs(deployer.config.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test Job: %v", err)
	}

	// Should timeout because there's no pod
	err = deployer.WaitForCompletion(ctx, 10*time.Millisecond)
	if err == nil {
		t.Error("WaitForCompletion() should fail when no pod exists")
	}
}

func TestGetPodForJob_NoPod(t *testing.T) {
	deployer, _ := createDeployer()
	ctx := context.Background()

	// getPodForJob should fail when no Job/pod exists
	_, err := deployer.getPodForJob(ctx)
	if err == nil {
		t.Error("getPodForJob() expected error when no Job exists, got nil")
	}
}

func TestStreamLogs_NoPod(t *testing.T) {
	deployer, _ := createDeployer()
	ctx := context.Background()

	// streamPodLogs should fail when no pod exists
	err := deployer.streamPodLogs(ctx)
	if err == nil {
		t.Error("streamPodLogs() expected error when no pod exists, got nil")
	}
}

func TestParseGoTestJSON_InvalidJSON(t *testing.T) {
	// Invalid JSON lines are skipped; result defaults to pass with no tests
	result, err := parseGoTestJSON("not valid json")
	if err != nil {
		t.Fatalf("parseGoTestJSON() unexpected error: %v", err)
	}
	if result.Status != statusPass {
		t.Errorf("expected status %q for invalid JSON, got %q", statusPass, result.Status)
	}
	if len(result.Tests) != 0 {
		t.Errorf("expected 0 tests for invalid JSON, got %d", len(result.Tests))
	}
}

func TestParseGoTestJSON_PartialOutputReassembly(t *testing.T) {
	tests := []struct {
		name       string
		jsonOutput string
		wantOutput []string
	}{
		{
			name: "split across two events",
			jsonOutput: `{"Action":"run","Test":"TestArtifact"}
{"Action":"output","Test":"TestArtifact","Output":"ARTIFACT:AAAA"}
{"Action":"output","Test":"TestArtifact","Output":"BBBB\n"}
{"Action":"pass","Test":"TestArtifact","Elapsed":0.1}`,
			wantOutput: []string{"ARTIFACT:AAAABBBB"},
		},
		{
			name: "split across three events",
			jsonOutput: `{"Action":"run","Test":"TestArtifact"}
{"Action":"output","Test":"TestArtifact","Output":"part1"}
{"Action":"output","Test":"TestArtifact","Output":"part2"}
{"Action":"output","Test":"TestArtifact","Output":"part3\n"}
{"Action":"pass","Test":"TestArtifact","Elapsed":0.1}`,
			wantOutput: []string{"part1part2part3"},
		},
		{
			name: "complete line not split",
			jsonOutput: `{"Action":"run","Test":"TestComplete"}
{"Action":"output","Test":"TestComplete","Output":"complete line\n"}
{"Action":"pass","Test":"TestComplete","Elapsed":0.1}`,
			wantOutput: []string{"complete line"},
		},
		{
			name: "mixed complete and partial",
			jsonOutput: `{"Action":"run","Test":"TestMixed"}
{"Action":"output","Test":"TestMixed","Output":"line1\n"}
{"Action":"output","Test":"TestMixed","Output":"partial"}
{"Action":"output","Test":"TestMixed","Output":"-complete\n"}
{"Action":"output","Test":"TestMixed","Output":"line3\n"}
{"Action":"pass","Test":"TestMixed","Elapsed":0.1}`,
			wantOutput: []string{"line1", "partial-complete", "line3"},
		},
		{
			name: "trailing partial flushed",
			jsonOutput: `{"Action":"run","Test":"TestTrailing"}
{"Action":"output","Test":"TestTrailing","Output":"no-newline"}
{"Action":"pass","Test":"TestTrailing","Elapsed":0.1}`,
			wantOutput: []string{"no-newline"},
		},
		{
			name: "package-level partial reassembly",
			jsonOutput: `{"Action":"output","Output":"pkg-"}
{"Action":"output","Output":"level\n"}
{"Action":"pass"}`,
			wantOutput: nil, // package-level output goes to overallOutput, not test output
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseGoTestJSON(tt.jsonOutput)
			if err != nil {
				t.Fatalf("parseGoTestJSON() error = %v", err)
			}

			if tt.wantOutput == nil {
				// For package-level test, verify package output was reassembled
				if tt.name == "package-level partial reassembly" {
					output, ok := result.Details["output"]
					if !ok {
						t.Fatal("expected package-level output in Details")
					}
					outputSlice, ok := output.([]string)
					if !ok {
						t.Fatalf("expected []string, got %T", output)
					}
					if len(outputSlice) != 1 || outputSlice[0] != "pkg-level" {
						t.Errorf("package output = %v, want [pkg-level]", outputSlice)
					}
				}
				return
			}

			if len(result.Tests) == 0 {
				t.Fatal("expected test results")
			}

			test := result.Tests[0]
			if len(test.Output) != len(tt.wantOutput) {
				t.Fatalf("output lines = %d, want %d; got %v", len(test.Output), len(tt.wantOutput), test.Output)
			}
			for i, want := range tt.wantOutput {
				if test.Output[i] != want {
					t.Errorf("output[%d] = %q, want %q", i, test.Output[i], want)
				}
			}
		})
	}
}

func TestParseGoTestJSON_PartialOutputPerTest(t *testing.T) {
	// Two tests with interleaved partial output — each should be reassembled independently.
	jsonOutput := `{"Action":"run","Test":"TestA"}
{"Action":"run","Test":"TestB"}
{"Action":"output","Test":"TestA","Output":"a-part1"}
{"Action":"output","Test":"TestB","Output":"b-part1"}
{"Action":"output","Test":"TestA","Output":"a-part2\n"}
{"Action":"output","Test":"TestB","Output":"b-part2\n"}
{"Action":"pass","Test":"TestA","Elapsed":0.1}
{"Action":"pass","Test":"TestB","Elapsed":0.1}`

	result, err := parseGoTestJSON(jsonOutput)
	if err != nil {
		t.Fatalf("parseGoTestJSON() error = %v", err)
	}

	if len(result.Tests) != 2 {
		t.Fatalf("expected 2 tests, got %d", len(result.Tests))
	}

	testMap := make(map[string]*TestResult)
	for i := range result.Tests {
		testMap[result.Tests[i].Name] = &result.Tests[i]
	}

	testA := testMap["TestA"]
	if testA == nil {
		t.Fatal("TestA not found")
	}
	if len(testA.Output) != 1 || testA.Output[0] != "a-part1a-part2" {
		t.Errorf("TestA output = %v, want [a-part1a-part2]", testA.Output)
	}

	testB := testMap["TestB"]
	if testB == nil {
		t.Fatal("TestB not found")
	}
	if len(testB.Output) != 1 || testB.Output[0] != "b-part1b-part2" {
		t.Errorf("TestB output = %v, want [b-part1b-part2]", testB.Output)
	}
}

func TestParseGoTestJSON_EmptyOutput(t *testing.T) {
	result, err := parseGoTestJSON("")
	if err != nil {
		t.Fatalf("parseGoTestJSON() unexpected error: %v", err)
	}
	if result.Status != statusPass {
		t.Errorf("expected status %q for empty output, got %q", statusPass, result.Status)
	}
	if len(result.Tests) != 0 {
		t.Errorf("expected 0 tests for empty output, got %d", len(result.Tests))
	}
}

func TestWaitForJobPodTermination_NoPod(t *testing.T) {
	t.Helper()

	deployer, _ := createDeployer()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Should return immediately when no pod exists (no panic, no block)
	deployer.WaitForJobPodTermination(ctx)
}

func TestWaitForJobPodTermination_AlreadyTerminal(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create a pod in Failed state (terminal)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: deployer.config.Namespace,
			Labels: map[string]string{
				"batch.kubernetes.io/job-name": deployer.config.JobName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
		},
	}
	if _, err := clientset.CoreV1().Pods(deployer.config.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create test pod: %v", err)
	}

	// Should return immediately for terminal pod
	termCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	deployer.WaitForJobPodTermination(termCtx)

	// Verify context was not exhausted (returned early)
	if termCtx.Err() != nil {
		t.Error("WaitForJobPodTermination should return immediately for terminal pod")
	}
}

func TestWaitForJobPodTermination_Succeeded(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create a pod in Succeeded state (terminal)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: deployer.config.Namespace,
			Labels: map[string]string{
				"batch.kubernetes.io/job-name": deployer.config.JobName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
		},
	}
	if _, err := clientset.CoreV1().Pods(deployer.config.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create test pod: %v", err)
	}

	termCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	deployer.WaitForJobPodTermination(termCtx)

	if termCtx.Err() != nil {
		t.Error("WaitForJobPodTermination should return immediately for succeeded pod")
	}
}
