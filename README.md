# spacee-herdr-bridge

[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Docker](https://img.shields.io/badge/Docker-Compose-2496ED?logo=docker&logoColor=white)](https://docs.docker.com/compose/)
[![Tailscale](https://img.shields.io/badge/Tailscale-serve-242424?logo=tailscale&logoColor=white)](https://tailscale.com/kb/1312/serve)
[![herdr-core](https://img.shields.io/badge/herdr--core-hc-6E56CF)](https://github.com/herdrdev/herdr-core)
[![Web Push](https://img.shields.io/badge/Web%20Push-VAPID-5A0FC8?logo=pwa&logoColor=white)](https://datatracker.ietf.org/doc/html/rfc8292)
[![SQLite](https://img.shields.io/badge/SQLite-read--only-003B57?logo=sqlite&logoColor=white)](https://pkg.go.dev/modernc.org/sqlite)
[![Platform](https://img.shields.io/badge/platform-Linux-FCC624?logo=linux&logoColor=black)](docs/host-setup.md)

A small Go server that lets a phone use the terminals and coding agents of a Linux machine
running [herdr-core](https://github.com/herdrdev/herdr-core) (`hc`). It serves the
[spacee-herdr web app](https://github.com/spacecodee/spacee-herdr-frontend) (an installable
PWA, embedded in the binary), relays the terminal stream to herdr-core, and adds the few host
features a browser cannot do on its own.

```
iPhone (installed PWA)
   |  https / wss, tailnet only (tailscale serve)
   v
spacee-herdr-bridge  (this repo, Docker, 127.0.0.1:8080)
   |  Unix socket
   v
herdr-core (hc)  ---- ssh ---->  other machines added with `hc machine add`
```

## Features

| Area | What it does |
| --- | --- |
| Access | Token login exchanged for an HttpOnly, Secure, SameSite=Strict session cookie. Origin checked on every login, upgrade and write. |
| Terminals | `/ws` relays bytes verbatim between the browser and the herdr-core socket. The web app speaks the herdr framed protocol itself. |
| Web app | The PWA build is embedded and served with SPA routing, correct content types and cache headers for the service worker. |
| Chat | Reads coding-agent transcripts read-only (Claude Code, pi, Codex, opencode) and streams them as normalized messages over SSE, plus per-terminal summaries for the home screen. |
| Notifications | Watches herdr-core events and sends Web Push when an agent is blocked or done, or a terminal finishes or is lost. No terminal output is ever included. |
| Images | Stores images uploaded from the phone on the host and returns their path, so agents can read them. Deleted after 7 days. |
| Layout | Creates, renames, moves and closes herdr spaces, tabs and panes through herdr-core, so it works with the desktop window closed. |

## Requirements

- Linux host running herdr-core (`hc`) as your user, with lingering enabled.
- Docker Engine with the Compose plugin.
- Tailscale, logged in, with HTTPS certificates enabled for the tailnet.

## Quick start

```sh
git clone git@github.com:spacecodee/spacee-herdr-bridge.git
cd spacee-herdr-bridge
install -d -m 700 ~/.config/spacee-herdr ~/.cache/spacee-herdr/uploads
docker compose up -d --build
tailscale serve --bg 8080
```

Open `https://<machine>.<tailnet>.ts.net` on the phone, sign in with the token from
`~/.config/spacee-herdr/token`, then use Share > Add to Home Screen.

The complete, step-by-step setup (tailnet policy, verification, token rotation, migration to a
new machine, troubleshooting) is in [docs/host-setup.md](docs/host-setup.md).

## Security model

herdr-core has no authentication: whoever reaches its socket has a shell. The bridge exists to
keep that set of people at one.

- The container publishes only on host loopback. The only way in is `tailscale serve`
  (tailnet only, never Funnel), and the tailnet policy keeps tagged machines out.
- Every API route requires the session cookie; writes and upgrades also require a same-origin
  request.
- Tokens, terminal data, transcripts and upload paths are never logged.
- The container runs as your user, not root. It mounts the agent directories read-only and
  uses `pid: host` only to see which agent runs in each terminal; this adds visibility, not
  privilege, since it already holds a shell-equivalent socket.
- Layout and agent launches accept only an allow-listed set of programs; closing anything
  requires an explicit confirmation flag.

## Configuration

Flags (Compose sets the ones that differ from the defaults):

| Flag | Default | Purpose |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | Listen address. Keep it on loopback. |
| `-core` | `/run/user/<uid>/herdr-core/default.sock` | herdr-core socket. |
| `-upload-dir` | `~/.cache/spacee-herdr/uploads` | Uploaded images; must be a host path. |
| `-home` | your home | Where `.claude`, `.pi` and `.codex` live. |
| `-proc` | `/proc` | Process table used to identify agents. |
| `-push-subject` | repository URL | VAPID subject for Web Push. |

Files in `~/.config/spacee-herdr` (all mode 0600): `token`, `vapid.json`, `push.json`.

## API

All routes require the session cookie.

| Route | Description |
| --- | --- |
| `POST /api/session`, `GET /api/session` | Sign in with the token; check the session. |
| `GET /ws` | WebSocket relayed to herdr-core. |
| `GET /api/chat?terminal=<address>` | SSE stream of an agent conversation (`reset`, `append`, `update`, `none`). |
| `GET /api/chat/summaries` | Title and last message of every agent terminal. |
| `GET /api/push/key`, `POST`/`DELETE /api/push/subscription`, `POST /api/push/test` | Web Push. |
| `POST /api/upload` | Store an image (PNG, JPEG, WebP, GIF, at most 15 MiB). |
| `/api/layout/...` | Spaces, tabs and panes: create, split, rename, move, close. |
| `GET /healthz` | Liveness, no authentication. |

## Development

```sh
go vet ./... && go test -race ./...
go build -o bin/bridge ./cmd/bridge
```

Tests use a fake herdr-core on a temporary Unix socket, never your real one.

To ship a new web app build, build the frontend and copy it into the embedded directory:

```sh
(cd ../spacee-herdr-frontend && pnpm build)
rm -rf internal/webui/dist/* && cp -R ../spacee-herdr-frontend/dist/spacee-herdr-frontend/browser/. internal/webui/dist/
docker compose up -d --build
```

## Project layout

```
cmd/bridge/          entry point and flags
internal/auth/       token file and cookie session
internal/coreproxy/  /ws relay to the herdr-core socket
internal/webui/      embedded web app and its HTTP caching rules
internal/chat/       transcript adapters, SSE stream, summaries
internal/push/       VAPID keys, subscriptions, core event watcher, sender
internal/upload/     image uploads and cleanup
internal/layout/     herdr layout document edits and labeled terminals
docs/                host setup guide
```

## Changelog

See [CHANGELOG.md](CHANGELOG.md).
