#!/usr/bin/env bash
# Build the bridge image from this checkout with the git version and (re)start it.
# Plain `docker compose up -d --build` also works but the bridge then reports "dev".
# Users of the published image run `docker compose pull && docker compose up -d` instead.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
export BRIDGE_BUILD_VERSION="$(git describe --tags --always --dirty)"
docker compose up -d --build

for _ in $(seq 1 20); do
  if curl -fsS -o /dev/null http://127.0.0.1:8080/healthz; then
    echo "deployed $BRIDGE_BUILD_VERSION: /healthz ok"
    exit 0
  fi
  sleep 1
done
echo "deploy: /healthz did not answer on 127.0.0.1:8080 (docker compose logs bridge)" >&2
exit 1
