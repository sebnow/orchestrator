# The harness-user container check (README.md, "Running the harness as
# another user"). run.sh builds this image and mounts the server and
# daemon binaries at /opt/orchestrator/bin. The default target, stub,
# has the claude stub at /usr/local/bin/claude; the target real, which
# run.sh --real-claude builds, has Claude Code itself there.
FROM debian:trixie-slim AS packages

RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		sudo git openssh-server openssh-client procps jq curl ca-certificates \
	&& rm -rf /var/lib/apt/lists/*

# Claude Code's native build, fetched as https://claude.ai/install.sh
# fetches it and checked against the release manifest's checksum. The
# installer's own `claude install` step is left out: it installs into
# the installing user's home, and the check needs the binary at
# /usr/local/bin/claude, where the harness user can run it.
FROM packages AS claude-code
ARG CLAUDE_VERSION=2.1.289
RUN set -eu; \
	case $(dpkg --print-architecture) in \
		arm64) platform=linux-arm64 ;; \
		amd64) platform=linux-x64 ;; \
		*) echo "unsupported architecture" >&2; exit 1 ;; \
	esac; \
	base=https://downloads.claude.ai/claude-code-releases/$CLAUDE_VERSION; \
	sum=$(curl -fsSL "$base/manifest.json" | jq -er --arg p "$platform" '.platforms[$p].checksum'); \
	curl -fsSL -o /claude "$base/$platform/claude"; \
	echo "$sum  /claude" | sha256sum -c -; \
	chmod 0755 /claude

FROM packages AS base

# The daemon's user, the harness user, and the user the git remote is
# reached as over ssh. The daemon user's home is 0755 on purpose, so that
# the check depends on the modes of the daemon's own files rather than on
# a closed home directory.
RUN useradd --create-home --shell /bin/bash orchestrator \
	&& useradd --create-home --shell /bin/bash orch-agent \
	&& useradd --create-home --shell /bin/bash git \
	&& usermod -p '*' git \
	&& chmod 0755 /home/orchestrator

# The README's sudoers rule with the paths of this image: -claude
# /usr/local/bin/claude, and git and rm as `command -v git rm` finds them
# for the daemon's user, whose PATH check.sh sets to Debian's default.
COPY sudoers /etc/sudoers.d/orchestrator
RUN chmod 0440 /etc/sudoers.d/orchestrator && visudo -c

# The workspace directory, owned by the harness user (checklist step 3).
RUN install -d -o orch-agent -m 0755 /srv/orchestrator/workspaces

COPY check.sh real-claude.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/check.sh /usr/local/bin/real-claude.sh

# ssh as every user runs it from PATH, logging each run first.
COPY ssh-log /usr/local/bin/ssh
RUN chmod 0755 /usr/local/bin/ssh

CMD ["/usr/local/bin/check.sh"]

FROM base AS real
COPY --from=claude-code /claude /usr/local/bin/claude

FROM base AS stub
COPY claude-stub /usr/local/bin/claude
RUN chmod 0755 /usr/local/bin/claude
