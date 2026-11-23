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

package ctrf

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"

	"aicr.run/pkg/k8s/pod"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	corev1 "k8s.io/api/core/v1"
)

// PodImage is one image a check ran or planned to run.
type PodImage struct {
	// Image is the reference the pod spec named, or the planned reference
	// when no pod was read.
	Image string `json:"image"`
	// Digest is the registry digest (sha256:<hex>) from the container's
	// imageID. Depending on the container runtime it is the image index
	// digest or the platform manifest digest, so it identifies one image
	// only together with Platform. It is empty when no container status
	// shows a registry digest for Image's repository.
	Digest string `json:"digest"`
	// Platform is the os/arch, with optional variant, that the image's own
	// process reported running as. It is empty when the process's output was
	// not read, or reported none or conflicting ones.
	Platform string `json:"platform,omitempty"`
}

var platformPattern = regexp.MustCompile(`^[a-z0-9_]{1,32}/[a-z0-9_]{1,32}(/[a-z0-9]{1,16})?$`)

// ValidPlatform reports whether s has the os/arch form, with an optional
// variant, and bounded segment lengths. It checks only the form, not whether
// the platform exists.
func ValidPlatform(s string) bool {
	return platformPattern.MatchString(s)
}

// PlatformLines folds the PlatformLinePrefix payloads of one process's output.
// A process reports one platform, so lines naming two different platforms are
// a conflict and leave the platform unknown. The zero value has seen no line.
type PlatformLines struct {
	platform string
	conflict bool
}

// Add folds the payload of one PlatformLinePrefix line. A payload that is not
// a JSON string with the os/arch form is logged and ignored. Every line after
// a conflict is ignored.
func (p *PlatformLines) Add(payload string) {
	if p.conflict {
		return
	}
	var got string
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		slog.Warn("dropping malformed validator platform sentinel", "error", err)
		return
	}
	if !ValidPlatform(got) {
		slog.Warn("dropping validator platform sentinel without the os/arch form", "platform", got)
		return
	}
	switch p.platform {
	case "":
		p.platform = got
	case got:
	default:
		slog.Warn("validator reported conflicting platforms; recording none",
			"first", p.platform, "second", got)
		p.platform, p.conflict = "", true
	}
}

// Platform returns the platform every valid line named, or "" when there was
// none or the lines conflicted.
func (p *PlatformLines) Platform() string { return p.platform }

// Valid reports whether r.Image parses as an image reference, r.Digest is
// empty or a well-formed sha256 digest, and r.Platform is empty or has the
// os/arch[/variant] form. The reference grammar bounds the image's length and
// character set.
func (r PodImage) Valid() bool {
	if _, err := reference.ParseNormalizedNamed(r.Image); err != nil {
		return false
	}
	if r.Digest != "" && !isSHA256Digest(r.Digest) {
		return false
	}
	return r.Platform == "" || ValidPlatform(r.Platform)
}

// ContainerImage returns the image and registry digest of p's container named
// containerName, leaving Platform for the caller. It returns false when p is
// nil or has no such container with an image. Digest is empty when the
// container's status shows no registry digest for the image's repository.
func ContainerImage(p *corev1.Pod, containerName string) (PodImage, bool) {
	if p == nil {
		return PodImage{}, false
	}
	var img PodImage
	for _, c := range p.Spec.Containers {
		if c.Name == containerName {
			img.Image = c.Image
			break
		}
	}
	if img.Image == "" {
		return PodImage{}, false
	}
	if cs, ok := pod.ContainerStatus(p, containerName); ok {
		img.Digest = registryDigest(img.Image, cs.ImageID)
	}
	return img, true
}

// registryDigest returns the registry digest in a container status imageID
// for image, ignoring a runtime scheme prefix such as docker-pullable://. It
// returns "" when imageID names no registry manifest, as a bare sha256:<hex>
// local image ID does, or names a repository other than image's.
func registryDigest(image, imageID string) string {
	if _, rest, ok := strings.Cut(imageID, "://"); ok {
		imageID = rest
	}
	at := strings.LastIndex(imageID, "@")
	if at <= 0 || !isSHA256Digest(imageID[at+1:]) {
		return ""
	}
	// An image loaded onto the node without a registry can carry a digest
	// under a runtime-generated name, such as docker.io/library/import-<date>.
	got, err := reference.ParseNormalizedNamed(imageID[:at])
	if err != nil {
		return ""
	}
	want, err := reference.ParseNormalizedNamed(image)
	if err != nil || got.Name() != want.Name() {
		return ""
	}
	return imageID[at+1:]
}

// isSHA256Digest reports whether d is sha256:<64 lowercase hex>.
func isSHA256Digest(d string) bool {
	parsed := digest.Digest(d)
	return parsed.Validate() == nil && parsed.Algorithm() == digest.SHA256
}
