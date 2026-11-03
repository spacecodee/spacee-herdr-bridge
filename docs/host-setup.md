# Host setup: bridge + Tailscale

Goal: open the herdr-core terminals of the Linux host (`<host>`) from the iPhone, reachable
only by your own devices on the tailnet. Nothing is exposed to the internet, and tagged
servers in your tailnet cannot reach it.

```
iPhone (Tailscale app) --https--> <host>:443 (tailscale serve) --> 127.0.0.1:8080 (bridge container) --> hc socket
```

| Piece | Listens on | Who can reach it |
| --- | --- | --- |
| bridge (Docker container) | published on host `127.0.0.1:8080` | only processes on <host> |
| `tailscale serve` | <host> tailnet address, port 443 | devices allowed by the tailnet policy: your own (`autogroup:member`) |
| herdr-core (`hc`) | Unix socket `/run/user/<uid>/herdr-core/default.sock` | only your user on <host> |

Why this shape:

- **Core has no authentication.** Whoever reaches the bridge with the token has a shell on
  <host> and on every machine added with `hc machine add`. Every layer below exists to keep
  that set of people at one: you.
- **HTTPS is required.** The session cookie is `Secure` and Safari needs a secure context for
  the PWA. `tailscale serve` provides HTTPS with a valid certificate for
  `<host>.<tailnet>.ts.net`, so the bridge does not handle TLS.
- **`serve`, never `funnel`.** `serve` is tailnet-only; `funnel` publishes to the internet.

Follow the steps in order. Commands run on <host> unless a step says otherwise.

## 1. Prerequisites

```sh
systemctl --user is-enabled herdr-core   # core starts at login (unit: scripts/herdr-core.service)
hc ls                     # herdr-core answers
tailscale status          # <host> is logged in to the tailnet
loginctl show-user "$USER" -p Linger   # must print Linger=yes
docker compose version    # Docker Engine with the Compose plugin
```

If `Linger` is `no`, enable it so herdr-core and its socket directory (`/run/user/<uid>`)
exist at boot without an open session:

```sh
sudo loginctl enable-linger "$USER"
```

Optional helper: install the core unit once per machine (it starts herdr-core at boot, without a login):

```sh
install -m 644 scripts/herdr-core.service ~/.config/systemd/user/herdr-core.service
systemctl --user daemon-reload
systemctl --user enable --now herdr-core
```

Another optional helper, `scripts/herdr-core-restart`, restarts the core cleanly from your user session (it stops every live terminal).

## 2. Tailnet settings (admin console, once)

In <https://login.tailscale.com/admin/dns>:

1. **MagicDNS**: on.
2. **HTTPS Certificates**: enable. `tailscale serve` needs it to issue the
   `*.ts.net` certificate. It is already on if this prints <host>'s name:
   ```sh
   tailscale status --json | grep -A2 CertDomains
   ```

Enabling it changes nothing that already works. It only lets a machine request a
certificate for its own `*.ts.net` name when that machine runs `tailscale serve`, `funnel` or
`cert`; there is no redirect and nothing is forced to HTTPS. Services you already run on other
tailnet machines (for example a panel on `http://<vps>:3000`) keep working.

The one cost is privacy: every public certificate is recorded in the Certificate
Transparency logs, which anyone can search. Issuing <host>'s certificate publishes
`<host>.<tailnet>.ts.net` (machine and tailnet names only; nobody outside the tailnet can
connect with it), and only for machines that request one. To avoid revealing real names,
before enabling:

- rename the machine (Machines > <host> > Edit machine name), and/or
- change the tailnet DNS name to a random one on the DNS page. This renames every
  `*.ts.net` name: update any place that uses the VPS's full `<vps>.<tailnet>.ts.net` name.
  Short names (`<vps>`) and tailnet IP addresses keep working.

## 3. Tailnet policy

In <https://login.tailscale.com/admin/acls/file>. The policy denies everything that no grant
allows, so the rule that matters is that **no tagged machine is a `src` towards your
devices**. The current policy already does this:

```jsonc
"grants": [
    // Your personal devices reach everything. Tagged machines are not members.
    {"src": ["autogroup:member"], "dst": ["*"], "ip": ["*"]},

    // Example: a CI runner reaches only the web ports of one server.
    {"src": ["tag:ci"], "dst": ["tag:server"], "ip": ["tcp:3000", "tcp:443"]},
],
```

A server tagged `tag:server` can answer connections you open (SSH, a panel) but cannot open one
towards <host>.

Add tests so a future edit cannot open <host> to tagged machines without failing on save.
`<host-tailnet-ip>` is <host>'s tailnet address (`tailscale ip -4` on <host>):

```jsonc
"tests": [
    {
        "src":    "tag:ci",
        "accept": ["tag:server:3000", "tag:server:443"],
        "deny":   ["tag:server:22", "tag:server:5432", "<host-tailnet-ip>:443"],
    },
    {
        "src":  "tag:server",
        "deny": ["<host-tailnet-ip>:443", "<host-tailnet-ip>:22"],
    },
],
```

