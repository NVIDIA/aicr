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
"""Draw the trust episode on provenance, tamper detection, and validation."""

import math

from . import shared as a
from .trust_middle import bundle, evidence, verify


BACKGROUND = '#0b0d10'
SLATE = '#29313a'
STEEL = '#9aa7b4'
SILVER = '#b9c5d0'
PAPER = '#f4f7f9'
ACCENT = '#93d22a'
DARK_GREEN = '#3e5620'


def inspect_lens(c, x, y, scale):
    c.save()
    c.translate(x, y)
    c.scale(scale, scale)
    a.line(c, [(33, 38), (76, 86)], a.INK, 23)
    a.line(c, [(33, 38), (76, 86)], '#556677', 15)
    a.ellipse(c, 0, 0, 53, 53, '#d2dfea', a.INK, 7, 0.76)
    a.ellipse(c, 0, 0, 49, 49, None, '#b8cadb', 2)
    a.curve(c, (-37, -17), (-30, -34), (-20, -41), (-5, -41), '#fafcff', 4, 0.84)
    a.text(c, 1, -24, '?', 55, '#435b70', 8, 'center')
    c.restore()


def opener_label(c, x, y, value, alpha):
    a.ellipse(c, x + 10, y + 11, 9, 9, ACCENT, alpha=alpha)
    a.tick(c, x + 10, y + 11, a.INK, 0.59, 2)
    a.text(c, x + 31, y, value, 22, SILVER, 5, alpha=alpha)


def hook(c, t):
    a.bg(c, BACKGROUND, '#252c34', True)
    a.ellipse(c, 966, 344, 268, 225, '#394653', alpha=0.5)
    a.ellipse(c, 966, 344, 228, 196, None, '#758598', 1.5, 0.50)
    a.soft_circuit(c, t, '#a5b4c3', 679, 214, 480, 369, 0.19)

    recipe_at, question_at = 7.08, 12.62
    u = a.reveal(t, 0.6, 0.72)
    if t < recipe_at:
        a.heading(c, t + 0.30, 'GPU KUBERNETES', ['A GPU cluster', 'has many', 'components'], None, True, 62, 142)
        a.line(c, [(71, 361), (71 + 154 * u, 361)], ACCENT, 3)
        d = a.reveal(t, 2.76, 0.65)
        f = a.reveal(t, 4.94, 0.65)
        if d > 0:
            opener_label(c, 70, 389, 'Dependencies', d)
        if f > 0:
            opener_label(c, 70, 428, 'Configuration files', f)
    elif t < question_at:
        step = t - recipe_at
        a.heading(c, step + 0.15, 'AICR RECIPES', ['Reproducible', 'configurations'], None, True, 68, 157)
        opener_label(c, 70, 362, 'Pinned component versions', a.reveal(step, 0.35, 0.55))
        p2 = a.reveal(t, 10.06, 0.59)
        if p2 > 0:
            opener_label(c, 70, 402, 'Artifact digests where supported', p2)
    else:
        a.heading(c, t - question_at + 0.10, 'AICR RECIPES', ['Which recipes', 'can you trust?'], None, True, 72, 157)
        u = a.reveal(t - question_at, 0.5, 0.65)
        a.line(c, [(71, 363), (71 + 157 * u, 363)], ACCENT, 3)

    p = a.reveal(t, recipe_at - 0.3, 1)
    q = a.reveal(t, question_at, 0.65)
    lift = 35 * (1 - a.reveal(t, 0.04, 0.8))
    c.save()
    c.translate(0, lift)
    a.ellipse(c, 951, 540, 264, 23, '#6a7a8d', alpha=0.24)
    a.rack(c, 947 + 110 * p, 533, 0.92, t + 0.25)
    a.rack(c, 1070 + 72 * p, 526, 0.72, t)
    a.robot(c, 757 + 29 * p, 554, 0.85, t, 'scan' if t > 1.8 else 'idle')
    dependency = a.reveal(t, 2.76, 0.68)
    if dependency > 0 and p < 0.95:
        a.curve(c, (937, 259), (963, 199), (1081, 203), (1099, 290), '#9fb3cf', 3, dependency * (1 - p), [6, 8])
        a.chip(c, 945, 235 + 5 * math.sin(t * 2), 0.49 * dependency * (1 - p), '#a6bf8d', -0.16)
        a.chip(c, 1061, 221 + 6 * math.sin(t * 2 + 1), 0.43 * dependency * (1 - p), '#9fb3cf', 0.17)
    filed = a.reveal(t, 4.94, 0.69)
    if filed > 0 and p < 0.98:
        a.card(c, 1090 - 110 * p, 397 + 29 * (1 - filed), 0.61 * filed * (1 - p), PAPER, 'YAML', None, 0.17)
        a.card(c, 1009 - 65 * p, 418 + 43 * (1 - filed), 0.66 * filed * (1 - p), '#dce8ee', 'YAML', None, -0.13)
    if p > 0:
        a.card(
            c,
            939,
            374 + 40 * (1 - p) + 5 * math.sin(t * 1.65),
            1.12 * p,
            PAPER,
            'RECIPE',
            ['Version pins', 'Settings', 'Checks'],
            -0.085,
        )
        a.spark(c, 837, 290, 15 * p, ACCENT)
    if q > 0:
        inspect_lens(c, 1004 + 12 * (1 - q) + 5 * math.sin(t * 2), 373 + 17 * (1 - q), 0.84 * q)
        a.spark(c, 1143, 259, 14 * q, ACCENT)
    c.restore()


