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

// Package registrytest serves in-memory OCI registries, and fakes ECR's token
// API, for tests.
package registrytest

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/inmemory"
	"github.com/sirupsen/logrus"
)

var quiet sync.Once

// New returns CNCF Distribution, the registry aicr-registry runs, backed by
// memory and with blob deletion enabled.
func New() http.Handler {
	// Distribution logs every request to logrus' standard logger, which
	// would bury a failing test's output.
	quiet.Do(func() { logrus.SetOutput(io.Discard) })
	config := &configuration.Configuration{
		Storage: configuration.Storage{
			"inmemory":    configuration.Parameters{},
			"delete":      configuration.Parameters{"enabled": true},
			"maintenance": configuration.Parameters{"uploadpurging": map[any]any{"enabled": false}},
		},
	}
	config.Log.AccessLog.Disabled = true
	return handlers.NewApp(context.Background(), config)
}
