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

### Changed

- The bridge runs as a Docker Compose service instead of a systemd user service; update with `docker compose up -d --build`.

### Fixed

- PWA serving: a missing file (`/ngsw.json`, `/nope.js`) now returns 404 instead of `index.html`; only extensionless app routes fall back to the SPA. `.webmanifest` is served as `application/manifest+json`, and cache headers are set (`no-cache` for `index.html`, the service worker and the manifest; immutable for hashed build files; one day for icons and fonts) so updates reach installed apps.

[unreleased]: https://github.com/spacecodee/spacee-herdr-bridge/commits/main
