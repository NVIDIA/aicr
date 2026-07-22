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

package serializer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FormatFromPath determines the serialization format based on file extension.
// Supported extensions:
//   - .json → FormatJSON
//   - .yaml, .yml → FormatYAML
//   - .table, .txt → FormatTable
//
// Returns FormatJSON as default for unknown extensions.
// Extension matching is case-insensitive.
func FormatFromPath(filePath string) Format {
	lowerPath := strings.ToLower(filePath)
	switch {
	case strings.HasSuffix(lowerPath, ".json"):
		return FormatJSON
	case strings.HasSuffix(lowerPath, ".yaml"), strings.HasSuffix(lowerPath, ".yml"):
		return FormatYAML
	case strings.HasSuffix(lowerPath, ".table"), strings.HasSuffix(lowerPath, ".txt"):
		return FormatTable
	default:
		slog.Warn("unknown file extension, defaulting to JSON", "filePath", filePath)
		return FormatJSON
	}
}

// Reader handles deserialization of structured data from various formats (JSON, YAML).
// It supports reading from any io.Reader source including files, strings, and HTTP responses.
//
// Resource Management:
//   - Close must be called to release resources when using NewFileReader or newFileReaderAuto
//   - Safe to call Close multiple times (idempotent)
//   - Close is a no-op for readers created with NewReader from non-closeable sources
//
// Supported formats: JSON, YAML (Table format is write-only)
type Reader struct {
	format Format
	input  io.Reader
	closer io.Closer
	strict bool
}

// ReaderOption configures a Reader.
type ReaderOption func(*Reader)

// WithStrict enables strict decoding so unknown fields are rejected. JSON
// uses DisallowUnknownFields; YAML uses KnownFields(true). Use this for
// user-supplied recipe / snapshot / config files where a typo silently
// dropped to a zero-value is a footgun.
func WithStrict() ReaderOption {
	return func(r *Reader) {
		r.strict = true
	}
}

// NewReader creates a new Reader for deserializing data from an io.Reader source.
//
// Parameters:
//   - format: The serialization format (FormatJSON or FormatYAML)
//   - input: Any io.Reader implementation (e.g., strings.Reader, bytes.Buffer, *os.File)
//
// Returns error if:
//   - format is unknown or unsupported
//   - format is FormatTable (table format does not support deserialization)
//
// Resource Management:
//   - If input implements io.Closer, it will be stored and closed by Reader.Close()
//   - Otherwise, Close() is a no-op
//
// Example:
//
//	reader, err := NewReader(FormatJSON, strings.NewReader(`{"key":"value"}`})
//	if err != nil { panic(err) }
//	var data map[string]string
//	err = reader.Deserialize(&data)
func NewReader(format Format, input io.Reader) (*Reader, error) {
	if format.IsUnknown() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unknown format: %s", format))
	}

	if format == FormatTable {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "table format does not support deserialization")
	}

	r := &Reader{
		format: format,
		input:  input,
	}

	// Store closer if input implements it
	if closer, ok := input.(io.Closer); ok {
		r.closer = closer
	}

	return r, nil
}

// NewFileReader creates a new Reader that reads from a file path or URL.
//
// Parameters:
//   - format: The serialization format (FormatJSON or FormatYAML)
//   - filePath: Local file path or HTTP/HTTPS URL
//
// URL Support:
//   - Supports http:// and https:// URLs
//   - Downloads remote files to temporary directory
//   - Temporary files are managed by Reader.Close()
//
// Returns error if:
//   - format is unknown or unsupported
//   - format is FormatTable (table format does not support deserialization)
//   - file cannot be opened or URL cannot be downloaded
//
// Resource Management:
//   - Close must be called to release the file handle
//   - For remote URLs, Close also removes the temporary downloaded file
//
// Example:
//
//	reader, err := NewFileReader(FormatJSON, "/path/to/config.json")
//	if err != nil { panic(err) }
//	defer reader.Close()
func NewFileReader(format Format, filePath string) (*Reader, error) {
	// Bound the read with FileReadTimeout so a hung filesystem (network
	// mount, FUSE, /proc anomaly) cannot stall the caller indefinitely.
	// Callers that need a different bound should use NewFileReaderWithContext.
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	return NewFileReaderWithContext(ctx, format, filePath)
}

