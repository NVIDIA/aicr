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
"""Draw the shared mascot, scene elements, captions, and overview."""

from functools import lru_cache
from io import BytesIO
import math
from pathlib import Path
import random

import cairo
from PIL import Image, ImageDraw, ImageFont

from .mascot import draw_robot

WIDTH, HEIGHT = 1280, 720
SCALE = 1.5
INK = '#0b0d10'
WHITE = '#e6edf3'
LIME = '#93d22a'
GREEN = '#76b900'
MINT = '#d6ebb4'
BLUE = '#e6edf3'
TEAL = '#9fb3cf'
CORAL = '#eb957c'
YELLOW = '#b1cb83'
BLUE_DARK = '#1b2026'
FOREST = '#0b0d10'
PAPER = '#e6edf3'
TAU = math.tau
MAC_SANS = Path("/System/Library/Fonts/Avenir Next.ttc")
MAC_MONO = Path("/System/Library/Fonts/Menlo.ttc")
LINUX_FONTS = Path("/usr/share/fonts/truetype/dejavu")
LINUX_MONO = LINUX_FONTS / "DejaVuSansMono-Bold.ttf"
LINUX_FACES = {
    0: "DejaVuSans-Bold.ttf",
    1: "DejaVuSans-BoldOblique.ttf",
    2: "DejaVuSans-Bold.ttf",
    3: "DejaVuSans-BoldOblique.ttf",
    4: "DejaVuSans-Oblique.ttf",
    5: "DejaVuSans.ttf",
    6: "DejaVuSans-Oblique.ttf",
    7: "DejaVuSans.ttf",
    8: "DejaVuSans-Bold.ttf",
}


def between(v, lower=0.0, upper=1.0):
    return max(lower, min(v, upper))


def smooth(v):
    v = between(v)
    return v * v * (3 - 2 * v)


def out(v):
    return 1 - (1 - between(v)) ** 3


def lerp(a, b, f):
    return a + (b - a) * f


def reveal(t, at=0, duration=0.65):
    return out((t - at) / duration)


@lru_cache(maxsize=64)
def rgb(color):
    return tuple(int(color[i:i + 2], 16) / 255 for i in (1, 3, 5))


def rgba(c, color, alpha=1):
    c.set_source_rgba(*rgb(color), alpha)


def paint_path(c, fill, stroke=None, width=3, alpha=1):
    if fill:
        rgba(c, fill, alpha)
        if stroke:
            c.fill_preserve()
        else:
            c.fill()
    if stroke:
        rgba(c, stroke, alpha)
        c.set_line_width(width)
        c.stroke()


def rr(c, x, y, w, h, r, fill, stroke=None, width=3, alpha=1):
    r = min(w / 2, h / 2, r)
    c.new_sub_path()
    c.arc(x + w - r, y + r, r, -math.pi / 2, 0)
    c.arc(x + w - r, y + h - r, r, 0, math.pi / 2)
    c.arc(x + r, y + h - r, r, math.pi / 2, math.pi)
    c.arc(x + r, y + r, r, math.pi, 3 * math.pi / 2)
    c.close_path()
    paint_path(c, fill, stroke, width, alpha)


def ellipse(c, x, y, rx, ry, fill, stroke=None, width=3, alpha=1):
    c.save()
    c.translate(x, y)
    c.scale(rx, ry)
    c.new_sub_path()
    c.arc(0, 0, 1, 0, TAU)
    c.restore()
    paint_path(c, fill, stroke, width, alpha)


def poly(c, points, fill, stroke=None, width=3, alpha=1):
    c.move_to(*points[0])
    for point in points[1:]:
        c.line_to(*point)
    c.close_path()
    paint_path(c, fill, stroke, width, alpha)


def line(c, pts, color, width=3, alpha=1):
    c.move_to(*pts[0])
    for pt in pts[1:]:
        c.line_to(*pt)
    paint_path(c, None, color, width, alpha)


def curve(c, start, a, b, end, color, width=3, alpha=1, dash=None):
    c.save()
    if dash:
        c.set_dash(dash)
    c.move_to(*start)
    c.curve_to(*a, *b, *end)
    paint_path(c, None, color, width, alpha)
    c.restore()


def arrow(c, x, y, color=INK, size=8):
    line(c, [(x - size, y - size), (x, y), (x - size, y + size)], color, 3)


