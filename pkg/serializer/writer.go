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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
)

// Format represents the output format type
type Format string

const (
	// FormatJSON outputs data in JSON format
	FormatJSON Format = "json"
	// FormatYAML outputs data in YAML format
	FormatYAML Format = "yaml"
	// FormatTable outputs data in table format
	FormatTable Format = "table"
)

const defaultValueKey = "value"

func (f Format) IsUnknown() bool {
	switch f {
	case FormatJSON, FormatYAML, FormatTable:
		return false
	default:
		return true
	}
}

// SupportedFormats returns a list of all supported output formats
// for serialization.
func SupportedFormats() []string {
	return []string{
		string(FormatJSON),
		string(FormatYAML),
		string(FormatTable),
	}
}

// Writer handles serialization of configuration data to various formats.
// Close must be called to release file handles when using NewFileWriterOrStdout.
type Writer struct {
	format Format
	output io.Writer
	closer io.Closer
}

// NewWriter creates a new Writer with the specified format and output destination.
// If output is nil, os.Stdout will be used.
// If format is unknown, defaults to JSON format.
func NewWriter(format Format, output io.Writer) *Writer {
	if output == nil {
		output = os.Stdout
	}
	if format.IsUnknown() {
		slog.Warn("unknown format, defaulting to JSON", "format", format)
		format = FormatJSON
	}
	return &Writer{
		format: format,
		output: output,
	}
}

// NewFileWriterOrStdout creates a new Writer that outputs to the specified file path in the given format.
// If path is empty or "-", writes to stdout.
// Returns an error if the path is invalid or the file cannot be created.
// Remember to call Close() on the returned Writer to ensure the file is properly closed.
//
// Supports ConfigMap URIs in the format cm://namespace/name for Kubernetes ConfigMap output.
func NewFileWriterOrStdout(format Format, path string) (Serializer, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || trimmed == "-" || trimmed == StdoutURI {
		return NewStdoutWriter(format), nil
	}

	// Check for ConfigMap URI (cm://namespace/name)
	if strings.HasPrefix(trimmed, ConfigMapURIScheme) {
		namespace, name, err := pod.ParseConfigMapURI(trimmed)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid ConfigMap URI %q", trimmed), err)
		}
		return NewConfigMapWriter(namespace, name, format), nil
	}

	file, err := os.Create(trimmed)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create output file %q", trimmed), err)
	}

	if format.IsUnknown() {
		slog.Warn("unknown format, defaulting to JSON", "format", format)
		format = FormatJSON
	}

	return &Writer{
		format: format,
		output: file,
		closer: file,
	}, nil
}

// NewStdoutWriter creates a new Writer that outputs to stdout in the specified format.
func NewStdoutWriter(format Format) *Writer {
	if format.IsUnknown() {
		slog.Warn("unknown format, defaulting to JSON", "format", format)
		format = FormatJSON
	}
	return &Writer{
		format: format,
		output: os.Stdout,
	}
}

// Close releases any resources associated with the Writer.
// It should be called when done writing, especially for file-based writers.
// Idempotent: subsequent calls are no-ops.
func (w *Writer) Close() error {
	if w.closer == nil {
		return nil
	}
	closer := w.closer
	w.closer = nil // mark closed first so retries no-op even if Close panics
	if err := closer.Close(); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to close writer", err)
	}
	return nil
}

// Serialize outputs the given configuration data in the configured format.
// Serialize writes the configuration data in the specified format.
// Context is provided for consistency with the Serializer interface,
// but is not actively used for file/stdout writes (which are fast and blocking).
func (w *Writer) Serialize(ctx context.Context, config any) error {
	switch w.format {
	case FormatJSON:
		return w.serializeJSON(config)
	case FormatYAML:
		return w.serializeYAML(config)
	case FormatTable:
		return w.serializeTable(config)
	default:
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported format: %s", w.format))
	}
}

