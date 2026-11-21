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
"""Render the checked-in AICR video episodes on a local machine."""

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import wave

VIDEOS = Path(__file__).resolve().parent
FPS = 30
SECONDS = 60
SAMPLE_RATE = 24000
ENDING = "Explore AICR at aicr.run."
SLUG = re.compile(r"[a-z0-9]+(?:-[a-z0-9]+)*")
MIX = (
    "[1:a]highpass=f=70,equalizer=f=2450:t=o:w=1.7:g=1.25,"
    "acompressor=threshold=.11:ratio=2.1:attack=12:release=120:makeup=1.2,"
    "loudnorm=I=-16:TP=-2.5:LRA=6,aresample=48000,pan=stereo|c0=c0|c1=c0[voice];"
    "[2:a]volume=.19[bed];[voice][bed]amix=inputs=2:duration=longest:normalize=0,"
    "alimiter=limit=.88:level=0:latency=1[audio]"
)


def catalog() -> dict:
    """Load and check the editable episode specifications."""
    episodes = {}
    names = set()
    for file in sorted((VIDEOS / "episodes").glob("*.json")):
        spec = json.loads(file.read_text(encoding="utf-8"))
        episode, slug, edges = spec["id"], spec["slug"], spec["edges"]
        if not isinstance(episode, int) or not 1 <= episode <= 99 or not SLUG.fullmatch(slug):
            raise ValueError(f"{file.name}: invalid episode ID or slug")
        if edges[0] != 0 or edges[-1] not in (53, SECONDS) or any(a >= b for a, b in zip(edges, edges[1:])):
            raise ValueError(f"{file.name}: scene boundaries must increase from 0 to 53 or 60")
        if len(spec["scenes"]) + 1 != len(edges) or any(not scene["voice"] for scene in spec["scenes"]):
            raise ValueError(f"{file.name}: one narration list is required per scene")
        name = f"{episode:02d}-{slug}.mp4"
        if name in names:
            raise ValueError(f"duplicate video filename {name}")
        names.add(name)
        episodes[file.stem] = (name, spec)
    if not episodes:
        raise ValueError("the episode directory is empty")
    return episodes


def phonemes(engine, line: str) -> str:
    """Use the series dictionary while leaving written captions alone."""
    from sound import voice

    text = line.replace("H100", "H one hundred").replace("DynamoGraphDeployment", "Dynamo Graph Deployment")
    result = voice.phonemes(engine, text)
    if re.search(r"\b(?:It|it|recipe|snapshot|result|AICR|bundle|record|output) records\b", line):
        result = result.replace("ɹˈɛkɔːdz", "ɹɪkˈɔːdz")
    return result


def fit_takes(engine, texts: list[str], budget: float) -> list:
    """Fit complete spoken takes into a time slot while retaining their pitch."""
    from sound import voice

    ipa = [phonemes(engine, text) for text in texts]
    takes = [voice.speak(engine, p, .94) for p in ipa]
    duration = sum(len(take) for take in takes) / SAMPLE_RATE
    if duration > budget:
        speed = min(1.10, .95 * duration / budget)
        takes = [voice.speak(engine, p, speed) for p in ipa]
        duration = sum(len(take) for take in takes) / SAMPLE_RATE
    if duration > budget:
        ratio = duration / budget * 1.02
        if ratio > 1.22:
            raise ValueError("narration is too long for its approved timing. Shorten the line or revise the cue")
        takes = [voice.speed_up(take, ratio) for take in takes]
    return takes


