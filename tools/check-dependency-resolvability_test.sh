#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

python3 - <<'PY'
import io
import os
import pathlib
import types
import urllib.error
from email.message import Message

path = pathlib.Path(".github/scripts/check-dependency-resolvability.py")
source = path.read_text()
module = types.ModuleType("dependency_resolvability")
module.__dict__["__file__"] = str(path)
exec(compile(source.replace("sys.exit(main())", "pass"), str(path), "exec"), module.__dict__)
real_http_get = module.http_get

assert module.escape_proxy("github.com/Masterminds/semver") == "github.com/!masterminds/semver"
assert module.escape_proxy("v1.2.3-RC1") == "v1.2.3-!r!c1"
assert module.escape_proxy("mód") == "mód"

parsed = module.parse_go_sum(
    "example.com/Mod v1.2.3-RC1 h1:zip\n"
    "example.com/Mod v1.2.3-RC1/go.mod h1:mod\n"
)
expected = frozenset(parsed[("example.com/Mod", "v1.2.3-RC1")])
assert len(expected) == 2, expected

calls = []
def matching_get(url):
    calls.append(url)
    if url.startswith(module.PROXY):
        return b"{}", "200"
    return ("0\n" + "\n".join(sorted(expected)) + "\n").encode(), "200"

module.http_get = matching_get
_, _, problems = module.check(("example.com/Mod", "v1.2.3-RC1", expected))
assert not problems, problems
assert any("@v/v1.2.3-!r!c1.info" in url for url in calls), calls

module.http_get = lambda url: (b"{}", "200") if url.startswith(module.PROXY) else (b"0\n", "200")
_, _, problems = module.check(("example.com/Mod", "v1.2.3-RC1", expected))
assert any("disagrees with go.sum" in problem for problem in problems), problems

os.environ["GONOSUMDB"] = "example.com/*"
module.http_get = lambda url: (b"{}", "200")
_, _, problems = module.check(("example.com/Mod", "v1.2.3-RC1", expected))
assert not problems, problems
os.environ.pop("GONOSUMDB")

headers = Message()
headers["Retry-After"] = "0"
attempts = []
sleeps = []
class Response(io.BytesIO):
    status = 200
    headers = Message()
    def __enter__(self):
        return self
    def __exit__(self, *args):
        return False

def retrying_urlopen(*args, **kwargs):
    attempts.append(1)
    if len(attempts) == 1:
        raise urllib.error.HTTPError("https://example.invalid", 429, "limited", headers, None)
    return Response(b"ok")

module.urllib.request.urlopen = retrying_urlopen
module.time.sleep = sleeps.append
module.http_get = real_http_get
body, detail = module.http_get("https://example.invalid")
assert body == b"ok" and detail == "200", (body, detail)
assert len(attempts) == 2 and sleeps == [0.0], (attempts, sleeps)

print("dependency resolvability tests passed")
PY
