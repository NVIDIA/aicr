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
"""Cairo character art and gesture loops for the AICR cartoon."""
from math import cos, pi, sin

INK = (0.0431, 0.051, 0.0627)
CREAM = (0.902, 0.9294, 0.9529)
WHITE = (0.9647, 0.9725, 0.9804)
LIME = (0.5765, 0.8235, 0.1647)
LIGHT = (0.7882, 0.9137, 0.5569)
SHADE = (0.4627, 0.7255, 0.0)
GOLD = (0.6667, 0.7451, 0.5176)
CORAL = (0.9216, 0.5843, 0.4863)
MINT = (0.6235, 0.702, 0.8118)
TAU = 2 * pi


def _path(ctx, points):
    ctx.new_path()
    for point in points:
        if len(point) == 2:
            if ctx.has_current_point():
                ctx.line_to(*point)
            else:
                ctx.move_to(*point)
        else:
            ctx.curve_to(*point)


def _fill(ctx, color):
    ctx.set_source_rgb(*color)
    ctx.fill()


def _stroke(ctx, color=INK, width=4):
    ctx.set_source_rgb(*color)
    ctx.set_line_width(width)
    ctx.stroke()


def _shape(ctx, points, fill, width=4, outline=INK):
    _path(ctx, points)
    ctx.close_path()
    if width:
        ctx.set_source_rgb(*fill)
        ctx.fill_preserve()
        _stroke(ctx, outline, width)
    else:
        _fill(ctx, fill)


def _line(ctx, points, color=INK, width=4):
    _path(ctx, points)
    _stroke(ctx, color, width)


def _rounded(ctx, x, y, w, h, r, fill, width=4, outline=INK):
    ctx.new_path()
    ctx.arc(x + w - r, y + r, r, -pi / 2, 0)
    ctx.arc(x + w - r, y + h - r, r, 0, pi / 2)
    ctx.arc(x + r, y + h - r, r, pi / 2, pi)
    ctx.arc(x + r, y + r, r, pi, 3 * pi / 2)
    ctx.close_path()
    if width:
        ctx.set_source_rgb(*fill)
        ctx.fill_preserve()
        _stroke(ctx, outline, width)
    else:
        _fill(ctx, fill)


def _ellipse(ctx, x, y, rx, ry, color, width=0, outline=INK, alpha=1):
    ctx.new_path()
    ctx.save()
    ctx.translate(x, y)
    ctx.scale(rx, ry)
    ctx.arc(0, 0, 1, 0, TAU)
    ctx.restore()
    ctx.set_source_rgba(*color, alpha)
    if width:
        ctx.fill_preserve()
        _stroke(ctx, outline, width)
    else:
        ctx.fill()


def _shoe(ctx, foot_x, lift, direction, tilt):
    ctx.save()
    ctx.translate(foot_x, -lift)
    ctx.scale(direction, 1)
    ctx.rotate(tilt)
    _shape(
        ctx,
        [
            (-12, -35),
            (3, -35, 10, -30, 11, -22),
            (14, -19, 25, -21, 29, -12),
            (30, -9, 30, -3, 26, -2),
            (13, 1, -7, 0, -17, -2),
            (-23, -6, -22, -15, -19, -20),
            (-18, -24, -16, -33, -12, -35),
        ],
        CREAM,
        4.5,
    )
    _line(ctx, [(-19, -9), (-3, -6, 17, -7, 28, -9)], INK, 3)
    _line(ctx, [(5, -22), (10, -18), (17, -21)], INK, 2.5)
    _line(ctx, [(-15, -10), (-15, -16, -14, -21, -11, -24)], WHITE, 2.5)
    ctx.restore()