// NewFileReaderWithContext is the context-aware variant of NewFileReader.
// The context bounds both the URL-download path and the local read: the local
// content is read fully (up to MaxSpecFileBytes) so the size cap is enforced
// and a hung filesystem cannot outlive the deadline.
func NewFileReaderWithContext(ctx context.Context, format Format, filePath string) (*Reader, error) {
	if format.IsUnknown() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unknown format: %s", format))
	}

	if format == FormatTable {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "table format does not support deserialization")
	}

	if filePath == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "failed to open file: path is empty")
	}

	// If the filePath is a URL or special scheme, handle accordingly
	var file *os.File
	var err error

	if strings.HasPrefix(filePath, "http://") || strings.HasPrefix(filePath, "https://") {
		name := fmt.Sprintf("aicr-%d.tmp", time.Now().UnixNano())
		tempFilePath := filepath.Join(os.TempDir(), name)
		httpReader := NewHTTPReader()
		// Bound the download even when the caller supplied a context without
		// a deadline, so an unresponsive server cannot hang indefinitely.
		dlCtx, cancel := context.WithTimeout(ctx, defaults.HTTPClientTimeout)
		defer cancel()
		if err = httpReader.DownloadWithContext(dlCtx, filePath, tempFilePath); err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeUnavailable, "failed to download remote file")
		}
		file, err = os.Open(tempFilePath)
	} else {
		file, err = os.Open(filepath.Clean(filePath)) //nolint:gosec // G703 -- path from CLI arg or config
	}

	// Handle file open error. Distinguish ENOENT (NotFound) from other I/O
	// failures so callers can map "file missing" to a 4xx and other failures
	// to a 5xx.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, "file not found", err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to open file", err)
	}

	// Honor cancellation/timeout on the local read path too — a hung filesystem
	// (network mount, FUSE, /proc anomaly) must not stall past the deadline.
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = file.Close()
		return nil, errors.Wrap(errors.ErrCodeTimeout, "file read canceled", ctxErr)
	}

	// Read the bounded content fully so the size limit is actually enforced.
	// A LimitReader alone only caps how much the decoder *can* read; a valid
	// first document followed by trailing excess (or a single oversize value)
	// would otherwise be silently accepted. Reading up to MaxSpecFileBytes+1
	// lets us reject anything over the cap, matching the body cap used
	// elsewhere for spec-like inputs while bounding memory for an
	// attacker-influenced path (e.g. a multi-GB local file passed via --recipe).
	// The read runs under ctx so a hung filesystem (NFS/FUSE/procfs) cannot
	// block past the deadline.
	data, readErr := readAllBounded(ctx, file, defaults.MaxSpecFileBytes+1)
	if readErr != nil {
		_ = file.Close()
		if errors.IsTransient(readErr) {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "file read canceled", readErr)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read file", readErr)
	}
	if int64(len(data)) > defaults.MaxSpecFileBytes {
		_ = file.Close()
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file exceeds maximum allowed size of %d bytes", defaults.MaxSpecFileBytes))
	}

	return &Reader{
		format: format,
		input:  bytes.NewReader(data),
		closer: file,
	}, nil
}

// readAllBounded reads up to limit bytes from r, returning early if ctx is
// canceled or its deadline fires. The read runs in a goroutine so a hung
// filesystem read (network mount, FUSE, /proc anomaly) cannot outlive the
// deadline: on cancellation the caller is unblocked and the goroutine ends
// when the underlying Read eventually returns. Returns ctx.Err() on timeout.
func readAllBounded(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1) // buffered so the goroutine never leaks on send
	go func() {
		data, err := io.ReadAll(io.LimitReader(r, limit))
		ch <- result{data: data, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		return res.data, res.err
	}
}

// newFileReaderAuto creates a new Reader with automatic format detection.
// The format is determined from the file extension using FormatFromPath.
//
// This is a convenience wrapper around NewFileReader that auto-detects the format.
// See NewFileReader for full documentation on supported paths, URLs, and resource management.
func newFileReaderAuto(filePath string) (*Reader, error) {
	format := FormatFromPath(filePath)
	return NewFileReader(format, filePath)
}

