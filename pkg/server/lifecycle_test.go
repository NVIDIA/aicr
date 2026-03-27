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

package server

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// freePort returns an available TCP port by binding to :0 and releasing.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func TestWithOnStart(t *testing.T) {
	called := false
	hook := func(_ context.Context) error {
		called = true
		return nil
	}

	s := New(WithOnStart(hook))

	if len(s.config.OnStart) != 1 {
		t.Fatalf("expected 1 OnStart hook, got %d", len(s.config.OnStart))
	}

	if called {
		t.Error("hook should not be called until Start()")
	}
}

func TestWithOnShutdown(t *testing.T) {
	called := false
	hook := func(_ context.Context) error {
		called = true
		return nil
	}

	s := New(WithOnShutdown(hook))

	if len(s.config.OnShutdown) != 1 {
		t.Fatalf("expected 1 OnShutdown hook, got %d", len(s.config.OnShutdown))
	}

	if called {
		t.Error("hook should not be called until Shutdown()")
	}
}

func TestOnStartHookExecutesOnStart(t *testing.T) {
	var startCalled atomic.Bool
	port := freePort(t)

	cfg := parseConfig()
	cfg.Port = port
	cfg.ShutdownTimeout = 100 * time.Millisecond

	s := New(
		withConfig(cfg),
		WithOnStart(func(_ context.Context) error {
			startCalled.Store(true)
			return nil
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		errChan <- s.Start(ctx)
	}()

	// Wait for server to start
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !startCalled.Load() {
		t.Error("OnStart hook should have been called")
	}

	cancel()

	select {
	case err := <-errChan:
		if err != nil {
			t.Errorf("expected clean shutdown, got error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("shutdown timed out")
	}
}

func TestOnStartHookFailureStopsServer(t *testing.T) {
	port := freePort(t)

	cfg := parseConfig()
	cfg.Port = port

	s := New(
		withConfig(cfg),
		WithOnStart(func(_ context.Context) error {
			return fmt.Errorf("hook failed")
		}),
	)

	ctx := context.Background()
	err := s.Start(ctx)

	if err == nil {
		t.Fatal("expected error from failed OnStart hook")
	}

	// Use the exported method path to check readiness safely
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()
	if ready {
		t.Error("server should not be ready after failed OnStart hook")
	}
}

func TestOnShutdownHookExecutesOnShutdown(t *testing.T) {
	var shutdownCalled atomic.Bool
	port := freePort(t)

	cfg := parseConfig()
	cfg.Port = port
	cfg.ShutdownTimeout = 2 * time.Second

	s := New(
		withConfig(cfg),
		WithOnShutdown(func(_ context.Context) error {
			shutdownCalled.Store(true)
			return nil
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		errChan <- s.Start(ctx)
	}()

	// Wait for server to start
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()

	select {
	case <-errChan:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown timed out")
	}

	if !shutdownCalled.Load() {
		t.Error("OnShutdown hook should have been called")
	}
}

func TestMultipleHooksExecuteInOrder(t *testing.T) {
	var order []int

	s := New(
		WithOnStart(func(_ context.Context) error {
			order = append(order, 1)
			return nil
		}),
		WithOnStart(func(_ context.Context) error {
			order = append(order, 2)
			return nil
		}),
	)

	if len(s.config.OnStart) != 2 {
		t.Fatalf("expected 2 OnStart hooks, got %d", len(s.config.OnStart))
	}

	// Simulate execution
	for _, hook := range s.config.OnStart {
		if err := hook(context.Background()); err != nil {
			t.Fatalf("hook error: %v", err)
		}
	}

	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("hooks executed in wrong order: %v", order)
	}
}
