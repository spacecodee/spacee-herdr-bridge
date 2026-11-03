# spacee-herdr-bridge (Go)

Thin bridge between the web client (`../spacee-herdr-frontend`) and herdr-core (`hc`). It also serves the Angular build embedded in the binary.
It has no business logic: it moves bytes between a WebSocket and core's endpoint, and checks the access token.

## Project context (read before any work)

**What this is.** `spacee-herdr` is a web client (installable PWA) to use herdr-core terminals from a phone. The Herdr TUI worked great on mobile over SSH; the new Herdr desktop app is a native GPU window (Rust) that cannot run on a phone, and there is no mobile app. The owner uses an iPhone and does not want a paid Apple developer account, so the answer is a PWA reached over Tailscale.
It is only a client for machines that already run herdr-core (`hc`), such as a Linux box. It does not replace the desktop app. Herdr mods and layouts are JavaScript inside the native window and are unrelated to this project.

**Architecture**
```
iPhone (Safari/PWA) --https/wss via tailscale serve--> spacee-herdr-bridge (Go) --Unix socket--> herdr-core (hc) --ssh--> other machines
```
- `spacee-herdr-frontend`: Angular PWA (Tailwind, Spartan UI, xterm.js). No SSR.
- `spacee-herdr-bridge`: Go. Copies bytes between a WebSocket and core's socket, checks the token, and serves the embedded frontend build. No business logic, no database.
- `hc` (herdr-core, repo https://github.com/herdrdev/herdr-core): keeps terminals alive and draws nothing. `hc machine add <ssh-host>` makes one core list the terminals of other machines, so the bridge talks to a single core and new devices need no app change.
- The bridge runs on the same machine as the core it talks to. Do not deploy it to Dokploy or any public host.

**Protocol facts** (check `hc schema`; the API is preview 0.x, additive only)
- Plain mode: JSON-RPC 2.0, one message per line. `hello {"framed":true}` switches to framed mode: `[channel u32 LE][kind u8][len u32 LE][payload]`, kinds DATA 0, CREDIT 1, CLOSE 2, RESIZE 3, FOCUS 4, FACTS 5.
- `terminal.attach` returns a channel carrying the first paint, then raw program output; keys go back as DATA. Flow control: 256 KiB initial credit; the viewer sends CREDIT. `events.subscribe` streams status events.
- Methods: `terminal.{list,get,run,attach,send,keys,read,kill,...}`, `machine.*`, `events.subscribe`, `server.status`, `doc.get/doc.set`, `rpc.discover`.
- The API has no spaces/tabs/splits methods; those may live behind `doc.get/doc.set` (unverified). Agent status (working, blocked, done, idle) comes from the program via OSC 7501.

**Security.** Core has no authentication: whoever reaches its socket has a shell. The bridge must listen only on loopback or the tailnet, require a token, and check the WebSocket `Origin`. Never log tokens or terminal data.

**Decisions already made.** No SSR. Go for the bridge, standard library first. No Spring or other backend. pnpm for the frontend. Frontend build is embedded into the bridge binary.

**MVP scope, in order**
1. List terminals grouped by machine, with agent status, live via `events.subscribe`.
2. Attach to a terminal with xterm.js: output, input, resize.
3. Mobile key bar (Esc, Ctrl, Tab, arrows).
4. Reconnect and re-attach when iOS suspends the tab.
Later: PWA install, push notifications when an agent is blocked, spaces/tabs via `doc.get`.
Out of scope: user accounts, multi-tenant, public hosting, desktop mods.

**Status.** MVP step 1 done: the bridge has token login (`/api/session`, HttpOnly cookie) and `/ws`, an authenticated raw byte pipe to core's socket; the frontend speaks the hc framed protocol itself (`src/app/hc/`) and shows the live terminal list. Step 2 done: `/t/:address` attaches with xterm.js 6 (WebGL, fit, unicode11): output with CREDIT after xterm writes, input (64 KiB DATA chunks), RESIZE, FOCUS, exit/reattach banners. Step 3 done: key bar under the terminal (Esc, Tab, one-shot Ctrl, arrows honoring DECCKM; buttons keep focus on xterm so the iOS keyboard stays open). Theme follows the system light/dark scheme (app and xterm palettes), `/settings` sets terminal text size and scroll speed (localStorage), touch drags scroll the terminal (xterm scrollSensitivity), bundled JetBrainsMono Nerd Font Mono (public/fonts) for p10k icons. Step 4 done: HcClient reconnects with backoff (pauses while hidden, probes on resume for dead-but-open sockets, 401 -> login); the list resubscribes and the terminal view re-attaches automatically. MVP complete. PWA done: installable (manifest, icons, iOS metas), Angular service worker caches only the app shell and fonts (never /api or /ws), update toast; bridge unreachable keeps the route and shows Reconnecting… (only 401 goes to login). Web Push (bridge side) done: `internal/push`; the frontend subscription UI is separate. Spaces: list is Space > Tab > terminals from the herdr-app doc (doc.get, reloaded on doc events). Push enable/disable/test in Settings (installed app only on iOS). Compose drafts persist per terminal; Return key setting (send/newline); images attach or paste, downscaled client-side, uploaded to /api/upload, path inserted for agents. Chat view: agent terminals open as a chat (Spartan message/bubble/message-scroller/trace, markdown via marked+DOMPurify) fed by /api/chat SSE (bridge reads Claude Code, pi, Codex transcripts read-only); Terminal mode one tap away. Home is a conversations list (/api/chat/summaries: title, last message preview; Needs attention strip; search; grouped by herdr space in desktop tab order, agents as conversation rows and shells as compact rows; unplaced terminals per machine). opencode supported via read-only SQLite (modernc.org/sqlite); agy not supported (binary payload). Layout management from the phone: /api/layout (space, tab, split, rename, move with 0-based index, confirmed close) edits the herdr-app doc via doc.get/doc.set and starts labeled terminals, works with the desktop window closed. Next (later): terminal size when another viewer (desktop) is larger (nvim shows only the left part), push when an agent is blocked, spaces/tabs via doc.get.

**Repo layout.** Two sibling repos in one parent folder: `spacee-herdr-bridge/` and `spacee-herdr-frontend/`. Nothing lives in the parent folder. The orchestrator (in the bridge) can read both; each project keeps its own agent templates in `agents/`.

## Layout
- `cmd/bridge/main.go`: entry point (`-listen`, `-core`).
- `internal/auth/`: token file (`~/.config/spacee-herdr/token`, 0600) and cookie session (`POST/GET /api/session`).
- `internal/coreproxy/`: `GET /ws`, raw byte pipe between the WebSocket and core's Unix socket; core socket path rules.
- `internal/push/`: Web Push. VAPID keys (`vapid.json`) and subscriptions (`push.json`) beside the token, `/api/push/*` handlers, a watcher on core's `events.subscribe` (plain JSON-RPC, reconnects with backoff) and a transition tracker that sends Angular-service-worker payloads. Flag `-push-subject`. Exception to "no business logic", approved by the owner.
- `internal/upload/`: `POST /api/upload`, stores phone images (type from bytes, 15 MiB, 0600) in `-upload-dir` (default `~/.cache/spacee-herdr/uploads`, mounted at the same path in the container) and returns the host path; 7-day hourly cleanup.
- `internal/chat/`: read-only agent transcripts for the chat view (`GET /api/chat?terminal=<addr>`, SSE: reset/append/update/remove/none; Claude queued messages carry `queued: true` until delivered). `Parser` per agent (`claudeParser`, `piParser`, `codexParser`) feeding a `Builder`; `Tailer` follows the JSONL file; opencode (`opencode.go`) is a `follower` over its SQLite DB (`~/.local/share/opencode/opencode.db`, read-only `mode=ro`+`query_only`, pure-Go `modernc.org/sqlite`, session = newest top-level `session_v2` row for the cwd, messages from `session_message` by `seq`, polled; `session_part` is unused); `Resolver` reports `waiting` (agent running, no transcript yet: the stream sends an empty `reset` with the agent, summaries list it with `messages: 0`) and maps the terminal's foreground pid (from core `terminal.get`) to a transcript through `/proc` (needs `pid: host` in the container) and `~/.claude|.pi|.codex` (read-only mounts, symlinks resolved, nothing outside) and `~/.local/share/opencode`. agy is `unsupported`. Second approved exception to "no business logic"; never log message contents or paths. Flags `-home`, `-proc`.
- `internal/layout/`: manage spaces/tabs/panes without the window. Pure tree edits over the core doc `herdr-app` (`tree.go`, unknown fields kept, ids `spc|tb|p` + 32 hex), `Service` (doc.get -> edit -> doc.set with the version, 3 retries on `conflict`; terminals start first with labels `herdr.space`/`herdr.tab`, killed if the doc write fails; deletes write the doc then `terminal.kill`) and `/api/layout/*` handlers (`POST space|tab|split|move`, `PATCH space|tab/{id}`, `DELETE space|tab|pane/{id}?confirmed=1`). `argv` allow-list: none, claude, pi, codex, opencode, agy. Third approved exception to "no business logic".
- `internal/webui/`: embeds `dist/` (a copy of the frontend build) and serves it with SPA fallback.

## Commands
```sh
go vet ./... && go test ./...
go build -o bin/bridge ./cmd/bridge
# after `pnpm build` in ../spacee-herdr-frontend:
rm -rf internal/webui/dist/* && cp -R ../spacee-herdr-frontend/dist/spacee-herdr-frontend/browser/. internal/webui/dist/
```

## Run on the host (the Linux box that runs herdr-core, the real deployment)
Full step-by-step setup (tailnet policy, Docker container, `tailscale serve`, iPhone, verification): `docs/host-setup.md`. Only the bridge is containerized (`compose.yaml`); herdr-core stays on the host.
Repos: `~/Development/fullstack/spacee-herdr-app/{spacee-herdr-bridge,spacee-herdr-frontend}` (GitHub `spacecodee/*`, private).
```sh
scripts/deploy.sh                 # bridge on host 127.0.0.1:8080 (compose up --build with the git version + /healthz check); also the redeploy command
tailscale serve --bg 8080         # https://<host>.<tailnet>.ts.net -> the iPhone opens this
cat ~/.config/spacee-herdr/token  # paste once in the login screen
```
The session cookie is `Secure`: the iPhone must use the https tailscale URL. For local browser checks use `http://127.0.0.1:8080` (or `ng serve` on localhost with `proxy.conf.json`); `playwright-cli` is installed for that.

## Orchestrator workflow
- Write each task as a spec file (exact files, contract, tests, proof) and `hc send` the implementer a pointer to it.
- Plans and specs live in the repo they change, local only (`docs/superpowers/` is gitignored; never commit it, never write absolute home paths): plans in `docs/superpowers/plans/YYYY-MM-DD-<feature>.md`, task specs in `docs/superpowers/specs/YYYY-MM-DD-<task>.md` (frontend tasks in the frontend repo). Never in `tmp/` (`tmp/specs` is only a symlink kept for old pointers).
- Verify every report yourself: run the checks, review the diff, and do a real-browser check against the live core when behavior is visible. Then commit as a work unit and push.
- After a frontend change that should ship, rebuild and copy it into `internal/webui/dist`, rebuild the bridge, and commit the embedded build in the bridge repo.

## Rules
- Every user-visible change (behavior, deployment, docs users rely on) adds a line under `## [Unreleased]` in `CHANGELOG.md` (Keep a Changelog 1.1.0) in the same commit. The orchestrator checks it before committing.
- Standard library first. Add a dependency only when it removes real code (e.g. a WebSocket library), and say why.
- Validate the token and the WebSocket `Origin` on every upgrade. Do not log tokens or terminal data.
- Respect framed-mode credit/flow control; never buffer a stream without a bound.
- Tests go beside the code. Use a fake core on a temp Unix socket, not the user's real core.

## Agents

Claude Code reads this file through `CLAUDE.md`.
The orchestrator is pi in the "Orchestator - B" pane: it sees both projects, plans, delegates, verifies, commits. It does not write feature code.
Implementers are Claude Code panes the owner already runs in herdr: "Implementer - B" (bridge) and "Implementer - F" (frontend). Find their addresses with `hc ls` (`where` label) and delegate with `hc send <address> "<task>"`. Never start new agents with `hc run`.
An implementer does one scoped change per task, edits only the files named, writes tests first, runs the project checks, and reports with `hc send <orchestrator address>`: what changed, proof, open risks.
UI work in the frontend uses the `spartan-ui` MCP (see the frontend AGENTS.md).
`agents/*.md` are equivalent hc templates, kept for `hc run` use. Use `hc wait`, never sleep or poll.
