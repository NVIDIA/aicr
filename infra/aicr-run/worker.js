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

// aicr-go-discovery answers Go's module discovery requests for aicr.run.
// `go get aicr.run/<path>` fetches https://aicr.run/<path>?go-get=1 and reads
// the go-import meta tag, which maps the aicr.run module root to this
// repository. Every path gets the same tag. Other requests pass through; the
// zone's Redirect Rules send browsers to the repository before this runs.

const discoveryHTML = `<!doctype html>
<html>
<head>
<meta name="go-import" content="aicr.run git https://github.com/NVIDIA/aicr">
<title>AICR Go module discovery</title>
</head>
<body></body>
</html>
`;

export default {
  async fetch(request) {
    const url = new URL(request.url);
    const discovery =
      (request.method === "GET" || request.method === "HEAD") &&
      url.hostname === "aicr.run" &&
      url.searchParams.get("go-get") === "1";
    if (!discovery) {
      return fetch(request);
    }
    return new Response(request.method === "HEAD" ? null : discoveryHTML, {
      headers: {
        "Content-Type": "text/html; charset=utf-8",
        "Cache-Control": "no-store",
        "X-Content-Type-Options": "nosniff",
      },
    });
  },
};
