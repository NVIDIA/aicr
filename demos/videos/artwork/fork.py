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
"""The platform fork and recipe-to-bundle sequence for AICR inference."""

import math

from . import shared as a


INK = a.INK
LIME = a.LIME
WHITE = a.WHITE
DARK = "#131d29"
SLATE = "#8295a9"
EDGE = "#4b5d72"
LIGHT_EDGE = "#9caebd"
MUTED = "#aebed0"
PAPER = "#f3f6f8"
DYNAMO_RECIPE = "h100-eks-ubuntu-inference-dynamo"
NIM_RECIPE = "h100-eks-ubuntu-inference-nim"


def _dynamo(c, x, y, s, fill):
    """Draw the alternative with a compact serving-graph symbol."""
    c.save()
    c.translate(x, y)
    c.scale(s, s)
    for y_end in (-14, 14):
        a.line(c, [(-19, 0), (-2, 0), (6, y_end), (19, y_end)], EDGE, 3)
        a.ellipse(c, 19, y_end, 7, 7, fill, INK, 1.8)
    a.ellipse(c, -19, 0, 9, 9, fill, INK, 1.8)
    c.restore()


def _nim(c, x, y, s, fill):
    """Draw the alternative with a model-container symbol."""
    c.save()
    c.translate(x, y)
    c.scale(s, s)
    a.poly(c, [(-22, -11), (0, -22), (22, -11), (0, 0)], "#ebf3f8", INK, 1.7)
    a.poly(c, [(-22, -11), (0, 0), (0, 22), (-22, 11)], fill, INK, 1.7)
    a.poly(c, [(0, 0), (22, -11), (22, 11), (0, 22)], "#7793a5", INK, 1.7)
    c.restore()


def _option(c, y, text, icon, selected, t):
    x, w, h = 803, 381, 108
    a.rr(c, x + 5, y + 8, w, h, 17, INK, alpha=0.3)
    a.rr(c, x, y, w, h, 17, "#263a2e" if selected else DARK, LIME if selected else EDGE, 2.8)
    a.ellipse(c, x + 63, y + 54, 35, 35, "#3a5540" if selected else "#43576c")
    icon(c, x + 63, y + 53, 0.92, LIME if selected else WHITE)
    a.text(c, x + 116, y + 25, text, 29, WHITE, 7)
    a.text(c, x + 117, y + 63, "Inference platform", 17, MUTED, 2)
    a.ellipse(c, x + w - 29, y + 52, 10, 10, None, LIME if selected else SLATE, 2)
    if selected:
        a.ellipse(c, x + w - 29, y + 52, 4.5 + math.sin(t * 3) * 0.6, 4.5, LIME)


def _flow_dot(c, start, end, phase, fill):
    x = a.lerp(start[0], end[0], phase)
    y = a.lerp(start[1], end[1], phase)
    a.ellipse(c, x, y, 5, 5, fill, INK, 1.5)


def _branch_dot(c, start, control_1, control_2, end, phase):
    """Keep the motion on the visible alternative branch."""
    v = 1 - phase
    coefficients = (v**3, 3 * v * v * phase, 3 * v * phase * phase, phase**3)
    points = (start, control_1, control_2, end)
    x, y = [sum(k * p[i] for k, p in zip(coefficients, points)) for i in (0, 1)]
    a.ellipse(c, x, y, 5, 5, LIME, INK, 1.5)


def choose(c, t):
    """Introduce two supported platforms for one example cluster."""
    a.bg(c, "#0c1119", "#203143", True)
    a.heading(c, t, "GPU INFERENCE", ("Choose an inference platform",), None, dark=True, size=64, y=116)
    a.soft_circuit(c, t, "#647e99", 107, 296, 445, 246, alpha=0.20)
    a.ellipse(c, 256, 525, 145, 19, INK, alpha=0.26)
    a.rack(c, 205, 519, 0.85, t)
    a.rack(c, 300, 528, 0.98, t + 0.45)
    reveal = a.reveal(t, 0.75, 0.7)
    if reveal > 0:
        a.rr(c, 101, 548, 320, 48, 12, DARK, EDGE, 1.5, alpha=reveal)
        a.text(c, 261, 560, "EKS  ·  H100  ·  Ubuntu", 21, WHITE, 7, 'center', alpha=reveal)
    if t < 2.5:
        a.robot(c, 484, 569, 0.65, t, pose="idle")
        return

    dy_on = 3.3 <= t < 6
    nim_on = 6 <= t < 8.4
    mid = (610, 386)
    a.line(c, [(394, 386), mid], SLATE, 3)
    a.curve(c, mid, (670, 386), (720, 314), (791, 314), LIME if dy_on else SLATE, 3)
    a.curve(c, mid, (670, 386), (720, 456), (791, 456), LIME if nim_on else SLATE, 3)
    a.arrow(c, 791, 314, LIME if dy_on else SLATE, 9)
    a.arrow(c, 791, 456, LIME if nim_on else SLATE, 9)
    a.ellipse(c, *mid, 24, 24, DARK, SLATE, 2.5)
    a.text(c, mid[0], mid[1] - 8, "OR", 16, WHITE, 'mono', 'center')

    if t >= 2.65:
        c.save()
        c.translate(0, 16 * (1 - a.reveal(t, 2.65, 0.6)))
        _option(c, 260, "Dynamo", _dynamo, dy_on, t)
        c.restore()
    if t >= 3.15:
        c.save()
        c.translate(0, 16 * (1 - a.reveal(t, 3.15, 0.6)))
        _option(c, 402, "NVIDIA NIM", _nim, nim_on, t)
        c.restore()
    if dy_on or nim_on:
        end_at = 314 if dy_on else 456
        phase = (t * 0.7) % 1
        _branch_dot(c, mid, (670, 386), (720, end_at), (791, end_at), 0.14 + phase * 0.83)
    a.robot(c, 500, 571, 0.65, t, pose="scan")


