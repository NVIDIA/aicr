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
"""AICR's NVIDIA NIM workload pilot."""

import math

from . import shared as a


INK = a.INK
LIME = a.LIME
WHITE = a.WHITE
SLATE = "#9fb2c6"
MUTED = "#a3afbc"
DARK = "#18202a"
WELL = "#101722"
RULE = "#394959"
EDGE = "#556575"
ICE = "#d0d9e3"
PAPER = "#ecf0f4"
GREEN_TEXT = "#c5e985"
AMBER = "#efb77b"
MODEL = "meta/llama-3.2-1b-instruct"
PULL = "ngc-pull-secret"
AUTH = "ngc-api-secret"


def _place(c, fraction, lift=14):
    c.save()
    c.translate(0, lift * (1 - fraction))


def _check_dot(c, x, y, ready, fill=LIME):
    a.ellipse(c, x, y, 12, 12, fill if ready else WELL, EDGE, 1.5)
    if ready:
        a.tick(c, x, y + 1, INK, 0.67, 2.6)
    else:
        a.ellipse(c, x, y, 2.5, 2.5, MUTED)


def _panel(c, x, y, w, h, r=21, fill=DARK, stroke=EDGE):
    a.rr(c, x + 5, y + 9, w, h, r, INK, alpha=0.23)
    a.rr(c, x, y, w, h, r, fill, stroke, 2.4)


def _model_badge(c, x, y, t, fraction, size=0.72):
    if fraction < 0.07:
        return
    c.save()
    c.translate(x, y + 8 * (1 - fraction) + 2.3 * math.sin(t * 2.1))
    c.scale(size * fraction, size * fraction)
    a.poly(c, [(-56, -36), (0, -62), (57, -36), (0, -9)], "#d3ef9b", INK, 3)
    a.poly(c, [(-56, -36), (0, -9), (0, 48), (-56, 22)], LIME, INK, 3)
    a.poly(c, [(0, -9), (57, -36), (57, 22), (0, 48)], "#6ca018", INK, 3)
    for i in range(3):
        a.line(c, [(10, 8 + i * 9), (44, -7 + i * 9)], "#395c0a", 2.5)
    a.text(c, -27, -12, "NIM", 16, INK, 'mono', 'center')
    c.restore()


def _empty_bay(c, x, y, t):
    _panel(c, x, y, 423, 356, 25, "#25323f", EDGE)
    a.rr(c, x + 15, y + 63, 393, 275, 15, WELL, "#405167", 1.5)
    a.text(c, x + 23, y + 22, "MODEL SERVER", 16, WHITE, 'mono')
    a.ellipse(c, x + 373, y + 29, 4, 4, EDGE)
    a.ellipse(c, x + 391, y + 29, 4, 4, AMBER)
    c.save()
    c.set_dash([10, 8])
    a.rr(c, x + 87, y + 104, 247, 133, 16, None, EDGE, 2)
    c.restore()
    a.text(c, x + 210, y + 135 + 4 * math.sin(t * 2.1), "?", 56, SLATE, 8, 'center')
    a.text(c, x + 211, y + 257, "Awaiting NIMService", 21, AMBER, 2, 'center')
    a.line(c, [(x + 36, y + 301), (x + 387, y + 301)], RULE, 1.5)


def _install_track(c, x, y, t):
    _panel(c, x, y, 561, 99, 17)
    centers = (x + 105, x + 275, x + 449)
    for index, (cx, label) in enumerate(zip(centers, ("NIM recipe", "Deployment bundle", "NIM Operator"))):
        progress = a.reveal(t, 4.2 + index * 0.75, 0.48)
        a.text(c, cx, y + 58, label, 17, WHITE, 2, 'center', alpha=progress)
        if progress > 0:
            _check_dot(c, cx, y + 30, True)
        if index < len(centers) - 1 and progress > 0:
            a.line(c, [(cx + 35, y + 30), (centers[index + 1] - 30, y + 30)], RULE, 2)
            a.arrow(c, centers[index + 1] - 29, y + 30, MUTED, 6)


