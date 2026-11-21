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
"""Draw a short from the episode's scene board."""

import math

from . import shared as a


INK = "#10161d"
DARK = "#263849"
WHITE = "#f0f5f8"
STEEL = "#87a0b8"
DIM = "#556e85"
MID = "#c7d7e5"
LIME = "#93d22a"
AMBER = "#edcf92"
CORAL = "#eb9581"
BLUE = "#a9c9e6"
VIZ = (
    "probe",
    "fork",
    "compare",
    "trace",
    "compile",
    "queue",
    "inspect",
    "gate",
    "plot",
    "terminal",
    "dashboard",
    "layers",
    "incident",
    "portal",
    "vault",
)
COMPARE_VALUES = {
    (4, 2): ("Kubeflow Trainer", "NIM Operator"),
    (5, 2): ("unset", "nvidia-kernel-module-params"),
    (7, 1): ("6.1", "≥ 6.8"),
    (10, 2): ("Earlier observation", "Latest observation"),
    (14, 2): ("Driver installer", "DRA advertises whole GPUs"),
    (22, 1): ("Simulated nodes", "Physical GPUs"),
    (28, 3): ("Bound to approval", "Changed after approval"),
    (30, 0): ("Current rollout", "Previous rollout"),
    (31, 2): ("Rendered from plan inputs", "Pulled from runtime registry"),
    (35, 1): ("Desired state", "Observed state"),
    (36, 3): ("Research catalog", "Production catalog"),
    (37, 3): ("From AICRConfig", "From the same AICRConfig"),
    (40, 0): ("1 node × 1 GPU", "2 nodes × 1 GPU"),
}


def fit(txt, maximum, preferred, weight=2, minimum=12):
    for size in range(preferred, minimum - 1, -1):
        if a.font(size, weight).getlength(txt) / a.SCALE <= maximum:
            return size
    return minimum


def line(c, x, y, txt, width=400, size=22, color=INK, weight=2, align="left", alpha=1):
    a.text(c, x, y, txt, fit(txt, width, size, weight), color, weight, align, alpha)


def card(c, x, y, w, h, fill=WHITE, border=INK, r=18):
    a.rr(c, x + 6, y + 8, w, h, r, INK, alpha=0.21)
    a.rr(c, x, y, w, h, r, fill, border, 2)


def mood(text):
    needle = text.lower()
    if any(
        s in needle
        for s in (
            "failed",
            "blocked",
            "mismatch",
            "not ready",
            "exit nonzero",
            "error",
            "rejected",
            "socket fallback",
            "observed socket",
            "no validator jobs",
        )
    ):
        return CORAL
    if any(
        s in needle
        for s in ("wait", "pending", "manual", "review", "unknown", "stale", "paused", "stalled", "stalls the rollout")
    ):
        return AMBER
    return LIME


def dot(c, x, y, fill=BLUE):
    a.ellipse(c, x, y, 7, 7, fill, INK, 1.4)


def stage(c, x, y, w, dark):
    a.ellipse(c, x, y, w, 105, "#536d83" if dark else "#9eb6cc", alpha=0.25)


def code(c, x, y, w, h, value, dark=True, signal=False):
    a.rr(c, x, y, w, h, 10, "#18242f" if dark else "#e8eff4", "#7592ac", 1.3)
    line(c, x + 14, y + 11, value, w - 28, 17, WHITE if dark else INK, "mono")
    if signal:
        dot(c, x + w - 15, y + h / 2, LIME)


def rope(c, x0, x1, y, t, active=True):
    a.curve(c, (x0, y), (x0 + 18, y - 5), (x1 - 28, y - 5), (x1, y), DIM, 2.6, 0.83, [6, 8])
    a.arrow(c, x1, y, DIM, 8)
    if active:
        r = (t * 0.44 + x0 * 0.002) % 1
        a.ellipse(c, a.lerp(x0 + 6, x1 - 8, r), y, 4.8, 4.8, LIME, INK, 1.2)


def badge(c, x, y, text, fill=None, max_w=334):
    size = fit(text, max_w - 28, 17, 2)
    w = min(max_w, max(100, a.font(size, 2).getlength(text) / a.SCALE + 28))
    a.rr(c, x - w / 2, y, w, 30, 15, fill or "#e1e9ef", INK, 1.4)
    line(c, x, y + 7, text, w - 16, size, INK, 2, "center")


