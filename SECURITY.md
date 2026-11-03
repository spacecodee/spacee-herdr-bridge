# Security policy

## Threat model

herdr-core has no authentication: whoever reaches its socket has a shell on the host and on
every machine added with `hc machine add`. The bridge exposes that socket to a browser, so
**anyone who gets past the bridge has a shell on your host.** Everything in the design exists to
keep that set of people at one: you.

- The bridge listens on loopback only (`127.0.0.1:8080`). It is meant to be reached through
  `tailscale serve`, so only devices on your tailnet can connect.
- Every API route needs a session: the access token, or a device paired with a one-time code.
  Each device has its own revocable session.
- The WebSocket `Origin` is checked on every upgrade, and writes require a same-origin request.
- Tokens, terminal data, transcripts and upload paths are never logged.
- Layout and agent launches accept only an allow-listed set of programs.

**Never expose the bridge to the public internet** (no Tailscale Funnel, no port forwarding, no
reverse proxy on a public host). It is not designed to be a public service.

Out of scope: a compromised host or tailnet device, a leaked token that you have not rotated,
and anything that requires the bridge to be exposed publicly.

## Supported versions

Only the latest minor release receives security fixes.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub private vulnerability reporting: open the
repository's **Security** tab and choose **Report a vulnerability**. Include the version, what you
did and what you expected. You will get a reply as soon as possible.
