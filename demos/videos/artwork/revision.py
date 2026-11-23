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
"""Operator preview. Follow the revision that owns the green signal."""
import math
from . import shared as a

INK = "#0b0d10"
PAPER = "#eff4f7"
ACCENT = "#93d22a"
SILVER = "#b9c5d0"
AMBER = "#edcf92"


def base(c, t, title, dark):
    a.bg(c, INK if dark else "#c7d3df", "#29313a" if dark else "#eef3f7", dark)
    a.heading(c, t + 0.36, "AICR OPERATOR  /  PREVIEW", [title], None, dark, 60, 139)


def surface(c, x, y, w, h, fill=PAPER):
    a.rr(c, x + 7, y + 9, w, h, 18, INK, alpha=0.22)
    a.rr(c, x, y, w, h, 18, fill, INK, 3)


def check(c, x, y, ready, blocked=False):
    a.ellipse(c, x, y, 12, 12, ACCENT if ready else AMBER if blocked else "#cbd5de", INK, 1.6)
    if ready:
        a.tick(c, x, y, INK, 0.7, 2.7)
    elif blocked:
        a.line(c, [(x - 4, y), (x + 4, y)], INK, 2.8)


def lift_lens(c, x, y, t):
    c.save()
    c.translate(x, y + 1.5 * math.sin(t * 3))
    a.line(c, [(40, 39), (86, 83)], INK, 23)
    a.line(c, [(40, 39), (86, 83)], "#536b82", 13)
    a.ellipse(c, 0, 0, 56, 56, "#77a4bb", INK, 6, 0.46)
    a.ellipse(c, 0, 0, 49, 49, None, "#b9d9e5", 1.7, 0.85)
    a.curve(c, (-35, -22), (-26, -35), (-20, -39), (-5, -40), "#f6fdff", 4, 0.86)
    c.restore()


def stare(c, t):
    base(c, t, "Healthy at which revision?", True)
    a.ellipse(c, 831, 438, 346, 139, "#526578", alpha=0.38)
    a.soft_circuit(c, t, "#9eb9cd", 639, 302, 479, 268, 0.22)
    a.robot(c, 325, 546, 1.03, t, "scan" if t < 5.9 else "worried")
    surface(c, 521, 268, 612, 253)
    a.rr(c, 544, 287, 566, 52, 10, "#24313c")
    a.text(c, 567, 299, "Illustrative Argo CD status", 24, PAPER, 2)
    a.text(c, 570, 364, "Synced and healthy", 39, INK, 8)
    check(c, 979, 387, True)
    a.line(c, [(562, 412), (1093, 412)], "#aab8c6", 1.5)
    a.text(c, 567, 433, "Last reported revision", 17, "#42566a", 2)
    a.text(c, 855, 430, "sha256:…a1", 23, INK, "mono")
    scan = a.reveal(t, 3.4, 1.7)
    if scan > 0.01:
        lift_lens(c, a.lerp(707, 983, scan), a.lerp(373, 441, scan), t)
    d = a.reveal(t, 6.3, 0.58)
    if d > 0.01:
        a.rr(c, 744, 478, 289, 32, 16, AMBER, INK, 1.4, d)
        a.text(c, 889, 486, "Previous revision", 17, INK, 8, "center", d)


def wire(c, x1, x2, y, t, active):
    a.line(c, [(x1, y), (x2, y)], "#647d95", 2.7)
    a.arrow(c, x2, y, "#647d95", 7)
    if active:
        q = (t * 0.37) % 1
        a.ellipse(c, a.lerp(x1, x2 - 11, q), y, 4.8, 4.8, ACCENT, INK, 1.2)


def lane(c, x, y, w, title, subtitle, token):
    surface(c, x, y, w, 97)
    a.text(c, x + 21, y + 15, title, 21, INK, 8)
    a.text(c, x + 21, y + 50, subtitle, 16, "#4b6073", 2)
    if token:
        check(c, x + w - 25, y + 48, token == "pass", token == "hold")


def waiting(c, t):
    base(c, t, "The next release waits", False)
    a.ellipse(c, 675, 426, 525, 131, "#8ca3b9", alpha=0.21)
    a.robot(c, 206, 532, 0.84, t, "scan")
    wire(c, 323, 424, 384, t, True)
    wire(c, 756, 872, 384, t, False)
    lane(c, 440, 334, 307, "Apply new revision", "Target chart  sha256:…b2", "")
    lane(c, 890, 334, 321, "Dependent release", "Waiting for readiness", "hold")
    a.text(c, 597, 293, "Argo CD", 18, "#4b6073", 2, "center")
    a.text(c, 1051, 293, "AICR rollout queue", 18, "#4b6073", 2, "center")
    p = a.reveal(t, 4.55, 0.75)
    if p > 0.01:
        a.rr(c, 442, 462, 304, 41, 20, AMBER, INK, 2, p)
        a.text(c, 594, 472, "Last report still …a1", 18, INK, 8, "center", p)
        for idx in range(3):
            a.ellipse(c, 578 + 16 * idx, 521, 3.3, 3.3, "#899eaf", alpha=0.49 + 0.45 * math.sin(t * 3 + idx * 0.66))


def ready_check(c, t):
    base(c, t, "What counts as ready?", True)
    a.ellipse(c, 737, 417, 462, 147, "#4c6678", alpha=0.4)
    a.soft_circuit(c, t, "#9cb2c9", 358, 252, 804, 293, 0.2)
    a.robot(c, 235, 544, 0.9, t, "scan")
    surface(c, 430, 244, 707, 308, "#eaf0f4")
    rows = (
        ("Expected chart digest", "sha256:…b2", 1.8),
        ("Current spec and values", "Compared", 4.38),
        ("Fresh sync or reconcile", "Since this step began", 7.05),
        ("Healthy, clear of blocking errors", "Ready", 10.1),
    )
    for i, (what, state, delay) in enumerate(rows):
        y = 274 + i * 62
        on = t > delay
        check(c, 463, y + 19, on)
        a.text(c, 498, y + 5, what, 23, INK, 2)
        p = a.reveal(t, delay, 0.47)
        a.text(c, 1099, y + 8, state, 17, "#527523" if on else "#657587", "mono", "right", max(0.25, p))
        if i < len(rows) - 1:
            a.line(c, [(455, y + 47), (1110, y + 47)], "#bcc8d4", 1.6)
    if t > 10.5:
        a.spark(c, 1171, 304, 14 * (0.8 + 0.2 * math.sin(t * 3)), ACCENT)


def advance(c, t):
    base(c, t, "Now the rollout advances", False)
    a.ellipse(c, 691, 414, 447, 131, "#8ca3b9", alpha=0.25)
    a.robot(c, 203, 543, 0.92, t, "scan" if t < 6.4 else "cheer")
    wire(c, 319, 417, 388, t, True)
    p = a.reveal(t, 2.0, 0.67)
    wire(c, 760, 876, 388, t, p > 0.02)
    lane(c, 430, 334, 317, "GPU release ready", "Synced and healthy  …b2", "pass")
    lane(
        c, 888, 334, 323, "Dependent release", "Next rollout step" if p > 0.5 else "Waiting", "" if p > 0.5 else "hold"
    )
    v = a.reveal(t, 7.0, 0.75)
    if v > 0.01:
        a.rr(c, 476, 479 + 9 * (1 - v), 676, 45, 21, "#e1edce", "#7c9f53", 1.75, v)
        a.text(c, 814, 490 + 9 * (1 - v), "When enabled, validate the finished stack", 21, INK, 8, "center", v)


SCENES = (stare, waiting, ready_check, advance)
