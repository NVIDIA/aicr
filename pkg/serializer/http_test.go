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

package serializer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testData struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

func TestRespondJSON_Success(t *testing.T) {
	w := httptest.NewRecorder()
	data := testData{
		Message: "success",
		Code:    200,
	}

	RespondJSON(w, http.StatusOK, data)

	// Verify status code
	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	// Verify content type
	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", contentType)
	}

	// Verify response body
	var result testData
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if result.Message != data.Message {
		t.Errorf("expected message %s, got %s", data.Message, result.Message)
	}

	if result.Code != data.Code {
		t.Errorf("expected code %d, got %d", data.Code, result.Code)
	}
}

func TestRespondJSON_DifferentStatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"OK", http.StatusOK},
		{"Created", http.StatusCreated},
		{"BadRequest", http.StatusBadRequest},
		{"NotFound", http.StatusNotFound},
		{"InternalServerError", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			data := testData{Message: tt.name, Code: tt.statusCode}

			RespondJSON(w, tt.statusCode, data)

			if w.Code != tt.statusCode {
				t.Errorf("expected status %d, got %d", tt.statusCode, w.Code)
			}
		})
	}
}

func TestRespondJSON_EncodingError(t *testing.T) {
	w := httptest.NewRecorder()

	// Create data that cannot be JSON encoded
	// Channels cannot be marshaled to JSON
	badData := make(chan int)

	RespondJSON(w, http.StatusOK, badData)

	// Should return internal server error
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d for encoding error, got %d", http.StatusInternalServerError, w.Code)
	}

	// Should have error message
	if w.Body.Len() == 0 {
		t.Error("expected error message in body")
	}
}

func TestRespondJSON_ComplexData(t *testing.T) {
	w := httptest.NewRecorder()

	type nested struct {
		Field1 string
		Field2 int
	}

	complexData := map[string]any{
		"string": "value",
		"number": 42,
		"bool":   true,
		"nested": nested{Field1: "test", Field2: 123},
		"array":  []int{1, 2, 3},
		"null":   nil,
	}

	RespondJSON(w, http.StatusOK, complexData)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	// Verify it's valid JSON
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal complex response: %v", err)
	}

	// Verify some fields
	if result["string"] != defaultValueKey {
		t.Errorf("expected string field to be 'value', got %v", result["string"])
	}

	if result["number"].(float64) != 42 {
		t.Errorf("expected number field to be 42, got %v", result["number"])
	}
}

func TestRespondJSON_EmptyData(t *testing.T) {
	w := httptest.NewRecorder()

	RespondJSON(w, http.StatusOK, nil)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	// nil encodes to "null\n" in JSON
	body := w.Body.String()
	if body != "null\n" {
		t.Errorf("expected 'null\\n', got %q", body)
	}
}