def artifact(c, x, y, txt, t, scale=0.72):
    q = txt.lower()
    if any(
        r in q
        for r in (
            "gpu node",
            "worker node",
            "live cluster",
            "cpu node",
            "h100 node",
            "gpu pool",
            "source registry",
            "upstream registry",
            "live release",
        )
    ):
        a.rack(c, x, y + 69, 0.61 * scale / 0.72, t)
    elif any(r in q for r in ("bundle", "artifact", "archive", "zip", "digest", "release file")):
        a.crate(c, x, y, scale * 1.04)
    elif any(
        r in q
        for r in (
            "recipe",
            "manifest",
            "snapshot",
            "yaml",
            "report",
            "settings",
            "evidence",
            "config",
            "values",
            "logs",
            "records",
            "sbom",
            "provenance",
        )
    ):
        a.card(c, x, y + 2 * math.sin(t * 2), scale, WHITE, "FILE", None, -0.08)
    elif any(r in q for r in ("health", "verdict", "pass", "failed", "check", "blocked", "current revision")):
        col = mood(q)
        a.ellipse(c, x, y, 43 * scale / 0.72, 43 * scale / 0.72, col, INK, 3)
        if col == CORAL:
            for n in (-1, 1):
                a.line(c, [(x - 10, y + 10 * n), (x + 10, y - 10 * n)], INK, 3.5)
        elif col == LIME:
            a.tick(c, x, y, INK, 1.7, 4.2)
        else:
            a.line(c, [(x - 14, y), (x + 14, y)], INK, 4)
    else:
        a.chip(c, x, y, scale, BLUE if "cpu" in q or "input" in q else LIME, 0.07 * math.sin(t * 1.6))


def columns(c, sc, dark, t, p, kind):
    items = sc["items"]
    xs = (226, 641, 1055)
    stage(c, 640, 424, 558, dark)
    if kind == "queue":
        a.line(c, [(229, 329), (1052, 329)], DIM, 3)
        for i, x in enumerate(xs):
            col = mood(items[i])
            a.ellipse(c, x, 329, 25, 25, col if p > 0.23 + i * 0.18 else BLUE, INK, 2.3)
            a.text(c, x, 319, str(i + 1), 20, INK, 8, "center")
        a.robot(c, 640 + 193 * a.smooth((p - 0.42) / 0.26), 514, 0.63, t, "scan")
        for i, x in enumerate(xs):
            badge(c, x, 387, items[i], mood(items[i]) if i == 2 and p > 0.56 else None)
        return
    a.robot(c, 641, 512, 0.84, t, "scan" if p < 0.62 else "cheer")
    rope(c, 333, 500, 370, t)
    rope(c, 779, 929, 370, t, p > 0.24)
    for i, x in enumerate(xs):
        if i != 1:
            motion = 1 - a.out((p - 0.06 * i) / 0.13)
            artifact(c, x + (50 if i == 2 else -50) * motion, 375, items[i], t + i, 0.78)
        badge(c, x, 487, items[i], mood(items[i]) if i == 2 and p > 0.52 else None)


def split(c, sc, dark, t, p, kind, example):
    items = sc["items"]
    stage(c, 652, 443, 515, dark)
    if kind == "fork":
        a.robot(c, 235, 498, 0.88, t, "scan")
        a.curve(c, (355, 370), (510, 370), (578, 287), (757, 287), DIM, 3)
        a.curve(c, (355, 370), (510, 370), (578, 443), (757, 443), DIM, 3)
        a.arrow(c, 757, 287, DIM, 8)
        a.arrow(c, 757, 443, DIM, 8)
        choice = 1 if p < 0.59 else 2
        frac = (t * 0.42) % 1
        a.ellipse(c, 390 + 330 * frac, a.lerp(370, 287 if choice == 1 else 443, frac), 6, 6, LIME, INK, 1.5)
        a.ellipse(c, 504, 370, 25, 25, "#21303d", STEEL, 2.1)
        a.text(c, 504, 362, "OR", 15, WHITE, "mono", "center")
        badge(c, 226, 502, items[0])
        for i, y in enumerate((245, 402), 1):
            card(c, 777, y, 411, 84, "#e9f1dc" if i == choice else WHITE)
            artifact(c, 820, y + 43, items[i], t, 0.36)
            line(c, 881, y + 29, items[i], 286, 23, INK, 8)
        return
    phase = a.reveal(p, 0.15, 0.35)
    values = COMPARE_VALUES.get(example)
    if example in {(36, 3), (37, 3)}:
        items = [items[1], items[2], items[0]]
    for i, (x, y) in enumerate(((85, 252), (784, 252))):
        card(c, x, y, 412, 205, WHITE)
        a.rr(c, x + 16, y + 17, 380, 38, 9, BLUE if i == 0 else "#dbe9c7")
        line(c, x + 206, y + 26, items[i], 348, 21, INK, 8, "center")
        if values:
            a.rr(c, x + 25, y + 90, 362, 62, 10, "#22313f")
            line(
                c,
                x + 206,
                y + 108,
                values[i],
                335,
                21,
                WHITE,
                "mono" if example in {(5, 2), (7, 1), (40, 0)} else 2,
                "center",
                0.34 + 0.66 * a.reveal(p, 0.11 + i * 0.1, 0.29),
            )
        else:
            artifact(c, x + 206, y + 122, items[i], t + i, 0.66)
    a.robot(c, 637, 496, 0.78, t, "scan")
    if phase > 0.01:
        badge(c, 640, 502, items[2], mood(items[2]), max_w=460)


