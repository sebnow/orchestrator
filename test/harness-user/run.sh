#!/bin/sh
# Runs the harness-user container check: cross-compiles cmd/server and
# cmd/daemon for the Docker server's architecture, builds the image in
# this directory, and runs check.sh in a container. Needs Docker and the
# Nix dev shell; run from anywhere:
#
#	test/harness-user/run.sh [--real-claude] [OUTDIR]
#
# OUTDIR, default a new temporary directory, receives the binaries and
# the server's and daemon's logs. The exit status is check.sh's.
#
# Without --real-claude, a stub stands in for claude. With it, the image
# has Claude Code itself (the Dockerfile's target real), and the run
# spends a few short haiku turns of the login macOS's Keychain holds for
# Claude Code ("Claude Code-credentials"). run.sh reads that login once,
# drops its refresh token, and pipes the rest to check.sh's standard
# input, which writes it to ~orch-agent/.claude/.credentials.json on a
# tmpfs and deletes it at the end. It never passes the login as an
# argument. The output also goes to OUTDIR/check.log, and run.sh fails if
# the access or refresh token appears anywhere under OUTDIR.
set -eu

real=false
if [ "${1:-}" = --real-claude ]; then
	real=true
	shift
fi

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

if [ "$real" = false ]; then
	echo "building the image"
	docker build --quiet --target stub --tag orchestrator-harness-user "$here"

	docker run --rm \
		--volume "$out/bin:/opt/orchestrator/bin:ro" \
		--volume "$out:/out" \
		orchestrator-harness-user
	exit
fi

# The access token must outlive the run: without its refresh token the
# container cannot renew it, and a renewal there could invalidate the
# owner's own refresh token.
login=$(security find-generic-password -s "Claude Code-credentials" -w) || {
	echo "run.sh: no Claude Code login in the Keychain" >&2
	exit 1
}
minutes=$(printf '%s' "$login" | jq '((.claudeAiOauth.expiresAt / 1000) - now) / 60 | floor')
if [ "$minutes" -lt 30 ]; then
	echo "run.sh: the Claude Code access token expires in $minutes minutes; run claude on this machine to renew it first" >&2
	exit 1
fi
echo "the Claude Code access token expires in $minutes minutes"

echo "building the image with Claude Code"
docker build --quiet --target real --tag orchestrator-harness-user-real "$here"

{
	if printf '%s' "$login" | jq -c 'del(.claudeAiOauth.refreshToken, .claudeAiOauth.refreshTokenExpiresAt)' |
		docker run --rm --interactive \
			--tmpfs /home/orch-agent/.claude:mode=0700 \
			--volume "$out/bin:/opt/orchestrator/bin:ro" \
			--volume "$out:/out" \
			orchestrator-harness-user-real /usr/local/bin/check.sh --real-claude 2>&1; then
		echo 0 >"$out/check.status"
	else
		echo $? >"$out/check.status"
	fi
} | tee "$out/check.log"
status=$(cat "$out/check.status")

if printf '%s' "$login" | jq -r '.claudeAiOauth | .accessToken, .refreshToken | select(type == "string" and length > 20)' |
	grep -rqF -f /dev/stdin "$out"; then
	echo "run.sh: a token of the login appears under $out" >&2
	status=1
else
	echo "run.sh: no token of the login under $out"
fi
exit "$status"