func TestRespondJSON_BuffersBeforeWritingHeaders(t *testing.T) {
	// This test verifies that RespondJSON buffers the JSON
	// before writing headers, so encoding errors don't result
	// in partial responses

	w := httptest.NewRecorder()

	// Bad data that will fail encoding
	badData := make(chan int)

	RespondJSON(w, http.StatusOK, badData)

	// If buffering works correctly, we should get a 500 error
	// If it doesn't buffer, we'd get a 200 with an error body
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected buffering to prevent status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestNewHTTPReader_Defaults(t *testing.T) {
	reader := NewHTTPReader()

	if reader == nil {
		t.Fatal("expected non-nil HTTPReader")
		return
	}

	if reader.Client == nil {
		t.Error("expected non-nil Client")
	}

	if reader.UserAgent != HTTPReaderUserAgent {
		t.Errorf("expected UserAgent 'AICR-Serializer/1.0', got %s", reader.UserAgent)
	}
}

func TestNewHTTPReader_WithOptions(t *testing.T) {
	customUserAgent := "TestAgent/1.0"

	reader := NewHTTPReader(
		WithUserAgent(customUserAgent),
		WithInsecureSkipVerify(true),
		WithMaxIdleConns(50),
		WithMaxIdleConnsPerHost(5),
		WithMaxConnsPerHost(10),
	)

	if reader.UserAgent != customUserAgent {
		t.Errorf("expected UserAgent %s, got %s", customUserAgent, reader.UserAgent)
	}

	if *reader.InsecureSkipVerify != true {
		t.Error("expected InsecureSkipVerify to be true")
	}

	if *reader.MaxIdleConns != 50 {
		t.Errorf("expected MaxIdleConns 50, got %v", reader.MaxIdleConns)
	}

	if *reader.MaxIdleConnsPerHost != 5 {
		t.Errorf("expected MaxIdleConnsPerHost 5, got %v", reader.MaxIdleConnsPerHost)
	}

	if *reader.MaxConnsPerHost != 10 {
		t.Errorf("expected MaxConnsPerHost 10, got %v", reader.MaxConnsPerHost)
	}

	tr, ok := reader.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected Client.Transport to be *http.Transport")
	}

	if tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify != true {
		t.Error("expected transport TLS InsecureSkipVerify to be true")
	}
	if tr.MaxIdleConns != 50 {
		t.Errorf("expected transport MaxIdleConns 50, got %d", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 5 {
		t.Errorf("expected transport MaxIdleConnsPerHost 5, got %d", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != 10 {
		t.Errorf("expected transport MaxConnsPerHost 10, got %d", tr.MaxConnsPerHost)
	}
}

func TestNewHTTPReader_WithCustomClient(t *testing.T) {
	customClient := &http.Client{
		Timeout: 5 * time.Second,
	}

	reader := NewHTTPReader(WithClient(customClient))

	if reader.Client != customClient {
		t.Error("expected custom client to be used")
	}

	if reader.Client.Timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", reader.Client.Timeout)
	}
}

func TestHTTPReader_TimeoutOptions(t *testing.T) {
	totalTimeout := 10 * time.Second
	connectTimeout := 2 * time.Second
	tlsTimeout := 3 * time.Second
	headerTimeout := 4 * time.Second
	idleTimeout := 5 * time.Second

	reader := NewHTTPReader(
		WithTotalTimeout(totalTimeout),
		WithConnectTimeout(connectTimeout),
		WithTLSHandshakeTimeout(tlsTimeout),
		WithResponseHeaderTimeout(headerTimeout),
		WithIdleConnTimeout(idleTimeout),
	)

	if *reader.TotalTimeout != totalTimeout {
		t.Errorf("TotalTimeout = %v, want %v", reader.TotalTimeout, totalTimeout)
	}

	if *reader.ConnectTimeout != connectTimeout {
		t.Errorf("ConnectTimeout = %v, want %v", reader.ConnectTimeout, connectTimeout)
	}

	if *reader.TLSHandshakeTimeout != tlsTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want %v", reader.TLSHandshakeTimeout, tlsTimeout)
	}

	if *reader.ResponseHeaderTimeout != headerTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", reader.ResponseHeaderTimeout, headerTimeout)
	}

	if *reader.IdleConnTimeout != idleTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", reader.IdleConnTimeout, idleTimeout)
	}

	if reader.Client.Timeout != totalTimeout {
		t.Errorf("Client.Timeout = %v, want %v", reader.Client.Timeout, totalTimeout)
	}

	tr, ok := reader.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected Client.Transport to be *http.Transport")
	}
	if tr.TLSHandshakeTimeout != tlsTimeout {
		t.Errorf("transport TLSHandshakeTimeout = %v, want %v", tr.TLSHandshakeTimeout, tlsTimeout)
	}
	if tr.ResponseHeaderTimeout != headerTimeout {
		t.Errorf("transport ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, headerTimeout)
	}
	if tr.IdleConnTimeout != idleTimeout {
		t.Errorf("transport IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, idleTimeout)
	}
}

func TestHTTPReader_Read_Success(t *testing.T) {
	// Create test server
	testData := []byte("test response data")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(testData)
	}))
	defer server.Close()

	reader := NewHTTPReader()
	data, err := reader.Read(server.URL)
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}

	if string(data) != string(testData) {
		t.Errorf("expected data %q, got %q", string(testData), string(data))
	}
}

func TestHTTPReader_Read_EmptyURL(t *testing.T) {
	reader := NewHTTPReader()
	_, err := reader.Read("")
	if err == nil {
		t.Error("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "url is empty") {
		t.Errorf("expected error to contain 'url is empty', got %v", err)
	}
}

func TestHTTPReader_Read_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	reader := NewHTTPReader()
	_, err := reader.Read(server.URL)
	if err == nil {
		t.Error("expected error for 404 status")
	}
}

func TestHTTPReader_Read_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	reader := NewHTTPReader()
	_, err := reader.Read(server.URL)
	if err == nil {
		t.Error("expected error for 500 status")
	}
}

