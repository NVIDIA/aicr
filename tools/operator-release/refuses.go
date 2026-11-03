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
	"context"
	stderrors "errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// refusalTimeout bounds each run of the candidate's binary.
const refusalTimeout = 60 * time.Second

// developmentSettings are the operator's development settings and their flags.
var developmentSettings = []struct{ name, flag string }{
	{"bypassApprovals", "--development-bypass-approvals"},
	{"skipDiscovery", "--development-skip-discovery"},
}

// hostPlatforms maps a host's GOOS/GOARCH to the image platform it can run.
var hostPlatforms = map[string]string{
	"linux/amd64": "linux/amd64",
	"linux/arm64": "linux/arm64/v8",
}

// hostPlatform returns the image platform host can run. On a host that cannot
// run the image's binary it returns "", or an error when ci is set.
func hostPlatform(host string, ci bool) (string, error) {
	if platform, ok := hostPlatforms[host]; ok {
		return platform, nil
	}
	if ci {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"the image's binary does not run on "+host+", so CI cannot check that the candidate refuses development settings")
	}
	return "", nil
}

// refuses reports an error unless the candidate image's operator binary
// refuses every development setting. Without -platform it runs the host's
// platform, and skips the check outside CI on a host that cannot run the
// binary.
func refuses(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("refuses", flag.ContinueOnError)
	dir := fs.String("dir", "dist/candidate", "Candidate directory.")
	platform := fs.String("platform", "", "Image platform to run. Defaults to the host's.")
	settingsFile := fs.String("settings", ".settings.yaml", "Pins.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *platform == "" {
		host := runtime.GOOS + "/" + runtime.GOARCH
		var err error
		if *platform, err = hostPlatform(host, os.Getenv("CI") != ""); err != nil {
			return err
		}
		if *platform == "" {
			_, err = fmt.Fprintf(out, "skipping the development settings refusal check, since the image's binary does not run on %s\n", host)
			return err
		}
	}
	s, err := readSettings(*settingsFile)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "read the pinned AICR image")
	}
	work, err := os.MkdirTemp("", "operator-refuses-")
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "create the refusal check's work directory")
	}
	defer os.RemoveAll(work)
	bin := filepath.Join(work, "aicr-operator")
	if err = extract([]string{"-dir", *dir, "-platform", *platform, "-out", bin}); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "extract the candidate's "+*platform+" operator binary")
	}
	for _, d := range developmentSettings {
		if err = refusesSetting(bin, filepath.Join(work, "no-kubeconfig"), s.Operator.AICRImage, d.name, d.flag); err != nil {
			return err
		}
		if _, err = fmt.Fprintf(out, "the candidate's %s binary refuses development.%s\n", *platform, d.name); err != nil {
			return err
		}
	}
	return nil
}

// refusesSetting reports an error unless bin, run with flag, exits refusing
// development.<name>. kubeconfig must name a file that does not exist.
func refusesSetting(bin, kubeconfig, aicrImage, name, flag string) error {
	ctx, cancel := context.WithTimeout(context.Background(), refusalTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, flag, "--aicr-version=v1.0.0", "--aicr-image="+aicrImage,
		"--registry-repository=oci://registry.example.com/aicr/cluster")
	// No cluster is reachable, so a binary that failed to refuse cannot act
	// on one, and the timeout ends it.
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig, "KUBERNETES_SERVICE_HOST=", "KUBERNETES_SERVICE_PORT=")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return errors.New(errors.ErrCodeTimeout,
			fmt.Sprintf("the candidate did not refuse development.%s within %s: %s", name, refusalTimeout, output))
	case err == nil:
		return errors.New(errors.ErrCodeInvalidRequest, "the candidate started with development."+name)
	case !stderrors.As(err, &exit):
		return errors.Wrap(errors.ErrCodeInternal, "run the candidate with development."+name, err)
	case !strings.Contains(string(output), "development."+name+" is accepted only by development builds"):
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("the candidate failed without refusing development.%s: %s", name, output))
	}
	return nil
}
