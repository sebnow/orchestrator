---
status: accepted
date: 2026-10-10
source: owner decision, working session on 2026-10-10
---

# GUI access

## Context

The server serves the daemons and the owner on one port. It terminates
TLS itself, verifying a daemon by its client certificate under the
server's own CA and letting the owner's browser connect without one
([daemon authentication](2026-10-08-daemon-authentication.md)). The
owner signs in with a token, and every route but the login form needs
it ([owner authentication](2026-10-08-owner-authentication.md)).

The owner wants the GUI and the owner API reachable only through a
Cloudflare Tunnel with Cloudflare Access in front, so that a request
reaches the owner's side only after Access has checked who sent it:
`cloudflared` on the server's VM dials out to Cloudflare and forwards
requests for the GUI's hostname to a listener on the VM's loopback
address. Daemons keep dialling the VM
directly with mutual TLS under the server's CA, on a hostname of their
own. The VM's address is public, since provisioned daemons, whose
addresses are not known in advance, must reach it. On one listener, a
request to that address reaches the GUI without passing Access.

## Decision

A server flag, `-daemon-listen`, gives the daemons an address of their
own. With it, the server serves on two listeners:

- `-daemon-listen` serves the daemons alone: the daemon API, enrolment
  and the daemon binaries, with TLS, verifying daemons' certificates
  under the server's CA.
- `-listen` serves the owner alone: the GUI, the owner API, the GUI's
  static files and the login form, as plain HTTP. The server refuses to
  start unless `-listen` is a loopback IP address. The owner's token is
  still required.

The daemons' listener answers 404 to every route of the owner's. The
owner's listener serves no daemon route: a request for one is refused
as any request without the owner's token is, and with the token
answers 404. `-public-url`, the URL provisioned daemons dial, names the
daemon listener's hostname. Both listeners start and stop together; if
either fails, the server exits.

The owner's listener serves plain HTTP because the tunnel's
`cloudflared` reaches it over loopback on the same machine, and the
browser gets TLS from Cloudflare's edge. The browser checks the
session cookie's `Secure` attribute against that edge, and the
server's check that a form was submitted from its own pages goes by the
browser's `Sec-Fetch-Site` header, so neither needs TLS on the loopback
hop.

Without `-daemon-listen`, one listener serves every route, for a server
on a laptop or one reached without a tunnel.

This amends [daemon authentication](2026-10-08-daemon-authentication.md)
at two points, both only for a server on two listeners: the owner's
browser does not share the daemons' port, and the server does not
terminate the owner's TLS, which Cloudflare's edge does.

### Rejected alternatives

- One listener, with daemons dialling it directly and the GUI's
  requests allowed only from Cloudflare's addresses. Without that
  allowlist a request to the VM's address reaches the GUI without
  passing Access. With it, a request from Cloudflare's addresses proves
  only that it crossed Cloudflare's network, which carries every
  Cloudflare account's traffic, not that it passed this account's
  Access.
- One listener behind Cloudflare for daemons and the owner alike.
  Cloudflare terminates TLS, so the server would not see the daemon
  certificates it identifies daemons by, only Cloudflare's report of
  them. Checking client certificates under the server's own CA at
  Cloudflare's edge needed, as of 2026-10-10, the Enterprise plan for a
  zone, or a paid Zero Trust plan for Access.

## Consequences

- The GUI's hostname and the daemons' hostname differ, and the server
  certificate names only the daemons'; Cloudflare serves its own
  certificate for the GUI's.
- The VM needs only the daemons' port and SSH open; the tunnel dials
  out.
- A server on two listeners needs `cloudflared` running for the owner
  to reach the GUI. Without it the GUI is reachable only from the VM
  itself.
- `-insecure-loopback`, which serves plain HTTP on loopback without any
  authentication, and `-daemon-listen` exclude each other, since the
  daemons' listener always serves TLS.

Revisit if the owner reaches the GUI other than through the tunnel, or
if daemons are to dial through Cloudflare.