def spark(c, x, y, s, color, rotation=0, alpha=1):
    if s <= 0:
        return
    c.save()
    c.translate(x, y)
    c.rotate(rotation)
    poly(
        c,
        [
            (0, -s),
            (s * 0.2, -s * 0.2),
            (s, 0),
            (s * 0.2, s * 0.2),
            (0, s),
            (-s * 0.2, s * 0.2),
            (-s, 0),
            (-s * 0.2, -s * 0.2),
        ],
        color,
        alpha=alpha,
    )
    c.restore()


def tick(c, x, y, color=INK, scale=1, width=3.5):
    line(c, [(x - 8 * scale, y), (x - 2 * scale, y + 6 * scale), (x + 9 * scale, y - 8 * scale)], color, width)


@lru_cache(maxsize=128)
def font(size, weight):
    """Load the native macOS face or its installed DejaVu fallback."""
    pixels = round(size * SCALE)
    if weight == "mono":
        if MAC_MONO.is_file():
            return ImageFont.truetype(str(MAC_MONO), pixels, index=1)
        return ImageFont.truetype(str(LINUX_MONO), pixels)
    if MAC_SANS.is_file():
        return ImageFont.truetype(str(MAC_SANS), pixels, index=weight)
    return ImageFont.truetype(str(LINUX_FONTS / LINUX_FACES[weight]), pixels)


@lru_cache(maxsize=256)
def text_surface(value, size, weight, color):
    fnt = font(size, weight)
    x0, y0, x1, y1 = fnt.getbbox(value, anchor='lt')
    if x1 <= x0 or y1 <= y0:
        return None, 0
    img = Image.new('RGBA', (x1 - x0 + 4, y1 - y0 + 4))
    ImageDraw.Draw(img).text((2 - x0, 2 - y0), value, font=fnt, fill=color, anchor='lt')
    buffer = BytesIO()
    img.save(buffer, format='PNG')
    buffer.seek(0)
    return cairo.ImageSurface.create_from_png(buffer), (x1 - x0) / SCALE


def text(c, x, y, value, size, color=INK, weight=0, align='left', alpha=1):
    surface, width = text_surface(value, size, weight, color)
    if not surface:
        return
    if align == 'center':
        x -= width / 2
    if align == 'right':
        x -= width
    c.save()
    c.translate(x - 2 / SCALE, y - 2 / SCALE)
    c.scale(1 / SCALE, 1 / SCALE)
    c.set_source_surface(surface)
    c.paint_with_alpha(alpha)
    c.restore()


def heading(c, t, eyebrow, title_lines, description, dark=False, size=64, y=143):
    main, accent = (WHITE, LIME) if dark else (INK, '#48651a')
    f = reveal(t, 0.02, 0.6)
    text(c, 69, y - 57 + 10 * (1 - f), eyebrow, 17, accent, 'mono', alpha=f)
    for i, title in enumerate(title_lines):
        u = reveal(t, 0.08 + i * 0.085, 0.67)
        text(c, 65, y + i * (size * 0.99) + 16 * (1 - u), title, size, main, 8, alpha=u)
    if description:
        v = reveal(t, 0.30, 0.6)
        text(c, 70, y + len(title_lines) * size * 0.99 + 14 + 10 * (1 - v), description, 24, main, 5, alpha=v)


def chip(c, x, y, s, fill=LIME, rotate=0, gpu=False):
    c.save()
    c.translate(x, y)
    c.rotate(rotate)
    c.scale(s, s)
    for offset in [-20, 0, 20]:
        line(c, [(offset, -48), (offset, -34)], INK, 5)
        line(c, [(offset, 34), (offset, 48)], INK, 5)
        line(c, [(-48, offset), (-34, offset)], INK, 5)
        line(c, [(34, offset), (48, offset)], INK, 5)
    rr(c, -34, -34, 68, 68, 10, fill, INK, 3)
    rr(c, -22, -22, 44, 44, 7, WHITE, INK, 2)
    if gpu:
        text(c, 0, -7, 'GPU', 14, INK, 'mono', 'center')
    else:
        spark(c, 0, 0, 14, INK)
    c.restore()


