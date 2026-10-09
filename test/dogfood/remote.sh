#!/bin/sh
# Starts the dogfood run's git remote: builds the image in this directory
# with the first key the ssh agent holds (`ssh-add -L`; the private key
# stays in the agent), runs it as the container orchestrator-dogfood with
# port 22 on 127.0.0.1:PORT, and pushes REV of this repository as the
# remote's main. Prints the remote's URL. A second run reuses the
# container, so it can follow a run that stopped on known_hosts. Run
# from anywhere:
#
#	test/dogfood/remote.sh [PORT] [REV]
#
# PORT defaults to 2222 and REV to HEAD. `ssh` needs the container's host
# key in known_hosts as [localhost]:PORT; the script prints the
# `ssh-keyscan` line to add rather than writing it. Stop the remote with
# `docker rm -f orchestrator-dogfood`.
set -eu

port=${1:-2222}
rev=${2:-HEAD}
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
url=ssh://git@localhost:$port/orchestrator.git

key=$(ssh-add -L | head -n 1)
docker build --quiet --build-arg "AUTHORIZED_KEY=$key" --tag orchestrator-dogfood "$here" >/dev/null
if docker container inspect orchestrator-dogfood >/dev/null 2>&1; then
	docker start orchestrator-dogfood >/dev/null
else
	docker run --detach --name orchestrator-dogfood --publish "127.0.0.1:$port:22" orchestrator-dogfood >/dev/null
fi
sleep 1

if ! ssh-keygen -F "[localhost]:$port" >/dev/null; then
	echo "remote.sh: known_hosts lacks [localhost]:$port; add it with" >&2
	echo "	ssh-keyscan -p $port -t ed25519 localhost >>~/.ssh/known_hosts" >&2
	echo "after comparing the fingerprint with:" >&2
	docker exec orchestrator-dogfood ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub >&2
	exit 1
fi

GIT_SSH_COMMAND='ssh -o BatchMode=yes' git -C "$root" push --quiet "$url" "$rev:refs/heads/main"
echo "$url"