// Deserialize reads data from the input source and unmarshals it into v.
//
// Parameters:
//   - v: A pointer to the target structure or variable
//
// Type Requirements:
//   - v must be a pointer (e.g., &myStruct, &mySlice, &myMap)
//   - The underlying type must be compatible with the format (JSON or YAML)
//
// Returns error if:
//   - Reader is nil
//   - Input source is nil
//   - Data cannot be decoded (invalid format, type mismatch)
//   - Format is FormatTable (not supported for deserialization)
//
// Example:
//
//	var config struct { Name string; Value int }
//	err := reader.Deserialize(&config)
func (r *Reader) Deserialize(v any) error {
	if r == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "reader is nil")
	}

	if r.input == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "input source is nil")
	}

	switch r.format {
	case FormatJSON:
		decoder := json.NewDecoder(r.input)
		if r.strict {
			decoder.DisallowUnknownFields()
		}
		if err := decoder.Decode(v); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to decode JSON", err)
		}
		return nil

	case FormatYAML:
		decoder := yaml.NewDecoder(r.input)
		if r.strict {
			decoder.KnownFields(true)
		}
		if err := decoder.Decode(v); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to decode YAML", err)
		}
		return nil

	case FormatTable:
		return errors.New(errors.ErrCodeInvalidRequest, "table format is not supported for deserialization")

	default:
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported format for deserialization: %s", r.format))
	}
}

// Close releases any resources held by the Reader.
//
// Behavior:
//   - If Reader was created from a file (NewFileReader), closes the file handle
//   - If Reader was created from a non-closeable source (NewReader), this is a no-op
//   - Sets internal closer to nil after first close to prevent double-close errors
//   - Safe to call on nil Reader
//
// Idempotency:
//   - Safe to call multiple times (subsequent calls are no-ops)
//   - Returns nil on subsequent calls after successful first close
//
// Best Practice:
//   - Always defer Close() immediately after creating a Reader from files
//   - Example: defer reader.Close()
func (r *Reader) Close() error {
	if r == nil {
		return nil
	}

	if r.closer != nil {
		err := r.closer.Close()
		r.closer = nil // Prevent double-close
		return err
	}
	return nil
}

// FromFile is a generic convenience function that loads and deserializes a file in one call.
// The file format is automatically detected from the file extension.
//
// Type Parameter:
//   - T: The target type (struct, slice, map, etc.) compatible with JSON/YAML unmarshaling
//
// Parameters:
//   - path: File path or HTTP/HTTPS URL
//
// Returns:
//   - Pointer to populated instance of type T
//   - Error if file cannot be read or deserialized
//
// Resource Management:
//   - Automatically handles Reader creation and cleanup (Close is called internally)
//   - No need to manually close the reader
//
// Example:
//
//	type Config struct { Name string; Port int }
//	config, err := FromFile[Config]("config.yaml")
//	if err != nil { panic(err) }
//	fmt.Println(config.Name) // Use config directly
//
// Note: This is a higher-level API. Use NewFileReader directly if you need
// more control over the Reader lifecycle or want to reuse it.
// FromFile reads and deserializes data from a file path, URL, or ConfigMap URI into type T.
//
// Supported input sources:
//   - Local file paths: /path/to/file.json, ./config.yaml
//   - HTTP URLs: http://example.com/data.json, https://api.example.com/config.yaml
//   - ConfigMap URIs: cm://namespace/configmap-name
//
// Format detection:
//   - File paths: Determined by extension (.json, .yaml, .yml)
//   - URLs: Determined by URL path extension or response Content-Type
//   - ConfigMap: Always YAML format (ConfigMaps store data as YAML)
//
// Returns:
//   - Pointer to deserialized object of type T
//   - Error if file/URL/ConfigMap not found, network error, or deserialization fails
//
// ConfigMap Format:
//   - Reads from ConfigMap data field "snapshot.{json|yaml}"
//   - Falls back to "snapshot.yaml" if specific format field not found
//   - Requires Kubernetes cluster access (kubeconfig)
//
// Example:
//
//	snap, err := FromFile[Snapshot]("cm://gpu-operator/aicr-snapshot")
func FromFile[T any](path string) (*T, error) {
	return FromFileWithKubeconfig[T](path, "")
}

// FromFileContext is the context-aware variant of FromFile. The provided
// context bounds the ConfigMap read when path is a cm:// URI and is threaded
// into NewFileReaderWithContext for plain file and HTTP reads. Prefer this
// variant in CLI/handler call sites that already hold a request-scoped
// context.
func FromFileContext[T any](ctx context.Context, path string) (*T, error) {
	return FromFileWithKubeconfigContext[T](ctx, path, "")
}

