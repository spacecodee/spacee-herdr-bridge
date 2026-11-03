# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The web client served by the bridge has its own changelog in `spacee-herdr-frontend`.

## [Unreleased]

### Added

- Token login: the access token is created on first start in `~/.config/spacee-herdr/token` (mode 0600) and exchanged for an HttpOnly, Secure, SameSite=Strict session cookie.
- Authenticated WebSocket `/ws` that relays bytes between the browser and the local herdr-core socket; upgrades from a foreign Origin are rejected.
- The web client is embedded in the binary and served with single-page-app fallback.
- `/healthz` endpoint for local checks.
- Docker image and Compose file: runs as the host user, publishes only on host loopback (`127.0.0.1:8080`), mounts the core socket directory and the token directory, restarts with Docker.
- Host setup guide (`docs/host-setup.md`): tailnet settings, tailnet policy with tests that keep tagged machines (the VPS) away from the host, `tailscale serve` (tailnet only, no Funnel), iPhone login, verification table, token rotation, migration to a new machine, troubleshooting.

- Web Push: the bridge watches core's `events.subscribe` stream and sends a push (Angular service worker format, TTL 1 h) when a terminal's agent becomes `blocked` ("Needs you", high urgency) or `done`, or a terminal exits or loses its connection. Only transitions notify (never snapshots or reconnects), at most one per terminal per 10 s, and no terminal output is ever included. VAPID keys are created on first start in `~/.config/spacee-herdr/vapid.json` and subscriptions kept in `push.json` (both 0600, at most 10 browsers; an unreadable `push.json` is renamed to `push.json.corrupt-<unix-seconds>` and the bridge starts with no subscriptions, an unreadable `vapid.json` stops it). New authenticated endpoints `GET /api/push/key`, `POST`/`DELETE /api/push/subscription` and `POST /api/push/test`; new flag `-push-subject`. Uses `github.com/SherClockHolmes/webpush-go` for VAPID signing and RFC 8291 encryption.
- Image upload: authenticated `POST /api/upload` (same-origin only) takes a raw PNG, JPEG, WebP or GIF (type checked from the bytes, at most 15 MiB) and stores it on the host as `<YYYYMMDD-HHMMSS>-<random>.<ext>` (file 0600, directory 0700), returning `{"path": "<absolute host path>"}` so a terminal agent can read the image. Files older than 7 days are deleted at startup and hourly. New flag `-upload-dir` (default `$XDG_CACHE_HOME/spacee-herdr/uploads`, else `~/.cache/spacee-herdr/uploads`); Compose mounts that directory at the same path inside the container.
- Chat transcripts: authenticated, same-origin `GET /api/chat?terminal=<address>` streams the normalized, read-only transcript of the agent running in a terminal as Server-Sent Events (`reset` with the last 200 messages, then `append` / `update`, `none` with a reason, 15 s heartbeat; the file is polled every 500 ms and only appended bytes are read). Adapters for Claude Code (`~/.claude`), pi (`~/.pi`) and Codex (`~/.codex`); the agent is found by walking the terminal's foreground process tree in `/proc`. Text parts are cut at 64 KiB and tool input/output at 8 KiB, images are never sent as base64, paths and messages are never logged, at most 8 streams (`429` beyond). New flags `-home` and `-proc`.
- Chat summaries: authenticated, same-origin `GET /api/chat/summaries` returns one entry per live local terminal whose agent has a transcript (`terminal`, `agent`, `session`, `title` = first user prompt without slash commands (80 chars), `last` = plain-text preview of the newest message or tool call (140 chars) with its role and time, `messages`) for the conversations list. Only the last 256 KiB of each transcript is read (the first 64 KiB too when the tail has no user prompt), so `messages` counts that window, and results are cached per file until its size or mtime changes. Shells and unsupported agents are omitted; contents and paths are never logged.
- Chat adapter for opencode: sessions are read straight from its SQLite database (`~/.local/share/opencode/opencode.db`), opened read-only (`mode=ro`, `query_only`, 2 s busy timeout; never written or checkpointed, WAL rows from a running opencode are visible) through the pure-Go `modernc.org/sqlite` driver (no CGO; the binary grows about 6 MB). The session is the newest top-level `session_v2` row whose `directory` is the terminal's cwd (same caveat as pi and Codex); messages come from `session_message` by `seq` (text, reasoning, tool calls with status/input/output, attachments as notes), polled every 500 ms for new rows and for rows updated in place (a running tool completing). Works for `/api/chat` and `/api/chat/summaries`.
- Compose: `pid: host` (to see the agents' processes) and read-only mounts of `~/.claude`, `~/.pi`, `~/.codex` and `~/.local/share/opencode` at the same paths; the security impact is explained in `docs/host-setup.md`.

### Changed

- The bridge runs as a Docker Compose service instead of a systemd user service; update with `docker compose up -d --build`.

### Fixed

- PWA serving: a missing file (`/ngsw.json`, `/nope.js`) now returns 404 instead of `index.html`; only extensionless app routes fall back to the SPA. `.webmanifest` is served as `application/manifest+json`, and cache headers are set (`no-cache` for `index.html`, the service worker and the manifest; immutable for hashed build files; one day for icons and fonts) so updates reach installed apps.

[unreleased]: https://github.com/spacecodee/spacee-herdr-bridge/commits/main
