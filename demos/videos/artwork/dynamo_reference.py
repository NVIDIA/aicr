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
"""An illustrated request trace through Dynamo's documented aggregated example."""

import math

from . import shared as a


DARK = "#111820"
SLATE = "#263444"
EDGE = "#54657a"
DIM = "#8297af"
BLUE = "#abcbe5"
COOL = "#78b9d6"
PALE = "#eaf2f1"
GREEN = a.LIME
WHITE = a.WHITE
INK = a.INK
CONTEXT = "AICR  /  DYNAMO INFERENCE"
TITLE = "Follow a request through Dynamo"


def _title(c, t, heading, detail=None, dark=True):
    a.heading(c, t, CONTEXT, [heading], detail, dark=dark, size=55, y=120)


def _caption(c, x, y, label, color=WHITE, size=19):
    a.text(c, x, y, label, size, color, 2, "center")


def _line_fraction(c, x1, x2, y, progress, color=GREEN, width=4):
    if progress > 0:
        a.line(c, [(x1, y), (a.lerp(x1, x2, progress), y)], color, width)


def _motion(t, start, end):
    return a.smooth((t - start) / (end - start))


def _frame(c, x, y, w, h, title, fill="#202b37"):
    a.rr(c, x + 5, y + 8, w, h, 20, INK, alpha=0.38)
    a.rr(c, x, y, w, h, 20, fill, EDGE, 2)
    a.line(c, [(x + 1, y + 48), (x + w - 1, y + 48)], EDGE, 1.5)
    for n in range(3):
        a.ellipse(c, x + 21 + 14 * n, y + 23, 3.5, 3.5, DIM if n else GREEN)
    a.text(c, x + 68, y + 17, title, 14, BLUE, "mono")


def _client(c, x, y, w, h, prompt=False, reply=0, show_send=True, large=False):
    _frame(c, x, y, w, h, "Chat client")
    if large:
        top = y + 65
        w_in = w - 36
        font = 21
        prompt_h = 44
    else:
        top = y + 63
        w_in = w - 31
        font = 17
        prompt_h = 40

    if prompt:
        a.rr(c, x + 17, top, w_in, prompt_h, 10, "#34475b")
        a.text(c, x + 29, top + 11, "What is Kubernetes?", font, WHITE, 2)
    if show_send:
        label = "Send" if reply == 0 else "Reply"
        a.rr(c, x + 17, top + prompt_h + 13, 64, 32, 7, GREEN if reply else "#3f5468")
        a.text(c, x + 49, top + prompt_h + 20, label, 13, INK if reply else WHITE, 2, "center")
    if reply:
        text_x = x + 30
        top = top + prompt_h + (63 if show_send else 17)
        phrases = ("Kubernetes", " manages", " containerized", " workloads.")
        words = "".join(phrases[:reply])
        a.rr(c, x + 17, top, w_in, 61, 11, "#293e36", "#4c6c4d", 1.5)
        a.text(c, text_x, top + 20, words, font, a.MINT, 2)


def _packet(c, x, y, value, color=GREEN, w=91):
    a.rr(c, x - w / 2 + 2, y - 17 + 4, w, 34, 11, INK, alpha=0.28)
    a.rr(c, x - w / 2, y - 17, w, 34, 11, color, INK, 1.5)
    a.text(c, x, y - 7, value, 13, INK, "mono", "center")


def _tray(c, x, y, w, title, subtitle, active, gpu=False):
    edge = "#748ba1" if active else EDGE
    fill = "#253646" if active else "#1b2731"
    a.rr(c, x + 4, y + 8, w, 154, 15, INK, alpha=0.29)
    a.rr(c, x, y, w, 154, 15, fill, edge, 2)
    a.text(c, x + 15, y + 16, title, 19, WHITE, 8)
    a.text(c, x + 16, y + 44, subtitle, 13, BLUE, 5)
    a.line(c, [(x + 14, y + 72), (x + w - 14, y + 72)], EDGE, 1.5)
    for i in range(3):
        a.rr(c, x + 16, y + 87 + i * 13, 44 + i * 11, 3.5, 1.7, DIM, alpha=0.63)
    if gpu:
        a.chip(c, x + w - 42, y + 107, 0.49, GREEN if active else DIM, gpu=True)
    else:
        a.ellipse(c, x + w - 40, y + 108, 19, 19, "#2b4554", EDGE, 1.5)
        a.line(c, [(x + w - 49, y + 108), (x + w - 39, y + 99), (x + w - 31, y + 108)], BLUE, 2.4)
        a.line(c, [(x + w - 49, y + 108), (x + w - 39, y + 117), (x + w - 31, y + 108)], BLUE, 2.4)


