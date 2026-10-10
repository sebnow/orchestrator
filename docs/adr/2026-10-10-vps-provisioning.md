---
status: accepted
date: 2026-10-10
source: >-
  owner decisions, 2026-10-10: Hetzner Cloud; the server orchestrates; no
  manual steps on a client; security considered, TLS required
---

# VPS provisioning and daemon enrolment

## Context

[Client connectivity](2026-10-07-client-connectivity.md) had the owner
provision each daemon's machine by hand and deferred server-driven
provisioning. A survey of VPS providers
([survey](../design/2026-10-10-vps-providers.md), [follow-up
note](../design/2026-10-10-vps-providers-followup.md)) found Hetzner
Cloud the only one meeting every stated requirement: a provisioning
API, an official Terraform provider, EU data centres, S3-compatible
object storage, hourly billing, and arm64 instances; its prices come
from an aggregator and are to be confirmed on Hetzner's own pricing
page before they are acted on.

Today a daemon's certificate is issued by hand with `server
issue-daemon-cert` (`cmd/server/pki.go`) and its files are copied to the
machine, as [daemon authentication](2026-10-08-daemon-authentication.md)
records. Cloud-init user data on a Hetzner server is readable by every
process on the machine through the metadata service, so it can carry no
private key. A daemon already generates its own ssh key for pushing
work ([daemon push identity](2026-10-10-daemon-push-identity.md)) and
logs in to its harness through the server rather than by any step on
the machine itself ([harness login](2026-10-10-harness-login.md)); VPS
enrolment follows the same shape, keeping every client-side step out of
the owner's hands.

## Decision

The server creates and destroys daemon VPSes through the Hetzner Cloud
API: it creates a server with `hcloud` server-create, labelled for
ownership, of a server type and in a location the owner configures, and
deletes it the same way. Terraform manages the server's own VPS; it is
not used for daemons, which the server manages itself through the API.

The created server's cloud-init installs the daemon binary and runs it
with a one-time enrolment token and the CA's public certificate, both
passed as the one-time inputs provisioning gives it; neither is a
private key. The daemon generates its own TLS key pair and sends a
certificate signing request, carrying the token, to the server's
enrolment endpoint over TLS, verifying the server's certificate against
the CA it was given; the server issues the daemon's certificate in
response. The token is single-use and expires. The daemon's id is the
one the server assigned when it created the VPS, carried in the
certificate as daemon authentication already does for a hand-issued
one.

Once enrolled, the daemon connects like any other: it dials the server
over mutual TLS, generates its own ssh push key, reports its facts, and
is logged in to its harness through the server's GUI as any daemon is.
`issue-daemon-cert` stays, for machines the server does not create.

The server destroys a VPS on the owner's request today; destroying one
on a policy, such as being idle for a configured time, is a separate
decision. Hetzner's S3-compatible object storage is the backup target
([SQLite backups](2026-10-10-sqlite-backups.md)).

## Consequences

- The server holds a Hetzner API token, its first stored secret,
  configured by flag or environment and never written to the database.
- The enrolment endpoint is the first route a daemon reaches without
  mutual TLS, since the daemon has no certificate yet when it calls it;
  the token, sent over TLS to a server the daemon has verified against
  the CA, is what protects it.
- A VPS costs by the hour for as long as it exists.
- A daemon's first login on a fresh VPS still needs one browser
  authorisation by the owner, as any daemon's login does.

Revisit if a provider other than Hetzner is needed, or if idle-VPS
destruction is decided.