def label_tile(c, x, y, width, title, detail, dark=False):
    if dark:
        a.rr(c, x - width / 2, y, width, 72, 15, '#1b2026', '#52606d', 1.5)
        a.text(c, x, y + 12, title, 21, a.WHITE, 8, 'center')
        a.text(c, x, y + 42, detail, 17, SILVER, 5, 'center')
    else:
        a.rr(c, x - width / 2, y, width, 72, 15, PAPER, '#7a8da1', 1.5)
        a.text(c, x, y + 12, title, 21, a.INK, 8, 'center')
        a.text(c, x, y + 42, detail, 17, '#46576a', 5, 'center')


def questions(c, t):
    a.bg(c, '#c7d2dd', '#edf2f5')
    a.heading(c, t, 'NVIDIA AI CLUSTER RUNTIME', ['Recipes you can trust'], None, size=71, y=131)
    a.ellipse(c, 306, 412, 131, 116, '#d2dbe1', alpha=0.85)
    a.ellipse(c, 983, 412, 131, 116, '#c5d8b2', alpha=0.72)
    a.ellipse(c, 640, 512, 125, 17, '#899eac', alpha=0.38)
    a.robot(c, 640, 518, 0.84, t, 'scan' if t > 4.0 else 'idle')
    p = a.reveal(t, 1.78, 0.60)
    if p > 0:
        a.curve(c, (463, 432), (489, 424), (522, 426), (548, 442), '#748da0', 2, 0.9, [5, 8])
        a.card(
            c,
            303,
            399 + 24 * (1 - p) + 4 * math.sin(t * 1.8),
            1.02,
            PAPER,
            'BUILD RECORD',
            ['Release', 'Signer', 'Digest'],
            -0.10,
        )
        a.badge(c, 368, 418, 0.76 * p)
        label_tile(c, 306, 525, 386, 'Provenance', 'Where did these components come from?')
    q = a.reveal(t, 4.2, 0.60)
    if q > 0:
        a.curve(c, (737, 439), (762, 424), (796, 424), (824, 432), '#748da0', 2, 0.9, [5, 8])
        a.rack(c, 948, 500 + 18 * (1 - q), 0.76, t)
        a.rack(c, 1032, 499 + 18 * (1 - q), 0.62, t + 0.23)
        a.spark(c, 1070, 315, 13 * q, '#577329')
        label_tile(c, 983, 525, 386, 'Recipe evidence', 'Did they work together on real hardware?')


