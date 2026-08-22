#!/usr/bin/env bash
set -euo pipefail

# Build a production artifact with one authoritative version injected into
# both the embedded frontend and the Go backend.
#
# Usage:
#   scripts/build-release.sh --version v2.7.8-fr-20260814.1
#
# The -fr-YYYYMMDD.N suffix is intentional: it keeps production releases
# distinct from upstream semver tags while remaining easy to sort and audit.

die() {
  echo "release build error: $*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: scripts/build-release.sh --version vX.Y.Z-fr-YYYYMMDD.N [--output DIR]

Builds the frontend and Linux amd64 backend with the same release version,
then verifies that the embedded frontend and backend both contain that version.
EOF
}

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
readonly release_branch='codex/production-main'
version=${CODEX2API_RELEASE_VERSION:-}
output_dir=$repo_root/dist/releases

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      version=${2:-}
      shift 2
      ;;
    --output)
      output_dir=${2:-}
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[[ -n "$version" ]] || { usage >&2; exit 2; }
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-fr-[0-9]{8}\.[0-9]+$ ]] ||
  die "version must match vX.Y.Z-fr-YYYYMMDD.N: $version"

current_branch=$(git -C "$repo_root" symbolic-ref --quiet --short HEAD || true)
[[ "$current_branch" == "$release_branch" ]] ||
  die "release must run from $release_branch (current: ${current_branch:-detached})"

tracked_changes=$(git -C "$repo_root" diff --name-only && git -C "$repo_root" diff --cached --name-only)
if [[ -n "$tracked_changes" ]]; then
  while IFS= read -r changed_path; do
    [[ -z "$changed_path" ]] && continue
    [[ "$changed_path" == docs/*.md || "$changed_path" == scripts/build-release.sh ]] ||
      die "tracked code change blocks release: $changed_path"
  done <<< "$tracked_changes"
  echo "release_check=docs-only-local-changes-allowed"
fi

command -v go >/dev/null || die "go is required"
command -v npm >/dev/null || die "npm is required"
command -v git >/dev/null || die "git is required"
command -v sha256sum >/dev/null || die "sha256sum is required"

status=$(git -C "$repo_root" status --porcelain --untracked-files=all)
[[ -z "$status" ]] || die "release build requires a clean worktree"

revision=$(git -C "$repo_root" rev-parse --short=7 HEAD)
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/codex2api-release-build.XXXXXX")
trap 'rm -rf "$build_dir"' EXIT

mkdir -p "$output_dir"
artifact_dir=$(cd "$output_dir" && pwd)
artifact="$artifact_dir/codex2api-${version}-${revision}-linux-amd64"

echo "release_version=$version"
echo "revision=$revision"

echo "build_step=frontend"
(
  cd "$repo_root/frontend"
  VITE_APP_VERSION="$version" npm run build
)

frontend_dist="$repo_root/frontend/dist"
[[ -d "$frontend_dist" ]] || die "frontend dist was not generated"
grep -R -F -q "$version" "$frontend_dist" ||
  die "frontend bundle does not contain release version $version"

echo "build_step=backend"
(
  cd "$repo_root"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
      -ldflags "-s -w -X github.com/codex2api/internal/version.Version=$version" \
      -o "$build_dir/codex2api" .
)

grep -a -F -q "$version" "$build_dir/codex2api" ||
  die "backend binary does not contain release version $version"

cp "$build_dir/codex2api" "$artifact"
chmod 755 "$artifact"
artifact_name=${artifact##*/}
(cd "$artifact_dir" && sha256sum "$artifact_name") | tee "$artifact.sha256"
echo "release_artifact=$artifact"
