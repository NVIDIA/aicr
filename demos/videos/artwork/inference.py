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
"""Show two supported inference platforms through one AICR workflow."""
import math
from . import shared as a
from .fork import choose, resolve

INK = "#0b0d10"
WHITE = "#eff4f7"
LIME = "#93d22a"
SLATE = "#293849"
DIM = "#9bb0c6"
BLUE = "#a8c7e0"


def panel(c, x, y, w, h, fill):
    a.rr(c, x + 6, y + 9, w, h, 17, INK, alpha=0.23)
    a.rr(c, x, y, w, h, 17, fill, INK, 2.3)


def pipe(c, x1, x2, y, t, active=True):
    a.line(c, [(x1, y), (x2, y)], "#647d95", 2.4)
    a.arrow(c, x2, y, "#647d95", 7)
    if active:
        pos = (t * 0.38 + x1 * 0.002) % 1
        a.ellipse(c, a.lerp(x1 + 3, x2 - 10, pos), y, 5, 5, LIME, INK, 1.3)


def deploy_validate(c, t):
    a.bg(c, "#0d141c", "#263747", True)
    a.heading(c, t, "AICR  /  DEPLOYMENT CHECKS", ["Apply the bundle. Check the stack."], None, True, 56, 133)
    a.ellipse(c, 634, 464, 514, 82, "#526982", alpha=0.30)
    a.crate(c, 250, 375, 1.18)
    a.robot(c, 424, 496, 0.72, t, "carry" if t < 6 else "scan")
    pipe(c, 515, 585, 379, t)
    a.rack(c, 676, 467, 0.76, t)
    pipe(c, 755, 877, 379, t, t > 5.7)
    panel(c, 895, 282, 280, 175, "#e6edf3")
    a.text(c, 1035, 303, "Deployment checks", 20, INK, 8, "center")
    for i, line in enumerate(("Expected components", "Component health", "Configuration")):
        y = 346 + 30 * i
        passed = t > 6 + i * 0.9
        a.ellipse(c, 918, y + 7, 8.2, 8.2, LIME if passed else "#bac9d6", INK, 1.1)
        if passed:
            a.tick(c, 918, y + 7, INK, 0.52, 2.3)
        a.text(c, 937, y, line, 16, INK, 2)
    for x, label in [(250, "Chosen bundle"), (676, "Your deployer applies it"), (1035, "AICR validates the stack")]:
        a.text(c, x, 497, label, 20, WHITE, 2, "center")
    p = a.reveal(t, 6.1, 0.71)
    if p > 0.02:
        a.rr(c, 244, 541, 792, 41, 20, "#18232d", "#7995aa", 1.6, p)
        a.text(
            c,
            640,
            553,
            "aicr validate --recipe ./bundle/recipe.yaml --phase deployment",
            17,
            WHITE,
            "mono",
            "center",
            p,
        )


def workload_panel(c, x, y, w, title, kind, what, t, blue=False):
    tint = "#dceaf1" if blue else "#e8f1d3"
    panel(c, x, y, w, 169, WHITE)
    a.rr(c, x + 13, y + 14, w - 26, 35, 10, tint)
    a.text(c, x + w / 2, y + 22, title, 21, INK, 8, "center")
    a.text(c, x + w / 2, y + 73, kind, 17, INK, "mono", "center", a.reveal(t, 0.5, 0.7))
    a.text(c, x + w / 2, y + 116, what, 16, "#42586e", 2, "center", a.reveal(t, 1.03, 0.72))


def serve(c, t):
    a.bg(c, "#c9d6e1", "#eef3f6")
    a.heading(c, t, "YOUR MODEL WORKLOAD", ["Send a request to the chosen endpoint"], None, False, 50, 130)
    a.ellipse(c, 643, 429, 439, 115, "#a7bed1", alpha=0.34)
    workload_panel(c, 70, 267, 392, "Dynamo", "DynamoGraphDeployment", "CPU frontend and GPU worker", t, blue=True)
    workload_panel(c, 815, 267, 391, "NVIDIA NIM", "NIMService", "Model server and namespace credentials", t)
    a.ellipse(c, 636, 445, 107, 16, "#698da4", alpha=0.4)
    a.robot(c, 636, 447, 0.84, t, "scan" if t < 8.6 else "cheer")
    a.text(c, 636, 225, "Choose the matching workload", 17, "#4b6174", 2, "center")
    a.curve(c, (505, 435), (494, 493), (563, 516), (601, 516), "#7290a5", 2.5, 0.79, [5, 7])
    a.curve(c, (766, 435), (779, 493), (708, 516), (670, 516), "#7290a5", 2.5, 0.79, [5, 7])
    v = a.reveal(t, 8.45, 0.67)
    if v > 0.01:
        a.rr(c, 324, 500 + 14 * (1 - v), 625, 74, 16, "#222e3a", INK, 2.5, v)
        a.text(c, 636, 512 + 14 * (1 - v), "Example smoke test", 14, BLUE, 2, "center", v)
        outcome = "  →  200 OK" if t >= 11.1 else ""
        a.text(c, 636, 539 + 14 * (1 - v), "POST /v1/chat/completions" + outcome, 22, WHITE, "mono", "center", v)
        a.spark(c, 974, 533, 10 + 2 * math.sin(t * 2), LIME, alpha=v)


SCENES = (choose, resolve, deploy_validate, serve)
