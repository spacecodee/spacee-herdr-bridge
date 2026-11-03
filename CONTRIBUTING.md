# Contributing

Thanks for helping. Please open an issue to discuss a feature before sending code; bug fixes
can go straight to a pull request.

- Run `go vet ./... && go test ./...` before pushing.
- Put tests beside the code and use a fake herdr-core on a temporary Unix socket, never your
  real core.
- Standard library first. Add a dependency only when it removes real code, and say why.
- Every user-visible change adds a line under `## [Unreleased]` in `CHANGELOG.md`
  ([Keep a Changelog](https://keepachangelog.com/en/1.1.0/)).
- Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`).
- Never log tokens or terminal data.

The web app lives in the sibling repository
[spacee-herdr-frontend](https://github.com/spacecodee/spacee-herdr-frontend). Its build is
embedded in this binary: `scripts/build.sh` builds it and copies it into `internal/webui/dist`.
Keep both repositories side by side (or set `FRONTEND_DIR`).