def _target_bar(c, t):
    a.rr(c, 70, 210, 887, 66, 15, "#dae4eb", LIGHT_EDGE, 1.8)
    a.text(c, 92, 230, "TARGET", 16, "#466074", 'mono')
    a.line(c, [(182, 226), (182, 261)], LIGHT_EDGE, 2)
    a.text(c, 203, 227, "EKS   ·   H100   ·   Ubuntu   ·   inference", 25, INK, 7)
    if t >= 3.2:
        f = a.reveal(t, 3.2, 0.6)
        a.rr(c, 721, 225, 216, 36, 9, "#e5f1d2", "#94b956", 1.2, alpha=f)
        a.text(c, 829, 233, "SELECT ONE PLATFORM", 14, "#3c5a18", 'mono', 'center', alpha=f)


def _recipe_lane(c, y, flag, title, recipe, icon, selected, t, cue):
    x, w, h = 70, 1137, 122
    visibility = a.reveal(t, 3.15 if cue < 10 else 3.7, 0.65)
    if visibility <= 0:
        return
    c.save()
    c.translate(0, 11 * (1 - visibility))
    a.rr(c, x + 4, y + 6, w, h, 17, INK, alpha=0.16)
    a.rr(c, x, y, w, h, 17, "#f1f7e9" if selected else PAPER, "#75a63c" if selected else LIGHT_EDGE, 2.4)
    a.ellipse(c, x + 47, y + 43, 24, 24, "#deebcd" if selected else "#dce6ee")
    icon(c, x + 47, y + 43, 0.62, LIME if selected else WHITE)
    a.text(c, x + 84, y + 15, title, 23, INK, 7)
    a.text(c, x + 84, y + 53, flag, 19, "#435c70", 'mono')
    a.line(c, [(x + 333, y + 13), (x + 333, y + 104)], LIGHT_EDGE, 1.6)
    a.line(c, [(x + 794, y + 13), (x + 794, y + 104)], LIGHT_EDGE, 1.6)

    for offset, caption in [(357, "RECIPE"), (823, "DEPLOYMENT BUNDLE")]:
        a.text(c, x + offset, y + 15, caption, 15, "#415b6f", 'mono')
    f = a.reveal(t, 7.4, 0.58)
    if f > 0:
        a.text(c, x + 357, y + 42, recipe, 17, INK, 'mono', alpha=f)
        a.text(c, x + 357, y + 75, "Component versions and settings", 17, "#4e6679", 2, alpha=f)
        a.text(c, x + 823, y + 43, "Deployer files", 18, INK, 5, alpha=f)
    a.line(c, [(x + 91, y + 104), (x + 310, y + 104)], "#9eb884" if selected else LIGHT_EDGE, 2)
    if selected:
        phase = a.between((t - cue) / 2.25)
        _flow_dot(c, (x + 93, y + 104), (x + 309, y + 104), phase, LIME)
        if t > cue + 1.2:
            f = a.reveal(t, cue + 1.2, 0.7)
            a.crate(c, x + 1032 + 12 * f, y + 58, 0.47 * f)
    c.restore()


def resolve(c, t):
    """Show AICR resolving and packaging whichever platform the user selects."""
    a.bg(c, "#c2d0de", "#edf3f8")
    a.heading(c, t, "AICR RECIPE + BUNDLE", ("One target. Pick one platform.",), None, size=58, y=110)
    _target_bar(c, t)
    a.robot(c, 1099, 276, 0.54, t, pose="scan" if t >= 3.2 else "idle")
    dy_on = 3.6 <= t < 11
    nim_on = 11 <= t
    _recipe_lane(c, 302, "--platform dynamo", "Dynamo", DYNAMO_RECIPE, _dynamo, dy_on, t, 7.8)
    _recipe_lane(c, 443, "--platform nim", "NVIDIA NIM", NIM_RECIPE, _nim, nim_on, t, 11.0)
