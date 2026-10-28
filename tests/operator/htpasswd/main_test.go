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

package main

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestLine(t *testing.T) {
	t.Parallel()
	got, err := line("aicr-e2e", strings.NewReader("s3cret\n"))
	if err != nil {
		t.Fatalf("line() = %v", err)
	}
	user, hash, _ := strings.Cut(got, ":")
	if user != "aicr-e2e" || bcrypt.CompareHashAndPassword([]byte(hash), []byte("s3cret")) != nil {
		t.Fatalf("line() = %q", got)
	}
	for _, bad := range []struct{ user, in string }{{"", "p\n"}, {"a:b", "p\n"}, {"u", "\n"}} {
		if _, err := line(bad.user, strings.NewReader(bad.in)); err == nil {
			t.Errorf("line(%q, %q) accepted", bad.user, bad.in)
		}
	}
}
