#!/bin/sh
# Starts the dogfood run's git remote: builds the image in this directory
# with the public key in KEYFILE as the one key that may push, runs it as
# the container orchestrator-dogfood with port 22 on 127.0.0.1:PORT, and
# makes REV of this repository the remote's main. Prints the remote's
# URL. A second run reuses the container, so it can follow a run that
# stopped on known_hosts. Run from anywhere:
#
#	test/dogfood/remote.sh KEYFILE [PORT] [REV]
#
# KEYFILE is the daemon's public key, <state-dir>/ssh_ed25519.pub, which
# the daemon writes at its first start
# (docs/adr/2026-10-10-daemon-push-identity.md); the owner's own keys are
# not authorized. A changed KEYFILE takes effect once the container is
# removed. PORT defaults to 2222 and REV to HEAD. REV reaches the remote
# through docker exec rather than ssh, so the owner needs no key there.
# The daemon's ssh needs the container's host key in known_hosts as
# [localhost]:PORT; the script prints the `ssh-keyscan` line to add
# rather than writing it. Stop the remote with
# `docker rm -f orchestrator-dogfood`.
set -eu

if [ $# -lt 1 ]; then
	echo "usage: remote.sh KEYFILE [PORT] [REV]" >&2
	exit 2
fi
keyfile=$1
port=${2:-2222}
rev=${3:-HEAD}
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
url=ssh://git@localhost:$port/orchestrator.git

key=$(head -n 1 "$keyfile")
case $key in
ssh-*) ;;
*)
	echo "remote.sh: $keyfile does not hold an ssh public key" >&2
	exit 1
	;;
esac
docker build --quiet --build-arg "AUTHORIZED_KEY=$key" --tag orchestrator-dogfood "$here" >/dev/null
if docker container inspect orchestrator-dogfood >/dev/null 2>&1; then
	docker start orchestrator-dogfood >/dev/null
else
	docker run --detach --name orchestrator-dogfood --publish "127.0.0.1:$port:22" orchestrator-dogfood >/dev/null
fi
sleep 1

commit=$(git -C "$root" rev-parse --verify "$rev^{commit}")
printf '%s\n' "$commit" | git -C "$root" pack-objects --revs --stdout -q |
	docker exec --interactive --user git orchestrator-dogfood \
		sh -c "git -C /orchestrator.git index-pack --stdin >/dev/null && git -C /orchestrator.git update-ref refs/heads/main $commit"

if ! ssh-keygen -F "[localhost]:$port" >/dev/null; then
	echo "remote.sh: known_hosts lacks [localhost]:$port; add it with" >&2
	echo "	ssh-keyscan -p $port -t ed25519 localhost >>~/.ssh/known_hosts" >&2
	echo "after comparing the fingerprint with:" >&2
	docker exec orchestrator-dogfood ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub >&2
	exit 1
fi
echo "$url"
