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

package errors

import (
	"errors"
	"net"
	"strings"
	"syscall"
)

// networkErrStrings are substrings that indicate a network-level error when
// found in an error message. Used as a fallback when typed checks don't match
// (e.g. the error is wrapped inside a URL error or similar).
var networkErrStrings = []string{
	"dial tcp",
	"no such host",
	"connection refused",
	"i/o timeout",
	"TLS handshake timeout",
	"network is unreachable",
	"no route to host",
}

// IsNetworkError reports whether err indicates a network-level connectivity
// problem (DNS resolution, TCP dial, TLS handshake, etc.).
//
// It does NOT match context.DeadlineExceeded or context.Canceled — those
// represent application-level timeouts, not network failures.
func IsNetworkError(err error) bool {
	if err == nil {
		return false
	}

	// Specific network operation types (net.OpError, net.DNSError).
	// We intentionally skip the broader net.Error interface because
	// context.DeadlineExceeded and syscall.Errno also satisfy it.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno { //nolint:exhaustive // only network-related errno values
		case syscall.ECONNREFUSED, syscall.EHOSTUNREACH, syscall.ENETUNREACH:
			return true
		}
	}

	// String fallback for wrapped errors that lose type information
	msg := err.Error()
	for _, s := range networkErrStrings {
		if strings.Contains(msg, s) {
			return true
		}
	}

	return false
}