func (w *Writer) serializeJSON(config any) error {
	encoder := json.NewEncoder(w.output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(config); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize to JSON", err)
	}
	return nil
}

func (w *Writer) serializeYAML(config any) error {
	out, err := MarshalYAMLDeterministic(config)
	if err != nil {
		return err
	}
	if _, err := w.output.Write(out); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write YAML", err)
	}
	return nil
}

func (w *Writer) serializeTable(config any) error {
	flat := make(map[string]any)
	flattenValue(flat, reflect.ValueOf(config), "")
	if len(flat) == 0 {
		fmt.Fprintln(w.output, "<empty>")
		return nil
	}

	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	tw := tabwriter.NewWriter(w.output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintln(tw, "-----\t-----")
	for _, key := range keys {
		fmt.Fprintf(tw, "%s\t%v\n", key, flat[key])
	}
	return tw.Flush()
}

func flattenValue(out map[string]any, val reflect.Value, prefix string) {
	if !val.IsValid() {
		return
	}

	for val.Kind() == reflect.Pointer || val.Kind() == reflect.Interface {
		if val.IsNil() {
			if prefix != "" {
				out[prefix] = nil
			}
			return
		}
		val = val.Elem()
	}

	//nolint:exhaustive // We handle the common cases explicitly; all others go to default
	switch val.Kind() {
	case reflect.Struct:
		typ := val.Type()
		for i := 0; i < val.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			key := joinKey(prefix, field.Name)
			flattenValue(out, val.Field(i), key)
		}
	case reflect.Map:
		for _, mapKey := range val.MapKeys() {
			key := joinKey(prefix, fmt.Sprintf("%v", mapKey.Interface()))
			flattenValue(out, val.MapIndex(mapKey), key)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < val.Len(); i++ {
			key := joinKey(prefix, fmt.Sprintf("[%d]", i))
			flattenValue(out, val.Index(i), key)
		}
	default:
		if prefix == "" {
			prefix = defaultValueKey
		}
		out[prefix] = val.Interface()
	}
}

func joinKey(prefix, suffix string) string {
	if prefix == "" {
		return suffix
	}
	if suffix == "" {
		return prefix
	}
	return prefix + "." + suffix
}

// serializeJSON serializes data to JSON format and returns the bytes.
// This is used by ConfigMapWriter to serialize data without needing an io.Writer.
func serializeJSON(data any) ([]byte, error) {
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to serialize to JSON", err)
	}
	return content, nil
}

// serializeYAML serializes data to YAML format and returns the bytes.
// This is used by ConfigMapWriter to serialize data without needing an io.Writer.
func serializeYAML(data any) ([]byte, error) {
	return MarshalYAMLDeterministic(data)
}

// serializeTable serializes data to table format and returns the bytes.
// This is used by ConfigMapWriter to serialize data without needing an io.Writer.
func serializeTable(data any) ([]byte, error) {
	flat := make(map[string]any)
	flattenValue(flat, reflect.ValueOf(data), "")
	if len(flat) == 0 {
		return []byte("<empty>\n"), nil
	}

	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var builder strings.Builder
	tw := tabwriter.NewWriter(&builder, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintln(tw, "-----\t-----")
	for _, key := range keys {
		fmt.Fprintf(tw, "%s\t%v\n", key, flat[key])
	}
	if err := tw.Flush(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to flush table", err)
	}
	return []byte(builder.String()), nil
}

// WriteToFile writes data to a file at the specified path.
// This is a convenience function for writing raw byte data to a file.
// The file is created with 0644 permissions.
func WriteToFile(path string, data []byte) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create file", err)
	}
	defer func() {
		closeErr := file.Close()
		if err == nil && closeErr != nil {
			err = errors.Wrap(errors.ErrCodeInternal, "failed to close file", closeErr)
		}
	}()

	if _, err := file.Write(data); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write data", err)
	}

	return nil
}
