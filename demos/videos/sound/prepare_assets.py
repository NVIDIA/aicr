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
"""Prepare the local score and pinned offline voice assets."""

import argparse
from dataclasses import dataclass
from hashlib import sha256
import os
from pathlib import Path
from tempfile import NamedTemporaryFile
from time import monotonic
from urllib.request import urlopen
import wave

if __package__:
    from .music import CHANNELS, DURATION, SAMPLE_RATE, write_music
else:
    from music import CHANNELS, DURATION, SAMPLE_RATE, write_music

MODEL_BASE = "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/"
BLOCK_BYTES = 256 * 1024
DOWNLOAD_TIMEOUT = 180
READ_TIMEOUT = 20


@dataclass(frozen=True)
class ModelAsset:
    """A release asset with verified length and content identity."""

    name: str
    size: int
    digest: str


MODEL = ModelAsset(
    "kokoro-v1.0.int8.onnx", 92_361_271,
    "6e742170d309016e5891a994e1ce1559c702a2ccd0075e67ef7157974f6406cb",
)
VOICES = ModelAsset(
    "voices-v1.0.bin", 28_214_398,
    "bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d",
)
MUSIC_NAME = "music.wav"


def verify_asset(path: Path, asset: ModelAsset) -> Path:
    """Reject a cache entry that differs from the pinned upstream bytes."""
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"Missing regular asset file {path}; run prepare_assets.py to populate the cache")
    with path.open("rb") as source:
        if os.fstat(source.fileno()).st_size != asset.size:
            raise ValueError(f"Unexpected size for {path}; prepare a fresh asset cache")
        digest = sha256()
        for block in iter(lambda: source.read(BLOCK_BYTES), b""):
            digest.update(block)
    if digest.hexdigest() != asset.digest:
        raise ValueError(f"SHA-256 mismatch for {path}; prepare a fresh asset cache")
    return path


def verified_models(cache: Path) -> tuple[Path, Path]:
    """Resolve the voice inputs from the cache without any network access."""
    return verify_asset(cache / MODEL.name, MODEL), verify_asset(cache / VOICES.name, VOICES)


def fetch_asset(cache: Path, asset: ModelAsset) -> None:
    """Download a missing release asset and publish it only after verification."""
    path = cache / asset.name
    if path.exists() or path.is_symlink():
        verify_asset(path, asset)
        return

    with NamedTemporaryFile(prefix=f".{asset.name}.", suffix=".part", dir=cache, delete=False) as target:
        staged = Path(target.name)
    try:
        deadline = monotonic() + DOWNLOAD_TIMEOUT
        size = 0
        with urlopen(MODEL_BASE + asset.name, timeout=READ_TIMEOUT) as response, staged.open("wb") as target:
            if not response.geturl().startswith("https://"):
                raise ValueError(f"Refusing redirected asset from a non-HTTPS source: {asset.name}")
            while True:
                if monotonic() > deadline:
                    raise TimeoutError(f"Download exceeded {DOWNLOAD_TIMEOUT} seconds: {asset.name}")
                block = response.read1(BLOCK_BYTES)
                if not block:
                    break
                size += len(block)
                if size > asset.size:
                    raise ValueError(f"Download exceeded pinned size for {asset.name}")
                target.write(block)
        verify_asset(staged, asset)
        staged.replace(path)
    finally:
        staged.unlink(missing_ok=True)


def verified_music(cache: Path) -> Path:
    """Check the score's PCM encoding and duration before it enters the mix."""
    path = cache / MUSIC_NAME
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"Missing regular score file {path}; run prepare_assets.py to populate the cache")
    validate_music(path)
    return path


def validate_music(path: Path) -> None:
    """Reject a score with unexpected PCM parameters or missing samples."""
    with wave.open(str(path), "rb") as score:
        expected = (CHANNELS, 2, SAMPLE_RATE, DURATION * SAMPLE_RATE, "NONE")
        if score.getparams()[:5] != expected:
            raise ValueError(f"Invalid score format for {path}; prepare a fresh asset cache")
        if len(score.readframes(expected[3])) != expected[3] * CHANNELS * expected[1]:
            raise ValueError(f"Truncated score {path}; prepare a fresh asset cache")


def prepare_assets(cache: Path) -> None:
    """Prepare a cache for offline rendering without replacing its existing files."""
    cache.mkdir(parents=True, exist_ok=True)
    for asset in (MODEL, VOICES):
        fetch_asset(cache, asset)
    path = cache / MUSIC_NAME
    if path.exists() or path.is_symlink():
        verified_music(cache)
        return
    with NamedTemporaryFile(prefix=".music.", suffix=".part", dir=cache, delete=False) as target:
        staged = Path(target.name)
    try:
        write_music(staged)
        validate_music(staged)
        staged.replace(path)
    finally:
        staged.unlink(missing_ok=True)


def main() -> None:
    """Prepare the cache selected by the caller."""
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("--output", type=Path, required=True, help="asset cache directory")
    args = cli.parse_args()
    cache = args.output.expanduser()
    try:
        prepare_assets(cache)
    except (EOFError, OSError, ValueError, wave.Error) as error:
        cli.exit(1, f"{error}\n")
    print(f"Audio assets ready in {cache.resolve()}")


if __name__ == "__main__":
    main()
