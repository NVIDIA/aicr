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

// Command htpasswd prints a bcrypt htpasswd line for the test registry that
// tools/operator-kind runs with authentication. It reads the password from
// stdin so it never appears in a process list.
//
// Usage: htpasswd <user> < password
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: htpasswd <user> < password")
		os.Exit(2)
	}
	out, err := line(os.Args[1], os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(out)
}

// line hashes the first line of in for user, in the htpasswd format the
// registry accepts (bcrypt only).
func line(user string, in io.Reader) (string, error) {
	password, err := bufio.NewReader(in).ReadString('\n')
	password = strings.TrimSpace(password)
	if (err != nil && !errors.Is(err, io.EOF)) || password == "" || user == "" || strings.Contains(user, ":") {
		return "", errors.New("need a user without ':' and a password on stdin")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return user + ":" + string(hash), nil
}