def question(c, t):
    """Start with a human asking a question and following its highlighted route."""
    a.bg(c, DARK, SLATE, True)
    _title(c, t, TITLE, "An illustrated EKS example")
    a.soft_circuit(c, t, DIM, 614, 264, 521, 293, 0.14)
    _client(c, 82, 265, 443, 222, t > 0.55, show_send=True, large=True)
    a.ellipse(c, 1104, 521, 127, 13, INK, alpha=0.4)
    a.robot(c, 1110, 523, 1.05, t, "scan" if t > 2.9 else "idle")

    x0, x1, y = 550, 1008, 370
    a.curve(c, (x0, y), (674, y), (851, y), (x1, y), EDGE, 3, 0.96, [6, 8])
    for x in (687, 877):
        a.ellipse(c, x, y, 16, 16, DARK, EDGE, 2)
    first = a.reveal(t, 4.5, 0.75)
    second = a.reveal(t, 6.2, 0.75)
    if first:
        a.ellipse(c, 687, y, 8 * first, 8 * first, COOL)
        a.text(c, 687, y + 29, "CPU", 18, WHITE, 2, "center", first)
    if second:
        a.ellipse(c, 877, y, 8 * second, 8 * second, GREEN)
        a.text(c, 877, y + 29, "GPU", 18, WHITE, 2, "center", second)
    if t >= 2.7:
        progress = _motion(t, 2.7, 7.8)
        _packet(c, a.lerp(x0, x1 - 70, progress), y, "Prompt")
    if t >= 7.8:
        a.spark(c, 1010, 317, 11 + (math.sin(t * 4) + 1) * 3, GREEN)
    _caption(c, 303, 517, "OpenAI-compatible chat request", BLUE, 18)


def install(c, t):
    """Show the infrastructure handoff as one beat on a pale workbench."""
    a.bg(c, "#c8d4dd", "#edf2f1")
    _title(c, t, "Prepare the inference stack", "Start with an existing EKS cluster", False)
    a.soft_circuit(c, t, "#536174", 348, 271, 827, 226, 0.13)
    a.ellipse(c, 640, 509, 535, 33, "#688090", alpha=0.15)
    a.robot(c, 186, 479, 0.75, t, "scan")
    a.card(c, 357, 365, 1.07, PALE, "AICR RECIPE", ["EKS and H100", "Ubuntu", "Dynamo inference"], -0.089)
    a.line(c, [(445, 365), (802, 365)], EDGE, 2, 0.75)
    a.arrow(c, 806, 365, EDGE)
    progress = _motion(t, 4.65, 8.7)
    bundle_x = a.lerp(571, 751, progress)
    a.crate(c, bundle_x, 415, 1.01, 0.64 * (1 - progress))
    _caption(c, bundle_x, 489, "Deployment bundle", INK, 17)
    a.rack(c, 1012, 488, 0.85, t)
    a.rack(c, 1112, 488, 0.80, t + 0.19)

    rack_label = a.reveal(t, 8.35, 0.72)
    if rack_label:
        a.ellipse(c, 927, 536, 12, 12, GREEN, alpha=rack_label)
        a.tick(c, 927, 536, INK, 0.82, 2)
        a.text(c, 949, 527, "Dynamo stack installed", 20, INK, 2, alpha=rack_label)
    if t > 8.35:
        a.spark(c, 1165, 307, 11 + 2 * math.sin(t * 3), "#4b700d")