def rack(c, x, y, s, t, state='ok'):
    c.save()
    c.translate(x, y)
    c.scale(s, s)
    ellipse(c, 0, 0, 116, 16, INK, alpha=0.16)
    poly(c, [(-75, -236), (-49, -255), (87, -255), (69, -236)], '#8899ad', INK, 3)
    poly(c, [(69, -236), (87, -255), (87, -27), (69, -8)], '#4b5868', INK, 3)
    rr(c, -75, -237, 144, 230, 14, '#465361', INK, 3)
    rr(c, -66, -225, 126, 201, 9, '#1b2026', INK, 2)
    for i in range(4):
        row = -211 + i * 46
        rr(c, -58, row, 109, 34, 6, '#a7b5c3' if i == 1 else '#c9d5e0', INK, 2)
        for j in range(3):
            line(c, [(-44, row + 10 + j * 7), (-16 + (j % 2) * 10, row + 10 + j * 7)], '#536174', 2)
        bad = state == 'warn' and i == 1
        lit = ((t * 1.45 + i * 0.33) % 1) < 0.62
        ellipse(c, 27, row + 17, 6, 6, CORAL if bad and lit else (LIME if lit else '#728394'), INK, 1.5)
        ellipse(c, 40, row + 17, 2.5, 2.5, INK)
    rr(c, -62, -13, 24, 13, 4, INK)
    rr(c, 35, -13, 24, 13, 4, INK)
    c.restore()


def card(c, x, y, scale, fill=WHITE, label='RECIPE', detail=None, angle=0):
    c.save()
    c.translate(x, y)
    c.rotate(angle)
    c.scale(scale, scale)
    rr(c, -72 + 7, -100 + 8, 144, 192, 15, INK, alpha=0.16)
    rr(c, -72, -100, 144, 192, 15, fill, INK, 3)
    path = [(-72, -56), (72, -56)]
    line(c, path, INK, 2)
    text(c, 0, -84, label, 14, INK, 'mono', 'center')
    if detail:
        for i, value in enumerate(detail):
            row = -28 + i * 32
            ellipse(c, -50, row + 7, 5, 5, LIME, INK, 1.6)
            text(c, -37, row, value, 13, INK, 2)
    else:
        for i, amount in enumerate([61, 83, 48, 69]):
            rr(c, -47, -27 + i * 24, amount, 5, 2.5, '#728096')
            ellipse(c, 46, -25 + i * 24, 3, 3, '#728096')
    c.restore()


def crate(c, x, y, s, open_amount=0):
    c.save()
    c.translate(x, y)
    c.scale(s, s)
    ellipse(c, 0, 63, 108, 15, INK, alpha=0.14)
    poly(c, [(-86, -29), (-32, -60), (86, -59), (37, -28)], '#c6e88b', INK, 3)
    poly(c, [(37, -28), (86, -59), (86, 28), (37, 62)], '#648b2b', INK, 3)
    rr(c, -86, -29, 123, 91, 5, LIME, INK, 3)
    poly(c, [(-15, -29), (4, -29), (4, 16), (-5, 10), (-15, 16)], '#dfecca', INK, 2)
    rr(c, -69, 24, 46, 22, 4, WHITE, INK, 2)
    for i in range(6):
        rr(c, -63 + 6 * i, 29, 2 + i % 2, 11, 1, INK)
    if open_amount > 0:
        p = open_amount
        poly(c, [(-86, -29), (-32, -60), (-44, -60 - 29 * p), (-98, -26 - 31 * p)], '#cde9a1', INK, 2.5)
    c.restore()


def badge(c, x, y, s, kind='tick'):
    c.save()
    c.translate(x, y)
    c.scale(s, s)
    for side in [-1, 1]:
        poly(c, [(7 * side, 13), (23 * side, 11), (28 * side, 46), (16 * side, 40), (6 * side, 48)], GREEN, INK, 2.5)
    points = []
    for i in range(26):
        ang = i * TAU / 26
        radius = 34 if i % 2 == 0 else 29
        points.append((math.sin(ang) * radius, math.cos(ang) * radius))
    poly(c, points, LIME, INK, 3)
    ellipse(c, 0, 0, 22, 22, '#e3efcf', INK, 2)
    tick(c, 0, 0, INK, 1.14, 3.7)
    c.restore()


def pill(c, x, y, value, fill=WHITE, size=18, stroke=INK):
    w = max(82, font(size, 2).getlength(value) / SCALE + 28)
    rr(c, x - w / 2, y, w, size + 20, (size + 20) / 2, fill, stroke, 2)
    text(c, x, y + 10, value, size, INK, 2, 'center')
    return w


def soft_circuit(c, t, ink, x, y, width, height, alpha=0.2):
    for i in range(6):
        row = y + i * height / 5
        a = x + (i % 3) * 14
        b = x + width - (i % 4) * 15
        line(c, [(a, row), (a + 30, row), (a + 47, row - 17), (b - 28, row - 17), (b, row - 45)], ink, 2, alpha)
        ellipse(c, a, row, 4, 4, None, ink, 1.5, alpha)
        if i % 2 == 0:
            phase = (t * 0.3 + i * 0.21) % 1
            ellipse(c, lerp(a + 50, b - 32, phase), row - 17, 2.6, 2.6, ink, alpha=alpha * 2)


