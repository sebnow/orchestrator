#!/bin/sh
# Runs the harness-user container check: cross-compiles cmd/server and
# cmd/daemon for the Docker server's architecture, builds the image in
# this directory, and runs check.sh in a container. Needs Docker and the
# Nix dev shell; run from anywhere:
#
#	test/harness-user/run.sh [OUTDIR]
#
# OUTDIR, default a new temporary directory, receives the binaries and
# the server's and daemon's logs. The exit status is check.sh's.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
out=${1:-$(mktemp -d "${TMPDIR:-/tmp}/harness-user.XXXXXX")}
mkdir -p "$out/bin"

case $(docker info --format '{{.Architecture}}') in
aarch64 | arm64) arch=arm64 ;;
x86_64 | amd64) arch=amd64 ;;
*)
	echo "run.sh: unsupported Docker architecture" >&2
	exit 1
	;;
esac

echo "building linux/$arch binaries into $out/bin"
(cd "$root" && nix develop -c env CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$out/bin/" ./cmd/server ./cmd/daemon)

echo "building the image"
docker build --quiet --tag orchestrator-harness-user "$here"

docker run --rm \
	--volume "$out/bin:/opt/orchestrator/bin:ro" \
	--volume "$out:/out" \
	orchestrator-harness-user