def workload(c, t):
    """Make the separate workload and its different pod placements visible."""
    a.bg(c, DARK, SLATE, True)
    _title(c, t, "Place the model workload", "Apply the DynamoGraphDeployment separately")
    _frame(c, 88, 265, 322, 200, "Model workload manifest")
    a.text(c, 108, 328, "kind: DynamoGraphDeployment", 15, WHITE, "mono")
    a.text(c, 108, 361, "components:", 15, BLUE, "mono")
    a.text(c, 122, 389, "Frontend", 15, COOL, "mono")
    a.text(c, 122, 414, "VllmDecodeWorker", 15, GREEN, "mono")

    top_p = _motion(t, 4.4, 6.6)
    lower_p = _motion(t, 7.0, 9.0)
    a.curve(c, (415, 384), (474, 384), (499, 325), (601, 325), EDGE, 3, 0.8, [5, 6])
    a.curve(c, (415, 392), (474, 397), (512, 485), (601, 485), EDGE, 3, 0.8, [5, 6])
    if top_p:
        _packet(c, a.lerp(442, 561, top_p), a.lerp(379, 325, top_p), "Frontend", COOL, 89)
    if lower_p:
        _packet(c, a.lerp(442, 561, lower_p), a.lerp(404, 485, lower_p), "vLLM", GREEN, 78)

    ready_top = t > 6.48
    ready_low = t > 8.89
    _tray(c, 620, 245, 346, "CPU node pool", "Dynamo frontend pod", ready_top)
    _tray(c, 620, 414, 346, "GPU node pool", "vLLM worker pod", ready_low, gpu=True)
    if ready_top:
        a.ellipse(c, 941, 268, 9, 9, COOL)
        a.tick(c, 941, 268, INK, 0.67, 2.2)
    if ready_low:
        a.ellipse(c, 941, 436, 9, 9, GREEN)
        a.tick(c, 941, 436, INK, 0.67, 2.2)
    a.robot(c, 1110, 507, 0.83, t, "scan" if ready_low else "idle")


def trace(c, t):
    """Follow one prompt from client to frontend to compute."""
    a.bg(c, DARK, "#1f3040", True)
    _title(c, t, "Follow one request", "A single prompt crosses two node pools")
    _client(c, 79, 278, 308, 189, True)
    first = _motion(t, 1.8, 4.15)
    second = _motion(t, 5.15, 7.7)
    _tray(c, 510, 286, 242, "CPU frontend", "Dynamo request router", first > 0.9)
    _tray(c, 896, 286, 264, "GPU worker", "vLLM model execution", second > 0.88, gpu=True)

    y = 367
    for left, right, progress in ((396, 503, first), (759, 889, second)):
        a.line(c, [(left, y), (right, y)], EDGE, 3)
        _line_fraction(c, left, right, y, progress, COOL if left < 500 else GREEN)
        a.arrow(c, right, y, COOL if left < 500 else GREEN, 7)

    if 1.8 <= t < 4.15:
        _packet(c, a.lerp(402, 487, first), y, "Prompt", COOL, 78)
    if 5.15 <= t < 7.7:
        _packet(c, a.lerp(772, 871, second), y, "Prompt", GREEN, 78)
    if t >= 4.15:
        _caption(c, 631, 472, "Prompt received", BLUE)
    if t >= 7.7:
        _caption(c, 1026, 472, "Generating tokens", a.MINT)
        for i in range(5):
            alpha = 0.65 + 0.35 * math.sin(4 * t + i * 1.4)
            a.rr(c, 980 + i * 19, 504, 12, 7, 3, GREEN, alpha=alpha)
        a.spark(c, 1158, 256, 12 + 2.6 * math.sin(t * 3), GREEN)


def result(c, t):
    """Reveal the token stream through the route back to the same client."""
    a.bg(c, DARK, SLATE, True)
    _title(c, t, "The reply comes back", "Worker to frontend to client")
    a.soft_circuit(c, t, DIM, 627, 296, 543, 210, 0.15)
    _client(c, 79, 270, 614, 245, True, sum(t > p for p in (2.33, 2.68, 3.1, 3.6)), show_send=False, large=True)

    y = 397
    a.line(c, [(700, y), (1100, y)], EDGE, 3)
    a.chip(c, 1110, y, 1.00, GREEN, gpu=True)
    a.ellipse(c, 897, y, 27, 27, SLATE, COOL, 2.5)
    a.line(c, [(908, y - 8), (896, y), (908, y + 8)], COOL, 2.5)
    a.line(c, [(897, y - 8), (885, y), (897, y + 8)], COOL, 2.5)
    _caption(c, 1110, 463, "GPU worker", WHITE, 17)
    _caption(c, 897, 463, "CPU frontend", WHITE, 17)

    for start in (0.43, 0.81, 1.22, 1.67):
        if start < t < start + 1.9:
            p = _motion(t, start, start + 1.9)
            x = a.lerp(1053, 708, p)
            a.rr(c, x - 8, y - 5, 16, 10, 4, GREEN if x >= 897 else COOL, INK, 1)
    if t > 3.6:
        a.ellipse(c, 104, 544, 11, 11, GREEN)
        a.tick(c, 104, 544, INK, 0.77, 2.2)
        a.text(c, 125, 534, "Reply received", 19, WHITE, 2)
        a.spark(c, 1153, 345, 14 + math.sin(t * 5) * 2.5, GREEN)


SCENES = (question, install, workload, trace, result)