@lru_cache(maxsize=1)
def grain_surface():
    rng = random.Random(613)
    w, h = 720, 405
    img = Image.new('RGBA', (w, h))
    data = []
    for _ in range(w * h):
        shade = 255 if rng.random() > 0.54 else 0
        data.append((shade, shade, shade, rng.choice([0, 0, 4, 7, 10, 16, 22])))
    img.putdata(data)
    buf = BytesIO()
    img.save(buf, 'PNG')
    buf.seek(0)
    return cairo.ImageSurface.create_from_png(buf)


def grain(c):
    c.save()
    c.scale(WIDTH / 720, HEIGHT / 405)
    c.set_source_surface(grain_surface())
    c.paint_with_alpha(0.46)
    c.restore()


def _draw_background(c, base, light, dark):
    gradient = cairo.LinearGradient(0, 0, WIDTH, HEIGHT)
    gradient.add_color_stop_rgb(0, *rgb(light))
    gradient.add_color_stop_rgb(1, *rgb(base))
    c.set_source(gradient)
    c.paint()
    grain(c)
    rgba(c, WHITE if dark else INK, 0.16)
    c.set_line_width(1)
    c.move_to(68, 58)
    c.line_to(1212, 58)
    c.stroke()


@lru_cache(maxsize=12)
def background(base, light, dark=False):
    """Compose one reusable background at the output frame size."""
    image = cairo.ImageSurface(cairo.FORMAT_RGB24, round(WIDTH * SCALE), round(HEIGHT * SCALE))
    ctx = cairo.Context(image)
    ctx.scale(SCALE, SCALE)
    _draw_background(ctx, base, light, dark)
    return image


def bg(c, base, light, dark=False):
    """Paint the background onto the output frame."""
    if c.get_matrix() != cairo.Matrix(SCALE, 0, 0, SCALE):
        _draw_background(c, base, light, dark)
        return
    c.save()
    c.identity_matrix()
    c.set_source_surface(background(base, light, dark))
    c.paint()
    c.restore()


def robot(c, x, y, s, t, pose='idle'):
    draw_robot(c, x, y, s, t, pose)


def intro(c, t):
    bg(c, '#0b0d10', '#252c34', True)
    ellipse(c, 967, 339, 280, 235, '#3a4653', alpha=0.53)
    ellipse(c, 967, 339, 240, 206, None, '#718293', 1.5, 0.48)
    soft_circuit(c, t, '#a5b4c3', 678, 228, 460, 342, 0.22)
    heading(
        c, t + 0.45, 'GPU KUBERNETES', ['GPU clusters', 'have a lot of', 'moving parts'], None, True, size=65, y=157
    )
    f = reveal(t, 0.55, 0.8)
    line(c, [(71, 384), (71 + 173 * f, 384)], '#93d22a', 3)
    text(c, 70, 410, 'Drivers, network, Kubernetes, runtimes, operators', 21, '#b8c4d1', 5, alpha=f)

    settle = 1 - reveal(t, 0.14, 0.74)
    c.save()
    c.translate(0, 60 * settle)
    ellipse(c, 920, 551, 266, 31, '#6a7a8d', alpha=0.24)
    curve(c, (877, 531), (838, 620), (721, 528), (826, 510), '#8090a1', 5, 0.76)
    curve(c, (991, 530), (1083, 585), (1166, 544), (1113, 478), '#8090a1', 4, 0.76)
    rack(c, 1080, 517, 0.89, t)
    rack(c, 953, 558, 1.20, t, 'warn' if t > 4.04 else 'ok')
    robot(c, 779, 577, 0.79, t, 'worried' if t > 4.04 else 'idle')
    chip(c, 1067, 217 + 8 * math.sin(t * 2), 0.64, TEAL, 0.24, True)
    q = reveal(t, 4.05, 0.43)
    if q > 0:
        c.save()
        c.translate(1148, 303 + 6 * math.sin(t * 3))
        c.rotate(0.16)
        c.scale(q, q)
        rr(c, -33, -35, 64, 65, 17, CORAL, INK, 3)
        line(c, [(0, -18), (0, 1)], INK, 5)
        ellipse(c, 0, 13, 3, 3, INK)
        c.restore()
        spark(c, 1186, 254, 15 * q, INK, alpha=0.66)
        spark(c, 1127, 227, 11 * q, CORAL)
    else:
        chip(c, 1140, 311 + 8 * math.sin(t * 2 + 1), 0.56, YELLOW, -0.22)
    c.restore()


