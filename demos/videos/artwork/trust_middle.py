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
"""Bundle, contributor evidence, and local verification cartoon scenes."""

import math

from . import shared as a


SLATE = '#657488'
MIDDLE_SLATE = '#4b5868'
FAINT_STEEL = '#a4b9ca'
SHEET = '#f1f4f6'
DARK_SHEET = '#222a31'
DARK_RULER = '#435260'
WARNING = '#fcbaaa'


def _wrong(c, x, y, color=a.INK, size=6):
    a.line(c, [(x - size, y - size), (x + size, y + size)], color, 3)
    a.line(c, [(x - size, y + size), (x + size, y - size)], color, 3)


def _packet(c, x, y, size, fill=a.LIME):
    a.rr(c, x - size / 2, y - size / 2, size, size, 3, fill, a.INK, 1.3)


def _panel(c, x, y, width, height, dark=False):
    a.rr(c, x + 7, y + 9, width, height, 20, a.INK, alpha=0.15 if not dark else 0.54)
    a.rr(c, x, y, width, height, 20, DARK_SHEET if dark else SHEET, DARK_RULER if dark else a.INK, 2.4)


def _file(c, x, y, scale, changed=False, angle=0):
    c.save()
    c.translate(x, y)
    c.rotate(angle)
    c.scale(scale, scale)
    fill = WARNING if changed else a.WHITE
    points = [(-35, -46), (15, -46), (35, -26), (35, 46), (-35, 46)]
    a.poly(c, [(p + 4, q + 5) for p, q in points], a.INK, alpha=0.14)
    a.poly(c, points, fill, a.INK, 2.6)
    a.poly(c, [(15, -46), (15, -26), (35, -26)], '#f6f8fa', a.INK, 2.2)
    for i, length in enumerate((24, 39, 31, 42)):
        row = -17 + i * 13
        a.line(c, [(-20, row), (-20 + length, row)], a.INK if changed else SLATE, 2.5)
    c.restore()


def _chain_link(c, x, y, width, label, detail, progress):
    a.rr(c, x + 4, y + 6, width, 98, 17, a.INK, alpha=0.12)
    a.rr(c, x, y, width, 98, 17, SHEET, a.INK, 2.3)
    fill = a.MINT if progress > 0.2 else '#d7e2eb'
    a.ellipse(c, x + 37, y + 47, 18, 18, fill, a.INK, 1.5)
    if progress:
        a.tick(c, x + 37, y + 47, a.INK, 0.85 * progress, 2.7)
    a.text(c, x + 66, y + 26, label, 23, a.INK, 8)
    a.text(c, x + 67, y + 58, detail, 16, MIDDLE_SLATE, 2)


def _bundle_chain(c, t, alpha):
    if alpha <= 0:
        return
    c.push_group()
    chain = (
        (143, 'Bundle creator', 'Signed identity'),
        (491, 'AICR binary', 'Binary digest'),
        (839, 'AICR release', 'Signed build provenance'),
    )
    for index, (x, label, detail) in enumerate(chain):
        shown = a.reveal(t, 0.38 + index * 1.42, 0.68)
        if not shown:
            continue
        if index:
            a.line(c, [(x - 58, 288), (x - 11, 288)], '#526b32', 2.8, shown)
            a.arrow(c, x - 11, 288, '#526b32', 7)
            offset = 4 + ((t * 0.66 + 0.45 * index) % 1) * 35
            _packet(c, x - 58 + offset, 288, 7.5)
        _chain_link(c, x, 240 + 14 * (1 - shown), 292, label, detail, shown)
    c.pop_group_to_source()
    c.paint_with_alpha(alpha)


def _field_value(c, x, y, digit, changed=False):
    a.rr(c, x, y, 322, 43, 7, '#14191e', DARK_RULER, 1.2)
    label = 'replicas: '
    glyph_x = x + 13 + a.font(27, 'mono').getlength(label) / a.SCALE
    a.text(c, x + 13, y + 10, label, 27, a.WHITE, 'mono')
    if changed:
        a.rr(c, glyph_x - 4, y + 5, 24, 33, 5, WARNING)
    a.text(c, glyph_x, y + 10, digit, 27, a.INK if changed else a.WHITE, 'mono')