// FromFileWithKubeconfig reads and deserializes data from a file path, HTTP URL, or ConfigMap URI with custom kubeconfig.
//
// This is identical to FromFile but allows specifying a custom kubeconfig path for ConfigMap URIs.
// The kubeconfig parameter is only used when path is a ConfigMap URI (cm://namespace/name).
//
// Parameters:
//   - path: File path, HTTP/HTTPS URL, or ConfigMap URI (cm://namespace/name)
//   - kubeconfig: Path to kubeconfig file (only used for ConfigMap URIs, empty string uses default discovery)
//
// Example:
//
//	snap, err := FromFileWithKubeconfig[Snapshot]("cm://gpu-operator/aicr-snapshot", "/custom/kubeconfig")
func FromFileWithKubeconfig[T any](path, kubeconfig string) (*T, error) {
	return FromFileWithKubeconfigContext[T](context.Background(), path, kubeconfig)
}

// FromFileWithKubeconfigContext is the context-aware variant of
// FromFileWithKubeconfig. The context bounds the ConfigMap read when path is
// a cm:// URI.
func FromFileWithKubeconfigContext[T any](ctx context.Context, path, kubeconfig string) (*T, error) {
	// Check for ConfigMap URI
	if strings.HasPrefix(path, ConfigMapURIScheme) {
		namespace, name, err := pod.ParseConfigMapURI(path)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid ConfigMap URI", err)
		}
		return fromConfigMapWithKubeconfigContext[T](ctx, namespace, name, kubeconfig)
	}

	fileFormat := FormatFromPath(path)
	slog.Debug("determined file format",
		slog.String("path", path),
		slog.String("format", string(fileFormat)),
	)

	ser, err := NewFileReaderWithContext(ctx, fileFormat, path)
	if err != nil {
		slog.Error("failed to create file reader", "error", err, "path", path, "format", fileFormat)
		// Preserve the reader's structured code (NotFound / InvalidRequest /
		// Timeout) instead of flattening every failure to Internal.
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, fmt.Sprintf("failed to create serializer for %q", path))
	}

	if ser == nil {
		slog.Error("reader is unexpectedly nil despite no error", "path", path, "format", fileFormat)
		return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf("reader is nil for %q", path))
	}

	defer func() {
		if closeErr := ser.Close(); closeErr != nil {
			slog.Warn("failed to close serializer", "error", closeErr)
		}
	}()

	var r T
	if err := ser.Deserialize(&r); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to deserialize object from %q", path), err)
	}

	slog.Debug("successfully loaded object from file",
		slog.String("path", path),
	)

	return &r, nil
}

// fromConfigMapWithKubeconfigContext reads and deserializes data from a Kubernetes ConfigMap.
// The provided context is wrapped with defaults.ConfigMapWriteTimeout so the read is bounded
// even when the caller passes context.Background().
func fromConfigMapWithKubeconfigContext[T any](ctx context.Context, namespace, name, kubeconfig string) (*T, error) {
	var k8sClient client.Interface
	var err error

	if kubeconfig != "" {
		k8sClient, _, err = client.GetKubeClientWithConfig(kubeconfig)
	} else {
		k8sClient, _, err = client.GetKubeClient()
	}
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to get kubernetes client")
	}

	readCtx, cancel := context.WithTimeout(ctx, defaults.ConfigMapWriteTimeout)
	defer cancel()
	cm, err := k8sClient.CoreV1().ConfigMaps(namespace).Get(readCtx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("failed to get ConfigMap %s/%s", namespace, name), err)
	}

	// Try to get format from ConfigMap metadata
	format := FormatYAML // default
	if formatStr, ok := cm.Data["format"]; ok {
		format = Format(formatStr)
	}

	// Try to read data with format-specific key first
	var content string
	dataKey := fmt.Sprintf("snapshot.%s", format)
	if data, ok := cm.Data[dataKey]; ok {
		content = data
	} else {
		// Fall back to trying all known extensions
		for _, ext := range []string{"yaml", "json", "txt"} {
			if data, ok := cm.Data[fmt.Sprintf("snapshot.%s", ext)]; ok {
				content = data
				format = Format(ext)
				break
			}
		}
		if content == "" {
			return nil, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("ConfigMap %s/%s has no snapshot data", namespace, name))
		}
	}

	slog.Debug("reading from ConfigMap",
		"namespace", namespace,
		"name", name,
		"format", format,
		"size", len(content))

	// Deserialize content
	reader, err := NewReader(format, strings.NewReader(content))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create reader for ConfigMap data", err)
	}

	var result T
	if err := reader.Deserialize(&result); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to deserialize ConfigMap data", err)
	}

	return &result, nil
}