def inspect(c, sc, dark, t, p, kind):
    items = sc["items"]
    stage(c, 755, 443, 419, dark)
    skin = "#1d2b39" if kind in ("terminal", "incident") else WHITE
    foreground = WHITE if skin != "#f0f5f8" else INK
    x, y, w, h = (487, 235, 677, 272) if kind in ("terminal", "inspect", "dashboard") else (423, 237, 748, 271)
    card(c, x, y, w, h, skin)
    if kind == "probe":
        a.rack(c, 220, 466, 0.72, t)
        a.robot(c, 363, 502, 0.67, t, "scan")
        lens = 746 + 117 * math.sin(t * 1.02)
        a.rr(c, x + 14, y + 53, w - 28, 11, 5, BLUE, alpha=0.47)
        a.line(c, [(lens, y + 62), (lens, y + 222)], LIME, 2.4, 0.65)
    else:
        a.robot(c, 304, 507, 0.89, t, "worried" if kind == "incident" and p < 0.7 else "scan")
    if kind in ("terminal", "incident"):
        for i, col in enumerate((CORAL, AMBER, LIME)):
            dot(c, x + 25 + 19 * i, y + 25, col)
        a.line(c, [(x + 1, y + 50), (x + w - 1, y + 50)], STEEL, 1.5)
    else:
        a.rr(c, x + 16, y + 17, w - 32, 36, 8, "#d4e1eb")
        text = {"probe": "CAPTURED INPUT", "inspect": "INSPECT THE RESULT", "dashboard": "RESULTS"}[kind]
        a.text(c, x + 29, y + 24, text, 15, "#476379", "mono")
    for idx, item in enumerate(items):
        yy = y + 77 + idx * 55
        f = 0.28 + 0.72 * a.reveal(p, 0.08 + 0.155 * idx, 0.17)
        a.rr(c, x + 19, yy, w - 38, 39, 9, "#34475a" if skin != "#f0f5f8" else "#e4ecf2", alpha=f)
        if kind in ("terminal", "incident"):
            a.text(c, x + 35, yy + 11, "›", 18, BLUE, "mono", alpha=f)
        else:
            dot(c, x + 43, yy + 20, mood(item) if f > 0.7 else STEEL)
        line(c, x + 62, yy + 10, item, w - 109, 21, foreground, "mono" if kind == "terminal" else 2, alpha=f)
    if kind == "incident":
        a.spark(c, 1201, 310, 21 * (0.86 + 0.14 * math.sin(t * 4)), CORAL)


def layers(c, sc, dark, t, p, kind):
    stage(c, 617, 441, 516, dark)
    a.robot(c, 253, 507, 0.94, t, "carry" if p < 0.66 else "cheer")
    if kind == "compile":
        for i, item in enumerate(sc["items"]):
            f = a.reveal(p, 0.01 + 0.14 * i, 0.26)
            x = 405 + i * 102 + 160 * f
            artifact(c, x, 372 - 22 * i, item, t + i, 0.50)
            if p > 0.1 + i * 0.12:
                badge(c, 496 + i * 121, 478, item, max_w=140)
        rope(c, 847, 1000, 373, t, p > 0.33)
        a.crate(c, 1071, 376, 0.92)
        return
    for i, item in enumerate(sc["items"]):
        g = a.reveal(p, 0.03 + i * 0.19, 0.27)
        x = 536 + (1 - g) * (145 + i * 43)
        y = 280 + i * 70
        tint = (BLUE, "#cbdcc2", "#edf3f6")[i]
        a.poly(c, [(x, y), (x + 489, y), (x + 541, y + 42), (x + 51, y + 42)], tint, INK, 2.4)
        a.poly(c, [(x + 51, y + 42), (x + 541, y + 42), (x + 541, y + 56), (x + 51, y + 56)], DIM, INK, 2.2)
        line(c, x + 77, y + 11, item, 412, 24, INK, 8)


