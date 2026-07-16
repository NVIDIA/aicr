// Copyright 2025 The Sigstore Authors.
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

package util

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type RetryConfig struct {
	MaxAttempts     int
	InitialInterval time.Duration
	MaxInterval     time.Duration
	Multiplier      float64
	MaxElapsedTime  time.Duration
}

func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxAttempts:     5,
		InitialInterval: 1 * time.Second,
		MaxInterval:     5 * time.Second,
		Multiplier:      1.5,
		MaxElapsedTime:  30 * time.Second,
	}
}

type RetryError struct {
	Err error
}

func (r RetryError) Error() string {
	return r.Err.Error()
}

func (r RetryError) ShouldRetry() bool {
	if r.Err == nil {
		return false
	}

	if httpErr, ok := r.Err.(interface{ StatusCode() int }); ok {
		statusCode := httpErr.StatusCode()
		return statusCode >= 500 || statusCode == http.StatusTooManyRequests
	}

	if http2Err, ok := r.Err.(interface{ Error() string }); ok {
		if strings.Contains(http2Err.Error(), "http2: server sent GOAWAY") {
			return true
		}
	}

	return false
}

func WrapError(err error) error {
	if err == nil {
		return nil
	}
	return RetryError{Err: err}
}

// Retry executes the provided function with retry logic based on the configuration.
// It stops on success, unrecoverable errors, context cancellation, or exceeding the retry limits.
func Retry(ctx context.Context, f func() (any, error), opts ...func(*RetryConfig)) (any, error) {
	config := DefaultRetryConfig()
	for _, opt := range opts {
		opt(config)
	}

	if err := validateRetryConfig(config); err != nil {
		return nil, fmt.Errorf("invalid retry configuration: %w", err)
	}

	var lastErr error
	startTime := time.Now()
	currentInterval := config.InitialInterval

	for attempt := 1; attempt <= config.MaxAttempts; attempt++ {
		if config.MaxElapsedTime > 0 && time.Since(startTime) > config.MaxElapsedTime {
			return nil, fmt.Errorf("max elapsed time exceeded after %d attempts, last error: %w", attempt-1, lastErr)
		}

		result, err := f()
		if err == nil {
			return result, nil
		}

		lastErr = err

		if retryableErr, ok := err.(RetryError); ok && !retryableErr.ShouldRetry() {
			return nil, fmt.Errorf("non-retryable error encountered after %d attempts: %w", attempt, err)
		}

		if attempt == config.MaxAttempts {
			return nil, fmt.Errorf("retry cancelled after %d attempts: %w", attempt, context.DeadlineExceeded)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("retry cancelled after %d attempts: %w", attempt, ctx.Err())
		case <-time.After(currentInterval):
			currentInterval = time.Duration(float64(currentInterval) * config.Multiplier)
			if currentInterval > config.MaxInterval {
				currentInterval = config.MaxInterval
			}
		}
	}

	return nil, fmt.Errorf("retry logic exited unexpectedly, last error: %w", lastErr)
}

func validateRetryConfig(config *RetryConfig) error {
	if config.MaxAttempts <= 0 {
		return errors.New("MaxAttempts must be greater than zero")
	}
	if config.InitialInterval <= 0 {
		return errors.New("InitialInterval must be greater than zero")
	}
	if config.MaxInterval <= 0 {
		return errors.New("MaxInterval must be greater than zero")
	}
	if config.Multiplier <= 1 {
		return errors.New("multiplier must be greater than one")
	}
	if config.MaxElapsedTime <= 0 {
		return errors.New("MaxElapsedTime must be greater than zero")
	}
	return nil
}

// WithMaxAttempts sets the maximum number of retry attempts.
func WithMaxAttempts(attempts int) func(*RetryConfig) {
	return func(c *RetryConfig) {
		if attempts > 0 {
			c.MaxAttempts = attempts
		}
	}
}

// WithInitialInterval sets the initial interval between retries.
func WithInitialInterval(interval time.Duration) func(*RetryConfig) {
	return func(c *RetryConfig) {
		if interval > 0 {
			c.InitialInterval = interval
		}
	}
}

// WithMaxInterval sets the maximum interval between retries.
func WithMaxInterval(interval time.Duration) func(*RetryConfig) {
	return func(c *RetryConfig) {
		if interval > 0 {
			c.MaxInterval = interval
		}
	}
}

// WithMultiplier sets the backoff multiplier for exponential backoff.
func WithMultiplier(multiplier float64) func(*RetryConfig) {
	return func(c *RetryConfig) {
		if multiplier > 1 {
			c.Multiplier = multiplier
		}
	}
}

// WithMaxElapsedTime sets the maximum time allowed for retries.
func WithMaxElapsedTime(duration time.Duration) func(*RetryConfig) {
	return func(c *RetryConfig) {
		if duration > 0 {
			c.MaxElapsedTime = duration
		}
	}
}
