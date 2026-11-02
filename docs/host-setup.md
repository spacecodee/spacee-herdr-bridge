# Host setup: bridge + Tailscale

Goal: open the herdr-core terminals of the Linux host (`workstation`) from the iPhone, reachable
only by your own devices on the tailnet. Nothing is exposed to the internet, and tagged
machines such as the Dokploy VPS cannot reach it.

```
iPhone (Tailscale app) --https--> workstation:443 (tailscale serve) --> 127.0.0.1:8080 (bridge container) --> hc socket
```

| Piece | Listens on | Who can reach it |
| --- | --- | --- |
| bridge (Docker container) | published on host `127.0.0.1:8080` | only processes on workstation |
| `tailscale serve` | workstation tailnet address, port 443 | devices allowed by the tailnet policy: your own (`autogroup:member`) |
| herdr-core (`hc`) | Unix socket `/run/user/<uid>/herdr-core/default.sock` | only your user on workstation |

Why this shape:

- **Core has no authentication.** Whoever reaches the bridge with the token has a shell on
  workstation and on every machine added with `hc machine add`. Every layer below exists to keep
  that set of people at one: you.
- **HTTPS is required.** The session cookie is `Secure` and Safari needs a secure context for
  the PWA. `tailscale serve` provides HTTPS with a valid certificate for
  `workstation.<tailnet>.ts.net`, so the bridge does not handle TLS.
- **`serve`, never `funnel`.** `serve` is tailnet-only; `funnel` publishes to the internet.

Follow the steps in order. Commands run on workstation unless a step says otherwise.

## 1. Prerequisites

```sh
hc ls                     # herdr-core answers (it runs as systemd user units hc-serve-*)
tailscale status          # workstation is logged in to the tailnet
loginctl show-user "$USER" -p Linger   # must print Linger=yes
docker compose version    # Docker Engine with the Compose plugin
```

If `Linger` is `no`, enable it so herdr-core and its socket directory (`/run/user/<uid>`)
exist at boot without an open session:

```sh
sudo loginctl enable-linger "$USER"
```

## 2. Tailnet settings (admin console, once)

In <https://login.tailscale.com/admin/dns>:

1. **MagicDNS**: on.
2. **HTTPS Certificates**: enable. `tailscale serve` needs it to issue the
   `*.ts.net` certificate. It is already on if this prints workstation's name:
   ```sh
   tailscale status --json | grep -A2 CertDomains
   ```

Enabling it changes nothing that already works. It only lets a machine request a
certificate for its own `*.ts.net` name when that machine runs `tailscale serve`, `funnel` or
`cert`; there is no redirect and nothing is forced to HTTPS. The Dokploy panel on
`http://<vps>:3000`, Traefik's own certificates on the VPS, and the CI grant keep working.

The one cost is privacy: every public certificate is recorded in the Certificate
Transparency logs, which anyone can search. Issuing workstation's certificate publishes
`workstation.<tailnet>.ts.net` (machine and tailnet names only; nobody outside the tailnet can
connect with it), and only for machines that request one. To avoid revealing real names,
before enabling:

- rename the machine (Machines > workstation > Edit machine name), and/or
- change the tailnet DNS name to a random one on the DNS page. This renames every
  `*.ts.net` name: update any place that uses the VPS's full `<vps>.<tailnet>.ts.net` name.
  Short names (`<vps>`) and `100.x` addresses keep working.

## 3. Tailnet policy

In <https://login.tailscale.com/admin/acls/file>. The policy denies everything that no grant
allows, so the rule that matters is that **no tagged machine is a `src` towards your
devices**. The current policy already does this:

```jsonc
"grants": [
    // Your personal devices reach everything. Tagged machines are not members.
    {"src": ["autogroup:member"], "dst": ["*"], "ip": ["*"]},

    // The CI runner reaches only the Dokploy panel and the app.
    {"src": ["tag:ci"], "dst": ["tag:dokploy"], "ip": ["tcp:3000", "tcp:443"]},
],
```

The VPS (`tag:dokploy`) can answer connections you open (SSH, panel) but cannot open one
towards workstation.

Add tests so a future edit cannot open workstation to tagged machines without failing on save.
`<host-tailnet-ip>` is workstation's tailnet address (`tailscale ip -4` on workstation):

```jsonc
"tests": [
    {
        "src":    "tag:ci",
        "accept": ["tag:dokploy:3000", "tag:dokploy:443"],
        "deny":   ["tag:dokploy:22", "tag:dokploy:5432", "<host-tailnet-ip>:443"],
    },
    {
        "src":  "tag:dokploy",
        "deny": ["<host-tailnet-ip>:443", "<host-tailnet-ip>:22"],
    },
],
```

Save. If Tailscale rejects it, a grant lets a tagged machine reach workstation: fix the grant, do
not delete the test.

## 4. Run the bridge container

