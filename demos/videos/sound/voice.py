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
"""Synthesize and normalize the series' British English narration."""

from pathlib import Path
import re
import subprocess

from kokoro_onnx import Kokoro
import numpy as np
from numpy.typing import NDArray
import onnxruntime as ort

from .prepare_assets import verified_models

RATE = 24_000
LANGUAGE = "en-gb"
VOICE = "bf_emma"

PRONUNCIATIONS = (
    (re.compile(r"\baicr\.run\b", re.IGNORECASE), "acre dot run"),
    (re.compile(r"\baicr\b", re.IGNORECASE), "acre"),
    (re.compile(r"\bNIMService\b"), "nim Service"),
    (re.compile(r"\bNIM\b"), "nim"),
    (re.compile(r"\bkubectl\b"), "cube C T L"),
    (re.compile(r"\bArgoCD\b"), "Argo CD"),
    (re.compile(r"\bAIPerf\b"), "AI perf"),
    (re.compile(r"\baicrd\b"), "acre dee"),
    (re.compile(r"\bkubeconfig\b", re.IGNORECASE), "cube config"),
    (re.compile(r"\bNCCL\b"), "nickel"),
    (re.compile(r"\bKWOK\b"), "kwock"),
    (re.compile(r"\bEKS\b"), "E K S"),
)


def engine(models: Path) -> Kokoro:
    """Initialize Emma's CPU voice from a verified local model and voice bank."""
    model, voices = verified_models(models)
    ort.disable_telemetry_events()
    options = ort.SessionOptions()
    options.intra_op_num_threads = 2
    options.inter_op_num_threads = 1
    options.log_severity_level = 3
    session = ort.InferenceSession(str(model), sess_options=options, providers=["CPUExecutionProvider"])
    return Kokoro.from_session(session, str(voices))


def phonemes(voice: Kokoro, line: str) -> str:
    """Apply the pronunciation dictionary without altering captions."""
    spoken = line.replace("’", "'")
    for pattern, pronunciation in PRONUNCIATIONS:
        spoken = pattern.sub(pronunciation, spoken)
    ipa = voice.tokenizer.phonemize(spoken, lang=LANGUAGE)
    ipa = (ipa.replace("ˈɛnvˈɪdiə", "ˌɛnvˈɪdiə")
           .replace("pɹˈuːvnəns", "pɹˈɒvənəns")
           .replace("daɪdʒˈɛst", "dˈaɪdʒɛst"))
    if "Kubernetes" in line:
        fallback = voice.tokenizer.phonemize("Kubernetes", lang=LANGUAGE)
        ipa = ipa.replace(fallback, "kˌuːbəˈnɛtiːz")
    return ipa


def speak(voice: Kokoro, ipa: str, speed: float) -> NDArray[np.float32]:
    """Return a trimmed, click-free mono take as 24 kHz floating-point PCM."""
    samples, rate = voice.create(ipa, voice=VOICE, speed=speed, lang=LANGUAGE, is_phonemes=True)
    if rate != RATE or samples.ndim != 1 or not np.all(np.isfinite(samples)):
        raise ValueError("Invalid speech samples")
    window = RATE // 100
    blocks = np.pad(samples, (0, (-len(samples)) % window)).reshape(-1, window)
    activity = np.flatnonzero(np.sqrt(np.mean(blocks ** 2, axis=1)) > 0.0015)
    if not len(activity):
        raise ValueError("Empty speech")
    first = max(0, (activity[0] - 2) * window)
    last = min(len(samples), (activity[-1] + 5) * window)
    take = samples[first:last].copy()
    fade = np.linspace(0, 1, min(RATE // 140, len(take) // 2))
    if len(fade):
        take[:len(fade)] *= fade
        take[-len(fade):] *= fade[::-1]
    return take


def speed_up(take: NDArray[np.float32], speed: float) -> NDArray[np.float32]:
    """Fit an existing take to a shorter shot while retaining its pitch."""
    result = subprocess.run(
        ["ffmpeg", "-v", "error", "-f", "f32le", "-ar", str(RATE), "-ac", "1",
         "-i", "pipe:0", "-af", f"atempo={speed:.6f}", "-f", "f32le", "pipe:1"],
        input=take.astype("<f4").tobytes(), capture_output=True, check=True, timeout=10,
    )
    samples = np.frombuffer(result.stdout, dtype="<f4")
    if not len(samples) or not np.all(np.isfinite(samples)):
        raise ValueError("Invalid retimed speech samples")
    return samples


def line_for_caption(line: str) -> str:
    """Restore model names written phonetically in narration scripts."""
    line = re.sub(r"\bH one hundred\b", "H100", line)
    return re.sub(r"\bG B two hundred\b", "GB200", line)
