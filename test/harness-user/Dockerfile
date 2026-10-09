# The harness-user container check (README.md, "Running the harness as
# another user"). run.sh builds this image and mounts the server and
# daemon binaries at /opt/orchestrator/bin.
FROM debian:trixie-slim

RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		sudo git openssh-server openssh-client procps jq curl ca-certificates \
	&& rm -rf /var/lib/apt/lists/*

# The daemon's user, the harness user, and the user the git remote is
# reached as over ssh. The daemon user's home is 0755 on purpose, so that
# the check depends on the modes of the daemon's own files rather than on
# a closed home directory.
RUN useradd --create-home --shell /bin/bash orchestrator \
	&& useradd --create-home --shell /bin/bash orch-agent \
	&& useradd --create-home --shell /bin/bash git \
	&& usermod -p '*' git \
	&& chmod 0755 /home/orchestrator

COPY claude-stub /usr/local/bin/claude
RUN chmod 0755 /usr/local/bin/claude

# The README's sudoers rule with the paths of this image: -claude
# /usr/local/bin/claude, and git and rm as `command -v git rm` finds them
# for the daemon's user, whose PATH check.sh sets to Debian's default.
COPY sudoers /etc/sudoers.d/orchestrator
RUN chmod 0440 /etc/sudoers.d/orchestrator && visudo -c

# The workspace directory, owned by the harness user (checklist step 3).
RUN install -d -o orch-agent -m 0755 /srv/orchestrator/workspaces

COPY check.sh /usr/local/bin/check.sh
RUN chmod 0755 /usr/local/bin/check.sh

CMD ["/usr/local/bin/check.sh"]