Save. If Tailscale rejects it, a grant lets a tagged machine reach <host>: fix the grant, do
not delete the test.

## 4. Run the bridge container

Only the bridge runs in a container. herdr-core stays on the host: its terminals must be
shells on <host>, with your files and tools. The container gets no extra isolation from
this: it holds core's socket, which is a shell on the host.

What `compose.yaml` does:

- runs as your host user (uid/gid 1000 by default; export `UID`/`GID` if yours differ), so
  it can use core's socket and own the token file;
- mounts `/run/user` read-only with `rslave` propagation, so it sees core's socket even when
  `/run/user/<uid>` is mounted after Docker starts at boot, and when hc recreates it;
- mounts `~/.config/spacee-herdr` for the bridge's state: the token, the signed-in devices
  (`devices.json`), the Web Push keys (`vapid.json`) and push subscriptions (`push.json`), all 0600;
- mounts `~/.cache/spacee-herdr/uploads` at the same absolute path, for images uploaded from the phone;
- mounts `~/.claude`, `~/.pi`, `~/.codex` and `~/.local/share/opencode` read-only at the same
  absolute paths, so the chat view (`/api/chat`) can read the agents' transcripts (they must
  exist before the first start; opencode's is a SQLite database opened read-only, which works on
  the read-only mount while opencode is running because its `-shm`/`-wal` files exist then);
- runs in the host PID namespace (`pid: host`), see "Chat transcripts" below;
- publishes port 8080 on host loopback only (`127.0.0.1:8080`). Never change it to
  `8080:8080` or a tailnet IP: Docker's published ports bypass `ufw`, and the only way in
  must be `tailscale serve`;
- restarts with Docker (`restart: unless-stopped`), including after a reboot.

The image embeds the frontend build committed in `internal/webui/dist`.

```sh
cd spacee-herdr-bridge
install -d -m 700 ~/.config/spacee-herdr   # must exist and be yours before the first start
install -d -m 700 ~/.cache/spacee-herdr/uploads   # images sent from the phone; same rule
docker compose pull && docker compose up -d   # prebuilt image from ghcr.io (or `docker compose up -d --build` to build here)
docker compose ps                          # Up, 127.0.0.1:8080->8080/tcp
curl -s http://127.0.0.1:8080/healthz      # ok
```

`~/.cache/spacee-herdr/uploads` holds the images you send from the phone to agents (PNG, JPEG,
WebP, GIF, at most 15 MiB, mode 0600). The bridge returns their path on the host, which is
where the agents read them. Files older than 7 days are deleted at startup and then hourly.

### Chat transcripts (`/api/chat`)

`GET /api/chat?terminal=<address>` streams the normalized transcript of the coding agent
running in a terminal (Claude Code, pi, Codex, opencode) so the web client can show it as a chat.
It is read-only: input still goes through the terminal. To do that the bridge:

1. asks core (`terminal.get`) for the terminal's foreground process id and directory;
2. walks that process's descendants in `/proc` (the agent is often a child of a wrapper) and
   recognizes the agent by its executable or first arguments;
3. finds its transcript: Claude Code via `~/.claude/sessions/<pid>.json`; pi and Codex as the
   newest session file in the terminal's directory modified after the process started; opencode
   as the newest top-level session for that directory in its SQLite database. An agent that has
   no transcript yet (nothing sent) is still reported, with an empty chat.

Why `pid: host`: without it the container sees only its own processes, so step 2 is impossible.
What it changes: the container can **see** the processes of the host (list them, read their
`cmdline` and `stat`; `/proc/<pid>/environ` and `fd` stay private to other users). It adds no
capability and stays non-root, but as the same uid it could now also signal your processes
(`kill`) and read same-uid process details such as `environ`. That adds no real power: the
container already holds core's socket, which is a shell as you on the host. `cmdline` may
contain secrets passed as arguments; the bridge reads it only to match an agent name and never
logs or returns it.

Other safeguards: the bridge only opens files under `~/.claude`, `~/.pi` and `~/.codex`
(symlinks resolved, anything outside refused) and the opencode database under
`~/.local/share/opencode` (opened read-only, never written), all mounted read-only; it never logs message
contents or paths; at most 8 chat streams at once (`429` beyond); and the endpoint needs the
session cookie and a same-origin request like the rest of the API. Remote terminals
(`machine/tN`) and other agents (agy) answer `none` for now.

On first start the bridge creates the token at `~/.config/spacee-herdr/token` and the Web Push
keys in `vapid.json` (mode 0600); `devices.json` appears with the first login. To keep the token
and the signed-in devices when moving to a new machine, copy the whole directory (mode 0700,
files 0600) before the first start; otherwise sign in again.

## 5. Ship a newer frontend

The frontend is embedded at build time. After a frontend change:

```sh
(cd ../spacee-herdr-frontend && pnpm build)
rm -rf internal/webui/dist/* && cp -R ../spacee-herdr-frontend/dist/spacee-herdr-frontend/browser/. internal/webui/dist/
docker compose up -d --build
```