def meet(c, t):
    bg(c, '#0b0d10', '#1b2026', True)
    heading(c, t, 'OPEN SOURCE', ['Meet AICR'], None, True, 94, 165)
    f = reveal(t, 0.20, 0.65)
    text(c, 72, 282 + 10 * (1 - f), 'NVIDIA AI Cluster Runtime', 31, WHITE, 2, alpha=f)
    f2 = reveal(t, 1.1, 0.75)
    text(c, 73, 344 + 7 * (1 - f2), 'Reproducible GPU cluster', 25, '#b9c5d0', 5, alpha=f2)
    text(c, 73, 379 + 7 * (1 - f2), 'configurations', 25, '#b9c5d0', 5, alpha=f2)
    ellipse(c, 934, 324, 244, 222, '#3e5620', alpha=0.37)
    ellipse(c, 934, 324, 217, 195, None, '#80a64a', 1.7, 0.59)
    ellipse(c, 934, 324, 179, 161, None, '#80a64a', 1.2, 0.32)
    soft_circuit(c, t, '#aac685', 691, 205, 490, 362, 0.14)
    settle = 1 - reveal(t, 0.02, 0.8)
    c.save()
    c.translate(0, 38 * settle)
    ellipse(c, 940, 551, 248, 40, INK, alpha=0.75)
    ellipse(c, 940, 535, 235, 34, '#395126', '#759c44', 2)
    rack(c, 1028, 532, 0.94, t)
    rack(c, 1131, 524, 0.64, t + 0.2)
    robot(c, 851, 548, 1.22, t, 'cheer' if t < 2.5 else 'idle')
    for i, (x, y, col) in enumerate([(768, 229, TEAL), (984, 185, YELLOW), (1105, 259, LIME)]):
        a = reveal(t, 0.32 + 0.18 * i, 0.72)
        if a > 0:
            chip(
                c,
                x + 28 * (1 - a),
                y - 31 * (1 - a) + 7 * math.sin(t * 2 + i),
                0.44 * a,
                col,
                (i - 1) * 0.22 + 0.06 * math.sin(t * 2),
            )
    spark(c, 715, 325, (12 + 3 * math.sin(t * 2)), LIME)
    spark(c, 1170, 358, (14 + 4 * math.sin(t * 2 + 1)), YELLOW)
    c.restore()


def recipe(c, t):
    bg(c, '#c6d2de', '#eef2f5')
    heading(c, t, 'RECIPE', ['Build a recipe for your cluster'], None, size=61, y=136)
    text(c, 70, 220, 'Start with target details or a cluster snapshot.', 23, INK, 5, alpha=reveal(t, 0.32))
    for x, w in [(227, 144), (633, 146), (1021, 157)]:
        ellipse(c, x, 535, w, 22, '#a5b3c3', alpha=0.55)
    p = reveal(t, 0.20, 0.85)
    q = reveal(t, 3.6, 1.05)
    for y, offset in [(392, -22), (448, 16)]:
        curve(c, (330, y), (434, y), (463, 433 + offset), (512, 433 + offset), '#6f8991', 2.5, 0.85, [5, 8])
        x = 338 + 154 * ((t * 0.44 + (y == 448) * 0.52) % 1)
        ellipse(c, x, lerp(y, 433 + offset, (x - 338) / 154), 4, 4, LIME, INK, 1)
    curve(c, (744, 412), (822, 412), (852, 407), (938, 407), '#6f8991', 2.5, 0.85, [5, 8])
    if q > 0:
        arrow(c, 930, 407)
        j = (t * 0.55) % 1
        ellipse(c, lerp(770, 918, j), lerp(411, 407, j), 4.5, 4.5, LIME, INK, 1.4)
    rack(c, 212, 534, 0.79, t)
    a = reveal(t, 0.55, 0.60)
    if a > 0:
        card(c, 292 - 14 * (1 - a), 467 - 20 * (1 - a), 0.51, WHITE, 'SNAPSHOT', None, 0.12)
        for scan in range(2):
            z = (t * 0.47 + 0.5 * scan) % 1
            scan_y = 331 + z * 159
            line(c, [(153, scan_y), (271, scan_y)], '#b3ef58', 2.4, a * (0.52 + 0.35 * math.sin(z * math.pi)))
    robot(c, 633, 543, 0.9, t, 'scan' if t > 0.8 else 'idle')
    p2 = reveal(t, 2.25, 0.68)
    if p2 > 0:
        pill(c, 565, 560 + 13 * (1 - p2), 'Training', MINT, 17)
        pill(c, 699, 560 + 13 * (1 - p2), 'Inference', WHITE, 17)
    if q > 0:
        card(
            c,
            1022,
            416 - 43 * (1 - q) + 4 * math.sin(t * 1.95),
            1.20 * (0.82 + 0.18 * q),
            '#f6f8fa',
            'RECIPE',
            ['Pinned versions', 'Settings', 'Checks'],
            -0.055,
        )
        spark(c, 1127, 330, 16 * q, '#4b641f')
        spark(c, 916, 485, 11 * q, '#69814e')
    text(c, 220, 570, 'Target or snapshot', 17, INK, 2, 'center', alpha=p)