def _hand(ctx, x, y, side, style, tilt):
    ctx.save()
    ctx.translate(x, y)
    ctx.scale(side, 1)
    ctx.rotate(tilt)
    if style == "point":
        _shape(
            ctx,
            [
                (-16, 6),
                (-20, 0, -17, -11, -9, -13),
                (-6, -18, 0, -18, 4, -14),
                (15, -18, 28, -22, 30, -16),
                (31, -13, 25, -10, 15, -7),
                (19, -6, 21, -2, 17, 1),
                (21, 4, 19, 9, 13, 10),
                (11, 14, 1, 16, -4, 13),
                (-8, 14, -13, 12, -16, 6),
            ],
            CREAM,
        )
        _line(ctx, [(2, -9), (7, -5, 6, 1, 1, 3)], INK, 2.5)
        _line(ctx, [(13, 0), (17, 1)], INK, 2.5)
        _line(ctx, [(12, 6), (16, 8)], INK, 2.5)
    elif style == "palm":
        _shape(
            ctx,
            [
                (-13, 14),
                (-15, 8, -16, 5, -18, 2),
                (-23, -5, -19, -11, -15, -9),
                (-11, -8, -9, -3, -8, -2),
                (-10, -12, -10, -23, -6, -24),
                (-2, -26, 0, -22, 0, -10),
                (0, -19, 1, -28, 6, -28),
                (11, -28, 10, -20, 10, -10),
                (13, -23, 19, -24, 20, -19),
                (21, -16, 18, -9, 17, -5),
                (22, -12, 26, -11, 27, -7),
                (28, -4, 22, 5, 20, 11),
                (18, 20, 8, 24, 0, 23),
                (-6, 22, -11, 19, -13, 14),
            ],
            CREAM,
        )
        _line(ctx, [(-5, 3), (0, 0, 8, 2, 10, 8)], INK, 2.5)
    else:
        _shape(
            ctx,
            [
                (-9, -12),
                (-4, -19, 9, -17, 16, -11),
                (22, -5, 21, 2, 16, 9),
                (12, 16, -2, 16, -10, 10),
                (-18, 4, -17, -6, -9, -12),
            ],
            CREAM,
        )
        _line(ctx, [(-9, -3), (-6, -7, 2, -7, 5, -2), (6, 2, 4, 5, 0, 6)], INK, 2.5)
        _line(ctx, [(13, -8), (16, -4, 17, 0, 17, 3)], GOLD, 2.5)
    ctx.restore()


def _arm(ctx, side, head_y, target, style, tilt):
    x, y = target
    root_x, root_y = side * 64, -120 + head_y
    control_x = (abs(x) + 12) * side
    points = [(root_x, root_y), (control_x, root_y + (y - root_y) * 0.30, control_x, y + 17, x, y)]
    _line(ctx, points, INK, 13)
    _line(ctx, points, SHADE, 6)
    _hand(ctx, x, y, side, style, tilt)


def _chip(ctx):
    _shape(
        ctx,
        [
            (54, -202),
            (76, -197, 90, -188, 90, -174),
            (90, -76),
            (90, -63, 82, -55, 68, -53),
            (-48, -53),
            (-68, -54, -81, -65, -81, -81),
            (-78, -187),
            (-74, -200),
        ],
        SHADE,
        5,
    )
    for py in (-165, -137, -109, -81):
        _rounded(ctx, -90, py - 5, 25, 10, 4, GOLD, 3.4)
        _rounded(ctx, 74, py + 2, 25, 10, 4, GOLD, 3.4)
    for px in (-43, -15, 13, 41):
        _rounded(ctx, px - 5, -218, 10, 23, 4, GOLD, 3.4)
        _rounded(ctx, px + 4, -66, 10, 20, 4, GOLD, 3.4)
    _rounded(ctx, -78, -203, 155, 145, 25, LIME, 5.5)
    _line(ctx, [(-65, -177), (-65, -184, -60, -192, -50, -192), (13, -192)], LIGHT, 5.5)
    _rounded(ctx, -58, -180, 115, 93, 21, CREAM, 4.3)
    _line(ctx, [(-47, -158), (-48, -165, -42, -171, -34, -171), (31, -171)], WHITE, 4.5)
    for px in (-34, -24, -14):
        _ellipse(ctx, px, -73, 2.5, 2.5, INK)
    _line(ctx, [(31, -73), (51, -73), (57, -79)], SHADE, 3)
    _ellipse(ctx, 30, -73, 3.3, 3.3, LIGHT, 2)


