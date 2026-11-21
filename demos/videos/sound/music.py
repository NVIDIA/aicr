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
"""Render the series' original 60-second instrumental score to stereo PCM."""

from array import array
import math
from pathlib import Path
import random
import sys
import wave

SAMPLE_RATE = 48_000
DURATION = 60
CHANNELS = 2
BEAT = 0.5625
BAR = 4 * BEAT
SCORE_SEED = 2606

CHORDS = {
    "D": (38, [57, 62, 66, 69]),
    "A": (45, [57, 61, 64, 71]),
    "Bm": (47, [59, 62, 66, 69]),
    "G": (43, [55, 59, 62, 69]),
    "Em": (40, [55, 59, 62, 66]),
    "A7": (45, [55, 57, 61, 64]),
}
PROGRESSION = (
    "D", "A", "Bm", "G", "D", "G", "A",
    "D", "Bm", "G", "A", "Em", "G", "D", "A",
    "Bm", "G", "D", "A", "G", "Em", "D", "A", "G", "A7",
)


class Score:
    """Synthesize and master the score's individual notes and percussion."""

    def __init__(self) -> None:
        """Allocate silent tracks and seed each performance identically."""
        self.tracks = [array("f", [0]) * (SAMPLE_RATE * DURATION) for _ in range(CHANNELS)]
        self.noise = random.Random(SCORE_SEED)

    def note(self, when: float, pitch: int, duration: float, volume: float, pan: float, voice: str) -> None:
        """Mix one panned note into the score."""
        start = round(when * SAMPLE_RATE)
        count = min(round(duration * SAMPLE_RATE), len(self.tracks[0]) - start)
        hz = 440 * 2 ** ((pitch - 69) / 12)
        left, right = self.tracks
        gain_l = volume * math.sqrt(0.5 - 0.45 * pan)
        gain_r = volume * math.sqrt(0.5 + 0.45 * pan)
        angular_hz = math.tau * hz
        for i in range(count):
            t = i / SAMPLE_RATE
            phase = angular_hz * t
            tail = min(1.0, (duration - t) / 0.035)
            if voice == "mallet":
                envelope = min(1.0, t / 0.009) * math.exp(-t / 0.17) * tail
                signal = (math.sin(phase) +
                          0.22 * math.sin(phase * 2.006) * math.exp(-t / 0.10) +
                          0.12 * math.sin(phase * 3.96) * math.exp(-t / 0.028))
            elif voice == "felt":
                envelope = min(1.0, t / 0.019) * math.exp(-t / 0.45) * tail
                signal = (math.sin(phase + 0.13 * math.sin(phase)) +
                          0.33 * math.sin(phase * 2) * math.exp(-t / 0.16))
            elif voice == "bass":
                envelope = min(1, t / 0.019, (duration - t) / 0.11) * math.exp(-t / 0.37)
                signal = math.sin(phase) + 0.14 * math.sin(2 * phase)
            else:
                envelope = min(1, t / 0.32, (duration - t) / 0.75)
                signal = (0.8 * math.sin(phase) + 0.12 * math.sin(phase * 1.003)
                          + 0.08 * math.sin(2 * phase))
            value = signal * envelope
            left[start + i] += value * gain_l
            right[start + i] += value * gain_r

    def tap(self, when: float, kind: str, volume: float, pan: float = 0.0) -> None:
        """Mix one panned kick, tick, or snap."""
        start = round(when * SAMPLE_RATE)
        duration = 0.23 if kind == "kick" else 0.10
        count = min(round(duration * SAMPLE_RATE), len(self.tracks[0]) - start)
        last = 0.0
        for i in range(count):
            t = i / SAMPLE_RATE
            if kind == "kick":
                phase = math.tau * (51 * t + 42 * 0.026 * (1 - math.exp(-t / 0.026)))
                value = math.sin(phase) * min(1, t / 0.004) * math.exp(-t / 0.044)
            else:
                noise = self.noise.uniform(-1, 1)
                decay = 0.016 if kind == "tick" else 0.024
                high_noise = noise - 0.82 * last
                last = noise
                tone = 0.22 * math.sin(math.tau * 1915 * t) + 0.16 * math.sin(math.tau * 2816 * t)
                value = (0.48 * high_noise + tone) * min(1, t / 0.003) * math.exp(-t / decay)
            self.tracks[0][start + i] += volume * value * (1 - 0.45 * pan)
            self.tracks[1][start + i] += volume * value * (1 + 0.45 * pan)

    def write(self, destination: Path) -> None:
        """Apply ambience, headroom and fades, then write portable stereo PCM."""
        left, right = self.tracks
        delay_a, delay_b = int(0.068 * SAMPLE_RATE), int(0.105 * SAMPLE_RATE)
        for i in range(len(left) - 1, delay_b, -1):
            left[i] += 0.092 * right[i - delay_a]
            right[i] += 0.095 * left[i - delay_b]
        peak = max(max(map(abs, channel)) for channel in self.tracks)
        gain = 0.49 / peak
        out = array("h", [0]) * (DURATION * SAMPLE_RATE * CHANNELS)
        for i, (l_channel, r_channel) in enumerate(zip(left, right)):
            moment = i / SAMPLE_RATE
            fade = min(1, moment / 0.24, (DURATION - moment) / 1.31)
            out[2 * i] = int(32766 * gain * fade * l_channel)
            out[2 * i + 1] = int(32766 * gain * fade * r_channel)
        if sys.byteorder != "little":
            out.byteswap()
        with wave.open(str(destination), "wb") as output:
            output.setparams((CHANNELS, 2, SAMPLE_RATE, DURATION * SAMPLE_RATE, "NONE", "not compressed"))
            output.writeframes(out.tobytes())