def conveyor(c, y, t):
    rr(c, 131, y + 10, 1012, 38, 18, '#657488', INK, 3)
    rr(c, 131, y, 1012, 30, 14, '#29313a', INK, 3)
    for idx in range(21):
        x = 153 + 47.9 * idx
        c.save()
        c.arc(x, y + 15, 9, 0, TAU)
        c.clip()
        ellipse(c, x, y + 15, 10, 10, '#b9c5d2', INK, 1.5)
        ang = -t * 3.2
        for i in range(3):
            a = ang + i * TAU / 3
            line(c, [(x, y + 15), (x + 8 * math.cos(a), y + 15 + 8 * math.sin(a))], '#5e6d80', 1.7)
        c.restore()
    for x in (181, 1092):
        rr(c, x, y + 44, 16, 20, 3, '#596678', INK, 2)


def bundle(c, t):
    bg(c, '#ccd7e1', '#f1f4f6')
    heading(
        c,
        t,
        'BUNDLE AND DEPLOY',
        ['From recipe to bundle'],
        'Use the deployment tools you already know.',
        size=62,
        y=136,
    )
    ellipse(c, 633, 441, 499, 113, '#90a3b7', alpha=0.18)
    conveyor(c, 490, t)
    p = reveal(t, 0.3, 0.72)
    pack = smooth((t - 1.15) / 2.1)
    ship = smooth((t - 4.00) / 2.28)
    card_x = lerp(220, 575, pack)
    card_y = 395 - 82 * math.sin(math.pi * pack)
    c.save()
    c.rectangle(0, 0, WIDTH, 489)
    c.clip()
    card(c, 221, 413, 0.65, WHITE, 'RECIPE', None, -0.09)
    if 0 < pack < 0.98:
        card(c, card_x, card_y, 0.58 * (1 - 0.48 * pack), WHITE, 'RECIPE', None, -0.11 + 0.28 * pack)
    robot(c, 643 - 130 * ship, 489, 0.72, t, 'carry' if t < 5.55 else 'cheer')
    box_x = lerp(576, 926, ship)
    crate(c, box_x, 433 - 15 * math.sin(ship * math.pi), 0.94, p * (1 - reveal(t, 3.45, 0.56)))
    rack(c, 1021, 491, 0.73, t)
    rack(c, 1119, 489, 0.57, t + 0.25)
    if t > 5.7:
        v = reveal(t, 5.7, 0.41)
        spark(c, 965, 337, 17 * v, INK, 0.12)
        spark(c, 1113, 302, 20 * v, '#537a27')
    c.restore()
    text(c, 221, 568, 'Recipe', 19, INK, 2, 'center')
    text(c, 617, 568, 'Deployment bundle', 19, INK, 2, 'center')
    text(c, 1052, 568, 'Kubernetes cluster', 19, INK, 2, 'center')
    for i, label in enumerate(['Helm', 'Helmfile', 'Argo CD', 'Flux']):
        a = reveal(t, 3.85 + i * 0.14, 0.68)
        if a > 0:
            pill(c, 670 + i * 103, 288 + 12 * (1 - a), label, '#f6f8fa', 17)


def heartbeat(c, x, y, t):
    rr(c, x, y, 188, 104, 14, '#29313a', '#8194a9', 2)
    for j in range(1, 5):
        line(c, [(x + 12, y + 18 * j), (x + 176, y + 18 * j)], '#667b8b', 1, 0.38)
    for j in range(1, 8):
        line(c, [(x + 22 * j, y + 12), (x + 22 * j, y + 92)], '#667b8b', 1, 0.35)
    base = [
        (x + 13, y + 70),
        (x + 41, y + 70),
        (x + 56, y + 50),
        (x + 72, y + 76),
        (x + 93, y + 28),
        (x + 112, y + 63),
        (x + 132, y + 56),
        (x + 173, y + 56),
    ]
    line(c, base, LIME, 3.0)
    q = (0.42 * t) % 1
    pos = q * (len(base) - 1)
    i = min(int(pos), len(base) - 2)
    dot = [lerp(base[i][j], base[i + 1][j], pos - i) for j in range(2)]
    ellipse(c, *dot, 5, 5, WHITE, INK, 1.5)


