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
"""Draw frames of the AICR video series on a 1280 by 720 canvas."""

from bisect import bisect_right

from . import boards, dynamo_reference, inference, nim_reference, revision, trust
from . import shared as a
from .shared import HEIGHT, SCALE, WIDTH, wrap_caption

__all__ = ["HEIGHT", "SCALE", "WIDTH", "draw", "wrap_caption"]

_CUSTOM_SCENES = {
    "overview": a.OVERVIEW_SCENES,
    "trust": trust.SCENES,
    "revision": revision.SCENES,
    "inference": inference.SCENES,
    "dynamo_reference": dynamo_reference.SCENES,
    "nim_reference": nim_reference.SCENES,
}
_OUTRO_COPY = {
    "revision": "Check the Operator readiness guide",
    "inference": "Compare the inference examples",
    "dynamo_reference": "Try the Dynamo inference walkthrough",
    "nim_reference": "Try the NVIDIA NIM workload example",
}


def draw(ctx, spec, edges, cues, t):
    """Draw one frame and its caption on a Cairo context scaled by SCALE.

    Edges include the first and last frame boundaries. Each cue contains
    an inclusive start, exclusive end, and its wrapped text lines.
    """
    mode = spec.get("renderer", "batch")
    if mode == "batch":
        boards.draw(ctx, spec, edges, cues, t)
        return
    scenes = _CUSTOM_SCENES[mode]
    end_copy = _OUTRO_COPY.get(mode)
    count = len(scenes) + (end_copy is not None)
    if len(edges) != count + 1:
        raise ValueError(f"{mode} needs {count + 1} scene boundaries")
    index = bisect_right(edges, t) - 1
    if not 0 <= index < count:
        raise ValueError(f"frame time {t:g} is outside the episode")
    local, remaining = t - edges[index], edges[index + 1] - t
    if index < len(scenes):
        scenes[index](ctx, local)
    else:
        a.ending(ctx, local)
        a.rr(ctx, 69, 428, 478, 49, 8, "#15191e")
        a.text(ctx, 75, 439, end_copy, 21, "#b9c5d0", 5)
    if index > 0 and local < 0.27:
        edge = (WIDTH + 180) * a.out(local / 0.27)
        top, bottom = a.between(edge - 180, 0, WIDTH), a.between(edge, 0, WIDTH)
        a.poly(ctx, [(top, 0), (WIDTH, 0), (WIDTH, HEIGHT), (bottom, HEIGHT)], a.LIME)
    if index < count - 1 and remaining < 0.22:
        edge = (WIDTH + 180) * a.smooth(1 - remaining / 0.22)
        top, bottom = a.between(edge - 180, 0, WIDTH), a.between(edge, 0, WIDTH)
        a.poly(ctx, [(0, 0), (top, 0), (bottom, HEIGHT), (0, HEIGHT)], a.LIME)
    a.caption(ctx, t, cues)