def recorded_cues(engine, spec: dict, edges: list, pcm) -> list[dict]:
    """Synthesize speech into the stored cue windows of a bespoke cut."""
    captions = spec["captions"]
    expected = [line for scene in spec["scenes"] for line in scene["voice"]]
    if spec["edges"][-1] == 53:
        expected.append(ENDING)
    if expected != [cue["text"] for cue in captions]:
        raise ValueError("stored captions must match the narration. Update both when editing a bespoke cut")
    last_end = 0
    for cue in captions:
        start, end = cue["start"], cue["end"]
        if start < last_end or not any(lo <= start < end <= hi for lo, hi in zip(edges, edges[1:])):
            raise ValueError("caption cue overlaps or crosses a scene boundary")
        last_end = end
        lead = .045
        budget = end - start - lead - .045
        if budget <= 0:
            raise ValueError("caption window is too short")
        take, = fit_takes(engine, [cue["text"]], budget)
        pos = round((start + lead) * SAMPLE_RATE)
        stop = pos + len(take)
        if stop > round(end * SAMPLE_RATE):
            raise ValueError(f"speech exceeds the caption window at {start:.2f}s")
        pcm[pos:stop] = take
    return captions


def narration(engine, spec: dict) -> tuple:
    """Produce a 60-second mono track, scene boundaries, and synchronized captions."""
    import numpy as np
    from sound import voice

    edges = list(spec["edges"])
    scenes = [scene["voice"] for scene in spec["scenes"]]
    if edges[-1] < SECONDS:
        edges.append(SECONDS)
        scenes.append([ENDING])
    pcm = np.zeros(SECONDS * SAMPLE_RATE, dtype="<f4")
    if spec.get("captions"):
        return pcm, edges, recorded_cues(engine, spec, edges, pcm)

    cues = []
    for start, end, lines in zip(edges, edges[1:], scenes):
        lead, gap, tail = (.32, .34, .55) if start != 53 else (1.23, .34, .50)
        budget = end - start - lead - tail - (len(lines) - 1) * gap
        if budget <= 0:
            raise ValueError(f"scene starting at {start}s has no narration time")
        takes = fit_takes(engine, lines, budget)
        free = end - start - lead - tail - sum(len(take) for take in takes) / SAMPLE_RATE
        gap = min(.65, max(gap, free / max(1, len(takes) - 1)))
        pos = round((start + lead) * SAMPLE_RATE)
        for line, take in zip(lines, takes):
            stop = pos + len(take)
            if stop > round((end - .17) * SAMPLE_RATE):
                raise ValueError(f"speech exceeds the scene ending at {end}s")
            pcm[pos:stop] = take
            cues.append({
                "start": round(pos / SAMPLE_RATE - .05, 3),
                "end": round(stop / SAMPLE_RATE + .12, 3),
                "text": voice.line_for_caption(line),
            })
            pos = stop + round(gap * SAMPLE_RATE)
    return pcm, edges, cues


def verify(path: Path) -> None:
    """Require a decodable 60-second HD video and stereo audio."""
    probe = subprocess.run(
        ["ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", str(path)],
        capture_output=True, check=True, timeout=15,
    )
    data = json.loads(probe.stdout)
    video, audio = data["streams"]
    measured = video["width"], video["height"], int(video["nb_frames"]), audio["channels"]
    if measured != (1920, 1080, SECONDS * FPS, 2) or float(data["format"]["duration"]) != SECONDS:
        raise ValueError(f"unexpected video output: {measured}, {data['format']['duration']} seconds")
    subprocess.run(
        ["ffmpeg", "-v", "error", "-nostdin", "-xerror", "-i", str(path), "-f", "null", "-"],
        check=True, timeout=60,
    )