def _hook(c, t):
    a.bg(c, "#0b0f14", "#1e2b39", True)
    a.soft_circuit(c, t, "#506074", 686, 194, 525, 320, alpha=0.20)
    a.heading(
        c,
        t,
        "NVIDIA NIM  /  EPISODE 43",
        ("The Operator is ready.", "Where is the model?"),
        None,
        dark=True,
        size=52,
        y=137,
    )
    a.text(c, 71, 286, "The model workload has its own lifecycle.", 23, SLATE, 5, alpha=a.reveal(t, 1.1))
    reveal = a.reveal(t, 0.65, 0.8)
    if reveal > 0:
        _place(c, reveal, 24)
        _empty_bay(c, 764, 159, t)
        c.restore()
    track = a.reveal(t, 3.75, 0.6)
    if track > 0:
        _place(c, track)
        _install_track(c, 70, 405, t)
        c.restore()
    if t > 1.1:
        a.robot(c, 689, 561, 0.72, t, pose="worried" if t < 4.4 else "scan")


def _code_card(c, y, t):
    x, w, h = 69, 632, 324
    _panel(c, x, y, w, h, 20, "#161e29", EDGE)
    a.rr(c, x + 13, y + 48, w - 26, h - 61, 10, WELL)
    a.text(c, x + 24, y + 17, "NIMService  /  example manifest excerpt", 17, WHITE, 'mono')
    rows = (
        ("kind: NIMService", WHITE),
        ("metadata:", WHITE),
        ("  name: llama-3-2-1b", SLATE),
        ("  namespace: nim-workload", SLATE),
        ("spec:", WHITE),
        ("  image:", WHITE),
        ("    pullSecrets:", SLATE),
        (f"      - {PULL}", GREEN_TEXT),
        (f"  authSecret: {AUTH}", GREEN_TEXT),
    )
    for n, (text, color) in enumerate(rows):
        appear = a.reveal(t, 0.66 + n * 0.46, 0.40)
        yy = y + 63 + n * 26
        if n in (7, 8) and appear > 0:
            a.rr(c, x + 24, yy - 4, w - 48, 24, 6, "#1b2f24", alpha=appear)
        a.text(c, x + 35, yy, text, 17, color, 'mono', alpha=appear)


def _lock(c, x, y):
    a.line(c, [(x - 6, y - 4), (x - 6, y - 10), (x + 6, y - 10), (x + 6, y - 4)], INK, 2.5)
    a.rr(c, x - 9, y - 5, 18, 14, 3, LIME, INK, 2.5)
    a.ellipse(c, x, y + 1, 1.8, 1.8, INK)


def _secret_tile(c, x, y, label, role):
    a.rr(c, x, y, 326, 69, 12, PAPER, "#8292a5", 1.7)
    a.ellipse(c, x + 28, y + 34, 17, 17, "#d2e8ad")
    _lock(c, x + 28, y + 34)
    a.text(c, x + 57, y + 10, label, 17, INK, 'mono')
    a.text(c, x + 57, y + 36, role, 16, "#3a4c60", 2)


def _namespace(c, t):
    a.bg(c, "#b6c3cf", "#eef2f6")
    a.heading(c, t, "02  /  DECLARE THE WORKLOAD", ("Apply a NIMService",), None, size=61, y=119)
    expand = a.reveal(t, 0.3, 0.7)
    if expand > 0:
        _place(c, expand)
        _code_card(c, 226, t)
        c.restore()
    expand = a.reveal(t, 2.4, 0.75)
    if expand > 0:
        _place(c, expand)
        _panel(c, 733, 226, 456, 324, 20, "#dde6ee", "#8799a9")
        a.text(c, 753, 246, "NAMESPACE  nim-workload", 16, "#34485c", 'mono')
        a.line(c, [(753, 280), (1166, 280)], "#a5b3c1", 1.6)
        pull = a.reveal(t, 4.8, 0.52)
        if pull > 0:
            _place(c, pull)
            _secret_tile(c, 761, 301, PULL, "Pull the container image")
            c.restore()
        auth = a.reveal(t, 6.8, 0.52)
        if auth > 0:
            _place(c, auth)
            _secret_tile(c, 761, 389, AUTH, "Access the model assets")
            c.restore()
        c.restore()
    if t > 2.4:
        a.robot(c, 1119, 574, 0.48, t, pose="scan")