def clipboard(c, x, y, t):
    c.save()
    c.translate(x, y)
    c.rotate(-0.042)
    rr(c, -169 + 9, -168 + 12, 338, 337, 22, '#0b0d10', alpha=0.72)
    rr(c, -169, -168, 338, 337, 22, '#9aa7b4', INK, 4)
    rr(c, -154, -153, 308, 304, 13, '#f1f4f6', INK, 2.4)
    rr(c, -52, -182, 104, 42, 12, YELLOW, INK, 3)
    ellipse(c, 0, -163, 4.8, 4.8, '#e3efcf', INK, 2)
    text(c, -120, -104, 'Validation checks', 25, INK, 8)
    for i, label in enumerate(['Deployment health', 'Performance', 'Conformance']):
        at = 3.62 + i * 0.88
        row = -41 + i * 56
        ready = reveal(t, at, 0.43)
        line(c, [(-123, row + 34), (123, row + 34)], '#c6d1dc', 1.5)
        rr(c, -122, row, 24, 24, 6, LIME if ready > 0.05 else WHITE, INK, 2)
        if ready > 0.05:
            tick(c, -110, row + 12, INK, ready * 0.84, 2.7)
        text(c, -83, row + 3, label, 19, INK, 2)
    c.restore()


def validate(c, t):
    bg(c, '#0b0d10', '#29313a', True)
    heading(c, t, 'VALIDATE', ['Check the', 'deployed cluster'], None, True, 62, 147)
    text(c, 71, 297, 'Use the same recipe.', 25, '#b9c5d0', 5, alpha=reveal(t, 0.35))
    ellipse(c, 939, 353, 279, 212, '#455564', alpha=0.45)
    soft_circuit(c, t, '#a4b9ca', 681, 229, 505, 348, 0.22)
    rack(c, 744, 543, 0.67, t)
    heartbeat(c, 667, 245, t)
    c.save()
    p = reveal(t, 0.05, 0.75)
    c.translate(0, 38 * (1 - p))
    clipboard(c, 1004, 360, t)
    c.restore()
    p = reveal(t, 6.24, 0.62)
    if p > 0:
        c.save()
        c.translate(705, 447)
        c.translate(0, 17 * (1 - p))
        c.scale(0.9 + 0.1 * p, 0.9 + 0.1 * p)
        rr(c, -634, 0, 283, 60, 15, '#29313a', '#788da2', 1.5, alpha=p)
        badge(c, -609, 23, 0.46)
        text(c, -578, 13, 'Signed evidence', 22, WHITE, 8, alpha=p)
        text(c, -578, 37, 'Optional', 15, '#b9c5d0', 2, alpha=p)
        c.restore()
        spark(c, 1179, 169, 14 * p, LIME)


def rollouts(c, t):
    bg(c, '#0b0d10', '#29313a', True)
    heading(
        c,
        t,
        'OPTIONAL OPERATOR',
        ['Coordinate rollouts'],
        'Manage changes as the cluster evolves.',
        True,
        size=65,
        y=136,
    )
    for x in (250, 648, 1048):
        ellipse(c, x, 525, 144, 24, '#6a7b8c', alpha=0.45)
    for x, y in [(422, 405), (815, 405)]:
        line(c, [(x - 44, y), (x + 57, y)], '#8fa2b6', 3)
        arrow(c, x + 58, y, '#aebdca', 9)
        f = (t * 0.57 + x / 1700) % 1
        ellipse(c, lerp(x - 44, x + 52, f), y, 5, 5, LIME, INK, 1.5)
    rr(c, 519, 324, 260, 187, 20, '#222832', alpha=0.55)
    rr(c, 512, 316, 260, 187, 20, WHITE, INK, 3)
    line(c, [(513, 365), (771, 365)], INK, 2)
    text(c, 641, 333, 'Rollout gates', 22, INK, 8, 'center')
    for i, label in enumerate(['Approval when required', 'Revision readiness']):
        w = reveal(t, 2.35 + i * 1.25, 0.44)
        y = 392 + i * 49
        ellipse(c, 541, y + 10, 12, 12, LIME if w > 0.01 else '#cfdae5', INK, 2)
        if w > 0.01:
            tick(c, 541, y + 10, INK, 0.74 * w, 2.4)
        text(c, 563, y + 3, label, 16, INK, 2)
    ellipse(c, 253, 421, 123, 100, '#506437', alpha=0.6)
    robot(c, 252, 526, 0.88, t, 'scan')
    rack(c, 1010, 525, 0.70, t)
    rack(c, 1095, 523, 0.57, t + 0.2)
    if t > 5.8:
        p = reveal(t, 5.8, 0.55)
        for x, y in [(945, 346), (1149, 353), (1091, 301)]:
            spark(c, x, y, (11 + 2 * math.sin(t * 3 + x)) * p, '#93d22a')
    text(c, 252, 554, 'AICR Operator', 22, WHITE, 8, 'center')
    text(c, 252, 585, 'Coordinates the rollout', 17, '#b9c5d0', 5, 'center')
    text(c, 1048, 554, 'Argo CD', 22, WHITE, 8, 'center')
    text(c, 1048, 585, 'Applies the changes', 17, '#b9c5d0', 5, 'center')


