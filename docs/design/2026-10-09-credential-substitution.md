# Credential substitution for agent-run tasks: research (2026-10-09)

The owner wants every credential a task uses — HTTPS tokens for forges,
registries, cloud CLIs; GPG keys — substituted on egress by something
outside the agent's reach, as the ssh-agent relay already does for ssh
keys. This note records a web survey made on 2026-10-09 by a research
agent, with unverified claims marked **UNVERIFIED**, and the decisions
still open listed at the end.

## Named tools

nono (<https://github.com/nolabs-ai/nono>; also reported at
<https://github.com/always-further/nono> and
<https://helpnetsecurity.com/2026/07/27/nono-open-source-ai-agent-sandboxing/>):
a Rust sandbox runtime on Landlock (Linux) and seatbelt (macOS); the
agent gets session-scoped placeholder credentials as env vars or files;
outbound requests go through nono's credential proxy, which validates
the placeholder, fetches the real credential from the OS keychain and
injects it only into approved request headers for allowed destinations,
with layer-7 policy limiting a token to selected API methods and paths;
delegated tools (git, curl, kubectl) run as separately sandboxed
processes with their own grants; kernel sandboxing makes the proxy the
only egress.

## Agent-held keys

ssh-agent: the key stays in the agent; the client gets signatures over
a Unix socket; the agent can request signatures on arbitrary data but
not read the key; covers git over ssh only.

gpg-agent with socket forwarding
(<https://wiki.gnupg.org/AgentForwarding>): the same property for
signing and decryption; covers GPG commit signing.

Hardware keys/Secretive: **UNVERIFIED** details; same pattern with a
physical confirmation step.

## Credential-injecting proxies

Cloak (<https://github.com/hoophq/cloak>): the agent gets a fake bearer
token or DSN pointing at localhost, Cloak substitutes the real one from
the OS keychain and connects with verified TLS upstream; no TLS
interception; no git/gh/npm integration.

Infisical Agent Vault
(<https://infisical.com/docs/documentation/platform/agent-vault/how-it-works>):
placeholder env vars, `HTTPS_PROXY` to a local proxy that injects the
real header for matching destinations under policy; **UNVERIFIED**
whether it terminates the agent's TLS with an ephemeral certificate.

CyberArk Secretless Broker
(<https://developer.cyberark.com/blog/introducing-the-secretless-broker-open-source-beta/>):
a protocol-aware localhost proxy for databases and HTTP, credentials
from a vault; no git.

1Password Credential Broker
(<https://1password.com/press/2026/june/credential-broker>):
broker-side injection; scope implementation-dependent.

Anthropic's Claude Code sandbox
(<https://anthropic.com/engineering/claude-code-sandboxing>): the
sandbox's only egress is a Unix socket to a host proxy; git uses a
host-side credential provider; session-scoped credentials are useless
outside.

LangSmith sandbox auth proxy
(<https://www.langchain.com/blog/how-auth-proxy-secures-network-access-for-langsmith-agent-sandboxes>):
`proxy_config` rules by host glob, path and header, injecting workspace
secrets; **UNVERIFIED** whether it intercepts TLS.

General point: a forward proxy cannot change headers inside an HTTPS
tunnel without terminating TLS with a CA the client trusts, or the
client must be pointed at a plaintext local endpoint.

## Helpers that hand the secret to the client

These do not meet the requirement; the agent can read them: git
credential helpers (<https://www.mankier.com/7/gitcredentials>), `op
run`, envchain (<https://github.com/sorah/envchain>).

## Network confinement

This makes the proxy the only egress; a separate goal, reaching only
approved hosts, from hiding the secret: Linux network namespaces with
firewall rules; Landlock
(<https://docs.kernel.org/5.18/userspace-api/landlock.html>); macOS
seatbelt/`sandbox-exec`; bubblewrap; per-process host firewall rules
(**UNVERIFIED** as a process-level mechanism).

Note that substitution alone keeps the secret out of the agent's reach;
a bypassed proxy yields an unauthenticated connection, not the token.

## What the agent can still do

Anything the credential's scope allows; the scope (hosts, paths,
methods) matters more than hiding the bytes.

## Table: credential type → mechanism

- git over ssh → ssh-agent relay (built)
- GPG signing → gpg-agent relay
- HTTPS tokens (forge APIs, registries, cloud CLIs) → injecting proxy
  with placeholder
- database credentials → protocol-aware proxy
- anything via env var or file → fails the requirement

## Open decisions (owner, pending)

- TLS-intercepting proxy with a daemon CA trusted by the harness user,
  versus plaintext localhost endpoints per tool.
- Whether confinement is built now or later.
- Daemon-local credential store with per-entry host scope,
  owner-provisioned, server provisioning later.
- GPG deferred.