def control(c, sc, dark, t, p, kind):
    items = sc["items"]
    stage(c, 642, 425, 563, dark)
    hold = mood(sc.get("result") or items[-1])
    if kind == "vault":
        a.crate(c, 258, 402, 1.03)
        card(c, 860, 263, 271, 203, "#dce7ef")
        a.rr(c, 893, 284, 204, 157, 25, "#96b2c7", INK, 2.5)
        a.ellipse(c, 994, 357, 44, 44, "#e0eaf0", INK, 3)
        a.line(c, [(972, 357), (1015, 357)], DIM, 4)
        a.line(c, [(994, 335), (994, 380)], DIM, 4)
    else:
        artifact(c, 233, 376, items[0], t, 0.93)
        artifact(c, 1055, 376, items[2], t, 0.90)
    a.robot(c, 614, 511, 0.74, t, "worried" if hold == CORAL else "scan")
    rope(c, 348, 486, 375, t, True)
    rope(c, 757, 935, 375, t, hold == LIME and p > 0.7)
    bar = 118 + 38 * a.smooth((p - 0.65) / 0.27) if hold == LIME else 117
    a.rr(c, 742, 302, 12, bar, 6, hold, INK, 2.0)
    a.ellipse(c, 748, 302, 9, 9, hold, INK, 1.8)
    for i, (x, item) in enumerate(zip((233, 614, 1055), items)):
        badge(c, x, 499, item, hold if i == 1 and p > 0.38 else None)


def plot(c, sc, dark, t, p):
    stage(c, 749, 431, 435, dark)
    a.robot(c, 279, 507, 0.81, t, "scan")
    card(c, 479, 218, 683, 292, WHITE)
    a.text(c, 506, 238, "ILLUSTRATIVE TRACE", 15, DIM, "mono")
    for y in (300, 340, 380, 420):
        a.line(c, [(522, y), (1119, y)], "#cbd9e3", 1.4)
    for x in range(522, 1120, 75):
        a.line(c, [(x, 280), (x, 420)], "#dce6ed", 1.2)
    points = []
    clip = max(0.02, a.smooth((p - 0.09) / 0.7))
    for i in range(23):
        factor = i / 22
        if factor > clip:
            break
        x = 522 + 597 * factor
        y = 402 - 87 * (1 - math.exp(-factor * 4)) - 16 * math.sin(factor * 15)
        points.append((x, y))
    if len(points) > 1:
        a.line(c, points, "#5d8a30", 4)
        dot(c, *points[-1], LIME)
    for i, item in enumerate(sc["items"]):
        x = 510 + i * 208
        a.rr(c, x, 454, 192, 30, 12, (BLUE, "#dceacf", "#e8eff3")[i])
        line(c, x + 96, 461, item, 176, 15, INK, 2, "center")


def portal(c, sc, dark, t, p):
    stage(c, 652, 427, 532, dark)
    card(c, 80, 235, 447, 270, WHITE)
    a.rr(c, 97, 252, 411, 39, 9, "#d5e4f0")
    a.text(c, 117, 261, "REQUEST INPUT", 16, DIM, "mono")
    for idx, item in enumerate(sc["items"][:2]):
        code(c, 116, 322 + 57 * idx, 374, 38, item, False)
    rope(c, 548, 766, 370, t, p > 0.32)
    a.robot(c, 655, 496, 0.76, t, "scan")
    card(c, 794, 256, 389, 221, "#e4efdc" if p > 0.39 else WHITE)
    a.text(c, 818, 280, "RESULT", 16, DIM, "mono")
    artifact(c, 990, 359, sc["items"][2], t, 0.57)
    line(c, 987, 439, sc["items"][2], 338, 20, INK, 8, "center")


def bg(c, dark, variation):
    """Select the background palette for one board."""
    light = ("#263849", "#263737", "#292f40")[variation]
    if dark:
        a.bg(c, "#0c1117", light, True)
    else:
        a.bg(c, "#c7d6e2", "#eef4f7")


