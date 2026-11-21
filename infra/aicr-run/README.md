# aicr.run Go module discovery

AICR's Go module is `aicr.run` (#3834). Go resolves that import path by
fetching `https://aicr.run/<path>?go-get=1` and reading a `go-import` meta tag,
which points the module root at this repository:

```html
<meta name="go-import" content="aicr.run git https://github.com/NVIDIA/aicr">
```

This folder is the source of truth for the Cloudflare Worker that serves that
tag. Change it here first, then deploy.

## What runs where

| Piece | Where | What it does |
|-------|-------|--------------|
| Worker `aicr-go-discovery` | `worker.js` here; route `https://aicr.run/*` in the dashboard | Answers `GET`/`HEAD` requests carrying `go-get=1` with the meta tag, on every path. Passes everything else through. |
| Redirect Rules | Cloudflare dashboard, `aicr.run` zone | Send browser `GET`/`HEAD` requests without `go-get=1` to the repository (301). They run before the Worker, so Go's requests are the only ones that reach it. |
| DNSSEC | Cloudflare dashboard, `aicr.run` zone | Enabled, with the DS record at the registrar. |

## Deploy

From this folder, with access to the Cloudflare account that owns the zone:

```bash
npx wrangler deploy
```

`wrangler.toml` deploys the code only; the route stays as configured in the
dashboard. Pasting `worker.js` into the dashboard editor works too. Either way,
keep the deployed code identical to this file.

## Verify

```bash
for p in / /pkg/client/v1 /cmd/aicr; do
  curl -fsS "https://aicr.run${p}?go-get=1" | grep go-import
done
curl -sI https://aicr.run/ | grep -i '^location'
```

Each `go-get` request must print exactly the meta tag above, and the last
command must show a redirect to the repository. The response sends
`Cache-Control: no-store`, so a deploy takes effect at once.

## If the repository moves

Change the repository URL in the meta tag and deploy. Go users keep importing
`aicr.run/...` and need no change. Update the Redirect Rules to the new
location at the same time.

If the zone's nameservers ever move off Cloudflare, remove the DS record at the
registrar first. A DS record left over an unsigned zone makes every validating
resolver fail to resolve `aicr.run`.