def signed_release(c, t):
    a.bg(c, BACKGROUND, SLATE, True)
    a.heading(c, t, 'BUILD PROVENANCE', ['Verify the', 'AICR release'], None, True, 70, 151)
    p = a.reveal(t, 0.8, 0.6)
    a.text(c, 70, 320, 'Signed by the AICR release workflow', 24, SILVER, 5, alpha=p)
    a.ellipse(c, 963, 357, 254, 208, '#48596b', alpha=0.38)
    a.ellipse(c, 963, 357, 215, 178, None, '#72859a', 2, 0.44)
    a.soft_circuit(c, t, '#b0bdce', 688, 224, 501, 351, 0.19)

    motion = 28 * (1 - a.reveal(t, 0.04, 0.73))
    c.save()
    c.translate(0, motion)
    a.ellipse(c, 959, 531, 233, 28, '#0b0d10', alpha=0.37)
    a.rack(c, 1084, 521, 0.80, t)
    a.rack(c, 1151, 513, 0.56, t + 0.3)
    a.robot(c, 756, 538, 0.76, t, 'scan')
    a.card(
        c,
        987,
        365 + 4 * math.sin(t * 1.65),
        1.18,
        '#f6f8fa',
        'AICR RELEASE',
        ['Recipes', 'Provenance', 'SPDX SBOM'],
        -0.085,
    )
    q = a.reveal(t, 3.22, 0.62)
    if q > 0:
        a.badge(c, 1058, 429, 0.86 * q)
        a.spark(c, 1113, 256, 12 * q, ACCENT)
    c.restore()

    checked = a.reveal(t, 5.19, 0.7)
    if checked > 0:
        by = 407 + 14 * (1 - checked)
        a.rr(c, 70, by, 355, 64, 13, '#252e36', '#7a8da1', 1.5, checked)
        a.text(c, 91, by + 12, 'Content digest', 16, SILVER, 2, alpha=checked)
        a.text(c, 91, by + 36, 'sha256:4f2b…', 19, a.WHITE, 'mono', alpha=checked)
        a.ellipse(c, 392, by + 32, 15, 15, ACCENT, alpha=checked)
        a.tick(c, 391, by + 32, a.INK, 0.78, 2.5)


def end_card(c, t):
    a.bg(c, BACKGROUND, '#1b2026', True)
    f = a.reveal(t, 0.03, 0.66)
    a.text(c, 73, 104 + 12 * (1 - f), 'NVIDIA AI CLUSTER RUNTIME', 18, ACCENT, 'mono', alpha=f)
    a.text(c, 61, 145 + 18 * (1 - f), 'AICR', 125, a.WHITE, 8, alpha=f)
    a.text(c, 71, 274 + 10 * (1 - f), 'Recipes you can trust', 39, a.WHITE, 8, alpha=f)
    q = a.reveal(t, 0.47, 0.60)
    top = 359 + 17 * (1 - q)
    a.rr(c, 73, top, 275, 68, 34, ACCENT, alpha=q)
    a.text(c, 110, top + 16, 'aicr.run', 32, a.INK, 8, alpha=q)
    if q > 0.5:
        a.line(c, [(290, top + 34), (310, top + 34)], a.INK, 3, q)
        a.arrow(c, 310, top + 34, a.INK, 7)
    a.ellipse(c, 945, 346, 246, 229, '#354c1f', alpha=0.66)
    a.ellipse(c, 945, 346, 215, 200, None, '#89ab58', 1.5, 0.78)
    a.ellipse(c, 945, 346, 176, 164, None, '#89ab58', 1.3, 0.45)
    a.soft_circuit(c, t, '#a1bb80', 679, 252, 522, 292, 0.2)
    for i in range(6):
        angle = i * math.tau / 6 + t * 0.095
        a.ellipse(c, 945 + 215 * math.cos(angle), 346 + 200 * math.sin(angle), 4, 4, ACCENT, alpha=0.82)
    a.ellipse(c, 945, 550, 203, 29, a.INK, alpha=0.58)
    a.ellipse(c, 945, 536, 191, 28, '#395126', '#759c44', 2)
    a.robot(c, 950, 544, 1.35, t, 'wave')
    a.badge(c, 1124, 253 + 9 * math.sin(t * 2.1 + 0.5), 0.65)
    a.chip(c, 760, 251 + 10 * math.sin(t * 2.1), 0.5, '#9fb3cf', -0.22, True)
    for i, (x, y, s) in enumerate([(828, 193, 15), (1086, 164, 17), (1159, 399, 15), (736, 404, 16)]):
        a.spark(c, x, y, s * (0.84 + 0.16 * math.cos(t * 2 + i)), ACCENT, 0.14 * i)


SCENES = (hook, questions, signed_release, bundle, evidence, verify, end_card)
