# Rebuild the AICR video series locally

Open the AICR checkout in Codex. The [video index](README.md) links to the editable source for every episode. The renderer uses those checked-in sources, so replaying an existing cut works with any Codex model or directly from a terminal. The narrator is pinned independently to Kokoro v1.0 with the British English Emma voice (`bf_emma`).

## Set up and render

Use Python 3.12, Cairo with development headers and `pkg-config`, and FFmpeg with `ffprobe` and the `libx264` encoder. On macOS, install the dependencies with Homebrew (`brew install python@3.12 cairo pkg-config ffmpeg`). On Ubuntu 24.04, install `python3.12`, `python3.12-venv`, `python3.12-dev`, `build-essential`, `libcairo2-dev`, `pkg-config`, `ffmpeg`, and `fonts-dejavu`. The renderer uses Avenir Next and Menlo when available on macOS and DejaVu Sans and Mono on Linux.

From the repository root, set up the environment and media cache in a directory outside the checkout.

```sh
video_work="$HOME/aicr-video-work"
mkdir -p "$video_work"
python3.12 -m venv "$video_work/venv"
video_python="$video_work/venv/bin/python"
export PYTHONDONTWRITEBYTECODE=1

"$video_python" -m pip install -r videos/sound/requirements.txt
"$video_python" videos/sound/prepare_assets.py --output "$video_work/assets"
"$video_python" videos/render.py --list
"$video_python" videos/render.py --episode 01 --episode 42 \
  --assets "$video_work/assets" --output "$video_work/first-build"
```

Repeat `--episode` for individual source keys or use `--all` for the full catalog, including reference cuts. Use a fresh output directory when comparing revisions. Each successful render is a 60-second, 1920 × 1080 MP4 at 30 fps with synchronized captions burned into the frames and music under the narration. The renderer checks the media before saving it and preserves existing outputs.

The initial setup downloads the voice assets and verifies their SHA-256 digests. The music is generated from [source](sound/music.py). Subsequent renders use the local cache and can run offline. The checked-in script, shot definitions, timing, mascot, and pronunciation rules preserve the production choices. Fonts, operating systems, speech runtime, and FFmpeg builds can affect pixels, audio, or the final file hash.

## Update an episode

Edit the episode JSON for narration and scene timing. The batch cuts also define on-screen copy there and generate caption timing from the synthesized speech. The six bespoke cuts draw on-screen copy in [artwork](artwork/) and store caption windows in JSON. Update their narration and caption text together. Keep the GPU chip mascot and palette consistent across the series, and choose a visual sequence that shows what the specific feature does.

The [pronunciation dictionary](sound/voice.py) keeps the spoken and written forms independent. The narrator says AICR like “acre,” NIM like “nim,” and kubectl like “cube C-T-L.” Titles and captions retain the written names. After a narration edit, listen to the result and watch the full cut with captions. Check the visible command or result against the episode’s source and verify the end-card address stays visible through the final frame.

## Review a release

With the previous and new stable tags available in the local checkout, run the planner. Substitute the tags you want to compare.

```sh
"$video_python" videos/review_release.py v1.2.0 v1.3.0 > "$video_work/v1.3.0-review.json"
```

The report records both release commits and the changed paths behind each review flag. Read the release notes and review the changes that match each episode’s watched paths. Rebuild a cut when a command, result, prerequisite, or factual claim changes. Carry forward the approved cut when its story still holds. Review the unmapped feature changes for new episode ideas. For a new episode, add its source, one-line index entry, and watched paths in [review-paths.json](review-paths.json). Use stable episode IDs and cite the tag or commit that supports each example. Label preview and unreleased behavior in the episode and index.

## Prompts for Codex

**Review a release**

> Review the AICR videos from `<previous tag>` to `<new tag>`. Follow `videos/REBUILD.md` to compare the tags locally. Check flagged episodes against the new tag and read the release notes for new episode ideas. Use the episode prompt below for each cut whose command, prerequisite, result, or factual claim changed. Save revised cuts in `<output directory>` and summarize what changed, what still holds, and which new episode topics the release supports.

**Update one episode**

> Update AICR episode `<episode ID>` for release `<tag>`. Read `videos/README.md`, `videos/REBUILD.md`, the episode JSON, and its artwork if it uses a bespoke renderer. Use the release tag’s docs, code, and demos to verify the claims, commands, prerequisites, and results. If the approved cut still holds, report that and keep it. Otherwise, tell one concrete story about what the audience can do with this feature. Use the shared GPU chip mascot, palette, Emma narration, and pronunciation rules. Give this episode a visual sequence suited to its story. Keep the narration conversational, the captions synchronized, and the end-card address visible through the final frame. Update the source and one-line index entry as needed. Render a 60-second 1080p candidate to `<output directory>` using the local guide. Report the supporting release sources and the local output path.
