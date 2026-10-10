#!/bin/sh
# Builds release binaries for the server and the daemon: cross-compiles
# both commands for linux/amd64 and linux/arm64, with CGO disabled and
# Go's own VCS stamp left intact, and names each file
# <command>-<version>-linux-<arch>. Run from anywhere:
#
#	scripts/build-release.sh [OUTDIR]
#
# OUTDIR defaults to dist under the repository root, created if
# missing. The version is the tag at HEAD (git describe --tags
# --exact-match), else dev-<short hash>; -dirty is appended if the
# working tree has uncommitted changes. Prints the version to stdout
# on success.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
out=${1:-$root/dist}

cd "$root"

if version=$(git describe --tags --exact-match HEAD 2>/dev/null); then
	:
else
	version="dev-$(git rev-parse --short HEAD)"
fi
if [ -n "$(git status --porcelain)" ]; then
	version="${version}-dirty"
fi

mkdir -p "$out"

for cmd in server daemon; do
	for arch in amd64 arm64; do
		env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
			go build -trimpath -o "$out/$cmd-$version-linux-$arch" "./cmd/$cmd"
	done
done

echo "$version"
