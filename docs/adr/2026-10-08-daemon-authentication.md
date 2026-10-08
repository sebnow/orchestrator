# Daemon authentication

- Status: Proposed
- Decided: 2026-10-08
- Source: owner decision, 2026-10-08, to add authentication before the
  server is exposed to the internet;
  [client connectivity](2026-10-07-client-connectivity.md), Consequences;
  [client protocol](2026-10-07-client-protocol.md), Transport

## Context

The server runs on a VPS reachable from the internet. Without
authentication, whoever reaches the server can claim to be any daemon:
they can open its command stream, which carries every prompt the owner
sends it, and post events into its tasks. A daemon has to trust the
server too, because the server's commands make it run an agent with
broad permissions on its machine
([task credentials](2026-10-07-task-credentials.md)).

[Client connectivity](2026-10-07-client-connectivity.md) requires the
connection to work without a VPN or private network. It prefers
certificate-based authentication, because a server that provisions
machines could issue each one a certificate as it builds it.
[Client protocol](2026-10-07-client-protocol.md) names mutual TLS as the
preferred form and leaves authentication undecided. The single owner
([tenancy](2026-10-07-tenancy.md)) provisions every machine by hand;
server-driven provisioning is deferred
([client connectivity](2026-10-07-client-connectivity.md)).

## Decision

Daemons authenticate with mutual TLS, using only the Go standard
library.

- The server owns a private certificate authority. `cmd/server`
  subcommands create it, issue the server's certificate for the names
  and addresses daemons dial, and issue each daemon's certificate.
  Certificates are valid for one year.
- A daemon's certificate carries the daemon's id as its Common Name. The
  issuing subcommand generates the daemon's private key, rather than
  signing a request made on the machine, and writes the certificate, the
  key and the CA certificate to files that the owner copies to the
  machine. This is simpler, but the daemon's private key passes through
  the host that issues it and through the copy to the machine.
- The server rejects a request under `/v1/daemons/{daemon}/` unless it
  carries a certificate that the TLS handshake verified against the CA
  and whose Common Name equals `{daemon}`. The daemon reads its id from
  its certificate, so its id always matches the one the server
  verifies.
- The daemon trusts only the private CA when it verifies the server,
  not the operating system's roots.
- The server terminates TLS itself. A client certificate is optional at
  the handshake, so the owner's browser can connect on the same port
  ([owner authentication](2026-10-08-owner-authentication.md)).
- A server started with `-insecure-loopback` serves plain HTTP with no
  authentication, for development and tests. It starts only when its
  listen address is a loopback address. Without the flag, the server
  refuses to start unless it has its certificate, the CA to verify
  daemons with, and an owner token. A daemon accepts a plain `http://`
  server URL only for a loopback address.

The protocol's messages are unchanged.

## Consequences

- A daemon on any network that reaches the server can connect without a
  VPN, and the server rejects any daemon request that lacks a
  certificate issued by the CA.
- The running server needs the CA's certificate but not its key; the
  key is needed only to issue certificates and can live elsewhere.
  Whoever holds the key can mint any daemon's identity.
- There is no revocation. A daemon certificate stays valid until it
  expires unless the owner replaces the CA and reissues every
  certificate. Renewal is by hand.
- The owner's browser warns about the server's certificate until it
  trusts the CA certificate.
- **UNVERIFIED:** a browser that holds client certificates may ask the
  owner to pick one, because the server asks for an optional client
  certificate at every handshake.
- A proxy in front of the server must pass TLS through rather than
  terminate it, or the server never sees the client certificate.
- In development on one machine, the daemon needs its certificate for
  its id, but the server needs neither its own certificate nor the CA.

Revisit if a machine is lost and its certificate has to stop working
before it expires.
