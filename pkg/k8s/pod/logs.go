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

package pod

import (
	"bufio"
	"bytes"
	"context"
	"io"

	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// StreamLogs streams pod logs to the provided writer in real-time.
// Logs are written line-by-line as they are received from the pod.
func StreamLogs(ctx context.Context, client kubernetes.Interface, namespace, podName string, logWriter io.Writer) error {
	req := client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Follow: true,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to stream pod logs", err)
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "context cancelled during log streaming", ctx.Err())
		default:
		}
		if _, err := logWriter.Write(append(scanner.Bytes(), '\n')); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write log output", err)
		}
	}

	if err := scanner.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to scan logs", err)
	}

	return nil
}

// GetPodLogs retrieves all logs from a pod as a string.
// This function is suitable for completed pods or when you need the full log history.
func GetPodLogs(ctx context.Context, client kubernetes.Interface, namespace, podName string) (string, error) {
	req := client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{})

	stream, err := req.Stream(ctx)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to get pod logs", err)
	}
	defer stream.Close()

	var logBuffer bytes.Buffer
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		logBuffer.WriteString(scanner.Text())
		logBuffer.WriteByte('\n')
	}

	if err := scanner.Err(); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to read pod logs", err)
	}

	return logBuffer.String(), nil
}