def heading(c, spec, sc, dark, p):
    fg = WHITE if dark else INK
    label = spec["track"].upper()
    if spec["status"] == "preview":
        label += "  /  PREVIEW"
    if spec["status"] == "main":
        label = "AICR  /  UNRELEASED IN V1.3.0"
    a.text(c, 67, 79, label, 16, LIME if dark else "#4b7229", "mono")
    f = a.out(p / 0.07 + 0.52)
    line(c, 65, 125 + 9 * (1 - f), sc["heading"], 1148, 63, fg, 8, alpha=f)
    if sc.get("sub"):
        line(c, 68, 191, sc["sub"], 1124, 22, "#a7bdd2" if dark else "#415a70", 5)


def footer(c, sc, p, dark):
    detail, result = sc.get("detail"), sc.get("result")
    if detail:
        band = 544 if not result else 530
        w = min(960, max(330, a.font(17, "mono").getlength(detail) / a.SCALE + 42))
        code(c, 640 - w / 2, band, w, 34, detail, True)
    end = a.reveal(p, 0.60, 0.13)
    if result and end > 0.01:
        y = 572 if detail else 551
        f = mood(result)
        a.rr(c, 283, y, 714, 31, 15, f, INK, 1.4, end)
        line(c, 640, y + 6, result, 686, 18, INK, 8, "center", end)


def outro(c, spec, t):
    bg(c, True, 1)
    a.text(c, 73, 102, "NVIDIA AI CLUSTER RUNTIME", 18, LIME, "mono")
    a.text(c, 61, 154, "AICR", 139, WHITE, 8)
    a.rr(c, 73, 352, 276, 68, 33, LIME)
    a.text(c, 110, 370, "aicr.run", 32, INK, 8)
    a.line(c, [(288, 387), (310, 387)], INK, 3)
    a.arrow(c, 310, 387, INK, 7)
    a.text(c, 74, 448, "See the episode guide for the next step", 22, "#b9c5d0", 5)
    a.ellipse(c, 945, 344, 240, 213, "#395421", alpha=0.62)
    a.ellipse(c, 945, 344, 202, 183, None, "#78a056", 1.5, 0.56)
    a.soft_circuit(c, t, "#a3bc81", 688, 261, 477, 257, 0.19)
    a.ellipse(c, 948, 538, 190, 24, "#2b4224", "#759c44", 1.5)
    a.robot(c, 950, 541, 1.34, t, "wave")
    a.chip(c, 762, 255 + 7 * math.sin(t * 2), 0.51, BLUE, -0.16, True)
    a.chip(c, 1139, 259 + 8 * math.sin(t * 2 + 0.6), 0.48, LIME, 0.14)


def draw(c, spec, edges, cues, t):
    ix = next(i for i, limit in enumerate(edges[1:]) if t < limit)
    local = t - edges[ix]
    if ix == 4:
        outro(c, spec, local)
    else:
        scene = spec["scenes"][ix]
        kind = scene["visual"]
        p = local / (edges[ix + 1] - edges[ix])
        dark = kind in {"incident", "terminal", "dashboard", "vault", "plot", "trace"} or (
            ix == 0 and kind not in {"portal", "compare"}
        )
        bg(c, dark, (spec["id"] + ix) % 3 if dark else 0)
        heading(c, spec, scene, dark, p)
        if kind in {"fork", "compare"}:
            split(c, scene, dark, local, p, kind, (spec["id"], ix))
        elif kind in {"probe", "inspect", "terminal", "dashboard", "incident"}:
            inspect(c, scene, dark, local, p, kind)
        elif kind in {"layers", "compile"}:
            layers(c, scene, dark, local, p, kind)
        elif kind in {"gate", "vault"}:
            control(c, scene, dark, local, p, kind)
        elif kind == "plot":
            plot(c, scene, dark, local, p)
        elif kind == "portal":
            portal(c, scene, dark, local, p)
        else:
            columns(c, scene, dark, local, p, kind)
        footer(c, scene, p, dark)
    rem = edges[ix + 1] - t
    if ix and local < 0.23:
        edge = 1456 * a.out(local / 0.23)
        a.poly(
            c, [(a.between(edge - 176, 0, 1280), 0), (1280, 0), (1280, 720), (a.between(edge, 0, 1280), 720)], LIME
        )
    if ix < 4 and rem < 0.19:
        edge = 1456 * a.smooth(1 - rem / 0.19)
        a.poly(c, [(0, 0), (a.between(edge - 176, 0, 1280), 0), (a.between(edge, 0, 1280), 720), (0, 720)], LIME)
    a.caption(c, t, cues)
