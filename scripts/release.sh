#!/usr/bin/env bash
# Cut a release of the bridge and the frontend, locally. Never pushes.
# Usage: scripts/release.sh [--dry-run] <version>      e.g. 1.3.0 (no "v")
#        scripts/release.sh --push-image <version>     multi-arch image to ghcr.io (needs docker login ghcr.io)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FRONTEND_DIR="${FRONTEND_DIR:-$ROOT/../spacee-herdr-frontend}"
IMAGE="ghcr.io/spacecodee/spacee-herdr-bridge"
DRY=0
PUSH_IMAGE=0
VERSION=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    --push-image) PUSH_IMAGE=1 ;;
    -*) echo "unknown option: $arg" >&2; exit 2 ;;
    *) VERSION="$arg" ;;
  esac
done

die() { echo "release: $*" >&2; exit 1; }
step() { if [ "$DRY" -eq 1 ]; then echo "[dry-run] $*"; fi; }

[ -n "$VERSION" ] || die "usage: release.sh [--dry-run] <version> | --push-image <version>"
[[ "$VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] \
  || die "'$VERSION' is not a semver version (use 1.3.0, without the v)"

if [ "$PUSH_IMAGE" -eq 1 ]; then
  echo "==> pushing $IMAGE:$VERSION and :latest (linux/amd64, linux/arm64)"
  step "docker buildx build --platform linux/amd64,linux/arm64 --push -t $IMAGE:$VERSION -t $IMAGE:latest"
  [ "$DRY" -eq 1 ] && exit 0
  docker buildx build --platform linux/amd64,linux/arm64 --build-arg "VERSION=$VERSION" \
    --push -t "$IMAGE:$VERSION" -t "$IMAGE:latest" "$ROOT" \
    || die "push failed; run 'docker login ghcr.io' first (token with write:packages)"
  exit 0
fi

[ -d "$FRONTEND_DIR" ] || die "frontend not found at $FRONTEND_DIR (set FRONTEND_DIR)"
FRONTEND_DIR="$(cd "$FRONTEND_DIR" && pwd)"
TAG="v$VERSION"
TODAY="$(date +%F)"

# --- checks ---------------------------------------------------------------
for repo in "$ROOT" "$FRONTEND_DIR"; do
  name="$(basename "$repo")"
  [ "$(git -C "$repo" rev-parse --abbrev-ref HEAD)" = "main" ] || die "$name is not on main"
  [ -z "$(git -C "$repo" status --porcelain)" ] || die "$name has uncommitted changes"
  git -C "$repo" rev-parse -q --verify "refs/tags/$TAG" >/dev/null && die "tag $TAG already exists in $name"
  # Entries = any non-blank line between "## [Unreleased]" and the next "## [".
  entries="$(awk '/^## \[Unreleased\]/{f=1;next} /^## \[/{f=0} f && NF' "$repo/CHANGELOG.md")"
  [ -n "$entries" ] || die "$name: CHANGELOG.md has no entries under [Unreleased]"
done

# move [Unreleased] to the new version and update the compare links.
update_changelog() { # <repo-dir> <github-repo>
  local file="$1/CHANGELOG.md" gh="$2" prev
  prev="$(sed -n 's|^\[unreleased\]: .*/compare/\(v[^.]*\.[^.]*\.[^.]*\)\.\.\.HEAD$|\1|p' "$file")"
  [ -n "$prev" ] || die "$file: cannot find the [unreleased] compare link"
  sed -i \
    -e "s|^## \[Unreleased\]\$|## [Unreleased]\n\n## [$VERSION] - $TODAY|" \
    -e "s|^\[unreleased\]: .*|[unreleased]: https://github.com/spacecodee/$gh/compare/$TAG...HEAD\n[$VERSION]: https://github.com/spacecodee/$gh/compare/$prev...$TAG|" \
    "$file"
}

echo "==> releasing $TAG (bridge + frontend), date $TODAY"
step "update both CHANGELOG.md files (move [Unreleased] to [$VERSION] - $TODAY, compare links)"
step "set frontend package.json version to $VERSION"
step "scripts/build.sh"
step "commit 'chore(release): $VERSION' and tag $TAG in both repos"
if [ "$DRY" -eq 0 ]; then
  update_changelog "$ROOT" spacee-herdr-bridge
  update_changelog "$FRONTEND_DIR" spacee-herdr-frontend
  (cd "$FRONTEND_DIR" && npm pkg set "version=$VERSION" >/dev/null)
  "$ROOT/scripts/build.sh"
  for repo in "$FRONTEND_DIR" "$ROOT"; do
    git -C "$repo" add -A
    git -C "$repo" commit -q -m "chore(release): $VERSION"
    git -C "$repo" tag "$TAG"
  done
fi

[ "$DRY" -eq 0 ] || exit 0
cat <<MSG

Done. Nothing was pushed. To publish:
  git -C "$FRONTEND_DIR" push origin main "$TAG"
  git -C "$ROOT" push origin main "$TAG"
Deploy on this machine:
  cd "$ROOT" && docker compose up -d
Optional multi-arch image on ghcr.io (after 'docker login ghcr.io'):
  scripts/release.sh --push-image $VERSION
MSG