def _face(ctx, t, pose):
    glance = {"scan": 4, "carry": 2, "worried": -2, "wave": -1}.get(pose, 0)
    blink = abs((t + 0.6) % 4.35 - 3.2)
    eye_height = max(0.13, min(1, blink / 0.13))
    if pose == "cheer":
        for x in (-22, 22):
            _line(ctx, [(x - 9, -143), (x - 7, -151, x + 7, -151, x + 9, -143)], INK, 4.5)
    else:
        for x in (-22, 22):
            _ellipse(ctx, x + glance, -143, 6.3, 12.3 * eye_height, INK)
            if eye_height > 0.65:
                _ellipse(ctx, x + glance + 1.0, -147, 2.0, 2.5, WHITE)
    for x in (-38, 38):
        _ellipse(ctx, x, -126, 7.6, 4.0, CORAL, alpha=0.74)
    if pose == "worried":
        _line(ctx, [(-32, -158), (-25, -159), (-19, -163)], INK, 3)
        _line(ctx, [(17, -163), (22, -159), (30, -157)], INK, 3)
        _line(ctx, [(-11, -111), (-4, -120, 5, -119, 10, -114)], INK, 4)
    elif pose in ("wave", "cheer"):
        _shape(
            ctx,
            [
                (-14, -121),
                (-7, -118, 7, -119, 14, -122),
                (13, -111, 9, -104, 1, -104),
                (-7, -104, -12, -111, -14, -121),
            ],
            INK,
            2.4,
        )
        _ellipse(ctx, 1, -108, 6, 2.7, CORAL)
    else:
        _line(ctx, [(-11, -119), (-5, -110, 4, -110, 12, -120)], INK, 4)


def draw_robot(ctx, x, y, scale, t, pose="idle"):
    """Draw the GPU mascot centered on the supplied floor anchor."""
    ctx.save()
    ctx.translate(x, y)
    ctx.scale(scale, scale)
    ctx.set_line_cap(1)
    ctx.set_line_join(1)

    motion = sin(t * 3.2)
    gesture = sin(t * 7.4)
    tilt = 0.035 * sin(t * 2.6)
    bounce = 0
    if pose == "cheer":
        bounce = max(0, sin(t * 3.4)) ** 2 * 17
    body_y = -3 * motion - bounce
    if pose == "worried":
        tilt -= 0.07
    if pose == "scan":
        tilt += 0.07

    _ellipse(ctx, 1, 0, 77 - bounce * 0.5, 8, INK, alpha=0.18 - bounce * 0.003)

    footsteps = []
    for side in (-1, 1):
        foot_x = side * 43 + sin(t * 2.6) * 2
        lift = bounce * (0.92 if side < 0 else 1.05)
        foot_tilt = side * (0.017 * motion + 0.035 * bounce / 17)
        leg = [(side * 30, -70 + body_y), (side * 38, -55 + body_y, foot_x - side * 4, -49 - lift, foot_x, -25 - lift)]
        _line(ctx, leg, INK, 12)
        _line(ctx, leg, SHADE, 5.5)
        footsteps.append((foot_x, lift, side, foot_tilt))
    for footstep in footsteps:
        _shoe(ctx, *footstep)

    poses = {
        "idle": ((-94, -89 + motion * 4), (96, -91 - motion * 4), "fist", "fist", -0.18, 0.20),
        "worried": ((-94, -117 + gesture * 1.8), (94, -108 - gesture * 1.8), "palm", "palm", -0.62, 0.68),
        "scan": ((-88, -90 + motion * 3), (106, -146 + 4 * motion), "fist", "point", 0.08, -0.1),
        "carry": ((-88, -115 + motion * 2), (102, -116 + motion * 2), "fist", "palm", 0.06, 1.18),
        "wave": (
            (-96, -91 + motion * 3),
            (105 + 4 * gesture, -166 + motion * 3),
            "fist",
            "palm",
            0.03,
            0.38 + 0.30 * gesture,
        ),
        "cheer": ((-99, -193 - bounce + motion * 4), (105, -198 - bounce - motion * 4), "palm", "palm", -0.35, 0.27),
    }
    left, right, left_style, right_style, left_tilt, right_tilt = poses.get(pose, poses["idle"])
    _arm(ctx, -1, body_y, left, left_style, left_tilt)
    _arm(ctx, 1, body_y, right, right_style, right_tilt)

    ctx.translate(0, -122 + body_y)
    ctx.rotate(tilt)
    ctx.translate(0, 122)
    _chip(ctx)
    _face(ctx, t, pose)
    ctx.restore()
