---
name: aicr-creating-slide-decks
description: Use when building a self-contained HTML slide deck or visual talking-point for a technical concept or workflow (e.g. a demos/*.html) — shown full-screen or projected and narrated, opening in any browser with no build step or dependencies.
---

# Creating Slide Decks

## Overview

A deck here is **one self-contained HTML file** — inline CSS and inline SVG, no build, no dependencies, no network — that opens in any browser and projects cleanly. One idea per slide; hand-drawn SVG diagrams carry the weight. It's the *talk-to-it* companion to a runnable demo script (see the `aicr-creating-guided-demos` skill).

Copy [`skeleton.html`](skeleton.html): it's already a working deck (palette + chrome + keyboard nav + a title slide + a content slide + an SVG diagram stub). Add one `<section class="slide">` per idea; the script auto-counts and navigates.

## When to use

- Building a visual talking-point (a `demos/*.html`) to present or teach a concept/workflow, full-screen or projected.
- A leave-behind a reader can click through on their own later.
- **Not for:** prose docs (use Markdown), anything needing a real web app/build, or live data dashboards.

## Conventions

- **One self-contained file.** Inline `<style>` + inline `<svg>`; **no external fonts/CSS/JS/CDN** — it must work offline and on a projector with no network. Lives in `demos/`.
- **Full-viewport slides + nav.** One `<section class="slide" data-title="…">` per idea; only `.active` shows. Keyboard (←/→/Space, Home/End), on-screen ‹ ›, a progress bar, a slide counter, **F** fullscreen, and `#n` deep-links — all from the skeleton's script.
- **One idea per slide.** An eyebrow `label` + a heading + **one** visual (a diagram, a code block, or a small table). If it overflows, split it.
- **Inline SVG carries the concept.** Hand-author with the node/edge classes (`node`/`node-g`, `edge`/`edge-g`, `nlabel`/`nsub`/`elabel`); one diagram per slide; a `viewBox` plus `max-height: …vh` so it scales. Keep labels legible from the back of a room.
- **Dark NVIDIA theme.** The `:root` palette (green `#76b900` accent, NVIDIA Sans with system fallback). Wordmark in text — **no fabricated NVIDIA logo**.

## Rules

- **Real, accurate content — no marketing fluff.** The deck explains the actual thing; don't overclaim. (Same honesty bar as the demo it accompanies.)
- **Self-contained or it doesn't ship.** A single external dependency breaks the offline/projector promise.
- **Commit-ready by default.** Generic content, no personal/machine specifics, no invented logos.

## Validate before you ship

Decks fail silently — a malformed `<svg>` just doesn't render. Before committing:

- **Parse every inline `<svg>` as XML.** Inline SVG is held to a stricter bar than the surrounding HTML: a bare `&`, an unclosed tag, or a stray attribute that a browser tolerates in HTML makes the *whole* `<svg>` fail to draw. Escape `&` as `&amp;`, `<`/`>` as `&lt;`/`&gt;`. Quick check:

  ```bash
  python3 - <<'PY'
  import re; from xml.dom import minidom
  html = open("demos/your-deck.html").read()
  for n, s in enumerate(re.findall(r'<svg\b.*?</svg>', html, re.DOTALL), 1):
      minidom.parseString(s); print(f"svg #{n}: well-formed")
  PY
  ```

- **Open it in a browser** and arrow through every slide; check each diagram/code block fits the viewport (cap `max-height`; slides scroll internally as a fallback). If you can't open a browser, at minimum confirm each diagram's `viewBox` height stays within its `max-height` cap.

## Worked example

`demos/evidence-demo-slides.html` — nine slides (problem → mechanics → diagrams → payoff → roadmap), four hand-built SVG diagrams, keyboard nav, dark theme.

## Common mistakes

- External fonts/CSS/JS → dies offline or on the projector. Inline everything.
- Wall of text per slide → one idea per slide; split instead.
- Fabricated logo or marketing fluff → text wordmark, real content.
- SVG taller than the viewport → set `max-height`; verify in a browser.