def _bundle_mismatch(c, t, alpha):
    if alpha <= 0:
        return
    c.push_group()
    rise = 14 * (1 - alpha)
    x, y = 142, 252 + rise
    _panel(c, x, y, 370, 233, True)
    a.text(c, x + 24, y + 23, 'Bundle configuration', 24, a.WHITE, 8)
    a.text(c, x + 24, y + 67, 'Original file', 16, a.WHITE, 2)
    _field_value(c, x + 24, y + 91, '1')
    a.text(c, x + 24, y + 153, 'Current file', 16, a.WHITE, 2)
    changed = t >= 7.05
    _field_value(c, x + 24, y + 174, '2' if changed else '1', changed)
    if 7.05 <= t <= 7.82:
        a.spark(c, x + 269, y + 195, 14 * math.sin(math.pi * (t - 7.05) / 0.77), WARNING, 0.2)

    x, y = 824, 283 + rise
    _panel(c, x, y, 313, 161)
    a.text(c, x + 22, y + 22, 'Bundle verification', 23, a.INK, 8)
    a.line(c, [(x + 22, y + 55), (x + 291, y + 55)], FAINT_STEEL, 1.2)
    verdict = a.reveal(t, 8.96, 0.42)
    if verdict:
        a.rr(c, x + 22, y + 69, 269, 43, 8, WARNING, a.INK, 1.6, verdict)
        _wrong(c, x + 42, y + 91, size=5)
        a.text(c, x + 59, y + 81, 'Verification failed', 19, a.INK, 8, alpha=verdict)
        a.text(c, x + 22, y + 127, 'File hash mismatch', 17, MIDDLE_SLATE, 2, alpha=verdict)
    else:
        a.text(c, x + 22, y + 80, 'Comparing file hashes', 18, MIDDLE_SLATE, 2)
        for i in range(3):
            beat = 0.42 + 0.53 * (0.5 + 0.5 * math.sin(5 * t - i * 0.7))
            a.ellipse(c, x + 27 + i * 17, y + 121, 3.9, 3.9, SLATE, alpha=beat)
    c.pop_group_to_source()
    c.paint_with_alpha(alpha)


def bundle(c, t):
    a.bg(c, '#cbd7e0', '#f0f4f7')
    a.heading(c, t, 'BUNDLE ATTESTATION', ['Verify the bundle'], None, size=62, y=127)
    a.ellipse(c, 627, 448, 513, 110, '#99adc0', alpha=0.22)
    a.conveyor(c, 520, min(t, 7.1))
    travel = a.smooth((t - 0.18) / 5.61)
    crate_x = a.lerp(318, 710, travel)
    crate_y = 456 - 11 * math.sin(math.pi * travel)
    a.robot(c, crate_x - 133, 520, 0.72, t, 'carry' if t < 8.96 else 'worried')
    a.crate(c, crate_x, crate_y, 0.98)
    seal = a.reveal(t, 0.75, 0.63)
    if seal:
        a.badge(c, crate_x + 21, crate_y + 5 - 38 * (1 - seal), 0.47 * seal)
    t_switch = 6.34
    _bundle_chain(c, t, 1 - a.reveal(t, t_switch, 0.45))
    _bundle_mismatch(c, t, a.reveal(t, t_switch + 0.13, 0.61))
    a.text(c, crate_x, 585, 'Deployment bundle', 19, a.INK, 2, 'center', alpha=a.reveal(t, 0.28, 0.61))


def _evidence_sheet(c, t):
    x, y = 768, 272
    enter = a.reveal(t, 1.55, 0.8)
    c.save()
    c.translate(x + 31 * (1 - enter), y + 15 * (1 - enter))
    c.rotate(-0.035)
    a.rr(c, 10, 15, 342, 276, 20, a.INK, alpha=0.65)
    a.rr(c, 0, 0, 342, 276, 20, '#9aa7b4', a.INK, 3)
    a.rr(c, 13, 13, 316, 250, 13, SHEET, a.INK, 2)
    a.rr(c, 126, -12, 89, 39, 11, a.MINT, a.INK, 2.8)
    a.ellipse(c, 171, 6, 4.2, 4.2, SHEET, a.INK, 1.5)
    a.text(c, 37, 52, 'Signed evidence', 27, a.INK, 8)
    for i, label in enumerate(('Recipe digest', 'Cluster snapshot', 'Check results')):
        row = 102 + 44 * i
        ready = a.reveal(t, 2.35 + 0.52 * i, 0.42)
        a.rr(c, 38, row, 20, 20, 5, a.MINT if ready else '#dbe5ed', a.INK, 1.7)
        if ready > 0:
            a.tick(c, 48, row + 10, a.INK, 0.69 * ready, 2.2)
        a.text(c, 72, row + 2, label, 19, a.INK, 2)
        if i < 2:
            a.line(c, [(38, row + 33), (291, row + 33)], '#b9c5d2', 1.25)
    stamp = a.reveal(t, 4.20, 0.58)
    if stamp > 0:
        a.badge(c, 287, 214 - 45 * (1 - stamp), 0.93 * stamp)
        a.spark(c, 326, 166, 14 * stamp, '#5b8529')
    c.restore()


