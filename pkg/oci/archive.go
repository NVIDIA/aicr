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

package oci

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/file"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

type archiveOptions struct {
	Prefix string
}

type archiveSource interface {
	io.ReadCloser
	Stat() (os.FileInfo, error)
}

type archiveDependencies struct {
	closeFile  func(*os.File) error
	closeTar   func(*tar.Writer) error
	closeGzip  func(*gzip.Writer) error
	chmodFile  func(*os.File) error
	removeFile func(*os.Root, string) error
	wrapWriter func(context.Context, io.Writer) io.Writer
	openVerify func(context.Context, *os.Root, string) (io.ReadCloser, error)
	openSource func(context.Context, *preparedSource, string) (archiveSource, error)
}

func defaultArchiveDependencies() archiveDependencies {
	return archiveDependencies{
		closeFile:  func(file *os.File) error { return file.Close() },
		closeTar:   func(writer *tar.Writer) error { return writer.Close() },
		closeGzip:  func(writer *gzip.Writer) error { return writer.Close() },
		chmodFile:  func(file *os.File) error { return file.Chmod(0o600) },
		removeFile: func(root *os.Root, name string) error { return root.Remove(name) },
		wrapWriter: func(_ context.Context, writer io.Writer) io.Writer { return writer },
		openVerify: func(_ context.Context, root *os.Root, name string) (io.ReadCloser, error) {
			return root.Open(name)
		},
		openSource: func(ctx context.Context, source *preparedSource, name string) (archiveSource, error) {
			return source.open(ctx, name)
		},
	}
}

func buildDeterministicTarGzip(
	ctx context.Context,
	source *preparedSource,
	layout *ownedLayout,
	opts archiveOptions,
) (string, ociv1.Descriptor, error) {

	return buildDeterministicTarGzipWithDependencies(
		ctx, source, layout, opts, defaultArchiveDependencies())
}

func buildDeterministicTarGzipWithDependencies(
	ctx context.Context,
	source *preparedSource,
	layout *ownedLayout,
	opts archiveOptions,
	deps archiveDependencies,
) (archiveName string, descriptor ociv1.Descriptor, retErr error) {

	if err := contextError(ctx, "archive creation canceled"); err != nil {
		return "", ociv1.Descriptor{}, err
	}
	if source == nil {
		return "", ociv1.Descriptor{}, apperrors.New(apperrors.ErrCodeInternal, "prepared source is required")
	}
	if layout == nil || layout.child == nil {
		return "", ociv1.Descriptor{}, apperrors.New(apperrors.ErrCodeInternal, "owned OCI layout is required")
	}
	if err := layout.validate(); err != nil {
		return "", ociv1.Descriptor{}, err
	}
	if err := source.validate(); err != nil {
		return "", ociv1.Descriptor{}, err
	}
	if opts.Prefix != "" {
		if err := validateSelectedPath(opts.Prefix); err != nil || strings.Contains(opts.Prefix, "/") {
			return "", ociv1.Descriptor{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
				"archive prefix must be a safe single path segment")
		}
	}
	name, archiveFile, err := createArchiveFile(ctx, layout.child, deps)
	if err != nil {
		return "", ociv1.Descriptor{}, err
	}
	archiveName = name
	complete := false
	defer func() {
		if complete {
			return
		}
		if removeErr := deps.removeFile(layout.child, name); removeErr != nil && !os.IsNotExist(removeErr) {
			retErr = stderrors.Join(retErr,
				apperrors.Wrap(apperrors.ErrCodeInternal, "failed to remove partial archive", removeErr))
		}
		archiveName = ""
		descriptor = ociv1.Descriptor{}
	}()

	contentDigester := digest.SHA256.Digester()
	tarDigester := digest.SHA256.Digester()
	archiveOutput := deps.wrapWriter(ctx, io.MultiWriter(archiveFile, contentDigester.Hash()))
	counted := &countingWriter{writer: archiveOutput}
	gzipWriter, err := gzip.NewWriterLevel(counted, gzip.BestCompression)
	if err != nil {
		_ = deps.closeFile(archiveFile)
		return "", ociv1.Descriptor{}, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create gzip writer", err)
	}
	gzipWriter.ModTime = time.Time{}
	gzipWriter.OS = 255
	tarWriter := tar.NewWriter(io.MultiWriter(gzipWriter, tarDigester.Hash()))

	retErr = writeArchiveEntries(ctx, source, tarWriter, opts, deps)

	tarCloseErr := deps.closeTar(tarWriter)
	gzipCloseErr := deps.closeGzip(gzipWriter)
	fileCloseErr := deps.closeFile(archiveFile)
	if retErr != nil || tarCloseErr != nil || gzipCloseErr != nil || fileCloseErr != nil {
		combined := stderrors.Join(retErr, tarCloseErr, gzipCloseErr, fileCloseErr)
		return "", ociv1.Descriptor{}, wrapContextualIOFailure(
			ctx, combined, apperrors.ErrCodeInternal, "failed to finalize archive")
	}

	descriptor = ociv1.Descriptor{
		MediaType: ociv1.MediaTypeImageLayerGzip,
		Digest:    contentDigester.Digest(),
		Size:      counted.count,
		Annotations: map[string]string{
			file.AnnotationDigest: tarDigester.Digest().String(),
			file.AnnotationUnpack: "true",
			// AnnotationTitle names the on-disk destination an ORAS file
			// store (oras.Copy into file.New(dir)) unpacks this layer to.
			// The generic archive packs the source tree at the tar root,
			// so the store must unpack it into its own root (".") — without
			// this the store treats the layer as an unnamed blob and writes
			// NOTHING to disk, and a downstream file-store pull (e.g. the
			// evidence verifier's materialize step) sees an empty directory
			// and fails "does not contain a recognizable summary bundle".
			// The Helm path sets its own title (the chart dir); only this
			// generic path regressed when the annotation was dropped.
			ociv1.AnnotationTitle: ".",
		},
	}
	if err := verifyArchiveDescriptor(ctx, layout.child, name, descriptor, deps); err != nil {
		return "", ociv1.Descriptor{}, err
	}
	if err := source.validate(); err != nil {
		return "", ociv1.Descriptor{}, err
	}
	if err := layout.validate(); err != nil {
		return "", ociv1.Descriptor{}, err
	}
	complete = true
	return archiveName, descriptor, nil
}