def write_music(destination: Path) -> None:
    """Synthesize the original accompaniment independently of scene edits."""
    score = Score()
    for index, name in enumerate(PROGRESSION):
        root, tones = CHORDS[name]
        base = index * BAR
        for j, key in enumerate(tones):
            score.note(base, key - 12, BAR + 0.43, 0.014, (j - 1.5) * 0.32, "pad")
        score.note(base, root, 0.50, 0.125, 0, "bass")
        score.note(base + 2 * BEAT, root + 12, 0.26, 0.064, 0, "bass")
        if index > 0:
            score.tap(base, "kick", 0.065)
            score.tap(base + 2 * BEAT, "kick", 0.044)
        if index not in (0, 1, 24):
            score.tap(base + BEAT, "snap", 0.026, -0.28)
            score.tap(base + 3 * BEAT, "snap", 0.023, 0.25)
            for count in (0.52, 1.52, 2.52, 3.52):
                score.tap(base + count * BEAT, "tick", 0.025 * (0.80 + 0.20 * score.noise.random()), 0.4)

        if index in (0, 1, 7, 11, 19, 23):
            pattern = [(0, 1), (0.76, 2), (1.5, 3), (2.5, 2), (3.24, 1)]
        elif index % 2:
            pattern = [(0.51, 0), (1.50, 2), (2.74, 1)]
        else:
            pattern = [(0.52, 1), (1.24, 2), (2.53, 3), (3.48, 2)]
        for j, (offset, degree) in enumerate(pattern):
            gain = (0.10 if index < 2 else 0.078) * (1 - j * 0.065)
            pan = [-0.30, 0.11, 0.29, -0.05][degree]
            score.note(base + offset * BEAT, tones[degree] + 12, 0.58, gain, pan, "mallet")
            score.note(base + offset * BEAT + 0.18, tones[degree] + 12, 0.34, gain * 0.09, -pan, "mallet")
        if index not in (2, 6, 10, 14, 18, 23, 24):
            for j, key in enumerate(tones[:3]):
                score.note(base + (1.06 + 0.027 * j) * BEAT, key, 0.71, 0.035, (j - 1) * 0.45, "felt")
                score.note(base + (3.03 + 0.028 * j) * BEAT, key, 0.61, 0.029, (1 - j) * 0.45, "felt")

    phrases = ([66, 69, 74], [64, 69, 73], [67, 71, 74])
    for accent in (7, 16, 25, 34, 43, 53):
        scale = phrases[(accent // 9) % len(phrases)]
        for step, (pitch, pan) in enumerate(zip(scale, (-0.36, 0, 0.36))):
            score.note(accent - 0.25 + step * 0.11, pitch + 12, 0.57, 0.032, pan, "mallet")
        score.tap(accent, "tick", 0.030, 0.04)

    for onset, pitch in [(56.25, 62), (56.55, 66), (56.84, 69), (57.56, 74)]:
        score.note(onset, pitch + 12, 0.87, 0.096, 0.15, "mallet")
        score.note(onset + 0.21, pitch + 12, 0.60, 0.022, -0.37, "mallet")
    for j, key in enumerate([50, 57, 62, 66, 69]):
        score.note(56.25 + j * 0.042, key, 2.95, 0.067, (j - 2) * 0.27, "felt")
        score.note(56.29, key, 3.65, 0.011, (j - 2) * 0.27, "pad")
    score.note(56.25, 38, 1.2, 0.139, 0, "bass")
    score.tap(56.25, "kick", 0.060)
    score.write(destination)
