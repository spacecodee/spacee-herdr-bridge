#!/usr/bin/env bash
# Cut a release of the bridge and the frontend, locally. Never pushes.
# Usage: scripts/release.sh [--dry-run] <version>      e.g. 1.3.0 (no "v")
#        scripts/release.sh --push-image <version>     multi-arch image to ghcr.io from tag v<version> (needs docker login ghcr.io; logs out after)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FRONTEND_DIR="${FRONTEND_DIR:-$ROOT/../spacee-herdr-frontend}"
IMAGE="ghcr.io/spacecodee/spacee-herdr-bridge"
# GHCR links a multi-arch package to its repo from the index annotations, not the per-platform labels.
INDEX_ANNOTATIONS=(
  --annotation "index:org.opencontainers.image.source=https://github.com/spacecodee/spacee-herdr-bridge"
  --annotation "index:org.opencontainers.image.description=Bridge between the spacee-herdr web client and herdr-core"
  --annotation "index:org.opencontainers.image.licenses=MIT"
)
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
  TAG="v$VERSION"
  BUILDER="spacee-herdr"
  echo "==> pushing $IMAGE:$VERSION and :latest (linux/amd64, linux/arm64), built from tag $TAG"
  git -C "$ROOT" rev-parse -q --verify "refs/tags/$TAG^{commit}" >/dev/null \
    || die "tag $TAG does not exist; run 'scripts/release.sh $VERSION' first"
  if [ "$DRY" -eq 1 ]; then
    step "docker login ghcr.io -u <GHCR_USER | gh user | asked> --password-stdin   (only if not logged in; asks the token without echo)"
    step "docker buildx create --name $BUILDER --driver docker-container   (only if missing)"
    step "git worktree add <tmp dir> $TAG   (removed on exit)"
    step "docker buildx build --builder $BUILDER --platform linux/amd64,linux/arm64 --build-arg VERSION=$VERSION --annotation index:org.opencontainers.image.{source,description,licenses}=... --push -t $IMAGE:$VERSION -t $IMAGE:latest <tmp dir>"
    step "docker logout ghcr.io   (always, on success or failure)"
    exit 0
  fi
  # Whoever can read ~/.docker/config.json can read the token, so log out no matter how this ends.
  WORK=""
  cleanup() {
    docker logout ghcr.io >/dev/null 2>&1 && echo "==> logged out of ghcr.io"
    if [ -n "$WORK" ]; then
      git -C "$ROOT" worktree remove --force "$WORK/src" >/dev/null 2>&1 || true
      rm -rf "$WORK"
    fi
  }
  trap cleanup EXIT
  if ! grep -q 'ghcr\.io' "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null; then
    [ -t 0 ] || die "not logged in to ghcr.io and stdin is not a terminal; run: echo <token> | docker login ghcr.io -u <github-user> --password-stdin   (classic token, only write:packages, short expiry)"
    GH_USER="${GHCR_USER:-}"
    if [ -z "$GH_USER" ] && command -v gh >/dev/null 2>&1; then GH_USER="$(gh api user -q .login 2>/dev/null || true)"; fi
    if [ -z "$GH_USER" ]; then read -r -p "GitHub user: " GH_USER; fi
    [ -n "$GH_USER" ] || die "no GitHub user"
    # The token is read without echo and goes to docker on stdin only: never an argument or an env var.
    read -rs -p "GitHub token (classic, write:packages): " GHCR_TOKEN
    echo
    [ -n "$GHCR_TOKEN" ] || die "empty token"
    docker login ghcr.io -u "$GH_USER" --password-stdin <<<"$GHCR_TOKEN" >/dev/null \
      || { unset GHCR_TOKEN; die "docker login ghcr.io failed"; }
    unset GHCR_TOKEN
  fi
  docker buildx inspect "$BUILDER" >/dev/null 2>&1 \
    || docker buildx create --name "$BUILDER" --driver docker-container >/dev/null
  WORK="$(mktemp -d)"
  git -C "$ROOT" worktree add --detach "$WORK/src" "$TAG" >/dev/null
  docker buildx build --builder "$BUILDER" --platform linux/amd64,linux/arm64 \
    --build-arg "VERSION=$VERSION" "${INDEX_ANNOTATIONS[@]}" --push -t "$IMAGE:$VERSION" -t "$IMAGE:latest" "$WORK/src" \
    || die "build or push failed (is the token valid, with write:packages?)"
  echo "==> pushed $IMAGE:$VERSION and :latest"
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
step "VERSION=$TAG scripts/build.sh"
step "commit 'chore(release): $VERSION' and tag $TAG in both repos"
if [ "$DRY" -eq 0 ]; then
  update_changelog "$ROOT" spacee-herdr-bridge
  update_changelog "$FRONTEND_DIR" spacee-herdr-frontend
  (cd "$FRONTEND_DIR" && npm pkg set "version=$VERSION" >/dev/null)
  VERSION="$TAG" "$ROOT/scripts/build.sh"
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