func writeArchiveEntries(
	ctx context.Context,
	source *preparedSource,
	tarWriter *tar.Writer,
	opts archiveOptions,
	deps archiveDependencies,
) error {

	files := source.relativeFiles()
	sort.Strings(files)
	writtenDirs := make(map[string]struct{})
	for _, rel := range files {
		if err := contextError(ctx, "archive creation canceled"); err != nil {
			return err
		}
		entryName := archiveEntryName(opts.Prefix, rel)
		for _, dir := range archiveParentDirectories(entryName) {
			entry := dir + "/"
			if _, ok := writtenDirs[entry]; ok {
				continue
			}
			if err := tarWriter.WriteHeader(&tar.Header{
				Name:     entry,
				Mode:     0o755,
				Typeflag: tar.TypeDir,
			}); err != nil {
				return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to write archive directory", err)
			}
			writtenDirs[entry] = struct{}{}
		}
		inputFile, err := deps.openSource(ctx, source, rel)
		if err != nil {
			return err
		}
		input := newContextReadCloser(ctx, inputFile)
		info, entryErr := inputFile.Stat()
		if entryErr == nil {
			entryErr = tarWriter.WriteHeader(&tar.Header{
				Name:     entryName,
				Mode:     int64(info.Mode().Perm()),
				Size:     info.Size(),
				Typeflag: tar.TypeReg,
			})
		}
		if entryErr == nil {
			_, entryErr = copyWithContext(ctx, tarWriter, input)
		}
		inputCloseErr := input.Close()
		if entryErr != nil || inputCloseErr != nil {
			return wrapContextualIOFailure(ctx, stderrors.Join(entryErr, inputCloseErr),
				apperrors.ErrCodeInternal, "failed to archive staged source file")
		}
	}
	return nil
}

func createArchiveFile(
	ctx context.Context,
	root *os.Root,
	deps archiveDependencies,
) (string, *os.File, error) {

	for range 128 {
		if err := contextError(ctx, "archive creation canceled"); err != nil {
			return "", nil, err
		}
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to generate archive name", err)
		}
		name := "bundle-" + hex.EncodeToString(random[:]) + ".tar.gz"
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if chmodErr := deps.chmodFile(file); chmodErr != nil {
				closeErr := deps.closeFile(file)
				removeErr := deps.removeFile(root, name)
				result := error(apperrors.Wrap(apperrors.ErrCodeInternal, "failed to secure archive", chmodErr))
				if closeErr != nil {
					result = stderrors.Join(result, apperrors.Wrap(
						apperrors.ErrCodeInternal, "failed to close rejected archive", closeErr))
				}
				if removeErr != nil {
					result = stderrors.Join(result, apperrors.Wrap(
						apperrors.ErrCodeInternal, "failed to remove rejected archive", removeErr))
				}
				return "", nil, result
			}
			return name, file, nil
		}
		if !os.IsExist(err) {
			return "", nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create archive", err)
		}
	}
	return "", nil, apperrors.New(apperrors.ErrCodeInternal, "failed to allocate a unique archive")
}

func archiveParentDirectories(rel string) []string {
	parent := path.Dir(rel)
	if parent == "." {
		return nil
	}
	return parentDirectories(parent)
}

func archiveEntryName(prefix, rel string) string {
	if prefix == "" {
		return rel
	}
	return prefix + "/" + rel
}

type countingWriter struct {
	writer io.Writer
	count  int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.count += int64(n)
	return n, err
}

func verifyArchiveDescriptor(
	ctx context.Context,
	root *os.Root,
	name string,
	want ociv1.Descriptor,
	deps archiveDependencies,
) (retErr error) {

	file, err := deps.openVerify(ctx, root, name)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to reopen completed archive", err)
	}
	reader := newContextReadCloser(ctx, file)
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			retErr = stderrors.Join(retErr,
				apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close archive verification reader", closeErr))
		}
	}()
	digester := digest.SHA256.Digester()
	size, err := copyWithContext(ctx, digester.Hash(), reader)
	if err != nil {
		return wrapContextualIOFailure(ctx, err,
			apperrors.ErrCodeInternal, "failed to verify completed archive")
	}
	if size != want.Size || digester.Digest() != want.Digest {
		return apperrors.New(apperrors.ErrCodeInternal, "completed archive descriptor mismatch")
	}
	return nil
}
