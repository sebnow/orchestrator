---
status: accepted
date: 2026-10-10
source: >-
  owner decisions, 2026-10-10 (the server orchestrates provisioning; no
  manual steps on a client); the enrolment design accepted with
  [VPS provisioning](2026-10-10-vps-provisioning.md)
---

# Daemon enrolment

## Context

[VPS provisioning](2026-10-10-vps-provisioning.md) decided that a
machine the server creates enrols with a one-time token and a
certificate signing request, rather than a certificate and key handed
to it, so that the private key never crosses the cloud-init user data
that every process on a Hetzner VPS can read through the metadata
service.

Two earlier records say otherwise, and neither record was later
amended.
[Daemon authentication](2026-10-08-daemon-authentication.md) decided
that `issue-daemon-cert` generates a daemon's private key on the
server's machine and that the running server needs only the CA's
certificate, never its key. [Client
connectivity](2026-10-07-client-connectivity.md) decided that the
owner provisions each machine by hand. This record states the
amendment [VPS provisioning](2026-10-10-vps-provisioning.md) made to
each, against what `internal/protocol/enrol.go`,
`internal/server/enrol.go` and `internal/daemon/enrol.go` build.

## Decision

[Daemon authentication](2026-10-08-daemon-authentication.md) says, in
its Decision, "The issuing subcommand generates the daemon's private
key, rather than signing a request made on the machine, and writes the
certificate, the key and the CA certificate to files that the owner
copies to the machine," and, in its Consequences, "The running server
needs the CA's certificate but not its key; the key is needed only to
issue certificates and can live elsewhere." [Client
connectivity](2026-10-07-client-connectivity.md) says, in its
Decision, "The owner provisions each machine, installs the daemon, and
points it at the server. Server-driven provisioning is deferred." This
record amends both clauses: a daemon enrolling itself generates its
own key, and the server that creates the VPS provisions it without a
step by the owner.

The server may be started with the CA's key, `-ca-key`, paired with
`-client-ca`; a server without it keeps verifying daemons as before,
but refuses enrolment. With the key, the server signs certificate
signing requests at `POST /v1/enrol`, the one route outside mutual
TLS, since a daemon that has not enrolled yet has no certificate to
present there. A single-use token, written `id:secret` and bound to a
daemon id, protects the route: it expires, one hour by default, the
server stores only its secret's hash, and it is spent only once
signing succeeds, so a request the CA refuses leaves the token usable
again.

A daemon started without `-cert` and `-key` generates its own TLS key
pair, sends the signing request carrying its token, and, once the
server answers, stores the certificate and the key it never sent
anywhere in its state directory; it uses them from then on and ignores
the token on later starts.

`issue-daemon-cert` stays, generating a key on the server's machine,
for the owner's own machines, where the key crossing the host that
issues it and the copy to the machine is accepted. `enrol-token`
issues a token for a machine the owner sets up without Hetzner: the
owner copies the CA certificate and a daemon binary to it and starts
the daemon with the token, enrolling it the same way a Hetzner VPS
does.

## Consequences

- The token is the only secret in cloud-init, and it sits in a
  world-readable systemd unit file until the daemon spends it on its
  first start.
- A daemon that crashes between the server signing its certificate and
  the daemon writing its files is stuck: the token is spent, so only a
  new token from the owner, or destroying the VPS, recovers it.
- A server started without `-ca-key` refuses enrolment.

Revisit if a daemon needs to re-enrol without the owner issuing a new
token, such as to replace a certificate before it expires.
