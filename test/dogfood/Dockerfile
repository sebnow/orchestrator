# A git remote for the dogfood run (docs/design/2026-10-09-dogfood-run.md):
# sshd and an empty bare repository /orchestrator.git, owned by the user
# git, the path of ssh://git@HOST:PORT/orchestrator.git. The repository is
# moved there in a layer of its own so that the host keys, generated in the
# layer before, survive a rebuild from cache. remote.sh passes the public key
# that may push as the build argument AUTHORIZED_KEY.
FROM debian:trixie-slim

RUN apt-get update \
	&& apt-get install -y --no-install-recommends git openssh-server \
	&& rm -rf /var/lib/apt/lists/*

ARG AUTHORIZED_KEY
RUN test -n "$AUTHORIZED_KEY" \
	&& useradd --create-home --shell /bin/sh git \
	&& usermod -p '*' git \
	&& install -d -o git -g git -m 0700 /home/git/.ssh \
	&& printf '%s\n' "$AUTHORIZED_KEY" >/home/git/.ssh/authorized_keys \
	&& chown git:git /home/git/.ssh/authorized_keys \
	&& chmod 0600 /home/git/.ssh/authorized_keys \
	&& su git -c 'git init --quiet --bare -b main /home/git/orchestrator.git' \
	&& ssh-keygen -A \
	&& mkdir -p /run/sshd

RUN mv /home/git/orchestrator.git /orchestrator.git

EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