def ending(c, t):
    bg(c, '#0b0d10', '#1b2026', True)
    f = reveal(t, 0.02, 0.74)
    text(c, 72, 110 + 13 * (1 - f), 'NVIDIA AI CLUSTER RUNTIME', 18, LIME, 'mono', alpha=f)
    text(c, 59, 155 + 23 * (1 - f), 'AICR', 150, WHITE, 8, alpha=f)
    a = reveal(t, 0.42, 0.65)
    c.save()
    c.translate(0, 16 * (1 - a))
    rr(c, 73, 337, 276, 70, 35, LIME, alpha=a)
    text(c, 111, 353, 'aicr.run', 33, INK, 8, alpha=a)
    line(c, [(290, 372), (310, 372)], INK, 3, a)
    arrow(c, 310, 372, INK, 7)
    c.restore()
    b = reveal(t, 1.2, 0.70)
    text(c, 75, 434 + 9 * (1 - b), 'Explore recipes and contribute', 23, '#b9c5d0', 5, alpha=b)
    ellipse(c, 946, 347, 248, 228, '#354c1f', alpha=0.66)
    ellipse(c, 946, 347, 215, 198, None, '#89ab58', 1.2, 0.77)
    ellipse(c, 946, 347, 181, 168, None, '#89ab58', 1.3, 0.43)
    ellipse(c, 946, 347, 146, 133, None, '#89ab58', 1.2, 0.28)
    for i in range(6):
        p = i * TAU / 6 + t * 0.095
        ellipse(c, 946 + 215 * math.cos(p), 347 + 198 * math.sin(p), 4, 4, LIME, alpha=0.8)
    soft_circuit(c, t, '#a1bb80', 678, 252, 518, 286, 0.18)
    ellipse(c, 946, 545, 198, 35, '#0b0d10', alpha=0.57)
    ellipse(c, 946, 533, 191, 29, '#395126', '#759c44', 2)
    robot(c, 950, 542, 1.35, t, 'wave')
    chip(c, 756, 265 + 10 * math.sin(t * 2.1), 0.58, '#9fb3cf', -0.24, True)
    chip(c, 1139, 256 + 9 * math.sin(t * 2.1 + 0.8), 0.50, YELLOW, 0.26)
    for i, (x, y, s) in enumerate([(858, 195, 22), (1081, 201, 18), (1162, 390, 17), (735, 402, 20)]):
        spark(c, x, y, s * (0.83 + 0.17 * math.cos(t * 2 + i)), LIME, 0.14 * i)


OVERVIEW_SCENES = (intro, meet, recipe, bundle, validate, rollouts, ending)


def wrap_caption(value, limit=1000):
    words = value.split()
    lines = []
    while words:
        count = len(words)
        while count > 1 and font(26, 2).getlength(' '.join(words[:count])) / SCALE > limit:
            count -= 1
        lines.append(' '.join(words[:count]))
        words = words[count:]
    if len(lines) == 2 and len(lines[1]) < 33:
        options = [(' '.join(value.split()[:i]), ' '.join(value.split()[i:])) for i in range(1, len(value.split()))]
        lines = min(options, key=lambda pair: abs(font(26, 2).getlength(pair[0]) - font(26, 2).getlength(pair[1])))
    return lines


def caption(c, t, cues):
    for start, end, lines in cues:
        if start <= t < end:
            w = max(font(26, 2).getlength(line) / SCALE for line in lines) + 48
            height = len(lines) * 31 + 19
            y = 690 - height
            rr(c, 640 - w / 2, y, w, height, 12, '#0b0d10', alpha=0.94)
            for i, value in enumerate(lines):
                text(c, 640, y + 13 + i * 31, value, 26, '#f5f7fa', 2, 'center')
            break