Only the bridge runs in a container. herdr-core stays on the host: its terminals must be
shells on workstation, with your files and tools. The container gets no extra isolation from
this: it holds core's socket, which is a shell on the host.

What `compose.yaml` does:

- runs as your host user (uid/gid 1000 by default; export `UID`/`GID` if yours differ), so
  it can use core's socket and own the token file;
- mounts `/run/user` read-only with `rslave` propagation, so it sees core's socket even when
  `/run/user/<uid>` is mounted after Docker starts at boot, and when hc recreates it;
- mounts `~/.config/spacee-herdr` for the token;
- publishes port 8080 on host loopback only (`127.0.0.1:8080`). Never change it to
  `8080:8080` or a tailnet IP: Docker's published ports bypass `ufw`, and the only way in
  must be `tailscale serve`;
- restarts with Docker (`restart: unless-stopped`), including after a reboot.

The image embeds the frontend build committed in `internal/webui/dist`.

```sh
cd ~/Development/fullstack/spacee-herdr-app/spacee-herdr-bridge
install -d -m 700 ~/.config/spacee-herdr   # must exist and be yours before the first start
docker compose up -d --build
docker compose ps                          # Up, 127.0.0.1:8080->8080/tcp
curl -s http://127.0.0.1:8080/healthz      # ok
```

On first start the bridge creates the token at `~/.config/spacee-herdr/token` (mode 0600).
To keep the token when moving to a new machine, copy that file (mode 0600) before the first
start; otherwise log in again with the new one.

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

`serve status` prints the URL, `https://workstation.<tailnet>.ts.net`, proxying to
`http://127.0.0.1:8080`. `--bg` keeps the configuration across reboots.

Check that nothing is public:

```sh
tailscale funnel status   # must not list workstation:443 as Funnel on
```

## 7. iPhone

1. Install the Tailscale app, log in with the same account and turn the VPN on.
2. Open `https://workstation.<tailnet>.ts.net` in Safari.
3. Paste the token once. Read it on workstation:
   ```sh
   cat ~/.config/spacee-herdr/token
   ```
   Send it through a channel you trust (for example AirDrop from a Mac, or type it). Do not
   paste it into chats or notes that sync to third parties.
4. Optional: Share > **Add to Home Screen** to open it as an app.

## 8. Verify

| From | Command | Expected |
| --- | --- | --- |
| workstation | `curl -s http://127.0.0.1:8080/healthz` | `ok` |
| workstation | `curl -s -o /dev/null -w '%{http_code}\n' http://<host-tailnet-ip>:8080/healthz` | connection refused (bridge is loopback only) |
| Mac or iPhone on the tailnet | open `https://workstation.<tailnet>.ts.net` | login screen; the token logs in; terminals listed |
| same | wrong token | rejected |
| VPS (`ssh <user>@<vps>`) | `curl -m 5 -sS -o /dev/null -w '%{http_code}\n' https://workstation.<tailnet>.ts.net/healthz` | times out |
| iPhone with Tailscale off, mobile data | open the URL | does not load |

If the VPS row answers with an HTTP code, the tailnet policy lets it reach workstation: go back to
step 3 before using the bridge.

## Maintenance

- **Update**: `git pull && docker compose up -d --build`.
- **Logs**: `docker compose logs -f`. The bridge never logs tokens or terminal data.
- **Rotate the token** (lost phone, token leaked): delete the file and restart. The old
  cookie stops working at once; log in again with the new token.
  ```sh
  rm ~/.config/spacee-herdr/token
  docker compose restart
  ```
  Also remove the lost device from the tailnet in the admin console.
- **Stop publishing**: `tailscale serve reset` (clears every serve rule on workstation).
- **Remove the bridge**: `docker compose down`.
- **New or reinstalled machine**: install herdr-core, Tailscale (log in) and Docker, enable
  linger, clone this repo, then steps 4 and 6. The tailnet settings and policy (steps 2-3)
  live in the admin console; update the workstation IP in the policy tests if it changed.

## Troubleshooting

- **`serve` says HTTPS is not enabled**: step 2, HTTPS Certificates.
- **`serve` says access denied**: run it with `sudo` or set the operator (step 6).
- **Login works on `http://127.0.0.1:8080` in a desktop browser but not over plain HTTP from
  another device**: expected; the cookie is `Secure`. Use the `https://...ts.net` URL.
- **Login returns 403**: the request `Origin` does not match the host. Open the bridge by the
  same name it is served on, not through another proxy or address.
- **Terminal list empty or errors**: `hc ls` on workstation; check that `hc-serve-*` is running
  (`systemctl --user list-units 'hc-*'`). `docker compose logs` printing `core unavailable`
  means the container cannot reach the socket: check `UID` and that
  `/run/user/<uid>/herdr-core/default.sock` exists.
- **Container fails to start with a bind-mount error**: `~/.config/spacee-herdr` does not
  exist; create it (step 4).