def evidence(c, t):
    a.bg(c, '#101418', '#2b3440', True)
    a.heading(c, t, 'RECIPE EVIDENCE', ['Evidence from real clusters'], None, dark=True, size=57, y=132)
    a.ellipse(c, 357, 490, 269, 53, '#435260', alpha=0.61)
    a.ellipse(c, 947, 391, 281, 166, '#405466', alpha=0.35)
    a.soft_circuit(c, t, '#788e9d', 686, 234, 510, 326, 0.14)

    for idx, (y_start, y_end) in enumerate(((376, 348), (436, 418))):
        start_x = 535
        end_x = 771
        a.curve(c, (start_x, y_start), (623, y_start), (659, y_end), (end_x, y_end), FAINT_STEEL, 2.2, 0.70, [5, 7])
        for step in range(3):
            pos = (t * 0.43 + idx * 0.20 + step / 3.0) % 1.0
            _packet(
                c,
                a.lerp(start_x + 5, end_x - 7, pos),
                a.lerp(y_start, y_end, a.smooth(pos)),
                9,
                a.LIME if idx == 0 else a.WHITE,
            )

    a.rack(c, 244, 487, 0.76, t)
    a.rack(c, 336, 492, 0.87, t + 0.5)
    a.rack(c, 475, 483, 0.65, t + 0.9)
    a.robot(c, 186, 528, 0.65, t, 'scan' if t < 4.20 else 'idle')
    a.chip(c, 428, 303 + 5 * math.sin(t * 2), 0.53, a.TEAL, -0.15, True)
    a.text(c, 365, 557, 'Contributor GPU clusters', 21, a.WHITE, 2, 'center')
    _evidence_sheet(c, t)


def _verify_console(c, t):
    x, y, width, height = 121, 272, 391, 222
    _panel(c, x, y, width, height, True)
    a.text(c, x + 25, y + 24, 'Verify offline', 27, a.WHITE, 8)
    a.line(c, [(x + 25, y + 68), (x + width - 25, y + 68)], DARK_RULER, 1.2)
    a.rr(c, x + 24, y + 89, width - 48, 44, 8, '#14191e', DARK_RULER, 1.3)
    a.text(c, x + 38, y + 105, 'aicr evidence verify', 16, a.WHITE, 'mono')
    for i, label in enumerate(('Check signature', 'Check file hashes')):
        row = y + 155 + 30 * i
        active = a.reveal(t, 0.74 + 0.66 * i, 0.46)
        a.ellipse(c, x + 32, row + 7, 7, 7, a.LIME if active else SLATE)
        if active > 0.1:
            a.tick(c, x + 32, row + 7, a.INK, 0.48 * active, 1.5)
        a.text(c, x + 47, row, label, 16, a.WHITE, 2)


def _local_cluster(c, t):
    a.ellipse(c, 1014, 495, 175, 27, '#91a5b9', alpha=0.43)
    a.rack(c, 1048, 491, 0.79, t + 0.4)
    a.rack(c, 956, 491, 0.60, t)
    p = a.reveal(t, 3.18, 0.72)
    if p > 0:
        wave_y = a.lerp(327, 468, ((t - 3.18) * 0.77) % 1)
        wash = 0.45 * (1 - ((t - 3.18) * 0.77) % 1)
        a.line(c, [(1002, wave_y), (1097, wave_y)], '#b3ef58', 2.5, wash)
    heading_alpha = a.reveal(t, 2.64, 0.54)
    rise = 14 * (1 - heading_alpha)
    a.text(c, 1005, 526 + rise, 'Validate your cluster', 22, a.INK, 8, 'center', heading_alpha)
    a.text(c, 1005, 559 + rise, 'aicr validate', 16, MIDDLE_SLATE, 'mono', 'center', heading_alpha)


def verify(c, t):
    a.bg(c, '#cbd6e1', '#eef2f6')
    a.heading(c, t, 'YOUR CLUSTER', ['Verify it. Then test your cluster.'], None, size=57, y=127)
    a.ellipse(c, 664, 449, 518, 111, '#9aacbe', alpha=0.19)
    a.curve(c, (502, 396), (586, 396), (553, 447), (628, 431), MIDDLE_SLATE, 2.5, 0.75, [6, 7])
    _verify_console(c, t)

    rise = 16 * (1 - a.reveal(t, 0.25, 0.65))
    c.save()
    c.translate(0, rise)
    a.ellipse(c, 685, 520, 133, 23, '#91a5b9', alpha=0.47)
    a.robot(c, 684, 514, 0.84, t, 'idle' if t < 2.64 else 'scan')
    if t < 1.95:
        _file(c, 785, 366 + 4 * math.sin(t * 2.2), 0.5, angle=0.13)
    else:
        a.badge(c, 783, 355 + 4 * math.sin(t * 2), 0.58 * a.reveal(t, 1.95, 0.43))
    c.restore()

    active = a.reveal(t, 2.82, 0.56)
    if active > 0:
        head = a.lerp(798, 900, active)
        a.line(c, [(798, 418), (head, 418)], '#5d7d2d', 3)
        a.arrow(c, head, 418, '#5d7d2d', 7)
        _packet(c, a.lerp(810, 895, (t * 0.55) % 1), 418, 8)
    _local_cluster(c, t)