def render(engine, spec: dict, music: Path, destination: Path) -> None:
    """Rasterize and encode the episode, then install the checked output."""
    import cairo
    from artwork import SCALE, draw, wrap_caption

    pcm, edges, captions = narration(engine, spec)
    cues = [(cue["start"], cue["end"], wrap_caption(cue["text"])) for cue in captions]
    if any(len(lines) > 2 for _, _, lines in cues):
        raise ValueError("a caption exceeds two lines. Divide the narration into shorter phrases")
    frame = cairo.ImageSurface(cairo.FORMAT_RGB24, 1920, 1080)
    ctx = cairo.Context(frame)
    ctx.scale(SCALE, SCALE)
    ctx.set_line_join(cairo.LINE_JOIN_ROUND)
    ctx.set_line_cap(cairo.LINE_CAP_ROUND)
    with tempfile.TemporaryDirectory(prefix=".aicr-video-", dir=destination.parent) as work:
        work = Path(work)
        raw, staged = work / "voice.f32", work / "episode.mp4"
        pcm.tofile(raw)
        command = [
            "ffmpeg", "-nostdin", "-v", "error", "-filter_threads", "1", "-filter_complex_threads", "1",
            "-f", "rawvideo", "-pixel_format", "bgr0", "-video_size", "1920x1080",
            "-framerate", str(FPS), "-i", "pipe:0", "-f", "f32le", "-ar", str(SAMPLE_RATE),
            "-ac", "1", "-i", str(raw), "-i", str(music), "-filter_complex", MIX, "-map", "0:v:0", "-map", "[audio]",
            "-c:v", "libx264", "-threads", "2", "-preset", "superfast", "-crf", "19", "-pix_fmt", "yuv420p",
            "-vf", "scale=out_color_matrix=bt709", "-colorspace", "bt709", "-color_trc", "bt709",
            "-color_primaries", "bt709", "-c:a", "aac", "-ar", "48000", "-b:a", "160k",
            "-metadata:s:a:0", "language=eng", "-metadata", f"title={spec['title']} | AICR",
            "-movflags", "+faststart", "-t", str(SECONDS), str(staged),
        ]
        with subprocess.Popen(command, stdin=subprocess.PIPE) as child:
            watchdog = threading.Timer(300, child.kill)
            watchdog.start()
            try:
                pixels = frame.get_data()
                for index in range(SECONDS * FPS):
                    draw(ctx, spec, edges, cues, index / FPS)
                    frame.flush()
                    child.stdin.write(pixels)
                child.stdin.close()
                result = child.wait(timeout=30)
            except BaseException:
                child.kill()
                raise
            finally:
                watchdog.cancel()
            if result:
                raise RuntimeError(f"FFmpeg exited with status {result}")
        verify(staged)
        os.link(staged, destination)


def main() -> None:
    """Render selected episodes or list their stable source keys."""
    cli = argparse.ArgumentParser(description=__doc__)
    select = cli.add_mutually_exclusive_group(required=True)
    select.add_argument("--list", action="store_true", help="list all available cuts")
    select.add_argument("--episode", action="append", metavar="KEY", help="render this episode, repeat as needed")
    select.add_argument("--all", action="store_true", help="render all episodes including reference cuts")
    cli.add_argument("--assets", type=Path, default=Path.home() / ".cache" / "aicr-videos")
    cli.add_argument("--output", type=Path, help="directory for generated MP4s")
    args = cli.parse_args()
    try:
        episodes = catalog()
        if args.list:
            for key, (filename, spec) in episodes.items():
                print(f"{key}\t{spec['title']}\t{filename}")
            return
        if args.output is None:
            cli.error("--output is required when rendering")
        keys = list(episodes) if args.all else list(dict.fromkeys(args.episode))
        missing = [key for key in keys if key not in episodes]
        if missing:
            cli.error(f"unknown episode keys: {', '.join(missing)}")
        if not shutil.which("ffmpeg") or not shutil.which("ffprobe"):
            raise ValueError("install FFmpeg and ffprobe before rendering")
        output = args.output.resolve()
        output.mkdir(parents=True, exist_ok=True)
        targets = [output / episodes[key][0] for key in keys]
        for target in targets:
            if target.exists():
                raise FileExistsError(f"{target} already exists. Choose a fresh output directory")
        from sound import voice
        from sound.prepare_assets import verified_music

        assets = args.assets.resolve()
        music = verified_music(assets)
        engine = voice.engine(assets)
        for key, target in zip(keys, targets):
            print(f"Rendering {target.name}…", flush=True)
            render(engine, episodes[key][1], music, target)
            print(f"{target} · verified 60 seconds, 1080p, stereo", flush=True)
    except (EOFError, KeyError, ModuleNotFoundError, OSError, RuntimeError, ValueError,
            subprocess.SubprocessError, wave.Error) as err:
        cli.exit(1, f"{cli.prog}: {err}\n")


if __name__ == "__main__":
    main()
