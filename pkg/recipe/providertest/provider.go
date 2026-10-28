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

// Package providertest provides a recipe.DataProvider over an fs.FS for
// tests. It must not import pkg/recipe, so that package's own tests can use
// it.
package providertest

import (
	"context"
	"io/fs"
	"path"
	"testing/fstest"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Provider is a recipe.DataProvider over an fs.FS that reads and walks like
// recipe.EmbeddedDataProvider. Its Source returns "providertest:" + path
// unless set with WithSource.
type Provider struct {
	fsys   fs.FS
	source func(path string) string
}

// Option configures a Provider.
type Option func(*Provider)

// WithSource sets the function Source calls.
func WithSource(source func(path string) string) Option {
	return func(p *Provider) { p.source = source }
}

// New returns a Provider over fsys.
func New(fsys fs.FS, opts ...Option) *Provider {
	p := &Provider{fsys: fsys, source: func(path string) string { return "providertest:" + path }}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// NewMap returns a Provider over files, keyed by slash-separated path.
func NewMap(files map[string][]byte, opts ...Option) *Provider {
	fsys := fstest.MapFS{}
	for name, data := range files {
		fsys[name] = &fstest.MapFile{Data: data}
	}
	return New(fsys, opts...)
}

// ReadFile returns the content of name, or an error if ctx is done. Errors
// from the fs.FS, such as fs.ErrNotExist, are returned unwrapped.
func (p *Provider) ReadFile(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "context done before reading "+name)
	}
	return fs.ReadFile(p.fsys, path.Clean(name))
}

// WalkDir walks the tree rooted at root. It stops with an error once ctx is
// done.
func (p *Provider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	return fs.WalkDir(p.fsys, path.Clean(root), func(name string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout, "context done during walk of "+root)
		}
		return fn(name, d, err)
	})
}

// Source describes where path came from.
func (p *Provider) Source(path string) string {
	return p.source(path)
}