func TestHTTPReader_Read_InvalidURL(t *testing.T) {
	reader := NewHTTPReader()
	_, err := reader.Read("not-a-valid-url")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestHTTPReader_Read_JSONResponse(t *testing.T) {
	testResponse := map[string]any{
		"message": "success",
		"code":    200,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(testResponse)
	}))
	defer server.Close()

	reader := NewHTTPReader()
	data, err := reader.Read(server.URL)
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal JSON response: %v", err)
	}

	if result["message"] != "success" {
		t.Errorf("expected message 'success', got %v", result["message"])
	}
}

func TestHTTPReader_Read_SetsUserAgent(t *testing.T) {
	customUserAgent := "TestAgent/9.9"

	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	reader := NewHTTPReader(WithUserAgent(customUserAgent))
	_, err := reader.Read(server.URL)
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}

	select {
	case ua := <-seen:
		if ua != customUserAgent {
			t.Fatalf("expected User-Agent %q, got %q", customUserAgent, ua)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server to receive request")
	}
}

func TestHTTPReader_ReadWithContext_Canceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If the request isn't canceled, block for long enough to fail the test.
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reader := NewHTTPReader()
	_, err := reader.ReadWithContext(ctx, server.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected error to wrap context.Canceled, got %v", err)
	}
}

func TestHTTPReader_ReadToFile_Success(t *testing.T) {
	testData := []byte("test file content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(testData)
	}))
	defer server.Close()

	// Create temp directory for test file
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test-output.txt")

	reader := NewHTTPReader()
	err := reader.Download(server.URL, filePath)
	if err != nil {
		t.Fatalf("ReadToFile() failed: %v", err)
	}

	// Verify file was created and contains expected data
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	if string(data) != string(testData) {
		t.Errorf("expected file content %q, got %q", string(testData), string(data))
	}
}

func TestHTTPReader_ReadToFile_ReadError(t *testing.T) {
	// Create temp directory for test file
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test-output.txt")

	reader := NewHTTPReader()
	err := reader.Download("not-a-valid-url", filePath)
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestHTTPReader_ReadToFile_WriteError(t *testing.T) {
	testData := []byte("test content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(testData)
	}))
	defer server.Close()

	// Use invalid path (directory that doesn't exist)
	invalidPath := "/nonexistent/directory/file.txt"

	reader := NewHTTPReader()
	err := reader.Download(server.URL, invalidPath)
	if err == nil {
		t.Error("expected error for invalid file path")
	}
}

func TestHTTPReader_ReadToFile_JSONFile(t *testing.T) {
	testResponse := map[string]string{
		"key1": "value1",
		"key2": "value2",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(testResponse)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.json")

	reader := NewHTTPReader()
	err := reader.Download(server.URL, filePath)
	if err != nil {
		t.Fatalf("ReadToFile() failed: %v", err)
	}

	// Verify file contains valid JSON
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal JSON from file: %v", err)
	}

	if result["key1"] != "value1" {
		t.Errorf("expected key1='value1', got %s", result["key1"])
	}
}

func TestHTTPReader_Read_UserAgentHeader(t *testing.T) {
	customUserAgent := "CustomAgent/2.0"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	reader := NewHTTPReader(WithUserAgent(customUserAgent))

	// Note: The current implementation doesn't set User-Agent header in requests
	// This test documents current behavior
	_, err := reader.Read(server.URL)
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}

	// The UserAgent field is set but not currently used in requests
	// This is a potential enhancement point
	if reader.UserAgent != customUserAgent {
		t.Errorf("expected UserAgent %s, got %s", customUserAgent, reader.UserAgent)
	}
}

func TestHTTPReader_Read_LargeResponse(t *testing.T) {
	// Create large test data (1MB)
	largeData := make([]byte, 1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(largeData)
	}))
	defer server.Close()

	reader := NewHTTPReader()
	data, err := reader.Read(server.URL)
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}

	if len(data) != len(largeData) {
		t.Errorf("expected data length %d, got %d", len(largeData), len(data))
	}
}

func TestHTTPReader_Read_MultipleRequests(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "request %d", requestCount)
	}))
	defer server.Close()

	reader := NewHTTPReader()

	// Make multiple requests with same reader
	for i := 1; i <= 3; i++ {
		data, err := reader.Read(server.URL)
		if err != nil {
			t.Fatalf("Read() request %d failed: %v", i, err)
		}

		expected := fmt.Sprintf("request %d", i)
		if string(data) != expected {
			t.Errorf("request %d: expected %q, got %q", i, expected, string(data))
		}
	}

	if requestCount != 3 {
		t.Errorf("expected 3 requests, got %d", requestCount)
	}
}