## 6. Publish to the tailnet with `tailscale serve`

`tailscale serve` changes need root unless your user is the Tailscale operator. Make it the
operator once, then publish:

```sh
sudo tailscale set --operator="$USER"
tailscale serve --bg 8080
tailscale serve status
```

`serve status` prints the URL, `https://<host>.<tailnet>.ts.net`, proxying to
`http://127.0.0.1:8080`. `--bg` keeps the configuration across reboots.

Check that nothing is public:

```sh
tailscale funnel status   # must not list <host>:443 as Funnel on
```

## 7. iPhone

1. Install the Tailscale app, log in with the same account and turn the VPN on.
2. Open `https://<host>.<tailnet>.ts.net` in Safari.
3. Sign in. The first device uses the token (read it on <host>, below). Later phones (iPhone or
   Android) can use a pairing code from a signed-in device (Settings → Devices → Add a device,
   once that screen ships in the app) or the token. Each device gets its own session.
   Read the token:
   ```sh
   cat ~/.config/spacee-herdr/token
   ```
   Send it through a channel you trust (for example AirDrop from a Mac, or type it). Do not
   paste it into chats or notes that sync to third parties.
4. Optional: Share > **Add to Home Screen** to open it as an app.

## 8. Verify

| From | Command | Expected |
| --- | --- | --- |
| <host> | `curl -s http://127.0.0.1:8080/healthz` | `ok` |
| <host> | `curl -s -o /dev/null -w '%{http_code}\n' http://<host-tailnet-ip>:8080/healthz` | connection refused (bridge is loopback only) |
| Mac or iPhone on the tailnet | open `https://<host>.<tailnet>.ts.net` | login screen; the token logs in; terminals listed |
| same | wrong token | rejected |
| VPS (`ssh <user>@<vps>`) | `curl -m 5 -sS -o /dev/null -w '%{http_code}\n' https://<host>.<tailnet>.ts.net/healthz` | times out |
| iPhone with Tailscale off, mobile data | open the URL | does not load |

If the VPS row answers with an HTTP code, the tailnet policy lets it reach <host>: go back to
step 3 before using the bridge.

## Maintenance

- **Update**: `git pull && docker compose pull && docker compose up -d` (or `git pull && docker compose up -d --build` to build locally). Pin a release with `BRIDGE_VERSION=1.2.0`.
- **Publishing the image** (maintainers): see "Publishing the image" in the README. The token needs only `write:packages`, logout is automatic, and `~/.docker` must never be mounted into the container. After the first push, make the package public once in GitHub (Package settings → Change visibility).
- **Logs**: `docker compose logs -f`. The bridge never logs tokens or terminal data.
- **Sign out a device** (lost phone): in the app, Settings → Devices, open the device and
  choose Sign out device. Its session, open streams and notifications stop at once. Each
  device has its own session (`~/.config/spacee-herdr/devices.json`, mode 0600); a new phone
  signs in with a pairing code from Settings → Devices → Add a device, or with the token.
  Also remove the lost device from the tailnet in the admin console.
- **Rotate the token** (token leaked): delete the file and restart. Only new logins with the
  token and old cookies from before per-device sessions are affected: devices already signed
  in keep working. To sign every device out as well, also delete `devices.json`.
  ```sh
  rm ~/.config/spacee-herdr/token ~/.config/spacee-herdr/devices.json  # devices.json: all devices out
  docker compose restart
  ```
- **Stop publishing**: `tailscale serve reset` (clears every serve rule on <host>).
- **Remove the bridge**: `docker compose down`.
- **New or reinstalled machine**: install herdr-core, Tailscale (log in) and Docker, enable
  linger, clone this repo, then steps 4 and 6. The tailnet settings and policy (steps 2-3)
  live in the admin console; update the <host> IP in the policy tests if it changed.

## Troubleshooting

- **`serve` says HTTPS is not enabled**: step 2, HTTPS Certificates.
- **`serve` says access denied**: run it with `sudo` or set the operator (step 6).
- **Login works on `http://127.0.0.1:8080` in a desktop browser but not over plain HTTP from
  another device**: expected; the cookie is `Secure`. Use the `https://...ts.net` URL.
- **Login returns 403**: the request `Origin` does not match the host. Open the bridge by the
  same name it is served on, not through another proxy or address.
- **Terminal list empty or errors**: `hc ls` on <host>; check that the core runs
  (`systemctl --user status herdr-core`). Run `scripts/herdr-core-restart` to restart it
  cleanly (it stops every live terminal). Do not start the core from an agent shell:
  it inherits `CLAUDE_CODE_*` and the chat view stays empty. `docker compose logs` printing `core unavailable`
  means the container cannot reach the socket: check `UID` and that
  `/run/user/<uid>/herdr-core/default.sock` exists.
- **Container fails to start with a bind-mount error**: `~/.config/spacee-herdr` does not
  exist; create it (step 4).