def _progress(c, t):
    _panel(c, 70, 281, 509, 249, 18)
    rows = (
        ("Image available", "The workload can start its container", 3.0),
        ("Model server starting", "The pod requests one GPU", 6.2),
        ("Pod ready", "Service endpoint can receive requests", 9.5),
    )
    for n, (title, detail, at) in enumerate(rows):
        yy = 305 + n * 69
        done = t > at
        if n > 0:
            a.line(c, [(90, yy - 12), (555, yy - 12)], RULE, 1.2)
        _check_dot(c, 100, yy + 16, done)
        a.text(c, 129, yy, title, 22, WHITE if done else SLATE, 7)
        a.text(c, 129, yy + 30, detail, 17, MUTED, 2)


def _startup(c, t):
    a.bg(c, "#0d131c", "#1f3144", True)
    a.heading(c, t, "03  /  WATCH THE WORKLOAD", ("Give the model time to start",), None, dark=True, size=59, y=120)
    a.soft_circuit(c, t, "#647b94", 692, 244, 491, 328, alpha=0.22)
    _progress(c, t)
    _panel(c, 777, 240, 404, 290, 25, "#202d39", EDGE)
    a.text(c, 803, 259, "LLAMA 3.2 MODEL SERVER", 16, WHITE, 'mono')
    a.line(c, [(805, 289), (1152, 289)], RULE, 1.4)
    a.rack(c, 970, 499, 0.73, t)
    payload = a.reveal(t, 3.8, 2.5)
    px = a.lerp(678, 1065, a.smooth((t - 3.7) / 4.7))
    if payload > 0:
        _model_badge(c, px, 385, t, payload, 0.67)
    a.robot(c, 688, 568, 0.70, t, pose="scan" if t < 9.55 else "cheer")
    on = a.reveal(t, 9.5, 0.5)
    if on > 0:
        _check_dot(c, 1137, 269, True)
        a.spark(c, 1117, 433, 9 + 3 * math.sin(t * 2), LIME, alpha=on)


def _response(c, t):
    x, y, w, h = 475, 220, 732, 326
    _panel(c, x, y, w, h, 20, "#151e27", "#42576a")
    a.rr(c, x + 13, y + 48, w - 26, h - 61, 10, WELL)
    a.text(c, x + 20, y + 16, "LOCAL TEST  /  PORT-FORWARDED SERVICE", 16, WHITE, 'mono')
    for i, color in enumerate((EDGE, EDGE, LIME)):
        a.ellipse(c, x + 643 + 20 * i, y + 23, 3.7, 3.7, color)
    command = "GET /v1/models"
    count = min(len(command), int(max(0, t - 1.3) * 15))
    a.text(c, x + 34, y + 72, command[:count], 24, WHITE, 'mono')
    if t > 1.25 and t < 3.0 and int(t * 4) % 2 == 0:
        advance = a.font(24, 'mono').getlength(command[:count]) / a.SCALE
        a.rr(c, x + 38 + advance, y + 71, 11, 25, 2, LIME)
    result = a.reveal(t, 4.1, 0.6)
    if result > 0:
        a.line(c, [(x + 33, y + 121), (x + w - 33, y + 121)], RULE, 1.5)
        a.text(c, x + 35, y + 142, "ILLUSTRATIVE RESPONSE", 14, AMBER, 'mono', alpha=result)
        a.text(c, x + 35, y + 175, "200 OK", 18, GREEN_TEXT, 'mono', alpha=result)
        a.text(c, x + 35, y + 209, '{ "data": [', 20, MUTED, 'mono', alpha=result)
        a.text(c, x + 35, y + 242, f'  {{ "id": "{MODEL}" }}', 20, GREEN_TEXT, 'mono', alpha=result)
        a.text(c, x + 35, y + 275, '] }', 20, MUTED, 'mono', alpha=result)


def _payoff(c, t):
    a.bg(c, "#bcc9d5", "#eff2f7")
    a.heading(c, t, "04  /  CHECK THE MODEL ENDPOINT", ("Is the model serving?",), None, size=62, y=120)
    a.rack(c, 248, 502, 1.0, t)
    a.robot(c, 359, 568, 0.64, t, pose="scan" if t < 4.7 else "cheer")
    ready = a.reveal(t, 0.4, 0.7)
    if ready > 0:
        _place(c, ready, 12)
        a.pill(c, 232, 517, "Service · port 8000", fill="#e5eece", size=17)
        _response(c, t)
        c.restore()


SCENES = (_hook, _namespace, _startup, _payoff)
