package server

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// maxUserDataBytes is the most user data Hetzner accepts with a server.
const maxUserDataBytes = 32 << 10

// claudeVersion is the Claude Code release a provisioned VPS installs,
// the one test/harness-user/Dockerfile installs.
const claudeVersion = "2.1.289"

// daemonBinaryURL replaces the literal "{arch}" in template with the
// architecture serverType provisions: "arm64" for a Hetzner "cax"
// server type, "amd64" for every other type ("cx" included), since a
// "cx" and a "cax" server need a daemon binary built for different
// architectures. A template without "{arch}" is returned unchanged.
func daemonBinaryURL(template, serverType string) string {
	arch := "amd64"
	if strings.HasPrefix(serverType, "cax") {
		arch = "arm64"
	}
	return strings.ReplaceAll(template, "{arch}", arch)
}

// userDataInput is what a provisioned VPS's user data is made from. It
// holds the enrolment token and public material only, since every
// process on the VPS can read its user data
// (docs/adr/2026-10-10-vps-provisioning.md).
type userDataInput struct {
	Token protocol.EnrolmentToken
	// CA is the CA certificate, PEM-encoded, that the daemon verifies the
	// server with.
	CA []byte
	// ServerURL is the server's address the daemon dials, DaemonURL where
	// the VPS downloads the daemon binary from.
	ServerURL, DaemonURL string
}

// userDataTemplate is cloud-config that sets a Debian VPS up as the
// README's "Running the harness as another user" describes, with a
// daemon user orchestrator and a harness user orch-agent, and starts the
// daemon, which enrols with the token on its first start. The sudoers
// rule is test/harness-user/sudoers with the secure_path exemption for
// claude.
var userDataTemplate = template.Must(template.New("user-data").Funcs(template.FuncMap{
	"indent": func(spaces int, text string) string {
		pad := strings.Repeat(" ", spaces)
		return pad + strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n"+pad)
	},
	"quote": func(s string) string { return `"` + s + `"` },
}).Parse(`#cloud-config
ssh_pwauth: false
package_update: true
packages:
  - ca-certificates
  - curl
  - git
  - jq
  - sudo
write_files:
  - path: /etc/orchestrator/ca.crt
    owner: root:root
    permissions: "0644"
    content: |
{{indent 6 .CA}}
  - path: /etc/sudoers.d/orchestrator
    owner: root:root
    permissions: "0440"
    content: |
      Cmnd_Alias ORCH_CLAUDE = /usr/local/bin/claude
      Cmnd_Alias ORCH_GIT = /usr/bin/git
      Cmnd_Alias ORCH_RM = /usr/bin/rm
      Cmnd_Alias ORCH_HARNESS = ORCH_CLAUDE, ORCH_GIT, ORCH_RM
      Defaults!ORCH_HARNESS !requiretty, umask=0077
      Defaults!ORCH_CLAUDE env_keep += "PATH SSH_AUTH_SOCK"
      Defaults!ORCH_GIT env_keep += "PATH SSH_AUTH_SOCK GIT_TERMINAL_PROMPT GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM GIT_SSH_COMMAND"
      Defaults!/usr/local/bin/claude !secure_path
      orchestrator ALL = (orch-agent) CWD=* NOPASSWD: ORCH_HARNESS
  - path: /etc/systemd/system/orchestrator-daemon.service
    owner: root:root
    permissions: "0644"
    content: |
      [Unit]
      Description=orchestrator daemon
      Wants=network-online.target
      After=network-online.target

      [Service]
      User=orchestrator
      Group=orchestrator
      Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
      ExecStart=/usr/local/bin/orchestrator-daemon -server {{.ServerURL}} -ca /etc/orchestrator/ca.crt -enrol-token {{.Token}} -state-dir /var/lib/orchestrator -harness-user orch-agent -workspace-dir /srv/orchestrator/workspaces -claude /usr/local/bin/claude
      Restart=on-failure
      RestartSec=10

      [Install]
      WantedBy=multi-user.target
  - path: /root/orchestrator-setup.sh
    owner: root:root
    permissions: "0700"
    content: |
      #!/bin/sh
      set -eu
      useradd --create-home --shell /bin/bash orchestrator
      useradd --create-home --shell /bin/bash orch-agent
      case $(dpkg --print-architecture) in
        arm64) platform=linux-arm64 ;;
        amd64) platform=linux-x64 ;;
        *) echo "unsupported architecture" >&2; exit 1 ;;
      esac
      base=https://downloads.claude.ai/claude-code-releases/{{.ClaudeVersion}}
      sum=$(curl -fsSL "$base/manifest.json" | jq -er --arg p "$platform" '.platforms[$p].checksum')
      curl -fsSL -o /usr/local/bin/claude.download "$base/$platform/claude"
      echo "$sum  /usr/local/bin/claude.download" | sha256sum -c -
      install -m 0755 /usr/local/bin/claude.download /usr/local/bin/claude
      rm /usr/local/bin/claude.download
      curl -fsSL -o /usr/local/bin/orchestrator-daemon.download {{quote .DaemonURL}}
      install -m 0755 /usr/local/bin/orchestrator-daemon.download /usr/local/bin/orchestrator-daemon
      rm /usr/local/bin/orchestrator-daemon.download
      install -d -o orchestrator -g orchestrator -m 0700 /var/lib/orchestrator
      install -d -o orch-agent -g orch-agent -m 0755 /srv/orchestrator/workspaces
      visudo -c
      systemctl daemon-reload
      systemctl enable --now orchestrator-daemon.service
runcmd:
  - [/bin/sh, /root/orchestrator-setup.sh]
`))

// renderUserData renders the user data of a VPS from input. The server
// URL and token go on a systemd command line unquoted, so they may hold
// no space, quote, backslash, '%' or '$'; the daemon URL is quoted for
// the shell, so it may hold none of '"', '\\', '$' and '`'.
func renderUserData(input userDataInput) (string, error) {
	if strings.ContainsAny(input.ServerURL, " \t\r\n\"'\\%$") || strings.ContainsAny(input.Token.String(), " \t\r\n\"'\\%$") {
		return "", fmt.Errorf("render user data: the server URL or token holds a character a systemd command line would change")
	}
	if strings.ContainsAny(input.DaemonURL, "\r\n\"\\$`") {
		return "", fmt.Errorf("render user data: the daemon URL holds a character the shell would change")
	}
	var buf bytes.Buffer
	err := userDataTemplate.Execute(&buf, struct {
		userDataInput
		Token         string
		CA            string
		ClaudeVersion string
	}{userDataInput: input, Token: input.Token.String(), CA: string(input.CA), ClaudeVersion: claudeVersion})
	if err != nil {
		return "", fmt.Errorf("render user data: %w", err)
	}
	if buf.Len() > maxUserDataBytes {
		return "", fmt.Errorf("render user data: %d bytes, more than Hetzner's %d", buf.Len(), maxUserDataBytes)
	}
	return buf.String(), nil
}
